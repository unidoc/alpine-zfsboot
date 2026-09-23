#!/bin/sh
# zfs-unlock.sh - the ONE implementation of "acquire a ZFS native-
# encryption passphrase interactively, and stage it for the kexec
# handoff" and "lock it back up again" - sourced by boot-dataset.sh
# directly, and driven by the standalone `zfs-unlock` executable (see
# that file) for menu.py's own "Unlock encrypted root"/"Lock encrypted
# root" menu actions.
#
# Why this exists as a THIRD file rather than just living inline in
# boot-dataset.sh (where the original version of this logic was
# written first): a real, reported architecture gap - menu.py's own
# ensure_key_loaded() called a bare `zfs load-key` directly, which
# unlocks the dataset (keystatus becomes "available") but never stages
# a handoff secret, and boot-dataset.sh's own kexec handoff only ever
# reads that staged secret, never keystatus itself. A real, valid
# operator workflow - SSH in, unlock while inspecting/chrooting, THEN
# choose Boot - could reach boot-dataset.sh with the dataset already
# unlocked but nothing staged, silently losing the single-passphrase
# boot property and making the TARGET initramfs prompt a second time.
# Two independently-written copies of the same secret-handling logic
# (Python's subprocess-based one, this file's shell-based one) is
# exactly the kind of drift that gap came from - one real
# implementation, called from both languages, is the fix.
#
# The three-state model this whole file (and menu.py's own
# encryption_status()) treats as authoritative:
#   LOCKED                    - keystatus unavailable, no staged secret
#   UNLOCKED, handoff READY   - keystatus available, a staged secret
#                                 exists and boot-dataset.sh's own kexec
#                                 handoff will use it - no second prompt
#   UNLOCKED, handoff NOT READY - keystatus available, but nothing is
#                                 staged (e.g. a bare `zfs load-key` run
#                                 by hand over the rescue shell) - a
#                                 real, legal state, but boot will have
#                                 to re-prompt after kexec unless this
#                                 is fixed via zfs_unlock() again first.
#
# Sourced, not exec'd, for the same reason net-config.sh/rescue-ssh.sh
# are (see rescue-ssh.sh's own header comment) - one shell, one set of
# function definitions, this project's test-harness stubbing (mount/
# zfs/dialog/...) sees calls made from in here exactly the same as
# calls made directly from boot-dataset.sh's own top-level code.
#
# MULTIPLE SIMULTANEOUS FRONTENDS ARE A SUPPORTED, REAL STATE, not an
# edge case - confirmed on real Hetzner CAX hardware: the local console
# (menu.py on /dev/tty0, itself possibly running boot-dataset.sh's own
# automatic-boot unlock prompt) and any number of rescue SSH sessions
# (menu.py on /dev/pts/N) are all independent frontends to the SAME
# per-encryptionroot ZFS state - none of them "owns" it. The real bug
# this file's design was rebuilt around: the per-encryptionroot
# operation lock (zfs_op_lock() below) originally spanned an ENTIRE
# zfs_unlock() call, including the interactive passphrase prompt
# itself. On real hardware this meant a LOCAL operator sitting at a
# passwordbox - not typing, just present - held that lock indefinitely,
# and a remote rescue operator connecting over SSH got an immediate,
# correct-LOOKING but functionally wrong "another encryption operation
# is already in progress" and could never unlock the machine remotely
# until the local dialog was dismissed one way or another. The lock was
# guarding the wrong thing: a human's think time, not the actual shared
# ZFS state transition.
#
# The fix, and the invariant every unlock path in this file now
# follows: a passphrase is captured from whichever terminal is prompting
# WITHOUT holding the operation lock at all. Only once a passphrase (or
# a verified non-interactive key) is in hand does the lock get acquired
# - and the very first thing done under it is to RE-READ the current
# state, because everything checked before the prompt can be stale by
# the time a human finishes typing: another terminal's own unlock or
# lock may have completed in the meantime. Every decision from that
# point on - a real load-key, a verify-only load-key -n, or discarding
# the passphrase entirely because someone else already finished - is
# made from that freshly re-read, lock-held state, never from what was
# true before the prompt. First successful transition wins; every other
# terminal's own in-flight attempt re-checks state under the lock and
# discards its own input cleanly rather than corrupting anything.
#
# zfs_lock() does NOT need this same split - it has no human input
# phase of its own (see its own header comment) - so it remains one
# single serialized transaction, lock held start to finish.
set -u
PATH=/sbin:/bin:/usr/sbin:/usr/bin
export PATH

ROOTFS="${STUB_ROOT:-}"

# The terminal device _zfs_prompt_once() saves/restores echo state on
# and flushes stray input from - a real, hardcoded /dev/tty at real
# boot, same STUB_ROOT-style override for the test harness (which,
# documented elsewhere in this project - see run-tests.sh's own note
# above its recovery-shell tests - has no controlling terminal of its
# own at all, so a bare /dev/tty would never resolve there; tests point
# this at any openable regular file instead, which is all the STUBBED
# `stty`/tcflush calls actually need to exercise the surrounding retry/
# staging logic, deliberately independent of whether a REAL tty is
# reachable in this harness).
ZFS_TTY="${STUB_TTY:-/dev/tty}"

# Own, distinctly-prefixed logger - NOT named `msg`, deliberately (see
# net-config.sh's own identical `net_config_msg` for the same reasoning):
# boot-dataset.sh already defines its own `msg` (elapsed-seconds
# prefixed) before sourcing this file, and shell function definitions
# are global, not scoped to whichever file they came from - naming this
# one `msg` would silently REPLACE boot-dataset.sh's own definition for
# the rest of that script's run too, not just calls made from in here.
zfs_unlock_msg() {
    echo "alpine-zfsboot-zfs-unlock: $*" > /dev/kmsg 2>/dev/null
    echo "alpine-zfsboot-zfs-unlock: $*"
}

# zfs_key_stage_path ENCRYPTIONROOT - the one place this path is ever
# computed, used identically by staging, cleanup (fail(), lock), and
# readiness checks (handoff_ready(), menu.py's own mirror of this same
# flattening rule) - previously duplicated inline in three separate
# spots in boot-dataset.sh, a real risk of the flattening rule quietly
# drifting between them.
zfs_key_stage_path() {
    echo "$ROOTFS/tmp/zfs-key.$(echo "$1" | tr '/' '_')"
}

# zfs_handoff_ready ENCRYPTIONROOT - true if a real (non-empty) staged
# secret exists for ENCRYPTIONROOT. Deliberately independent of
# keystatus - the two are allowed to disagree (that disagreement IS the
# "UNLOCKED, handoff NOT READY" state), so this never itself checks
# keystatus.
zfs_handoff_ready() {
    stage="$(zfs_key_stage_path "$1")"
    [ -s "$stage" ]
}

# zfs_stage_secret ENCRYPTIONROOT PASSPHRASE - the one place a
# passphrase is ever written to the shared tmpfs handoff location.
# umask 077/0600-equivalent - this tmpfs is already private to this
# initramfs, but a real secret gets real file permissions regardless.
#
# Publication is atomic: write the complete secret to a private
# temporary file in the SAME directory (same tmpfs, so the final `mv`
# is a plain rename, not a cross-filesystem copy), then rename it onto
# the canonical stage path only once the write itself has succeeded.
# zfs_handoff_ready() only ever looks at the canonical path, never the
# temporary - so a writer killed/interrupted mid-write (signal, I/O
# error) leaves at most an orphaned .tmp file that nothing treats as
# READY, never a partial secret at the path handoff_ready() checks.
# Before this fix, a write straight to the canonical path could leave
# a truncated-but-non-empty file there, which handoff_ready()'s own
# `[ -s "$stage" ]` check would have reported as READY - a state that
# was never actually true.
zfs_stage_secret() {
    stage="$(zfs_key_stage_path "$1")"
    stage_tmp="$stage.tmp.$$"
    rm -f "$stage_tmp"
    if ! ( umask 077; printf '%s' "$2" > "$stage_tmp" ); then
        rm -f "$stage_tmp"
        zfs_unlock_msg "failed to write the staged handoff secret for $1 - handoff will correctly report NOT READY"
        return 1
    fi
    if ! mv -f "$stage_tmp" "$stage"; then
        rm -f "$stage_tmp"
        zfs_unlock_msg "failed to publish the staged handoff secret for $1 - handoff will correctly report NOT READY"
        return 1
    fi
}

# _zfs_tty_restore - best-effort restore of the terminal state
# _zfs_prompt_once() itself saved before disabling echo (relies on the
# caller's own $stty_saved/$ZFS_TTY/$encryptionroot still being in
# scope - not a general-purpose helper, just factored out of 3 near-
# identical call sites within that one function). A restore failure
# does NOT mean a secret could leak - echo was already confirmed off
# before any passphrase was ever typed (see the fail-closed check
# above) - but a terminal left in whatever raw state `dialog` set it to
# is a real, separate usability problem, so this is loud about it and
# tries one further fallback (`stty sane`) rather than silently
# accepting an unrestored terminal.
_zfs_tty_restore() {
    [ -n "$stty_saved" ] || return 0
    stty "$stty_saved" 2>/dev/null < "$ZFS_TTY" && return 0
    zfs_unlock_msg "ERROR: could not restore terminal state after the $encryptionroot passphrase prompt - attempting 'stty sane' as a fallback"
    stty sane 2>/dev/null < "$ZFS_TTY"
}

# _zfs_unlock_cleanup_tempfiles - removes the two mktemp files that, for
# a real window between being written and being read back, hold the
# operator's plaintext passphrase: $dialog_err_file (dialog's own
# stderr/answer capture, inside _zfs_prompt_once) and $prompt_out (the
# OUT_FILE zfs_unlock()'s own loop passes it, holding the same content
# copied out). A full source audit found NEITHER file had any signal
# protection at all - an interrupt (Ctrl-C, a dropped SSH session)
# landing in that window left a real plaintext secret sitting in tmpfs
# for the rest of the rescue session, since every NORMAL rm -f site for
# these two files only runs on the ordinary, uninterrupted control-flow
# path. This function is deliberately just "rm -f whatever these two
# variable names currently hold" (${x:-} under `set -u`, since neither
# is assigned yet on a path that exits before ever reaching them) -
# there is no `local` in this codebase's shell (plain POSIX sh, not
# bash-only features), so both variables are already ordinary
# process-global state by the time either mktemp call happens, exactly
# like every other cross-function variable this file already relies on
# (encryptionroot, stty_saved, ...) - no new tracking state needed, and
# safe to call at any time, interrupted or not: `rm -f` on a path
# that's already gone (the normal case) is a silent no-op.
#
# NOT registered as a trap directly inside this file - zfs-unlock.sh is
# sourced by two different callers with two different trap situations:
# boot-dataset.sh already registers its OWN EXIT/INT/TERM/HUP trap
# (_on_exit_cleanup, see that file) BEFORE sourcing this one, and a
# trap set here would silently REPLACE it (only one handler binds per
# signal) - that caller's own _cleanup_secrets() calls this function
# directly instead (see its own comment). The standalone `zfs-unlock`
# wrapper has no competing trap of its own, and registers this function
# directly as its own trap right after sourcing this file.
_zfs_unlock_cleanup_tempfiles() {
    rm -f "${dialog_err_file:-}" "${prompt_out:-}" 2>/dev/null
}

# _zfs_prompt_once ENCRYPTIONROOT ATTEMPT ATTEMPTS VERIFY_ONLY OUT_FILE -
# ONE dialog passwordbox, with the full echo-suppression/restore/input-
# flush discipline this project's own CAX testing required (see this
# function's own inline comments, moved here verbatim from the
# original inline version in boot-dataset.sh - same behavior, same
# reasoning, not re-derived).
#
# The passphrase is written to OUT_FILE (a file the CALLER creates and
# owns), NOT returned via stdout/a command substitution - `clear` and
# `dialog` both write directly to /dev/tty already (not this function's
# own stdout - see menu.py's _dialog() docstring for the same real
# ncurses behavior), but capturing this function's return value via
# `$(...)` would still silently swallow any of ITS OWN plain stdout
# writes (diagnostic zfs_unlock_msg() calls in particular) into
# whatever the caller thought was "the passphrase" - a real, easy-to-
# reintroduce bug an earlier draft of this file actually had. A
# dedicated output file sidesteps the whole class of trap.
#
# VERIFY_ONLY=1 changes only the prompt's OWN wording (the dataset is
# already unlocked - this call is reacquiring a passphrase to verify
# and stage, not to actually unlock anything) and skips the "someone
# else already unlocked it" mid-poll race check below (there is nothing
# left to race - it's already unlocked).
#
# Returns via $?:
#   0 - a passphrase was captured, written to OUT_FILE
#   2 - cancelled (dialog ran fine, operator backed out) - retry
#   3 - dialog itself failed to initialize (see the caller's own
#       comment on why this stops retrying rather than trying again)
#   4 - VERIFY_ONLY=0 only: a remote unlock won the race while this
#       dialog was still up - OUT_FILE is untouched, caller should
#       treat this exactly like "loaded=1, no passphrase of our own"
#   5 - could not establish (and later restore) terminal echo
#       suppression - fail CLOSED: a passphrase is never accepted
#       without a confirmed-working echo guarantee, so no dialog is
#       even shown. This is the invariant this project committed to
#       after a real prior incident where a prompt proceeded anyway
#       and typed characters were exposed. Restoring the terminal
#       afterward is handled separately by _zfs_tty_restore() below,
#       which is loud (not silent) about a restore failure rather than
#       folding it into this same fail-closed contract - by the time
#       restore runs, the secret has already been captured with echo
#       confirmed off, so a restore failure is a real usability
#       problem (a terminal stuck echo-off) but not a secret-exposure
#       one, and doesn't call for discarding an already-captured
#       passphrase.
_zfs_prompt_once() {
    encryptionroot="$1"; attempt="$2"; attempts="$3"; verify_only="$4"; out_file="$5"
    : > "$out_file"
    # Checked via the command's own exit status, NOT via whether
    # $stty_saved came back non-empty - a real, working `stty -g`
    # always prints a full mode string, but the test harness's own
    # `stty() { :; }` stub (no real controlling tty to save/restore)
    # legitimately succeeds while printing nothing, and must keep
    # working exactly as before. What must fail closed is a genuine
    # `stty` failure (ENOTTY/ENXIO - no usable /dev/tty at all), not
    # "the saved state happens to be an empty string".
    if ! stty_saved="$(stty -g < "$ZFS_TTY" 2>/dev/null)"; then
        zfs_unlock_msg "cannot save terminal state via 'stty -g < $ZFS_TTY' for the $encryptionroot passphrase prompt - refusing to prompt (fail-closed: never accept a secret without a confirmed echo-suppression/restore guarantee)"
        return 5
    fi
    if ! stty -echo < "$ZFS_TTY" 2>/dev/null; then
        zfs_unlock_msg "cannot disable terminal echo via 'stty -echo < $ZFS_TTY' for the $encryptionroot passphrase prompt - refusing to prompt (fail-closed)"
        _zfs_tty_restore
        return 5
    fi
    if [ "$verify_only" = 1 ]; then
        prompt_text="$encryptionroot is already unlocked. Re-enter its passphrase to enable single-prompt boot (attempt $attempt/$attempts):"
    else
        prompt_text="Enter ZFS encryption passphrase for $encryptionroot (attempt $attempt/$attempts):"
    fi
    dialog_err_file="$(mktemp)"
    dialog --backtitle "alpine-zfsboot" --insecure --passwordbox \
        "$prompt_text" 10 60 2>"$dialog_err_file" &
    dialog_pid=$!
    while kill -0 "$dialog_pid" 2>/dev/null; do
        if [ "$verify_only" != 1 ]; then
            keystatus="$(zfs get -H -o value keystatus "$encryptionroot" 2>/dev/null)"
            if [ "$keystatus" = "available" ]; then
                kill "$dialog_pid" 2>/dev/null
                rm -f "$dialog_err_file"
                _zfs_tty_restore
                clear 2>/dev/null || true
                return 4
            fi
        fi
        sleep 1
    done
    wait "$dialog_pid" 2>/dev/null
    dialog_status=$?
    dialog_out="$(cat "$dialog_err_file" 2>/dev/null)"
    rm -f "$dialog_err_file"
    _zfs_tty_restore
    # $ZFS_TTY explicitly, NOT sys.stdin - the echo suppression/restore
    # above deliberately operates on this same terminal (see this
    # file's own header comment on why stdin can't be assumed to be the
    # same terminal under /init's exec-based redirection scheme), and
    # the flush must target the exact same terminal or it flushes
    # nothing useful in the one case (stdin != tty) this was written to
    # stop assuming away. O_NOCTTY - just reading the queue, never
    # reassigning the controlling terminal.
    ZFS_TTY="$ZFS_TTY" python3 -c '
import os, termios
try:
    fd = os.open(os.environ["ZFS_TTY"], os.O_RDONLY | os.O_NOCTTY)
    try:
        termios.tcflush(fd, termios.TCIFLUSH)
    finally:
        os.close(fd)
except OSError:
    pass
' 2>/dev/null
    clear 2>/dev/null || true
    if [ "$dialog_status" -ne 0 ]; then
        if [ -n "$dialog_out" ]; then
            # Same discriminator as menu.py's own _dialog() (see its
            # comment): dialog uses exit code 255 for BOTH ESC and a
            # genuine internal failure - an ESC always leaves stderr
            # EMPTY, a genuine crash always writes a real message.
            zfs_unlock_msg "dialog failed to prompt for the $encryptionroot passphrase: $dialog_out"
            return 3
        fi
        return 2
    fi
    printf '%s' "$dialog_out" > "$out_file"
    return 0
}

# zfs_op_lock_path ENCRYPTIONROOT - a SEPARATE lock from
# boot-dataset.sh's own $boot_lock_dir (single-owner-per-DATASET-boot,
# solves double-kexec). This one serializes the encryption STATE
# MACHINE itself: zfs_unlock() and zfs_lock() are now first-class
# operations reachable concurrently from the local console, any number
# of rescue SSH sessions, AND automatic boot orchestration all at once.
# Without this, a lock racing a reacquire-and-stage can violate this
# file's own invariant - e.g. zfs_lock() observes keystatus=available,
# removes the (not-yet-current) staged secret, runs `zfs unload-key`,
# while a concurrent zfs_unlock() reacquire finishes validating a
# passphrase and publishes a freshly staged secret right afterwards -
# ending in keystatus=unavailable WITH a staged secret present, exactly
# the state this project treats as a "semantic lie". Same sanitizing/
# scoping convention as $boot_lock_dir.
zfs_op_lock_path() {
    echo "$ROOTFS/tmp/zfs-key-lock.$(echo "$1" | tr '/' '_')"
}

# zfs_op_lock ENCRYPTIONROOT - single, non-blocking `mkdir` attempt,
# same atomicity reasoning as boot-dataset.sh's own boot lock (`mkdir`
# is atomic on a POSIX filesystem - exactly one concurrent caller's
# mkdir can ever succeed). Used directly by zfs_lock() (a single
# serialized transaction, see its own header comment) and as the first,
# non-waiting try inside zfs_op_lock_retry() below (used by zfs_unlock()
# AFTER a passphrase is already in hand - see this file's own header
# comment on why the lock is deliberately never held during the prompt
# itself).
#
# Records the holder's own $$ inside the lock dir and, on a failed
# mkdir, checks whether that recorded holder is still actually alive
# (_pid_alive, from pid-alive.sh) before giving up - a real gap an
# adversarial review found: this lock had no owner tracking and nothing
# anywhere ever cleaned up a directory left behind by a holder that
# died mid-operation (SIGKILL, an OOM kill, or SIGHUP when a rescue-SSH
# session drops mid-unlock). Every OTHER frontend - the local console,
# every new SSH session, automatic boot orchestration - would then see
# a live-looking "another encryption operation is already in progress"
# refusal until the next real reboot wiped tmpfs, even though the
# operator's own passphrase was correct the whole time. Reclaim moves
# the stale directory aside with `mv` (atomic) rather than removing it
# in place - if two frontends both observe the same dead holder, only
# one `mv` can win, and the loser fails this attempt cleanly (falling
# through to zfs_op_lock_retry()'s own next poll) instead of a race
# where both believe they hold a lock that was actually re-created out
# from under one of them.
zfs_op_lock() {
    lock_dir="$(zfs_op_lock_path "$1")"
    if mkdir "$lock_dir" 2>/dev/null; then
        echo "$$" > "$lock_dir/pid" 2>/dev/null
        return 0
    fi
    lock_pid="$(cat "$lock_dir/pid" 2>/dev/null)"
    # command -v, not a bare `! _pid_alive ...` - this file calls
    # _pid_alive but never sources pid-alive.sh itself, relying on
    # every caller having done so first (both real ones do -
    # boot-dataset.sh transitively via rescue-ssh.sh, the standalone
    # zfs-unlock wrapper explicitly). Found by unidoc-alip's review:
    # with _pid_alive undefined, `! _pid_alive "$lock_pid"` evaluates
    # to true (a bare `command not found` exits 127, and `!` negates
    # that to true), so a missing dependency would silently RECLAIM -
    # steal - a lock from a holder that is perfectly alive. This guard
    # makes that failure mode fail closed instead: no _pid_alive means
    # no reclaim, ever, not a false "the holder is dead."
    if [ -n "$lock_pid" ] && command -v _pid_alive >/dev/null 2>&1 && ! _pid_alive "$lock_pid"; then
        stale="$lock_dir.stale.$$"
        if mv "$lock_dir" "$stale" 2>/dev/null; then
            # F11 (unidoc-alip's PR #5 review): the staleness check
            # above (_pid_alive "$lock_pid") and this mv are NOT
            # atomic together - a real race: this process reads
            # lock_pid=X (dead) here; before this mv runs, a DIFFERENT
            # process that ALSO judged X dead finishes its own full
            # reclaim (mv+rm+mkdir+echo its own live pid Y) and starts
            # using the lock; THIS process's own mv then unconditionally
            # moves whatever is CURRENTLY at $lock_dir - the other
            # process's fresh, live lock, not the dead one this attempt
            # actually judged - and both processes end up believing
            # they hold it. Re-reading the pid actually captured in the
            # moved-aside directory and comparing it against the SAME
            # pid this attempt judged dead closes that window: a
            # mismatch means someone else already won a real reclaim in
            # between, so put their lock back untouched and fail this
            # attempt (falling through to the caller's own retry/poll)
            # instead of stealing it.
            moved_pid="$(cat "$stale/pid" 2>/dev/null)"
            if [ "$moved_pid" != "$lock_pid" ]; then
                mv "$stale" "$lock_dir" 2>/dev/null
                return 1
            fi
            rm -rf "$stale"
            if mkdir "$lock_dir" 2>/dev/null; then
                echo "$$" > "$lock_dir/pid" 2>/dev/null
                zfs_unlock_msg "reclaimed a stale encryption operation lock for $1 (holder pid $lock_pid is gone)"
                return 0
            fi
        fi
    fi
    return 1
}

zfs_op_unlock() {
    lock_dir="$(zfs_op_lock_path "$1")"
    rm -f "$lock_dir/pid" 2>/dev/null
    rmdir "$lock_dir" 2>/dev/null
}

# zfs_op_lock_retry ENCRYPTIONROOT - like zfs_op_lock(), but polls for a
# bounded window before giving up. Used ONLY once a passphrase has
# already been captured from a human (zfs_lock() and the non-
# interactive keylocation path in zfs_unlock() both use the plain,
# immediate zfs_op_lock() instead - neither has anything left to wait
# on a human for). By this point in zfs_unlock(), any contention is
# another terminal's own brief state transition (a `zfs load-key` +
# stage, not another human typing) - a bounded wait is worth it rather
# than discarding an already-typed passphrase and forcing a re-prompt
# on the very first collision.
#
# ~10 seconds total, not ~3 - deliberately NOT tuned to how long a
# `zfs load-key`+stage transition is expected to take on THIS
# hardware. How long that actually takes is a real, unverified
# assumption about ZFS/storage/tmpfs/platform performance, and
# correctness must not be coupled to it being fast. Giving up after
# the window elapses is always safe regardless of its length - the
# caller just discards this passphrase and re-checks current state,
# exactly like every other "someone else got there first" case in this
# function - so a longer bounded window only ever costs a little extra
# worst-case latency on a real collision, never a correctness risk.
# This is a bounded polling wait, not lock stealing - the lock itself
# is never forced or removed here. The poll interval is overridable
# (STUB_ZFS_OP_LOCK_RETRY_SLEEP) purely so tests can run this near-
# instantly while still exercising the real retry loop.
zfs_op_lock_retry() {
    # tries * poll_interval ~= 10s at the production default - a plain
    # iteration count, not elapsed-time arithmetic on poll_interval
    # itself, since that value is deliberately fractional in tests
    # (STUB_ZFS_OP_LOCK_RETRY_SLEEP=0.5 and the like) and POSIX integer
    # arithmetic can't operate on that anyway.
    poll_interval="${STUB_ZFS_OP_LOCK_RETRY_SLEEP:-1}"
    tries=10
    n=0
    while [ "$n" -lt "$tries" ]; do
        zfs_op_lock "$1" && return 0
        n=$((n + 1))
        sleep "$poll_interval"
    done
    return 1
}

# zfs_unlock ENCRYPTIONROOT - the shared primitive. Ensures
# ENCRYPTIONROOT is unlocked (prompting interactively if needed, up to
# 3 attempts for keylocation=prompt - same policy as the original
# inline version) AND, whenever possible, that a kexec handoff secret
# is staged for it - including reacquiring one via a VERIFIED re-prompt
# when the dataset arrives here already unlocked by something that
# never staged one (menu.py's OWN prior behavior, a manual `zfs
# load-key` over the rescue shell, or another terminal's own zfs_unlock()
# call that won the race below).
#
# THE ONE RULE THIS FUNCTION IS BUILT AROUND (see this file's own header
# comment for the full story of why): the per-encryptionroot operation
# lock is NEVER held while waiting on a human. It is acquired only AFTER
# a passphrase (or a verified non-interactive key) is already in hand,
# and the very first thing done under it is to re-read the CURRENT
# state - keystatus and handoff-readiness both - because either can
# have changed while this terminal's own operator was typing. Every
# state-changing decision below (a real load-key, a verify-only
# load-key -n, discarding this passphrase because another terminal
# already finished, or discovering the root was LOCKED again by a
# concurrent zfs_lock() and performing a fresh real unlock with what was
# just typed) is made from that freshly re-read state, never from
# whatever was true before the prompt.
#
# Does NOT call start_rescue_ssh/stop_rescue_ssh or fail() - those are
# boot-flow decisions specific to boot-dataset.sh's own caller, not
# properties of "how do I acquire a passphrase" itself (menu.py's own
# interactive "Unlock encrypted root" action has an operator already
# present using it right now - starting rescue SSH there would be
# meaningless). See boot-dataset.sh's own call site for that
# orchestration.
#
# Returns 0 if ENCRYPTIONROOT ends this call UNLOCKED (handoff ready or
# not - see zfs_handoff_ready() to tell those apart), 1 if it is still
# LOCKED (wrong passphrase every attempt, or a non-interactive
# keylocation that itself failed).
zfs_unlock() {
    encryptionroot="$1"
    attempts=3
    attempt=0
    while [ "$attempt" -lt "$attempts" ]; do
        attempt=$((attempt + 1))
        # A cheap, NON-authoritative peek, taken WITHOUT the lock - used
        # only to decide whether a prompt is needed at all right now,
        # and which wording to show if so. Never acted on directly; the
        # authoritative check happens again below, once the lock is
        # actually held.
        keystatus="$(zfs get -H -o value keystatus "$encryptionroot" 2>/dev/null)"
        if [ "$keystatus" = "available" ] && zfs_handoff_ready "$encryptionroot"; then
            return 0
        fi
        keylocation="$(zfs get -H -o value keylocation "$encryptionroot" 2>/dev/null)"
        # keylocation alone decides whether a human is involved at all -
        # NOT keylocation combined with keystatus. A full source audit
        # found this previously ALSO required `keystatus != available`
        # to enter this branch, which meant an ALREADY-unlocked dataset
        # with a non-prompt keylocation (file:// or https://) fell
        # through to the interactive path below instead: verify_only
        # became 1, a human was prompted for a "passphrase" that
        # doesn't correspond to anything real for this keylocation
        # class, and `zfs load-key -n` with no `-L` override reads the
        # dataset's OWN configured keylocation, silently ignoring
        # whatever was piped to it via stdin - so that check almost
        # always "succeeds" (the real file/https key still loads fine)
        # regardless of what the operator typed, and the OPERATOR'S
        # TYPED TEXT then got staged as the "verified" kexec handoff
        # secret. The target's own wrapper would later try that wrong
        # secret via `-L file:///run/alpine-zfsboot/zfs-key`, fail, and
        # fall back to a normal prompt - defeating the single-
        # passphrase-boot property this file exists for, without ever
        # actually verifying anything. The branch body just below
        # already handles BOTH keystatus states correctly under an
        # authoritative re-read of its own (available -> check handoff-
        # readiness, give up gracefully with a clear log line if
        # nothing to reacquire; not available -> a real load-key
        # attempt) - the fix is simply to let it decide based on its
        # own authoritative re-read, not gate entry on a stale
        # keystatus peek that excluded exactly the case that needed it
        # most.
        if [ "$keylocation" != "prompt" ]; then
            # No human involved at all (file:// or https://) - safe to
            # do the whole thing as one short-lived transaction under
            # the lock, same as every other caller of the plain,
            # non-waiting zfs_op_lock().
            if ! zfs_op_lock "$encryptionroot"; then
                zfs_unlock_msg "another encryption operation is already in progress for $encryptionroot - try again in a moment"
                return 1
            fi
            keystatus="$(zfs get -H -o value keystatus "$encryptionroot" 2>/dev/null)"
            if [ "$keystatus" = "available" ]; then
                zfs_op_unlock "$encryptionroot"
                if zfs_handoff_ready "$encryptionroot"; then
                    return 0
                fi
                zfs_unlock_msg "$encryptionroot is unlocked but no kexec handoff secret is staged, and keylocation=$keylocation has no passphrase to reacquire"
                return 0
            fi
            if zfs load-key "$encryptionroot" >/tmp/load-key.log 2>&1; then
                zfs_op_unlock "$encryptionroot"
                zfs_unlock_msg "encryption key loaded for $encryptionroot ($keylocation) - no interactive passphrase to stage for kexec handoff"
                return 0
            fi
            cat /tmp/load-key.log
            zfs_op_unlock "$encryptionroot"
            zfs_unlock_msg "failed to load encryption key for $encryptionroot ($keylocation)"
            return 1
        fi
        # keylocation=prompt: a human is involved. verify_only just
        # changes the PROMPT'S OWN WORDING (this dataset looked already
        # unlocked a moment ago) - it does NOT commit to which zfs
        # command runs afterward; that is decided fresh, under the
        # lock, from whatever is actually true by then.
        verify_only=0
        [ "$keystatus" = "available" ] && verify_only=1
        prompt_out="$(mktemp)"
        _zfs_prompt_once "$encryptionroot" "$attempt" "$attempts" "$verify_only" "$prompt_out"
        prompt_status=$?
        case "$prompt_status" in
            2)
                # Cancelled - retry, no lock ever touched.
                rm -f "$prompt_out"
                continue
                ;;
            3|5)
                # Dialog crashed, or the fail-closed terminal check
                # refused to even show it - stop retrying (see
                # _zfs_prompt_once()'s own comment on each), fall to
                # this loop's own final-state check below.
                rm -f "$prompt_out"
                break
                ;;
            4)
                # verify_only=0 only (see _zfs_prompt_once()'s own
                # comment) - another terminal's own transition finished
                # while this dialog was still up. Nothing of this
                # attempt's own to act on; loop back around and the
                # top-of-loop peek picks up the new state.
                rm -f "$prompt_out"
                continue
                ;;
        esac
        passphrase="$(cat "$prompt_out" 2>/dev/null)"
        rm -f "$prompt_out"

        if ! zfs_op_lock_retry "$encryptionroot"; then
            unset passphrase
            zfs_unlock_msg "another encryption operation is still in progress for $encryptionroot after waiting - discarding this passphrase and re-checking current state"
            continue
        fi
        # Authoritative: re-read state now that the lock is actually
        # held, and act ONLY on this, never on the peek taken before
        # the prompt above.
        keystatus_now="$(zfs get -H -o value keystatus "$encryptionroot" 2>/dev/null)"
        if [ "$keystatus_now" = "available" ]; then
            if zfs_handoff_ready "$encryptionroot"; then
                # Another terminal's own transition completed in full
                # while this one waited on a human - discard, done.
                unset passphrase
                zfs_op_unlock "$encryptionroot"
                zfs_unlock_msg "$encryptionroot was already unlocked and staged by another operation while waiting for input here - discarding this passphrase"
                return 0
            fi
            # Unlocked (by this terminal moments ago via a prior
            # attempt, by another terminal, or by a manual `zfs
            # load-key`), not staged - verify what was just typed
            # without disturbing the already-loaded key. See
            # zfs-load-key(8) on `-n`, and the long-standing comment
            # that used to sit here on exactly this dry-run contract.
            if printf '%s\n' "$passphrase" | zfs load-key -n "$encryptionroot" >/tmp/load-key.log 2>&1; then
                if zfs_stage_secret "$encryptionroot" "$passphrase"; then
                    # staged_by_me - see boot-dataset.sh's own
                    # _cleanup_secrets() comment (F2, unidoc-alip's PR
                    # #5 review) for why this exists: the staged-secret
                    # path is shared by every session unlocking the
                    # SAME encryptionroot, so cleanup must only ever
                    # remove a stage THIS process actually created.
                    staged_by_me=1
                    unset passphrase
                    zfs_op_unlock "$encryptionroot"
                    zfs_unlock_msg "kexec handoff secret for $encryptionroot staged"
                    return 0
                fi
                unset passphrase
                zfs_op_unlock "$encryptionroot"
                zfs_unlock_msg "verified the passphrase for $encryptionroot but staging the kexec handoff secret failed - proceeding UNLOCKED but WITHOUT kexec handoff (the target initramfs will prompt again)"
                return 0
            fi
            unset passphrase
            zfs_op_unlock "$encryptionroot"
            dialog --backtitle "alpine-zfsboot" --msgbox \
                "That does not match the already-loaded key - $((attempts - attempt)) attempt(s) remaining." 8 60 2>/dev/null
            clear 2>/dev/null || true
            continue
        fi
        # Still LOCKED (the common case), OR locked again because a
        # concurrent zfs_lock() won the race while this terminal's own
        # operator was typing - either way, a real load-key with
        # whatever was just typed is the correct next move: if this is
        # a fresh relock, the same correct passphrase unlocks it again
        # right here, with no extra re-prompt needed.
        if printf '%s\n' "$passphrase" | zfs load-key "$encryptionroot" >/tmp/load-key.log 2>&1; then
            if zfs_stage_secret "$encryptionroot" "$passphrase"; then
                # staged_by_me - see this function's other zfs_stage_secret
                # success site above for why (F2, unidoc-alip's PR #5 review).
                staged_by_me=1
                unset passphrase
                zfs_op_unlock "$encryptionroot"
                zfs_unlock_msg "encryption key loaded for $encryptionroot and kexec handoff secret staged"
                return 0
            fi
            unset passphrase
            zfs_op_unlock "$encryptionroot"
            zfs_unlock_msg "encryption key loaded for $encryptionroot, but staging the kexec handoff secret failed - proceeding UNLOCKED but WITHOUT kexec handoff (the target initramfs will prompt again)"
            return 0
        fi
        unset passphrase
        zfs_op_unlock "$encryptionroot"
        cat /tmp/load-key.log 2>/dev/null
        dialog --backtitle "alpine-zfsboot" --msgbox \
            "Incorrect passphrase - $((attempts - attempt)) attempt(s) remaining." 8 50 2>/dev/null
        clear 2>/dev/null || true
    done
    # Exhausted every attempt (or a dialog crash/fail-closed check broke
    # out early) without an explicit return above - decide the outcome
    # from the ACTUAL final state, not from which branch of the loop
    # happened to run last: genuinely still LOCKED is a real failure,
    # but "unlocked (by this terminal or another) with no verified
    # handoff secret" is not - the dataset IS readable either way.
    final_keystatus="$(zfs get -H -o value keystatus "$encryptionroot" 2>/dev/null)"
    if [ "$final_keystatus" = "available" ]; then
        zfs_unlock_msg "could not verify/stage a kexec handoff secret for $encryptionroot after $attempts attempt(s) - proceeding UNLOCKED but WITHOUT kexec handoff (the target initramfs will prompt again)"
        return 0
    fi
    zfs_unlock_msg "failed to unlock $encryptionroot after $attempts attempt(s)"
    return 1
}

# zfs_lock ENCRYPTIONROOT - the public entry point, same lock-wrap
# pattern as zfs_unlock() above (see zfs_op_lock()'s own comment for
# why this exists at all).
zfs_lock() {
    encryptionroot="$1"
    if ! zfs_op_lock "$encryptionroot"; then
        zfs_unlock_msg "another encryption operation is already in progress for $encryptionroot - try again in a moment"
        return 1
    fi
    _zfs_lock_locked "$encryptionroot"
    zfs_lock_status=$?
    zfs_op_unlock "$encryptionroot"
    return "$zfs_lock_status"
}

# _zfs_lock_locked ENCRYPTIONROOT - the inverse of
# _zfs_unlock_locked(). Refuses cleanly (does NOT force-unmount) if any
# dataset SHARING THIS ENCRYPTION KEY is currently mounted - deliberately:
# an operator who wants that is a distinct, deliberate later action, not
# an implicit side effect of "lock the encryption root". On success, the
# invariant this project relies on everywhere (menu.py's own status
# display, the state model in this file's own header comment) is
# enforced in the correct order: the staged handoff secret is gone
# BEFORE this ever reports success, so "LOCKED" never coexists with a
# leftover plaintext secret sitting in tmpfs.
_zfs_lock_locked() {
    encryptionroot="$1"
    keystatus="$(zfs get -H -o value keystatus "$encryptionroot" 2>/dev/null)"
    if [ "$keystatus" = "unavailable" ]; then
        # CONFIRMED already locked (the real, common no-op case: an
        # operator locks an already-locked root) - still worth
        # enforcing the invariant defensively (see this function's own
        # header comment) in case something left a stale staged secret
        # behind without going through this function (a killed process
        # mid-zfs_unlock(), for instance).
        rm -f "$(zfs_key_stage_path "$encryptionroot")"
        zfs_unlock_msg "$encryptionroot is already locked"
        return 0
    fi
    # Anything other than a CONFIRMED "unavailable" - "available", or a
    # genuinely unexpected value from a `zfs get` failure (transient
    # I/O error, pool momentarily busy, dataset briefly not visible) -
    # falls through to a REAL `zfs unload-key` attempt below, never
    # short-circuits to "already locked". A full source audit found
    # this check previously written as `!= "available"`, which treated
    # ANY unknown/failed read the exact same as a confirmed-locked
    # state: it took the early return above, skipped `zfs unload-key`
    # entirely, and reported success - while the key could still be
    # fully loaded and the dataset still fully readable. `zfs
    # unload-key`'s own exit status is the real, authoritative answer
    # either way and already has its own failure handling below: it
    # correctly errors out on a dataset that IS already unavailable
    # (same failure path as any other unload-key error - a report to
    # the operator, not a silent false success), and correctly unloads
    # a key that genuinely was available. There is no "unknown" state
    # left afterward the way there was with a plain `zfs get` read.
    # Cryptographically authoritative, not merely hierarchy-
    # authoritative: OpenZFS explicitly allows a clone to live ANYWHERE
    # in the pool's namespace while still using its origin's encryption
    # key (`encryptionroot` on the clone then names that origin, not
    # itself) - a plain `zfs list -r "$encryptionroot"` would miss such
    # a clone entirely if it sits outside this subtree, silently
    # unloading a key a mounted dataset elsewhere in the pool still
    # needs. So this walks every dataset in the POOL and asks ZFS
    # itself which ones actually share this encryption root, rather
    # than assuming "descendant of $encryptionroot" and "shares its
    # key" are the same thing. `zfs unload-key` itself remains the
    # final, authoritative refusal for a dataset this loop somehow
    # still missed - this is defense in depth, not the only check.
    pool="${encryptionroot%%/*}"
    busy=""
    for ds in $(zfs list -H -o name -r "$pool" 2>/dev/null); do
        ds_root="$(zfs get -H -o value encryptionroot "$ds" 2>/dev/null)"
        [ "$ds_root" = "$encryptionroot" ] || continue
        mounted="$(zfs get -H -o value mounted "$ds" 2>/dev/null)"
        # Belt-and-braces: this project mounts BEs with a raw
        # `mount -t zfs -o ro` (list_kernels()/chroot_be() in menu.py),
        # not `zfs mount`, so also check /proc/mounts directly rather
        # than trusting the `mounted` property alone to reflect every
        # mount path into this pool. Literal prefix comparison
        # (`index(...) == 1`), NOT a `~ "^" ds "/"` regex match - a
        # dataset name can itself contain characters meaningful to a
        # regex, and must never be interpreted as one.
        in_proc_mounts="$(awk -v ds="$ds" '$1 == ds || index($1, ds "/") == 1 { print $1; exit }' /proc/mounts 2>/dev/null)"
        if [ "$mounted" = "yes" ] || [ -n "$in_proc_mounts" ]; then
            mountpoint="$(zfs get -H -o value mountpoint "$ds" 2>/dev/null)"
            busy="$busy$ds (at ${mountpoint:-?})\n"
        fi
    done
    if [ -n "$busy" ]; then
        printf 'Cannot lock %s - still mounted:\n%b' "$encryptionroot" "$busy"
        zfs_unlock_msg "refusing to lock $encryptionroot - still mounted"
        return 1
    fi
    rm -f "$(zfs_key_stage_path "$encryptionroot")"
    if ! zfs unload-key "$encryptionroot" >/tmp/unload-key.log 2>&1; then
        cat /tmp/unload-key.log
        zfs_unlock_msg "zfs unload-key failed for $encryptionroot"
        return 1
    fi
    keystatus="$(zfs get -H -o value keystatus "$encryptionroot" 2>/dev/null)"
    if [ "$keystatus" != "unavailable" ]; then
        echo "zfs unload-key reported success but keystatus is still '$keystatus', not 'unavailable'"
        zfs_unlock_msg "zfs unload-key for $encryptionroot did not actually take effect (keystatus=$keystatus)"
        return 1
    fi
    zfs_unlock_msg "$encryptionroot is now locked"
    return 0
}
