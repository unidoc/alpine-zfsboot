#ifndef ZFSBOOT_BIOS_FAT_H
#define ZFSBOOT_BIOS_FAT_H

#include "stdint_local.h"

/*
 * Minimal, read-only FAT32 reader - the canonical alpine-zfsboot
 * runtime payload (kernel/initramfs/cmdline) lives as ordinary files
 * on the SAME FAT partition UEFI mode already uses as its ESP (see
 * gpt.h's ZFSBOOT_ESP_TYPE_GUID and mbr.h's ZFSBOOT_FAT_MBR_TYPE), not
 * in a separate filesystem-less "boot blob" partition any more.
 *
 * Deliberately narrow: FAT32 only (what `mkfs.vfat -F32` already
 * produces for the existing ESP - no FAT12/16 support), no write path
 * at all, and no VFAT long-filename (LFN) support - every path this
 * driver ever resolves is a project-controlled, plain 8.3 short name
 * (EFI/ALPINE/KERNEL, .../INITRD, .../CMDLINE - see fat_open()'s own
 * comment for why long names were deliberately avoided for this one
 * namespace). It IS a real parser, not a predetermined-LBA shortcut:
 * real BPB/geometry validation, real cluster-chain walking through the
 * FAT table itself, bounds/overflow checks on every cluster number,
 * and a hard cap on chain-walk length to catch a corrupt or cyclic
 * chain rather than looping forever.
 *
 * Calls only disk_read_lba() (see disk.h) for I/O - exactly like
 * gpt.c/mbr.c already do - so it works unmodified on both the real-
 * disk build (disk.c, BIOS INT13h) and the El Torito ISO build
 * (cdrom_disk.c+ata_atapi.c) without knowing or caring which backend
 * answered it.
 */

struct fat_volume {
	uint64_t partition_lba; /* LBA of this FAT volume's own boot sector (the partition's starting LBA) */
	uint32_t bytes_per_sector;
	uint32_t sectors_per_cluster;
	uint32_t num_fats;
	uint32_t sectors_per_fat;
	uint32_t root_cluster;
	uint32_t total_clusters; /* usable data clusters - valid cluster numbers are 2..total_clusters+1 */
	uint64_t fat_start_lba;
	uint64_t data_start_lba; /* LBA where cluster #2 begins */
};

struct fat_file {
	uint32_t first_cluster;
	uint32_t size; /* bytes, as recorded in the file's own directory entry */
};

/*
 * Reads and validates the FAT32 boot sector (BPB) at partition_lba
 * (the volume's own LBA 0, i.e. the partition's starting LBA as
 * already found via gpt_find_partition()/mbr_find_partition()).
 * partition_sectors is that SAME lookup's own reported partition
 * size - the external, GPT/MBR-derived trust boundary this volume's
 * own BPB-claimed extent (total_sectors_32/16) is checked against.
 * Returns 0 on success, -1 on any validation failure: missing 0x55AA
 * signature, unsupported sector size, non-power-of-two cluster size,
 * a FAT12/16 volume (root_entry_count/fat_size_16 nonzero, or a
 * resulting cluster count below FAT32's own spec floor of 65525), a
 * filesystem-type string that doesn't read "FAT32   ", or a
 * self-consistent BPB whose own claimed total_sectors exceeds what
 * partition_sectors says the real partition actually holds (see this
 * function's own definition for why a BPB-internal consistency check
 * alone is not enough).
 */
int fat_mount(struct fat_volume *vol, uint64_t partition_lba, uint64_t partition_sectors);

/*
 * Resolves an absolute, '/'-separated path of short (<=8 character,
 * extension-less) uppercase components - e.g. "/EFI/ALPINE/KERNEL" -
 * to a file. Returns 0 and fills *file on success; -1 if any
 * component is missing, is the wrong type (a file where a directory
 * was expected or vice versa), or the directory structure is
 * malformed/cyclic.
 *
 * No long-filename (VFAT LFN) support: entries with attribute 0x0F
 * are skipped outright. This is safe for every path component this
 * driver itself ever resolves - EFI and ALPINE (the directories) and
 * KERNEL/INITRD/CMDLINE (the files it opens), all deliberately kept
 * 8.3-safe specifically so this reader never needs an LFN decoder -
 * see this project's own architecture-decision writeup. The SAME
 * EFI/ALPINE directory also holds config/authorized_keys/
 * ssh_host_ed25519_key (long names, no 8.3 constraint on THOSE - see
 * /init's own comment) - fine, since this function is never pointed at
 * them; they're read only by /init, via the real kernel's own vfat
 * driver, after boot, which handles LFN transparently. One directory,
 * two readers, each only ever opening the specific files it needs.
 */
int fat_open(const struct fat_volume *vol, const char *path, struct fat_file *file);

/*
 * Called periodically during a fat_read_range() call with the number
 * of bytes copied since the last call (not a running total) - purely
 * for operator feedback on a large, otherwise-silent transfer (a 50+MB
 * initrd over real BIOS disk I/O takes long enough that "no output at
 * all" reads as a hang, not as progress - see fat_read_range()'s own
 * comment). Deliberately dumb: this file has no idea what a "dot" is
 * or how big a MiB is - that policy (when to actually print something)
 * belongs to the caller, which already owns the console. Pass NULL (or
 * anything) as ctx if unused - progress is optional, pass a NULL
 * function pointer to fat_read_range() to skip it entirely (cheap,
 * for the small header/cmdline reads that don't need feedback at all).
 */
typedef void (*fat_progress_fn)(void *ctx, uint32_t bytes_done);

/*
 * Reads `length` bytes starting at byte `offset` within `file`,
 * writing them to physical address `dst_phys` via unreal_copy() (see
 * switch32.h - works for destinations above and below 1MB alike, the
 * same primitive stage2_main.c's own former load_to_high() used).
 * offset+length must not exceed file->size - checked here, not left
 * to the caller. Returns 0 on success, -1 on any I/O failure, an
 * out-of-range request, or a corrupt/cyclic FAT chain (a hard cap on
 * clusters walked catches a cycle rather than looping forever).
 *
 * Batches physically-contiguous sectors (within one cluster, and
 * across cluster boundaries when consecutive cluster numbers are
 * genuinely contiguous on disk - see cluster_to_lba() in fat.c) into
 * single, larger disk_read_lba() calls, up to FAT_IO_BATCH_SECTORS
 * sectors at a time - NOT one disk_read_lba() call per 512-byte
 * sector unconditionally. That was a real, measured bottleneck: on
 * real SeaBIOS/INT13h hardware, one sector per BIOS call meant roughly
 * 117000 individual interrupts to load a 57MB initrd (confirmed by
 * reading an earlier version of fat_read_range() directly, not
 * inferred). A fragmented file (non-contiguous clusters) still works
 * correctly - the batch simply shrinks back down to whatever run of
 * genuinely contiguous sectors is actually available at each point,
 * verified by this project's own test suite against both fragmented
 * and contiguous fixtures (see bios/tests/).
 *
 * progress may be NULL to skip progress reporting entirely.
 */
int fat_read_range(const struct fat_volume *vol, const struct fat_file *file,
                    uint32_t offset, uint32_t length, uint32_t dst_phys,
                    fat_progress_fn progress, void *progress_ctx);

#endif
