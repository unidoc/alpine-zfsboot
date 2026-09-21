#!/bin/sh
# rescue-ssh.sh - start/stop the break-glass dropbear sshd, on demand,
# for exactly as long as it's needed and no longer.
#
# Deliberately separate from net-config.sh (this script CALLS it, never
# duplicates its logic) and from the "when" decision (callers - /init's
# die(), boot-dataset.sh's fail(), boot-dataset.sh's encryption-key
# wait - decide WHEN rescue SSH is needed; this file only knows HOW to
# start and stop it). Three independent concerns, three independent
# places they can be read/tested: network bring-up (net-config.sh),
# listener bind/lifecycle (this file), and the trigger conditions
# themselves (each caller's own code, where the actual boot state is
# known).
#
# Lifecycle this project's author explicitly asked for:
#   healthy boot            -> this file is never even called, no open
#                               port, ever
#   needs a decryption key,
#   or a boot/import failure -> start_rescue_ssh, operator connects,
#                               fixes/unlocks things, stop_rescue_ssh
#                               once resolved, boot continues
# This is a real behavior change from an earlier version of this
# project, which started dropbear unconditionally and early whenever a
# key was configured at all, specifically so reachability never
# depended on a boot failure being detected first. That property still
# matters (see the "started reactively, but AT the exact moment each
# trigger fires - not after a menu timeout or other delay" note on
# each caller), and is preserved here: every trigger point calls this
# BEFORE it does anything else (before the retry loop, before dropping
# to a shell) - the added latency is just "network + dropbear startup"
# (a few seconds), not "wait and see if the operator notices".
#
# Scope, stated plainly: this recovers from pre-pivot initramfs
# failures where /init's own shell or menu.py is still alive and can
# run this script - ZFS import failure, an encrypted root waiting on a
# key, a missing boot environment, a mount/kexec failure, anything
# boot-dataset.sh's own fail() or /init's own die() already catches. A
# genuine kernel panic is NOT recoverable this way - a panicked kernel
# runs nothing, including this. Kernel-panic recovery (panic=N reboot +
# a crash marker forcing the NEXT boot into rescue mode) is a real,
# separate, larger design - not implemented here, flagged as follow-up
# work for the project owner to decide on.
#
# Trust material comes from /init's own ESP-discovery pass (see its
# header comment), staged into fixed tmpfs paths this file reads
# directly - two separate files, two separate trust domains,
# deliberately: authorized_keys (who may reach THIS pre-boot rescue
# environment) is independent of whatever SSH access the target OS
# itself grants its own root user - "can SSH into the installed Alpine
# system" must never silently double as "can open a pre-boot shell with
# raw ZFS access to every dataset", so this file never reads or copies
# anything from a target boot environment's own /root/.ssh. An earlier
# version of this mechanism was a single alpine-zfsboot.ssh_key=<base64
# pubkey> cmdline/ESP-config key with no separate host identity at all
# - replaced (not kept alongside) with the two-file layout above once a
# real hardware test exposed both a spurious-warning bug in the old
# parser and the fact that a fresh in-memory dropbear host key every
# single boot makes real host-key verification unworkable (every reboot
# changes the fingerprint, training operators toward
# StrictHostKeyChecking=no). ssh_host_ed25519_key is generated ONCE, by
# the installer, and persists with the machine for its whole lifetime -
# a genuine per-machine SSH identity, reinstall-resets-it semantics,
# exactly like a normal server's own host key.
set -u
PATH=/sbin:/bin:/usr/sbin:/usr/bin
export PATH

ROOTFS="${STUB_ROOT:-}"
PIDFILE="$ROOTFS/tmp/rescue-sshd.pid"
NFT_TABLE="inet alpinezfsboot_rescue"

# Same real-boot-vs-test-harness path resolution as /init's own
# RESCUE_LIB_DIR (see its comment there for the full reasoning) -
# BASH_SOURCE[0], when this file is sourced under bash (the test
# harness), is THIS file's own real path regardless of who sourced it
# or what their own $0 is; a real boot's plain /bin/sh has no
# BASH_SOURCE at all, where $0 is whatever the top-level caller's own
# $0 was - which is fine there too, since net-config.sh sits right
# next to this file either way (both copied to `/` by build.sh).
RESCUE_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE:-$0}")" && pwd)"

# Sourced, not exec'd, for the same reason this whole file is sourced
# by ITS OWN callers rather than exec'd - see net-config.sh's own
# header comment on why a subprocess exec would be invisible to this
# project's test-harness stubbing (and, for the exact same reason,
# would bypass this file's own sourced-in stub functions during a real
# boot too - there are none in production, but the mechanism is the
# same either way: one shell, one set of function definitions,
# everything in it sees them).
. "$RESCUE_LIB_DIR/net-config.sh"

# _pid_alive - see its own file's comment. Shared with zfs-unlock.sh
# (reclaiming a stale per-encryptionroot operation lock), not
# duplicated - this used to be a second, identical copy defined right
# here.
. "$RESCUE_LIB_DIR/pid-alive.sh"

msg() {
    echo "alpine-zfsboot-rescue-ssh: $*" > /dev/kmsg 2>/dev/null
    echo "alpine-zfsboot-rescue-ssh: $*"
}

_running() {
    [ -r "$PIDFILE" ] || return 1
    pid="$(cat "$PIDFILE" 2>/dev/null)"
    [ -n "$pid" ] && _pid_alive "$pid"
}

# apply_allowlist CIDR PORT - real enforcement via nftables, not a
# dropbear-side option: dropbear's own authorized_keys restriction set
# is a documented SUBSET of OpenSSH's (no-port-forwarding,
# no-agent-forwarding, no-X11-forwarding, no-pty, restrict, permitopen,
# command= - confirmed against dropbear's own real manual page), and
# any unknown restriction (a `from="..."` entry, OpenSSH's own
# mechanism for exactly this) is silently IGNORED, not enforced - it
# would have been a dangerous silent no-op to rely on that instead of
# this. Fails LOUD (nonzero return) on anything it can't confidently
# apply - this feature must default-deny on misconfiguration, never
# silently fall open to "reachable from anywhere" just because a CIDR
# didn't parse.
#
# UNVERIFIED in this sandbox: no nft binary, no netfilter-capable
# kernel available here to actually run this against. The rule shape
# below is written from nftables' own documented syntax, not guessed,
# but has not been executed for real - confirm on the next real boot
# with alpine-zfsboot.ssh.allow= set, checked from both an allowed and
# a disallowed source.
apply_allowlist() {
    cidr="$1"
    port="$2"
    case "$cidr" in
        *:*)
            family_match="ip6 saddr"
            ;;
        *.*.*.*/*|*.*.*.*)
            family_match="ip saddr"
            ;;
        *)
            msg "malformed ALPINE_ZFSBOOT_SSH_ALLOW=$cidr - refusing to start (default-deny, not default-open)"
            return 1
            ;;
    esac
    command -v nft >/dev/null 2>&1 || {
        msg "alpine-zfsboot.ssh.allow=$cidr was set but nft is not available in this image - refusing to start (default-deny, not default-open)"
        return 1
    }
    nft add table $NFT_TABLE 2>"$ROOTFS/tmp/nft.log" &&
    nft add chain $NFT_TABLE input '{ type filter hook input priority 0 ; }' 2>>"$ROOTFS/tmp/nft.log" &&
    nft add rule $NFT_TABLE input tcp dport "$port" $family_match "$cidr" accept 2>>"$ROOTFS/tmp/nft.log" &&
    nft add rule $NFT_TABLE input tcp dport "$port" drop 2>>"$ROOTFS/tmp/nft.log" || {
        cat "$ROOTFS/tmp/nft.log"
        msg "failed to apply nftables allowlist for $cidr - refusing to start (default-deny, not default-open)"
        nft delete table $NFT_TABLE 2>/dev/null
        return 1
    }
    msg "nftables allowlist applied: only $cidr may reach tcp/$port"
    return 0
}

remove_allowlist() {
    nft delete table $NFT_TABLE 2>/dev/null || true
}

# start_rescue_ssh - idempotent: a second call while already running is
# a no-op, not a second dropbear instance.
start_rescue_ssh() {
    if _running; then
        return 0
    fi

    # Staged by /init's own ESP-discovery pass (see its header comment)
    # into fixed tmpfs paths, regardless of which of this project's
    # three trigger points ends up calling start_rescue_ssh this boot -
    # empty/absent either way if no real ESP material was ever found.
    authorized_keys_staged="$ROOTFS/tmp/alpine-zfsboot/authorized_keys"
    host_key_staged="$ROOTFS/tmp/alpine-zfsboot/ssh_host_ed25519_key"

    if [ ! -s "$authorized_keys_staged" ]; then
        msg "no alpine-zfsboot authorized_keys configured for this boot - cannot start rescue SSH"
        return 1
    fi

    # Deliberately checked before network bring-up below (cheapest,
    # side-effect-free checks first) and deliberately a hard refusal,
    # not a fallback to an ephemeral key: a fresh host identity every
    # boot is the exact behavior this whole mechanism replaces (see the
    # dropbear invocation's own comment further down) - silently
    # regenerating one here just because the persistent key is missing
    # would defeat the entire point without ever telling anyone.
    if [ ! -s "$host_key_staged" ]; then
        msg "authorized_keys is configured but no persistent ssh_host_ed25519_key was found on the ESP - refusing to start rescue SSH with a throwaway host identity (reinstall, or generate one by hand: dropbearkey -t ed25519 -f /EFI/alpine-zfsboot/ssh_host_ed25519_key)"
        return 1
    fi
    mkdir -p "$ROOTFS/etc/dropbear"
    host_key_path="$ROOTFS/etc/dropbear/dropbear_ed25519_host_key"
    cp "$host_key_staged" "$host_key_path"
    chmod 600 "$host_key_path"
    host_key_info="$(dropbearkey -y -f "$host_key_path" 2>"$ROOTFS/tmp/host-key-check.log")"
    if [ $? -ne 0 ]; then
        cat "$ROOTFS/tmp/host-key-check.log"
        rm -f "$host_key_path"
        msg "persistent ssh_host_ed25519_key failed to validate - refusing to start rescue SSH with a throwaway replacement (regenerate it: dropbearkey -t ed25519 -f /EFI/alpine-zfsboot/ssh_host_ed25519_key)"
        return 1
    fi
    msg "rescue SSH host identity: $(printf '%s\n' "$host_key_info" | grep -i fingerprint)"

    # Network is a fully separate, already-independently-testable step
    # - this is the ONLY call into it. Failure here is fatal to
    # starting rescue SSH (no point listening on an interface that
    # never came up), but net-config.sh itself has no idea dropbear
    # exists, and never will.
    if ! net_config eth0; then
        msg "network bring-up failed - rescue SSH cannot be reached, not starting dropbear"
        return 1
    fi

    port="${ALPINE_ZFSBOOT_SSH_PORT:-22}"
    case "$port" in
        ''|*[!0-9]*|0)
            msg "invalid alpine-zfsboot.ssh.port=$port, falling back to 22"
            port=22
            ;;
    esac

    allow="${ALPINE_ZFSBOOT_SSH_ALLOW:-}"
    if [ -n "$allow" ]; then
        apply_allowlist "$allow" "$port" || return 1
    fi

    listen="${ALPINE_ZFSBOOT_SSH_LISTEN:-both}"
    case "$listen" in
        ipv4) bind_spec="0.0.0.0:$port" ;;
        ipv6) bind_spec="[::]:$port" ;;
        both) bind_spec="$port" ;;   # no address = dropbear's own default dual-stack bind, same as this project's original always-on behavior
        *)
            msg "unknown alpine-zfsboot.ssh.listen=$listen, defaulting to both"
            bind_spec="$port"
            ;;
    esac

    mkdir -p "$ROOTFS/root/.ssh"
    # No command= here on purpose (unchanged from this project's
    # original reasoning): a forced command would replace ANY
    # client-requested command with menu.py, which would silently
    # break `zfs send ... | ssh host zfs recv ...` (that pipeline needs
    # the literal `zfs recv ...` to actually run). What runs for a given
    # session (menu.py vs. the client's own requested command) is
    # decided per-connection by root's login shell instead - see
    # /alpine-zfsboot-shell, which itself only ever actually runs a
    # `zfs send`/`zfs recv` invocation for the non-interactive case.
    #
    # The granular option set below (NOT the bare `restrict` keyword) is
    # injected onto EVERY accepted line here, unconditionally - a
    # mandatory property of this project's own rescue access, not
    # something left to whether the operator remembered to add it to
    # their own authorized_keys file. One rule, no ambiguity: an
    # operator who writes a bare key gets the same hardening as one who
    # writes nothing at all. $authorized_keys_staged already has
    # comments/invalid lines stripped by /init's own discovery pass -
    # one raw key line per line, no options of its own to preserve or
    # conflict with.
    #
    # Explicitly `no-port-forwarding,no-agent-forwarding,no-X11-forwarding`,
    # NOT the bare `restrict` keyword dropbear also supports (which is
    # exactly that same set PLUS `no-pty`) - confirmed against dropbear's
    # own real behavior that `restrict` denies pty allocation outright,
    # which would make every INTERACTIVE rescue session (plain `ssh
    # host`, no command - the one that's supposed to land in menu.py,
    # the same experience as the local console) fail with "not a tty"
    # before menu.py ever runs. Interactive sessions are exactly the
    # workflow this access exists for (unlocking an encrypted root
    # remotely, working the recovery menu) - a hardening keyword that
    # broke the primary use case would be worse than not adding it.
    zfsboot_key_options="no-port-forwarding,no-agent-forwarding,no-X11-forwarding"
    : > "$ROOTFS/root/.ssh/authorized_keys"
    while IFS= read -r zfsboot_key_line || [ -n "$zfsboot_key_line" ]; do
        [ -n "$zfsboot_key_line" ] || continue
        printf '%s %s\n' "$zfsboot_key_options" "$zfsboot_key_line" >> "$ROOTFS/root/.ssh/authorized_keys"
    done < "$authorized_keys_staged"
    chmod 700 "$ROOTFS/root/.ssh"
    chmod 600 "$ROOTFS/root/.ssh/authorized_keys"
    sed -i 's#^root:\([^:]*:[^:]*:[^:]*:[^:]*:[^:]*\):.*#root:\1:/alpine-zfsboot-shell#' "$ROOTFS/etc/passwd"

    # -s -g: disable password logins entirely (all users, and root
    # specifically) - confirmed against dropbear's own real manual
    # page, not assumed from "we only wrote authorized_keys". Public-
    # key auth against the configured key(s) is the ONLY way in, by
    # explicit configuration, not as a side effect of what happens to
    # be configured.
    #
    # -r <path>, NOT -R: this machine's OWN persistent Ed25519 host
    # identity, validated above - -R is dropbear's "generate hostkeys
    # as required" flag, this project's ORIGINAL behavior, deliberately
    # removed. A fresh host key every single boot means the SSH
    # fingerprint changes every single boot too, which either breaks
    # real host-key verification or trains operators into
    # StrictHostKeyChecking=no - both strictly worse than a stable
    # per-machine identity the installer generates once. Passing an
    # explicit -r means dropbear uses ONLY this key, not its own
    # default paths/auto-generation for the other key types either
    # (per dropbear's own documented -r behavior) - ed25519-only is a
    # deliberate choice (see ssh_host_ed25519_key's own name), not an
    # oversight of rsa/ecdsa, per this project's own "no additional
    # host-key types without a demonstrated compatibility reason".
    dropbear -r "$host_key_path" -F -s -g -p "$bind_spec" &
    dropbear_pid=$!
    echo "$dropbear_pid" > "$PIDFILE"

    # A backgrounded process starts successfully every time (`&` only
    # fails if fork(2) itself fails) - it says nothing about whether
    # dropbear then immediately exited on its own (bad bind_spec, a
    # port already in use, a malformed generated host key). Give it a
    # moment and actually check, rather than reporting success just
    # because the fork worked - a caller deciding "is rescue SSH really
    # reachable" (bootcheck's forced-rescue gate) needs this to be
    # truthful, not optimistic.
    sleep 1
    if ! _pid_alive "$dropbear_pid"; then
        rm -f "$PIDFILE"
        # An nftables allowlist applied above is now for a listener
        # that no longer exists - leaving it in place would (a) drop
        # every packet to $port with nothing behind it to ever answer,
        # and (b) make the NEXT start_rescue_ssh retry's own
        # `nft add table`/`nft add rule` calls stack a duplicate
        # accept/drop pair onto the same table instead of starting
        # clean.
        [ -n "$allow" ] && remove_allowlist
        msg "dropbear sshd exited immediately after starting (pid=$dropbear_pid, listen=$listen, port=$port) - rescue SSH NOT reachable"
        return 1
    fi

    msg "dropbear sshd started (pid=$dropbear_pid, listen=$listen, port=$port) - waiting for operator"
    return 0
}

# stop_rescue_ssh - safe to call even if never started (no-op). Called
# once whatever needed rescue SSH this boot is actually resolved - the
# explicit, repeated design point this project's author insisted on:
# no SSH server alive longer than the specific problem that justified
# starting it.
stop_rescue_ssh() {
    if _running; then
        pid="$(cat "$PIDFILE" 2>/dev/null)"
        kill "$pid" 2>/dev/null
        msg "dropbear sshd (pid=$pid) stopped"
    fi
    rm -f "$PIDFILE"
    remove_allowlist
}

# Allow this file to be both sourced (for its functions, the normal
# case - see /init and boot-dataset.sh) and run directly as
# `rescue-ssh.sh start|stop` for standalone testing without needing to
# fake a whole boot sequence just to exercise this file's own logic.
# $0 (NOT $1) is what distinguishes the two: sourcing (". rescue-ssh.sh")
# leaves $0 as the CALLING script's own name, never this file's -
# using $1 here instead would misfire on every caller that sources this
# file with its own real positional args already set (boot-dataset.sh's
# $1 is its DATASET argument, not "start"/"stop").
case "$0" in
    */rescue-ssh.sh|rescue-ssh.sh)
        case "${1:-}" in
            start) start_rescue_ssh ;;
            stop) stop_rescue_ssh ;;
        esac
        ;;
esac
