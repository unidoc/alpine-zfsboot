#!/usr/bin/env python3
"""alpine-zfsboot boot menu.

Always shown - FreeBSD-loader-style, one consistent first frame every
boot, not a separate fast automatic path plus a menu only reached on a
keypress (an earlier version of this project worked that way; see
/init's own header comment for why that changed). The REAL interactive
dialog --menu is visible from the very first frame (an earlier version
of this file printed a plain-text countdown/item-list first and only
showed the real widget once a key was pressed - real, repeated user
feedback was that this was wrong: the menu itself must be there
immediately, not a text preview of it).

main()'s own countdown is built from repeated dialog --menu calls
(see _countdown_menu()), not one call with dialog's --timeout set to
the full duration - confirmed for real (a genuine dialog binary,
driven through a real pseudo-terminal, both ways) that: (a) navigating
with arrow keys and then actually confirming a choice (Enter) is
recognized correctly and returns immediately, at any point during an
active --timeout, so a real selection is never blocked or delayed by
a countdown running underneath it; but (b) if nothing is ever
CONFIRMED for the entire configured duration, dialog's own --timeout
fires at the end regardless of any navigation that happened along the
way, since the widget itself has no notion of "activity" short of an
actual OK/Cancel/ESC - its own stderr says the literal word "timeout"
either way, whether the operator sat fully idle or spent the whole
tick navigating and just never pressed Enter. Confirmed a REAL, twice-
reported bug from exactly this: someone visibly navigating (arrow
keys, moving to a different item, waiting there) still got booted out
from under them, because dialog alone genuinely cannot tell "someone
is here" from "no one is here" without an actual confirm. Splitting
the full countdown into repeated dialog calls, each with its own
displayed remaining-time text, is what makes the countdown visibly
tick down inside a genuinely live, fully navigable widget rather than
a static "N seconds" message - a real confirmed selection on any tick
dispatches immediately regardless of which tick it happens on; and
_run_dialog_with_activity()'s own pty relay (see that function) is
what closes the other gap - detecting real keystrokes independently
of dialog's own confirm/cancel/timeout reporting, so ANY activity at
all silences the countdown for good, not just a confirmed one.

Each tick's own --timeout is TICK_SECONDS long, deliberately NOT
1 second (an earlier version used exactly that, confirmed a real,
serious bug, not just a cosmetic one): every tick is a fresh,
independent dialog invocation with no memory of the previous one (see
_countdown_menu()'s own comment) - the highlighted item resets to the
default on every new tick, so a real confirmed selection (arrow key,
then Enter) has to happen ENTIRELY within a single tick's window to
ever be recognized at all. With a 1-second window, accounting for real
process spawn/redraw overhead, that was essentially never achievable
by a real human - this, not the cosmetic highlight-reset itself, is
what actually explained "I press keys and it just boots anyway"
reports. TICK_SECONDS is long enough to give a real, comfortable
confirm window each tick.

This script never runs `kexec` itself - boot-dataset.sh is the one
authoritative implementation of "mount, find kernel, kexec", shared
with /init's own automatic-boot fallback. This file is presentation +
selection only: list things, let the user pick, then exec the same
shell script /init would have called directly.

Rendering goes through the real `dialog` binary (see the dialog_*()
helpers below), not Python's own curses module - an earlier version of
this file used curses directly and got it working, but only after
chasing several real ncurses/terminfo edge cases one at a time on real
hardware (setupterm(), curs_set(), endwin() all raising in ways a
hand-rolled draw loop had to work around individually). dialog is the
same battle-tested tool behind Debian-installer and countless other
rescue/installer TUIs, handles all of that internally, and needs none
of that defensive code here - each dialog invocation is a fresh,
self-contained subprocess that has fully exited (and restored the
terminal on its own) by the time control returns to this script, so
there is no persistent screen state to suspend/resume around a
`zfs load-key` prompt, a `chroot` shell, or anything else that needs
the real terminal for a moment.
"""
import datetime
import fcntl
import os
import pty
import re
import select
import struct
import subprocess
import sys
import tempfile
import termios
import time
import tty

POOL = os.environ.get("ALPINE_ZFSBOOT_POOL", "zroot")
BOOTFS = os.environ.get("ALPINE_ZFSBOOT_BOOTFS", f"{POOL}/ROOT/alpine")
# The ONE place this file names the rescue network interface - every
# other use below references this constant rather than its own "eth0"
# literal. rescue-ssh.sh/net-config.sh hardcode the identical interface
# name on the shell side (`net_config eth0`) - there is currently no
# runtime-shared source of truth across the shell/Python boundary for
# this (and introducing one would mean touching the frozen networking
# architecture), so this is the practical middle ground: one named
# constant here instead of several independent "eth0" literals that
# could quietly drift apart from each other.
RESCUE_IFACE = "eth0"
# Set by /init when `zpool import`/the bootfs lookup itself failed -
# see its own header comment there. A real, reported usability
# problem otherwise: /init used to `die` straight to a bare bash
# prompt in this exact situation, before menu.py ever ran at all,
# which gives no indication of WHAT went wrong or what else is
# available (Diagnostics, Network, a properly job-control-capable
# Recovery shell, Deploy to receive a different pool) short of already
# knowing the right zfs/zpool commands. Every pool-dependent action
# below checks this and shows a plain msgbox instead of proceeding
# into code that assumes a real, mounted-or-mountable BOOTFS exists.
POOL_IMPORT_ERROR = os.environ.get("ALPINE_ZFSBOOT_POOL_IMPORT_ERROR", "")
# Failed-boot detection ("bootcheck") - set by /init's own single
# decision point (see its own comment there) whenever the configured
# BOOTFS has exceeded its consecutive-unconfirmed-boot-attempts
# threshold and rescue SSH is either up or explicitly forced via
# alpine-zfsboot.bootcheck=force. A non-empty string doubles as the
# flag, same convention as POOL_IMPORT_ERROR above - both are handled
# together everywhere they matter (see main()) since both mean the
# same thing to this file: "don't auto-boot, something needs a human."
FORCED_RESCUE = os.environ.get("ALPINE_ZFSBOOT_FORCED_RESCUE", "")
# Last Boot Diagnostics, kernel-crash evidence - /init mounts this
# unconditionally, best-effort (see its own comment there). A
# kernel/platform with no efi-pstore backend just leaves this empty or
# missing, which previous_boot_evidence() below already treats as
# "nothing to show", not an error - same STUB_ROOT-style override
# convention as every other absolute path in this file, for tests.
PSTORE_ROOT = os.environ.get("STUB_PSTORE_ROOT", "/sys/fs/pstore")
# Read live from the kernel's own sysfs file, NOT via an
# ALPINE_ZFSBOOT_PSTORE_BACKEND environment variable exported by /init -
# an earlier version of this did exactly that, and it was a real,
# adversarially-found bug: dropbear's clearenv() wipes every variable
# /init exported for a rescue-SSH session (see ACTIVE_TTY's own,
# already-documented instance of this exact problem above), so a REMOTE
# operator - the primary audience this feature exists for - would
# always have seen "unsupported" here regardless of the real hardware
# state. Unlike ACTIVE_TTY (a fact about which process /init originally
# attached, genuinely lost once that process's environment is wiped),
# backend registration is a kernel-global, session-independent fact -
# reading the sysfs file fresh works identically over SSH, on the local
# console, or anywhere else, with no inheritance problem at all.
# CONFIG_PSTORE=y alone (Alpine's own kernel has this, built in) mounts
# the pstore filesystem successfully with ZERO backend registered, so
# "did the mount succeed" is never a valid substitute for this check.
PSTORE_BACKEND_PATH = os.environ.get(
    "STUB_PSTORE_BACKEND_PATH", "/sys/module/pstore/parameters/backend"
)


def _pstore_backend():
    """Empty return means "no backend registered" - legacy BIOS (no EFI
    variables at all), a kernel without the module loaded, or a
    modprobe/mount failure all collapse to this same, honestly-reported
    state.
    """
    try:
        with open(PSTORE_BACKEND_PATH) as f:
            return f.read().strip()
    except OSError:
        return ""
BOOT_DATASET_SH = "/boot-dataset.sh"
# The one shared implementation of "acquire a ZFS native-encryption
# passphrase interactively and stage it for the kexec handoff" (see its
# own header comment) - ensure_key_loaded()/unlock_encrypted_root()/
# lock_encrypted_root() below all shell out to this instead of calling
# `zfs load-key` directly, which is the real, reported gap that used to
# exist here: a successful unlock via THIS file never staged a handoff
# secret, so a boot reached afterward would silently re-prompt in the
# target initramfs.
# Overridable the same way MNT_ROOT/PCI_DEVICES_ROOT below are - lets
# tests point this at a fake script instead of the real absolute
# /zfs-unlock path, which only exists inside a real built initramfs.
ZFS_UNLOCK_SH = os.environ.get("STUB_ZFS_UNLOCK_SH", "/zfs-unlock")
# Same STUB_ROOT pattern as MNT_ROOT below - the staged-secret path
# must be computed IDENTICALLY to zfs-unlock.sh's own zfs_key_stage_path()
# (same flattening rule) since this process and boot-dataset.sh/
# zfs-unlock check the same file from two separate languages - see
# _zfs_key_stage_path()'s own comment.
ZFS_KEY_STAGE_ROOT = os.environ.get("STUB_ROOT", "")
# Empty at real boot, same reasoning and same env var as /init's own
# ROOTFS (see there) - only set by tests/run-tests.sh, so list_kernels()
# below never needs real root to mkdir under /mnt on a dev host.
MNT_ROOT = os.environ.get("STUB_ROOT", "") + "/mnt/root"
# Same STUB_ROOT pattern, for _probe_network_drivers() below - lets
# tests point this at a fake PCI device tree instead of this dev
# host's own real /sys.
PCI_DEVICES_ROOT = os.environ.get("STUB_ROOT", "") + "/sys/bus/pci/devices"
# Whole-number seconds for the main menu's own countdown (see main())
# - /init's own alpine-zfsboot.timeout= cmdline option, the exact same value
# that used to gate a separate pre-menu prompt before this project
# moved to always showing the menu (see /init's header comment).
#
# Parsed through _parse_menu_timeout() rather than a bare int(...) at
# module scope - a full source audit found that a bare int(...) here
# raises ValueError at IMPORT time on any non-numeric value, and this
# value comes from /init's own MENU_TIMEOUT shell variable (init:426,
# `MENU_TIMEOUT="${kv#alpine-zfsboot.timeout=}"`, zero validation there
# either), which in turn can come from a PERSISTENT source - a hand-
# edited alpine-zfsboot.timeout= line in EFI/ALPINE/config - not just a
# one-boot kernel cmdline typo. An import-time exception here happens
# before main()'s own try/except (further down) exists to catch
# anything, so one bad character in that config file killed menu.py on
# EVERY subsequent boot, on every console: no recovery shell, no
# console switch, no previous-boot diagnostics, no unlock screen - the
# entire rescue menu, permanently gone, with /init's own fallback being
# straight to automatic boot (see /init's own "menu.py exited... falling
# back to automatic boot" log line). _parse_menu_timeout() can never
# raise - an unparseable value falls back to the same documented
# default a missing/empty value already used, and says so on stderr
# (captured in /init's own boot log) instead of failing silently.
def _parse_menu_timeout():
    raw = os.environ.get("ALPINE_ZFSBOOT_MENU_TIMEOUT", "10") or "10"
    try:
        return int(raw)
    except ValueError:
        print(f"menu.py: alpine-zfsboot.timeout={raw!r} is not a whole number - using the default (10s) instead", file=sys.stderr)
        return 10


MENU_TIMEOUT = _parse_menu_timeout()
# Which real tty /init actually attached this process to (see /init's
# own select_console()) - switch_console() below needs to know this to
# compute "the other one(s)". Empty/wrong over SSH - dropbear's own
# clearenv() (see rescue-ssh.sh's own comment) wipes this exact
# variable along with the rest of /init's exported environment, so this
# always falls back to "tty0" over SSH regardless of what's actually
# true. IS_SSH_SESSION below is the real, transport-based signal this
# file uses instead wherever "is this actually SSH" - not "is
# ACTIVE_TTY unset" - is the real question (see _sanitize_term()).
ACTIVE_TTY = os.environ.get("ALPINE_ZFSBOOT_ACTIVE_TTY", "tty0")

# dropbear does NOT clear SSH_CONNECTION/SSH_TTY (see its own small
# built-in exception list to clearenv(), confirmed against the real
# bundled binary) - a direct, reliable "is this session actually SSH"
# signal, unlike ACTIVE_TTY above (which just silently defaults to a
# wrong constant once its own source variable is wiped). Used by
# _sanitize_term() to pick a sane fallback $TERM for a transport it can
# positively identify as remote, rather than guessing from a value that
# was never really about transport in the first place.
IS_SSH_SESSION = "SSH_CONNECTION" in os.environ or "SSH_TTY" in os.environ

# See this file's own header comment - the real, confirmed bug behind
# "I press keys and it just boots anyway" was this being 1 (second),
# not a cosmetic limitation of the countdown design itself.
TICK_SECONDS = 3

BACKTITLE = "alpine-zfsboot"

# A raw serial line carries NO terminal-geometry negotiation of its
# own, at ANY layer of a real connection - confirmed a real, reported
# rendering bug (box-drawing running together, lines starting at the
# wrong column, content overlapping itself) that persisted identically
# across THREE completely different local terminal emulators
# (Terminal.app, Ghostty, tmux-wrapped-Ghostty), over the SAME real
# chain: local terminal -> ssh -> `qm terminal` -> QEMU's emulated
# serial port -> this guest's /dev/ttyS0 - ruling out every one of
# those as the cause, since none of them changed the outcome at all.
# /init's own case-statement (see its comment there) forces this exact
# tty's own real winsize to a fixed 24x80 via `stty` before this
# process even starts - but that alone still leaves dialog itself free
# to independently compute its OWN idea of a widget's box size from
# whatever it reads back via TIOCGWINSZ (every dialog_*() helper below
# used to pass "0 0" - "figure out a size yourself" - for exactly this
# reason). This project no longer trusts that computation AT ALL for a
# serial console, forced tty winsize or not: every dialog widget below
# gets REAL, fixed numbers instead, so there is no longer anything for
# dialog itself to get wrong. tty0 (a real Linux vt) keeps "0 0" -
# there is no equivalent doubt there: the kernel's own vt/fbcon layer
# genuinely knows that console's real geometry, and TIOCGWINSZ against
# it has never been the reported problem.
IS_SERIAL_CONSOLE = ACTIVE_TTY != "tty0"
# Comfortably inside the 24x80 /init itself forces (1 row held back
# for dialog's own --backtitle line, drawn above the box rather than
# inside it; 1 column held back on each side as plain margin) -
# generous rather than minimal, since dialog already scrolls text/menu
# content that doesn't fit a given height (see _banner()'s own
# POOL_IMPORT_ERROR block - a real boot's own screenshot already showed
# dialog's own "54%"/"81%" scroll indicator in active use today, so
# handing it a smaller-than-auto-computed height is not a new
# capability being exercised for the first time here).
DIALOG_HEIGHT = 23 if IS_SERIAL_CONSOLE else 0
DIALOG_WIDTH = 78 if IS_SERIAL_CONSOLE else 0

# --ascii-lines: forces plain "+-|" box borders instead of the
# terminal's own ACS/line-drawing characters - confirmed the hard way
# on real hardware (and the exact bytes involved verified directly
# with a real dialog binary) that the "ansi" terminfo entry's own acsc
# mapping uses RAW HIGH-BYTE CP437 characters for box-drawing (unlike
# "linux", which uses real VT100 alternate-character-set escape
# switching) - whatever real terminal is on the other end of a serial
# connection decoding those bytes as Latin-1 instead of CP437 produces
# exactly the "Ä³ÀÙ¿Ú"-style corruption a real boot showed. Confirmed
# directly that --ascii-lines eliminates this at the source: a real
# rendered dialog widget with this flag contains ZERO non-ASCII bytes
# anywhere in its output, so there is no encoding for any terminal to
# get wrong in the first place - not a guess, verified byte-for-byte.
DIALOG_COMMON = ["dialog", "--ascii-lines", "--backtitle", BACKTITLE]

# Deliberately NOT setting DIALOGRC here - not because dialog then
# falls back to some plain, uncustomized default (it doesn't: dialog's
# own compiled-in GLOBALRC, consulted whenever DIALOGRC/$HOME/.dialogrc
# aren't set, is /etc/dialogrc, same as virtually every other distro -
# confirmed the hard way, when real hardware kept showing the exact
# same green/corrupt theme even after this env var was first removed).
# build.sh bundles init/dialogrc to exactly that GLOBALRC path, and
# THAT file - not this line - is what actually controls dialog's
# colors now (see its own header for the full story). This project
# just has no reason to also set DIALOGRC to the same path GLOBALRC
# already resolves to on its own.

# True on real UEFI firmware - the same test /init itself uses
# elsewhere (its own efi_pstore probe). Decides which of the two
# backing stores write_console_pref() below actually writes to - see
# its own docstring for why these are two genuinely different
# concepts, not two implementations of the same one.
IS_UEFI = os.path.isdir("/sys/firmware/efi")

# Same GUID/name /init's own read_efivar_console_pref() reads (see
# that function's own comment for the full reasoning - operator's
# persisted runtime choice, UEFI only, same mechanism systemd-boot's
# LoaderEntryDefault / GRUB's grubenv use).
EFIVAR_GUID = "ce0e7d88-f5ad-45e9-a195-680f5140efa5"
EFIVAR_NAME = "AlpineZfsBootConsole"
EFIVAR_PATH = f"/sys/firmware/efi/efivars/{EFIVAR_NAME}-{EFIVAR_GUID}"

# Which device /init already confirmed is the canonical alpine-zfsboot
# FAT/ESP partition (its own disambiguated LABEL=EFI scan, done once -
# see /init's own ESP-discovery comment for the full "exactly one
# candidate" reasoning) - empty if /init never found one (or found more
# than one and refused to guess). Only consulted on BIOS - see
# write_console_pref()'s own docstring.
ZFSBOOT_ESP_DEV = os.environ.get("ALPINE_ZFSBOOT_ESP_DEV", "")
ESP_CONFIG_MOUNT = "/tmp/esp-console-pref"


def _write_efivar_console_pref(tty):
    """Attributes + value in ONE write() - efivarfs's own write handler
    needs the whole thing at once (see /init's read_efivar_console_pref()
    for the exact attribute bits and why). Python's raw binary file I/O
    sidesteps the one real risk a shell printf would have here (embedding
    a NUL byte, then possibly not writing the rest of the format string in
    the same call) entirely - f.write(bytes) is one write() syscall of
    exactly those bytes, no NUL-terminated-string interpretation anywhere
    in the path. Best-effort: a write failure here is silently swallowed,
    same posture as the FAT-config path below.
    """
    try:
        with open(EFIVAR_PATH, "wb") as f:
            f.write(b"\x07\x00\x00\x00" + tty.encode("ascii"))
    except OSError:
        pass


def _write_fat_console_pref(tty):
    """Read-modify-write against EFI/ALPINE/config on the
    canonical FAT/ESP partition, preserving every other line untouched -
    this is the one persisted setting menu.py itself ever writes into
    that file (everything else in it is written once, at install time,
    by alpine-install-zfs.sh), so blowing away unrelated settings here
    would be a real regression, not a hypothetical one. Best-effort: if
    the ESP is missing, unwritable, or anything else goes wrong, this
    boot's LIVE console switch (the sys.exit(42) relaunch right after
    this call - see switch_console()) still happens regardless; only
    the "remember this for next reboot" part is lost.

    The actual write is transactional - temp file in the SAME
    directory, fsync the file, os.rename() over the real target, then
    fsync the directory too - not a plain truncate-in-place (an
    earlier version of this function did exactly that: open config_path
    directly in "w" mode, write, fsync the file descriptor only). A
    crash between truncate and the new content landing could leave
    config truncated or half-written, and even a clean file-fsync alone
    doesn't guarantee the rename's own directory-entry metadata is
    durable on every filesystem - real hardening for a file that also
    carries rescue-SSH/network settings, not just the console
    preference. Same semantics internal/espconfig.WriteFile implements
    in Go for the new alpine-zfsboot CLI's own writers - not shared
    code (different runtime), but the same contract.
    """
    if not ZFSBOOT_ESP_DEV:
        return
    subprocess.run(["mkdir", "-p", ESP_CONFIG_MOUNT], capture_output=True)
    subprocess.run(["umount", ESP_CONFIG_MOUNT], capture_output=True)
    mounted = subprocess.run(["mount", "-t", "vfat", ZFSBOOT_ESP_DEV, ESP_CONFIG_MOUNT],
                              capture_output=True)
    if mounted.returncode != 0:
        return
    try:
        config_dir = os.path.join(ESP_CONFIG_MOUNT, "EFI", "ALPINE")
        config_path = os.path.join(config_dir, "config")
        os.makedirs(config_dir, exist_ok=True)
        lines = []
        if os.path.exists(config_path):
            with open(config_path, "r", errors="replace") as f:
                lines = [ln.rstrip("\r\n") for ln in f]
        lines = [ln for ln in lines if not ln.startswith("alpine-zfsboot.console=")]
        lines.append(f"alpine-zfsboot.console={tty}")

        tmp_fd, tmp_path = tempfile.mkstemp(prefix=".alpine-zfsboot-write-", dir=config_dir)
        try:
            with os.fdopen(tmp_fd, "w") as f:
                f.write("\n".join(lines) + "\n")
                f.flush()
                os.fsync(f.fileno())
            os.rename(tmp_path, config_path)
        except OSError:
            os.unlink(tmp_path)
            raise
        dir_fd = os.open(config_dir, os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    except OSError:
        pass  # best-effort - see this function's own docstring
    finally:
        subprocess.run(["umount", ESP_CONFIG_MOUNT], capture_output=True)


def write_console_pref(tty):
    """Persists the operator's chosen console as their own runtime
    preference - UEFI NVRAM on UEFI hosts (the mechanism this project
    used originally), EFI/ALPINE/config on the canonical FAT/ESP
    partition on BIOS hosts (which have no NVRAM to persist anything
    into at all).

    Deliberately NOT the same storage either way. The FAT config file
    is the canonical, firmware-neutral MACHINE CONFIG/DEFAULT (set once
    at install time, by alpine-install-zfs.sh, same file rescue-SSH/
    network settings live in) - an operator's own "I prefer tty0 on
    THIS box" choice, made live from this menu, is a different concept,
    and on UEFI hosts it has a real, existing, firmware-native place to
    live that does that job well. An earlier version of this function
    routed BOTH cases through the FAT config file uniformly, on the
    reasoning that one storage mechanism everywhere is simpler - true,
    but it silently discarded a real UEFI capability (a persisted
    choice independent of whatever the installed machine config
    says) for no actual gain, since the FAT-config path was only ever
    NEEDED for BIOS (UEFI already had a working mechanism). Restored on
    review: see /init's own select_console() for the full four-layer
    precedence (cmdline > UEFI NVRAM > FAT config > kernel default) this
    split is designed around.
    """
    if IS_UEFI:
        _write_efivar_console_pref(tty)
    else:
        _write_fat_console_pref(tty)


def zfs_list(dataset_root):
    """Boot environments: datasets directly under <pool>/ROOT."""
    try:
        out = subprocess.run(
            ["zfs", "list", "-H", "-o", "name", "-r", dataset_root],
            capture_output=True, text=True, timeout=10,
        )
        return [line for line in out.stdout.splitlines() if line]
    except Exception:
        return []


def list_pools():
    """Every currently-imported pool - not just the one /init named on
    the cmdline. /init best-effort-imports any other reachable pool
    before starting this menu (see its own comment there), so asking
    ZFS directly here reflects whatever is actually available right
    now (e.g. a second disk plugged in for a rescue) rather than
    threading a separate list through an env var that could go stale.
    """
    try:
        out = subprocess.run(
            ["zpool", "list", "-H", "-o", "name"],
            capture_output=True, text=True, timeout=10,
        )
        return [line for line in out.stdout.splitlines() if line]
    except Exception:
        return []


def list_all_boot_environments():
    """Boot environments across every imported pool, pool-qualified
    (zfs_list already returns full dataset paths like
    "zroot/ROOT/alpine").

    Only DIRECT children of <pool>/ROOT - not <pool>/ROOT itself, and
    not anything nested deeper than one level. zfs_list()'s own
    `zfs list -r` recursion includes the root of the recursion too -
    confirmed the hard way on real hardware: "zroot/ROOT" showed up
    right alongside "zroot/ROOT/alpine" as a selectable boot
    environment, and picking it failed with "no matching kernel/
    initramfs" since it's just the container dataset, never meant to
    be bootable on its own.
    """
    bes = []
    for pool in list_pools():
        root = f"{pool}/ROOT"
        for name in zfs_list(root):
            rest = name[len(root) + 1:] if name.startswith(root + "/") else None
            if rest and "/" not in rest:
                bes.append(name)
    return bes


def _zfs_get(prop, dataset):
    # try/except added for consistency with zfs_list()/list_pools()'s
    # own identical reasoning - unlike those, this one had no guard at
    # all, so a subprocess.run() raising here (zfs binary missing from
    # PATH, resource exhaustion) would propagate uncaught through
    # ensure_key_loaded() -> list_kernels()/chroot_be()/deploy(),
    # crashing the whole menu session.
    try:
        return subprocess.run(
            ["zfs", "get", "-H", "-o", "value", prop, dataset],
            capture_output=True, text=True,
        ).stdout.strip()
    except Exception:
        return ""


def _zfs_key_stage_path(encryptionroot):
    """MUST compute the exact same path as zfs-unlock.sh's own
    zfs_key_stage_path() (same flattening rule) - this process and
    boot-dataset.sh/zfs-unlock check the same tmpfs file from two
    separate languages, so the two implementations have to agree, not
    just happen to.
    """
    return f"{ZFS_KEY_STAGE_ROOT}/tmp/zfs-key.{encryptionroot.replace('/', '_')}"


def _zfs_handoff_ready(encryptionroot):
    try:
        return os.path.getsize(_zfs_key_stage_path(encryptionroot)) > 0
    except OSError:
        return False


def encryption_status(dataset):
    """The three-state model this whole project uses wherever
    encryption state is shown or acted on (see zfs-unlock.sh's own
    header comment for the full invariant this mirrors):
      - None: DATASET is not encrypted at all.
      - keystatus="unavailable": LOCKED.
      - keystatus="available", handoff_ready=True: UNLOCKED, kexec
        handoff ready - boot will not re-prompt.
      - keystatus="available", handoff_ready=False: UNLOCKED, but no
        passphrase is staged (e.g. a bare `zfs load-key` run by hand
        over the rescue shell) - boot WILL re-prompt unless this is
        fixed via another unlock_encrypted_root() first.
    """
    root = _zfs_get("encryptionroot", dataset)
    if not root or root == "-":
        return None
    keystatus = _zfs_get("keystatus", root)
    return {
        "encryptionroot": root,
        "keystatus": keystatus,
        "handoff_ready": keystatus == "available" and _zfs_handoff_ready(root),
    }


def ensure_key_loaded(dataset):
    """Ensure DATASET is readable - same logic as boot-dataset.sh's own
    (that's the primary path; this covers the other direct `mount -t
    zfs` callers in this file, chroot_be() and list_kernels(), which
    would otherwise just fail with an opaque mount error on a locked
    dataset). Returns True if DATASET is readable afterward
    (unencrypted, already unlocked, or successfully unlocked here),
    False otherwise.

    Shells out to zfs-unlock's own zfs_unlock() (see ZFS_UNLOCK_SH's own
    comment) rather than calling `zfs load-key` directly - a real,
    reported gap in an earlier version of this function: a bare `zfs
    load-key` here unlocked the dataset but never staged a kexec handoff
    secret, so a boot reached via THIS path (chroot, then Boot default,
    without ever going through boot-dataset.sh's own unlock prompt)
    silently lost the single-passphrase-boot property. zfs_unlock()
    handles that consistently, including reacquiring a verified
    passphrase for handoff when the dataset arrives here already
    unlocked by something else.
    """
    status = encryption_status(dataset)
    if status is None:
        return True  # not encrypted
    if status["keystatus"] == "available" and status["handoff_ready"]:
        return True
    return subprocess.run([ZFS_UNLOCK_SH, "unlock", status["encryptionroot"]]).returncode == 0


def list_kernels(dataset):
    """Kernel versions on DATASET, by mounting it read-only briefly."""
    if not ensure_key_loaded(dataset):
        return []
    subprocess.run(["mkdir", "-p", MNT_ROOT])
    subprocess.run(["umount", MNT_ROOT], capture_output=True)
    mounted = subprocess.run(
        ["mount", "-t", "zfs", "-o", "ro", dataset, MNT_ROOT],
        capture_output=True,
    )
    if mounted.returncode != 0:
        return []
    try:
        names = sorted(
            f.removeprefix("vmlinuz-")
            for f in os.listdir(f"{MNT_ROOT}/boot")
            if f.startswith("vmlinuz-")
        )
        return names
    except OSError:
        return []
    finally:
        subprocess.run(["umount", MNT_ROOT], capture_output=True)


def boot(dataset, kernel_suffix=None, cmdline_override=None):
    """Run boot-dataset.sh. Effectively never returns on success - a
    successful kexec -e replaces the running kernel entirely, taking
    this whole process down with it, so there is nothing left to
    return to either way.

    Deliberately subprocess.run(), NOT os.execv() (an earlier version
    of this used execv) - on FAILURE, boot-dataset.sh's own fail()
    drops to a real bash shell to let the operator look around, and
    execv had already replaced this process by then: a real user's
    `exit` from that shell had nothing to return to, unwound all the
    way back to /init, and /init read that as "menu.py crashed",
    falling through to automatic boot instead of back to this menu -
    confirmed on real hardware, same class of bug recovery_shell() had
    and was fixed the same way. With a plain child process instead,
    that same `exit` just returns control here, and the caller's own
    return to main()'s loop redraws the menu exactly as intended.

    The pool is derived from DATASET itself (its first "/"-separated
    segment), not the module-level POOL constant - with multiple pools
    importable (see list_pools()), a chosen boot environment might
    belong to a DIFFERENT pool than the one /init originally named on
    the cmdline, and boot-dataset.sh's own `zpool export "$POOL"` after
    a successful kexec -l must export the pool the booted dataset
    actually lives on, not always the same one.
    """
    pool = dataset.split("/", 1)[0]
    argv = [BOOT_DATASET_SH, dataset, pool, kernel_suffix or ""]
    if cmdline_override is not None:
        argv.append(cmdline_override)
    subprocess.run(argv)


# --- dialog helpers ----------------------------------------------------

def _dialog(args):
    """Run dialog with common chrome. Returns (ok, answer) - ok is True
    for the OK/Yes button (exit code 0), False for Cancel/No (1) or
    ESC (255) - callers that only care about "did the user commit to
    this" don't need dialog's own exit-code distinctions.

    dialog draws its own screen straight to /dev/tty (real ncurses
    behavior, independent of stdin/stdout/stderr redirection) and
    writes its RESULT to stderr, not stdout, by long-standing shell-
    scripting convention - stdout is left alone here rather than
    redirected, and the answer is read back off stderr.

    No `--timeout` support here - the only place that ever needs it is
    main()'s own countdown, which has its own dedicated, more careful
    implementation (see _countdown_menu()) rather than reusing this
    general-purpose helper, since it needs to tell a genuine timeout
    apart from an explicit Cancel/ESC (via DIALOG_TIMEOUT), which
    every other caller of this function has no reason to care about.
    """
    cmd = DIALOG_COMMON + [str(a) for a in args]
    # Clear the real terminal first - see main()'s own identical
    # comment for the full reasoning (a serial console's real window
    # size never reaches this process, so dialog can only draw within
    # a modest assumed area, leaving anything outside it - a previous
    # screen's own leftover content, a recovery shell's own output -
    # frozen and visible forever otherwise). Applied here too, not
    # just once in main(), because EVERY call through this shared
    # chokepoint starts a brand-new dialog process with no memory of
    # what the terminal actually looked like beforehand - a submenu
    # returning from a raw interactive shell (recovery_shell(),
    # chroot_be()) is exactly as capable of leaving stray content
    # behind as anything main() itself guards against.
    try:
        sys.stdout.write("\x1b[2J\x1b[H")
        sys.stdout.flush()
    except OSError:
        pass
    # Guarded - this is the single shared chokepoint under every screen
    # in this file EXCEPT the countdown's own _run_dialog_with_activity()
    # (which already has an equivalent fallback, added after real,
    # confirmed hardware failures in exactly this class of call). This
    # one had none: any subprocess.run() failure here (fork failure
    # under process-table pressure, dialog binary transiently missing)
    # would propagate uncaught through whichever screen called it, up
    # through _dispatch(), and be caught only by main()'s own top-level
    # handler - discarding the ENTIRE rescue session over one failed
    # screen. Treated as a plain Cancel/No (the same "didn't commit to
    # anything" case every caller already handles) rather than crashing
    # - printed, not silent, so it's visible on the console instead of
    # looking like the operator just backed out on their own.
    try:
        proc = subprocess.run(cmd, stderr=subprocess.PIPE, text=True)
    except Exception as e:
        print(f"alpine-zfsboot: dialog failed to run ({e!r})", file=sys.stderr)
        return False, ""
    stderr_text = (proc.stderr or "").strip()
    # dialog's own exit-code convention (`man dialog`): 0 OK, 1 Cancel,
    # 2 Help, 3 Extra, 4 Item-help, 5 Timeout, 255 ESC OR a genuine
    # internal failure (e.g. "Error opening terminal: unknown." on an
    # unresolvable $TERM - see _sanitize_term()'s own comment for the
    # real, confirmed bug this was) - dialog uses that SAME code for
    # both, so the exit code alone can never tell them apart. What
    # does: an ESC always leaves stderr EMPTY (there was never any
    # "answer" to report, confirmed against the real bundled binary);
    # a genuine crash always writes a real message. Any OTHER
    # unrecognized exit code is treated the same way, on the same
    # reasoning - dialog does not document ANY other code as a normal
    # outcome, so an unexpected one plus actual stderr text is exactly
    # as suspicious as 255 plus text. Raising here (rather than
    # silently falling through as "the user cancelled", which is what
    # every caller of this function used to do) is what stops a broken
    # $TERM from wasting an unbounded number of retries that can never
    # succeed - see DialogUnavailable's own comment.
    if proc.returncode not in (0, 1, 2, 3, 4, 5) and stderr_text:
        print(f"alpine-zfsboot: dialog failed to initialize (exit={proc.returncode}): {stderr_text}",
              file=sys.stderr)
        raise DialogUnavailable(stderr_text)
    return proc.returncode == 0, stderr_text


def dialog_menu(title, items, text="", ok_label="Select", cancel_label="Back", default_item=None):
    """items: list of label strings. Returns the chosen index, or None
    on Cancel/ESC. default_item pre-highlights that entry (a tag string
    matching one of the indices below) without auto-confirming it -
    the user still has to actually press Enter on it.
    """
    if not items:
        return None
    menu_args = []
    for i, item in enumerate(items):
        menu_args += [str(i), item]
    menu_height = min(len(items), 15)
    args = ["--title", title, "--ok-label", ok_label, "--cancel-label", cancel_label]
    if default_item is not None:
        args += ["--default-item", str(default_item)]
    args += ["--menu", text, DIALOG_HEIGHT, DIALOG_WIDTH, menu_height] + menu_args
    ok, answer = _dialog(args)
    if not ok:
        return None  # Cancel/ESC - a real, unambiguous "didn't choose anything"
    try:
        return int(answer)
    except ValueError:
        # ok=True means dialog itself reported a confirmed OK/Enter -
        # an empty or unparseable answer here is NOT the same thing as
        # an explicit Cancel, but _dispatch()'s own `choice is None`
        # check treats them identically (falls through to "boot
        # default"). Confirmed a real, cross-cutting gap: nothing
        # distinguished "operator explicitly chose to boot default"
        # from "dialog confirmed something but we couldn't parse what"
        # - printed here so the second case is at least visible on the
        # console instead of silently looking like the first.
        print(f"alpine-zfsboot: dialog confirmed but returned an unparseable answer ({answer!r}) - treating as no selection", file=sys.stderr)
        return None


def dialog_msgbox(title, text):
    _dialog(["--title", title, "--msgbox", text, DIALOG_HEIGHT, DIALOG_WIDTH])


def dialog_yesno(title, text, default_no=True):
    args = ["--title", title]
    if default_no:
        args.append("--defaultno")
    args += ["--yesno", text, DIALOG_HEIGHT, DIALOG_WIDTH]
    ok, _ = _dialog(args)
    return ok


def dialog_inputbox(title, text, default=""):
    """Returns the entered text (possibly empty), or None on Cancel/ESC
    - distinct from an empty-but-confirmed answer, which callers that
    care about "did they actually cancel" need to tell apart from "they
    left it blank and hit OK".
    """
    ok, answer = _dialog(["--title", title, "--inputbox", text, DIALOG_HEIGHT, DIALOG_WIDTH, default])
    return answer if ok else None


def dialog_textbox(title, text):
    """Scrollable read-only viewer for output too long for a msgbox -
    dialog's --textbox needs a real file, not stdin text, hence the
    tempfile.
    """
    with tempfile.NamedTemporaryFile("w", delete=False, suffix=".txt") as f:
        f.write(text)
        path = f.name
    try:
        # Guarded for the same reason as _dialog() itself (see its own
        # comment) - this is the one dialog-spawning call in the file
        # that doesn't go through _dialog() at all (--textbox has no
        # meaningful stderr "answer" to capture), so it needs its own
        # identical try/except rather than inheriting one.
        try:
            subprocess.run(DIALOG_COMMON + ["--title", title, "--textbox", path,
                                            str(DIALOG_HEIGHT), str(DIALOG_WIDTH)])
        except Exception as e:
            print(f"alpine-zfsboot: dialog --textbox failed to run ({e!r})", file=sys.stderr)
    finally:
        os.remove(path)


# --- screens -------------------------------------------------------------

def select_boot_environment():
    bes = list_all_boot_environments()
    if not bes:
        dialog_msgbox("Select boot environment", "No boot environments found on any imported pool.")
        return
    choice = dialog_menu("Select boot environment", bes)
    if choice is not None:
        boot(bes[choice])


def select_kernel():
    kernels = list_kernels(BOOTFS)
    if not kernels:
        dialog_msgbox("Select kernel", f"No kernels found on {BOOTFS}.")
        return
    choice = dialog_menu(f"Select kernel on {BOOTFS}", kernels)
    if choice is not None:
        boot(BOOTFS, kernel_suffix=kernels[choice])


def switch_console():
    """Shows every real console candidate this boot has, current one
    marked and pre-highlighted, and lets the user pick explicitly -
    not just a blind cycle-to-next-one (an earlier version of this did
    that, with no indication anywhere of what the current default even
    was). Persists the choice via write_console_pref() (UEFI NVRAM or
    the canonical FAT/ESP partition's own config file, depending on
    firmware - see that function's own docstring), then asks /init to
    relaunch this whole menu attached to it - a real, live switch
    within THIS boot (FreeBSD-loader-style: flip between video/serial
    on demand), not just "takes effect next reboot". Exit code 42 is a
    deliberate, specific signal /init's own main loop treats as
    "restart me on the new console", not a crash (see /init's own
    comment on that same exit code) - sys.exit(), not os._exit(), so
    any real cleanup Python itself wants to do on the way out still
    happens.
    """
    # ttyS1/ttyS2 alongside ttyS0/ttyAMA0 - confirmed a real need on
    # physical OVH hardware, whose remote-console redirection doesn't
    # always land on ttyS0 (see /init's own select_console() comment).
    candidates = [c for c in ("tty0", "ttyS0", "ttyS1", "ttyS2", "ttyAMA0") if os.path.exists(f"/dev/{c}")]
    if len(candidates) < 2:
        dialog_msgbox("Switch console", "Only one console is available on this boot - nothing to switch to.")
        return
    current = ACTIVE_TTY if ACTIVE_TTY in candidates else candidates[0]
    labels = [f"/dev/{c}" + (" (current)" if c == current else "") for c in candidates]
    idx = dialog_menu(
        "Switch console", labels,
        text=f"Console: /dev/{current} (default)\n\nSelect a console to switch to:",
        default_item=str(candidates.index(current)),
    )
    if idx is None or candidates[idx] == current:
        return
    next_tty = candidates[idx]
    write_console_pref(next_tty)
    dialog_msgbox("Switch console", f"Switching to /dev/{next_tty}...")
    sys.exit(42)


def _network_status():
    """Real, current RESCUE_IFACE address, or None - re-checked every time this
    is called (menu item labels, network_menu() itself), never cached,
    since a background udhcpc lease from /init's own automatic bring-up
    (see that file's own comment - only happens if rescue SSH material was
    staged) can come up independently of anything menu.py does, and the
    whole point of showing this at all is that it stays honest about
    whatever the real state is right now.
    """
    try:
        out = subprocess.run(["ifconfig", RESCUE_IFACE], capture_output=True, text=True).stdout
    except Exception:
        return None
    m = re.search(r"inet (?:addr:)?(\d+\.\d+\.\d+\.\d+)", out)
    return m.group(1) if m else None


def _network_addresses():
    """All current global-scope addresses on RESCUE_IFACE, IPv4 AND IPv6 alike -
    used only by the Diagnostics cockpit (_rescue_status_text()).
    _network_status() above stays IPv4-only and is left untouched - it
    backs the DHCP bring-up flow, which is genuinely IPv4-only by
    design, a separate concern from this. The cockpit needs the honest
    full picture: Hetzner CAX rescue access is itself commonly
    IPv6-only (see net-config.sh's own SLAAC/static IPv6 support), so a
    cockpit that only ever checks for an IPv4 address would silently
    under-report a machine that is, right now, perfectly reachable over
    rescue SSH.
    """
    try:
        out = subprocess.run(["ifconfig", RESCUE_IFACE], capture_output=True, text=True).stdout
    except Exception:
        return []
    v4 = re.findall(r"inet (?:addr:)?(\d+\.\d+\.\d+\.\d+)", out)
    v6 = re.findall(r"inet6 addr:?\s*([0-9a-fA-F:]+)/\d+\s+Scope:Global", out)
    return v4 + v6


def _probe_network_drivers():
    """Loads whatever kernel module handles any network-class PCI
    device that doesn't already have a driver bound - real hardware
    NICs (e1000e, r8169, igb, ...) never get an explicit modprobe from
    /init at all, only virtio_net does (see /init's own comment - this
    project deliberately doesn't hand-pick a driver for every real NIC
    vendor, the same "surgical inclusion" reasoning build.sh's own
    FEATURES comment gives for leaving kms/GPU drivers out). Reads
    each device's own kernel-reported modalias - the exact same string
    real udev/hotplug would resolve a driver from - rather than
    guessing driver names: `modprobe` itself resolves a modalias to
    the right module via the system's own modules.alias database, so
    this works for whatever NIC is actually present without this
    project needing to know its name in advance. A one-time, on-demand
    stand-in for the persistent uevent listener this minimal
    initramfs deliberately doesn't run (see /init's own `mdev -s`
    comment on that same tradeoff, for storage devices).
    """
    pci_root = PCI_DEVICES_ROOT
    try:
        devices = os.listdir(pci_root)
    except OSError:
        return
    for dev in devices:
        try:
            with open(f"{pci_root}/{dev}/class") as f:
                pci_class = f.read().strip()
        except OSError:
            continue
        # PCI base class 0x02 = network controller - the kernel's own
        # class-code convention, not this project's invention.
        if not pci_class.startswith("0x02"):
            continue
        if os.path.exists(f"{pci_root}/{dev}/driver"):
            continue  # already bound to a driver - nothing to probe
        try:
            with open(f"{pci_root}/{dev}/modalias") as f:
                modalias = f.read().strip()
        except OSError:
            continue
        if modalias:
            subprocess.run(["modprobe", modalias], capture_output=True)


def network_menu():
    """Brings up RESCUE_IFACE via DHCP on demand, from the menu - deliberately
    separate from /init's own automatic bring-up. /init only brings the
    network up at all if authorized_keys/host key material was staged
    for this exact boot, and that's deliberate there too: remote rescue
    access is the whole point of that path, and it can't depend on
    anyone being at the local console to request it (see /init's own
    comment on that). This is the OTHER, local-operator case: reaching
    a golden-image host for deploy(), or just checking connectivity,
    on a boot that never had rescue SSH configured at all - explicitly
    requested here, not automatic, and not tied to rescue SSH in any way.

    ifconfig/udhcpc run in the foreground and wait (unlike /init's own
    backgrounded `&` version, which fires-and-forgets since nothing
    there is watching for the result) - a real person is sitting at
    this menu waiting for an answer, so this reports success or
    failure directly rather than leaving it to be discovered later.
    """
    ip = _network_status()
    if ip:
        dialog_msgbox("Network", f"{RESCUE_IFACE} is up: {ip}")
        return
    if not dialog_yesno("Network", f"{RESCUE_IFACE} has no address yet.\n\nBring it up via DHCP now?", default_no=False):
        return
    _probe_network_drivers()
    subprocess.run(["ifconfig", RESCUE_IFACE, "up"])
    try:
        subprocess.run(["udhcpc", "-i", RESCUE_IFACE, "-n", "-q"], timeout=15)
    except subprocess.TimeoutExpired:
        pass
    ip = _network_status()
    if ip:
        dialog_msgbox("Network", f"{RESCUE_IFACE} is up: {ip}")
    else:
        dialog_msgbox("Network", f"DHCP did not complete - {RESCUE_IFACE} still has no address.")


class DialogUnavailable(Exception):
    """Raised by _dialog() when dialog itself failed to even initialize
    (not a user Cancel/ESC - see _dialog()'s own comment on how those
    two are told apart). Deliberately just an Exception, not something
    special-cased further - main()'s own top-level handler already
    treats ANY uncaught exception here as "the TUI is unusable, fall
    back to automatic boot" (see its own comment for why that's the
    right behavior for this too, not just a generic bug), and letting
    it propagate there rather than swallowing/retrying it here is
    exactly what stops this from becoming the unbounded/silent retry
    loop a real CAX session hit (a dialog init failure can't ever
    un-fail by trying again with the same broken $TERM).
    """


def _sanitize_term():
    """Called ONCE, at the very top of main(), before ANY dialog/curses
    call in this file (including the very first one, in the countdown
    loop) or anything this process might later exec (boot-dataset.sh
    inherits this same, possibly-corrected environment - see boot()).

    The actual, confirmed root cause of a reported "no menu at all over
    SSH" bug that looked at first like a broken PTY: dropbear does NOT
    clear $TERM (see rescue-ssh.sh's own comment on its small built-in
    exception list to clearenv()) - it sets it from the CLIENT's own
    pty-request, almost always something like "xterm-256color". Without
    a matching bundled terminfo entry (see build.sh's own terminfo
    comment), dialog/ncurses's own setupterm() fails INSTANTLY on every
    single call - previously silently swallowed (see _dialog()'s own
    fix for that half of this bug) and easy to misread as the setsid()/
    TIOCSCTTY warnings being the real problem, when those are harmless
    no-ops (confirmed by direct reproduction against the real bundled
    busybox/dropbear binaries).

    build.sh now bundles terminfo entries for the common real-world SSH
    client cases (xterm/xterm-256color/screen/screen-256color/
    tmux-256color/vt100, alongside the pre-existing linux/ansi) - this
    function's job is just to verify whatever $TERM actually arrived
    resolves to one of them, and fall back to a KNOWN-bundled entry
    only when it genuinely doesn't, rather than assuming the bundled
    list covers everything a real client might ever send. Deliberately
    does NOT special-case dropbear's own `-e` flag as the fix (see
    rescue-ssh.sh's own comment on why dropbear is invoked without it)
    - the client's own real TERM (with its own real color/keypad
    capabilities) is used whenever a bundled entry actually matches it;
    this is only the backstop for when one doesn't.

    IS_SSH_SESSION (a real transport signal, not ACTIVE_TTY's own
    already-wrong-over-SSH default) picks between "ansi" (a safe,
    reasonably-capable minimum any real terminal emulator can render,
    for a remote client this process knows nothing else about) and
    "linux" (already always correct for the local console today,
    unaffected by this function - /init sets $TERM to exactly "linux"
    or "ansi" itself before ever starting this process locally, so this
    fallback branch is not expected to ever actually fire there; it
    exists as a defensive backstop, not the normal local path).
    """
    term = os.environ.get("TERM", "")
    # Same STUB_ROOT-style pattern as MNT_ROOT/PCI_DEVICES_ROOT above -
    # lets tests point this at a scratch directory with a couple of
    # fake entries instead of depending on whatever ncurses-terminfo-base
    # data (if any) happens to actually be installed on the machine
    # running the test suite.
    terminfo_root = os.environ.get("STUB_TERMINFO_ROOT", "/etc/terminfo")
    resolvable = bool(term) and os.path.exists(f"{terminfo_root}/{term[:1].lower()}/{term}")
    if resolvable:
        return
    fallback = "ansi" if IS_SSH_SESSION else "linux"
    print(f"alpine-zfsboot: TERM={term!r} has no bundled terminfo entry - "
          f"falling back to TERM={fallback!r} so dialog/curses can actually start",
          file=sys.stderr)
    os.environ["TERM"] = fallback


def _establish_controlling_terminal():
    """Called ONCE, early in main() - not per shell-out. See the real
    bug this closes: bash printed "cannot set terminal process group
    (-1): Not a tty" / "no job control in this shell", and Ctrl+C
    stopped interrupting a foreground command (`ping` with no -c)
    entirely. The actual cause: this whole process tree (/init's own
    shell -> python3, a plain forked child, never `exec`'d - see
    /init's own comment on why not) never had a controlling terminal
    established for it AT ALL - plain fd0/1/2 pointing at a real tty
    device is not the same thing as a session that has claimed it as
    its ctty, and job control specifically needs the latter (the
    kernel signals a tty's own foreground process group on Ctrl+C,
    which is only ever set once something establishes that tty as a
    real controlling terminal - nothing earlier in this boot chain
    ever did, unlike a normal login/getty).

    A FIRST version of this fix did setsid()+TIOCSCTTY inside a
    disposable forked child, spun up fresh per interactive shell
    (recovery_shell(), chroot_be()) - confirmed a real, DIFFERENT bug
    from that choice: that child, not menu.py, became the session
    LEADER of the tty, so exiting that shell tore its own session down
    (POSIX: a session's controlling-terminal association ends when its
    leader exits), and a real boot then fell straight through to
    automatic boot instead of back to this menu, right after typing
    `exit`. Doing this ONCE, here, in python3's own long-lived process
    instead, means THIS process is the session leader for as long as
    it runs - a plain subprocess.run() shell-out afterward (see
    _run_interactive()) correctly inherits real job control for free,
    the same way any ordinary login shell's own children do, and
    nothing tears the session down just because one shell-out finished.

    A SECOND version of this fix called setsid()+TIOCSCTTY
    unconditionally, every boot, local console or SSH alike - confirmed
    a real, DIFFERENT bug from THAT choice too: over SSH, dropbear has
    ALREADY done this exact dance for its own pty before ever exec'ing
    into alpine-zfsboot-shell -> this process (see alpine-zfsboot-shell's
    own `exec python3 /menu.py`), so both calls here were guaranteed to
    fail (EPERM: already a session/process-group leader; EPERM: already
    has a controlling terminal) - confirmed by direct reproduction
    (same busybox/python versions, real ptys) to be harmless, pure
    no-ops with no effect on process/session/tty state either way, but
    still printed a confusing warning on every single SSH login. Testing
    actual CAPABILITY first - does /dev/tty already open successfully,
    i.e. does this process already have SOME controlling terminal -
    rather than branching on transport (SSH vs local) or a hardcoded
    device name (ACTIVE_TTY's own "tty0" default, wrong over SSH) is
    the real, transport-agnostic question this function exists to
    answer, and the one every other terminal-establishing path in this
    project (see boot-dataset.sh's/init's own _recovery_shell()) now
    asks the exact same way.

    setsid() can fail (EPERM) if python3 is somehow already its own
    process group leader - not expected here (a plain forked, never
    exec'd child inherits /init's shell's own group), but printed
    rather than silently swallowed if it does happen anyway, so a
    next boot's log says definitively whether THIS is what broke
    rather than reproducing the exact same "no job control" silence
    once more while looking like a different fix worked.
    """
    try:
        with open("/dev/tty"):
            pass
        return  # already have a controlling terminal (e.g. dropbear's own pty over SSH) - nothing to do
    except OSError:
        pass
    try:
        os.setsid()
    except OSError as e:
        print(f"alpine-zfsboot: setsid() failed ({e}) - interactive shells may have no job control")
    try:
        fd = os.open(f"/dev/{ACTIVE_TTY}", os.O_RDWR)
    except OSError:
        try:
            fd = os.open("/dev/tty", os.O_RDWR)
        except OSError as e:
            print(f"alpine-zfsboot: could not open a tty to establish as controlling terminal ({e})")
            return
    try:
        fcntl.ioctl(fd, termios.TIOCSCTTY, 0)
    except OSError as e:
        print(f"alpine-zfsboot: TIOCSCTTY failed ({e}) - interactive shells may have no job control")
    os.close(fd)


def _run_interactive(argv):
    """Runs argv as a real foreground shell - recovery_shell() and
    chroot_be() both need proper job control (see
    _establish_controlling_terminal(), called once at startup, for why
    plain subprocess.run() is correct here and NOT its own
    setsid()/TIOCSCTTY dance per call - this used to do exactly that,
    and it was itself the bug: see that function's own comment).

    Resets the underlying tty's own termios to a plain, sane mode
    (ISIG/ICANON/ECHO on) first - belt-and-suspenders in case
    _run_dialog_with_activity()'s own raw-mode restore (see its
    comment) didn't fully take on some real hardware; cheap and
    idempotent either way.

    /dev/tty (this process's own controlling terminal, established by
    _establish_controlling_terminal() before main() ever reaches this
    point, one way or another) is tried FIRST, not /dev/{ACTIVE_TTY} -
    an earlier version of this function had that backwards, which
    happened to work locally (ACTIVE_TTY is the real console device
    there) but was silently resetting the termios of the WRONG device
    entirely over SSH (the physical console's tty0, opened successfully
    despite having nothing to do with the actual SSH session, since
    root can open any tty device node regardless of which one, if any,
    is actually attached to this process) instead of the real dropbear
    pty this shell-out is actually running on.
    """
    try:
        fd = os.open("/dev/tty", os.O_RDWR)
    except OSError:
        try:
            fd = os.open(f"/dev/{ACTIVE_TTY}", os.O_RDWR)
        except OSError:
            fd = None
    if fd is not None:
        try:
            attrs = termios.tcgetattr(fd)
            attrs[3] |= termios.ISIG | termios.ICANON | termios.ECHO
            termios.tcsetattr(fd, termios.TCSANOW, attrs)
        except OSError:
            pass
        os.close(fd)
    subprocess.run(argv)


def unlock_encrypted_root():
    """"Unlock encrypted root" menu action - makes unlocking a
    first-class, explicit operation instead of a side effect of picking
    a kernel or chrooting in, matching the workflow this project's own
    rescue SSH is FOR: connect, unlock while inspecting/chrooting/
    running diagnostics, THEN boot - with no second passphrase prompt
    after kexec, because THIS is what actually stages the handoff
    secret (see ensure_key_loaded()'s own comment on why a bare `zfs
    load-key` was never enough for that).
    """
    status = encryption_status(BOOTFS)
    if status is None:
        dialog_msgbox("Unlock encrypted root", f"{BOOTFS} is not encrypted - nothing to unlock.")
        return
    if status["keystatus"] == "available" and status["handoff_ready"]:
        dialog_msgbox("Unlock encrypted root",
                       f"{status['encryptionroot']} is already UNLOCKED - kexec handoff READY.")
        return
    subprocess.run([ZFS_UNLOCK_SH, "unlock", status["encryptionroot"]])
    status = encryption_status(BOOTFS)
    if status["keystatus"] != "available":
        dialog_msgbox("Unlock encrypted root", f"{status['encryptionroot']} is still LOCKED.")
    elif status["handoff_ready"]:
        dialog_msgbox("Unlock encrypted root",
                       f"{status['encryptionroot']} is UNLOCKED - kexec handoff READY. Boot will not prompt again.")
    else:
        dialog_msgbox("Unlock encrypted root",
                       f"{status['encryptionroot']} is UNLOCKED, but kexec handoff is NOT READY - "
                       "boot will re-prompt for the passphrase after kexec. Try 'Unlock encrypted root' again to fix this.")


def lock_encrypted_root():
    """"Lock encrypted root" - the deliberate inverse of unlock, added
    for the exact same reason unlock is now first-class: OpenZFS never
    hands plaintext back out once a key is loaded, so "lock" has to mean
    "unload the key and remove any staged handoff secret", not
    "remember the passphrase and re-lock later". Refuses cleanly (never
    force-unmounts) if anything under the encryption root is still
    mounted - see zfs_lock()'s own comment for why that stays a
    separate, deliberate operator action rather than an implicit side
    effect of locking.
    """
    status = encryption_status(BOOTFS)
    if status is None:
        dialog_msgbox("Lock encrypted root", f"{BOOTFS} is not encrypted - nothing to lock.")
        return
    if status["keystatus"] != "available":
        dialog_msgbox("Lock encrypted root", f"{status['encryptionroot']} is already LOCKED.")
        return
    proc = subprocess.run([ZFS_UNLOCK_SH, "lock", status["encryptionroot"]],
                           capture_output=True, text=True)
    if proc.returncode == 0:
        dialog_msgbox("Lock encrypted root", f"{status['encryptionroot']} is now LOCKED.")
    else:
        dialog_textbox("Lock encrypted root - refused", (proc.stdout or "") + (proc.stderr or ""))


def recovery_shell():
    """Drop to a real shell with $POOL already imported, then return to
    the menu on exit - a real, previously-shipped bug here used
    os.execv() instead, which replaced this whole process with bash: a
    real user's `exit` then ended python3 itself, which /init read as
    "menu.py crashed" and fell straight through to automatic boot
    instead of back to the menu, exactly backwards from what "exit the
    recovery shell" should do. subprocess.run() (a plain child, waited
    on, not exec'd) is what chroot_be()/deploy() already used for the
    same kind of shell-out and never had this problem.
    """
    if POOL_IMPORT_ERROR:
        print(f"alpine-zfsboot recovery shell - NO pool imported ({POOL_IMPORT_ERROR}).")
    else:
        print(f"alpine-zfsboot recovery shell - pool {POOL} already imported.")
    print("'exit' returns to this menu")
    print("'apk add <package>' works too (see 'Network' in the menu if it can't reach the mirror)")
    os.environ["PS1"] = "\\[\\e[1;32m\\]alpine-zfsboot\\[\\e[0m\\] \\w # "
    _run_interactive(["/bin/bash"])


def parse_source(src):
    """"host:dataset@snap" -> (host, "dataset@snap") for a remote golden
    image, reached over the dbclient ssh session this same rescue
    environment already has (see /init and rescue-ssh.sh's own
    authorized_keys/dropbear setup) - not a new transport, the identical one used to reach this
    menu in the first place, just outbound instead of inbound.
    "dataset@snap" (no ':') -> (None, "dataset@snap") for a golden
    image on a pool already importable locally (e.g. a plugged-in USB
    disk). A bare dataset name never itself contains ':', so splitting
    on the first one is unambiguous.
    """
    if ":" in src:
        host, _, rest = src.partition(":")
        return host, rest
    return None, src


def deploy():
    """Guided fresh-install, no installer involved: create a target
    pool, `zfs recv` a golden image's entire dataset tree onto it, set
    bootfs, then fix up the handful of things OpenZFS/SSH need to be
    unique per machine (hostid, ssh host keys) and the hostname. The
    golden image itself is never "sysprepped" beforehand - it stays an
    ordinary, immutable snapshot; every clone of it goes through this
    exact same personalization step instead, here, once, after receive.

    Input gathering and confirmation go through dialog; the actual
    zpool create / zfs send|recv run in a plain terminal instead (same
    pattern as recovery_shell()/chroot_be()) so real, live command
    output/progress stays visible rather than hidden behind a widget -
    a `zfs send` of a real system image can take a while.
    """
    src = dialog_inputbox(
        "Deploy: source",
        "Source: [host:]dataset@snapshot\n\ne.g. backup01:zroot/images/alpine@golden",
    )
    if not src:
        return
    ssh_host, dataset_at_snap = parse_source(src)

    pool = dialog_inputbox("Deploy: target pool", "Target pool name", "zroot")
    if pool is None:
        return
    pool = pool or "zroot"

    vdev_spec = dialog_inputbox(
        "Deploy: target disk(s)",
        "Target disk(s) for `zpool create`\n\n"
        "e.g. /dev/sda, or 'mirror /dev/sda /dev/sdb'",
    )
    if not vdev_spec:
        return

    target_dataset = dialog_inputbox("Deploy: target dataset", "Target dataset path", f"{pool}/ROOT/alpine")
    if target_dataset is None:
        return
    target_dataset = target_dataset or f"{pool}/ROOT/alpine"

    hostname = dialog_inputbox("Deploy: hostname", "Hostname for this machine (blank to leave unchanged)")
    if hostname is None:
        return

    # This rescue environment already has at least one pool imported
    # (reaching the menu at all requires it) - if the operator accepts
    # the default target pool name and it collides with one already
    # imported, `zpool create -f` would happily overwrite the disk(s)
    # backing an EXISTING pool. Refuse outright rather than trusting -f
    # to only ever apply to genuinely-empty disks.
    if pool in list_pools():
        # Also the exact state a PARTIALLY failed deploy leaves behind
        # (zpool create succeeds, then zfs send/recv fails partway
        # through - see the failure dialog below) - retrying deploy()
        # against the same pool name lands here every time until the
        # operator does something about the leftover pool by hand, so
        # this message says what that something actually is (destroy
        # it, not export it - export would just re-hide a broken,
        # half-received pool rather than clear it for a clean retry).
        dialog_msgbox(
            "Deploy: refused",
            f"pool '{pool}' is already imported - refusing to `zpool create` over it.\n\n"
            "Choose a different target pool name, or - if this is a leftover from "
            f"an earlier failed deploy attempt, not a real pool you want to keep - "
            f"`zpool destroy {pool}` in the recovery shell first, then retry.",
        )
        return

    summary = "about to run:\n\n"
    summary += f"  zpool create -f {pool} {vdev_spec}\n"
    if ssh_host:
        summary += f"  dbclient -y {ssh_host} zfs send -R -c {dataset_at_snap} | zfs recv -u {target_dataset}\n"
    else:
        summary += f"  zfs send -R -c {dataset_at_snap} | zfs recv -u {target_dataset}\n"
    summary += f"  zpool set bootfs={target_dataset} {pool}\n"
    summary += f"  personalize: hostname={hostname or '(unchanged)'}, fresh hostid, fresh ssh host keys\n\n"
    summary += "THIS DESTROYS ANY EXISTING DATA on the target disk(s)."
    if not dialog_yesno("Deploy: confirm", summary, default_no=True):
        return

    print("=== alpine-zfsboot deploy: zfs recv a golden image onto this machine ===")
    if subprocess.run(["zpool", "create", "-f", pool] + vdev_spec.split()).returncode != 0:
        dialog_msgbox("Deploy failed", "zpool create failed - see the terminal output above.")
        return

    # dbclient, not `ssh` - dropbear's own client, from the same
    # package already bundled for the inbound rescue session, rather
    # than pulling in a second, separate ssh-client dependency for
    # this one outbound leg. -y auto-accepts the remote host key
    # (no interactive prompt to get stuck on here); this only lowers
    # the bar to "same trust level as any other unauthenticated LAN
    # transfer", which a rescue/deploy tool already assumes.
    if ssh_host:
        send_argv = ["dbclient", "-y", ssh_host, "zfs", "send", "-R", "-c", dataset_at_snap]
    else:
        send_argv = ["zfs", "send", "-R", "-c", dataset_at_snap]
    recv_argv = ["zfs", "recv", "-u", target_dataset]
    send_proc = subprocess.Popen(send_argv, stdout=subprocess.PIPE)
    recv_proc = subprocess.run(recv_argv, stdin=send_proc.stdout)
    send_proc.stdout.close()
    send_proc.wait()
    if send_proc.returncode != 0 or recv_proc.returncode != 0:
        dialog_msgbox(
            "Deploy failed",
            f"zfs send/recv failed (send rc={send_proc.returncode}, recv rc={recv_proc.returncode})",
        )
        return

    bootfs_ok = subprocess.run(["zpool", "set", f"bootfs={target_dataset}", pool]).returncode == 0

    # Always reset bootcheck state fresh, unconditionally - a received
    # golden image's own attempt-count HISTORY must never carry over to
    # this machine (it belongs to a completely different boot history
    # on a different box), regardless of whatever the source's own
    # property said. Not defensive/redundant: `zfs send -R` implies
    # `-p` (properties), so the source's own org.alpinezfsboot:bootcheck
    # value genuinely does arrive on the received dataset (as
    # source=received) if this isn't done - confirmed against real
    # `zfs-send(8)` behavior, not assumed.
    subprocess.run(["zfs", "set", "org.alpinezfsboot:bootcheck=armed:0", target_dataset])

    # Same carryover gap, same fix, for org.alpinezfsboot:commandline: a
    # full source audit found this property was NOT reset here, even
    # though `zfs send -R` (see the comment just above) carries it over
    # from the source exactly like bootcheck used to - if an operator
    # ever set a custom boot cmdline override on the GOLDEN IMAGE itself
    # (menu.py's own "edit cmdline & persist" action, elsewhere in this
    # file), every future deploy() from it would silently inherit that
    # override onto a brand-new machine's very first real boot
    # (boot-dataset.sh reads this property directly), with nobody here
    # ever reviewing or even being told it exists. `zfs inherit` (not
    # `zfs set ...=""`) so the deployed target starts from the SAME
    # "no override, use the build-time default" state a fresh install
    # would - this project's own edit-cmdline menu action already uses
    # exactly this call for the same "clear it back to default" case.
    subprocess.run(["zfs", "inherit", "org.alpinezfsboot:commandline", target_dataset])

    # Personalize: mount rw briefly, fix up exactly the things that
    # must be unique per-machine - nothing else.
    mnt = "/mnt/deploy"
    subprocess.run(["mkdir", "-p", mnt])
    subprocess.run(["umount", mnt], capture_output=True)
    # A raw/encrypted golden image keeps its encryption across `zfs
    # recv` - the received copy needs unlocking the same as any other
    # encrypted dataset before it can be mounted.
    key_ok = ensure_key_loaded(target_dataset)
    mounted = key_ok and subprocess.run(["mount", "-t", "zfs", target_dataset, mnt]).returncode == 0
    if mounted:
        if hostname:
            with open(f"{mnt}/etc/hostname", "w") as f:
                f.write(hostname + "\n")
        # OpenZFS uses /etc/hostid to tell which host last imported a
        # pool - it must be unique per machine, hence regenerating it
        # here rather than carrying the golden image's own value onto
        # every clone of it. zgenhostid ships with the zfs package
        # itself, no separate dependency.
        try:
            os.remove(f"{mnt}/etc/hostid")
        except OSError:
            pass
        subprocess.run(["zgenhostid", "-o", f"{mnt}/etc/hostid"])
        # Wiped, not regenerated here - Alpine's own sshd OpenRC init
        # script runs `ssh-keygen -A` itself on first start whenever
        # host keys are missing, so first real boot handles this with
        # no custom first-boot script needed for it.
        ssh_dir = f"{mnt}/etc/ssh"
        try:
            for name in os.listdir(ssh_dir):
                if name.startswith("ssh_host_"):
                    os.remove(os.path.join(ssh_dir, name))
        except OSError:
            pass
        try:
            os.remove(f"{mnt}/etc/machine-id")
        except OSError:
            pass
        subprocess.run(["umount", mnt])
    else:
        dialog_msgbox(
            "Deploy: warning",
            f"could not mount {target_dataset} to personalize it - do this by hand before booting.",
        )

    # Checked before claiming success - confirmed a real gap: this used
    # to unconditionally say "bootfs is set on {pool}" even if the
    # `zpool set` above had actually failed, which would make this
    # newly-deployed dataset silently NOT be what boots next.
    if not bootfs_ok:
        dialog_msgbox(
            "Deploy: warning",
            f"{target_dataset} was received, but `zpool set bootfs={target_dataset} {pool}` failed - "
            "set it manually before rebooting, or this pool's default boot target hasn't changed.",
        )
    if dialog_yesno(
        "Deploy done",
        f"{target_dataset} is ready" + (f", bootfs is set on {pool}" if bootfs_ok else "") + ".\n\nBoot it now?",
        default_no=False,
    ):
        boot(target_dataset)


def chroot_be():
    """Mount a boot environment and chroot into it - for fixing
    something inside it directly (fstab, passwd, a broken package)
    without a manual mount+bind+chroot dance every time. Returns to the
    menu afterward, same as recovery_shell() - visiting a BE and coming
    back is the point.
    """
    bes = list_all_boot_environments()
    if not bes:
        dialog_msgbox("Chroot", "No boot environments found on any imported pool.")
        return
    idx = dialog_menu("Chroot into which boot environment?", bes)
    if idx is None:
        return
    dataset = bes[idx]

    if not ensure_key_loaded(dataset):
        dialog_msgbox("Chroot failed", f"could not load the encryption key for {dataset}.")
        return

    rw = dialog_yesno("Chroot", f"Mount {dataset} read-write? (needed to make changes)", default_no=False)
    mnt = "/mnt/chroot"
    subprocess.run(["mkdir", "-p", mnt])
    subprocess.run(["umount", mnt], capture_output=True)
    mount_argv = ["mount", "-t", "zfs"]
    if not rw:
        mount_argv += ["-o", "ro"]
    mount_argv += [dataset, mnt]
    if subprocess.run(mount_argv).returncode != 0:
        dialog_msgbox("Chroot failed", f"mount of {dataset} failed.")
        return

    # Bind the rescue environment's own live /proc /sys /dev in, rather
    # than expecting the BE to mount them itself - it's a dormant
    # filesystem, not a running system, so nothing would ever mount
    # them for it. /tmp does NOT get the same treatment - see below.
    for sub in ("proc", "sys", "dev"):
        target = f"{mnt}/{sub}"
        subprocess.run(["mkdir", "-p", target])
        subprocess.run(["mount", "--bind", f"/{sub}", target])

    # /tmp gets a FRESH, empty tmpfs instead of a bind-mount of this
    # rescue environment's own live /tmp - a real, confirmed secret-
    # exposure gap a full source audit found: this rescue session's own
    # /tmp is exactly where zfs-unlock.sh stages a plaintext encryption
    # passphrase (zfs-key.*), boot-dataset.sh's own boot/operation
    # locks live, and rescue-ssh.sh's own pidfile lives - bind-mounting
    # it straight into a BE's chroot meant anything running inside that
    # chroot (a script the operator runs, a compromised/malicious BE)
    # could read a currently-staged passphrase belonging to a
    # completely different boot operation, or interfere with this
    # rescue session's own lock state, neither of which chroot_be()'s
    # own stated purpose ("fixing something inside a dormant BE") ever
    # needed access to. A plain empty tmpfs still gives anything running
    # inside the chroot a real, writable /tmp (the actual functional
    # need this bind-mount served) without exposing this rescue
    # session's own state at all.
    target = f"{mnt}/tmp"
    subprocess.run(["mkdir", "-p", target])
    subprocess.run(["mount", "-t", "tmpfs", "tmpfs", target])

    # bash -> sh -> busybox: a target BE is a real installed Alpine
    # system, which may or may not have bash installed - fall back
    # rather than assuming this rescue environment's own choice of
    # shell exists inside every BE it might chroot into.
    shell_argv = None
    for candidate, argv in (
        ("/bin/bash", ["/bin/bash"]),
        ("/bin/sh", ["/bin/sh"]),
        ("/bin/busybox", ["/bin/busybox", "sh"]),
    ):
        if os.path.exists(f"{mnt}{candidate}"):
            shell_argv = argv
            break

    if shell_argv is None:
        dialog_msgbox("Chroot failed", f"no usable shell found inside {dataset} (tried bash, sh, busybox).")
    else:
        print(f"chrooting into {dataset} ({'rw' if rw else 'ro'}) - 'exit' returns to this menu")
        _run_interactive(["chroot", mnt] + shell_argv)

    # Always runs, regardless of whether a shell was found or how the
    # chroot exited - bind mounts must never be left hanging around.
    # -l (lazy): something inside the chroot may still hold a
    # reference (a backgrounded process, a leftover fd) even after
    # `chroot` itself returns - a plain umount failing there would
    # silently leave /mnt/chroot populated, and a LATER `zpool export`
    # (from booting something else afterward) would then also fail
    # silently under its own `2>/dev/null`, leaving a pool exported
    # only in appearance before kexec. Lazy unmount detaches the
    # mountpoint immediately regardless.
    for sub in ("tmp", "dev", "sys", "proc"):
        subprocess.run(["umount", "-l", f"{mnt}/{sub}"], capture_output=True)
    subprocess.run(["umount", "-l", mnt], capture_output=True)


def _rescue_ssh_running():
    """Mirrors rescue-ssh.sh's own _running()/_pid_alive() (see there)
    without a shell round-trip - good enough for a one-line status
    display, not a substitute for that file's own liveness handling.
    """
    pidfile = f"{ZFS_KEY_STAGE_ROOT}/tmp/rescue-sshd.pid"
    try:
        with open(pidfile) as f:
            pid = f.read().strip()
        return bool(pid) and os.path.exists(f"/proc/{pid}")
    except OSError:
        return False


def _rescue_status_text():
    """Concise "operator cockpit" summary - a real, requested
    improvement: an operator connecting to a dead machine over rescue
    SSH should understand its state in ten seconds, not by piecing it
    together from several separate diagnostic commands. Deliberately a
    handful of fields, not "everything" - the sections below already
    cover full detail for anyone who needs it.
    """
    lines = []
    if POOL_IMPORT_ERROR:
        lines.append(f"Pool:            {POOL} (NOT IMPORTED - see 'Boot log')")
    else:
        health = subprocess.run(["zpool", "list", "-H", "-o", "health", POOL],
                                 capture_output=True, text=True).stdout.strip()
        lines.append(f"Pool:            {POOL} ({health or 'unknown'})")
    lines.append(f"Bootfs:          {BOOTFS}")
    enc = None if POOL_IMPORT_ERROR else encryption_status(BOOTFS)
    if enc is None:
        lines.append("Encryption:      (not encrypted, or pool not imported)")
    else:
        lock_state = "UNLOCKED" if enc["keystatus"] == "available" else "LOCKED"
        if lock_state == "LOCKED":
            handoff = "n/a"
        else:
            handoff = "ready" if enc["handoff_ready"] else "NOT READY - boot will re-prompt"
        lines.append(f"Encryption root: {enc['encryptionroot']}")
        lines.append(f"Key status:      {lock_state}")
        lines.append(f"Kexec handoff:   {handoff}")
    bootcheck = _zfs_get("org.alpinezfsboot:bootcheck", BOOTFS) if not POOL_IMPORT_ERROR else ""
    lines.append(f"Bootcheck:       {bootcheck or '-'}")
    # Derived ONLY from bootcheck's own armed:K counter, never from
    # whether any evidence backend happens to have something to show -
    # see show_previous_boot_diagnostics()'s own comment for the real
    # bug this rule fixes ("pstore empty" is not the same fact as
    # "boot was clean").
    k = _bootcheck_k(BOOTFS)
    if k is None:
        prev_boot_line = "UNKNOWN (not tracked by bootcheck)"
    elif k > 0:
        prev_boot_line = "FAILED - see Previous boot diagnostics"
    else:
        prev_boot_line = "OK"
    lines.append(f"Prev boot:       {prev_boot_line}")
    lines.append(f"Rescue SSH:      {'running' if _rescue_ssh_running() else 'not running'}")
    addrs = _network_addresses()
    lines.append(f"Network ({RESCUE_IFACE}):  {', '.join(addrs) if addrs else 'no address configured'}")
    return "\n".join(lines)


def show_diagnostics():
    """Rescue status summary, zpool status, other importable pools,
    disk partitions, recent kernel log - one screen for understanding
    and troubleshooting machine state without hand-typing each command
    separately in the recovery shell. Every command here is either
    ZFS-native or a kernel pseudo-file/busybox applet already available
    - nothing new to bundle for this.

    A single dialog --textbox rather than plain print()s - scrollable,
    where plain print() output would just scroll off the top of the
    screen with no way back to it.
    """
    sections = [
        ("=== rescue status ===", None),
        ("=== zpool status ===", ["zpool", "status"]),
        ("=== other importable pools ===", ["zpool", "import"]),
        ("=== /proc/partitions ===", ["cat", "/proc/partitions"]),
        ("=== dmesg (last 40 lines) ===", ["sh", "-c", "dmesg | tail -40"]),
    ]

    def _run(argv):
        # Guarded - a diagnostics screen is exactly the wrong place to
        # let one failing command (of several, run back to back) take
        # down the whole menu session instead of just showing "(failed)"
        # for that one section. argv=None is the one section computed
        # directly in Python (_rescue_status_text()) rather than run as
        # a subprocess.
        if argv is None:
            try:
                return _rescue_status_text()
            except Exception as e:
                return f"(failed to compute: {e!r})"
        try:
            return subprocess.run(argv, capture_output=True, text=True).stdout
        except Exception as e:
            return f"(failed to run: {e!r})"

    text = "\n\n".join(f"{header}\n{_run(argv)}" for header, argv in sections)
    dialog_textbox("Diagnostics", text)


def _pstore_records():
    """Every dmesg-efi-* record currently in pstore - efi-pstore's own
    naming (NOT dmesg-ramoops-* - ramoops was rejected for this project,
    see boot-dataset.sh's attempt-record comment for why). Numeric-
    aware sort on the trailing record number, not plain lexicographic
    sorted() (an adversarial audit finding: "dmesg-efi-10" sorts before
    "dmesg-efi-2" under plain string sort, which matters once a dump
    splits across enough parts for a 2-digit index to appear - a real
    possibility given efi-pstore's default ~1KB-per-variable record
    size and a multi-KB kmsg dump). Empty if pstore has no backend
    registered (see _pstore_backend()) or a backend but nothing recorded
    (last boot's target kernel never panicked, or it did and a later
    confirmed-good boot already erased the evidence - see the
    alpine-zfsboot-bootcheck confirm service). This function alone
    can't and doesn't distinguish those two cases - both correctly mean
    "no evidence to offer" here - but callers that need to tell
    "not supported on this platform" apart from "nothing captured"
    should check _pstore_backend() directly rather than inferring it from
    an empty list.
    """
    try:
        names = os.listdir(PSTORE_ROOT)
    except OSError:
        return []
    records = [n for n in names if n.startswith("dmesg-efi-")]

    def _record_key(name):
        suffix = name[len("dmesg-efi-"):]
        digits = suffix.split(".", 1)[0]
        return int(digits) if digits.isdigit() else -1

    return sorted(records, key=_record_key)


def previous_boot_evidence():
    """Reads whatever pstore (efi-pstore backend) evidence exists from
    the last target boot's kernel. Returns None if there's nothing at
    all (see _pstore_records()'s own comment on why "no backend" and
    "backend but nothing captured" look the same here, correctly).

    Follows this subsystem's one hard rule, stated plainly so it's easy
    to hold every future change to it: NEVER CLAIM A CAUSE WE DIDN'T
    OBSERVE. "panic" below is only ever True when the literal substring
    "Kernel panic" appears in the captured text itself - not inferred
    from bootcheck's own unconfirmed-attempt count, not guessed from
    which BE was booting, nothing beyond what the kernel actually wrote
    to persistent storage before it died. When that substring is
    absent, the caller shows the raw captured text as the last
    available evidence and says nothing about what it means - a boot
    that hung with no panic, or a boot that succeeded and whose
    evidence merely wasn't cleared, look the same here on purpose;
    guessing between them would be exactly the kind of claim this rule
    exists to prevent.
    """
    records = _pstore_records()
    if not records:
        return None
    texts = []
    for name in records:
        try:
            with open(os.path.join(PSTORE_ROOT, name), errors="replace") as f:
                texts.append(f.read())
        except OSError:
            continue
    if not texts:
        return None
    combined = "\n\n".join(texts)
    return {"records": records, "text": combined, "panic": "Kernel panic" in combined}


def _bootcheck_k(dataset):
    """The bootcheck attempt counter for DATASET - a plain int if
    bootcheck is really tracking it (see boot-dataset.sh's/init's own
    identical parsing rules for why only a LOCAL, numeric armed:K value
    ever counts), or None if we genuinely don't know: the pool didn't
    import, bootcheck is off/absent/inherited-only for this dataset, or
    the value is unparseable. This is the ONLY thing Last Boot
    Diagnostics' headline (show_previous_boot_diagnostics() below) is
    allowed to be derived from - never evidence presence/absence.

    None must never be rendered as "OK" by a caller - that would be the
    same claim-from-silence bug an earlier audit pass already fixed
    (evidence-empty wrongly shown as "clean boot"), just inverted: a
    machine whose pool failed to import, or one not participating in
    bootcheck at all, has an UNKNOWN previous-boot state, not a good
    one. "OK" is earned only by a real, local, confirmed armed:0 - the
    same authority _rescue_status_text()'s pre-existing "Bootcheck:"
    line already trusted before this feature existed.
    """
    if POOL_IMPORT_ERROR:
        return None
    try:
        raw = subprocess.run(
            ["zfs", "get", "-H", "-o", "value,source", "org.alpinezfsboot:bootcheck", dataset],
            capture_output=True, text=True,
        ).stdout.strip()
    except Exception:
        return None
    value, _, source = raw.partition("\t")
    if value.startswith("armed:") and source == "local":
        k = value[len("armed:"):]
        if k.isdigit():
            return int(k)
    return None


def _last_attempt(dataset):
    """Reads the org.alpinezfsboot:attempt_* properties boot-dataset.sh
    writes right before every kexec (see its own comment) - the anchor
    every other per-attempt evidence source (rc.log below) is checked
    against for freshness. Returns None if never recorded: a BE that
    predates this feature, or one that has never reached a successful
    kexec -l at all.
    """
    if POOL_IMPORT_ERROR:
        return None
    time_ = _zfs_get("org.alpinezfsboot:attempt_time", dataset)
    if not time_ or time_ == "-":
        return None
    return {
        "time": time_,
        "kernel": _zfs_get("org.alpinezfsboot:attempt_kernel", dataset),
        "initrd": _zfs_get("org.alpinezfsboot:attempt_initrd", dataset),
        "cmdline": _zfs_get("org.alpinezfsboot:attempt_cmdline", dataset),
    }


def _rc_log_evidence(dataset, attempt):
    """Reads /var/log/rc.log from DATASET's own real ZFS filesystem -
    OpenRC's stock service logger (rc_logger="YES", enabled by the
    installer - see temp/alpine-install-zfs.sh), THE PRIMARY Last Boot
    Diagnostics evidence source for this project, not a "nice extra"
    alongside kernel-crash evidence - it is the only mechanism that
    covers the single most common real-world failure (a bad service, a
    bad fstab entry, broken networking) and needs no RAM/NVRAM tricks
    of any kind, since the evidence already lives on the BE's own disk.
    Mounts read-only exactly like list_kernels() does, for exactly the
    same reason.

    Freshness against ATTEMPT (see _last_attempt()): if rc.log's own
    mtime predates the attempt's own recorded time, the log was not
    written to during THIS attempt - that is the entire observed fact,
    and the only one returned. It does NOT by itself prove OpenRC never
    started (it could have started and simply failed to update the log
    for some other reason) - claiming "failure was before OpenRC
    started" would be exactly the kind of inferred cause this
    subsystem's "never claim a cause we didn't observe" rule forbids;
    an earlier version of this comment (and the UI string it justified)
    made exactly that over-claim, caught on review. Returned as
    stale=True rather than silently omitted, so the caller can say
    which of "no OpenRC evidence" vs "OpenRC evidence exists but
    predates this attempt" is true - a real, different fact either way,
    without guessing at why.

    Deliberately does NOT call ensure_key_loaded() on a locked
    encrypted BE - this is a passive diagnostics read, not a boot
    attempt, and unconditionally prompting for a passphrase just to
    open a status screen (with no way to see the OTHER two evidence
    sections, which need no unlock at all, without first going through
    or cancelling that prompt) would be a real, reported-class UX
    regression, not a security concern. Checks the CURRENT key state
    via encryption_status() (no side effect) and returns locked=True
    instead - the caller shows "requires unlocking this BE first",
    never forces the prompt.
    """
    result = {"available": False, "stale": False, "locked": False, "text": None}
    status = encryption_status(dataset)
    if status is not None and status["keystatus"] != "available":
        result["locked"] = True
        return result
    subprocess.run(["mkdir", "-p", MNT_ROOT])
    subprocess.run(["umount", MNT_ROOT], capture_output=True)
    mounted = subprocess.run(
        ["mount", "-t", "zfs", "-o", "ro", dataset, MNT_ROOT],
        capture_output=True,
    )
    if mounted.returncode != 0:
        return result
    try:
        path = f"{MNT_ROOT}/var/log/rc.log"
        try:
            mtime = os.stat(path).st_mtime
        except OSError:
            return result
        stale = False
        if attempt and attempt.get("time"):
            try:
                attempt_epoch = (
                    datetime.datetime.strptime(attempt["time"], "%Y-%m-%dT%H:%M:%SZ")
                    .replace(tzinfo=datetime.timezone.utc)
                    .timestamp()
                )
                stale = mtime < attempt_epoch
            except ValueError:
                pass
        try:
            with open(path, errors="replace") as f:
                text = f.read()
        except OSError:
            return result
        return {"available": True, "stale": stale, "text": text}
    finally:
        subprocess.run(["umount", MNT_ROOT], capture_output=True)


DIAGNOSTIC_TEXT_MAX_BYTES = 200_000

# CSI (`\x1b[...final-byte`), OSC (`\x1b]...BEL-or-ST`), and simple
# two-character escape sequences - covers OpenRC's own colored
# "* Starting X ..." output (rc.log) and whatever a pstore record's
# captured console text happened to contain, without needing to
# enumerate every possible sequence a real terminal understands.
_ANSI_ESCAPE_RE = re.compile(r"\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])")
# Remaining C0 control bytes, except \n and \t - \r is deliberately
# stripped too (not preserved as a newline-equivalent): a lone \r left
# in dialog's textbox can move the cursor back to column 0 and let a
# later line overwrite the start of an earlier one, which is exactly
# the "corrupt the display" failure mode this function exists to
# prevent, not something to carefully preserve.
_CONTROL_RE = re.compile(r"[\x00-\x08\x0b-\x1f\x7f]")


def sanitize_diagnostic_text(text, max_bytes=DIAGNOSTIC_TEXT_MAX_BYTES):
    """rc.log and pstore crash records are raw text sourced from the
    TARGET's own boot - OpenRC's colored output, or whatever a kernel
    happened to write to a persistent-storage record before dying -
    genuinely untrusted input by the time it reaches this rescue
    session's own terminal. `errors="replace"` at the point each is
    read (see _rc_log_evidence()/previous_boot_evidence()) already
    handles invalid byte sequences; this handles the OTHER way raw
    target-sourced text can hurt the operator: ANSI/control sequences
    that could corrupt THIS terminal's own display state, or a record
    large enough to make `dialog --textbox` unpleasant or slow to open.

    Presentation hardening only - deliberately does not touch what the
    evidence text says or whether it contains "Kernel panic" (that
    detection already happens against the raw text before this ever
    runs) - only how safely it's rendered. Never applied to anything
    this project generates itself (dmesg, zpool status, this file's own
    messages) - see show_diagnostics()/show_boot_log(), neither of
    which call this, because their content isn't target-controlled.
    """
    text = _ANSI_ESCAPE_RE.sub("", text)
    text = _CONTROL_RE.sub("", text)
    encoded = text.encode("utf-8", errors="replace")
    if len(encoded) > max_bytes:
        text = encoded[:max_bytes].decode("utf-8", errors="ignore")
        text += "\n\n[... truncated - output exceeds the display limit ...]"
    return text


def show_previous_boot_diagnostics():
    """The Last Boot Diagnostics screen: one coherent view over every
    evidence source this project has for BOOTFS's last boot attempt,
    regardless of which backend actually captured what. Structure
    approved by the project owner:

        Previous boot: FAILED / OK   <- from bootcheck ONLY, see below
        Boot environment: ...
        Attempt: K[/max]
        Boot attempt: kernel/initrd/cmdline/time (see _last_attempt())
        OpenRC boot log: AVAILABLE / STALE / locked / UNAVAILABLE (see _rc_log_evidence())
        Kernel crash record: AVAILABLE / NOT AVAILABLE / UNSUPPORTED

    The headline is the one piece of this screen with a hard rule
    behind it, found by an adversarial audit as a real, live violation
    in an earlier version: it is derived ONLY from bootcheck's own
    armed:K counter (_bootcheck_k() above), NEVER from any evidence
    source being present or absent. "pstore empty -> clean boot" was
    exactly the bug - a machine mid-FORCED_RESCUE (bootcheck already
    proved the last attempt failed) would have been told "no evidence
    (clean boot)" purely because the crash-evidence backend happened to
    be empty. Silence from a backend means only "this backend has
    nothing to say" - never "the boot was fine".
    """
    k = _bootcheck_k(BOOTFS)
    if k is None:
        headline = "UNKNOWN (not tracked by bootcheck)"
    elif k > 0:
        headline = "FAILED"
    else:
        headline = "OK"
    lines = [f"Previous boot: {headline}", f"Boot environment: {BOOTFS}"]
    if k:
        max_raw = os.environ.get("ALPINE_ZFSBOOT_BOOTCHECK_MAX", "")
        lines.append(f"Attempt: {k}/{max_raw}" if max_raw.isdigit() and 1 <= int(max_raw) <= 99 else f"Attempt: {k}")

    attempt = _last_attempt(BOOTFS)
    lines.append("")
    lines.append("Boot attempt:")
    if attempt is None:
        lines.append("  (never recorded - no successful kexec attempt on this BE yet)")
    else:
        lines.append(f"  Kernel:  {attempt['kernel']}")
        lines.append(f"  Initrd:  {attempt['initrd']}")
        lines.append(f"  Cmdline: {attempt['cmdline']}")
        lines.append(f"  Time:    {attempt['time']}")

    rc_log = _rc_log_evidence(BOOTFS, attempt)
    lines.append("")
    lines.append("OpenRC boot log:")
    if rc_log["stale"]:
        lines.append("  STALE (rc.log predates this boot attempt; no OpenRC evidence from this attempt is available)")
    elif rc_log["available"]:
        lines.append("  AVAILABLE (/var/log/rc.log - view below)")
    elif rc_log["locked"]:
        lines.append("  requires unlocking this BE first (see 'Unlock encrypted root')")
    else:
        lines.append("  UNAVAILABLE (no rc.log could be read from this BE - see Recovery shell to investigate further)")

    lines.append("")
    lines.append("Kernel crash record:")
    evidence = previous_boot_evidence()
    if not _pstore_backend():
        lines.append("  UNSUPPORTED on this platform (no pstore backend registered - legacy BIOS has no EFI variables at all; on UEFI this can also mean the module isn't loaded)")
    elif evidence is None:
        lines.append("  NOT AVAILABLE (backend: EFI pstore) - no record captured, or cleared since by a confirmed-good boot")
    else:
        tag = "KERNEL PANIC" if evidence["panic"] else "evidence present, cause not identified"
        lines.append(f"  AVAILABLE (backend: EFI pstore) - {tag} - view below")

    if rc_log["available"] and not rc_log["stale"]:
        lines.append("")
        lines.append("=== OpenRC boot log (/var/log/rc.log) ===")
        lines.append(sanitize_diagnostic_text(rc_log["text"]))
    if evidence is not None:
        lines.append("")
        lines.append("=== Kernel crash record (EFI pstore) ===")
        lines.append(sanitize_diagnostic_text(evidence["text"]))

    dialog_textbox("Previous boot diagnostics", "\n".join(lines))


def show_boot_log():
    """Always in the menu, not just when something went wrong - real,
    requested usability feedback: seeing console/kernel output is
    useful any time someone is tracking something down, not only on a
    failed boot. If this boot actually failed to import a pool,
    POOL_IMPORT_ERROR (see that variable's own module-level comment,
    and _banner()'s own comment for why that text lives HERE and not
    embedded in every single menu frame's own banner) is the most
    relevant thing to show and takes priority; otherwise this falls
    back to the kernel's own ring buffer (dmesg) - still genuinely
    useful for a normal boot someone wants to inspect, not just an
    empty "nothing to see" screen. A dedicated dialog --textbox either
    way - scrollable, not clipped to whatever happened to fit like a
    fixed-size --msgbox would be.
    """
    if POOL_IMPORT_ERROR:
        dialog_textbox("Boot log", POOL_IMPORT_ERROR)
        return
    if FORCED_RESCUE:
        dialog_textbox("Boot log", FORCED_RESCUE)
        return
    try:
        dmesg = subprocess.run(["sh", "-c", "dmesg | tail -200"], capture_output=True, text=True).stdout
    except Exception as e:
        dmesg = f"(failed to run dmesg: {e!r})"
    dialog_textbox("Boot log", dmesg or "(no kernel log output captured)")


def edit_cmdline_and_boot():
    """A one-shot kernel cmdline addition for just this boot - never
    written back to the dataset's own persisted /etc/alpine-zfsboot-cmdline.
    For troubleshooting (e.g. a single boot with `single` or a debug
    flag) without permanently changing how a boot environment boots
    every other time.
    """
    dataset = dialog_inputbox("Edit cmdline & boot", "Boot environment to boot", BOOTFS)
    if dataset is None:
        return
    dataset = dataset or BOOTFS
    extra = dialog_inputbox("Edit cmdline & boot", "Extra kernel cmdline for THIS BOOT ONLY (not saved)")
    if extra is None:
        return
    print(f"booting {dataset} with cmdline override: {extra!r}")
    boot(dataset, cmdline_override=extra)


def manage_boot_environments():
    """Boot environment lifecycle: snapshot / clone (duplicate) /
    rollback / delete / set-as-default / persistent cmdline.
    """
    while True:
        bes = list_all_boot_environments()
        if not bes:
            dialog_msgbox("Manage boot environments", "No boot environments found on any imported pool.")
            return
        labels = [be + (" (bootfs)" if be == BOOTFS else "") for be in bes]
        idx = dialog_menu("Manage boot environments", labels)
        if idx is None:
            return
        _manage_one_be(bes[idx])


def _manage_one_be(dataset):
    pool = dataset.split("/", 1)[0]
    actions = [
        "Take a new snapshot",
        "Clone a snapshot into a new boot environment",
        "Rollback to a snapshot (DESTRUCTIVE)",
        "Delete a snapshot",
        "Delete this entire boot environment (DESTRUCTIVE)",
        "Set as default (bootfs)",
        "Set persistent kernel cmdline (ZFS property)",
        "View / reset failed-boot tracking (bootcheck)",
    ]
    while True:
        snaps = [
            name.split("@", 1)[1]
            for name in zfs_list(dataset)
            if name.startswith(dataset + "@")
        ]
        text = f"snapshots: {', '.join(snaps) if snaps else '(none)'}"
        action = dialog_menu(dataset, actions, text=text)
        if action is None:
            return

        if action == 0:
            name = dialog_inputbox("New snapshot", f"Snapshot name for {dataset}")
            if name:
                if subprocess.run(["zfs", "snapshot", f"{dataset}@{name}"]).returncode != 0:
                    dialog_msgbox("Snapshot failed", f"zfs snapshot {dataset}@{name} failed - see the terminal output above.")

        elif action == 1:
            if not snaps:
                dialog_msgbox("Clone", "No snapshots to clone from.")
                continue
            si = dialog_menu("Clone which snapshot?", snaps)
            if si is None:
                continue
            snap = snaps[si]
            newname = dialog_inputbox("Clone", "New boot environment name")
            if not newname:
                continue
            target = f"{pool}/ROOT/{newname}"
            # Checked before claiming success - confirmed a real gap:
            # this used to show "Clone done" unconditionally, so a
            # failed `zfs clone` (name collision, quota, whatever) told
            # the operator a new boot environment now exists when it
            # doesn't.
            if subprocess.run(["zfs", "clone", f"{dataset}@{snap}", target]).returncode == 0:
                # Copy the origin's bootcheck POLICY (armed vs off) to
                # the new clone, but NEVER its attempt-count HISTORY - a
                # clone always starts at a fresh armed:0 if the origin
                # was locally armed, since `zfs clone` itself does not
                # copy local user properties at all (confirmed against
                # real OpenZFS behavior, not assumed), so without this
                # the clone would simply start absent/unprotected
                # regardless of what the origin's own local policy was.
                # An origin whose value is only INHERITED (not local)
                # is left alone here on purpose - the clone already
                # inherits the same hierarchy default the origin did,
                # forcing it local would be a policy change this action
                # never asked for.
                origin_bootcheck = subprocess.run(
                    ["zfs", "get", "-H", "-o", "value,source", "org.alpinezfsboot:bootcheck", dataset],
                    capture_output=True, text=True,
                ).stdout.strip()
                origin_value, _, origin_source = origin_bootcheck.partition("\t")
                if origin_source == "local":
                    if origin_value.startswith("armed:"):
                        subprocess.run(["zfs", "set", "org.alpinezfsboot:bootcheck=armed:0", target])
                    elif origin_value == "off":
                        subprocess.run(["zfs", "set", "org.alpinezfsboot:bootcheck=off", target])
                dialog_msgbox(
                    "Clone done",
                    f"cloned. {target} is a new, independent boot environment - "
                    "not booted by default until you set it as bootfs.",
                )
            else:
                dialog_msgbox("Clone failed", f"zfs clone {dataset}@{snap} {target} failed - see the terminal output above.")

        elif action == 2:
            if not snaps:
                dialog_msgbox("Rollback", "No snapshots to roll back to.")
                continue
            si = dialog_menu("Rollback to which snapshot?", snaps)
            if si is None:
                continue
            snap = snaps[si]
            if dialog_yesno(
                "Rollback - confirm",
                f"THIS DESTROYS any snapshots/clones of {dataset} newer than @{snap}.\n\nProceed?",
                default_no=True,
            ):
                if subprocess.run(["zfs", "rollback", "-r", f"{dataset}@{snap}"]).returncode != 0:
                    dialog_msgbox("Rollback failed", f"zfs rollback -r {dataset}@{snap} failed - see the terminal output above.")

        elif action == 3:
            if not snaps:
                dialog_msgbox("Delete snapshot", "No snapshots to delete.")
                continue
            si = dialog_menu("Delete which snapshot?", snaps)
            if si is None:
                continue
            snap = snaps[si]
            if dialog_yesno("Delete snapshot - confirm", f"Delete {dataset}@{snap}?", default_no=True):
                if subprocess.run(["zfs", "destroy", f"{dataset}@{snap}"]).returncode != 0:
                    dialog_msgbox("Delete failed", f"zfs destroy {dataset}@{snap} failed - see the terminal output above.")

        elif action == 4:
            warn = f"THIS DESTROYS {dataset} AND ALL ITS SNAPSHOTS, permanently."
            if dataset == BOOTFS:
                warn += "\n\n(this is the CURRENT bootfs - deleting it leaves no default to boot)"
            be_name = dataset.rsplit("/", 1)[-1]
            typed = dialog_inputbox(
                "Delete boot environment - confirm",
                f"{warn}\n\nType the boot environment's name ({be_name}) to confirm",
            )
            if typed == be_name:
                # Checked before claiming success - the most destructive
                # action in this whole menu unconditionally said
                # "deleted" before, even if `zfs destroy -r` actually
                # failed (dataset busy/held) - the operator would have
                # no way to tell the difference until they noticed it
                # still listed later.
                if subprocess.run(["zfs", "destroy", "-r", dataset]).returncode == 0:
                    dialog_msgbox("Deleted", f"{dataset} deleted.")
                    return
                dialog_msgbox("Delete failed", f"zfs destroy -r {dataset} failed - see the terminal output above, {dataset} still exists.")
            else:
                dialog_msgbox("Not deleted", "confirmation did not match, not deleted.")

        elif action == 5:
            if subprocess.run(["zpool", "set", f"bootfs={dataset}", pool]).returncode == 0:
                dialog_msgbox("Bootfs set", f"bootfs on {pool} is now {dataset}.")
            else:
                dialog_msgbox("Bootfs set failed", f"zpool set bootfs={dataset} {pool} failed - see the terminal output above.")

        elif action == 6:
            current = subprocess.run(
                ["zfs", "get", "-H", "-o", "value", "org.alpinezfsboot:commandline", dataset],
                capture_output=True, text=True,
            ).stdout.strip()
            if current in ("", "-"):
                current = ""
            new = dialog_inputbox(
                "Persistent cmdline",
                f"current persisted cmdline: {current!r}\n\nNew persistent kernel cmdline (blank to clear)",
                current,
            )
            if new is None:
                continue
            if new:
                rc = subprocess.run(["zfs", "set", f"org.alpinezfsboot:commandline={new}", dataset]).returncode
            else:
                rc = subprocess.run(["zfs", "inherit", "org.alpinezfsboot:commandline", dataset]).returncode
            if rc == 0:
                dialog_msgbox("Cmdline set", f"persisted cmdline for {dataset} is now {new!r}")
            else:
                dialog_msgbox("Cmdline set failed", f"setting the persisted cmdline for {dataset} failed - see the terminal output above.")

        elif action == 7:
            # Same get/set/inherit shape as the persisted-cmdline action
            # just above - the design-review-requested visible control
            # for a feature whose only other interface was `zfs set`
            # from a recovery shell. value,source (not just value) so
            # the operator can tell a LOCAL armed:K count apart from a
            # policy merely inherited from higher in the pool hierarchy
            # (see boot-dataset.sh/init's own identical distinction).
            current = subprocess.run(
                ["zfs", "get", "-H", "-o", "value,source", "org.alpinezfsboot:bootcheck", dataset],
                capture_output=True, text=True,
            ).stdout.strip()
            value, _, source = current.partition("\t")
            if value in ("", "-"):
                value = "(not participating)"
            choices = [
                "Disable (bootcheck=off)",
                "Re-arm at 0 (bootcheck=armed:0)",
                "Clear override (zfs inherit)",
            ]
            ci = dialog_menu(
                "Bootcheck",
                choices,
                text=f"current: {value}  (source: {source or 'default'})",
            )
            if ci is None:
                continue
            if ci == 0:
                rc = subprocess.run(["zfs", "set", "org.alpinezfsboot:bootcheck=off", dataset]).returncode
            elif ci == 1:
                rc = subprocess.run(["zfs", "set", "org.alpinezfsboot:bootcheck=armed:0", dataset]).returncode
            else:
                rc = subprocess.run(["zfs", "inherit", "org.alpinezfsboot:bootcheck", dataset]).returncode
            if rc == 0:
                dialog_msgbox("Bootcheck updated", f"bootcheck for {dataset} updated.")
            else:
                dialog_msgbox("Bootcheck update failed", f"updating bootcheck for {dataset} failed - see the terminal output above.")


def _project_version():
    """The project's own human-facing release number (semver, e.g.
    "0.1.0") - see build.sh's own comment for why this is a SEPARATE
    thing from _build_stamp() below (that one is what the actual
    update-check machinery compares; this one is purely what a human
    reads on the boot screen). "dev" if missing - same reasoning as
    _build_stamp()'s own "unknown build" fallback: a dev/test build
    that didn't go through build.sh's stamp-writing step shouldn't
    crash a boot over a missing diagnostic file.
    """
    try:
        with open("/etc/alpine-zfsboot-version") as f:
            return f.read().strip() or "dev"
    except OSError:
        return "dev"


def _build_stamp():
    """Real, visible confirmation of which build is actually running -
    confirmed the hard way, twice in one evening: real confusion about
    whether a given boot was running a stale .EFI file left over from
    before some fix landed, which looks identical to a fresh build at
    a glance otherwise. "unknown build" rather than raising if the
    file is missing (e.g. a dev/test build that didn't go through
    build.sh's own stamp-writing step) - this is a diagnostic nicety,
    not something worth ever failing a boot over.

    The file itself holds a compact, sortable, cmdline-safe stamp
    (e.g. "20260910T003200Z") - the exact same value build.sh also
    bakes into the .cmdline PE section as alpine-zfsboot.version=, so cmd/
    tool's own version check never has to unpack an initramfs to read
    it (see build.sh's own comment on why one value feeds both).
    Reformatted here into the human-friendly form this banner used to
    store directly, since a boot screen is the wrong place to make
    someone read "20260910T003200Z" at a glance.
    """
    try:
        with open("/etc/alpine-zfsboot-build-stamp") as f:
            raw = f.read().strip()
    except OSError:
        return "unknown build"
    try:
        return datetime.datetime.strptime(raw, "%Y%m%dT%H%M%SZ").strftime("%Y-%m-%d %H:%M UTC")
    except ValueError:
        return raw  # not the expected format - show it verbatim rather than hide it


# Plain ASCII on purpose, no box-drawing/block-glyph Unicode - this
# has to render identically on a real Linux vga console AND a genuine
# ansi-capable serial terminal (see /init's own TERM comment), and a
# misaligned "fancy" glyph-font banner would read as less trustworthy,
# not more, on whichever of those can't render it correctly.
def _banner():
    text = (
        "============================================================\n"
        "   A L P I N E - Z F S B O O T\n"
        "   the Rolls Royce of ZFS boot for Alpine, by UniDoc\n"
        f"   version: {_project_version()} (built {_build_stamp()})\n"
        "============================================================\n"
        f"   Console: /dev/{ACTIVE_TTY} (see 'Switch console' to change)"
    )
    if POOL_IMPORT_ERROR:
        # ONE short, fixed-length line - NOT the actual error text (an
        # earlier version embedded the full, often multi-line
        # POOL_IMPORT_ERROR string directly here). Real, reported
        # usability problem with that: this banner is part of EVERY
        # single menu frame's own text, so a long error made an
        # already content-heavy dialog frame overflow the /init-forced
        # 24-row serial console - not a separate rendering bug, but
        # this project's own oversized content colliding with a fixed
        # real-estate budget it had just gone to real effort to
        # enforce (see IS_SERIAL_CONSOLE's own comment). The banner's
        # whole job is to stay short and constant; see show_boot_log()
        # for the actual message, in its own dedicated, scrollable
        # screen instead.
        text += (
            "\n============================================================\n"
            "   NO POOL WAS IMPORTED - see 'Boot log' below for why"
        )
    elif FORCED_RESCUE:
        # Same short-fixed-line treatment as POOL_IMPORT_ERROR above,
        # same reasoning - full text lives in show_boot_log() instead.
        # `elif`, not a separate `if`: the two conditions are mutually
        # exclusive by construction (bootcheck's own decision point in
        # /init is skipped entirely whenever POOL_IMPORT_ERROR is
        # already set - see that comment there), so there is no case
        # where both banners would need to show at once.
        text += (
            "\n============================================================\n"
            "   BOOT ATTEMPTS EXCEEDED - see 'Boot log' below for why"
        )
    return text


def _items():
    """A function, not a constant - "Switch console" and "Network" both
    show their own live current state right in the label (a real,
    requested change for Switch console: it used to say only "Switch
    console" with no indication anywhere in the menu of what pressing
    it would actually do; Network follows the identical reasoning).
    """
    # _network_addresses(), not _network_status() - a full source audit
    # found this menu label used the IPv4-only helper, which meant a
    # machine reachable ONLY over IPv6 (the exact, documented,
    # real-world Hetzner CAX rescue scenario _network_addresses()'s own
    # comment already calls out) showed "Network: not connected" in the
    # main menu the entire session, despite the operator actively being
    # connected right now, over this exact interface, via the very SSH
    # session showing them that label. _network_status() itself stays
    # unchanged (its two OTHER callers genuinely need IPv4-only: the
    # DHCP bring-up flow and its own "is up"/"still has no address"
    # confirmation dialogs, where an IPv6 address would be a real, not
    # merely cosmetic, false positive) - this is the one caller that
    # actually needs the honest overall picture, same reasoning as the
    # Diagnostics cockpit's own existing use of this same helper.
    addrs = _network_addresses()
    ip = addrs[0] if addrs else None
    # Same live-state-in-the-label convention as Switch console/Network
    # below - encryption state used to only ever become visible as a
    # side effect of picking a kernel or chrooting in; showing it here
    # unconditionally is what makes "Unlock encrypted root" a first-
    # class rescue concept rather than a hidden precondition.
    enc = None if POOL_IMPORT_ERROR else encryption_status(BOOTFS)
    if enc is None:
        unlock_label = "Unlock encrypted root (not encrypted)"
        lock_label = "Lock encrypted root (not encrypted)"
    elif enc["keystatus"] != "available":
        unlock_label = "Unlock encrypted root (LOCKED)"
        lock_label = "Lock encrypted root (already locked)"
    elif enc["handoff_ready"]:
        unlock_label = "Unlock encrypted root (unlocked, handoff ready)"
        lock_label = "Lock encrypted root"
    else:
        unlock_label = "Unlock encrypted root (unlocked, handoff NOT ready)"
        lock_label = "Lock encrypted root"
    # Same live-state-in-the-label convention again - but the state
    # shown is bootcheck's own armed:K counter, NEVER evidence presence
    # (see show_previous_boot_diagnostics()'s own comment on why). None
    # (pool didn't import, not tracked, off, inherited-only) must never
    # collapse to "OK" - that is the same claim-from-silence bug an
    # earlier audit fixed, just inverted.
    _diag_k = _bootcheck_k(BOOTFS)
    if _diag_k is None:
        diag_label = "Previous boot diagnostics (UNKNOWN)"
    elif _diag_k > 0:
        diag_label = "Previous boot diagnostics (FAILED)"
    else:
        diag_label = "Previous boot diagnostics (OK)"
    return [
        f"Boot default ({BOOTFS})",
        unlock_label,
        lock_label,
        "Select boot environment",
        "Select kernel",
        "Manage boot environments (snapshot/clone/rollback/delete)",
        "Chroot into a boot environment",
        "Diagnostics (pool status / disks / dmesg)",
        "Edit cmdline & boot",
        "Deploy new machine (zfs recv)",
        f"Switch console (current: /dev/{ACTIVE_TTY})",
        f"Network: {ip if ip else 'not connected'}",
        "Recovery shell",
        # Always present, not just when POOL_IMPORT_ERROR is set - real,
        # requested usability feedback: seeing console/kernel output is
        # useful any time someone is tracking something down, not only
        # on a failed boot. See show_boot_log()'s own comment for what
        # it shows in each case.
        "Boot log",
        diag_label,
    ]


# Distinct dialog exit code reserved for "this tick's own --timeout
# genuinely elapsed with nothing confirmed" (see _countdown_menu()) -
# any value not colliding with dialog's own real exit codes (0 OK, 1
# Cancel, 255 ESC) works; this one is arbitrary.
_TICK_TIMEOUT_CODE = 66


def _run_dialog_with_activity(cmd, env, real_tty_fd):
    """Runs `cmd` (a dialog invocation) attached to a pty THIS function
    manages, relaying real keystrokes to it and its rendering back to
    the real terminal - the only way to detect that the operator
    pressed ANYTHING at all while it ran, independent of whatever
    dialog itself reports.

    Confirmed empirically, against a real dialog binary driven through
    a real pseudo-terminal: on a genuine --timeout expiry, dialog's
    own stderr contains the literal string "timeout" - and it writes
    that EXACT same string whether the operator sat completely idle
    the whole time or spent it navigating with arrow keys and simply
    never pressed Enter. There is no way to ask dialog itself "did
    anything happen" - confirmed, not assumed, by driving a real
    dialog binary both ways and comparing the raw output byte for
    byte. This relay is what makes _countdown_menu() able to tell
    those two cases apart, which dialog alone provably cannot.

    Returns (exit_code, stderr_text, activity, nav_delta) - activity is
    True if even a single byte was ever read from the real terminal
    while dialog was running, whether or not dialog itself ever
    recognized it as a real selection. nav_delta is a best-effort net
    Down(+1)/Up(-1) arrow-key count seen in that same input - confirmed
    a real, reported UX bug otherwise: a tick that times out AFTER the
    operator navigated (but never confirmed) falls into a brand new,
    unrelated dialog invocation next (see _countdown_menu()'s own
    default_item handling), which has no memory of where they'd
    navigated to and defaults back to item 0 - a visible "flicker back
    to the top" the moment anyone hesitates for even one tick. Only
    plain arrow keys are tracked (not Home/End/PageUp/Down or mouse) -
    good enough for the common case without parsing dialog's own
    screen output to determine the exact highlighted item.
    """
    master_fd, slave_fd = pty.openpty()
    # See IS_SERIAL_CONSOLE's own module-level comment - a serial
    # console gets the exact same fixed, deterministic 24x80 contract
    # here as every dialog_*() call below does, with NO TIOCGWINSZ
    # query at all: not because the query would fail (after /init's own
    # `stty rows 24 cols 80`, it would likely just echo back 24x80
    # anyway), but because this project no longer trusts a geometry
    # query on a serial line for anything, full stop, rather than
    # trusting it "unless it looks wrong". tty0 keeps the real query -
    # a genuine Linux vt's reported geometry has never been the
    # reported problem, and forcing it to a fixed size there would only
    # make GOOD information worse.
    if IS_SERIAL_CONSOLE:
        rows, cols = 24, 80
    else:
        try:
            rows, cols = struct.unpack("HH", fcntl.ioctl(
                real_tty_fd, termios.TIOCGWINSZ, b"\0" * 8)[:4])
        except OSError:
            rows, cols = 0, 0
        if rows <= 0 or cols <= 0:
            rows, cols = 24, 80
    try:
        fcntl.ioctl(slave_fd, termios.TIOCSWINSZ,
                    struct.pack("HHHH", rows, cols, 0, 0))
    except OSError:
        pass  # best-effort - dialog falls back to its own default size if this fails

    # Clear the REAL terminal (not the new pty - that one's already
    # blank) before this tick's own dialog process starts - see
    # main()'s own identical comment for the full reasoning. Needed on
    # EVERY tick, not just the first: each tick is a brand-new dialog
    # process with no memory of what the previous tick (or, on tick 1,
    # whatever ran before this countdown even started) left on the
    # real screen, and rows/cols above already proved that screen can
    # genuinely be far larger than the modest area dialog itself will
    # go on to assume.
    try:
        os.write(real_tty_fd, b"\x1b[2J\x1b[H")
    except OSError:
        pass

    # err_r/err_w/fork can all still fail (resource exhaustion is
    # exactly the kind of thing more likely under a long rescue
    # session, not just in theory) - without this try/except, a
    # failure here would leak master_fd/slave_fd (already open above)
    # forever, one real pty pair per failed attempt, which could
    # itself eventually manifest as the exact "out of pty devices"
    # error this whole mechanism already caused once (see /init's own
    # devpts-mount fix) - a leak feeding the same failure mode it was
    # already blamed for once.
    err_r = err_w = None
    try:
        err_r, err_w = os.pipe()
        pid = os.fork()
    except Exception:
        for leaked_fd in (master_fd, slave_fd, err_r, err_w):
            if leaked_fd is not None:
                try:
                    os.close(leaked_fd)
                except OSError:
                    pass
        raise

    if pid == 0:
        try:
            os.close(master_fd)
            os.close(err_r)
            # real_tty_fd's own inherited copy isn't closed here on
            # purpose when it's fd 0 - dup2(slave_fd, 0) below closes
            # it as a side effect either way. Every real caller in
            # this codebase passes sys.stdin.fileno() (always 0), but
            # this function's own signature accepts any fd, so a
            # future caller passing something else wouldn't leak it.
            if real_tty_fd not in (0, 1, 2):
                try:
                    os.close(real_tty_fd)
                except OSError:
                    pass
            os.setsid()
            fcntl.ioctl(slave_fd, termios.TIOCSCTTY, 0)
            os.dup2(slave_fd, 0)
            os.dup2(slave_fd, 1)
            os.dup2(err_w, 2)
            os.close(slave_fd)
            os.close(err_w)
            os.execvpe(cmd[0], cmd, env)
        finally:
            # Only reached if execvpe itself failed to even start -
            # a successful exec replaces this process entirely and
            # never returns here at all.
            os._exit(127)

    os.close(slave_fd)
    os.close(err_w)

    # Both fds stay ordinary BLOCKING fds - deliberately, after a real
    # regression this exact choice caused once already. An earlier
    # version of this loop made both non-blocking specifically to avoid
    # a write ever stalling the loop (a real theoretical concern: select()
    # here only ever watched read-readiness, so a stalled write could
    # hang forever with no escape). The fix at the time (non-blocking +
    # queue-and-retry-on-EAGAIN) introduced a WORSE, real bug instead:
    # on a genuinely slower-than-local serial line, EAGAIN was the
    # *common* case, not the rare one, and the queue's own bounds
    # (MAX_PENDING, a bounded post-loop flush) meant dialog's entire
    # rendering got silently truncated/abandoned - confirmed the hard
    # way, a real boot ran the whole countdown with literally nothing
    # ever drawn, no crash either, just silence.
    #
    # The actual fix is simpler than either of those: a plain blocking
    # os.write() here is EXACTLY what dialog's own direct writes to
    # this same real terminal already did, successfully, for the whole
    # rest of this file's non-relayed dialog_menu()/dialog_msgbox()
    # calls, and for every line of kernel/boot output this project has
    # ever shown - self-throttling to line rate is the CORRECT, already
    # proven-safe behavior for this exact device, not a risk to design
    # around. select() below still avoids busy-waiting on reads; each
    # write that follows a read is allowed to block like any of this
    # project's other direct writes to the console already can.
    old_attrs = termios.tcgetattr(real_tty_fd)
    tty.setraw(real_tty_fd)
    activity = False
    nav_delta = 0
    pending_esc = b""
    stderr_chunks = []

    def _write_all(fd, data):
        # A single write(2) can legitimately accept fewer bytes than
        # asked even on a blocking fd (POSIX allows a short write) -
        # looping here is what actually guarantees full delivery of
        # one chunk, not a false sense of safety from a single call.
        while data:
            try:
                n = os.write(fd, data)
            except OSError:
                return  # destination genuinely gone - nothing more to do
            data = data[n:]

    try:
        while True:
            try:
                rlist, _, _ = select.select([master_fd, err_r, real_tty_fd], [], [])
            except OSError:
                # A real serial line (ttyS0 - not a pty) can raise here
                # in ways a pty never does. Nothing about "select
                # itself failed" tells us whether dialog is even still
                # running - waitpid below (WNOHANG) is what actually
                # decides that, not this loop guessing.
                try:
                    if os.waitpid(pid, os.WNOHANG) != (0, 0):
                        break
                except OSError:
                    break
                continue
            if err_r in rlist:
                try:
                    chunk = os.read(err_r, 4096)
                except OSError:
                    chunk = b""
                if chunk:
                    stderr_chunks.append(chunk)
            if master_fd in rlist:
                try:
                    chunk = os.read(master_fd, 4096)
                except OSError:
                    chunk = b""
                if not chunk:
                    break  # dialog exited and closed its end of the pty
                _write_all(real_tty_fd, chunk)
            if real_tty_fd in rlist:
                try:
                    chunk = os.read(real_tty_fd, 4096)
                except OSError:
                    chunk = b""
                if chunk:
                    activity = True
                    # A trailing 1-2 bytes kept from the previous read
                    # in case an escape sequence lands split across two
                    # reads (a real risk on a real, slower line) -
                    # scanned again as the start of THIS chunk rather
                    # than lost.
                    scan = pending_esc + chunk
                    nav_delta += scan.count(b"\x1b[B") - scan.count(b"\x1b[A")
                    pending_esc = scan[-2:]
                    _write_all(master_fd, chunk)
    finally:
        try:
            termios.tcsetattr(real_tty_fd, termios.TCSADRAIN, old_attrs)
        except OSError:
            pass
        try:
            os.close(master_fd)
        except OSError:
            pass
        try:
            while True:
                chunk = os.read(err_r, 4096)
                if not chunk:
                    break
                stderr_chunks.append(chunk)
        except OSError:
            pass
        try:
            os.close(err_r)
        except OSError:
            pass

    try:
        _, status = os.waitpid(pid, 0)
        exit_code = (status >> 8) & 0xFF
    except OSError:
        exit_code = _TICK_TIMEOUT_CODE
    return exit_code, b"".join(stderr_chunks).decode("utf-8", "replace").strip(), activity, nav_delta


def _countdown_menu(title, items, text, seconds, default_item=0):
    """One tick of the main menu's own live countdown - a REAL, fully
    navigable dialog --menu, shown for up to `seconds` more seconds.
    Returns (choice, returncode, activity, next_default_item):
      - choice is the selected index if returncode == 0 (a genuine
        confirmed OK/Enter on some item), else None.
      - returncode is dialog's own real exit code for anything else:
        1 = the Cancel button (labeled "Boot default" here) was
        explicitly pressed; _TICK_TIMEOUT_CODE = this tick's own
        duration genuinely elapsed with nothing confirmed; 255 (ESC) =
        explicitly backed out without choosing anything.
      - activity is True if the operator pressed ANYTHING at all
        during this tick, confirmed or not - see
        _run_dialog_with_activity()'s own comment for why this can't
        come from dialog itself and needs a real pty relay instead.
        main()'s own loop treats this the same as an explicit ESC:
        real activity, even unconfirmed, silences the countdown for
        good rather than letting it keep ticking down underneath
        someone who is visibly still there.
      - next_default_item is this tick's own default_item plus
        whatever net arrow-key navigation the relay observed, wrapped
        into range - see _run_dialog_with_activity()'s own nav_delta
        comment for the real, reported "flicker back to item 0" bug
        this closes: whoever calls this again next (another tick, or
        the unhurried dialog_menu() once the countdown stops) should
        pass THIS back in as their own default_item, so a tick that
        times out mid-navigation hands off to a fresh widget that
        opens exactly where the operator left it, not item 0.
    See this file's own header comment for why the full countdown is
    built from many of these short calls rather than one long one.
    """
    menu_args = []
    for i, item in enumerate(items):
        menu_args += [str(i), item]
    menu_height = min(len(items), 15)
    cmd = DIALOG_COMMON + [
        "--title", title,
        "--ok-label", "Select", "--cancel-label", "Boot default",
        "--default-item", str(default_item), "--timeout", str(seconds),
        "--menu", text, str(DIALOG_HEIGHT), str(DIALOG_WIDTH), str(menu_height),
    ] + menu_args
    # DIALOG_TIMEOUT gives a genuine timeout its own distinct exit code
    # (confirmed against dialog's own manpage) - without it, a real
    # timeout is indistinguishable from ESC (both 255), which would
    # make "the tick just ran out" look identical to "explicitly backed
    # out", and this function couldn't tell them apart at all.
    env = dict(os.environ, DIALOG_TIMEOUT=str(_TICK_TIMEOUT_CODE))
    # sys.stdin.fileno() is NOT a fd this loop can write to on a real
    # boot - confirmed the hard way (real hardware: the full countdown
    # ran out every single time, dialog never once appeared, no crash
    # anywhere). /init attaches this whole process to its console via
    # TWO SEPARATE `exec < /dev/X > /dev/X` opens (see /init's own
    # comment), not one shared read-write fd - stdin ends up a
    # genuinely read-only file description of the console. Every write
    # _run_dialog_with_activity() makes to real_tty_fd was therefore
    # hitting EBADF on every tick, caught by its own defensive
    # try/except and silently dropped - the exact same errno test
    # hardware once crashed on outright, before that except was added,
    # just now failing invisibly instead of loudly.
    #
    # /dev/tty FIRST, not /dev/{ACTIVE_TTY} - an earlier version of this
    # line had that backwards, on the reasoning that /dev/tty's
    # controlling-terminal linkage wasn't independently confirmed on
    # real hardware yet. It's confirmed now: main() unconditionally
    # calls _establish_controlling_terminal() before this loop ever
    # runs (see its own comment), which guarantees a real ctty exists
    # by this point either way - already correctly established by
    # dropbear's own pty setup over SSH, or just established here,
    # locally, if it didn't already exist. Opening /dev/{ACTIVE_TTY}
    # directly instead was ALSO a real, confirmed bug over SSH
    # specifically: ACTIVE_TTY silently defaults to "tty0" there
    # (dropbear wipes the env var it actually comes from - see this
    # file's own module-level comment), so this used to relay dialog's
    # rendering to the physical console's own tty0 device instead of
    # the real SSH pty, AND query ITS geometry via TIOCGWINSZ below
    # instead of the client's actual negotiated window size - both
    # silently "succeeding" (root can open any tty device node) while
    # doing something completely disconnected from the actual session.
    # Falls back to /dev/{ACTIVE_TTY}, then stdin, only if /dev/tty
    # itself somehow fails to open - loudly, via a real print, not a
    # silent fallback, so a next boot's log says definitively which
    # case fired rather than reproducing the exact same invisible
    # silence again.
    try:
        real_tty_fd = os.open("/dev/tty", os.O_RDWR)
    except OSError as e:
        print(f"alpine-zfsboot: could not open /dev/tty rw ({e}), trying /dev/{ACTIVE_TTY}")
        try:
            real_tty_fd = os.open(f"/dev/{ACTIVE_TTY}", os.O_RDWR)
        except OSError as e2:
            print(f"alpine-zfsboot: could not open /dev/{ACTIVE_TTY} either ({e2}), "
                  "falling back to stdin - rendering will likely be invisible")
            real_tty_fd = sys.stdin.fileno()
    try:
        exit_code, answer, activity, nav_delta = _run_dialog_with_activity(cmd, env, real_tty_fd)
    except Exception as e:
        # The pty relay is real, novel, low-level code - confirmed
        # twice on real hardware to fail in ways no local testing (all
        # against a pty, never a real serial line) caught in advance.
        # Falling back to a plain, un-relayed dialog call rather than
        # letting this exception reach main()'s own top-level handler
        # and abandon the menu for the whole rest of this boot - this
        # single tick just loses activity-detection (dialog's own
        # confirm/cancel/timeout reporting still works fine on its
        # own), not the entire countdown. Printed, not silent - a
        # previous round of real-hardware failures had NO visible
        # signal distinguishing "the relay raised and this fallback
        # ran" from "the relay ran but nothing rendered for some other
        # reason" - this line is what tells the two apart on the next
        # boot's log instead of guessing again.
        print(f"alpine-zfsboot: pty relay failed ({e!r}), falling back to plain dialog")
        proc = subprocess.run(cmd, stderr=subprocess.PIPE, text=True, env=env)
        answer = (proc.stderr or "").strip()
        # No nav_delta available from this fallback path (no relay ran
        # at all) - passes default_item straight through unchanged
        # rather than losing it.
        return (int(answer), 0, False, default_item) if proc.returncode == 0 and answer.lstrip("-").isdigit() \
            else (None, proc.returncode, False, default_item)
    finally:
        if real_tty_fd != sys.stdin.fileno():
            try:
                os.close(real_tty_fd)
            except OSError:
                pass
    next_default_item = (default_item + nav_delta) % len(items)
    if exit_code == 0 and answer.lstrip("-").isdigit():
        return int(answer), 0, activity, next_default_item
    if exit_code == 0:
        # dialog itself reported a confirmed OK/Enter, but the answer
        # it gave couldn't be parsed - main()'s own `rc == 0` check
        # treats this identically to an explicit, successful selection
        # of item 0 ("boot default"), which it is NOT: this is an
        # unexplained anomaly, not an operator choice. Same class of
        # gap as dialog_menu()'s own identical comment - printed so
        # it's visible rather than silently indistinguishable from a
        # real "boot default" confirm.
        print(f"alpine-zfsboot: dialog confirmed but returned an unparseable answer ({answer!r}) - treating as no selection", file=sys.stderr)
    return None, exit_code, activity, next_default_item


def _require_pool():
    """Guards every menu action below that assumes a real, imported
    pool with a mountable BOOTFS - see POOL_IMPORT_ERROR's own comment.
    Returns False (and shows exactly why) instead of proceeding into
    code that would otherwise fail confusingly deep inside zfs/mount/
    kexec calls that were never going to work without a real pool.
    """
    if not POOL_IMPORT_ERROR:
        return True
    # A short, fixed message pointing at "Boot log" - NOT the raw
    # POOL_IMPORT_ERROR text itself (an earlier version embedded it
    # directly here) - same reasoning as _banner()'s own identical
    # change: a real zpool error can be long/multi-line, and --msgbox
    # (unlike --textbox) doesn't scroll, so a long one here risked the
    # exact same content-overflows-fixed-box problem, just in a
    # smaller, less frequently hit box instead of every menu frame.
    dialog_msgbox("No pool", "Not available - no pool was imported. See 'Boot log' for why.")
    return False


def _cancellable(fn, *args, **kwargs):
    """Runs fn(*args), catching Ctrl-C so it cancels just THIS one
    action and returns control to the menu - not the whole process.

    A real, confirmed bug this closes: main()'s own top-level handler
    (see the bottom of this file) only catches Exception, not
    BaseException - KeyboardInterrupt is deliberately NOT an Exception
    subclass in Python precisely so code that means "catch everything
    that can go wrong" doesn't accidentally also swallow Ctrl-C, but
    that also means it was never caught anywhere in this file at all,
    and propagated all the way past that handler too, dumping a raw
    Python traceback and - critically, over SSH - ending the WHOLE
    login session the moment python3 itself then exited (dropbear tears
    the session down when the login shell/its exec'd replacement
    exits). Confirmed via real hardware evidence that Ctrl-C DOES
    correctly reach this process as a real SIGINT (subprocess.run()'s
    own child - e.g. boot-dataset.sh, mid `zfs load-key` retry - is in
    the same foreground process group and receives it directly too,
    which is what actually cancels whatever IT was doing; this process
    just also needs to survive the KeyboardInterrupt subprocess.run()
    raises once that child exits) - ruling out a broken SSH PTY as the
    cause of the visible symptom, which is what made this look like a
    job-control bug rather than a plain missing except clause.
    """
    try:
        return fn(*args, **kwargs)
    except KeyboardInterrupt:
        print("alpine-zfsboot: cancelled (Ctrl-C) - returning to menu", file=sys.stderr)
        return None


def _dispatch(choice):
    """Runs whichever action `choice` selects - shared by both the
    countdown phase and the fully unhurried phase below it, since a
    real confirmed choice means the same thing regardless of which
    tick it happened to arrive on.
    """
    if choice is None or choice == 0:
        if _require_pool():
            boot(BOOTFS)
    elif choice == 1:
        if _require_pool():
            unlock_encrypted_root()
    elif choice == 2:
        if _require_pool():
            lock_encrypted_root()
    elif choice == 3:
        if _require_pool():
            select_boot_environment()
    elif choice == 4:
        if _require_pool():
            select_kernel()
    elif choice == 5:
        if _require_pool():
            manage_boot_environments()
    elif choice == 6:
        if _require_pool():
            chroot_be()
    elif choice == 7:
        show_diagnostics()
    elif choice == 8:
        if _require_pool():
            edit_cmdline_and_boot()
    elif choice == 9:
        deploy()
    elif choice == 10:
        switch_console()
    elif choice == 11:
        network_menu()
    elif choice == 12:
        recovery_shell()
    elif choice == 13:
        show_boot_log()
    elif choice == 14:
        show_previous_boot_diagnostics()


def main():
    # A plain, direct print - not through dialog, not through the
    # countdown's own pty relay, nothing that can silently swallow
    # output - specifically so there is SOME visible sign of life on
    # the real console the instant this script starts, before ZFS
    # import/anything else that runs first has a chance to. Confirmed
    # a real need for this the hard way: a real boot showed nothing
    # at all between /init's own "importing pool" message and the
    # eventual automatic-boot kexec, with no way to tell whether
    # menu.py had even started, let alone how far it got.
    print(f"alpine-zfsboot by UniDoc, version {_project_version()} built at {_build_stamp()}")
    # Logged once, here - the actual geometry dialog will render
    # against is decided authoritatively by /init BEFORE this process
    # ever starts (a real, deterministic `stty rows/cols` for a serial
    # console, real kernel-reported vt geometry for tty0 - see /init's
    # own comment on its console-selection case statement for why that
    # single point, not a per-dialog-call guess here, is the right
    # place to fix a wrong terminal size). This is diagnostic visibility
    # only: confirms what TIOCGWINSZ actually reports for THIS boot,
    # so a report like "the TUI is corrupted" has a real rows/cols/TERM
    # data point in the log instead of nothing.
    try:
        _rows, _cols = struct.unpack("HH", fcntl.ioctl(
            sys.stdin.fileno(), termios.TIOCGWINSZ, b"\0" * 8)[:4])
    except OSError:
        _rows, _cols = 0, 0
    print(f"alpine-zfsboot: terminal size rows={_rows} cols={_cols} TERM={os.environ.get('TERM', '(unset)')}")

    # Before ANYTHING else touches dialog/curses - see _sanitize_term()'s
    # own comment for the real, confirmed bug this closes (an
    # unresolvable $TERM, not a broken PTY, is why a real SSH session
    # showed no menu at all).
    _sanitize_term()

    # Clear the ENTIRE real terminal, unconditionally, before dialog
    # ever draws anything - a real, confirmed rendering bug otherwise:
    # on a serial console (no real window-size negotiation - see
    # _run_dialog_with_activity()'s own TIOCGWINSZ comment), dialog/
    # ncurses can only assume a modest default (80x24) even when the
    # REAL terminal on the other end is far larger (confirmed via a
    # real screenshot: a terminal emulator over 300 columns wide).
    # dialog only ever draws (and clears) within the area IT believes
    # the screen to be, so anything already sitting further right or
    # down than that - the plain print() just above included - is
    # never touched again and stays frozen on screen for the entire
    # rest of the boot, visually overlapping every dialog frame drawn
    # afterward. ANSI "clear entire screen, cursor to home" is
    # unconditional and size-independent - a real terminal honors it
    # for whatever its own actual current dimensions are, regardless
    # of what dialog will go on to assume, and it also pins the
    # cursor to a known (0,0) position before the first ncurses
    # process of this boot ever starts, rather than wherever a
    # previous, unrelated program left it.
    try:
        sys.stdout.write("\x1b[2J\x1b[H")
        sys.stdout.flush()
    except OSError:
        pass

    # Once, here, for this process's whole remaining lifetime - see
    # _establish_controlling_terminal()'s own comment for why this
    # can't be done per-interactive-shell instead (that was tried
    # first, and was itself a real, confirmed bug).
    _establish_controlling_terminal()

    # Flush any input already sitting in the queue before dialog ever
    # starts - confirmed the hard way on real hardware that skipping
    # this makes the menu look like it skips straight to booting
    # instead of ever appearing: a single leftover buffered keystroke
    # (easy to end up with - pressing the same key more than once, not
    # yet knowing whether the first one registered) gets read by
    # dialog's own very first read() as an immediate "confirm the
    # highlighted item", before the widget is ever visibly seen.
    try:
        termios.tcflush(sys.stdin.fileno(), termios.TCIFLUSH)
    except (OSError, termios.error):
        # termios.error is not an OSError subclass on every Python build
        # (confirmed the hard way, adding ensure_key_loaded()'s own
        # identical flush - a non-tty stdin raises termios.error here,
        # uncaught by `except OSError` alone) - both are caught
        # explicitly rather than relying on that relationship.
        pass

    # Which item the NEXT dialog invocation should open on - carried
    # across ticks (and into the unhurried loop below) precisely so a
    # tick that times out mid-navigation doesn't hand off to a fresh
    # widget that's forgotten where the operator left it. See
    # _countdown_menu()'s own next_default_item comment.
    default_item = 0

    if POOL_IMPORT_ERROR or FORCED_RESCUE:
        # No pool imported, OR bootcheck's own decision point in /init
        # (see its comment there) determined this BE has exceeded its
        # failed-boot threshold and rescue SSH is up/forced - either
        # way, auto-booting toward the same BE that either can't be
        # booted at all, or has already failed to boot N times in a
        # row, is exactly the outcome this whole mechanism exists to
        # avoid. Skip straight to the unhurried interactive menu below,
        # opened on "Previous boot diagnostics" (always the last item -
        # see _items()'s own comment - the natural first thing to check
        # here, ahead of even "Boot log": it's this exact screen's job
        # to summarize what happened, with the raw boot log one level
        # deeper if more detail is needed) rather than "Boot default".
        # The two conditions are mutually exclusive by construction (see
        # /init's own bootcheck comment) so there's no ambiguity about
        # which message applies - _banner() and show_boot_log() above
        # already handle that the same way. Note POOL_IMPORT_ERROR alone
        # (no FORCED_RESCUE) lands on a screen whose own headline
        # correctly says "UNKNOWN", not "OK" or "FAILED" - bootcheck
        # itself was never consulted because the pool never imported.
        default_item = len(_items()) - 1
    else:
        remaining = MENU_TIMEOUT
        while remaining > 0:
            # The last tick is clamped to whatever's actually left, so
            # a MENU_TIMEOUT not evenly divisible by TICK_SECONDS still
            # ends exactly at 0, not slightly negative.
            tick = min(TICK_SECONDS, remaining)
            text = _banner() + f"\n\nBooting default in {remaining}s if left untouched"
            choice, rc, activity, default_item = _countdown_menu(
                "alpine-zfsboot", _items(), text, tick, default_item)
            if rc == 0:
                _cancellable(_dispatch, choice)
                break
            if rc == 1:
                # Cancel button, labeled "Boot default" here - matches
                # what it says.
                _cancellable(boot, BOOTFS)
            if activity:
                # Confirmed a real, repeated user report: navigating
                # with arrow keys but never pressing Enter within one
                # tick still let the countdown keep ticking down
                # underneath them, right past whatever they'd
                # navigated to - dialog itself has no way to report
                # "something happened" on a bare timeout (see
                # _run_dialog_with_activity()'s own comment), so this
                # tick's own pty relay is what actually detected it.
                # Any real activity at all, confirmed or not, silences
                # the countdown for good from here - same as an
                # explicit ESC below, not just a one-tick reprieve.
                break
            if rc != _TICK_TIMEOUT_CODE:
                # ESC - a real, explicit "stop counting" signal
                # distinct from just letting a tick run out. Falls
                # straight into the fully unhurried loop below, no
                # dispatch and no boot.
                break
            remaining -= tick
        else:
            # The while loop's own else - runs only if the loop ended
            # by its condition going false (remaining reached 0),
            # never by `break`: every single tick genuinely timed out,
            # with nothing ever confirmed the whole way through.
            _cancellable(boot, BOOTFS)

    # A human is confirmed present (or explicitly cancelled the
    # countdown) - no more time pressure from here on. A loop, not
    # recursion - every submenu action (manage/chroot/deploy/edit-
    # cmdline) returns back here, and a rescue session can sit at this
    # menu for a long time; recursing on every return would grow the
    # Python call stack for no reason.
    # Only the FIRST call here needs default_item - it's what carries
    # over whatever the operator had navigated to in the countdown's
    # last tick (see above); returning to this same menu after a
    # submenu action is unrelated existing behavior, not something
    # anyone has reported an issue with, so it's left resetting to 0
    # like it always has.
    def _menu_round(**kwargs):
        choice = dialog_menu("alpine-zfsboot", _items(), text=_banner(),
                              cancel_label="Boot default", **kwargs)
        _dispatch(choice)

    _cancellable(_menu_round, default_item=default_item)
    while True:
        _cancellable(_menu_round)


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        # A TUI failure must not strand the user with no way to boot
        # anything - fall back to the same automatic default path
        # /init itself would have taken. No curses screen state to
        # restore first (see this file's own header comment) - dialog
        # leaves nothing lingering to clean up here.
        #
        # EXCEPT when POOL_IMPORT_ERROR/FORCED_RESCUE is set - a real
        # gap this same check elsewhere in main() would otherwise miss:
        # this except block is reached from ANY uncaught exception
        # inside main(), including one raised while main() was already
        # in its own "don't auto-boot" branch (e.g. dialog_menu() itself
        # failing while showing the Boot-log-focused menu) - falling
        # through to boot(BOOTFS) here regardless would silently defeat
        # the entire point of either flag. /init's own final fallback
        # (`exec /boot-dataset.sh` in the "menu.py exited/crashed"
        # case) has the identical check for the identical reason - this
        # is the second of the three auto-boot paths that decision has
        # to cover, not just the countdown inside main() itself.
        print(f"alpine-zfsboot menu.py failed ({exc}), booting default", file=sys.stderr)
        if POOL_IMPORT_ERROR or FORCED_RESCUE:
            print("... except POOL_IMPORT_ERROR/FORCED_RESCUE is set - not auto-booting", file=sys.stderr)
        else:
            boot(BOOTFS)
