#!/bin/sh
# Builds one alpine-zfsboot .EFI: an initramfs (Alpine's own mkinitfs,
# with init/init as the custom /init - see that file for the actual
# boot logic) bundled with a kernel into a single Unified-Kernel-Image
# -style EFI executable, using this project's own EFI loader (efi/ -
# see efi/main.c) as the stub.
#
# Why this project builds its own loader rather than depend on any
# existing stub: the previous approach (a patched build of a
# third-party x86 boot-protocol stub) turned out to be structurally
# x86-only - it implemented Linux's legacy "EFI Handover Protocol"
# (setup_header/boot_params/handover_offset), which has no arm64
# equivalent at all, and separately hit a real, reproducible boot
# failure on Proxmox/OVMF that took real debugging to track down (see
# git history for that story if curious). This project's own loader
# (efi/) instead loads the embedded kernel as a plain PE/COFF EFI
# application (LoadImage/StartImage) and lets the kernel's OWN EFI
# stub (CONFIG_EFI_STUB, built into vmlinuz/Image on every
# architecture the kernel supports it on) do the arch-specific
# handoff - this project only needs to hand it a command line and an
# initrd, both via mechanisms (LoadOptions, EFI_LOAD_FILE2_PROTOCOL)
# the kernel's stub already expects identically on x86_64 and arm64.
# See efi/main.c's own header comment for the full reasoning.
#
# Assembly is a single `objcopy --add-section`/`--change-section-vma`
# call per section (.cmdline/.initrd/.linux) at fixed VMAs - efi/'s
# own pe_sections.c locates them at runtime by name, not by these
# specific addresses, so the exact values just need to not collide
# with each other or with the loader's own (tiny) footprint.
#
# Meant to run inside a real `alpine:X.Y` container - see Justfile and
# .github/workflows/release.yml. Needs: mkinitfs, the zfs/zfs-<flavor>
# and linux-<flavor> packages matching KERNEL_FLAVOR, kexec-tools,
# gnu-efi/gnu-efi-dev/gcc/musl-dev/make (to build efi/), binutils
# (objcopy), python3 (the menu's logic - only the interpreter/library
# need to actually be installed here; build.sh copies in just the
# stdlib subset it needs, not the whole package - see the FEATURES
# comment below), dialog (the menu's actual rendering - see menu.py's
# own header comment), ncurses-terminfo-base (terminfo entries dialog
# itself needs at runtime), xz (bundled into the rescue image itself
# for boot-dataset.sh's own decompress_initrd() - a TARGET system's
# separately-built initramfs may be xz-compressed even though this
# project's OWN rescue initramfs is gzip now, see this file's own "-C
# gzip, NOT xz" comment below),
# xorriso/dosfstools/mtools (iso.sh's ISO wrapper, called at the end),
# sgdisk (bundled into the rescue shell itself, not used by this
# script - see the alpine-zfsboot.files list below).
set -eu

# Resolve the repo root from this script's own location, not $PWD/cd's
# implicit $OLDPWD - this gets `cd`ed away from below, and the caller
# might invoke this from anywhere (the Justfile does, the CI workflow
# does from a container's /work mount).
REPO_ROOT="$(cd "$(dirname "$0")" && pwd)"

ARCH="$(uname -m)"
KERNEL_FLAVOR="${KERNEL_FLAVOR:-lts}"
POOL="${ALPINE_ZFSBOOT_POOL:-zroot}"
MENU_TIMEOUT="${ALPINE_ZFSBOOT_MENU_TIMEOUT:-10}"
OUT_DIR="${OUT_DIR:-/out}"
BUILD_DIR="${BUILD_DIR:-/tmp/alpine-zfsboot-build}"

case "$ARCH" in
    x86_64|aarch64) ;;
    *) echo "unsupported ARCH: $ARCH (expected x86_64 or aarch64)" >&2; exit 1 ;;
esac

mkdir -p "$BUILD_DIR" "$OUT_DIR"

# --- ca-certificates, ENSURED here, not just assumed --------------------
# alpine-zfsboot.files' own `echo /etc/ssl/certs` entry (further below) just
# captures whatever's really on THIS build host at the moment mkinitfs
# runs - it has no way to tell a real cert bundle apart from a
# genuinely empty directory, and until now neither did this script.
# Real, repeatedly-hit bug: the Justfile's own Docker recipe DOES `apk
# add ca-certificates-bundle` before calling this script, but that's a
# property of ONE particular caller, not of build.sh itself - any other
# invocation (a build host that already has an `alpine:X.Y` rootfs set
# up some other way, say) that skips that one `apk add` silently ships
# a rescue image whose /etc/ssl/certs LOOKS populated (the directory
# exists) but has nothing real inside it - every single HTTPS fetch
# (apk, wget) inside a real boot of that image then fails with "TLS:
# server certificate not trusted", with no warning anywhere until
# someone hits it live, boots confused, and has to work around it by
# hand with --allow-untrusted. The build-time verify check further
# below (grep its own comment for "ca-certificates bundle presence
# check") catches this AFTER the fact, once the rest of this
# (expensive) build already ran - this fixes it at the SOURCE instead:
# build.sh no longer trusts any caller to have remembered this, it
# just makes sure itself, unconditionally, before anything else here
# even starts.
if [ -z "$(find /etc/ssl/certs -type f -print -quit 2>/dev/null)" ]; then
    if command -v apk >/dev/null 2>&1; then
        echo "no CA certificates found on this build host - installing ca-certificates-bundle"
        apk add --no-cache ca-certificates-bundle
    fi
    if [ -z "$(find /etc/ssl/certs -type f -print -quit 2>/dev/null)" ]; then
        echo "/etc/ssl/certs is still empty after attempting to install ca-certificates-bundle (or apk isn't available at all) - this build host has no real CA trust store, and the rescue image built from it would inherit exactly that. Install ca-certificates-bundle (or this host's real equivalent) and re-run - refusing to build a rescue image with a fake-looking but empty CA store." >&2
        exit 1
    fi
fi

# /etc/ssl/cert.pem (singular - NOT the /etc/ssl/certs directory just
# ensured above) - a real, confirmed gap even with a perfectly valid,
# correctly-dated /etc/ssl/certs/ca-certificates.crt already bundled:
# a real rescue boot still showed "TLS: server certificate not
# trusted" from `apk` even though the cert FILE itself, byte for byte,
# was already correct (confirmed directly - openssl reported identical
# notBefore/notAfter dates before and after the manual workaround that
# "fixed" it) - only re-installing ca-certificates-bundle FOR REAL
# through apk itself made HTTPS trust start working, which means
# something besides the raw file's own bytes was missing. The likely
# culprit: this singular path, which some TLS stacks (apk's own HTTPS
# client apparently among them) check as their actual default CA file,
# separately from /etc/ssl/certs - a real package install would create
# it (Alpine's own ca-certificates-bundle ships it), but this script's
# own manifest below only ever listed the certs DIRECTORY, never this
# sibling path one level up, so mkinitfs had no way to know it needed
# to exist at all. Created here explicitly (not assumed to already be
# there) so this doesn't depend on exactly which package version or
# build-host state happens to provide it.
if [ ! -e /etc/ssl/cert.pem ] && [ -f /etc/ssl/certs/ca-certificates.crt ]; then
    # Relative target (certs/ca-certificates.crt, not the absolute
    # path) - resolves correctly both here on the real build host AND
    # inside this script's own later build-time verify_dir extraction
    # (see the "bundled-file presence check" loop further below, which
    # inspects an extracted COPY of the initramfs, not the real root -
    # an absolute symlink target would silently resolve against the
    # build host's own real /etc/ssl/certs instead of whatever the
    # extracted copy actually contains, making that check pass for the
    # wrong reason).
    ln -s certs/ca-certificates.crt /etc/ssl/cert.pem
fi

# --- our own EFI loader, built from source ---------------------------------
# Fresh copy per invocation, not built in-place under $REPO_ROOT/efi -
# keeps the checked-in source tree untouched (no stray .o/.efi files).
rm -rf "$BUILD_DIR/efi"
cp -r "$REPO_ROOT/efi" "$BUILD_DIR/efi"
make -C "$BUILD_DIR/efi" ARCH="$ARCH" loader.efi
STUB="$BUILD_DIR/efi/loader.efi"

cd "$BUILD_DIR"

# --- kernel version this build targets ------------------------------------
# linux-<flavor> installs its modules under /lib/modules/<kver>/ - that
# directory name IS the kernel version string mkinitfs needs, and is
# also how the /boot/vmlinuz-<flavor> filename maps to a real module
# tree, both here and later at real boot time when the target system's
# own linux-<flavor> package is a different build.
#
# Asked apk itself which /lib/modules/<kver> directory belongs to
# linux-$KERNEL_FLAVOR specifically, rather than globbing
# /lib/modules and taking whatever `find | head -1` happens to list
# first - a full source audit flagged the old form as non-deterministic
# and silently wrong if a second kernel package's module tree is ever
# present in the same build container (a leftover from a previous apk
# layer, or a build-image change that installs more than one
# linux-<flavor> for other reasons): `find`'s ordering is not
# guaranteed to match $KERNEL_FLAVOR at all, and every check after this
# line (file exists, mkinitfs succeeds, initramfs contains a zfs.ko)
# would keep passing even while bundling modules built for the WRONG
# kernel - a mismatch that only surfaces as a real boot failure
# ("modules ... zfs: unknown symbol" or similar) on real hardware, not
# here. `apk info -L` lists the exact paths the linux-$KERNEL_FLAVOR
# package itself installed, so this ties KVER to that package
# deterministically instead of to whatever /lib/modules happens to
# contain.
# `usr/lib/modules/` as well as a bare `lib/modules/` - `apk info -L`
# prints paths relative to /, with no leading slash, and Alpine has
# been moving toward a /usr merge (bin/sbin/lib as symlinks into
# usr/); this pins to whichever prefix alpine-version.txt's own Alpine
# release actually uses without having to know that in advance, rather
# than guessing one and hard-failing on the other the moment it moves
# (the exact "cannot resolve a hard-earned system fact until the guess
# is finally proven wrong on a real build" bug class this file's own
# [ -d ... ] sanity check just below already exists to catch, made
# narrower by allowing both known-real prefixes here instead).
KVER="$(apk info -L "linux-$KERNEL_FLAVOR" 2>/dev/null | sed -n 's#^\(usr/\)\{0,1\}lib/modules/\([^/]*\)/.*#\2#p' | sort -u)"
case "$(printf '%s\n' "$KVER" | wc -l)" in
    1) [ -n "$KVER" ] || { echo "linux-$KERNEL_FLAVOR is not installed (apk info -L returned nothing under lib/modules/ or usr/lib/modules/)" >&2; exit 1; } ;;
    # F19 (unidoc-alip's PR #5 review): $KVER quoted here now, rather
    # than silenced via a disable comment the way the other five real
    # instances of this same deliberate pattern are elsewhere in this
    # repo - a linter's own per-line disable comments are only valid
    # in front of a complete command (confirmed directly: placed on a
    # single case branch like this one, this linter fails to parse the
    # branch at all), so quoting was simpler here than restructuring
    # around that placement rule. $KVER can hold more than one
    # newline-separated entry (sort -u, above) when this branch runs;
    # printf '%s\n' with it quoted still prints every one of them, one
    # per line - real, readable output, just not the same space-joined
    # single-line shape the old unquoted word-split produced.
    *) { echo "linux-$KERNEL_FLAVOR's own file list names more than one /lib/modules/<kver> directory - can't pick one deterministically:"; printf '%s\n' "$KVER"; } >&2; exit 1 ;;
esac
[ -d "/lib/modules/$KVER" ] || { echo "linux-$KERNEL_FLAVOR claims kernel version $KVER but /lib/modules/$KVER does not exist" >&2; exit 1; }
KERNEL="/boot/vmlinuz-$KERNEL_FLAVOR"
[ -f "$KERNEL" ] || { echo "$KERNEL not found" >&2; exit 1; }

# --- initramfs -------------------------------------------------------------
# Feature list, deliberately smaller than mkinitfs's own default
# ("ata base cdrom ext4 keymap kms mmc nvme raid scsi usb virtio"):
# dropped cdrom (no CD boot), ext4 (root is always ZFS here), mmc/raid
# (this project's own README says "you control the hardware" - add them
# back locally via FEATURES= if that stops being true for a given
# fleet). Kept: base (busybox, core tools - includes simpledrm, the
# generic EFI-GOP-framebuffer driver, unconditionally), zfs (the whole
# point - zfs userspace + zfs/spl/... kernel modules, both shipped as a
# first-class mkinitfs feature), ata/nvme/scsi/usb/virtio (storage
# controllers - need at least one to see the boot disk at all, and
# there is no reliable way to know which one in advance), keymap
# (recovery-shell usability on non-US layouts).
#
# kms deliberately NOT included, despite the obvious-looking need for
# a VGA console: it pulled in i915/amdgpu/radeon/nouveau (real,
# per-vendor GPU drivers via kms.modules's unqualified
# kernel/drivers/gpu glob) - and apk installing linux-lts drags in
# ~130 linux-firmware-* subpackages as a dependency, then mkinitfs
# bundles the firmware each included GPU driver needs. Confirmed the
# hard way: a real build came out 200+MB (base's own APKINDEX-listed
# install footprint for that dependency chain alone is "OK: 972.4 MiB
# in 159 packages") before this was dropped - nowhere near a
# rescue/boot image's actual needs. base's simpledrm already drives a
# UEFI GOP framebuffer
# directly - real hardware and OVMF/KVM VMs both expose one - with no
# vendor-specific driver or firmware needed, so this is expected to
# cover the VGA console case without kms at all. Re-add it (at real
# cost) only if a specific target genuinely has no GOP framebuffer and
# needs a real KMS driver instead.
FEATURES="${FEATURES:-base zfs ata nvme scsi usb virtio keymap dhcp alpine-zfsboot}"

# --- the Python menu, surgically included (not "apk add python3" and
# let mkinitfs's own dependency-following grab the whole ~23MB
# package) -----------------------------------------------------------
# mkinitfs has no "python" feature of its own - features.d/*.files
# entries are plain host-filesystem paths, copied verbatim (mkinitfs's
# own lddtree integration then resolves each listed ELF's shared-lib
# deps automatically, e.g. libpython3.*.so.1.0 for the python3
# launcher, and whatever shared libs `dialog` itself needs, including
# libncursesw - not listed explicitly here for that reason). The exact
# set below is the traced import closure of `import os, subprocess`
# under this exact interpreter (sys.modules before/after, run for
# real, not guessed), plus the couple of lib-dynload extensions that
# closure needs as separate .so files rather than being frozen into
# libpython itself. menu.py's own UI now goes through `dialog` (see
# below), not Python's curses module, so that closure no longer needs
# to include curses at all. Landed size: smaller than the ~23.5MB
# untrimmed python3 package - the same surgical-inclusion principle
# applied here by hand, since mkinitfs has no per-language mechanism
# for it.
PYVER="$(python3 -c 'import sys; print(f"{sys.version_info.major}.{sys.version_info.minor}")')"
PYLIBDIR="/usr/lib/python$PYVER"
PYDYNLOAD="$PYLIBDIR/lib-dynload"
PYTAG="$(python3 -c 'import sysconfig; print(sysconfig.get_config_var("EXT_SUFFIX"))')"

# /usr/share/zfs/compatibility.d - zpool's own `-o compatibility=`
# feature-flag sets. Written here from THIS repo's own zfs-
# compatibility.d/ (see that directory's own README) rather than
# copied from the build host's existing package state, because
# Alpine's `zfs` apk package doesn't ship this directory at all -
# confirmed empty against pkgs.alpinelinux.org's own package-contents
# search, not assumed - unlike upstream OpenZFS's own source tree,
# which does. Must happen before the alpine-zfsboot.files manifest below
# lists this path: mkinitfs only ever copies a path that already
# exists on THIS build host's filesystem at the time it runs, same as
# every other entry in that list.
mkdir -p /usr/share/zfs/compatibility.d
for f in "$REPO_ROOT"/zfs-compatibility.d/*; do
    [ "$(basename "$f")" = "README.md" ] && continue
    cp "$f" /usr/share/zfs/compatibility.d/
done

mkdir -p "$BUILD_DIR/features.d"
{
    echo "/usr/bin/python$PYVER"
    # The unversioned symlink, not just the real binary above: init's
    # own `exec python3 /menu.py` looks up "python3" on PATH - it has
    # no reason to know the exact installed version - and mkinitfs
    # only ever ships exactly what's listed here, verbatim, with no
    # implicit "also make the usual distro symlinks" step. Confirmed
    # missing the hard way: a real boot's own kernel panic
    # ("Attempted to kill init!") traced straight back to
    # "/init: exec: line 312: python3: not found" - the interpreter
    # was present (as python$PYVER) but nothing on PATH was named
    # plain "python3".
    echo "/usr/bin/python3"
    # _posixsubprocess: same story as math/select above - a separate
    # lib-dynload extension on this musl/Alpine python build even though
    # it's compiled directly into libpython on other builds (confirmed
    # missing the hard way: subprocess.py's own `from _posixsubprocess
    # import fork_exec` is the very next line after locale/functools/
    # collections/operator all succeeded, once those were added above).
    # NOT _curses/_curses_panel here (an earlier pass this session did
    # list them) - menu.py no longer imports Python's own curses module
    # at all, having moved its whole rendering layer onto the real
    # `dialog` binary instead (see below and menu.py's own header) -
    # dialog is its own separate ncurses-linked C program, resolved and
    # bundled by mkinitfs's lddtree integration same as any other ELF
    # binary listed here, not through anything in this python section.
    # _random: same story again, one boot later - random.py's own
    # `import _random` (needed by tempfile.py's `from random import
    # Random`, see below) is builtin on other platforms but a separate
    # lib-dynload .so on this musl/Alpine python build too.
    # termios: needed by menu.py's own multi-console keypress race
    # (flushing stale input on each candidate tty before racing them -
    # confirmed against Alpine's own real lib-dynload contents listing
    # this time, not guessed and found missing on a real boot again).
    # _datetime: same story again, for _build_stamp()'s own
    # datetime.strptime() call (reformatting the build stamp for
    # display - see menu.py) - traced for real against this dev
    # machine's own python3 (`sys.modules` before/after
    # `datetime.datetime.strptime(...)`, same method every other entry
    # in this list was found by) and included here on the same
    # precedent as math/select/_posixsubprocess/_random above: each of
    # those turned out to be a separate lib-dynload .so on this musl/
    # Alpine build despite being built directly into libpython on the
    # glibc build that trace actually ran against. Confirmed for real
    # on an actual Alpine build since (menu.py's own import check,
    # further down, passed).
    # _struct/fcntl: needed by menu.py's own _run_dialog_with_activity()
    # (the countdown's pty-relay keystroke-activity detector) - traced
    # the same way (`import pty, select, struct, fcntl, tty` before/
    # after `sys.modules`), and included on the same musl/Alpine
    # precedent as everything else in this list - NOT yet confirmed
    # against a real Alpine build the way _datetime now is; the same
    # import-check step below catches it loudly if this guess is wrong.
    # time: on this dev box's own (glibc) python3, `time` shows as a
    # genuine builtin with no separate file at all - the single most
    # "obviously fine" case of everything in this list, and precisely
    # because every other module here that ALSO looked builtin on
    # glibc (math, select, _posixsubprocess, _random, _datetime,
    # _struct, fcntl - every one of them) turned out to be a separate
    # lib-dynload .so on musl/Alpine anyway, "obviously fine" has not
    # once actually been a safe assumption this session - listed here
    # too rather than trusted on priors, same loud build-time
    # import-check catching it if this guess is wrong in either
    # direction.
    for so in math select _posixsubprocess _random termios _datetime _struct fcntl time; do
        echo "$PYDYNLOAD/$so$PYTAG"
    done
    for pkg in re collections; do
        echo "$PYLIBDIR/$pkg"
    done
    # operator.py/_weakrefset.py/_collections_abc.py: confirmed missing
    # the hard way on real hardware - `exec python3 /menu.py` got past
    # the earlier "python3: not found" bug (see above) only to panic
    # again immediately after, on `ModuleNotFoundError: No module
    # named 'operator'` (collections/__init__.py imports it directly;
    # _weakrefset and _collections_abc are the same class of miss,
    # confirmed by re-tracing `import curses, os, subprocess`'s full
    # closure for real rather than trusting the previous pass's list).
    # _py_warnings.py: same class of miss as operator.py/_weakrefset.py/
    # _collections_abc.py above, one boot later - warnings.py imports it
    # on this build's actual interpreter (3.14; the trace this list was
    # built from was run against a 3.13 locally, since that's what was
    # on hand - the two versions' stdlib import graphs aren't quite
    # identical). init/init no longer needs every one of these
    # caught in advance to stay safe (see its own "not exec" comment
    # on the python3 /menu.py line) - a module still missing here now
    # falls back to automatic boot instead of panicking the kernel -
    # but it's still worth listing everything real so the menu actually
    # comes up rather than falling back every time.
    # tempfile.py/shutil.py/fnmatch.py/bisect.py/weakref.py/random.py:
    # confirmed missing the hard way, one boot after the dialog
    # rewrite landed - `import tempfile` (added for dialog_textbox()'s
    # temp file - see menu.py) pulls in shutil unconditionally at
    # module load (tempfile.py's own `import shutil as _shutil`, not
    # inside a try/except), which in turn needs fnmatch; tempfile
    # itself also directly needs bisect, weakref, and random
    # (`from random import Random`). Re-traced `import tempfile` alone
    # for real rather than guessing which of its dependencies are
    # "obvious" - shutil's OWN bz2/lzma imports, by contrast, ARE each
    # wrapped in a real try/except ImportError in its source (confirmed
    # by reading it, not assumed) - genuinely optional, so neither
    # those nor their lib-dynload .so's are listed here.
    # datetime.py/_strptime.py/calendar.py: traced for real (same
    # sys.modules-before/after method) against `datetime.datetime.
    # strptime(...)`, added for _build_stamp()'s own reformatting of
    # the build stamp for display (see menu.py and this file's own
    # BUILD_STAMP comment above) - strptime specifically routes
    # through the separate, pure-Python _strptime module, which in
    # turn needs calendar.
    # pty.py/struct.py/tty.py: menu.py's own _run_dialog_with_activity()
    # (the countdown's pty-relay keystroke-activity detector) - same
    # sys.modules trace as everything else above. pty.py needs tty.py
    # (raw-mode setup) and both need the already-listed termios; struct
    # wraps the newly-added _struct extension above.
    for f in encodings/__init__.py encodings/aliases.py encodings/utf_8.py \
             os.py posixpath.py genericpath.py stat.py linecache.py \
             site.py sitecustomize.py abc.py codecs.py enum.py \
             functools.py keyword.py reprlib.py contextlib.py copyreg.py \
             warnings.py types.py threading.py subprocess.py \
             selectors.py signal.py locale.py operator.py \
             _weakrefset.py _collections_abc.py _py_warnings.py \
             tempfile.py shutil.py fnmatch.py bisect.py weakref.py random.py \
             datetime.py _strptime.py calendar.py \
             pty.py struct.py tty.py; do
        echo "$PYLIBDIR/$f"
    done
    # ncurses terminfo entries - needed regardless of the curses-vs-
    # dialog question above, since `dialog` is itself an ncurses
    # program with the exact same setupterm() requirement Python's own
    # curses module had (confirmed missing the hard way on real
    # hardware, back when menu.py still used curses directly:
    # `_curses.error: setupterm: could not find terminfo database`).
    # init/init exports TERM=linux or TERM=ansi (see there) matching
    # exactly the two entries looked up here - NOT vt100 (an earlier
    # pass here shipped that instead), confirmed for real via
    # `infocmp vt100` that its terminfo entry declares no color
    # capability whatsoever (real VT100 hardware was monochrome, and
    # the database reflects that literally) - this project's own
    # green-on-black dialog theme (init/dialogrc) could never have
    # shown any color over serial as long as vt100 was what TERM
    # resolved to, regardless of anything in the theme file itself.
    # Alpine's ncurses-terminfo-base ships these under
    # /etc/terminfo/<letter>/<name> - NOT /usr/share/terminfo, the far
    # more common location on other distros and what an earlier pass
    # here wrongly assumed - confirmed for real against Alpine's own
    # package contents listing, not guessed twice in a row again.
    # Still found via `find` rather than hardcoding that exact path,
    # and without -type f (some terminal names are symlink aliases to
    # a canonical entry).
    #
    # xterm/xterm-256color/screen/screen-256color/tmux-256color/vt100
    # (in addition to linux/ansi above) - dropbear does NOT clear
    # $TERM the way it clears the rest of the environment (see its own
    # small built-in exception list, confirmed against the real
    # bundled binary), so an interactive rescue SSH session arrives
    # here with whatever TERM the CLIENT's own pty-request actually
    # sent - almost always one of these, never "linux"/"ansi". Without
    # a matching bundled entry, dialog/ncurses's own setupterm() fails
    # OUTRIGHT and INSTANTLY for every single dialog call over SSH -
    # confirmed the hard way as the true root cause of a reported "no
    # menu at all over SSH" bug that looked, at first, like a broken
    # PTY (it wasn't - see menu.py's own _sanitize_term()/
    # _establish_controlling_terminal() comments). menu.py's own
    # _sanitize_term() falls back to a bundled entry when the client
    # sends something not in this list at all - bundling the common
    # real-world cases here means that fallback is the rare exception,
    # not the normal case, and the client's own real TERM (with its
    # own real color/keypad capabilities) is used whenever possible.
    for term in linux ansi xterm xterm-256color screen screen-256color tmux-256color vt100; do
        found="$(find /etc/terminfo -name "$term" 2>/dev/null | head -1)"
        [ -n "$found" ] || { echo "no terminfo entry for '$term' found - is ncurses-terminfo-base installed?" >&2; exit 1; }
        echo "$found"
    done
    # dialog - menu.py's whole rendering layer (see its own header
    # comment) after this session's real, live look at hand-rolled
    # curses: a proper menu/inputbox/yesno/textbox widget set instead,
    # the same tool behind Debian-installer and countless other rescue
    # TUIs. `command -v` rather than a hardcoded /usr/bin/dialog, same
    # idiom as dbclient/zgenhostid/kexec below - the exact directory
    # isn't the point, only that it exists on PATH.
    command -v dialog
    # /etc/dialogrc - genuinely monochrome (use_colors = OFF), see
    # init/dialogrc's own header for the real bug this fixes: dialog's
    # compiled-in GLOBALRC is this exact path, consulted whenever
    # DIALOGRC/$HOME/.dialogrc aren't set - menu.py not setting
    # DIALOGRC was NOT enough on its own to stop a colored theme from
    # loading, since dialog fell through to GLOBALRC regardless. This
    # is the file that actually governs dialog's rendering now, not an
    # unused leftover - it must keep being bundled.
    echo "/etc/dialogrc"
    # /etc/alpine-zfsboot-build-stamp - a real, visible build timestamp (see
    # below where it's actually generated) that menu.py's own banner
    # shows, so it's instantly checkable from the boot screen whether
    # a given boot is really running the build just made or a stale
    # one left over from earlier testing.
    echo "/etc/alpine-zfsboot-build-stamp"
    # /etc/alpine-zfsboot-version - the project's own human-facing release
    # number (see below where it's actually generated), shown right
    # alongside the build stamp on the boot screen.
    echo "/etc/alpine-zfsboot-version"
    echo "/menu.py"
    echo "/boot-dataset.sh"
    echo "/fix-kexec-dtb.py"
    # net-config.sh/rescue-ssh.sh - factored out of /init's own body so
    # both /init and boot-dataset.sh can source the SAME implementation
    # (each is a separate process image by the time boot-dataset.sh
    # runs - see its own header comment) rather than each having its
    # own copy. See rescue-ssh.sh's own header comment for the full
    # network/dropbear/lifecycle separation this exists to keep real.
    echo "/net-config.sh"
    echo "/rescue-ssh.sh"
    # pid-alive.sh - the one _pid_alive() implementation, sourced by
    # both rescue-ssh.sh (a stale dropbear PIDFILE) and zfs-unlock.sh
    # (reclaiming a stale per-encryptionroot operation lock) rather than
    # each keeping its own copy.
    echo "/pid-alive.sh"
    # zfs-unlock.sh - the ONE implementation of "acquire a ZFS native-
    # encryption passphrase interactively and stage it for the kexec
    # handoff"/"lock it back up again", sourced by boot-dataset.sh
    # directly and driven by the standalone zfs-unlock executable below
    # for menu.py's own "Unlock encrypted root"/"Lock encrypted root"
    # menu actions - see that file's own header comment for the real,
    # reported gap this closes (menu.py used to have a second, drifted
    # copy of this logic that never staged a handoff secret).
    echo "/zfs-unlock.sh"
    echo "/zfs-unlock"
    # alpine-zfsboot-shell - root's login shell over dropbear (see init, which
    # points /etc/passwd at it). Not command= in authorized_keys: that
    # would force every session to run menu.py regardless of what the
    # client actually asked for, which would silently break
    # `zfs send ... | ssh host zfs recv ...` (the client-requested
    # `zfs recv ...` would just never run). This script decides
    # per-connection instead.
    echo "/alpine-zfsboot-shell"
    # /etc/shells - load-bearing for rescue SSH to work AT ALL, not
    # documentation: see the real `echo "/alpine-zfsboot-shell" >
    # /etc/shells` line further below (and its own much longer comment)
    # for why dropbear refuses every single login outright, before any
    # auth method is even offered, without this exact entry present.
    echo "/etc/shells"
    # bash - a bit heavier than a minimal rescue environment strictly
    # needs, total luxury to use by deliberate choice: tab-completion/
    # history/line editing for the recovery shell and for interactively
    # driving `zfs recv` over the dropbear session below, not something
    # accidentally pulled in.
    echo "/bin/bash"
    # dropbear (sshd) + dropbearkey (host-key validation, see rescue-ssh.sh)
    # - only started at all if authorized_keys/host key material was
    # staged from the ESP for this boot (see /init), so an image with
    # neither configured has no listening sshd at all. udhcpc's own default.script comes
    # from the dhcp feature below, not listed again here.
    echo "/usr/sbin/dropbear"
    echo "/usr/bin/dropbearkey"
    # dbclient - dropbear's own ssh CLIENT (no separate openssh-client
    # dependency) - used by menu.py's "Deploy new machine" to `zfs
    # send` from a remote golden-image host, over the same kind of
    # session this rescue environment's own inbound sshd already
    # provides, just outbound. Confirmed against the real Alpine
    # 3.24 dropbear-*.apk contents: dropbear the base package ships
    # only dropbear/dropbearkey - dbclient is a genuinely SEPARATE
    # subpackage (dropbear-dbclient, see Justfile/release.yml's apk
    # install line), not bundled with the server. A real build first
    # caught this: `command -v dbclient` failing under `set -eu` here
    # aborts with exit 127, which is exactly what happened before that
    # package was added.
    command -v dbclient
    # zgenhostid ships with the zfs package itself (same one already
    # required for zpool/zfs above) - regenerates /etc/hostid for a
    # freshly-deployed machine, since OpenZFS needs that value unique
    # per host. Confirmed against the real Alpine 3.24 zfs-*.apk
    # contents (/usr/sbin/zgenhostid). Resolved via `command -v`
    # rather than that hardcoded path anyway, since the exact
    # directory isn't the point - only that it exists somewhere on
    # PATH.
    command -v zgenhostid
    # nft (nftables) - real enforcement for alpine-zfsboot.ssh.allow=
    # (a source-IP restriction on the rescue sshd - see rescue-ssh.sh's
    # own header comment for why this can't just be a dropbear
    # authorized_keys option: dropbear's restriction set is a
    # documented subset of OpenSSH's and has no `from=` equivalent).
    # Best-effort, NOT `command -v` under `set -eu` like dbclient/
    # zgenhostid above - unlike those, this project has never bundled
    # any packet-filter tool before, so it is NOT confirmed present on
    # every build host this script already runs on, and a hard
    # requirement here would break every existing build that doesn't
    # use ssh.allow= at all, for a feature most builds won't touch.
    # rescue-ssh.sh's own apply_allowlist() already fails loud and
    # default-deny at RUNTIME if ssh.allow= is set but nft isn't
    # actually in the built image - this is what makes that check ever
    # possibly succeed on a build host that does have it, without
    # breaking every other build that doesn't.
    if command -v nft >/dev/null 2>&1; then
        command -v nft
    else
        echo "nft not found on this build host - alpine-zfsboot.ssh.allow= will refuse to start rescue SSH at runtime (default-deny) rather than silently not enforcing it - install nftables on the build host to support this feature" >&2
    fi
    # kexec itself - confirmed missing the hard way on a real boot:
    # kexec-tools being installed on the BUILD container (see the apk
    # list this project's own docs already call out above) says
    # nothing about whether mkinitfs actually bundles its binary into
    # the initramfs - it doesn't, unless something explicitly lists
    # it, exactly like dbclient/zgenhostid above. boot-dataset.sh's
    # entire reason to exist - `kexec -l`/`kexec -e` - was silently
    # unreachable without this: `kexec -l` failed with a real but
    # unhelpful error, and the plain `kexec -e` right after it hit
    # a shell "not found" instead.
    command -v kexec
    # cpio + xz - boot-dataset.sh's own encrypted-root kexec handoff
    # (see that file's own header comment on the block right before its
    # `kexec -l` call) builds a small extra cpio archive at real boot
    # time and appends it to the target's own initrd, so the target's
    # stock initramfs doesn't have to ask for the same passphrase a
    # second time. cpio itself (both writing that new archive with `-o
    # -H newc` and reading a single file back out of the TARGET's own,
    # separately-built initrd with `-i --to-stdout`) has no busybox
    # applet equivalent in this project's build (confirmed against a
    # real Alpine install - `which cpio` resolves to the real apk
    # package, not a busybox symlink), so it must be bundled explicitly
    # like dbclient/zgenhostid above, not assumed. xz alongside it for
    # the same reason boot-dataset.sh's own decompress_initrd() needs
    # it - this project's OWN rescue initramfs is gzip now (see this
    # file's own "-C gzip, NOT xz" comment), but a given TARGET system's
    # separately-built initramfs-$KERNEL_SUFFIX might be gzip or xz
    # depending on that system's own mkinitfs config, and only gzip has
    # a busybox applet
    # to fall back on for free.
    command -v cpio
    command -v xz
    # sgdisk (from Alpine's `sgdisk` package - see Justfile/release.yml's
    # apk install line) - non-interactive, scriptable GPT partitioning,
    # for anyone using `recovery_shell()` to lay out a real disk by hand
    # (the legacy-BIOS path's own protective-MBR/BIOS-boot/boot-blob
    # partitions - see bios/ - need exactly this, and it's the same tool
    # `alpine-installer`'s own install script uses). Not otherwise
    # depended on by anything in this repo - this is convenience
    # tooling for the operator, not something /init or menu.py call
    # themselves - so a build where this line ever starts failing
    # (package renamed/removed) fails loudly here rather than silently
    # shipping a rescue shell that's quietly missing it.
    command -v sgdisk
    # apk itself, in the RESCUE SHELL - not this build container's own
    # copy re-purposed, a real, deliberate design choice: rather than
    # hand-picking one more individual tool into this list every time
    # some future need comes up (curl, then dosfstools for mkfs.vfat,
    # then parted for partprobe, then util-linux for wipefs, ...), give
    # the operator `apk add` itself and let them pull in whatever a
    # given rescue session actually needs, on demand, over whatever
    # network this session has (see menu.py's own "Network" item) -
    # nothing pre-installed, nothing baked into image size, matching
    # this project's own "surgical inclusion" principle at the META
    # level instead of the per-tool level. apk's own shared-library
    # deps (zlib, the crypto lib it verifies package signatures with,
    # ...) are resolved by mkinitfs's usual lddtree pass, same as every
    # other binary listed here - nothing special needed for those.
    command -v apk
    # apk's own trusted signing keys (Alpine's, not this project's) -
    # without these, every `apk add` in the rescue shell would fail
    # signature verification outright. A whole directory, not one file
    # - Alpine ships a handful of these (rotated over the years; older
    # ones kept around so older packages still verify).
    echo /etc/apk/keys
    # /etc/apk/repositories - confirmed missing the hard way on a real
    # boot: this file's real CONTENT is generated further below (a
    # `cat > /etc/apk/repositories` writing this build's own Alpine
    # branch mirror URLs), but that write alone was never enough on its
    # own - a real echo/path entry has to be listed HERE too, in this
    # same manifest alpine-zfsboot.files feeds to mkinitfs, or mkinitfs simply
    # never copies it into the image at all. It wasn't, this whole
    # time - `apk update` in a real rescue shell had no repositories
    # file whatsoever (`ls /etc/apk/repo*` => No such file or
    # directory), not an empty one.
    echo /etc/apk/repositories
    # The CA bundle apk (and anything else doing HTTPS in the rescue
    # shell - the golden-image ssh/zfs-recv path included) needs to
    # validate a mirror's TLS certificate at all - confirmed missing
    # the hard way on a real boot, TWICE: first "TLS: server
    # certificate not trusted" because a bare `alpine:X.Y` build
    # container has no CA bundle installed at all; then, after adding
    # the plain `ca-certificates` package (see Justfile/release.yml),
    # the EXACT SAME error again - that package doesn't write
    # /etc/ssl/certs/ca-certificates.crt directly, it generates it via
    # a post-install trigger script (`update-ca-certificates`) that
    # apparently never actually ran during this build. Confirmed
    # working (a real rescue shell, `apk update` succeeding) only after
    # switching to `ca-certificates-bundle` instead - Alpine's own
    # pre-built alternative that ships the finished bundle file as
    # plain package content, no trigger involved at all. The WHOLE
    # directory bundled here, not just the one file - same reasoning as
    # /etc/apk/keys just above, and harmless even though
    # ca-certificates-bundle itself only actually populates the one
    # consolidated file, not the individual per-cert/hash-symlink form
    # the full ca-certificates package would have.
    echo /etc/ssl/certs
    # /etc/ssl/cert.pem (singular) - a SEPARATE path from the
    # /etc/ssl/certs directory just above, ensured to exist as a
    # symlink to it at the top of this script (grep for "a real,
    # confirmed gap even with a perfectly valid" for the full story) -
    # a real rescue boot showed "TLS: server certificate not trusted"
    # from apk even with a byte-correct /etc/ssl/certs/ca-certificates.crt
    # already bundled, and this singular sibling path being missing is
    # the confirmed difference: apk's own HTTPS client (unlike curl,
    # which worked throughout) apparently checks this default path
    # too/instead. A symlink is still just one path entry for mkinitfs's
    # manifest to know about, same as any other file here.
    echo /etc/ssl/cert.pem
    # apk's own package-state directory - real world, even a fresh
    # Alpine root created straight from the official minirootfs tarball
    # already has this (an empty `installed` index) before `apk add`
    # is ever run against it; this initramfs's own root was built via
    # mkinitfs's file-copying instead, which never creates it. Created
    # for real below (empty, not faked) rather than assuming apk
    # auto-creates it itself the first time it's asked to add something.
    echo /lib/apk/db/installed
    # /etc/apk/world - confirmed missing the hard way on a real boot
    # (`apk update` failing with "ERROR: Unable to read database" /
    # "Failed to open apk database") - see the real content written
    # for this path, further below, for the full explanation.
    echo /etc/apk/world
    # zfs/zpool themselves come from mkinitfs's own built-in "zfs" feature
    # (see FEATURES= below), not this list. This data-only directory is
    # written from THIS repo's own zfs-compatibility.d/ just above, NOT
    # provided by the zfs apk package at all (confirmed real - see that
    # directory's own README) - without it, ANY `zpool create -o
    # compatibility=<name>` in this rescue shell fails outright with
    # "could not read/parse feature file(s): <name>", regardless of
    # which name is given. /etc/zfs/compatibility.d (an alternate path
    # zpool also checks) is deliberately not also bundled - nothing
    # writes anything there, so there's nothing to gain from it.
    echo /usr/share/zfs/compatibility.d
    # cmd/tool, this project's own status/install/update/verify CLI -
    # baked in from THIS SAME build/commit (see the cp below, which
    # copies the just-cross-compiled out/alpine-zfsboot-$ARCH binary
    # to this path before mkinitfs runs), not apk-installed. A rescue
    # shell that had to `apk add alpine-zfsboot` instead would get
    # whatever version unidoc-aports last happened to package - which
    # lags behind this repo's own master by however long it's been
    # since someone last bumped that APKBUILD's pkgver, and could be
    # flat-out incompatible with THIS build's own on-disk layout
    # (internal/layout's ABI constants) if the two ever drift. Bundling
    # the freshly-built binary here instead means the rescue shell's
    # own alpine-zfsboot always matches the image it's running on,
    # exactly - the same guarantee status/verify/update/install already
    # give an operator on an already-installed target OS, now also true
    # of the rescue environment itself. `apk add alpine-zfsboot` is
    # still how a target OS gets it post-install (see alpine-installer's
    # own README) - this is only about what ships inside THIS image.
    #
    # /boot/alpine-zfsboot, deliberately NOT /usr/bin/alpine-zfsboot:
    # this rescue shell's own `apk add` is real (see the `command -v
    # apk` entry above) - an operator running `apk add alpine-zfsboot`
    # by habit inside a live rescue session would install straight
    # over /usr/bin/alpine-zfsboot if that's where this baked-in copy
    # lived, silently replacing the exactly-matching build with
    # whatever unidoc-aports last packaged. /boot is not on $PATH, so
    # it has to be invoked as /boot/alpine-zfsboot - a small, correct
    # price for "the baked-in copy can never be clobbered by mistake".
    echo /boot/alpine-zfsboot
} > "$BUILD_DIR/features.d/alpine-zfsboot.files"

# Deliberately using mkinitfs's `dhcp` feature (small: af_packet.ko +
# udhcpc's default.script) rather than its `network` feature, which
# also includes kernel/drivers/net/ethernet (every vendor's ethernet
# driver, unqualified glob) - same lesson as the kms/GPU-firmware
# finding above, real NIC firmware for hardware this project has no
# reason to assume exists. virtio_net (VMs, this project's primary
# target) is the deliberately narrow substitute for real hardware NIC
# support; a fleet on real NICs adds its own driver via FEATURES=/a
# custom modules file rather than this getting it by default for
# everyone.
# virtio_scsi/virtio_blk: confirmed missing the hard way on real
# Proxmox hardware - `zpool import` found no pool at all because
# there was no /dev/sd*|vd* node to begin with, and a real boot's own
# PCI enumeration showed the disk controller as [1af4:1004] class
# 0x010000 (Virtio SCSI), which mkinitfs's generic `virtio`/`scsi`
# features apparently don't bundle on their own - same lesson as
# virtio_pci/virtio_net just above, just for storage instead of
# networking. virtio_blk alongside it for the other common Proxmox
# disk-attachment choice (VirtIO Block rather than VirtIO SCSI) -
# not confirmed needed yet, but the identical class of gap.
# virtio_mmio/virtio_gpu added here for the identical reason as the
# four entries above, confirmed the hard way on a real Hetzner Cloud
# ARM64 (CAX) boot: the upstream "virtio" mkinitfs feature's own
# features.d/virtio.modules DOES list kernel/drivers/gpu/drm/virtio
# (confirmed directly against that file upstream), so this looked
# double-covered on paper - but this project's own precedent, right
# here, is that "the generic feature's glob theoretically covers it"
# has already been proven insufficient once (virtio_pci/virtio_net
# above) and cannot be trusted without forcing it explicitly too.
# virtio_mmio matters on its own even without a GPU behind it: QEMU's
# aarch64 "virt" machine (used by Hetzner Cloud, and by any similarly-
# configured KVM/QEMU aarch64 cloud host) exposes virtio devices over
# MMIO, not PCI, by default - virtio_pci alone finds nothing to bind
# to there.
# xhci-pci/xhci-hcd/ehci-pci/ehci-hcd/usbkbd/usbhid/hid-generic/hid,
# alongside virtio_input above: the other half of the real "display
# output not active" root cause on Hetzner CAX - see the CONSOLE_CMDLINE
# comment above for the full story. Explicit file globs, not a
# directory glob (`kernel/drivers/hid/*` was tried and reverted - it
# pulls in every vendor HID driver mkinitfs ships, bloating/breaking the
# build for this platform's bootstrap loader). xhci covers this
# platform's PCIe-attached USB controller (confirmed via dmesg: virtio-
# gpu already enumerates over PCI here); ehci alongside it for older
# QEMU/KVM hosts; usbkbd is the exact module a real, working Alpine
# install on the same Hetzner CAX platform relies on; usbhid/hid-generic
# as a fallback for any keyboard that doesn't support the boot-protocol
# usbkbd needs. dwc2/dwc3 (SoC-integrated USB controllers common on
# physical ARM SBCs) deliberately left out for now - untested, add with
# the same explicit-file-glob discipline once a real target needs them.
# efi-pstore.ko: Last Boot Diagnostics' kernel-crash-evidence reader
# (see init/init's own comment) - this rescue kernel needs its own copy
# of the module bundled to modprobe it at all, same reasoning as every
# other module here. Filename (hyphenated, matching the upstream source
# file drivers/firmware/efi/efi-pstore.c - kbuild does not rewrite
# hyphens to underscores in .ko filenames) is real-kernel-source-
# confirmed as of this writing, but has NOT been verified against
# Alpine's actual built linux-virt module tree the way the (since-
# rejected) ramoops chain was via real QEMU testing - if this glob
# matches nothing at build time, mkinitfs should be checked to confirm
# whether it warns about that or just silently omits the module.
cat > "$BUILD_DIR/features.d/alpine-zfsboot.modules" <<'EOF'
kernel/drivers/virtio/virtio_pci.ko*
kernel/drivers/virtio/virtio_mmio.ko*
kernel/drivers/virtio/virtio_input.ko*
kernel/drivers/net/virtio_net.ko*
kernel/drivers/scsi/virtio_scsi.ko*
kernel/drivers/block/virtio_blk.ko*
kernel/drivers/gpu/drm/virtio/virtio-gpu.ko*
kernel/drivers/usb/host/xhci-pci.ko*
kernel/drivers/usb/host/xhci-hcd.ko*
kernel/drivers/usb/host/ehci-pci.ko*
kernel/drivers/usb/host/ehci-hcd.ko*
kernel/drivers/hid/usbhid/usbkbd.ko*
kernel/drivers/hid/usbhid/usbhid.ko*
kernel/drivers/hid/hid-generic.ko*
kernel/drivers/hid/hid.ko*
kernel/drivers/firmware/efi/efi-pstore.ko*
EOF

cp "$REPO_ROOT/init/menu.py" /menu.py
cp "$REPO_ROOT/init/boot-dataset.sh" /boot-dataset.sh
cp "$REPO_ROOT/init/fix-kexec-dtb.py" /fix-kexec-dtb.py
cp "$REPO_ROOT/init/net-config.sh" /net-config.sh
cp "$REPO_ROOT/init/rescue-ssh.sh" /rescue-ssh.sh
cp "$REPO_ROOT/init/pid-alive.sh" /pid-alive.sh
cp "$REPO_ROOT/init/zfs-unlock.sh" /zfs-unlock.sh
cp "$REPO_ROOT/init/zfs-unlock" /zfs-unlock
cp "$REPO_ROOT/init/alpine-zfsboot-shell" /alpine-zfsboot-shell
cp "$REPO_ROOT/init/dialogrc" /etc/dialogrc
chmod +x /menu.py /boot-dataset.sh /fix-kexec-dtb.py /net-config.sh /rescue-ssh.sh /pid-alive.sh /zfs-unlock.sh /zfs-unlock /alpine-zfsboot-shell

# cmd/tool's own binary, built by `just build-tool` (a Justfile
# dependency of the `build` recipe - see Justfile) BEFORE this script
# ever runs, straight from this checkout's own source, no Docker/apk
# involved for this one piece. Same $ARCH this whole script is already
# building for - build-tool's own GOARCH mapping (amd64/arm64) names
# its output out/alpine-zfsboot-x86_64 / out/alpine-zfsboot-aarch64 to
# match $ARCH exactly, no translation needed here.
[ -f "$OUT_DIR/alpine-zfsboot-$ARCH" ] || { echo "$OUT_DIR/alpine-zfsboot-$ARCH not found - run 'just build-tool' first (the Justfile's build recipe already depends on it)" >&2; exit 1; }
# /boot, not /usr/bin - see the alpine-zfsboot.files entry above for why
# (apk add alpine-zfsboot inside the rescue shell must never be able to
# clobber this exact-match build). /boot already exists on this build
# container (linux-$KERNEL_FLAVOR's own install populates it).
cp "$OUT_DIR/alpine-zfsboot-$ARCH" /boot/alpine-zfsboot
chmod +x /boot/alpine-zfsboot

# /etc/shells, listing /alpine-zfsboot-shell - load-bearing for rescue SSH,
# not documentation. dropbear's own checkusername() (confirmed against the
# real bundled binary, not just its docs) refuses a login OUTRIGHT if the
# target user's passwd shell isn't a byte-exact match against an entry in
# getusershell() - BEFORE any auth method is even offered. This initramfs
# never had an /etc/shells at all, so musl's own getusershell() fallback
# ("/bin/sh"/"/bin/csh" only) applied - meaning every single rescue SSH
# login was refused with "invalid shell, rejected" server-side (surfacing
# to the client as a plain "Permission denied (publickey)"/"No auth
# methods could be used", indistinguishable from a bad key) regardless of
# whether the configured key was ever correct. This is almost certainly
# the real root cause of the "dropbear reachable, key never authenticates"
# symptom seen on real Hetzner CAX hardware - confirmed by actually running
# the bundled aarch64 dropbear binary under qemu-user against a real
# authorized_keys/host-key pair with and without this fix.
echo "/alpine-zfsboot-shell" > /etc/shells

# The rescue shell's own `apk add` (see the alpine-zfsboot.files comment on
# `command -v apk` above) needs a real repositories file to fetch
# anything at all. Overwrites THIS BUILD CONTAINER's own
# /etc/apk/repositories - safe only because every apk-dependent step
# this script itself needs (installing gnu-efi/mkinitfs/etc.) already
# ran, via the Justfile/release.yml `apk add` line, before build.sh
# ever started - nothing after this point in build.sh calls apk again.
# Reads the container's own /etc/alpine-release rather than a
# hardcoded version, so the rescue shell's apk always points at
# whatever Alpine branch this exact build actually ran on, not a
# separately-maintained guess that could drift from it.
alpine_branch="v$(cut -d. -f1,2 /etc/alpine-release)"
mkdir -p /etc/apk
cat > /etc/apk/repositories <<EOF
https://dl-cdn.alpinelinux.org/alpine/$alpine_branch/main
https://dl-cdn.alpinelinux.org/alpine/$alpine_branch/community
EOF

# apk's own package-state directory - see the alpine-zfsboot.files comment on
# why this initramfs doesn't already have it the way a real Alpine
# root would. Empty, not faked - apk populates it for real the first
# time something is actually installed.
#
# /etc/apk/world - confirmed missing the hard way on a real boot:
# `apk update` failed outright with "ERROR: Unable to read database:
# No such file or directory" / "ERROR: Failed to open apk database" -
# apk's own database open sequence reads this file unconditionally
# (the "world" of explicitly-requested packages, empty here since
# nothing was ever apk-installed onto this image) and errors if it's
# flat-out missing rather than treating that as "empty" - a real
# Alpine root built from the official minirootfs tarball always has
# this file already; this initramfs's own root, built via mkinitfs's
# file-copying instead, never did until now.
mkdir -p /lib/apk/db
: > /lib/apk/db/installed
: > /etc/apk/world

# A real, visible build timestamp - menu.py's own banner shows this,
# and it's also embedded into the .cmdline PE section below
# (alpine-zfsboot.buildstamp=) so a machine already running an alpine-zfsboot
# build can tell what it's on without unpacking its initramfs at all -
# see cmd/tool, this project's own update helper. One value, computed
# once, used both places: a compact, sortable, cmdline-safe format
# (no spaces - kernel cmdline word-splits on whitespace) rather than
# the human-friendlier "%Y-%m-%d %H:%M UTC" form an earlier version of
# just the banner file used - menu.py's own _build_stamp() reformats
# this for display, so the boot-screen banner still reads naturally.
#
# Confirmed the hard way, twice in one evening (before either
# consumer of this existed): real confusion about whether a given
# boot is actually running the latest build at all (a stale .EFI file
# still on the test box, from before some fix landed, looking
# IDENTICAL to a fresh one at a glance) - this makes that instantly
# checkable, no more guessing or re-deriving it from unrelated
# symptoms.
BUILD_STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
echo "$BUILD_STAMP" > /etc/alpine-zfsboot-build-stamp

# The project's own human-facing release number (semver, "0.1.0") -
# a SEPARATE thing from BUILD_STAMP above: BUILD_STAMP is what the
# update-check machinery actually compares (cmd/tool's own
# comparison.upToDate, the alpine-zfsboot.buildstamp= cmdline value - both
# lexicographically sortable timestamps, "which build is newer"), this
# is purely what a human reads on the boot screen ("alpine-zfsboot
# version 0.1.0") and cmd/tool's own `version` output (alpine-zfsboot.version=
# below). Read from the repo's own version.txt file, not hardcoded
# here, so a release bump is one file edit, not a build.sh change too.
cat "$REPO_ROOT/version.txt" > /etc/alpine-zfsboot-version

# -C gzip, NOT xz: a real, direct latency measurement, not a re-guess of
# the earlier xz-vs-gzip false lead noted below. `alpine-zfsboot status`
# was measured at 6.37s wall-clock on a real Hetzner CAX aarch64 machine,
# almost entirely CPU time (`real 6.37s` / `user 6.34s`) spent inside
# internal/initrdinfo decompressing this rescue initramfs purely to find
# zfs.ko's own version string. Benchmarked directly (go tool pprof +
# hand timing, identical realistic content, both via Go): this project's
# own pure-Go xz decoder (github.com/ulikunitz/xz, kept for reading
# already-deployed xz artifacts - see internal/initrdinfo's own header
# comment) sustains ~37-43MB/s; Go's stdlib compress/gzip sustains
# ~170-175MB/s on the exact same data - a ~4.6x gap, confirmed via
# pprof to be the LZMA range-decoder's own inherent bit-level cost, not
# a usage inefficiency (no GC/syscall/buffering pathology found). The
# ESP/FAT partition alpine-installer provisions is a fixed 512MiB
# (alpine-install-zfs.sh's own `sgdisk -n ...:+512MiB`) - gzip's own
# ~1.6x larger compressed size versus xz on the same content (a few MiB
# in absolute terms for this project's own initramfs) is negligible
# against that, unlike the LZMA decode cost, which is paid in full on
# every single status/verify call. See the hardening ledger's own entry
# for the full benchmark.
#
# The historical note below (why this project once believed gzip itself
# was implicated in a real boot bug, and what the bug actually was) is
# kept for context - that investigation is unrelated to and superseded
# by the real performance measurement above, not a reason to doubt this
# switch.
#
# gzip was adopted earlier in this project's history when a real boot
# hit "RAMDISK: Couldn't find valid RAM disk image starting at 0."
# right after the EFI loader's own initrd delivery - at the time this
# looked like it might be a kernel xz-support gap, since the archive
# itself checked out fine offline (`xzcat ... | cpio -itv` showed real
# XZ, /init at the root, correct mode). The REAL bug, found later the
# same session, was entirely different and upstream of compression
# altogether: efi/initrd.c was using gnu-efi's CopyMem_1 across an ABI
# mismatch that silently copied nothing, handing the kernel a buffer of
# firmware poison bytes instead of the real initrd - which a kernel
# decompressor of any kind would equally fail to recognize. Once that
# was fixed (see initrd.c's own comment), initrd delivery started
# working correctly regardless of compression, and this had never
# actually been an xz-vs-gzip
# problem to begin with.
mkinitfs -i "$REPO_ROOT/init/init" -P "$BUILD_DIR/features.d" \
    -F "$FEATURES" -C gzip -o initramfs.img "$KVER"

# Explicit, immediate format check - the exact class of bug a real
# pre-release audit found just below (the verification step a few
# lines down was still extracting with `xz -dc` after this line had
# already been switched to `-C gzip`, a leftover from before the
# switch that unit/Go tests never caught since nothing in them runs
# this far). Checking the real magic bytes right here, right after
# mkinitfs produces the file, means a future accidental compression/
# tooling mismatch fails loudly at the exact point it was introduced,
# not several steps later with a confusing decompressor error.
initramfs_magic="$(od -An -tx1 -N2 initramfs.img | tr -d ' ')"
if [ "$initramfs_magic" != "1f8b" ]; then
    echo "initramfs.img does not start with the gzip magic bytes (1f8b, got $initramfs_magic) - mkinitfs's own -C flag and this script's later extraction/verification steps have drifted apart; keep them in sync" >&2
    exit 1
fi

# --- verify menu.py's own import chain against what actually got bundled --
# Catches, at build time with no VM/hardware needed, exactly the
# failure mode this project hit repeatedly in one real evening of
# hardware boots: `python3 /menu.py` panicking/falling back on
# ModuleNotFoundError (operator, _weakrefset, _posixsubprocess,
# _py_warnings, tempfile, shutil, fnmatch, bisect, weakref, random,
# _random - one at a time, one real reboot each, before this existed).
# Extracts the EXACT archive that would boot, chroots into it, and
# imports menu.py using ONLY the files this build actually copied in -
# the chroot's own trimmed python3/lib-dynload/stdlib subset, not this
# container's full, untrimmed python3 install (which would silently
# hide a missing module this build's own surgical file list forgot).
# `import menu`, not `python3 /menu.py`, deliberately - menu.py's own
# `if __name__ == "__main__":` guard means importing it as a module
# runs every top-level import without also trying to launch the real
# TUI (which needs a real terminal `dialog` doesn't have here).
verify_dir="$BUILD_DIR/initramfs-verify"
rm -rf "$verify_dir"
mkdir -p "$verify_dir"
# gzip, not xz: this project's OWN rescue initramfs.img is gzip now
# (see this file's own "-C gzip, NOT xz" comment above) - a real
# pre-release audit found this line had been left extracting with xz,
# a leftover from before that switch, which would fail outright
# against the actual mkinitfs-produced artifact on the very next real
# build. Exactly why this step exists: unit/Go tests all stayed green
# throughout (nothing in them ever runs this far), so only a REAL
# build catches a break here.
(cd "$verify_dir" && gzip -dc "$BUILD_DIR/initramfs.img" | cpio -idm --quiet)
if ! chroot "$verify_dir" /bin/sh -c 'cd / && exec python3 -c "import menu"' \
        2>"$BUILD_DIR/menu-import-check.log"; then
    cat "$BUILD_DIR/menu-import-check.log" >&2
    echo "menu.py failed to import using only the files bundled into initramfs.img (see traceback above) - add the missing module to build.sh's python file list" >&2
    exit 1
fi
echo "menu.py import check passed against the actual bundled initramfs contents"

# --- verify every "generate content, list it for mkinitfs" pair actually
# landed - the exact class of bug that let /etc/apk/repositories ship
# completely missing for a real while: its CONTENT was written (the
# `cat > /etc/apk/repositories` below) but the matching `echo` entry in
# alpine-zfsboot.files was simply never added, so mkinitfs never copied it in
# at all - confirmed the hard way on a real boot (`ls /etc/apk/repo*`
# => No such file or directory), not caught by the menu.py import
# check above since that only exercises Python's own import graph, not
# ordinary data files apk/dialog/etc. need. Same already-extracted
# $verify_dir as that check, no extra cost - just a real, direct
# existence check for the things this file has historically gotten
# wrong before, so it fails the BUILD instead of a real boot next time.
for f in /etc/apk/repositories /etc/apk/keys /etc/apk/world /etc/ssl/certs /etc/ssl/cert.pem \
         /lib/apk/db/installed /etc/dialogrc /etc/alpine-zfsboot-build-stamp /etc/alpine-zfsboot-version \
         /usr/share/zfs/compatibility.d /etc/shells /boot/alpine-zfsboot /fix-kexec-dtb.py; do
    if [ ! -e "$verify_dir$f" ]; then
        echo "$f is missing from the bundled initramfs - its content-generation and its alpine-zfsboot.files manifest entry have drifted apart, add/fix the missing one" >&2
        exit 1
    fi
done
echo "bundled-file presence check passed for apk/dialog/version-stamp files"

# The CLI binary specifically also needs its execute bit to actually be
# runnable from the rescue shell, not just present - cp preserves
# source permissions rather than guaranteeing +x on its own, and
# out/alpine-zfsboot-$ARCH's mode depends on whatever `go build` (or a
# umask) happened to leave it as.
if [ ! -x "$verify_dir/boot/alpine-zfsboot" ]; then
    echo "/boot/alpine-zfsboot is bundled but not executable" >&2
    exit 1
fi

# /etc/shells existing isn't enough on its own - dropbear's checkusername()
# does a byte-exact strcmp against getusershell() entries, so a stray
# trailing space, a missing newline, or the wrong path here is exactly as
# broken as the file not existing at all (see this same file's own line
# generating it for the full "why"). Confirmed against the actual bundled
# initramfs content, not just the manifest.
if ! grep -qx "/alpine-zfsboot-shell" "$verify_dir/etc/shells" 2>/dev/null; then
    echo "/etc/shells is bundled but doesn't contain an exact '/alpine-zfsboot-shell' line - dropbear will refuse every rescue login outright (checkusername() against getusershell()), before any auth method is even offered" >&2
    exit 1
fi
echo "/etc/shells content check passed (dropbear's own login-shell gate)"

# /etc/ssl/certs above only confirmed the DIRECTORY exists, not that it
# has any real content - a real, confirmed gap: a build run on a host
# that never had ca-certificates-bundle installed first (see this
# file's own comment further up on why that specific package, not
# plain ca-certificates, is what actually needs to already be present
# on the BUILD host before mkinitfs runs - alpine-zfsboot.files' own
# `echo /etc/ssl/certs` entry just captures whatever's really there at
# build time, nothing more) would still pass an `[ -e ]` check with a
# genuinely empty directory, then ship a rescue image that LOOKS like
# it has a CA trust store but has nothing in it - every single HTTPS
# fetch (apk, wget) inside a real boot of that image fails with "TLS:
# server certificate not trusted", with nothing at build time ever
# having indicated a problem.
if [ -z "$(find "$verify_dir/etc/ssl/certs" -type f -print -quit 2>/dev/null)" ]; then
    echo "/etc/ssl/certs is bundled into the initramfs but contains no files at all - the build host this ran on was very likely missing ca-certificates-bundle before build.sh started, so the resulting rescue image would have no real CA trust store and every HTTPS fetch inside it would fail with 'TLS: server certificate not trusted'" >&2
    exit 1
fi
echo "ca-certificates bundle presence check passed (/etc/ssl/certs has real content)"

# --- verify the encrypted-root kexec handoff's own assumptions against a REAL,
# stock (unmodified) Alpine initramfs - not assumed to still hold from
# whenever this comment was last updated, and not left to be discovered
# from a confused user's real boot report months from now. boot-
# dataset.sh's own kexec-handoff block (see its header comment, grep
# for "Encrypted-root kexec handoff") depends on three facts about a
# TARGET system's own, separately-built initramfs-$KERNEL_SUFFIX that
# this project does NOT control and Alpine makes no stability promise
# about: the zfs binary living at exactly usr/sbin/zfs, its own stock
# initramfs-init still calling it as a bare "zfs load-key
# $encryptionroot" with no flags, and "-L file://..." still being a
# valid override on that exact zfs CLI build. If any of these silently
# changes in a future mkinitfs/zfs package release, boot-dataset.sh's
# own runtime fallback degrades GRACEFULLY (no handoff built, just
# today's ordinary double-prompt - see that block's own comment) -
# correct behavior at boot time, but silent, which is exactly what
# this check exists to stop being the ONLY way this project finds out.
#
# Built the same way any real alpine-install-zfs.sh run's own mkinitfs
# invocation would: Alpine's OWN stock initramfs-init - no `-i`
# override here, deliberately NOT this project's own init/init - the
# same "zfs" feature, the same KVER already resolved above.
echo "verifying encrypted-root kexec handoff assumptions against a real stock Alpine initramfs..."
mkinitfs -P "$BUILD_DIR/features.d" -F "base zfs ata nvme scsi usb virtio" \
    -C gzip -o "$BUILD_DIR/target-like-initramfs.img" "$KVER"
target_verify_dir="$BUILD_DIR/target-like-initramfs-verify"
rm -rf "$target_verify_dir"
mkdir -p "$target_verify_dir"
(cd "$target_verify_dir" && gzip -dc "$BUILD_DIR/target-like-initramfs.img" | cpio -idm --quiet)

if [ ! -x "$target_verify_dir/usr/sbin/zfs" ]; then
    echo "boot-dataset.sh's kexec handoff assumes usr/sbin/zfs exists in a target system's own initramfs - it does not in a freshly-built stock one (mkinitfs's own zfs feature or the zfs package's own file layout has changed) - find the real path and update it in boot-dataset.sh's handoff block (and this check) together" >&2
    exit 1
fi

if [ ! -f "$target_verify_dir/init" ] || ! grep -q 'zfs load-key' "$target_verify_dir/init"; then
    echo "boot-dataset.sh's kexec handoff assumes Alpine's own stock initramfs-init calls 'zfs load-key' directly on an encrypted root - a freshly-built stock initramfs's own /init no longer matches that pattern (Alpine's own mkinitfs has changed its ZFS unlock flow) - re-read prepare_zfs_root() in the new /init and update boot-dataset.sh's handoff block to match" >&2
    exit 1
fi

# Static compatibility probe against the actual extracted zfs CLI
# binary's own compiled-in usage text - NOT an execution-based probe.
# An earlier version of this check ran `chroot ... zfs load-key -L
# file:///nonexistent-key nonexistent/pool` and treated anything other
# than an "invalid option" message as proof `-L` was accepted - but
# real zfs's own main() calls libzfs_init() before it ever gets to
# parsing load-key's own options, and libzfs_init() itself fails first
# in this build container (no /dev/zfs, no loaded zfs kernel module,
# by design - this is a build step, not a real machine with a real
# pool) with "The ZFS modules are not loaded", a message that also
# doesn't match the "invalid option" grep - so that version of this
# check silently declared success without -L ever having been reached
# at all, defeating its own purpose. Grepping the binary's own embedded
# usage string instead needs no kernel module, no /dev/zfs, and no
# execution at all - `load-key`'s own compiled-in usage text documents
# every option it accepts as literal, static content in the binary.
if ! strings "$target_verify_dir/usr/sbin/zfs" 2>/dev/null | grep -qi -- 'load-key.*-L\|-L.*load-key'; then
    echo "boot-dataset.sh's kexec handoff assumes 'zfs load-key -L file://...' is still a valid override on the target's own zfs CLI - the extracted binary's own usage text no longer documents a -L option alongside load-key (this Alpine branch's zfs package may have renamed/removed it) - update the handoff block in boot-dataset.sh to match whatever replaced it" >&2
    exit 1
fi
echo "encrypted-root kexec handoff assumptions verified against a real stock Alpine initramfs"

# --- UEFI stub assembly (Unified Kernel Image via objcopy) ----------------
# Section order is .cmdline, .initrd, .linux - no .osrel/.splash/.dtb,
# this project has none of those.
#
# VMAs are fixed, hardcoded offsets, deliberately spaced far apart
# (0x30000/0x2000000/0x3000000) so kernel/initrd growth can't collide
# with anything or with the loader's own (tiny, a few tens of KiB)
# footprint. efi/pe_sections.c locates each section at runtime by
# name, not by these specific values - they only need to not overlap,
# not match anything the loader itself assumes.
objcopy_args=""
add_section() {
    name="$1"; file="$2"; vma="$3"
    objcopy_args="$objcopy_args --add-section $name=$file --change-section-vma $name=$vma"
}

# CONSOLE_CMDLINE always puts BOTH console= entries on the cmdline -
# an earlier version of this project also built separate single-
# console (vga-only/serial-only) variants selectable via a
# CONSOLE_NAME build-time choice, three variants x two arches, six
# .EFI files total. Dropped: init/init's own select_console() and
# menu.py's switch_console() already make the single "auto" build
# behave correctly for every case a single-console build was ever for
# (it picks whichever console is actually being watched, honors a
# persisted preference, and switches live, FreeBSD-loader-style) - the
# separate builds were never buying anything the always-both-consoles
# build didn't already handle, just multiplying the release matrix.
# The kernel logs boot messages to every console listed here, but only
# the LAST one becomes /dev/console - that's exactly what
# select_console() picks between.
# nomodeset was tried here and REMOVED - a real Hetzner Cloud CAX
# (aarch64) boot still showed "display output not active" with it set,
# and this project ships no vendor KMS driver at all (see the driver-
# exclusion comment above) - simpledrm is the only DRM driver in this
# image either way, and upstream's own nomodeset-handling patch series
# ("drm: Make all drivers to honour the nomodeset parameter") makes
# simpledrm the one driver that keeps working under nomodeset
# regardless - so it was always a no-op here. Do not re-add it.
#
# The real "display output not active" cause, found on real Hetzner CAX
# hardware, was two independent bugs stacked on top of each other:
#
# 1. Console ORDER. select_console() in init/init picks ACTIVE_TTY by
#    "last console= wins", the kernel's own convention (see its comment
#    for the /sys/class/tty/console/active mechanics). aarch64 used to
#    list tty0 first, ttyAMA0 last, same shape as x86_64 below - which
#    made ttyAMA0 (serial, unreachable on Hetzner Cloud - no serial
#    console access there at all) the interactive target instead of
#    tty0 (the only console the Hetzner VNC viewer shows). Confirmed on
#    a real boot: with the old order, ACTIVE_TTY/fd 0/1/2/the kernel's
#    own preferred console all agreed on ttyAMA0, and the on-screen menu
#    never responded to a single keypress. x86_64 was never affected -
#    the interactive target being "wrong" there doesn't matter, because
#    vgacon backs tty0 independently of console= order entirely, so
#    x86_64 gives no signal either way on this question. Swapped to
#    ttyAMA0 first, tty0 last for aarch64 - confirmed via the same
#    real-boot instrumentation (ACTIVE_TTY, /proc/consoles' C flag, and
#    fd 0/1/2 all landing on tty0) that this is now correct.
#
# 2. Missing USB/HID input stack. Even with console order fixed, the
#    on-screen menu still didn't respond to a keypress - /proc/bus/
#    input/devices and lsmod on a real boot showed no bound driver for
#    the emulated USB keyboard at all (QEMU's aarch64 "virt" machine
#    exposes input over USB/xHCI, not a PS/2-style controller x86 has
#    built in). Fixed by loading the xhci/ehci/usbhid/hid_generic/
#    usbkbd/virtio_input modules (see init/init's modprobe loop and this
#    file's own features.d manifest below) - confirmed via the same
#    /proc/bus/input/devices check that the keyboard now enumerates, and
#    the on-screen menu responds.
#
# Both were necessary; neither alone was sufficient - fixed console
# order with no input driver still shows a dead keyboard, and a working
# input driver routed to the wrong tty (the old order) is equally dead.
#
# fbcon=nodefer (aarch64 only): Alpine's aarch64 lts kernel config sets
# CONFIG_FRAMEBUFFER_CONSOLE_DEFERRED_TAKEOVER=y (confirmed by reading
# the actual upstream config, not guessed) - fbcon normally waits for a
# "real" DRM driver to register before binding to any framebuffer, to
# avoid a flicker/mode-switch when handing off from an early boot
# framebuffer to the real one. Kept as defense-in-depth against a
# takeover-timing edge case this project's own real-hardware testing
# never actually hit (virtio_gpu's own fb0 came up fine without it too)
# - harmless either way, and there's no evidence against keeping it.
case "$ARCH" in
    x86_64)  CONSOLE_CMDLINE="console=tty0 console=ttyS0,115200n8" ;;
    aarch64) CONSOLE_CMDLINE="console=ttyAMA0,115200n8 console=tty0 fbcon=nodefer" ;;
esac

# kexec_load_disabled=0: Alpine's own linux-lts/linux-virt kernels
# carry a downstream patch (not upstream Linux - see
# 0003-kexec-add-kexec_load_disabled-boot-option.patch in aports)
# that flips kexec_load's default from enabled to DISABLED, returning
# "Operation not permitted" from every `kexec -l` regardless of
# Secure Boot state (confirmed on real hardware: SB off, still
# blocked). The sysctl itself is deliberately one-way (only ever
# writable TO 1, confirmed the hard way - `echo 0 >
# /proc/sys/kernel/kexec_load_disabled` fails with EINVAL from a live
# boot), but this patch's own cmdline parser runs in init/main.c
# before any of that applies, and accepts either value - this is the
# ONLY way to get kexec_load back on Alpine, not a runtime fix.
# quiet: hides the kernel's own boot-time chatter (ACPI/PCI/SCSI probe
# messages, module load noise) from the console - real user feedback
# was that this noise buried this project's own countdown/menu prompt
# ("hverfur í fullt af boot skilaboðum", "ekki alveg augljóst"), making
# it easy to miss. Safe to hide: every message still lands in the
# kernel's own ring buffer regardless of console loglevel, and
# menu.py's own Diagnostics screen shows the full `dmesg` on demand -
# nothing is actually lost, just not thrown at the screen unasked.
# This does NOT affect /init's own msg() output - that goes through a
# plain userspace `echo` to the console, not printk, so pool-import
# status, errors, and the countdown banner all stay visible exactly as
# before; only the kernel's own noise is quieted. Override per-build
# with EXTRA_CMDLINE if verbose kernel output is needed again for
# debugging (e.g. `EXTRA_CMDLINE="loglevel=7" just build ...`).
# alpine-zfsboot.version=<repo version.txt> / alpine-zfsboot.buildstamp=$BUILD_STAMP -
# TWO separate cmdline fields, deliberately not one: confirmed real
# confusion otherwise (a real boot's own cmdline showed
# "alpine-zfsboot.version=20260910T211808Z" and reasonably read as "the
# project's version is a build timestamp?", when 0.1.0 is the actual
# release identifier - the same conflation _project_version()/
# _build_stamp() in menu.py already keep apart for the boot-screen
# banner, mirrored here for cmd/tool's own `version`/`check`/`update`.
# buildstamp is what cmd/tool's Read()/upToDate comparison actually
# uses (see internal/cmdline's own comment) - version is display-only.
printf '%s root=ZFS=%s ro quiet kexec_load_disabled=0 alpine-zfsboot.pool=%s alpine-zfsboot.timeout=%s alpine-zfsboot.version=%s alpine-zfsboot.buildstamp=%s %s\n' \
    "$CONSOLE_CMDLINE" "$POOL/ROOT/alpine" "$POOL" "$MENU_TIMEOUT" "$(cat "$REPO_ROOT/version.txt")" "$BUILD_STAMP" "${EXTRA_CMDLINE:-}" > cmdline.txt

# The embedded .cmdline section needs its own, separate copy: efi/'s
# loader (see cmdline.c) reads it as a plain NUL-terminated ASCII C
# string straight out of the section's raw bytes. cmdline.txt itself
# stays newline-terminated, plain text, for the loose GRUB-chainload
# copy below - only this section-specific copy gets the newline
# stripped and a NUL appended.
tr -d '\n' < cmdline.txt > cmdline.section
printf '\000' >> cmdline.section

# The .linux/.initrd gap (0x2000000-0x3000000, 16MiB) is fixed, not
# computed from the kernel's own size like the old scheme was - a
# kernel that grows past this budget would silently overlap .initrd
# instead of failing loudly, so check for real rather than assume a
# current-day linux-lts vmlinuz (~14.5MiB) always fits.
kernel_size="$(wc -c < "$KERNEL")"
[ "$kernel_size" -lt $((0x1000000)) ] || {
    echo "kernel ($KERNEL, $kernel_size bytes) exceeds the 16MiB budget between the fixed .linux (0x2000000) and .initrd (0x3000000) VMAs below" >&2
    exit 1
}

add_section .cmdline cmdline.section 0x30000
add_section .initrd initramfs.img 0x3000000
add_section .linux "$KERNEL" 0x2000000

OUT_FILE="$OUT_DIR/alpine-zfsboot-${ARCH}.EFI"
# shellcheck disable=SC2086 # objcopy_args is a deliberately unquoted
# word-split argument list, built up above for exactly this purpose.
objcopy $objcopy_args "$STUB" "$OUT_FILE"

echo "built: $OUT_FILE"

# Loose kernel + initramfs pair, alongside the combined .EFI - for
# chainloading from an existing GRUB stage 1 (or any other bootloader
# that wants to load a Linux kernel + initrd directly, cmdline given
# separately) instead of the firmware loading this project's own .EFI
# straight off the ESP. Same cmdline this build's combined .EFI
# carries, just not baked into a UKI.
cp "$KERNEL" "$OUT_DIR/alpine-zfsboot-${ARCH}-vmlinuz"
cp initramfs.img "$OUT_DIR/alpine-zfsboot-${ARCH}-initramfs.img"
cp cmdline.txt "$OUT_DIR/alpine-zfsboot-${ARCH}-cmdline.txt"
echo "built: $OUT_DIR/alpine-zfsboot-${ARCH}-{vmlinuz,initramfs.img,cmdline.txt} (for GRUB chainload)"

# --- legacy BIOS/GPT boot path (x86_64 only - no aarch64 equivalent: -----
# legacy BIOS itself doesn't exist there) -----------------------------------
# A second, completely independent way to reach the exact same kernel+
# initramfs this build already produced above, for GPT-partitioned disks
# on hardware with no UEFI at all - see bios/ for the full design (stage1:
# the protective-MBR boot sector, fixed-LBA-read only, no GPT parsing;
# stage2: loaded from a "BIOS boot partition", does the real GPT lookup,
# disk reads, and Linux/x86 boot-protocol handoff into the kernel).
# Deliberately built entirely in-house, not by vendoring an existing
# bootloader's own stage code.
if [ "$ARCH" = "x86_64" ]; then
    echo "building legacy BIOS boot code (stage1/stage2/stage-iso)..."
    rm -rf "$BUILD_DIR/bios"
    cp -r "$REPO_ROOT/bios" "$BUILD_DIR/bios"
    # `make clean` first, unconditionally - confirmed the hard way: a
    # real build's own log showed several .o files (gpt.o, bootparams.o,
    # console.o, switch32.o, and their iso_*.o equivalents) NOT being
    # recompiled at all, going straight from `cp -r` into the final
    # `ld` link commands - `rm -rf "$BUILD_DIR/bios"` only clears
    # BUILD_DIR's own copy from a PREVIOUS run of this script, it does
    # nothing about stray .o/.elf/.bin files already sitting in
    # $REPO_ROOT/bios itself (e.g. from a local, outside-Docker `make`
    # run - not tracked by git, per .gitignore, but very much still on
    # disk and very much still copied by `cp -r` regardless of
    # .gitignore, which only affects git, never plain file copies).
    # Once copied, make's own up-to-date check can easily conclude a
    # stray .o is newer than its .c source (their timestamps come from
    # the SAME cp -r moment) and skip rebuilding it - silently shipping
    # stale object code with no error or warning at all. `make clean`
    # here removes every such possibility before the real build below
    # ever runs, regardless of where the staleness came from.
    make -C "$BUILD_DIR/bios" clean
    # stage-iso.bin: an alternative to stage1.bin+stage2.bin for
    # booting the exact same boot-blob from an El Torito "no emulation"
    # CD-ROM boot entry instead of a real GPT disk - see iso.sh and
    # bios/Makefile's own comment on this target.
    #
    # ZFSBOOT_VERSION: the same repo version.txt file already read below
    # for /etc/alpine-zfsboot-version, passed down here too so the
    # BIOS stage2 boot banner shows the real semver instead of its own
    # Makefile's "dev" fallback - one file, one value, used everywhere
    # (see bios/Makefile's own comment on why it can't just read
    # "../version.txt" itself).
    make -C "$BUILD_DIR/bios" stage1.bin stage2.bin stage-iso.bin \
        ZFSBOOT_VERSION="$(cat "$REPO_ROOT/version.txt")"

    # No more boot-blob packing step - stage2's own FAT32 reader
    # (bios/fat.c) reads the kernel/initramfs.img/cmdline.txt already
    # built above for the UEFI path DIRECTLY off the same canonical
    # alpine-zfsboot FAT/ESP partition UEFI mode already uses (see
    # bios/gpt.h's ZFSBOOT_ESP_TYPE_GUID and this project's own
    # architecture-decision writeup) - nothing to repackage into a
    # separate raw format any more. The alpine-installer repo's own
    # `partition_disk()`/artifact-install step is what actually copies
    # these loose files onto a real machine's FAT partition at install
    # time, at EFI/ALPINE/{KERNEL,INITRD,CMDLINE}; see that repo.

    cp "$BUILD_DIR/bios/stage1.bin" "$OUT_DIR/alpine-zfsboot-${ARCH}-bios-stage1.bin"
    cp "$BUILD_DIR/bios/stage2.bin" "$OUT_DIR/alpine-zfsboot-${ARCH}-bios-stage2.bin"
    cp "$BUILD_DIR/bios/stage-iso.bin" "$OUT_DIR/alpine-zfsboot-${ARCH}-bios-stage-iso.bin"
    echo "built: $OUT_DIR/alpine-zfsboot-${ARCH}-bios-{stage1,stage2,stage-iso.bin}"
fi

# --- ISO wrapper -----------------------------------------------------------
# Same .EFI, just wrapped so it's a valid boot target somewhere a bare
# .EFI file isn't: BMC/IPMI virtual media, a real USB stick, a VM's
# virtual CDROM. Pure packaging - see iso.sh's own header for why.
# x86_64 also gets legacy-BIOS boot on the ISO itself (stage-iso.bin,
# already on disk from the BIOS section above, plus the same loose
# kernel/initramfs.img/cmdline.txt already built above for the UEFI
# path - iso.sh copies those onto the SAME appended FAT image the UEFI
# entry uses, see its own header comment) - aarch64 has no legacy-BIOS
# equivalent, so iso.sh gets called with just the two required
# arguments there, same as always.
if [ "$ARCH" = "x86_64" ]; then
    "$REPO_ROOT/iso.sh" "$OUT_FILE" "$ARCH" \
        "$BUILD_DIR/bios/stage-iso.bin" "$KERNEL" initramfs.img cmdline.txt
else
    "$REPO_ROOT/iso.sh" "$OUT_FILE" "$ARCH"
fi
