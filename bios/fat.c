#include "fat.h"
#include "disk.h"
#include "switch32.h"

/*
 * On-disk FAT32 boot sector (BPB) layout, per the Microsoft FAT spec's
 * own BPB32 table - field order and sizes matter, hence
 * __attribute__((packed)), same convention as gpt.h/mbr.c's own
 * on-disk structs.
 */
struct fat32_bpb {
	uint8_t jmp[3];
	char oem_name[8];
	uint16_t bytes_per_sector;
	uint8_t sectors_per_cluster;
	uint16_t reserved_sector_count;
	uint8_t num_fats;
	uint16_t root_entry_count;  /* must be 0 on FAT32 - the root dir is a normal cluster chain, not a fixed area */
	uint16_t total_sectors_16;  /* 0 on FAT32 - total_sectors_32 below is used instead */
	uint8_t media;
	uint16_t fat_size_16;       /* must be 0 on FAT32 - fat_size_32 below is used instead */
	uint16_t sectors_per_track;
	uint16_t num_heads;
	uint32_t hidden_sectors;
	uint32_t total_sectors_32;
	uint32_t fat_size_32;       /* sectors per FAT */
	uint16_t ext_flags;
	uint16_t fs_version;
	uint32_t root_cluster;
	uint16_t fsinfo_sector;
	uint16_t backup_boot_sector;
	uint8_t reserved[12];
	uint8_t drive_number;
	uint8_t reserved1;
	uint8_t boot_signature;
	uint32_t volume_id;
	char volume_label[11];
	char fs_type[8]; /* "FAT32   ", space-padded - informational per spec, checked here anyway as a cheap sanity gate */
} __attribute__((packed));

_Static_assert(sizeof(struct fat32_bpb) == 90,
               "fat32_bpb used by fat.c is not 90 bytes");

/*
 * One short (8.3) directory entry, exactly as it lies on disk - 32
 * bytes, per the FAT spec's own directory-entry table.
 */
struct fat_dirent {
	char name[11]; /* 8.3, space-padded, no dot stored */
	uint8_t attr;
	uint8_t nt_reserved;
	uint8_t create_time_tenth;
	uint16_t create_time;
	uint16_t create_date;
	uint16_t access_date;
	uint16_t first_cluster_hi;
	uint16_t write_time;
	uint16_t write_date;
	uint16_t first_cluster_lo;
	uint32_t file_size;
} __attribute__((packed));

_Static_assert(sizeof(struct fat_dirent) == 32,
               "fat_dirent used by fat.c is not 32 bytes");

#define FAT_ATTR_DIRECTORY 0x10u
#define FAT_ATTR_VOLUME_ID 0x08u
#define FAT_ATTR_LONG_NAME 0x0Fu /* READ_ONLY|HIDDEN|SYSTEM|VOLUME_ID all set together - the LFN marker, never a real short entry */

#define FAT_DIRENT_FREE_REST 0x00u  /* name[0]: no more entries in this directory, in this or any later cluster */
#define FAT_DIRENT_DELETED 0xE5u    /* name[0]: this entry is deleted - skip it, keep scanning */

#define FAT_CLUSTER_MASK 0x0FFFFFFFu /* FAT32 entries are 32 bits wide but only the low 28 bits are the cluster number - top 4 are reserved */

/*
 * Generous relative to any realistically-sized kernel/initrd on even
 * the smallest FAT32 cluster size (65536 clusters covers a 70MB file
 * down to a 1KB-per-cluster volume, well past anything mkfs.vfat -F32
 * would ever actually choose for a partition this size) - a real,
 * finite ceiling so a corrupt or cyclic FAT chain can never loop this
 * driver forever, not "unlimited".
 */
#define FAT_MAX_CHAIN_CLUSTERS 65536u

/* Component length cap for fat_pack_name()/fat_open() below - see
 * fat_open()'s own header comment for why this driver only ever needs
 * to match short, extension-less names. */
#define FAT_MAX_COMPONENT_LEN 8

/*
 * Batch size for fat_read_range()'s own bulk data reads - the cap on
 * how many sectors a single disk_read_lba() call here is allowed to
 * cover, once fat_read_range() has confirmed they're genuinely
 * contiguous on disk (see its own comment). Confirmed the hard way: an
 * early version of fat_read_range() read one 512-byte sector per
 * disk_read_lba() call unconditionally, regardless of how many
 * subsequent sectors/clusters were physically contiguous - a real 57MB
 * initrd meant roughly 117000 individual BIOS INT13h calls, each with
 * its own real per-call overhead, and was reported as "noticeably
 * slow, no progress indication, looks hung" on real SeaBIOS hardware.
 *
 * Overridable via `-DFAT_IO_BATCH_SECTORS=N` (see bios/Makefile) -
 * the real ceiling here is two stacked link-time invariants, both in
 * bios/Makefile: the raw real-mode segment fit (this stage's whole
 * data area, g_fat_io_buf included, must stay under offset 0x10000 -
 * see disk.c's own DAP comment for why - enforced first, by the
 * LINKER's own R_386_16 relocation-overflow check, before
 * check-bss-bounds ever runs) and, since this session's own
 * BSS/stack-collision hardening pass, check-bss-bounds's own tighter
 * `STAGE2_STACK_TOP - STAGE2_STACK_MARGIN` ceiling (currently 0xf7f0)
 * on top of that - NOT one shared number for every build either way.
 * The two stage2 builds genuinely differ: 36 (18432 bytes) is
 * stage2_main.c's own former CHUNK_SIZE, already tuned by hand against
 * a real ATAPI command-batching ceiling for the ISO/El-Torito build
 * (stage-iso.bin, which links ata_atapi.c's own .bss usage on top of
 * this buffer and has measurably less headroom - confirmed the hard
 * way: FAT_IO_BATCH_SECTORS=64 on this build overflows the raw 64KB
 * segment outright, failing at the LINKER's own relocation-overflow
 * check before check-bss-bounds gets a chance to run at all - not
 * "check-bss-bounds rejects it", a stricter and earlier failure than
 * that). The real DISK build (stage2.bin, disk.c/plain INT13h only,
 * never touches ATAPI at all - see this Makefile's own STAGE2_C_SRCS
 * vs STAGE2_ISO_C_SRCS split) was carrying that same ATAPI-derived cap
 * for no reason of its own; 64 (32768 bytes) is confirmed safe for it
 * specifically (`_bss_end` lands at 0xc641, comfortably under both the
 * 0x10000 segment limit and the tighter 0xf7f0 stack-margin ceiling -
 * measured directly against the current tree, not carried over from
 * an earlier build) and roughly halves its own real INT13h call count
 * for a large initrd - the actual, measured bottleneck once
 * fat_get_next_cluster()'s own cache (see above) closed the FAT-table-
 * lookup gap. Kept as two different numbers, not one raised for both,
 * because they answer two different questions ("what does ATAPI allow"
 * vs "what fits in this segment") that only happen to share a
 * variable name.
 *
 * Re-examined (not just re-asserted) alongside cdrom_disk.c's own
 * NATIVE_BATCH=11: 36 GPT-sectors already needs only ceil(36/4)=9
 * native (2048-byte) sectors, which already fits inside ONE
 * NATIVE_BATCH=11 hardware command - so this batch size already never
 * costs more than a single INT13h/ATAPI command for the ISO backend;
 * raising it further doesn't reduce hardware command count for the
 * common case (still one command, up to 44 GPT-sectors' worth), only
 * static headroom cdrom_disk.c's own NATIVE_BATCH margin needs more -
 * see that file's own NATIVE_BATCH comment for the measured numbers.
 * 36 stays as-is.
 */
#ifndef FAT_IO_BATCH_SECTORS
#define FAT_IO_BATCH_SECTORS 36
#endif
static uint8_t g_fat_io_buf[FAT_IO_BATCH_SECTORS * DISK_SECTOR_SIZE];

static int fat_read_sector(const struct fat_volume *vol, uint64_t lba, uint8_t *buf)
{
	(void)vol;
	return disk_read_lba(lba, 1, buf);
}

/* Same as fat_read_sector() but for `count` (<= FAT_IO_BATCH_SECTORS,
 * enforced by every caller, not re-checked here) physically contiguous
 * sectors in ONE disk_read_lba() call, into the shared g_fat_io_buf
 * staging buffer - the actual fix for the one-sector-at-a-time
 * bottleneck described above. Static/global, not a local/stack array,
 * for the same reason stage2_main.c's own former g_chunk buffer was:
 * this stage's real-mode stack (see stage1.S's own `movw $0x7c00, %sp`)
 * has nowhere near enough room for an 18KB local array. Costs nothing
 * in stage2.bin's own on-disk size either way - .bss is excluded from
 * the flat binary (see stage2_entry.S's own comment), only real-mode
 * memory at runtime, which has ample room here.
 */
static int fat_read_sectors(const struct fat_volume *vol, uint64_t lba, uint32_t count, uint8_t *buf)
{
	(void)vol;
	return disk_read_lba(lba, (uint16_t)count, buf);
}

static uint64_t cluster_to_lba(const struct fat_volume *vol, uint32_t cluster)
{
	return vol->data_start_lba + (uint64_t)(cluster - 2) * vol->sectors_per_cluster;
}

/*
 * Reads the FAT table entry for `cluster` and returns its raw 28-bit
 * value in *next (the caller decides what a given value means - free/
 * bad/end-of-chain/a real next cluster - since that differs between a
 * directory walk, which treats end-of-chain as a normal stopping
 * point, and a data read, which treats it as an error if more bytes
 * were still expected). Bounds-checks `cluster` itself against the
 * volume's real cluster count first, so a bad cluster number can never
 * compute an out-of-range FAT-table LBA in the first place.
 */
/*
 * One-sector cache for fat_get_next_cluster()'s own FAT-table reads,
 * separate from g_fat_io_buf (that one holds file DATA, this one holds
 * FAT-table metadata - different content, must never share a buffer).
 * Without this, walking N clusters in a row - which both
 * fat_chain_advance() and fat_read_range()'s own contiguity-probe loop
 * do, one cluster at a time, by design (see their own comments) - cost
 * N separate disk_read_lba() calls even when every one of those hops
 * landed in the SAME 512-byte FAT sector (128 cluster entries per
 * sector on FAT32). Confirmed the hard way: fat_read_range()'s batching
 * fix (see FAT_IO_BATCH_SECTORS above) only ever batched the file DATA
 * reads - real hardware still showed kernel/initrd loading as the
 * slowest phase of boot after that fix shipped, and reading this
 * function is what explains why: the FAT-table lookups driving the
 * chain walk were never batched or cached at all, so a large,
 * many-cluster file still paid one full INT13h call per cluster
 * regardless of how big FAT_IO_BATCH_SECTORS was. This driver is
 * read-only (see fat.h's own header comment - no write path exists
 * anywhere in this file), so a cached sector can never go stale from a
 * write this code itself made.
 */
static uint8_t g_fat_table_cache[DISK_SECTOR_SIZE];
static uint64_t g_fat_table_cache_lba;
static int g_fat_table_cache_valid;

/*
 * A SECOND, independent one-sector cache slot, used only by
 * fat_chain_advance()'s own tortoise pointer (see that function) -
 * found necessary by a real, measured HDD boot-performance audit, not
 * theorized. fat_chain_advance() advances two pointers per hop: the
 * "hare" (the real chain position) every call, and the "tortoise"
 * (Floyd's cycle detection - see that function's own comment) every
 * OTHER call. Once a chain is longer than ~128 clusters (one FAT
 * sector's worth of 4-byte entries), the tortoise trails the hare by
 * enough clusters that the two are reading from DIFFERENT FAT sectors
 * - and with only ONE cache slot shared between them, every tortoise
 * hop evicted the hare's just-cached sector, and the very next hare
 * hop evicted the tortoise's, in a constant back-and-forth that left
 * the single-slot cache almost never actually hit for any real,
 * multi-hundred-cluster file. Measured directly (a real QEMU boot,
 * instrumented disk_read_lba() call counts, real 72MB kernel+initrd
 * on a realistic ~4KB-cluster volume): 20827 total INT13h calls before
 * this, 2824 after - a 7.4x reduction, entirely from this one change,
 * with the file-data batching (FAT_IO_BATCH_SECTORS) itself completely
 * unchanged. The theoretical minimum for that same file (distinct FAT
 * sectors it actually needs, at 128 cluster-entries per sector) is
 * ~144 - a single shared cache slot was achieving closer to 0% hit
 * rate than 100% for any chain long enough to matter, which is exactly
 * why splitting the tortoise into its own slot (needing no eviction
 * policy at all - just two fixed, independent slots by caller role)
 * fixes nearly all of it. Confirmed via bios/tests/fat_host_test.c
 * too: cyclic/corrupt-chain detection (the tortoise's actual job)
 * still terminates and fails cleanly - this only changes WHERE its
 * lookups are cached, never the values a correct walk depends on.
 */
static uint8_t g_fat_table_cache2[DISK_SECTOR_SIZE];
static uint64_t g_fat_table_cache2_lba;
static int g_fat_table_cache2_valid;

static int fat_get_next_cluster_slot(const struct fat_volume *vol, uint32_t cluster, uint32_t *next, int use_slot2)
{
	uint64_t fat_sector;
	/*
	 * Deliberately uint32_t, not uint64_t: cluster is itself a 28-bit
	 * quantity (FAT32's own cluster-number width) so cluster*4 always
	 * fits in 32 bits for any cluster number fat_cluster_in_range()
	 * would ever let through - this keeps the division/modulo below a
	 * plain 32-bit operation the target's own hardware DIV instruction
	 * handles directly, rather than a 64-bit one that would need
	 * __udivmoddi4 from libgcc - not linked into this freestanding,
	 * no-libc/no-libgcc build (confirmed the hard way: an earlier draft
	 * of this function used uint64_t here and failed to link with
	 * exactly that undefined reference).
	 */
	uint32_t fat_offset, sector_offset, raw;

	if (cluster < 2 || cluster >= vol->total_clusters + 2)
		return -1;

	fat_offset = cluster * 4u;
	fat_sector = vol->fat_start_lba + fat_offset / vol->bytes_per_sector;
	sector_offset = fat_offset % vol->bytes_per_sector;

	if (use_slot2) {
		if (!g_fat_table_cache2_valid || g_fat_table_cache2_lba != fat_sector) {
			if (fat_read_sector(vol, fat_sector, g_fat_table_cache2) != 0) {
				g_fat_table_cache2_valid = 0;
				return -1;
			}
			g_fat_table_cache2_lba = fat_sector;
			g_fat_table_cache2_valid = 1;
		}
		raw = (uint32_t)g_fat_table_cache2[sector_offset] | ((uint32_t)g_fat_table_cache2[sector_offset + 1] << 8) |
		      ((uint32_t)g_fat_table_cache2[sector_offset + 2] << 16) | ((uint32_t)g_fat_table_cache2[sector_offset + 3] << 24);
		*next = raw & FAT_CLUSTER_MASK;
		return 0;
	}

	if (!g_fat_table_cache_valid || g_fat_table_cache_lba != fat_sector) {
		if (fat_read_sector(vol, fat_sector, g_fat_table_cache) != 0) {
			g_fat_table_cache_valid = 0;
			return -1;
		}
		g_fat_table_cache_lba = fat_sector;
		g_fat_table_cache_valid = 1;
	}

	raw = (uint32_t)g_fat_table_cache[sector_offset] | ((uint32_t)g_fat_table_cache[sector_offset + 1] << 8) |
	      ((uint32_t)g_fat_table_cache[sector_offset + 2] << 16) | ((uint32_t)g_fat_table_cache[sector_offset + 3] << 24);

	*next = raw & FAT_CLUSTER_MASK;
	return 0;
}

/*
 * Plain (non-tortoise) accesses - the peek-ahead contiguity probe in
 * fat_read_range() and fat_chain_advance()'s own hare - share the
 * first cache slot; see fat_get_next_cluster_slot()'s own header
 * comment for why the tortoise gets a separate one.
 */
static int fat_get_next_cluster(const struct fat_volume *vol, uint32_t cluster, uint32_t *next)
{
	return fat_get_next_cluster_slot(vol, cluster, next, 0);
}

/*
 * True if `cluster` is a valid, in-range data cluster this volume
 * actually has - the one check every cluster-chain advance in this
 * file runs before trusting a FAT-table value (whether it's a real
 * next cluster, an end-of-chain marker that's arrived too early, or
 * flat-out corrupt) as something safe to keep walking.
 */
static int fat_cluster_in_range(const struct fat_volume *vol, uint32_t cluster)
{
	return cluster >= 2 && cluster < vol->total_clusters + 2;
}

/*
 * Cluster-chain cursor shared by every chain walk in this file
 * (directory traversal in fat_find_in_dir(), the skip-ahead and the
 * main read loop in fat_read_range()) - tracks not just the current
 * position ("hare") but a second, half-speed "tortoise" pointer for
 * real cycle detection (Floyd's algorithm), not just a generous hop
 * count.
 *
 * FAT_MAX_CHAIN_CLUSTERS alone is NOT sufficient here: a caller
 * driven by a byte/entry budget (fat_read_range() in particular) can
 * satisfy that budget by re-reading a SHORT cycle's clusters over and
 * over - the hop count still comes out exactly as expected, so it
 * never trips a hop-count ceiling at all; the bug is silently WRONG
 * data (repeated clusters), not a hang or an over-long walk. Confirmed
 * the hard way against a synthetic self-referencing test fixture
 * (bios/tests/build_fat_fixtures.py's cyclic_chain.img): an earlier
 * version of this file that only capped chain_walked against
 * FAT_MAX_CHAIN_CLUSTERS returned success with corrupted output on
 * that fixture instead of failing. The tortoise pointer catches this
 * directly - it advances one cluster every second hare hop, so on any
 * real cycle of length L the two pointers coincide within about 2L
 * hops, independent of how many total bytes/entries the caller still
 * wants.
 */
struct fat_chain_walk {
	uint32_t cluster;   /* current ("hare") position */
	uint32_t tortoise;  /* half-speed cycle-detection pointer */
	uint32_t hops;      /* total hare hops so far - FAT_MAX_CHAIN_CLUSTERS is still the backstop for a very long but genuinely non-cyclic chain */
	int tortoise_due;   /* alternates each hop - tortoise only moves on every other one */
};

static void fat_chain_walk_init(struct fat_chain_walk *w, uint32_t first_cluster)
{
	w->cluster = first_cluster;
	w->tortoise = first_cluster;
	w->hops = 0;
	/*
	 * Starts at 1 (not 0) so the first flip in fat_chain_advance()
	 * below lands on 0 (tortoise does NOT move on hare's first hop).
	 * Both pointers start at the same cluster - if tortoise moved on
	 * hop 1 too, it would land on the exact same node hare just moved
	 * to and immediately (and wrongly) look like a cycle on every
	 * single walk, cyclic or not. Confirmed the hard way: an earlier
	 * version of this file initialized this to 0 and failed even the
	 * plain, non-cyclic multi-cluster read tests in
	 * fat_host_test.c - not just the deliberately cyclic fixture.
	 */
	w->tortoise_due = 1;
}

/*
 * Advances the walk by one cluster. Returns 0 with w->cluster updated
 * to the new position, or -1 if the chain ended before this hop (a
 * normal, non-error outcome for a directory-search caller, which
 * checks fat_cluster_in_range() on its own to tell "end of chain"
 * apart from "real I/O/range failure" - see fat_find_in_dir()), the
 * new cluster is out of range, or a cycle was detected.
 */
static int fat_chain_advance(const struct fat_volume *vol, struct fat_chain_walk *w)
{
	uint32_t next;

	if (w->hops++ > FAT_MAX_CHAIN_CLUSTERS)
		return -1;
	if (fat_get_next_cluster(vol, w->cluster, &next) != 0)
		return -1;
	if (!fat_cluster_in_range(vol, next))
		return -1;
	w->cluster = next;

	w->tortoise_due = !w->tortoise_due;
	if (w->tortoise_due) {
		uint32_t t_next;

		if (fat_get_next_cluster_slot(vol, w->tortoise, &t_next, 1) != 0)
			return -1;
		if (!fat_cluster_in_range(vol, t_next))
			return -1;
		w->tortoise = t_next;
		if (w->tortoise == w->cluster)
			return -1; /* cycle detected */
	}
	return 0;
}

int fat_mount(struct fat_volume *vol, uint64_t partition_lba, uint64_t partition_sectors)
{
	uint8_t sector[DISK_SECTOR_SIZE];
	const struct fat32_bpb *bpb;
	uint32_t fat_size, total_sectors, data_sectors, cluster_count;
	/*
	 * nonstring: deliberately NOT NUL-terminated - this is an exact,
	 * fixed 8-byte, space-padded field compared byte-for-byte against
	 * the BPB's own fs_type[8] below, which is itself never
	 * NUL-terminated (a real FAT32 BPB field, not a C string) - newer
	 * GCC's -Wunterminated-string-initialization would otherwise warn
	 * that the trailing NUL from this literal doesn't fit, which is
	 * exactly the intended behavior here, not a bug.
	 */
	static const char expect_fs_type[8] __attribute__((nonstring)) = "FAT32   ";

	/*
	 * Invalidate fat_get_next_cluster()'s cross-call FAT-sector cache -
	 * a real disk only ever has one volume live at a time in stage2's
	 * own boot flow, but the host test harness mounts several different
	 * fixture images in one process, and two different volumes can
	 * share the same LBA number while holding completely different FAT
	 * tables. Without this reset, a stale hit here would silently
	 * return the wrong image's data instead of a fresh read. Both
	 * slots - see g_fat_table_cache2's own comment for why there are
	 * two now.
	 */
	g_fat_table_cache_valid = 0;
	g_fat_table_cache2_valid = 0;
	int i;

	if (disk_read_lba(partition_lba, 1, sector) != 0)
		return -1;

	if (sector[510] != 0x55 || sector[511] != 0xAA)
		return -1;

	bpb = (const struct fat32_bpb *)sector;

	if (bpb->bytes_per_sector != DISK_SECTOR_SIZE)
		return -1; /* only the one sector size every other backend/struct in this stage already assumes */

	if (bpb->sectors_per_cluster == 0)
		return -1;
	if ((bpb->sectors_per_cluster & (bpb->sectors_per_cluster - 1)) != 0)
		return -1; /* must be a power of two per the FAT spec */

	if (bpb->num_fats == 0 || bpb->reserved_sector_count == 0)
		return -1;

	/* FAT32-specific fields: a FAT12/16 volume always has a nonzero
	 * root_entry_count/fat_size_16 here instead - reject outright
	 * rather than trying to also support those. */
	if (bpb->root_entry_count != 0 || bpb->fat_size_16 != 0)
		return -1;
	if (bpb->fat_size_32 == 0 || bpb->root_cluster < 2)
		return -1;

	for (i = 0; i < 8; i++) {
		if (bpb->fs_type[i] != expect_fs_type[i])
			return -1;
	}

	fat_size = bpb->fat_size_32;
	total_sectors = bpb->total_sectors_32 != 0 ? bpb->total_sectors_32 : bpb->total_sectors_16;
	if (total_sectors == 0)
		return -1;

	/*
	 * A full source audit (second pass, after the fat_size-vs-
	 * cluster_count check below already closed one escape route)
	 * found this call's own caller (stage2_main.c) discovers a REAL,
	 * external size for this partition via gpt_find_partition()/
	 * mbr_find_partition() - the partition table's own entry, not
	 * anything this volume's own BPB claims about itself - and then
	 * threw it away (`(void)esp_sectors`) before ever calling this
	 * function, trusting the BPB's own total_sectors unconditionally
	 * instead. That is backwards: the partition table is the ONE
	 * authority for "how big is this partition", and a FAT32 volume's
	 * own BPB is just content living inside whatever that authority
	 * says the partition is - it must never get to overrule it. The
	 * fat_size-vs-cluster_count check below only proves total_sectors
	 * is INTERNALLY consistent with fat_size (both fields agree with
	 * each other) - it says nothing about whether total_sectors is
	 * consistent with the REAL partition size, and a fully
	 * self-consistent BPB (total_sectors AND fat_size both inflated
	 * together, in proportion) sails through it unchanged. Rejecting
	 * here, before any further BPB-derived arithmetic runs, closes the
	 * actual escape: every subsequent cluster/LBA computed in this
	 * function is monotonic in total_sectors, so bounding total_sectors
	 * itself against the partition's own real size bounds every
	 * cluster this volume can ever address, transitively - see this
	 * function's own reserved_plus_fats/data_start_lba arithmetic
	 * below for why no separate per-cluster bounds check is needed on
	 * top of this one.
	 */
	if (total_sectors > partition_sectors)
		return -1;

	{
		uint32_t reserved_plus_fats = bpb->reserved_sector_count + bpb->num_fats * fat_size;
		if (reserved_plus_fats >= total_sectors)
			return -1; /* underflow guard - the arithmetic below would wrap */
		data_sectors = total_sectors - reserved_plus_fats;
	}

	cluster_count = data_sectors / bpb->sectors_per_cluster;
	if (cluster_count < 65525u)
		return -1; /* below FAT32's own spec floor - a mislabeled/corrupt FAT12/16 volume */

	/*
	 * cluster_count above is derived entirely from total_sectors_32/
	 * sectors_per_cluster - fat_size (the ACTUAL on-disk FAT table
	 * size, a completely separate BPB field) is never cross-checked
	 * against it. A full source audit found this a real gap: an
	 * inflated/corrupt total_sectors_32 (paired with a small,
	 * genuine fat_size_32) produces a cluster_count implying more
	 * clusters than the real FAT table can actually describe -
	 * fat_cluster_in_range() then validates every cluster number
	 * against THIS SAME inflated cluster_count, so a high cluster
	 * number that's "in range" per that check can still compute a FAT-
	 * entry sector address (fat_get_next_cluster()'s own
	 * fat_start_lba + cluster*4/bytes_per_sector arithmetic) that
	 * lands PAST the real FAT table entirely - into the data region
	 * that immediately follows it (data_start_lba = fat_start_lba +
	 * num_fats*fat_size), reading whatever's actually there as if it
	 * were real FAT entries, and potentially chaining a "file" through
	 * arbitrary disk content, including - on this project's own
	 * layout, where the ESP is immediately followed by the ZFS pool -
	 * past the whole ESP partition's own bounds. Each FAT32 entry is 4
	 * bytes; cluster_count+2 entries must fit (rounding up) within
	 * fat_size sectors - reject outright if the BPB's own declared
	 * fat_size can't actually hold as many clusters as total_sectors_32
	 * implies exist, the same "don't trust one field's implications
	 * over another's" discipline the reserved_plus_fats/data_sectors
	 * underflow guard above already applies.
	 */
	{
		uint64_t required_fat_bytes = (uint64_t)(cluster_count + 2) * 4u;
		uint64_t required_fat_sectors =
			(required_fat_bytes + bpb->bytes_per_sector - 1) / bpb->bytes_per_sector;
		if ((uint64_t)fat_size < required_fat_sectors)
			return -1;
	}

	vol->partition_lba = partition_lba;
	vol->bytes_per_sector = bpb->bytes_per_sector;
	vol->sectors_per_cluster = bpb->sectors_per_cluster;
	vol->num_fats = bpb->num_fats;
	vol->sectors_per_fat = fat_size;
	vol->root_cluster = bpb->root_cluster;
	vol->total_clusters = cluster_count;
	vol->fat_start_lba = partition_lba + bpb->reserved_sector_count;
	vol->data_start_lba = vol->fat_start_lba + (uint64_t)bpb->num_fats * fat_size;

	if (!fat_cluster_in_range(vol, vol->root_cluster))
		return -1;

	return 0;
}

/*
 * Packs `component` (a plain C string, expected uppercase, at most
 * FAT_MAX_COMPONENT_LEN characters, no dot) into the 11-byte space-
 * padded short-name form directory entries store on disk. No
 * extension handling at all - every path this driver resolves is
 * extension-less by construction (see fat_open()'s own comment), so
 * this is deliberately NOT a general 8.3 name packer.
 */
static void fat_pack_name(const char *component, char packed[11])
{
	int i;

	for (i = 0; i < 11; i++)
		packed[i] = ' ';
	for (i = 0; i < FAT_MAX_COMPONENT_LEN && component[i]; i++)
		packed[i] = component[i];
}

/*
 * Scans directory `dir_cluster` for an entry whose short name matches
 * packed_name, requiring it to be a directory (want_dir=1) or a plain
 * file (want_dir=0). On a match, fills out_cluster/out_size from the
 * entry and returns 0. Returns -1 if not found, the type doesn't
 * match, or the directory's own cluster chain is out of range/cyclic.
 */
static int fat_find_in_dir(const struct fat_volume *vol, uint32_t dir_cluster, const char packed_name[11],
                            int want_dir, uint32_t *out_cluster, uint32_t *out_size)
{
	struct fat_chain_walk w;
	uint8_t sector[DISK_SECTOR_SIZE];

	if (!fat_cluster_in_range(vol, dir_cluster))
		return -1;
	fat_chain_walk_init(&w, dir_cluster);

	for (;;) {
		uint32_t s;

		for (s = 0; s < vol->sectors_per_cluster; s++) {
			uint64_t lba = cluster_to_lba(vol, w.cluster) + s;
			uint32_t e, entries_per_sector;

			if (fat_read_sector(vol, lba, sector) != 0)
				return -1;

			entries_per_sector = DISK_SECTOR_SIZE / sizeof(struct fat_dirent);
			for (e = 0; e < entries_per_sector; e++) {
				const struct fat_dirent *ent =
					(const struct fat_dirent *)(sector + e * sizeof(struct fat_dirent));
				int i, match;

				if ((uint8_t)ent->name[0] == FAT_DIRENT_FREE_REST)
					return -1; /* end of this directory's real entries - nothing to find here or later */
				if ((uint8_t)ent->name[0] == FAT_DIRENT_DELETED)
					continue;
				if (ent->attr == FAT_ATTR_LONG_NAME)
					continue; /* VFAT LFN entry - never matched by this driver, see fat_open() */
				if (ent->attr & FAT_ATTR_VOLUME_ID)
					continue;

				match = 1;
				for (i = 0; i < 11; i++) {
					if (ent->name[i] != packed_name[i]) {
						match = 0;
						break;
					}
				}
				if (!match)
					continue;

				if ((want_dir && !(ent->attr & FAT_ATTR_DIRECTORY)) ||
				    (!want_dir && (ent->attr & FAT_ATTR_DIRECTORY)))
					return -1;

				*out_cluster = ((uint32_t)ent->first_cluster_hi << 16) | ent->first_cluster_lo;
				*out_size = ent->file_size;
				return 0;
			}
		}

		if (fat_chain_advance(vol, &w) != 0)
			return -1; /* end-of-chain, corrupt, or cyclic - directory search ends, not found */
	}
}

int fat_open(const struct fat_volume *vol, const char *path, struct fat_file *file)
{
	uint32_t dir_cluster = vol->root_cluster;
	uint32_t cluster = 0, size = 0;
	const char *p = path;

	if (*p != '/')
		return -1;
	p++;

	for (;;) {
		char component[FAT_MAX_COMPONENT_LEN + 1];
		char packed[11];
		int len = 0, is_last;

		while (*p && *p != '/') {
			if (len >= FAT_MAX_COMPONENT_LEN)
				return -1; /* too long for this driver's own short-name-only namespace */
			component[len++] = *p++;
		}
		component[len] = '\0';
		if (len == 0)
			return -1; /* empty component (leading/double slash) - malformed path */

		is_last = (*p == '\0');
		if (*p == '/')
			p++;

		fat_pack_name(component, packed);

		if (fat_find_in_dir(vol, dir_cluster, packed, !is_last, &cluster, &size) != 0)
			return -1;

		if (is_last) {
			file->first_cluster = cluster;
			file->size = size;
			return 0;
		}

		dir_cluster = cluster;
		if (!fat_cluster_in_range(vol, dir_cluster))
			return -1; /* an empty (cluster 0) or out-of-range directory can't hold the rest of the path */
	}
}

int fat_read_range(const struct fat_volume *vol, const struct fat_file *file, uint32_t offset,
                    uint32_t length, uint32_t dst_phys, fat_progress_fn progress, void *progress_ctx)
{
	struct fat_chain_walk w;
	uint32_t cluster_bytes, skip_clusters, i;
	uint32_t remaining, sector_off, sector_in_cluster;

	if (vol->sectors_per_cluster == 0 || vol->bytes_per_sector == 0)
		return -1;
	cluster_bytes = vol->bytes_per_sector * vol->sectors_per_cluster;

	if (offset > file->size || length > file->size - offset)
		return -1; /* out of range - checked here, not left to the caller */
	if (length == 0)
		return 0;

	if (!fat_cluster_in_range(vol, file->first_cluster))
		return -1;
	fat_chain_walk_init(&w, file->first_cluster);

	skip_clusters = offset / cluster_bytes;
	for (i = 0; i < skip_clusters; i++) {
		if (fat_chain_advance(vol, &w) != 0)
			return -1; /* chain ended, corrupt, or cyclic before reaching the requested offset */
	}

	sector_in_cluster = (offset % cluster_bytes) / vol->bytes_per_sector;
	sector_off = offset % vol->bytes_per_sector;
	remaining = length;

	while (remaining > 0) {
		uint64_t run_lba = cluster_to_lba(vol, w.cluster) + sector_in_cluster;
		/* Sectors left in the CURRENT cluster - always contiguous with
		 * each other by construction (cluster_to_lba() is linear), no
		 * lookahead needed for this part of the run. */
		uint32_t run_sectors = vol->sectors_per_cluster - sector_in_cluster;
		uint32_t probe_cluster = w.cluster;
		uint32_t sectors_needed, sector_pos_in_cluster, full_clusters_consumed;
		uint32_t max_bytes_this_run, bytes_this_run;

		/*
		 * Extend the run across cluster boundaries, but ONLY by whole
		 * additional clusters (never a partial one beyond the first) -
		 * keeps the "how many clusters did sectors_needed actually
		 * consume" bookkeeping below unambiguous. A plain peek
		 * (fat_get_next_cluster(), not fat_chain_advance()) - safe
		 * without Floyd's cycle protection here: this loop's own
		 * FAT_IO_BATCH_SECTORS cap already bounds it to a small,
		 * fixed number of iterations regardless of what the chain
		 * actually looks like, cyclic or not.
		 */
		while (run_sectors + vol->sectors_per_cluster <= FAT_IO_BATCH_SECTORS) {
			uint32_t next;

			if (fat_get_next_cluster(vol, probe_cluster, &next) != 0)
				break;
			if (next != probe_cluster + 1 || !fat_cluster_in_range(vol, next))
				break; /* not contiguous on disk (or end of chain) - stop extending */
			run_sectors += vol->sectors_per_cluster;
			probe_cluster = next;
		}
		if (run_sectors > FAT_IO_BATCH_SECTORS)
			run_sectors = FAT_IO_BATCH_SECTORS; /* the current cluster's own remaining sectors alone already exceed the cap (a large cluster size) - clamp; the while loop above never added anything in this case */

		max_bytes_this_run = run_sectors * vol->bytes_per_sector - sector_off;
		bytes_this_run = remaining < max_bytes_this_run ? remaining : max_bytes_this_run;
		sectors_needed = (sector_off + bytes_this_run + vol->bytes_per_sector - 1) / vol->bytes_per_sector;

		if (fat_read_sectors(vol, run_lba, sectors_needed, g_fat_io_buf) != 0)
			return -1;
		unreal_copy(dst_phys, g_fat_io_buf + sector_off, bytes_this_run);
		if (progress)
			progress(progress_ctx, bytes_this_run);

		dst_phys += bytes_this_run;
		remaining -= bytes_this_run;

		/* Advance the REAL (cycle-protected) walk by however many
		 * whole clusters this run actually consumed - re-derives that
		 * from sectors_needed rather than trusting the lookahead
		 * above directly, so a corrupt/hostile FAT entry the peek
		 * happened to accept still goes through fat_chain_advance()'s
		 * own full validation before this function ever trusts it. */
		sector_pos_in_cluster = sector_in_cluster + sectors_needed;
		full_clusters_consumed = sector_pos_in_cluster / vol->sectors_per_cluster;
		sector_in_cluster = sector_pos_in_cluster % vol->sectors_per_cluster;
		sector_off = 0;

		for (i = 0; i < full_clusters_consumed && remaining > 0; i++) {
			if (fat_chain_advance(vol, &w) != 0)
				return -1; /* chain ended, corrupt, or cyclic before delivering all of `length` */
		}
	}

	return 0;
}
