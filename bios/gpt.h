#ifndef ZFSBOOT_BIOS_GPT_H
#define ZFSBOOT_BIOS_GPT_H

#include "stdint_local.h"

/*
 * Minimal GPT header + partition-array reading - only what stage2
 * needs: find the one partition it cares about (the canonical
 * alpine-zfsboot FAT/ESP partition, by type GUID - see
 * ZFSBOOT_ESP_TYPE_GUID_BYTES below) and hand back its starting LBA +
 * size. No
 * writing, no CRC verification of the partition array (the header's
 * own CRC is checked - see gpt_read() - but re-summing the whole
 * partition array on every boot is real code+time this stage doesn't
 * need to spend re-verifying something the disk-write path is
 * responsible for getting right once), no support for a corrupt
 * primary header falling back to the backup at the end of the disk -
 * a real, deliberate gap, acceptable for how narrow this is: reading
 * one already-known-good partition boot.sh itself just wrote.
 */

#define GPT_SIGNATURE "EFI PART"
#define GPT_SIGNATURE_LEN 8

/*
 * Returned by gpt_read_header() specifically when the sector read
 * cleanly but simply did not start with the GPT signature at all -
 * distinct from a generic -1 (I/O read failure, or a signature that IS
 * present but the header_size/CRC32 don't validate). This distinction
 * matters to stage2_main.c's own retry loop: a missing signature is
 * the normal, PERMANENT state of a msdos/MBR-labeled disk (see this
 * project's own DISK_LAYOUT=msdos install mode) - retrying can never
 * turn "EFI PART" into existence, unlike a genuine transient read
 * error or corrupt-but-present header, which retrying for real can
 * plausibly recover from.
 */
#define GPT_ERR_NO_SIGNATURE (-2)

/* GPT header lives in LBA 1 (right after the protective MBR in LBA 0) - a
 * fixed, standardized location, not something this code discovers. */
#define GPT_HEADER_LBA 1

/*
 * The GPT header is only 92 bytes of a 512-byte (or larger) logical
 * sector - the rest of that sector is reserved/zero. This struct is
 * exactly the on-disk layout, per the UEFI spec's own GPT header
 * table - field order and sizes matter, hence __attribute__((packed)).
 */
struct gpt_header {
	char signature[GPT_SIGNATURE_LEN]; /* "EFI PART" */
	uint32_t revision;
	uint32_t header_size;
	uint32_t header_crc32;
	uint32_t reserved;
	uint64_t my_lba;
	uint64_t alternate_lba;
	uint64_t first_usable_lba;
	uint64_t last_usable_lba;
	uint8_t disk_guid[16];
	uint64_t partition_entry_lba;
	uint32_t num_partition_entries;
	uint32_t partition_entry_size;
	uint32_t partition_entry_array_crc32;
} __attribute__((packed));

/*
 * One partition entry - partition_entry_size in the header above is
 * usually 128 (this struct's own size), but the spec allows it to be
 * larger with the tail reserved/vendor-defined - gpt_find_partition()
 * in gpt.c strides by the header's own partition_entry_size, not
 * sizeof(this struct), so a future larger entry size on some other
 * disk still gets walked correctly.
 */
struct gpt_partition_entry {
	uint8_t type_guid[16];
	uint8_t unique_guid[16];
	uint64_t starting_lba;
	uint64_t ending_lba; /* inclusive */
	uint64_t attributes;
	uint16_t name[36]; /* UTF-16LE, not used by this code */
} __attribute__((packed));

/*
 * ZFSBOOT_ESP_TYPE_GUID: the REAL, standard EFI System Partition type
 * GUID (C12A7328-F81F-11D2-BA4B-00A0C93EC93B - the same GUID `sgdisk
 * -t N:EF00` writes, and the one UEFI firmware itself looks for).
 * Deliberately NOT a project-specific GUID: since alpine-zfsboot's own
 * unified architecture change, this ESP is the single canonical,
 * firmware-neutral alpine-zfsboot storage partition on every layout -
 * UEFI firmware loads BOOTX64.EFI/BOOTAA64.EFI from it directly, and
 * on legacy-BIOS/GPT disks THIS code (stage2's own FAT reader, see
 * fat.h) finds the exact same partition, by the exact same real ESP
 * identity, to load the kernel/initramfs/cmdline stage2 needs - one
 * partition, one identity, both firmware paths. This retires the
 * project's own former ZFSBOOT_BOOTBLOB_TYPE_GUID (a freshly generated
 * random UUID that identified a filesystem-less "boot blob" partition
 * format this project no longer uses at all - see git history, not
 * this file, for that format).
 *
 * Distinct from GRUB's own "BIOS boot partition" GUID
 * (21686148-6449-6E6F-744E-656564454649), which the *stage1/stage2
 * code itself* lives on instead, at a fixed LBA stage1 already knows
 * by construction - see stage1.S - so it never needs to be found via
 * GPT lookup at all.
 *
 * Stored here in the mixed-endian on-disk byte order the GPT spec
 * itself uses for every GUID field (first three fields little-endian,
 * last two as-is) - computed once for real (not hand-converted) and
 * cross-checked against Python's own uuid module before being written
 * here, not just eyeballed hex; verified against this file's own
 * pre-existing ZFSBOOT_BOOTBLOB_TYPE_GUID_BYTES entry (same byte-order
 * algorithm, known-correct worked example) before being trusted.
 */
#define ZFSBOOT_ESP_TYPE_GUID_BYTES \
	{ 0x28, 0x73, 0x2a, 0xc1, 0x1f, 0xf8, 0xd2, 0x11, \
	  0xba, 0x4b, 0x00, 0xa0, 0xc9, 0x3e, 0xc9, 0x3b }

/*
 * Reads and validates the primary GPT header (signature + CRC32 of
 * the header itself) into *hdr. Returns 0 on success; on failure,
 * GPT_ERR_NO_SIGNATURE if the sector read cleanly but had no GPT
 * signature at all (a msdos/MBR-labeled disk - permanent, not worth
 * retrying), or -1 for any other failure (the read itself failed, or
 * a signature IS present but header_size/CRC32 don't validate - both
 * genuinely worth a retry, see GPT_ERR_NO_SIGNATURE's own comment).
 */
int gpt_read_header(struct gpt_header *hdr);

/*
 * Scans the partition array for the first entry whose type_guid
 * matches type_guid (16 raw bytes, on-disk order - pass
 * ZFSBOOT_ESP_TYPE_GUID_BYTES for the canonical alpine-zfsboot FAT/ESP
 * partition).
 * On a match, fills *start_lba and *sector_count (inclusive range
 * converted to a plain count) and returns 0. Returns -1 if no
 * matching partition is found, or if hdr itself looks invalid.
 */
int gpt_find_partition(const struct gpt_header *hdr, const uint8_t type_guid[16],
                        uint64_t *start_lba, uint64_t *sector_count);

#endif
