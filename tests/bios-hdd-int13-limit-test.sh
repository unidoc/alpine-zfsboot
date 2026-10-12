#!/bin/sh
# bios-hdd-int13-limit-test.sh - boots the real raw-disk stage1+stage2 in QEMU as
# a classic MBR hard disk, like bios-hdd-entry-test.sh, but in a build whose
# INT 13h REJECTS every transfer above 16 sectors with status 0x09
# (-DDISK_FAULT_INJECT_MAX_SECTORS=16, see bios/disk.c): the behaviour of a BIOS
# or option ROM that does not accept the 64-sector reads the FAT layer asks for.
# QEMU's own SeaBIOS accepts every size, so no ordinary boot test can see this.
#
# Written after a physical host failed with "FAT read failed loading kernel" a
# few MiB into the kernel and nothing said whether the BIOS or the FAT chain was
# to blame. What it asserts: the loader (disk_policy.c) shrinks its chunk size,
# says so on screen ("BIOS refused a read ..."; with alpine-zfsboot.diag=1 also
# every failed call: LBA, count, attempt, status), still loads the kernel and
# initrd, the bytes in RAM match EFI/ALPINE/CHECKSUM (so the split reads
# were byte-correct, not just "successful"), and it reaches "starting kernel".
# Before disk_policy.c, the same build stopped with "FAT read failed loading
# kernel".
#
# It is bios-hdd-entry-test.sh's int13-limit scenario; the injected build is
# made in a temporary COPY of bios/ (BIOS_EXTRA_CFLAGS) so no fault-injected
# object file can ever end up in the tree. stage1 is not affected by the
# injection (it calls the real BIOS directly). Needs the same tools as
# bios-hdd-entry-test.sh.
set -eu
BIOS_EXTRA_CFLAGS=-DDISK_FAULT_INJECT_MAX_SECTORS=16 HDD_TEST_SCENARIOS=int13-limit \
    exec "$(dirname "$0")/bios-hdd-entry-test.sh"
