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

# boot_lock_held tracks whether THIS process is the one that actually
# won boot_lock_acquire below (not just whether boot_lock_dir happens to
# exist - it might exist because some OTHER process holds it, in which
# case lose_boot_race() below must NOT release it). Read by
# release_boot_lock's trap handler - see that function's own comment for
# why this exists at all.
boot_lock_held=0

# boot_lock_acquire / release_boot_lock - PID-tracked, stale-reclaiming,
# interrupt-safe ownership for $boot_lock_dir, the exact same quality of
# lock zfs-unlock.sh's own zfs_op_lock()/zfs_op_unlock() already provide
# for the encryption state machine (see that pair's own extensive
# comment for the full reasoning - _pid_alive-gated reclaim via an
# atomic `mv` aside, so two callers racing a reclaim can't both believe
# they won it). A full source audit found this lock had NEITHER quality
# zfs_op_lock does: no owner recorded at all (so a dead holder's
# directory could never be told apart from a live one - only a real
# reboot, wiping tmpfs, ever cleared it) and no trap (so a Ctrl-C or a
# dropped SSH session mid-boot - both explicitly supported, ordinary
# operator actions, not edge cases - bypassed fail()'s own existing
# `rmdir "$boot_lock_dir"` entirely, since a killed process runs no
# cleanup code at all without one). Either gap alone was enough to
# permanently wedge a dataset's boot for the rest of the rescue session,
# recoverable only by a human manually finding and removing the stale
# directory from a recovery shell.
#
# Deliberately two SEPARATE, independent layers, not one - a trap
# handles the common case (INT/HUP/a normal exit) but cannot fire on
# SIGKILL/an OOM kill at all; stale-PID reclaim handles exactly that
# case on the NEXT attempt instead. Neither one alone is sufficient.
boot_lock_acquire() {
    if mkdir "$boot_lock_dir" 2>/dev/null; then
        echo "$$" > "$boot_lock_dir/pid" 2>/dev/null
        boot_lock_held=1
        return 0
    fi
    lock_pid="$(cat "$boot_lock_dir/pid" 2>/dev/null)"
    # F11 (unidoc-alip's PR #5 follow-up review) - same real gap as
    # zfs_op_lock's own identical fix (zfs-unlock.sh, see its own
    # comment for the full reasoning): mkdir above and the pid write
    # below it are not atomic together, so a holder killed in that
    # exact gap leaves a lock directory with no pid file - permanently
    # wedged, since lock_pid then reads empty and the dead-pid check
    # below never runs at all. A brief, bounded poll first: the pid
    # write is a single near-instant tmpfs write, so if it still hasn't
    # appeared after several short checks, this is genuinely abandoned.
    if [ -z "$lock_pid" ]; then
        _i=0
        while [ -z "$lock_pid" ] && [ "$_i" -lt 5 ]; do
            sleep 0.05
            lock_pid="$(cat "$boot_lock_dir/pid" 2>/dev/null)"
            _i=$((_i + 1))
        done
        if [ -z "$lock_pid" ]; then
            stale="$boot_lock_dir.stale.$$"
            if mv "$boot_lock_dir" "$stale" 2>/dev/null; then
                moved_pid="$(cat "$stale/pid" 2>/dev/null)"
                if [ -n "$moved_pid" ]; then
                    if [ ! -e "$boot_lock_dir" ]; then
                        mv "$stale" "$boot_lock_dir" 2>/dev/null
                    else
                        rm -rf "$stale"
                    fi
                    return 1
                fi
                rm -rf "$stale"
                if mkdir "$boot_lock_dir" 2>/dev/null; then
                    echo "$$" > "$boot_lock_dir/pid" 2>/dev/null
                    boot_lock_held=1
                    msg "reclaimed an abandoned boot lock for $DATASET (no owner pid was ever recorded - a previous holder was likely killed between acquiring the lock and recording ownership)"
                    return 0
                fi
            fi
        fi
        return 1
    fi
    # command -v guard: same fail-closed reasoning as zfs_op_lock's own
    # identical guard - a missing _pid_alive must never be silently
    # treated as "the holder is dead" (that would make bypassing this
    # entire lock as simple as not sourcing pid-alive.sh).
    if [ -n "$lock_pid" ] && command -v _pid_alive >/dev/null 2>&1 && ! _pid_alive "$lock_pid"; then
        stale="$boot_lock_dir.stale.$$"
        if mv "$boot_lock_dir" "$stale" 2>/dev/null; then
            # F11 (unidoc-alip's PR #5 review) - same real race as
            # zfs_op_lock's own identical reclaim shape (zfs-unlock.sh):
            # the staleness check above and this mv are not atomic
            # together, so another session that ALSO judged $lock_pid
            # dead could already have finished its own full reclaim
            # (mv+rm+mkdir+its own live pid) before this mv runs - which
            # would then move THAT session's fresh, live lock aside
            # instead of the dead one this attempt actually judged, and
            # both sessions end up believing they hold it (the exact
            # double-kexec scenario this lock exists to prevent).
            # Re-reading the pid actually captured in the moved-aside
            # directory and comparing it against the dead pid this
            # attempt judged closes that window: a mismatch means
            # someone else already won a real reclaim, so put their
            # lock back untouched and fail this attempt instead of
            # stealing it.
            moved_pid="$(cat "$stale/pid" 2>/dev/null)"
            if [ "$moved_pid" != "$lock_pid" ]; then
                # N4 (unidoc-alip's PR #5 follow-up review) - same real
                # race as zfs_op_lock's own identical fix (zfs-unlock.sh,
                # see its own comment for the full reasoning): a THIRD
                # process can `mkdir "$boot_lock_dir"` (its own plain
                # fast path) in the gap between this process's mv-aside
                # and this mv-back. `mv src dst` onto an EXISTING
                # directory nests src inside it rather than failing or
                # replacing it - moving back onto a since-recreated
                # $boot_lock_dir would permanently wedge that third
                # party's own lock (their later release_boot_lock()'s
                # `rmdir` fails on a non-empty directory forever after,
                # recoverable only by a reboot). Checking existence
                # immediately before the mv-back closes that
                # deterministic, unrecoverable outcome.
                if [ ! -e "$boot_lock_dir" ]; then
                    mv "$stale" "$boot_lock_dir" 2>/dev/null
                else
                    rm -rf "$stale"
                fi
                return 1
            fi
            rm -rf "$stale"
            if mkdir "$boot_lock_dir" 2>/dev/null; then
                echo "$$" > "$boot_lock_dir/pid" 2>/dev/null
                boot_lock_held=1
                msg "reclaimed a stale boot lock for $DATASET (holder pid $lock_pid is gone)"
                return 0
            fi
        fi
    fi
    return 1
}

# release_boot_lock - idempotent (checks boot_lock_held first) and safe
# to call from a path that never acquired the lock at all (the common
# case - most exits reach here via lose_boot_race(), which must never
# release a lock this process does not own). A real, successful
# kexec -e never reaches this at all - it replaces the running kernel
# outright, taking this whole tmpfs (and the lock file living in it)
# with it, so there is nothing to leak on that path either way.
release_boot_lock() {
    [ "$boot_lock_held" = 1 ] || return 0
    rm -f "$boot_lock_dir/pid" 2>/dev/null
    rmdir "$boot_lock_dir" 2>/dev/null
    boot_lock_held=0
}

# _cleanup_secrets - every scratch file in this tmpfs that ever holds a
# copy of the encryption passphrase in plaintext, removed
# unconditionally: the kexec handoff's own combined-initrd (which
# embeds the key inside its own appended cpio segment - see that
# block's own header comment further down), the handoff block's own
# two intermediate copies ($handoff_root, containing
# .../run/alpine-zfsboot/zfs-key, and $handoff_cpio, the packed
# archive built FROM it - both exist only transiently while the
# handoff block runs, normally rm'd by its own end, but not yet if a
# signal interrupts partway through), and (staged_by_me only - see
# below) the staged secret itself ($zfs_key_stage_path). ${var:-}
# throughout, deliberately - this runs from contexts reached long
# before kexec_initrd/encryptionroot/handoff_root/handoff_cpio are ever
# assigned (the encryption-key-load failure, the mount failure, a
# signal arriving before the handoff block is even entered), all under
# `set -u`. Shared by fail() (an explicit, ordinary failure) AND the
# EXIT/INT/TERM/HUP trap below (a signal - Ctrl-C, a dropped SSH
# session - arriving at some arbitrary point, not just at one of the
# specific spots fail() itself is called from) - a full source audit
# found the trap previously released ONLY the boot lock, leaving every
# one of these plaintext copies sitting in tmpfs indefinitely if a
# signal happened to land between staging a secret and this file's own
# normal end-of-block cleanup.
#
# staged_by_me gate (F2, unidoc-alip's PR #5 review): the staged-secret
# path ($zfs_key_stage_path) is keyed per encryptionroot, not per
# process - every session unlocking the SAME dataset shares it. This
# used to remove that path unconditionally, on ANY exit, including
# lose_boot_race()'s own `exit 0` - a real, confirmed regression: a
# session that loses the boot race (arrives after another session
# already staged a secret and won boot_lock_acquire) deleted the
# WINNER's staged key on its own way out, without ever holding
# zfs_op_lock, leaving the winner's own kexec handoff carrying no key -
# exactly the double-ZFS-prompt bug this project's kexec handoff fix
# exists to remove, reintroduced by this cleanup path. staged_by_me is
# a plain global (no `local` in this codebase's shell, same convention
# every other cross-function variable here already uses), set ONLY
# inside zfs_stage_secret() itself (zfs-unlock.sh), immediately before
# the rename that actually publishes the secret - not by either of
# zfs_unlock()'s own two call sites after the whole call has already
# returned (N3, unidoc-alip's PR #5 follow-up review closed a real
# signal race here: `mv` is an external command, and a signal landing
# while the shell merely waits on it could leave the rename genuinely
# complete with the flag not yet set under the old shape) - so this
# process only ever removes a stage it actually created itself, never
# one it merely found already staged (zfs_unlock()'s own "already
# unlocked and staged... discarding this passphrase" early-return path
# never sets it) or one another session
# is using.
_cleanup_secrets() {
    if [ -n "${kexec_initrd:-}" ] && [ "${kexec_initrd:-}" != "${initrd:-}" ]; then
        rm -f "$kexec_initrd" 2>/dev/null
    fi
    if [ "${staged_by_me:-0}" = 1 ] && [ -n "${encryptionroot:-}" ] && [ "${encryptionroot:-}" != "-" ]; then
        rm -f "$(zfs_key_stage_path "$encryptionroot")" 2>/dev/null
    fi
    [ -n "${handoff_root:-}" ] && rm -rf "$handoff_root" 2>/dev/null
    [ -n "${handoff_cpio:-}" ] && rm -f "$handoff_cpio" 2>/dev/null
    # zfs-unlock.sh's own two plaintext-passphrase mktemp files
    # ($dialog_err_file, $prompt_out) - see _zfs_unlock_cleanup_tempfiles's
    # own comment for why this file calls it directly rather than
    # zfs-unlock.sh registering a competing trap of its own.
    # command -v guard: zfs-unlock.sh is always sourced before this
    # trap could ever actually fire on a real run (see this script's own
    # top-level sourcing order), but defined defensively anyway, same
    # reasoning as every other command -v guard in this project.
    command -v _zfs_unlock_cleanup_tempfiles >/dev/null 2>&1 && _zfs_unlock_cleanup_tempfiles
    # F3 (unidoc-alip's PR #5 follow-up review): this file calls
    # zfs_unlock()/zfs_lock() in-process (see this script's own header
    # comment), so a signal landing anywhere inside one of those calls'
    # own body - after zfs_op_lock() succeeded but before that same call
    # reached its own zfs_op_unlock() - used to leave the per-
    # encryptionroot lock held with nothing in THIS file's trap to ever
    # release it, same gap as the standalone zfs-unlock wrapper (see
    # zfs-unlock.sh's own comment on _zfs_unlock_release_held_lock for
    # the full reasoning). Distinct from release_boot_lock below, which
    # is this file's own separate whole-boot lock, not the per-
    # encryptionroot one.
    command -v _zfs_unlock_release_held_lock >/dev/null 2>&1 && _zfs_unlock_release_held_lock
}

# _on_exit_cleanup - the actual EXIT trap target: removes every
# plaintext secret AND releases the boot lock exactly like a normal
# fail()/lose_boot_race() exit would.
_on_exit_cleanup() {
    _cleanup_secrets
    release_boot_lock
}
trap _on_exit_cleanup EXIT

# INT/TERM/HUP handling (F3, unidoc-alip's PR #5 review): a single
# `trap _on_exit_cleanup EXIT INT TERM HUP` (this file's own previous
# shape) is a real, confirmed regression, not a hardening no-op. POSIX
# sh signal traps do not terminate the script on their own - a
# handler that doesn't itself exit lets the script RESUME right after
# the trap runs. So a signal here used to release the boot lock AND
# delete the staged secret, then carry on straight through to
# `kexec -e` with NEITHER held - defeating the entire point of both:
# a second frontend can take the now-released lock and start its own
# boot while the first is still heading for its own kexec (the exact
# double-kexec the lock exists to prevent), and if the signal lands
# before the handoff initrd is built, the target re-prompts anyway.
# Realistic triggers, not edge cases: a dropped rescue-SSH session
# (SIGHUP) or an operator's `kill <pid>`/Ctrl-C while this script
# waits in a long `kexec -l` on a real, tens-of-MB initrd.
#
# This process is always PID 1 on a real boot (exec'd straight from
# /init or from menu.py's own os.execv - see this file's own top
# comment) - and PID 1 must never actually exit on a signal: the
# kernel panics ("Attempted to kill init") the instant it does,
# whether via this trap's own explicit exit or via the signal's
# ordinary default disposition (this file had NO trap at all before
# this whole hardening pass - meaning a signal used to terminate the
# process outright via that default disposition, which is exactly as
# unsafe for PID 1 as this bug's own resume-without-cleanup behavior,
# just a different failure mode). So PID 1 explicitly ignores these
# three signals - the one behavior that is actually safe for it -
# while a non-PID-1 invocation (this project's own test harness,
# tests/run-tests.sh, does not exec this file as PID 1) gets a real
# handler: clean up, disarm the EXIT trap (so cleanup doesn't run
# TWICE - once here, once again from this same handler's own `exit`),
# and terminate with the conventional 128+signum exit code instead of
# resuming.
if [ "$$" -eq 1 ]; then
    trap '' INT TERM HUP
else
    trap '_on_exit_cleanup; trap - EXIT; exit 129' HUP
    trap '_on_exit_cleanup; trap - EXIT; exit 130' INT
    trap '_on_exit_cleanup; trap - EXIT; exit 143' TERM
fi

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
# alpine-zfsboot's own rescue build use" (build.sh's own initramfs.img
# is gzip now - see that file's own "-C gzip, NOT xz" comment) - this
# function decompresses a TARGET system's separate initramfs-
# $KERNEL_SUFFIX, built by that system's own, independent mkinitfs run,
# which may have chosen differently (a real, external Alpine install
# this project does not control the build of). gzip confirmed against
# a real Alpine install this project targets; xz supported on the same
# basis this project's own build already depends on the xz binary
# existing at all (see build.sh) - both decompress through tools
# bundled into THIS (rescue) initramfs (gzip via busybox's own applet,
# xz explicitly listed in build.sh's alpine-zfsboot.files). Anything
# else (zstd, lz4, ...) falls through to a plain `cat` - correct only
# for an already-uncompressed cpio image, and caught as a real failure
# by the handoff block's own -s/exit-code checks further down rather
# than silently producing garbage.
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
    # Deterministic cleanup of every plaintext-secret scratch file this
    # script can leave in tmpfs - see _cleanup_secrets' own comment (up
    # near release_boot_lock, which this function is shared with) for
    # the full reasoning and exactly what it removes.
    _cleanup_secrets
    # Release the single-owner boot lock (see boot_lock_acquire's own
    # comment, up near where boot_lock_dir is computed) on every failure
    # path, unconditionally - release_boot_lock is a safe no-op if this
    # particular fail() was reached before the lock was ever acquired
    # (several paths above the lock-acquisition point call fail() too -
    # the encryption-key-load failure, for one). Without this, a human
    # who reaches this recovery shell and manually re-runs
    # boot-dataset.sh for the same DATASET would be incorrectly told
    # "another session is already proceeding" by their own, already-
    # abandoned previous attempt's stale lock. Also already registered
    # as this script's own INT/TERM/HUP/EXIT trap - calling it here
    # explicitly too is redundant with that (idempotent either way) but
    # keeps this path's own intent locally obvious.
    release_boot_lock
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
    # F2 (unidoc-alip's PR #5 follow-up review): staged_by_me, if this
    # process is the one that staged the handoff secret earlier in its
    # own run, is cleared to 0 HERE, before the EXIT trap below (via
    # _cleanup_secrets()) can act on it. "Losing the race" means, by
    # definition, some OTHER session is the one actually proceeding
    # with THIS dataset's boot right now - and since the staged secret
    # is published at a path keyed by encryptionroot, not by process,
    # that other session may well be relying on the EXACT stage this
    # process itself created a moment ago (e.g. this session unlocked
    # and staged it, then lost boot_lock_acquire to a session that
    # arrived first and was already mid-boot). staged_by_me alone (N3)
    # only ever protected a DIFFERENT process's stage from being
    # deleted by a losing session that never staged anything itself -
    # it does nothing for the case where the losing session IS the one
    # that staged it. Never deleting a self-staged secret on the
    # specific "stepping back, someone else has this" path is a strictly
    # safer failure mode than deleting a secret a still-booting session
    # needs - worst case here is a secret sitting in tmpfs a little
    # longer than the single-owner-at-a-time norm, not a re-prompt.
    staged_by_me=0
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
if ! boot_lock_acquire; then
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
            # F9 (unidoc-alip's PR #5 review): this cp's own exit status
            # used to go unchecked entirely - if $zfs_key_stage vanished
            # between being staged and here (F2/F3's own race window,
            # now closed, but a transient tmpfs I/O error is still a
            # real possibility independent of those) or the copy itself
            # failed partway, this block used to carry on regardless,
            # packing a missing-or-truncated key into the handoff
            # archive. [ -s ... ] right after cp (not just checking cp's
            # own exit status) also catches a cp that "succeeded" but
            # produced a real, on-disk EMPTY file (some cp
            # implementations can do this on specific I/O error shapes
            # without a nonzero exit) - cheap, immediate, before ever
            # building the cpio archive from it.
            cp "$zfs_key_stage" "$handoff_root/run/alpine-zfsboot/zfs-key"
            cp_status=$?
            chmod 400 "$handoff_root/run/alpine-zfsboot/zfs-key" 2>/dev/null
            handoff_cpio="$ROOTFS/tmp/zfs-handoff.$$.cpio"
            if [ "$cp_status" = 0 ] && [ -s "$handoff_root/run/alpine-zfsboot/zfs-key" ] \
               && ( cd "$handoff_root" && find . | cpio -o -H newc 2>/dev/null > "$handoff_cpio" ) \
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
                if [ ! -s "$handoff_check/usr/sbin/zfs" ] || [ -L "$handoff_check/usr/sbin/zfs" ] \
                   || [ ! -x "$handoff_check/usr/sbin/zfs" ]; then
                    handoff_ok=0
                fi
                if [ ! -s "$handoff_check/usr/sbin/zfs.alpine-zfsboot-real" ] \
                   || [ -L "$handoff_check/usr/sbin/zfs.alpine-zfsboot-real" ] \
                   || [ ! -x "$handoff_check/usr/sbin/zfs.alpine-zfsboot-real" ]; then
                    handoff_ok=0
                elif [ "$(od -An -tx1 -N4 "$handoff_check/usr/sbin/zfs.alpine-zfsboot-real" 2>/dev/null | tr -d ' \n')" != "7f454c46" ]; then
                    handoff_ok=0
                fi
                # Byte-for-byte, not just `-f`/`-s` - a full source audit
                # found this previously only checked the key file EXISTS
                # (`[ -f ... ]`), which a truncated or entirely EMPTY
                # extracted file would also satisfy: a `cp` interrupted
                # partway (signal, tmpfs I/O error) leaves a real,
                # non-empty-looking regular file at the source path (the
                # cpio pack/unpack round-trip above would faithfully carry
                # that same truncation through, `-f` true throughout), and
                # the target would then receive a wrong/incomplete secret
                # with the wrapper's own diagnostic misleadingly logging it
                # as a rejected passphrase rather than the real cause. `od`
                # (piped through `tr`, exactly the SAME two commands the
                # ELF-magic check right above this already uses, just over
                # the whole file instead of the first 4 bytes) against
                # $zfs_key_stage - still readable here, never consumed by
                # anything above - proves the ENTIRE pipeline (stage -> cp
                # -> cpio -o -> cpio -i) reproduced the exact secret,
                # catching empty, truncated, AND any other corruption in
                # one check, not just the empty case. Deliberately NOT
                # `cmp` - this project was bitten once already by assuming
                # a plausible-sounding coreutils tool exists inside its own
                # minimal rescue initramfs (`dirname` isn't a busybox
                # applet, confirmed missing the hard way) - `cmp` is
                # neither declared in this project's own alpine-zfsboot.files
                # manifest nor used anywhere else in this boot path, so its
                # presence here would be an unverified assumption, not a
                # confirmed fact; `od`/`tr` are both already proven present
                # by the very next check up. The staged secret is always
                # short (an interactively-typed passphrase), so a full hex
                # dump comparison costs nothing.
                # [ -s ... ] on BOTH files first, restored (F9,
                # unidoc-alip's PR #5 review) - the byte-for-byte od
                # comparison alone has a real blind spot: if EITHER
                # file is missing or unreadable, `od` produces no
                # output (stderr already discarded), `tr` on empty
                # input is still empty, and empty-string equals
                # empty-string - so a staged key that vanished (a
                # signal landing in this exact window, before this
                # file's own F2/F3 trap fixes existed to prevent it
                # racing another session's own stage; a transient read
                # error either way) or an extraction that never
                # actually produced the file made this check report
                # "match" with NO KEY PRESENT AT ALL, and the log line
                # below said "verified ... round-tripped correctly"
                # for a kexec that would carry no key whatsoever.
                if [ ! -s "$zfs_key_stage" ] \
                   || [ ! -s "$handoff_check/run/alpine-zfsboot/zfs-key" ]; then
                    handoff_ok=0
                elif [ "$(od -An -tx1 "$zfs_key_stage" 2>/dev/null | tr -d ' \n')" \
                     != "$(od -An -tx1 "$handoff_check/run/alpine-zfsboot/zfs-key" 2>/dev/null | tr -d ' \n')" ]; then
                    handoff_ok=0
                fi
                rm -rf "$handoff_check"
                if [ "$handoff_ok" = 1 ]; then
                    kexec_initrd="$ROOTFS/tmp/zfs-combined-initrd.$$"
                    # Every write below is now checked - a full source
                    # audit found none of them were: `cat`/`dd`/the
                    # final append `cat` all ran unconditionally, with
                    # no exit-status check at all. This file creates a
                    # full SECOND copy of $initrd in the SAME tmpfs that
                    # already holds the original - a real, plausible
                    # ENOSPC case, not a hypothetical one - and a
                    # truncated write used to proceed all the way to
                    # `kexec -l`/`kexec -e` regardless, handing the
                    # target kernel a truncated gzip stream/cpio segment
                    # to panic on instead of the documented graceful
                    # "just re-prompts" degradation every OTHER handoff
                    # failure in this block already gets. build_ok
                    # tracks this separately from handoff_ok - the
                    # round-trip verification above already proved the
                    # SOURCE material (the small handoff cpio) is
                    # correct; this tracks whether COMBINING it with the
                    # (potentially large) real initrd actually succeeded.
                    build_ok=1
                    if ! cat "$initrd" > "$kexec_initrd"; then
                        build_ok=0
                    fi
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
                    if [ "$build_ok" = 1 ]; then
                        handoff_pad=$(( (4 - $(wc -c < "$kexec_initrd") % 4) % 4 ))
                        if [ "$handoff_pad" -gt 0 ] && ! dd if=/dev/zero bs=1 count="$handoff_pad" >> "$kexec_initrd" 2>/dev/null; then
                            build_ok=0
                        fi
                    fi
                    if [ "$build_ok" = 1 ] && ! cat "$handoff_cpio" >> "$kexec_initrd"; then
                        build_ok=0
                    fi
                    if [ "$build_ok" = 1 ]; then
                        msg "encryption key handoff cpio verified (wrapper + real ELF binary + key all round-tripped correctly) and appended to the target initrd (${handoff_pad:-0} alignment pad byte(s))"
                    else
                        rm -f "$kexec_initrd"
                        kexec_initrd="$initrd"
                        msg "WARNING: failed to write the combined handoff initrd (disk/tmpfs full?) - falling back to the plain target initrd, target will prompt again"
                    fi
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

# Real-hardware-found (a QEMU/KVM aarch64 "virt" board, 2026-09-25):
# QEMU injects a random, nonzero /chosen/kaslr-seed into the device
# tree it hands the kernel. A kernel built WITH CONFIG_RANDOMIZE_BASE
# reads and zeroes that value once at boot; Alpine's own aarch64
# kernel builds do not set CONFIG_RANDOMIZE_BASE, so it's never
# consumed - kexec-tools' arm64 backend (setup_2nd_dtb()) then treats
# the still-nonzero seed in the CURRENT kernel's own live device tree
# (/sys/firmware/fdt, what kexec -l reads by default) as unsafe to
# reuse and silently refuses ("kexec: setup_2nd_dtb failed.", no
# further detail without kexec's own -d). fix-kexec-dtb.py's own
# top-of-file comment has the full root-cause writeup and this
# workaround's own safety contract - in short: on any platform/kernel
# combination that does NOT have this problem (x86_64, real hardware
# where CONFIG_RANDOMIZE_BASE IS set, a kaslr-seed that's already
# zero, or literally anything unexpected about the live device tree),
# this prints nothing and kexec_dtb_arg stays empty - IDENTICAL
# behavior to before this fix existed. It only ever changes anything
# on the exact combination that was otherwise a hard, unrecoverable
# boot failure.
# STUB_FIX_KEXEC_DTB_SCRIPT: tests only, same convention as
# STUB_ZFS_UNLOCK_SH/STUB_ROOT elsewhere in this file - the real path
# on a booted rescue system is always /fix-kexec-dtb.py (build.sh's
# own copy step), never overridden there.
kexec_dtb_arg=""
kexec_dtb_fixed="$(python3 "${STUB_FIX_KEXEC_DTB_SCRIPT:-/fix-kexec-dtb.py}" /tmp/kexec-fixed.dtb 2>/tmp/kexec-dtb-fix.log)" || true
if [ -n "$kexec_dtb_fixed" ] && [ -r "$kexec_dtb_fixed" ]; then
    msg "worked around a nonzero /chosen/kaslr-seed in the live device tree (see /tmp/kexec-dtb-fix.log) - passing --dtb=$kexec_dtb_fixed to kexec"
    kexec_dtb_arg="--dtb=$kexec_dtb_fixed"
fi

if ! kexec -l "$kernel" --initrd="$kexec_initrd" \
    ${kexec_dtb_arg:+"$kexec_dtb_arg"} \
    --command-line="$full_cmdline" 2>/tmp/kexec-l.log; then
    cat /tmp/kexec-l.log
    fail "kexec -l failed for $DATASET"
fi

msg "kexec -l succeeded, unmounting and exporting $POOL before the jump"

# N2 (unidoc-alip's PR #5 follow-up review): this BE's own confirm-
# service files MUST be read before the umount two lines below, not
# after - a real, shipped regression the F21 pre-check above introduced
# by moving the umount earlier in this file without moving every read
# of $ROOTFS/mnt/root along with it. The bootcheck block further down
# used to run before this umount existed at all; once it started
# running AFTER an umount of the very path it reads
# ($ROOTFS/mnt/root/etc/runlevels/.../etc/init.d/...), both file tests
# became permanently false - not "sometimes wrong", every single armed
# BE on every single boot silently disarmed itself
# (org.alpinezfsboot:bootcheck reset to armed:0) instead of ever
# incrementing, making the whole failed-boot-counter/forced-rescue
# mechanism permanently inert. Confirmed the hard way (per the review):
# a harness whose `umount` stub actually hides the mounted content
# showed armed:3 before this fix's own predecessor moved the umount up,
# armed:0/"disarming" after. Computed here, while the mount still
# exists, and used (not recomputed) inside that later block.
bootcheck_confirm_svc="$ROOTFS/mnt/root/etc/runlevels/default/alpine-zfsboot-bootcheck"
bootcheck_confirm_script="$ROOTFS/mnt/root/etc/init.d/alpine-zfsboot-bootcheck"
if { [ -L "$bootcheck_confirm_svc" ] || [ -f "$bootcheck_confirm_svc" ]; } \
   && [ -f "$bootcheck_confirm_script" ]; then
    bootcheck_confirm_present=1
else
    bootcheck_confirm_present=0
fi

umount "$ROOTFS"/mnt/root 2>/dev/null

# F21 (unidoc-alip's PR #5 review): a read-only busy pre-check, BEFORE
# the attempt record/bootcheck counter below - not a full replacement
# for the real `zpool export` check further down (that one stays,
# unchanged, as the authoritative final gate right before the jump -
# see its own comment), but a real gap this closes on its own: the
# export hard-fail used to run strictly AFTER the attempt record was
# already written and the bootcheck counter already bumped, so a
# refused export (something else still has a dataset under $POOL
# mounted - another boot environment, a rescue-SSH chroot_be() session
# left open) counted as a real boot attempt even though the actual
# jump (kexec -e) never happened at all - the very next Last Boot
# Diagnostics screen would then read "Previous boot: FAILED" for a
# kexec that was never even tried. The real, authoritative zpool
# export call can't simply move earlier instead - it re-derives which
# properties this record needs by unmounting/exporting $POOL itself,
# and once export succeeds this pool is gone from this rescue kernel's
# own view entirely, with no way to still write the org.alpinezfsboot:
# attempt_*/bootcheck properties on it afterward - the record and
# export are unavoidably coupled to happen on the SAME still-imported
# pool, in that order. A cheap, read-only /proc/mounts scan for any
# OTHER dataset under $POOL already mounted (this script's own
# $ROOTFS/mnt/root was just unmounted above, so it no longer shows up
# here) catches the exact realistic scenarios named above before ever
# touching the attempt record - not a full guarantee (a genuinely new
# busy condition appearing in the brief window between this check and
# the real export further down would still slip through, same as
# today), but real, meaningful coverage for the common case.
# STUB_PROC_MOUNTS: same real-vs-test-harness override convention as
# STUB_ROOT/STUB_TTY elsewhere in this project (/proc/mounts is a real
# kernel-provided file, not something under $ROOTFS a test could point
# elsewhere the way every OTHER path in this script already can).
other_pool_mount="$(awk -v pool="$POOL" '$1 == pool || index($1, pool "/") == 1 { print $1 " at " $2; exit }' "${STUB_PROC_MOUNTS:-/proc/mounts}" 2>/dev/null)"
if [ -n "$other_pool_mount" ]; then
    fail "refusing to proceed toward the kexec jump for $DATASET - $POOL still has something else mounted ($other_pool_mount), which would make the real zpool export below fail anyway; find out what's still using this pool (another mounted boot environment? a rescue-SSH chroot session?) before retrying"
fi

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
        # bootcheck_confirm_present was computed BEFORE the umount
        # above, while $ROOTFS/mnt/root still pointed at this BE's real
        # mounted filesystem (N2, unidoc-alip's PR #5 follow-up review)
        # - re-testing bootcheck_confirm_svc/bootcheck_confirm_script
        # here, now, would test paths under an already-unmounted
        # directory and always read as absent.
        if [ "$bootcheck_confirm_present" = 1 ]; then
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

# $ROOTFS/mnt/root is already unmounted (moved earlier, right after
# kexec -l - see the F21 busy pre-check's own comment above for why:
# the attempt record needs an accurate "is anything else mounted under
# $POOL" read, which needs this script's own mount out of the way
# first). No umount call here anymore - a second one would just be a
# harmless no-op.
#
# Checked, not swallowed like every OTHER export/umount call in this
# file (fail()'s own best-effort umount, the handoff block's own
# cleanup) - a full source audit found this one previously silently
# ignored via `2>/dev/null` with no exit-status check at all, proceeding
# straight to the irreversible kexec -e jump regardless of whether the
# pool was actually exported cleanly. `kexec -l` has already succeeded
# by this point (the new kernel is staged in memory), but the jump
# itself (`kexec -e`, a few lines down) has NOT happened yet - stopping
# here via fail() is still fully safe (the staged image just sits
# unused) and gives an operator a real chance to find out WHY, rather
# than jumping blind. `zpool export` (with no -f here) already refuses
# outright if any dataset under $POOL is still mounted/busy - a real
# gap: something else in this pool still mounted (another boot
# environment, a rescue-SSH chroot_be() session left open) would
# otherwise be torn out from under a live user with the ONLY visible
# symptom being an unexplained reboot, and the freshly-kexec'd target
# kernel would then re-import a pool that was never cleanly exported
# for no reason the operator was ever told about.
if ! zpool export "$POOL" 2>/tmp/zpool-export.log; then
    cat /tmp/zpool-export.log
    fail "failed to export $POOL before the kexec jump for $DATASET - kexec -l already succeeded (the new kernel is staged, not yet triggered) but jumping with $POOL not cleanly exported risks a confusing re-import on the far side; find out what's still using this pool (another mounted boot environment? a rescue-SSH chroot session?) before retrying"
fi
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
