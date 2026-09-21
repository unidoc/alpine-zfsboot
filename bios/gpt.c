#include "gpt.h"
#include "disk.h"

/*
 * Verified directly in THIS translation unit, not a standalone
 * reproduction elsewhere - if this ever trips, whatever gpt.h this
 * exact compile sees is not the 92-byte packed struct the rest of
 * this file assumes, and every check below is meaningless until
 * that's fixed first.
 */
_Static_assert(sizeof(struct gpt_header) == 92,
               "gpt_header used by gpt.c is not 92 bytes");

/*
 * Bit-by-bit CRC32 (ISO 3309 / zlib's own polynomial and convention -
 * the exact algorithm the UEFI spec's own GPT header_crc32 field
 * uses) - no lookup table, since this runs exactly once per boot over
 * one 92-byte header; a table would trade a few hundred bytes of
 * flash for microseconds this stage doesn't need to save.
 */
static uint32_t crc32(const void *data, uint32_t len)
{
	static const uint32_t poly = 0xEDB88320u;
	const uint8_t *p = (const uint8_t *)data;
	uint32_t crc = 0xFFFFFFFFu;
	uint32_t i, j;

	for (i = 0; i < len; i++) {
		crc ^= p[i];
		for (j = 0; j < 8; j++) {
			if (crc & 1)
				crc = (crc >> 1) ^ poly;
			else
				crc >>= 1;
		}
	}
	return ~crc;
}

static int guid_equal(const uint8_t *a, const uint8_t *b)
{
	int i;
	for (i = 0; i < 16; i++) {
		if (a[i] != b[i])
			return 0;
	}
	return 1;
}

int gpt_read_header(struct gpt_header *hdr)
{
	uint8_t sector[DISK_SECTOR_SIZE];
	uint8_t work[sizeof(struct gpt_header)];
	uint32_t stored_crc, computed_crc;
	int i;

	if (disk_read_lba(GPT_HEADER_LBA, 1, sector) != 0)
		return -1;

	for (i = 0; i < (int)sizeof(work); i++)
		work[i] = sector[i];

	for (i = 0; i < GPT_SIGNATURE_LEN; i++) {
		if (work[i] != GPT_SIGNATURE[i])
			return GPT_ERR_NO_SIGNATURE;
	}

	for (i = 0; i < (int)sizeof(*hdr); i++)
		((uint8_t *)hdr)[i] = work[i];

	/*
	 * The spec allows header_size to exceed sizeof(*hdr) (reserved
	 * trailing bytes this struct doesn't model) - every real writer,
	 * including this project's own future partitioning step, always
	 * writes exactly 92 (sizeof(*hdr)) here, so a header claiming
	 * otherwise is treated as invalid outright rather than handled,
	 * keeping the CRC computation below simple (always exactly
	 * sizeof(*hdr) bytes, never a variable length).
	 */
	if (hdr->header_size != sizeof(*hdr))
		return -1;

	stored_crc = hdr->header_crc32;
	/* CRC32 is computed with header_crc32 itself zeroed during the
	 * computation - standard GPT convention, per the UEFI spec's own
	 * GPT header definition. */
	((struct gpt_header *)work)->header_crc32 = 0;
	computed_crc = crc32(work, sizeof(work));

	if (computed_crc != stored_crc)
		return -1;

	return 0;
}

int gpt_find_partition(const struct gpt_header *hdr, const uint8_t type_guid[16],
                        uint64_t *start_lba, uint64_t *sector_count)
{
	uint8_t sector[DISK_SECTOR_SIZE];
	uint32_t entries_per_sector, entry_index, sector_index, total_sectors_to_scan, i;

	/*
	 * Lower bound is 48, not just "nonzero": that's the offset one
	 * past the last field this code actually dereferences from a
	 * struct gpt_partition_entry (ending_lba, at byte offset 40, 8
	 * bytes wide - type_guid at offset 0 is covered trivially).
	 * Without this, a header claiming a smaller entry size (whether
	 * from real corruption or a deliberately crafted GPT - this
	 * header is trusted disk data, not re-verified against anything
	 * beyond its own CRC32) would walk `entry` past the end of the
	 * fixed 512-byte `sector` buffer below on the last iteration of
	 * the inner loop - a real, if narrow (read-only, 40 bytes past a
	 * stack buffer), out-of-bounds read.
	 */
	if (hdr->partition_entry_size < 48 || hdr->partition_entry_size > DISK_SECTOR_SIZE)
		return -1;
	if (hdr->num_partition_entries == 0)
		return -1;

	/*
	 * Assumes partition_entry_size divides DISK_SECTOR_SIZE evenly
	 * (true for every real-world GPT writer, which always uses the
	 * standard 128-byte entry size - 4 per 512-byte sector) - an
	 * entry straddling a sector boundary would need extra handling
	 * this code doesn't do, a deliberate, narrow limitation.
	 */
	entries_per_sector = DISK_SECTOR_SIZE / hdr->partition_entry_size;
	if (entries_per_sector == 0)
		return -1;

	total_sectors_to_scan =
		(hdr->num_partition_entries + entries_per_sector - 1) / entries_per_sector;

	entry_index = 0;
	for (sector_index = 0; sector_index < total_sectors_to_scan; sector_index++) {
		uint64_t lba = hdr->partition_entry_lba + sector_index;

		if (disk_read_lba(lba, 1, sector) != 0)
			return -1;

		for (i = 0; i < entries_per_sector && entry_index < hdr->num_partition_entries;
		     i++, entry_index++) {
			const struct gpt_partition_entry *entry =
				(const struct gpt_partition_entry *)(sector + (i * hdr->partition_entry_size));

			if (guid_equal(entry->type_guid, type_guid)) {
				*start_lba = entry->starting_lba;
				*sector_count = entry->ending_lba - entry->starting_lba + 1;
				return 0;
			}
		}
	}

	return -1;
}
