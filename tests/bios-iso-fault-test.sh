#!/bin/sh
# bios-iso-fault-test.sh - the ISO (El Torito) counterpart of
# bios-hdd-fault-test.sh: bios-iso-entry-test.sh boots with stage-iso.bin
# built with cdrom_disk.c's DISK_FI_* fault injection (CF=0 with wrong data),
# on both CD backends (INT 13h and the ATAPI driver), and checks what the
# per-block checks (EFI/ALPINE/BLKSUM, written by iso.sh) do:
#   - wrong data on every read above 8 512-byte units: healed by halving
#     the transfer size, which then stays at 8;
#   - wrong data once in kernel block 1: healed by one re-read;
#   - the same wrong data on every read of one sector: reported with the
#     block and both digests, then booted (integrity warn) or refused
#     (integrity enforce).
# Plus the ISO without CHECKSUM/BLKSUM (as built before this existed) and
# the transfer cap at 127/64/32/16/8/1 512-byte units. Needs what
# bios-iso-entry-test.sh needs.
set -eu
T="$(dirname "$0")/bios-iso-entry-test.sh"
O=$((5 * 512 + 65536 + 300))  # a byte of kernel block 1 (RAM range)
rc=0
run() { echo "=== $1"; shift; env "$@" sh "$T" || rc=1; }

run "no CHECKSUM/BLKSUM on the ISO (as before)" ISO_TEST_NO_MANIFEST=1 ISO_TEST_EXPECT="no EFI/ALPINE/CHECKSUM"
run "above 8 units, INT 13h" ISO_EXTRA_CFLAGS="-DDISK_FI_MODE=1 -DDISK_FI_ABOVE=8" \
    ISO_TEST_EXPECT="summary: cd int13 cap 0008|healed 0001 2nd 0000 bad 0000|verifying kernel+initrd"
run "above 8 units, ATAPI" FORCE_ATAPI=1 ISO_EXTRA_CFLAGS="-DDISK_FI_MODE=2 -DDISK_FI_ABOVE=8" \
    ISO_TEST_EXPECT="summary: cd atapi cap 0008|healed 0001 2nd 0000 bad 0000|verifying kernel+initrd"
run "once, INT 13h" ISO_FI_KERNEL_OFFSET=$O ISO_EXTRA_CFLAGS="-DDISK_FI_MODE=2 -DDISK_FI_TIMES=1" \
    ISO_TEST_EXPECT="healed 0001 2nd 0000 bad 0000|verifying kernel+initrd"
run "persistent, warn, INT 13h" ISO_FI_KERNEL_OFFSET=$O ISO_EXTRA_CFLAGS="-DDISK_FI_MODE=1" \
    ISO_TEST_EXPECT="integrity: KERNEL block 0001 stayed wrong after 08 re-reads|bad 0001|WARNING: differs from CHECKSUM - booting anyway"
run "persistent, enforce, ATAPI" FORCE_ATAPI=1 ISO_TEST_INTEGRITY=enforce ISO_TEST_EXPECT_HALT=1 ISO_FI_KERNEL_OFFSET=$O \
    ISO_EXTRA_CFLAGS="-DDISK_FI_MODE=2" ISO_TEST_EXPECT="integrity: KERNEL block 0001 stayed wrong after 08 re-reads|integrity enforce: a block stayed wrong"
for c in 127 64 32 16 8 1; do
    run "int13chunk=$c" ISO_TEST_CMDLINE_EXTRA="alpine-zfsboot.int13chunk=$c" \
        ISO_TEST_EXPECT="int13chunk: transfers of at most $(printf '0x%08x' "$c") sectors|verifying kernel+initrd"
done
exit $rc
