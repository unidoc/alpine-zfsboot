#ifndef ZFSBOOT_BIOS_GPT_H
#define ZFSBOOT_BIOS_GPT_H

#include "stdint_local.h"

/*
 * Minimal GPT header + partition-array reading - only what stage2
 * needs: find the one partition it cares about (the "alpine-zfsboot
 * boot blob", by type GUID) and hand back its starting LBA + size. No
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
 * ZFSBOOT_BOOTBLOB_TYPE_GUID: a real, freshly generated random UUID
 * (f5bd658b-eee4-402f-be5b-d939c082b649), specific to this project,
 * not reused from anywhere else - same precedent as EFIVAR_GUID in
 * init/menu.py. Identifies the one partition on a legacy-BIOS/GPT
 * disk that holds the boot-blob (see bootblob.h) build.sh packs and
 * this code reads - deliberately a NEW type GUID, not GRUB's own
 * "BIOS boot partition" GUID (21686148-6449-6E6F-744E-656564454649,
 * which the *stage1/stage2 code itself* lives on instead, at a fixed
 * LBA stage1 already knows by construction - see stage1.S - so it
 * never needs to be found via GPT lookup at all).
 *
 * Stored here in the mixed-endian on-disk byte order the GPT spec
 * itself uses for every GUID field (first three fields little-endian,
 * last two as-is) - computed once for real (not hand-converted) and
 * cross-checked against Python's own uuid module before being written
 * here, not just eyeballed hex.
 */
#define ZFSBOOT_BOOTBLOB_TYPE_GUID_BYTES \
	{ 0x8b, 0x65, 0xbd, 0xf5, 0xe4, 0xee, 0x2f, 0x40, \
	  0xbe, 0x5b, 0xd9, 0x39, 0xc0, 0x82, 0xb6, 0x49 }

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
 * ZFSBOOT_BOOTBLOB_TYPE_GUID_BYTES for the boot-blob partition).
 * On a match, fills *start_lba and *sector_count (inclusive range
 * converted to a plain count) and returns 0. Returns -1 if no
 * matching partition is found, or if hdr itself looks invalid.
 */
int gpt_find_partition(const struct gpt_header *hdr, const uint8_t type_guid[16],
                        uint64_t *start_lba, uint64_t *sector_count);

#endif
