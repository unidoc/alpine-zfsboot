#ifndef ZFSBOOT_BIOS_DISK_H
#define ZFSBOOT_BIOS_DISK_H

#include "stdint_local.h"

/*
 * Plain BIOS INT 0x13 disk reads - deliberately the ONLY thing in
 * this file, kept apart from switch32.S's own GDT/CR0 trickery (see
 * that file's header comment): a real-mode BIOS interrupt call needs
 * none of that, so it stays ordinary, easy-to-review C + inline asm.
 */

#define DISK_SECTOR_SIZE 512

/*
 * Records the BIOS drive number (e.g. 0x80 for the first hard disk) -
 * stage1 receives this from the BIOS itself in %dl at boot (before
 * anything has a chance to clobber it) and stage2_entry.S passes it
 * through as stage2_main()'s own argument; this must be called before
 * disk_read_lba() below.
 */
void disk_init(uint8_t drive_number);

/*
 * Reads `count` sectors starting at LBA `lba` into `buf`, via the
 * BIOS INT 0x13, AH=0x42 "extended read" service (the LBA-capable
 * one - the old CHS-based AH=0x02 service can't address a modern
 * disk past ~8GB at all). `buf` must be real-mode-addressable - a
 * plain near pointer into this stage's own data area (below 1MB,
 * within the current %ds) - never a >1MB address; see switch32.S's
 * unreal_copy() for getting data from a low buffer like this one up
 * to its final >1MB destination.
 *
 * Returns 0 on success, -1 on any BIOS-reported error - stops at the
 * first failing sector, no retry: a transient read error on a boot
 * disk this early isn't something worth trying to paper over.
 */
int disk_read_lba(uint64_t lba, uint16_t count, void *buf);

#endif
