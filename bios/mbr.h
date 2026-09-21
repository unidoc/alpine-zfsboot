#ifndef ZFSBOOT_BIOS_MBR_H
#define ZFSBOOT_BIOS_MBR_H

#include "stdint_local.h"

/*
 * Minimal, classic MBR partition-table reading - a fallback for
 * stage2's own boot-blob lookup for a disk partitioned the older way
 * (fdisk/parted with an msdos label) instead of GPT (see gpt.h for
 * the primary, GPT-based version this project's own sgdisk-based
 * install instructions produce). Deliberately no extended/logical
 * partition chain support - kept to exactly the 4 fixed-size entries
 * the classic MBR itself holds, simple and complete for what this
 * project actually needs: one partition, found by a project-specific
 * type byte, the same role ZFSBOOT_BOOTBLOB_TYPE_GUID plays for GPT.
 *
 * stage1.S itself needs NO changes to support this: it already reads
 * stage2 from a fixed LBA (34) regardless of what partitioning scheme
 * is on the rest of the disk - the same "leave LBA 1-33 free" build-
 * time invariant (see stage1.S's own header comment) applies whether
 * the disk ends up GPT or MBR-labeled; only stage2's own boot-blob
 * partition *lookup* needs to know about both schemes.
 */

#define MBR_BOOT_SIGNATURE_OFFSET 510
#define MBR_PARTITION_TABLE_OFFSET 446
#define MBR_PARTITION_ENTRY_SIZE 16
#define MBR_MAX_PARTITIONS 4

/*
 * ZFSBOOT_BOOTBLOB_MBR_TYPE: an arbitrary, project-specific MBR
 * partition type byte, picked the same way ZFSBOOT_BOOTBLOB_TYPE_GUID
 * was for GPT (see gpt.h) - deliberately not one of the many already-
 * assigned values in the classic MBR partition-type list (0x83 Linux,
 * 0x82 swap, 0xEE GPT-protective, 0xEF EFI system, etc.), since
 * nothing except this project's own code (both the install-time write
 * and this lookup) ever needs to recognize it.
 */
#define ZFSBOOT_BOOTBLOB_MBR_TYPE 0x2E

/*
 * Reads LBA 0, checks the 0x55AA boot signature, and scans the (up
 * to) 4 primary partition entries for one whose type byte matches
 * `type` and whose sector_count is nonzero (an all-zero entry is how
 * an unused primary slot is conventionally represented - not a real
 * partition to match against). On a match, fills *start_lba/
 * *sector_count directly from the entry (already plain LBA + count,
 * unlike GPT's inclusive start/end pair) and returns 0. Returns -1 if
 * the signature is missing or no partition matches.
 */
int mbr_find_partition(uint8_t type, uint64_t *start_lba, uint64_t *sector_count);

#endif
