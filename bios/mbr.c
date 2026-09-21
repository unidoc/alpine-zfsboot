#include "mbr.h"
#include "disk.h"

/*
 * One classic MBR partition-table entry, exactly as it lies in bytes
 * 446-509 of LBA 0 (4 of these, back to back - see mbr.h). Field
 * order and sizes matter, hence __attribute__((packed)); the two CHS
 * fields are legacy geometry this code never reads (LBA is all this
 * whole boot stage ever uses, same as gpt.c).
 */
struct mbr_partition_entry {
	uint8_t status;
	uint8_t chs_first[3];
	uint8_t type;
	uint8_t chs_last[3];
	uint32_t start_lba;
	uint32_t sector_count;
} __attribute__((packed));

int mbr_find_partition(uint8_t type, uint64_t *start_lba, uint64_t *sector_count)
{
	uint8_t sector[DISK_SECTOR_SIZE];
	int i;

	if (disk_read_lba(0, 1, sector) != 0)
		return -1;

	if (sector[MBR_BOOT_SIGNATURE_OFFSET] != 0x55 || sector[MBR_BOOT_SIGNATURE_OFFSET + 1] != 0xAA)
		return -1;

	for (i = 0; i < MBR_MAX_PARTITIONS; i++) {
		const struct mbr_partition_entry *entry =
			(const struct mbr_partition_entry *)(sector + MBR_PARTITION_TABLE_OFFSET +
			                                      i * MBR_PARTITION_ENTRY_SIZE);

		if (entry->type == type && entry->sector_count != 0) {
			*start_lba = entry->start_lba;
			*sector_count = entry->sector_count;
			return 0;
		}
	}

	return -1;
}
