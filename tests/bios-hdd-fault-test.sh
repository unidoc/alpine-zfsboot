#!/bin/sh
# bios-hdd-fault-test.sh - the fault-injection boots of bios-hdd-entry-test.sh:
# the real raw-disk stage1+stage2 in QEMU, built with disk.c's DISK_FI_* so the
# "BIOS" answers CF=0 with wrong data (a flipped bit, a sector of zeros, stale
# data, the wrong LBA's data, a short transfer with the DAP count unchanged) -
# once, on every transfer above 8 sectors, or on every read of one sector -
# every kernel block wrong (the breaker: only 3 blocks re-read and reported),
# plus a second (mirror) disk, a BIOS without INT 13h extensions (CHS), and a
# BIOS whose AH=48h reports 4096-byte sectors. What each must show is in
# bios-hdd-entry-test.sh's scenario list. Needs what that script needs.
set -eu
HDD_TEST_SCENARIOS="fi-transient-flip fi-transient-zero fi-transient-stale fi-transient-wronglba fi-transient-short \
fi-above-flip fi-above-zero fi-above-stale fi-above-wronglba fi-above-short \
fi-persistent fi-persistent-enforce fi-allbad fi-second fi-noext fi-4k" \
    exec "$(dirname "$0")/bios-hdd-entry-test.sh"
