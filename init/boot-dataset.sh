#!/bin/sh
# boot-dataset.sh DATASET POOL [KERNEL_SUFFIX] [CMDLINE_OVERRIDE]
#
# Mounts DATASET read-only at /mnt/root, kexecs into the newest (or a
# specific, if KERNEL_SUFFIX is given) kernel/initramfs pair found on
# it, never returns on success. The one authoritative implementation
# of "how we actually boot something" - called both from /init's own
# final automatic-boot fallback and from menu.py's various menu
# choices, so there is exactly one place this logic lives and is
# tested, not one per caller.
#
# CMDLINE_OVERRIDE, if given (even as an empty string passed
# explicitly), replaces DATASET's own persisted cmdline for this ONE
# boot only - nothing is written back to the dataset. menu.py's "edit
# cmdline & boot" is the only caller that ever passes this; every
# other caller omits it and gets the persisted value as below.
#
# The persisted value itself comes from the org.alpinezfsboot:commandline
# ZFS property if set (readable immediately after `zpool import`, no
# mount needed - see menu.py's manage_boot_environments()), falling
# back to a plain /etc/alpine-zfsboot-cmdline file inside the dataset if the
# property was never set. The property is the preferred, "deeply ZFS"
# mechanism; the file remains as a fallback for anyone who'd rather
# just edit a file on a mounted root than run `zfs set`.
set -u
PATH=/sbin:/bin:/usr/sbin:/usr/bin
export PATH

DATASET="${1:?usage: boot-dataset.sh DATASET POOL [KERNEL_SUFFIX] [CMDLINE_OVERRIDE]}"
POOL="${2:?usage: boot-dataset.sh DATASET POOL [KERNEL_SUFFIX] [CMDLINE_OVERRIDE]}"
KERNEL_SUFFIX="${3:-}"
CMDLINE_OVERRIDE="${4-__unset__}"

# Empty at real boot (see /init's own ROOTFS comment - same reasoning,
# same variable) - only set by tests/run-tests.sh, to a scratch dir.
ROOTFS="${STUB_ROOT:-}"

# Single-owner boot lock for DATASET specifically - computed here,
# early, unconditionally, so it's always defined under `set -u` no
# matter which path through this script runs (including failure paths
# reached long before the actual mkdir attempt further down - see
# fail()'s own use of it). Sanitized because dataset names contain
# `/`, invalid inside a single path component - "zroot/ROOT/alpine"
# becomes "zroot_ROOT_alpine". Scoped to $ROOTFS/tmp like this file's
# other transient state (rescue-ssh.sh's own PIDFILE) - this whole
# initramfs is already tmpfs/ramfs-backed, wiped fresh every real boot,
# no separate /run mount needed or used anywhere else in this project.
boot_lock_dir="$ROOTFS/tmp/boot-lock.$(echo "$DATASET" | tr '/' '_')"

# Same real-boot-vs-test-harness path resolution as /init's own
# RESCUE_LIB_DIR (see its comment there for the full reasoning) -
# including the same dirname(1)-isn't-a-busybox-applet fix (confirmed
# missing the hard way, same session, same class of bug - this file
# had its own separate copy of the old `dirname` call that would have
# hit the identical "dirname: not found" failure).
_src="${BASH_SOURCE:-$0}"
case "$_src" in
    */*) _dir="${_src%/*}"; _dir="${_dir:-/}" ;;
    *)   _dir="." ;;
esac
RESCUE_LIB_DIR="$(cd "$_dir" && pwd)"

# Sourced, not exec'd, for the same reason /init sources it: this
# script is a completely separate process image by the time it runs
# (exec'd from /init or from menu.py's os.execv), so it needs its own
# copy of start_rescue_ssh/stop_rescue_ssh - the ALPINE_ZFSBOOT_SSH_*/
# NET_*/IPV4_*/IPV6_* environment variables it needs are already
# inherited from /init's own export (env vars survive exec; shell
# functions don't), one implementation either way.
. "$RESCUE_LIB_DIR/rescue-ssh.sh"

# The one shared implementation of "acquire a ZFS native-encryption
# passphrase interactively and stage it for the kexec handoff" - see
# its own header comment for the real, reported gap this closes (menu.py
# used to have its own, second copy of this logic that never staged a
# handoff secret).
. "$RESCUE_LIB_DIR/zfs-unlock.sh"

# Every message prefixed with seconds-since-boot (from /proc/uptime,
# not wall-clock date - correct even before any RTC/NTP sync has
# happened, and directly comparable to dmesg's own [n.nnnnnn] timestamps)
# - confirmed a real, reported need: a ~40 second blank screen between
# encryption unlock and the target kernel's own console output, with
# nothing to say which of encryption/mount/kexec/the target kernel's own
# boot it was actually spent in. One consistent timestamp on every step
# here beats guessing about ZFS/PBKDF2 overhead after the fact.
_elapsed() {
    awk '{printf "%.1f", $1}' "$ROOTFS/proc/uptime" 2>/dev/null || echo "?"
}
msg() {
    echo "alpine-zfsboot: [$(_elapsed)] $*" > /dev/kmsg 2>/dev/null
    echo "[$(_elapsed)] $*"
}

# decompress_initrd FILE - decompresses a whole initramfs image to
# stdout, detected purely from its own leading magic bytes rather than
# assumed. This is NOT the same question as "what compression does
# alpine-zfsboot's own build use" (see build.sh's own -C xz) - it
# decompresses a TARGET system's separate initramfs-$KERNEL_SUFFIX,
# built by that system's own, independent mkinitfs run, which may have
# chosen differently. gzip confirmed against a real Alpine install this
# project targets; xz supported on the same basis this project's own
# build already depends on the xz binary existing at all (see build.sh)
# - both decompress through tools bundled into THIS initramfs (gzip via
# busybox's own applet, xz explicitly listed in build.sh's alpine-
# zfsboot.files). Anything else (zstd, lz4, ...) falls through to a
# plain `cat` - correct only for an already-uncompressed cpio image,
# and caught as a real failure by the handoff block's own -s/exit-code
# checks further down rather than silently producing garbage.
decompress_initrd() {
    magic="$(od -An -tx1 -N6 "$1" 2>/dev/null | tr -d ' \n')"
    case "$magic" in
        1f8b*)        gzip -dc "$1" ;;
        fd377a585a00) xz -dc "$1" ;;
        *)            cat "$1" ;;
    esac
}

# fail MESSAGE - every failure path below goes through this, never a
# bare `exit`. This script is always reached via `exec` (from /init's
# automatic path, from menu.py's os.execv, or chained from
# alpine-zfsboot-shell) - by the time it runs, it well may BE pid 1's own
# process image, with nothing left to fall back to. A bare `exit`
# there doesn't return control to any caller - it kills pid 1 outright
# and panics the kernel ("Attempted to kill init!"). Dropping to a
# real shell instead is what /init's own die() and this script's
# kexec -e fallback already do; every other failure exit here now
# does the same, consistently.
fail() {
    msg "$*"
    umount "$ROOTFS"/mnt/root 2>/dev/null
    # Deterministic cleanup of the kexec handoff's own combined-initrd
    # file (see that block's own header comment, further down) on every
    # failure path, not just its own success path - the one gap a real
    # review of this file's secret lifecycle found: `kexec -l` failing
    # outright (as opposed to `kexec -e` failing AFTER a successful -l,
    # already handled by the plain rm right after that call succeeds)
    # would otherwise leave the plaintext-embedding combined-initrd
    # file sitting in this tmpfs indefinitely. ${var:-} throughout,
    # deliberately - fail() is called from failure paths reached long
    # before $kexec_initrd/$initrd are ever assigned (the encryption-
    # key-load failure, the mount failure, ...), and this runs under
    # `set -u`.
    if [ -n "${kexec_initrd:-}" ] && [ "${kexec_initrd:-}" != "${initrd:-}" ]; then
        rm -f "$kexec_initrd" 2>/dev/null
    fi
    # Same gap, same fix, for the staged plaintext passphrase itself
    # (see the handoff block's own comment on $zfs_key_stage) - a
    # successful unlock followed by ANY later failure (mount failed, no
    # matching kernel found, lost the boot-lock race) used to leave the
    # passphrase sitting in this tmpfs (0600, but indefinitely) with
    # nothing ever cleaning it up, since every OTHER cleanup of it lives
    # inside the handoff block itself, which a failure before reaching
    # that point never runs.
    if [ -n "${encryptionroot:-}" ] && [ "${encryptionroot:-}" != "-" ]; then
        rm -f "$(zfs_key_stage_path "$encryptionroot")" 2>/dev/null
    fi
    # Release the single-owner boot lock (see its own comment further
    # down) on every failure path, unconditionally - harmless no-op via
    # `2>/dev/null` if this particular fail() was reached before the
    # lock was ever acquired (several paths above the lock-acquisition
    # point call fail() too - the encryption-key-load failure, for
    # one). Without this, a human who reaches this recovery shell and
    # manually re-runs boot-dataset.sh for the same DATASET would be
    # incorrectly told "another session is already proceeding" by their
    # own, already-abandoned previous attempt's stale lock.
    rmdir "$boot_lock_dir" 2>/dev/null
    # One of this project's three rescue-ssh trigger points (see
    # rescue-ssh.sh's own header comment) - a real, recoverable
    # pre-pivot failure (mount failed, no matching kernel/initramfs,
    # kexec -l failed) with no menu.py/UI running from here on. No-ops
    # cleanly if no authorized_keys/host key were ever staged for this boot.
    start_rescue_ssh
    _recovery_shell
}

# _recovery_shell/_recovery_shell_child - give the operator a real,
# working emergency shell, regardless of whether this process is
# genuinely PID 1 (reached via /init's own final fallback `exec
# /boot-dataset.sh ...` - see /init's own header comment) or just an
# ordinary subprocess of menu.py's boot() (a plain subprocess.run(),
# never exec'd - see boot()'s own comment there for why). Same design,
# same reasoning, as /init's own identical pair of functions (this
# file has no way to share code with a separate process's own script,
# so it's duplicated rather than sourced).
#
# The two contexts need genuinely DIFFERENT handling, not just
# different terminal devices: as PID 1, this process must never exit
# (see below); as an ordinary subprocess, `exit`/`exec` behaving like
# any other child process's is completely fine - the whole point of
# menu.py using subprocess.run() rather than os.execv() in the first
# place (see boot()'s own comment) is that an operator's `exit` from
# THIS shell just ends that one subprocess and returns control to the
# menu, exactly like exiting recovery_shell() does.
#
# `[ "$$" -eq 1 ]` is also what tests/run-tests.sh's own dot-sourcing
# (never pid 1 - see run_stubbed()) relies on to safely exercise the
# not-PID-1 branch instead of hanging in a real fork+wait loop -
# lose_boot_race() already used this exact same check for the exact
# same reason before this existed.
_recovery_shell() {
    export PS1='\[\e[1;32m\]alpine-zfsboot\[\e[0m\] \w # '
    if [ -n "$ROOTFS" ]; then
        if [ "$$" -eq 1 ]; then
            msg "TEST STUB: would fork a recovery shell and wait forever (pid 1)"
        elif ( : < /dev/tty ) 2>/dev/null; then
            msg "TEST STUB: would exec /bin/bash directly (already has a controlling terminal)"
        else
            msg "TEST STUB: would exec setsid -c /bin/bash (no controlling terminal yet)"
        fi
        # `exit`, not `return` - every real call site below this point
        # is an `exec` that never returns to its own caller (the whole
        # point of a recovery/emergency shell). A plain `return` here
        # would instead let the REST of this script's top-level code
        # keep running right past whatever called this - a real,
        # confirmed regression the first version of this test stub
        # caused (a test staged for "encryption key never loads" ended
        # up mounting and kexec'ing an unrelated kernel anyway, because
        # returning from here just fell through to unrelated code
        # further down the script).
        exit 0
    fi
    if [ "$$" -eq 1 ]; then
        # PID 1 must NEVER exit - `exec`-ing straight into
        # `setsid -c /bin/bash` (as the old `exec setsid cttyhack
        # /bin/bash` effectively tried to) would make PID 1 itself
        # become `setsid`'s own waiting parent (busybox's `setsid`
        # always forks internally and waits for its own child, then
        # exits with the child's exit status - confirmed by direct
        # reproduction) - so the moment the operator's shell exits,
        # PID 1 would exit too: "Attempted to kill init!", a real
        # kernel panic, not a cosmetic bug. Forking here instead means
        # PID 1 keeps running as the loop below regardless of what the
        # recovery shell itself ever does.
        #
        # A fresh child every time around, not one child forked once
        # outside the loop - `wait` with no pid argument, once no
        # children remain to reap, returns immediately rather than
        # blocking (POSIX), which would otherwise turn this into a
        # tight, permanent 100%-CPU busy-loop the instant the operator
        # exits that first shell. Waiting for a SPECIFIC pid blocks
        # correctly, and re-forking on every iteration means exiting
        # the shell (accidentally or on purpose) hands the operator a
        # brand new one instead of leaving PID 1 alive but permanently
        # stuck with no usable shell for the rest of this boot.
        while true; do
            ( _recovery_shell_child ) &
            wait "$!"
        done
    fi
    _recovery_shell_child
}

_recovery_shell_child() {
    if ( : < /dev/tty ) 2>/dev/null; then
        # Already has a controlling terminal (dropbear's own pty over
        # SSH, or any other already-correct case) - nothing to
        # establish, just use it. See menu.py's own
        # _establish_controlling_terminal() for the identical
        # capability test and the same reasoning.
        exec /bin/bash
    fi
    # No controlling terminal yet - the real local-console case
    # (reached here as pid 1, or as a subprocess that itself never got
    # one - either way, the same fix applies). `cttyhack` (a standard
    # busybox applet built for exactly this) is NOT compiled into this
    # project's busybox at all - confirmed via `strings`/applet-table
    # inspection, not a PATH issue - meaning this emergency shell had
    # NEVER actually worked, locally or over SSH, since it was first
    # written (the visible symptom was always "cttyhack: applet not
    # found", silently swallowed by the shell exiting immediately).
    # `setsid -c` (bare `setsid`, which resolves to the busybox applet
    # via /init's own `busybox --install`, not a coreutils binary) is
    # used instead - confirmed by direct, hands-on reproduction against
    # this exact busybox version to correctly claim an
    # available-but-unclaimed pty as the new child's own controlling
    # terminal, and to never touch the calling process's own
    # session/tty state either way, in every scenario tested.
    exec setsid -c /bin/bash
}

# lose_boot_race - called when this process loses the single-owner
# boot lock below to some other process already proceeding to boot
# this same DATASET. Deliberately NOT routed through fail(): nothing
# is actually broken here, a calm backoff, not an error.
#
# Still has to respect the exact same pid-1 hazard fail() itself
# documents: this process may or may not currently BE pid 1 depending
# on which path led here (see this file's own header comment - a
# non-exec'd, plain `python3 /menu.py` child that itself os.execv'd
# into this script is NOT pid 1; /init's own final fallback `exec
# /boot-dataset.sh ...` IS). A bare `exit` on the pid-1 path would
# panic the kernel ("Attempted to kill init!"); the same safe
# _recovery_shell() fail() itself uses covers that case here too, just
# worded calmly, not as FATAL/error output. Every other case (by far
# the common one here - a remote SSH session's own boot-dataset.sh
# instance, a child of that session, never pid 1) is safe to just exit
# cleanly - that just ends the one SSH command normally, the same way
# any other finished remote command would.
lose_boot_race() {
    msg "another session is already proceeding with booting $DATASET - stepping back"
    if [ "$$" -eq 1 ]; then
        _recovery_shell
    fi
    exit 0
}

# Encryption: keystatus/keylocation/encryptionroot are all readable on
# an imported-but-unmounted dataset - no need to attempt the mount
# first just to find out a key is missing. encryptionroot is "-" for
# an entirely unencrypted dataset, which is the common case and must
# stay exactly as fast as before this existed: one extra `zfs get`,
# then straight through to the mount below, unchanged.
#
# The actual prompt/retry/staging mechanics live in zfs-unlock.sh's own
# zfs_unlock() (sourced above) - the ONE implementation shared with
# menu.py's "Unlock encrypted root" action, see that file's own header
# comment for the real gap this closes. What stays HERE, boot-flow-
# specific, is: starting rescue SSH before a local prompt that might
# otherwise block forever with nobody physically present to answer it,
# stopping it again once resolved, and routing an unrecoverable failure
# through fail() (this project's one PID-1-safe emergency-shell entry
# point - see that function's own comment).
encryptionroot="$(zfs get -H -o value encryptionroot "$DATASET" 2>/dev/null)"
if [ -n "$encryptionroot" ] && [ "$encryptionroot" != "-" ]; then
    keystatus="$(zfs get -H -o value keystatus "$encryptionroot" 2>/dev/null)"
    keylocation="$(zfs get -H -o value keylocation "$encryptionroot" 2>/dev/null)"
    if [ "$keystatus" != "available" ] && [ "$keylocation" = "prompt" ]; then
        # One of this project's three rescue-ssh trigger points (see
        # rescue-ssh.sh's own header comment) - started BEFORE
        # zfs_unlock()'s own prompt, not after it gives up: on hardware
        # with no local keyboard at all (the real scenario this was
        # built for - a remote bare-metal box), that prompt would
        # otherwise block forever with nobody ever able to answer it.
        # No-ops cleanly if no authorized_keys/host key were staged.
        start_rescue_ssh
    fi
    if zfs_unlock "$encryptionroot"; then
        if zfs_handoff_ready "$encryptionroot"; then
            msg "$encryptionroot is unlocked, kexec handoff ready"
        else
            msg "$encryptionroot is unlocked but the kexec handoff has no staged secret - the target initramfs will prompt again"
        fi
        # Resolved (locally or remotely) - don't leave rescue SSH
        # listening any longer than the problem it was started for.
        # Told BEFORE actually killing dropbear, not after - confirmed
        # on real hardware: to an operator connected over this exact
        # session, dropbear stopping right before kexec (which tears
        # down this whole rescue kernel, PTY and SSH session included -
        # there is no way to keep the connection alive across it) looks
        # exactly like the connection just died for no reason, unless
        # something says so first. _running() (from rescue-ssh.sh) -
        # only says this if rescue SSH was ever actually started this
        # boot; a purely local, non-rescue boot has nothing to announce.
        if _running; then
            msg "boot handoff successful - rescue SSH is shutting down, continuing boot via kexec. This connection will end now (kexec replaces this rescue kernel entirely) - reconnect once the target machine is back up."
        fi
        stop_rescue_ssh
    else
        fail "failed to load encryption key for $encryptionroot"
    fi
fi

# Single-owner lock: only one process may ever proceed past this
# point to actually mount+kexec DATASET. Real race this closes, not a
# hypothetical one: the encryption-wait logic above lets a REMOTE
# operator's own, completely separate menu.py/boot-dataset.sh session
# unlock this exact dataset out from under a LOCAL process that's also
# about to boot it (see that block's own comment) - once unlocked,
# both could independently reach here and both attempt their own
# mount+kexec of the SAME dataset, a genuine double-kexec race, not
# just wasted work.
#
# `mkdir` specifically because it's atomic on a POSIX filesystem -
# exactly one caller's mkdir can ever succeed against the same path,
# even under real concurrent attempts, with no check-then-act TOCTOU
# gap the way "does a marker file exist, if not write it" would have.
#
# Deliberately unconditional - not scoped to only the encryption path
# above: any two concurrent attempts to boot the exact same dataset
# (encrypted or not - two operators both triggering the same boot from
# their own independent menu.py sessions, say) hit the identical real
# hazard, and one single invariant here is simpler to reason about
# than special-casing "was this reached via the encryption-wait path
# or not". $boot_lock_dir is scoped to DATASET specifically (computed
# at the top of this file) - two DIFFERENT datasets booting
# concurrently is legitimate and must never contend with each other.
if ! mkdir "$boot_lock_dir" 2>/dev/null; then
    lose_boot_race
fi

msg "mounting $DATASET read-only"
mkdir -p "$ROOTFS"/mnt/root
umount "$ROOTFS"/mnt/root 2>/dev/null
if ! mount -t zfs -o ro "$DATASET" "$ROOTFS"/mnt/root 2>/tmp/mount.log; then
    cat /tmp/mount.log
    fail "mount of $DATASET failed"
fi
msg "mounted $DATASET"

if [ -z "$KERNEL_SUFFIX" ]; then
    # Alpine's linux-<flavor> packages overwrite /boot/vmlinuz-<flavor>
    # in place on upgrade - a normal dataset has exactly ONE file per
    # flavor, so there is no real "version" to compare between
    # vmlinuz-* candidates in general (a second file only exists
    # because a human placed it there, e.g. a hand-kept backup kernel,
    # under whatever name they chose). `sort -V | tail -1` is a
    # deterministic tiebreak for that case, not a genuine version
    # comparison - if several kernels are meant to coexist on purpose,
    # use menu.py's explicit "Select kernel" instead of relying on this
    # picking the one you mean.
    KERNEL_SUFFIX="$(find "$ROOTFS"/mnt/root/boot -maxdepth 1 -name 'vmlinuz-*' 2>/dev/null \
        | sed 's#.*/vmlinuz-##' | sort -V | tail -1)"
fi
kernel="$ROOTFS/mnt/root/boot/vmlinuz-$KERNEL_SUFFIX"
initrd="$ROOTFS/mnt/root/boot/initramfs-$KERNEL_SUFFIX"

if [ ! -r "$kernel" ] || [ ! -r "$initrd" ]; then
    fail "no matching kernel/initramfs on $DATASET (kernel=$kernel initrd=$initrd)"
fi
msg "found kernel/initramfs: vmlinuz-$KERNEL_SUFFIX + initramfs-$KERNEL_SUFFIX"

if [ "$CMDLINE_OVERRIDE" = "__unset__" ]; then
    extra_cmdline="$(zfs get -H -o value org.alpinezfsboot:commandline "$DATASET" 2>/dev/null)"
    if [ -z "$extra_cmdline" ] || [ "$extra_cmdline" = "-" ]; then
        extra_cmdline=""
        [ -r "$ROOTFS"/mnt/root/etc/alpine-zfsboot-cmdline ] && extra_cmdline="$(cat "$ROOTFS"/mnt/root/etc/alpine-zfsboot-cmdline)"
    fi
else
    extra_cmdline="$CMDLINE_OVERRIDE"
fi

# console=: confirmed missing the hard way on real hardware - with
# none of the above ever specifying one, the target kernel fell back
# to its OWN default console, which isn't necessarily the same one the
# rescue environment (and the operator watching it) was actually on -
# a real boot showed no output at all past the kexec message, on a
# console that DID work fine right up until that point. $ALPINE_ZFSBOOT_ACTIVE_TTY
# is exported by /init (or inherited through menu.py's own environment,
# which got it from /init the same way) - reuse it here so the target
# system logs to the exact same console, same tty0/ttyS0/ttyS1/ttyS2/
# ttyAMA0 -> baud mapping switch_console() offers (see menu.py). Only added
# if $extra_cmdline doesn't already name its own console= - an explicit
# persisted choice there is a deliberate override, not an oversight to
# paper over.
case "$extra_cmdline" in
    *console=*) console_cmdline="" ;;
    *)
        case "${ALPINE_ZFSBOOT_ACTIVE_TTY:-tty0}" in
            tty0)    console_cmdline="console=tty0" ;;
            ttyS0)   console_cmdline="console=ttyS0,115200n8" ;;
            ttyS1)   console_cmdline="console=ttyS1,115200n8" ;;
            ttyS2)   console_cmdline="console=ttyS2,115200n8" ;;
            ttyAMA0) console_cmdline="console=ttyAMA0,115200n8" ;;
            *)       console_cmdline="" ;;
        esac
        ;;
esac

# panic=: a headless physical machine sitting forever at a kernel
# panic screen is bad boot policy on its own, independent of whether
# bootcheck (this project's own failed-boot-detection feature, see
# this file's own "content-derived arming" comment further down) is
# even in use for this BE - general policy, not an implementation
# detail of that counter, so it's injected unconditionally here, not
# gated on bootcheck state. Only added if $extra_cmdline doesn't
# already name its own panic= - same "an explicit persisted choice is
# a deliberate override" rule console= above already follows. 0 is a
# legal, meaningful value (explicit "never auto-reboot on panic", for
# deliberate crash debugging) - ALPINE_ZFSBOOT_PANIC_TIMEOUT being
# EMPTY (never configured at all) must fall back to this project's own
# conservative default instead, so "unset" and "explicitly 0" stay
# distinguishable even though both would otherwise just look like
# empty/zero strings. ${:-} throughout - this script is called
# directly by tests and by menu.py's own os.execv, neither of which is
# guaranteed to have gone through /init's own export of this var first.
case "$extra_cmdline" in
    *panic=*) panic_cmdline="" ;;
    *)
        case "${ALPINE_ZFSBOOT_PANIC_TIMEOUT:-}" in
            ''|*[!0-9]*) panic_cmdline="panic=10" ;;
            *)           panic_cmdline="panic=${ALPINE_ZFSBOOT_PANIC_TIMEOUT}" ;;
        esac
        ;;
esac

# Built up piece by piece rather than one interpolated string - a
# plain "... ro $console_cmdline $extra_cmdline" leaves a stray double
# space whenever console_cmdline is empty (i.e. whenever extra_cmdline
# already has its own console=), which is harmless to the kernel's own
# cmdline parsing but not the kind of thing worth leaving sloppy.
full_cmdline="root=ZFS=$DATASET ro"
[ -n "$console_cmdline" ] && full_cmdline="$full_cmdline $console_cmdline"
[ -n "$panic_cmdline" ] && full_cmdline="$full_cmdline $panic_cmdline"
[ -n "$extra_cmdline" ] && full_cmdline="$full_cmdline $extra_cmdline"

msg "kexec: $kernel + $initrd (root=ZFS=$DATASET)"

# Encrypted-root kexec handoff: without this, the target kernel's own
# stock initramfs would ask for the SAME passphrase a second time -
# kexec does not preserve ZFS's in-kernel key state across the jump to
# a new kernel, and Alpine's own initramfs-init calls bare `zfs
# load-key "$_encryption_root"` with no override, unconditionally, any
# time it finds an encrypted root not yet unlocked.
#
# Mechanism: append a small extra cpio segment to the target's own
# initrd before handing it to kexec. Linux's initramfs format natively
# supports a sequence of concatenated (compressed or plain) cpio
# archives, unpacked in order onto the SAME rootfs, with a later
# archive's files overwriting a same-path file from an earlier one -
# the documented mechanism behind things like dracut's own "extra
# image" / CoreOS Ignition's config injection, not a hack specific to
# this project.
#
# What gets overwritten: /usr/sbin/zfs itself (the real path on a real
# Alpine-built target initramfs - confirmed directly against one,
# rather than assumed), replaced with a tiny wrapper that intercepts
# exactly the one call prepare_zfs_root() makes AFTER it has already
# run `zpool import` (so the pool genuinely exists to unlock by the
# time this runs) - `-L file://...` overrides keylocation for that ONE
# invocation only. The dataset's own persisted keylocation property is
# NEVER touched (stays "prompt" forever) - a boot that ever reaches the
# target initramfs without this handoff (no alpine-zfsboot kexec, ever)
# still gets a completely ordinary interactive prompt, no fallback
# footgun, no persistent state changed on disk anywhere.
#
# The wrapper's own fallthrough (every OTHER zfs subcommand Alpine's
# init might still call) cannot just re-exec plain "zfs" - alpine-
# zfsboot's OWN zfs binary is gone by the time this runs (it was part
# of THIS initramfs, which stops existing the moment the target
# kernel's own initramfs becomes the running rootfs after kexec) - so
# the real target-side zfs binary is extracted from the target's own
# initrd first (decompress_initrd + a single-file cpio extract, read-
# only, no modification of the original archive's own bytes) and
# renamed alongside the wrapper, not borrowed from here.
#
# Failure anywhere in here (compression not recognized, path not
# found, cpio unavailable) falls back to $initrd unchanged, logged as a
# warning, not a boot failure - worst case is the target simply prompts
# again, exactly today's behavior.
kexec_initrd="$initrd"
if [ -n "$encryptionroot" ] && [ "$encryptionroot" != "-" ]; then
    zfs_key_stage="$(zfs_key_stage_path "$encryptionroot")"
    if [ -r "$zfs_key_stage" ]; then
        # This staged copy only ever gets rm'd by the ZFSWRAP wrapper
        # below, inside its own "zfs load-key" branch, once the target
        # actually calls it - a target init that never invokes this
        # wrapper's load-key path at all leaves the key sitting in
        # /run/alpine-zfsboot/zfs-key inside the TARGET's own initramfs
        # for the rest of that boot. That's tmpfs/RAM, wiped on the
        # next real reboot, not disk - but it is an assumption about
        # Alpine's own init calling this, not a guarantee enforced here.
        handoff_root="$ROOTFS/tmp/zfs-handoff.$$"
        rm -rf "$handoff_root"
        mkdir -p "$handoff_root/usr/sbin" "$handoff_root/run/alpine-zfsboot"
        if decompress_initrd "$initrd" | cpio -i --to-stdout usr/sbin/zfs \
                > "$handoff_root/usr/sbin/zfs.alpine-zfsboot-real" 2>/dev/null \
           && [ -s "$handoff_root/usr/sbin/zfs.alpine-zfsboot-real" ]; then
            chmod 755 "$handoff_root/usr/sbin/zfs.alpine-zfsboot-real"
            cat > "$handoff_root/usr/sbin/zfs" <<'ZFSWRAP'
#!/bin/sh
# Unconditional, every-invocation sentinel - a real boot showed the
# archive round-trip verifying correctly (see boot-dataset.sh's own
# handoff block) and the target STILL re-prompting from scratch, which
# is ambiguous between two very different bugs: this wrapper file never
# actually being what Alpine's own init executes as "zfs" at all
# (proving the cpio overwrite itself never took effect on the real
# running system, despite the archive being provably correct on disk),
# versus this wrapper running but the staged key content itself being
# wrong. No sentinel at all on the next boot means the first bug; a
# sentinel followed by a wrong-key error narrows straight to the
# second. /dev/console, not /dev/kmsg - visible inline in the same
# boot log the operator is already watching, not something that
# requires a separate `dmesg` afterward.
echo "ALPINE-ZFSBOOT: handoff wrapper invoked: zfs $*" >/dev/console 2>/dev/null
if [ "$1" = "load-key" ] && [ -r /run/alpine-zfsboot/zfs-key ]; then
    # Byte count only, never the key content itself.
    echo "ALPINE-ZFSBOOT: key bytes=$(wc -c < /run/alpine-zfsboot/zfs-key 2>/dev/null)" >/dev/console 2>/dev/null
    shift
    # -n: a real, documented OpenZFS dry run - verifies the key would
    # be accepted without actually loading it (no state change), run
    # first specifically so a wrong-key failure is visible as its own
    # distinct, clearly-labeled line before the real attempt, rather
    # than only showing up as Alpine's own generic re-prompt with no
    # indication the handoff was ever involved at all.
    /usr/sbin/zfs.alpine-zfsboot-real load-key -n -L file:///run/alpine-zfsboot/zfs-key "$@" >/dev/console 2>&1
    echo "ALPINE-ZFSBOOT: dry-run (-n) exit=$?" >/dev/console 2>/dev/null
    if /usr/sbin/zfs.alpine-zfsboot-real load-key -L file:///run/alpine-zfsboot/zfs-key "$@"; then
        rm -f /run/alpine-zfsboot/zfs-key
        exit 0
    fi
    # The handoff key was rejected - fall through to a PLAIN load-key
    # call (no -L), which reads from the dataset's own persisted
    # keylocation property (still "prompt", never touched by any of
    # this - see this block's own design notes) - a real normal
    # interactive prompt, exactly what would have happened with no
    # handoff at all. Exiting with the failed status here instead (an
    # earlier version of this wrapper did) is NOT equivalent: Alpine's
    # own prepare_zfs_root() never checks this call's exit status at
    # all (bare `eval zfs load-key "$_encryption_root"`, no &&/||/eend
    # around it - confirmed against the real script), so a nonzero exit
    # here doesn't trigger a retry or a prompt - it just falls through
    # silently to the next line's `mount ... $sysroot`, which fails
    # because the dataset is still locked, landing in Alpine's own
    # emergency recovery shell instead of the graceful "just prompts
    # again" this block's own design promises.
    echo "ALPINE-ZFSBOOT: handoff key rejected - falling back to a normal interactive prompt" >/dev/console 2>/dev/null
    rm -f /run/alpine-zfsboot/zfs-key
    exec /usr/sbin/zfs.alpine-zfsboot-real load-key "$@"
fi
exec /usr/sbin/zfs.alpine-zfsboot-real "$@"
ZFSWRAP
            chmod 755 "$handoff_root/usr/sbin/zfs"
            cp "$zfs_key_stage" "$handoff_root/run/alpine-zfsboot/zfs-key"
            chmod 400 "$handoff_root/run/alpine-zfsboot/zfs-key"
            handoff_cpio="$ROOTFS/tmp/zfs-handoff.$$.cpio"
            if ( cd "$handoff_root" && find . | cpio -o -H newc 2>/dev/null > "$handoff_cpio" ) \
               && [ -s "$handoff_cpio" ]; then
                # Round-trip verification, not just "cpio -o exited 0
                # and wrote non-empty output" - a real boot showed the
                # handoff having NO effect at all (target still
                # prompted normally, unwrapped) despite this exact
                # construction reporting success right up to this
                # point, so "success" from here on means the archive
                # was actually extracted back out and its real content
                # inspected - the wrapper and the real binary are both
                # genuine, non-symlink, executable regular files (cpio
                # represents a symlink's own "content" as its target
                # path string, not file bytes - a real risk if
                # usr/sbin/zfs in the target's own initrd ever turned
                # out to be a symlink rather than a plain binary, which
                # would make the earlier extraction step silently
                # capture a short text string instead), and the
                # extracted "real" binary starts with an actual ELF
                # magic number, not truncated/garbage content.
                handoff_check="$ROOTFS/tmp/zfs-handoff-check.$$"
                rm -rf "$handoff_check"
                mkdir -p "$handoff_check"
                ( cd "$handoff_check" && cpio -idm --quiet < "$handoff_cpio" 2>/dev/null )
                handoff_ok=1
                if [ ! -f "$handoff_check/usr/sbin/zfs" ] || [ -L "$handoff_check/usr/sbin/zfs" ] \
                   || [ ! -x "$handoff_check/usr/sbin/zfs" ]; then
                    handoff_ok=0
                fi
                if [ ! -f "$handoff_check/usr/sbin/zfs.alpine-zfsboot-real" ] \
                   || [ -L "$handoff_check/usr/sbin/zfs.alpine-zfsboot-real" ] \
                   || [ ! -x "$handoff_check/usr/sbin/zfs.alpine-zfsboot-real" ]; then
                    handoff_ok=0
                elif [ "$(od -An -tx1 -N4 "$handoff_check/usr/sbin/zfs.alpine-zfsboot-real" 2>/dev/null | tr -d ' \n')" != "7f454c46" ]; then
                    handoff_ok=0
                fi
                [ -f "$handoff_check/run/alpine-zfsboot/zfs-key" ] || handoff_ok=0
                rm -rf "$handoff_check"
                if [ "$handoff_ok" = 1 ]; then
                    kexec_initrd="$ROOTFS/tmp/zfs-combined-initrd.$$"
                    cat "$initrd" > "$kexec_initrd"
                    # Pad to a 4-byte boundary before appending the next
                    # (uncompressed newc) segment - a real boot showed
                    # this exact archive, round-trip-verified as correct
                    # on its own, having NO effect at all once appended.
                    # Root cause, confirmed against the kernel's own
                    # init/initramfs.c: after inflating one compressed
                    # segment, the unpack loop's own check for the START
                    # of the NEXT (uncompressed) cpio segment is gated
                    # on `!(this_header & 3)` - a 4-byte-aligned offset.
                    # $initrd (the target's own real, gzip-compressed
                    # initramfs) has no reason to be a multiple of 4
                    # bytes long - true roughly 3 times out of 4 for an
                    # arbitrary compressed size - and a plain `cat` with
                    # no padding lands the second segment's own magic
                    # bytes at a misaligned offset the kernel's own
                    # check silently rejects, keeping ONLY what the
                    # first segment already unpacked and never erroring
                    # anywhere - exactly the observed symptom (verified-
                    # correct archive, unwrapped target, no error at
                    # all). NUL padding is safe by the same loop's own
                    # logic: it skips NUL bytes one at a time while
                    # still advancing this_header, so padding here is
                    # never misread as anything else. The same reason
                    # GRUB pads between concatenated initrd images
                    # (ALIGN_UP(size, 4)) rather than relying on raw
                    # concatenation.
                    handoff_pad=$(( (4 - $(wc -c < "$kexec_initrd") % 4) % 4 ))
                    if [ "$handoff_pad" -gt 0 ]; then
                        dd if=/dev/zero bs=1 count="$handoff_pad" >> "$kexec_initrd" 2>/dev/null
                    fi
                    cat "$handoff_cpio" >> "$kexec_initrd"
                    msg "encryption key handoff cpio verified (wrapper + real ELF binary + key all round-tripped correctly) and appended to the target initrd ($handoff_pad alignment pad byte(s))"
                else
                    msg "WARNING: handoff verification failed - the round-tripped archive did not contain a genuine wrapper/real-binary/key as expected - target will prompt again"
                fi
            else
                msg "WARNING: failed to build encryption key handoff cpio - target will re-prompt for the passphrase"
            fi
            rm -f "$handoff_cpio"
        else
            msg "WARNING: could not extract usr/sbin/zfs from the target initramfs (unrecognized compression, or path not found) - target will re-prompt for the passphrase"
        fi
        rm -rf "$handoff_root"
        rm -f "$zfs_key_stage"
    fi
fi

if ! kexec -l "$kernel" --initrd="$kexec_initrd" \
    --command-line="$full_cmdline" 2>/tmp/kexec-l.log; then
    cat /tmp/kexec-l.log
    fail "kexec -l failed for $DATASET"
fi

msg "kexec -l succeeded, unmounting and exporting $POOL before the jump"

# Last Boot Diagnostics: the "attempt record" - what was actually
# attempted this kexec, independent of whether it succeeds. This is the
# only reliable anchor for binding OTHER evidence (rc.log, a future
# kernel-crash record) to THIS specific attempt rather than a stale one
# from days/reboots ago - see menu.py's own freshness check against
# this record's "time" field before trusting rc.log as belonging to the
# boot that just failed. Deliberately unconditional (not gated on
# ALPINE_ZFSBOOT_BOOTCHECK=off the way the counter below is) - "what did
# we last try to boot" is useful on its own even for a BE that has
# failure-counting disabled.
#
# Four separate org.alpinezfsboot:attempt_* properties, not one
# combined/encoded blob - never reusing org.alpinezfsboot:commandline
# (that property is an operator-editable INPUT, a persisted cmdline
# override; these are a system-written OUTPUT, a record of what was
# actually used, override already applied). Kept as plain separate
# properties deliberately - avoids inventing a delimiter/encoding
# scheme (and a new binary dependency like base64, unverified as
# actually present in this project's minimal busybox build) just to
# cram four fields into one `zfs get -H -o value` line. `zfs set` takes
# multiple property=value pairs in one call and splits each on its
# FIRST "=" only, so $full_cmdline's own "=" characters (root=ZFS=...)
# are not a problem.
#
# Wall-clock `date`, not /proc/uptime like _elapsed() above - deliberately
# different tradeoff: _elapsed() is a same-boot progress metric, always
# comparable to itself; this record's timestamp has to still mean
# something on a LATER boot, when the previous boot's own uptime counter
# no longer exists at all - RTC-only accuracy (no NTP this early) is a
# real limitation, but far more useful than no wall-clock timestamp at
# all for a human reading this later.
attempt_time="$(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo unknown)"
if ! zfs set \
    "org.alpinezfsboot:attempt_time=$attempt_time" \
    "org.alpinezfsboot:attempt_kernel=/boot/vmlinuz-$KERNEL_SUFFIX" \
    "org.alpinezfsboot:attempt_initrd=/boot/initramfs-$KERNEL_SUFFIX" \
    "org.alpinezfsboot:attempt_cmdline=$full_cmdline" \
    "$DATASET" 2>/dev/null; then
    msg "WARNING: could not record this boot attempt (org.alpinezfsboot:attempt_*) on $DATASET"
fi

# $kexec_initrd's contents are already copied into the kernel's own
# kexec-reserved memory by the time `kexec -l` returns above - no
# reason for the plaintext key inside it to keep sitting in this
# initramfs's own tmpfs an extra moment longer than that, even though
# the eventual jump/reboot would wipe it anyway.
[ "$kexec_initrd" != "$initrd" ] && rm -f "$kexec_initrd"

# Failed-boot detection ("bootcheck") - the ONLY place the per-BE
# attempt counter is ever incremented, and only AFTER kexec -l has
# already succeeded (a failed kexec -l already routes through fail(),
# which already starts rescue SSH on its own, so no separate
# accounting is needed for that path). Placed here, after the
# plaintext-key cleanup just above rather than before it - a
# synchronous, txg-syncing `zfs set` call has no business extending
# that exposure window when moving it costs nothing (the BE is still
# mounted and the pool still imported right up until the umount/zpool
# export two lines below either way).
#
# Content-derived, not just property-derived: incrementing is gated on
# whether THIS BE's own mounted filesystem actually has the target-
# side confirm service registered (the real OpenRC runlevel symlink,
# not just the ZFS property value) - a design-review-found gap this
# closes: a hand-cloned BE, a BE rolled back to a pre-service
# snapshot, or a BE that had the service removed later would otherwise
# accumulate a count it can NEVER reset, eventually refusing to
# auto-boot a perfectly healthy machine.
#
# `-L`, not just `-f`: `rc-update add` creates this runlevel entry as
# an ABSOLUTE symlink (confirmed against real OpenRC source) - `-f`
# alone DEREFERENCES it, resolving the absolute target against THIS
# initramfs's own root (where the target BE's files never exist at
# that path) rather than the mounted BE, making the check always false
# on real hardware regardless of whether the service is actually
# there. An adversarial review caught this via a test fixture that
# used a plain regular file instead of a real symlink, which is
# exactly what let it survive undetected. Also require the symlink's
# real target (the actual /etc/init.d script, checked directly under
# the BE's own mounted root rather than by dereferencing the runlevel
# symlink itself) to exist too - a DANGLING runlevel symlink (the
# script removed by hand, an interrupted apk upgrade, ...) is the same
# "no confirm service will ever actually run" case this whole check
# exists to catch, and `-L` alone is true for a dangling symlink.
#
# If the confirm service turns out to be missing, this doesn't just
# refuse to increment - it actively disarms (resets to armed:0) rather
# than leaving whatever count was already there. Refusing to increment
# alone only stops the count from getting WORSE; a BE that already
# crossed the threshold before losing its confirm service (apk
# upgrade, rc-update del, a rollback to an old snapshot) would
# otherwise stay stuck in forced-rescue on a perfectly healthy machine
# forever, with no way to self-heal short of a human clearing it by
# hand from the menu.
#
# Only a LOCAL, numeric armed:K counts - see /init's own identical
# read-and-parse logic (its own decision-point comment) for why
# "inherited" must never be treated as a real count.
if [ "${ALPINE_ZFSBOOT_BOOTCHECK:-}" != "off" ]; then
    bootcheck_raw="$(zfs get -H -o value,source org.alpinezfsboot:bootcheck "$DATASET" 2>/dev/null)"
    bootcheck_value="${bootcheck_raw%%	*}"
    bootcheck_source="${bootcheck_raw#*	}"
    bootcheck_k=""
    case "$bootcheck_value" in
        armed:*)
            bootcheck_k="${bootcheck_value#armed:}"
            case "$bootcheck_k" in
                ''|*[!0-9]*) bootcheck_k="" ;;
            esac
            ;;
    esac
    if [ -n "$bootcheck_k" ] && [ "$bootcheck_source" = "local" ]; then
        bootcheck_confirm_svc="$ROOTFS/mnt/root/etc/runlevels/default/alpine-zfsboot-bootcheck"
        bootcheck_confirm_script="$ROOTFS/mnt/root/etc/init.d/alpine-zfsboot-bootcheck"
        if { [ -L "$bootcheck_confirm_svc" ] || [ -f "$bootcheck_confirm_svc" ]; } \
           && [ -f "$bootcheck_confirm_script" ]; then
            if zfs set "org.alpinezfsboot:bootcheck=armed:$((bootcheck_k + 1))" "$DATASET" 2>/dev/null; then
                msg "bootcheck: $DATASET attempt $((bootcheck_k + 1)) recorded"
            else
                msg "WARNING: bootcheck: zfs set failed for $DATASET - attempt NOT recorded"
            fi
        else
            msg "WARNING: $DATASET is armed for bootcheck but has no alpine-zfsboot-bootcheck confirm service registered - disarming (armed:0) instead of accumulating an unresettable count"
            zfs set "org.alpinezfsboot:bootcheck=armed:0" "$DATASET" 2>/dev/null
        fi
    fi
fi

umount "$ROOTFS"/mnt/root 2>/dev/null
zpool export "$POOL" 2>/dev/null
# Last thing alpine-zfsboot itself ever prints - everything after this
# point, if anything, is the target kernel's own console output, not
# this script's. If the ~40 second gap reported on a real boot falls
# BEFORE this line, the overhead is in alpine-zfsboot's own encryption/
# mount/kexec-prep steps above; if it falls AFTER, this script has no
# way to know from here on, since it's about to stop being the running
# kernel at all - see the printk bump immediately below for why "after"
# still splits into two very different possibilities, not just "the
# target Alpine system's own boot".
#
# This kernel's own console loglevel is raised to maximum right before
# the jump because this build's own cmdline sets `quiet` (see build.sh)
# - Alpine's initramfs-init responds to that with `dmesg -n 1`, which
# suppresses everything except KERN_EMERG from ever reaching the
# console. A real, reported multi-minute black screen between this
# message and the target kernel's own first visible output could
# easily be time spent inside THIS kernel's own kexec_kernel()/
# machine_kexec() shutdown path (CPU quiescing, device shutdown
# callbacks, reboot notifiers - a slow/blocked one MAY surface as a
# "task blocked for more than N seconds" hung-task warning, though not
# necessarily (if the hang is deep enough that the watchdog thread
# itself never gets scheduled, no such warning ever fires regardless of
# loglevel - this is a debugging aid, not a guarantee it'll explain
# everything) - but `quiet` would have silently swallowed any such
# message this whole time regardless, so raising it is a strict
# improvement either way. Plain `echo 8` (the documented interface for
# just the current console loglevel - see kernel-parameters.txt) rather
# than writing all four printk levels - only the console level matters
# for this, no reason to also touch default/minimum/default-console.
echo 8 > /proc/sys/kernel/printk 2>/dev/null
msg "jumping to new kernel now"
kexec -e
# Only reachable if kexec -e itself failed to jump - -l already
# succeeded, so this is a genuinely unexpected, unrecoverable state.
fail "FATAL: kexec -e did not take over (kexec -l had already succeeded)"
