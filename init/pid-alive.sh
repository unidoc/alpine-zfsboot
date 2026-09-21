#!/bin/sh
# _pid_alive PID - like `kill -0`, but also correct for a zombie: a
# background child that already exited but hasn't been reaped yet
# still owns its pid (kill -0 on it succeeds) until something actually
# wait()s on it. Whether/when that reap happens automatically for a
# plain `cmd &` with no job control (busybox ash, this project's real
# shell at boot) is not something to assume either way, so check
# /proc's own idea of the process's state directly instead - /proc is
# already mounted well before this file is ever sourced (see /init's
# own `mount -t proc` near the top of its startup).
#
# Its own file, sourced by both rescue-ssh.sh (the original caller -
# checking whether a stale dropbear PIDFILE's process is actually
# still running) and zfs-unlock.sh (reclaiming a stale per-
# encryptionroot operation lock - see zfs_op_lock()'s own comment) -
# not duplicated between them.
_pid_alive() {
    pid="$1"
    [ -d "/proc/$pid" ] || return 1
    state="$(awk '{print $3}' "/proc/$pid/stat" 2>/dev/null)"
    [ -n "$state" ] && [ "$state" != "Z" ]
}
