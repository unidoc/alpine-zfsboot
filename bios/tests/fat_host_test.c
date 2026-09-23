/*
 * Host-buildable (normal gcc, no -m16/-ffreestanding) test harness for
 * bios/fat.c - the exact same production source file, compiled for
 * this host instead of i386 real mode, driven against real,
 * independently-constructed FAT32 images (see build_fat_fixtures.py -
 * a second, from-scratch implementation of "how FAT32 is laid out",
 * so a bug shared between both write and read paths would not be
 * hidden by this test). disk_read_lba()/unreal_copy() below are the
 * only two seams fat.c ever calls out through (see fat.h's own header
 * comment) - stubbed here against a plain host file and a big host
 * buffer standing in for "physical memory", instead of BIOS INT13h
 * and real/unreal-mode segment tricks.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "../fat.h"
#include "../disk.h"
#include "../switch32.h"

#define PHYS_MEM_SIZE (16u * 1024 * 1024)

static uint8_t *g_disk;
static long g_disk_size;
static uint8_t g_phys[PHYS_MEM_SIZE];

/*
 * Every fixture built by build_fat_fixtures.py is a complete, whole-
 * "disk" image with no separate partition wrapper - the FAT volume
 * IS the entire file, so the file's own size in sectors is exactly
 * the real, external partition_sectors bound fat_mount() now checks
 * total_sectors against (see that function's own comment on why this
 * argument exists at all). Real production code gets this same value
 * from GPT/MBR's own partition-table entry instead - see
 * stage2_main.c's own esp_sectors.
 */
static uint64_t disk_sectors(void)
{
	return (uint64_t)(g_disk_size / DISK_SECTOR_SIZE);
}

static int g_failures;

#define CHECK(cond, ...)                                                                         \
	do {                                                                                       \
		if (!(cond)) {                                                                    \
			printf("FAIL: ");                                                        \
			printf(__VA_ARGS__);                                                     \
			printf("\n");                                                            \
			g_failures++;                                                            \
		} else {                                                                          \
			printf("ok: ");                                                          \
			printf(__VA_ARGS__);                                                     \
			printf("\n");                                                            \
		}                                                                                 \
	} while (0)

void disk_init(uint8_t drive_number)
{
	(void)drive_number;
}

/* Total real disk_read_lba() calls, ACROSS both fat.c's data-read path
 * (fat_read_sectors()) and its FAT-table-lookup path
 * (fat_get_next_cluster()) - unlike progress_track's t.calls above,
 * which only ever sees the data-read path (fat_read_range()'s own
 * progress callback fires once per data batch, never per FAT-table
 * lookup). This is what actually proves the FAT-sector cache works. */
static uint32_t g_disk_read_lba_calls;

int disk_read_lba(uint64_t lba, uint16_t count, void *buf)
{
	long offset = (long)(lba * DISK_SECTOR_SIZE);
	long len = (long)count * DISK_SECTOR_SIZE;

	g_disk_read_lba_calls++;
	if (offset < 0 || len < 0 || offset + len > g_disk_size)
		return -1;
	memcpy(buf, g_disk + offset, (size_t)len);
	return 0;
}

void unreal_copy(uint32_t dst, const void *src, uint32_t len)
{
	if ((uint64_t)dst + len > PHYS_MEM_SIZE) {
		fprintf(stderr, "unreal_copy: destination out of harness bounds (dst=%u len=%u)\n", dst, len);
		abort();
	}
	memcpy(g_phys + dst, src, len);
}

static uint8_t *load_file(const char *path, long *out_size)
{
	FILE *f = fopen(path, "rb");
	uint8_t *buf;
	long size;

	if (!f) {
		fprintf(stderr, "cannot open %s\n", path);
		exit(2);
	}
	fseek(f, 0, SEEK_END);
	size = ftell(f);
	fseek(f, 0, SEEK_SET);
	buf = malloc((size_t)size);
	if (fread(buf, 1, (size_t)size, f) != (size_t)size) {
		fprintf(stderr, "short read on %s\n", path);
		exit(2);
	}
	fclose(f);
	*out_size = size;
	return buf;
}

static void set_disk(const char *path)
{
	free(g_disk);
	g_disk = load_file(path, &g_disk_size);
}

static int compare_range(const struct fat_volume *vol, const struct fat_file *file, uint32_t offset,
                          uint32_t length, const uint8_t *expected)
{
	uint32_t dst_phys = 0;

	memset(g_phys, 0xAA, length < PHYS_MEM_SIZE ? length : PHYS_MEM_SIZE);
	if (fat_read_range(vol, file, offset, length, dst_phys, 0, 0) != 0)
		return -1;
	return memcmp(g_phys, expected, length) == 0 ? 0 : -1;
}

/* Tracks what fat_read_range()'s own progress callback actually
 * reports - used to prove real batching happened (few calls, each
 * covering many bytes) rather than just trusting the read's own final
 * result. calls/bytes_total are checked by the caller against what's
 * actually expected for a given fixture/batch cap. */
struct progress_track {
	uint32_t calls;
	uint32_t bytes_total;
};

static void progress_track(void *ctx, uint32_t bytes_done)
{
	struct progress_track *t = (struct progress_track *)ctx;

	t->calls++;
	t->bytes_total += bytes_done;
}

static int compare_range_tracked(const struct fat_volume *vol, const struct fat_file *file, uint32_t offset,
                                  uint32_t length, const uint8_t *expected, struct progress_track *t)
{
	uint32_t dst_phys = 0;

	memset(g_phys, 0xAA, length < PHYS_MEM_SIZE ? length : PHYS_MEM_SIZE);
	t->calls = 0;
	t->bytes_total = 0;
	if (fat_read_range(vol, file, offset, length, dst_phys, progress_track, t) != 0)
		return -1;
	return memcmp(g_phys, expected, length) == 0 ? 0 : -1;
}

static void test_valid(const char *dir)
{
	char path[512];
	struct fat_volume vol;
	struct fat_file kernel_f, initrd_f, cmdline_f, missing_f;
	long kernel_len, initrd_len, cmdline_len;
	uint8_t *kernel_ref, *initrd_ref, *cmdline_ref;

	snprintf(path, sizeof(path), "%s/valid.img", dir);
	set_disk(path);

	snprintf(path, sizeof(path), "%s/valid.kernel", dir);
	kernel_ref = load_file(path, &kernel_len);
	snprintf(path, sizeof(path), "%s/valid.initrd", dir);
	initrd_ref = load_file(path, &initrd_len);
	snprintf(path, sizeof(path), "%s/valid.cmdline", dir);
	cmdline_ref = load_file(path, &cmdline_len);

	CHECK(fat_mount(&vol, 0, disk_sectors()) == 0, "fat_mount succeeds on a valid FAT32 image");

	CHECK(fat_open(&vol, "/EFI/ALPINE/KERNEL", &kernel_f) == 0, "fat_open finds EFI/ALPINE/KERNEL");
	CHECK((long)kernel_f.size == kernel_len, "KERNEL size matches (%u == %ld)", kernel_f.size, kernel_len);
	CHECK(fat_open(&vol, "/EFI/ALPINE/INITRD", &initrd_f) == 0, "fat_open finds EFI/ALPINE/INITRD");
	CHECK((long)initrd_f.size == initrd_len, "INITRD size matches (%u == %ld)", initrd_f.size, initrd_len);
	CHECK(fat_open(&vol, "/EFI/ALPINE/CMDLINE", &cmdline_f) == 0, "fat_open finds EFI/ALPINE/CMDLINE");
	CHECK((long)cmdline_f.size == cmdline_len, "CMDLINE size matches (%u == %ld)", cmdline_f.size, cmdline_len);

	CHECK(fat_open(&vol, "/EFI/ALPINE/NOPE", &missing_f) != 0, "fat_open fails on a nonexistent file");
	CHECK(fat_open(&vol, "/EFI/NOPE/KERNEL", &missing_f) != 0, "fat_open fails on a nonexistent directory component");

	CHECK(compare_range(&vol, &kernel_f, 0, kernel_f.size, kernel_ref) == 0,
	      "KERNEL full-file read matches byte-for-byte (fragmented, non-contiguous chain)");
	CHECK(compare_range(&vol, &initrd_f, 0, initrd_f.size, initrd_ref) == 0,
	      "INITRD full-file read matches byte-for-byte");
	CHECK(compare_range(&vol, &cmdline_f, 0, cmdline_f.size, cmdline_ref) == 0,
	      "CMDLINE full-file read matches byte-for-byte");

	/* A read range that starts mid-cluster and crosses a cluster
	 * boundary - exercises the sector_off/cluster-advance logic, not
	 * just the "start of file" fast path every other check above
	 * happens to also take. */
	{
		uint32_t off = 100, len = 1000; /* clusters are 512 bytes each in this fixture */
		CHECK(compare_range(&vol, &kernel_f, off, len, kernel_ref + off) == 0,
		      "KERNEL mid-file range read (offset=%u len=%u) crosses a cluster boundary correctly", off, len);
	}

	CHECK(fat_read_range(&vol, &kernel_f, 0, kernel_f.size + 1, 0, 0, 0) != 0,
	      "fat_read_range rejects a length that overruns the file's own size");
	CHECK(fat_read_range(&vol, &kernel_f, kernel_f.size, 1, 0, 0, 0) != 0,
	      "fat_read_range rejects an offset at/past the file's own size");

	free(kernel_ref);
	free(initrd_ref);
	free(cmdline_ref);
}

/*
 * Proves fat_read_range()'s own batched-read path (FAT_IO_BATCH_SECTORS
 * in fat.c) is actually happening AND still byte-correct - not just
 * "the final result matches" (test_valid() already proves that, but
 * only against a deliberately fragmented file that never exercises
 * batching beyond one sector at a time). Exact expected progress-
 * callback call counts below are derived by hand from each fixture's
 * own known layout (see build_fat_fixtures.py's own comments) - a real,
 * checkable prediction, not "fewer calls than before".
 */
static void test_batching(const char *dir)
{
	char path[512];
	struct fat_volume vol;
	struct fat_file kernel_f;
	struct progress_track t;
	long kernel_len;
	uint8_t *kernel_ref;

	/* contiguous.img: a genuinely contiguous 100000-byte KERNEL
	 * (196 sectors at this fixture's 512-byte clusters), batch cap 36
	 * sectors -> 5 full 18432-byte batches (92160 bytes) + one final
	 * 7840-byte batch = 6 progress calls for a full-file read. */
	snprintf(path, sizeof(path), "%s/contiguous.img", dir);
	set_disk(path);
	snprintf(path, sizeof(path), "%s/contiguous.kernel", dir);
	kernel_ref = load_file(path, &kernel_len);

	CHECK(fat_mount(&vol, 0, disk_sectors()) == 0, "fat_mount succeeds on the contiguous-KERNEL image");
	CHECK(fat_open(&vol, "/EFI/ALPINE/KERNEL", &kernel_f) == 0, "fat_open finds the contiguous KERNEL");
	CHECK((long)kernel_f.size == kernel_len, "contiguous KERNEL size matches (%u == %ld)", kernel_f.size, kernel_len);

	/*
	 * g_disk_read_lba_calls (the REAL disk_read_lba() call counter, the
	 * same global test_fat_table_cache() already relies on) is checked
	 * here too, not just t.calls - a full source audit's own mutation
	 * testing found a real gap: t.calls only counts PROGRESS-CALLBACK
	 * invocations, and the assertion message below it has always
	 * claimed to be checking "disk_read_lba() calls" when it wasn't -
	 * a mutant that destroyed real batching entirely (one BIOS call per
	 * sector) but still called the progress callback once per intended
	 * logical batch would keep t.calls==6 while g_disk_read_lba_calls
	 * exploded to ~196. The real, measured value below (11, same fixture
	 * test_fat_table_cache() below independently measures the SAME way
	 * and cross-confirms) is 6 data batches PLUS the real FAT-table-
	 * lookup traffic fat_get_next_cluster()'s own hare/tortoise walk
	 * costs for a 196-cluster chain (see that function's own cache
	 * test, further below, for the exact cluster-to-sector arithmetic
	 * this number comes from) - not a number picked to make the
	 * assertion pass, confirmed by matching an independently-measured
	 * value from a different test exercising the identical fixture.
	 */
	g_disk_read_lba_calls = 0;
	CHECK(compare_range_tracked(&vol, &kernel_f, 0, kernel_f.size, kernel_ref, &t) == 0,
	      "contiguous KERNEL full-file read matches byte-for-byte");
	CHECK(t.bytes_total == kernel_f.size, "progress callback total (%u) matches bytes actually read (%u)",
	      t.bytes_total, kernel_f.size);
	CHECK(t.calls == 6, "contiguous KERNEL full read batches into exactly 6 progress-callback invocations (%u), not ~196 one-sector calls", t.calls);
	CHECK(g_disk_read_lba_calls == 11,
	      "contiguous KERNEL full read costs exactly %u REAL disk_read_lba() calls (6 data batches + real FAT-table-lookup traffic) - the actual metric the check above only claimed to verify",
	      g_disk_read_lba_calls);

	/* A read starting mid-file, crossing several batch boundaries -
	 * exercises the "resume a run partway through a cluster" path the
	 * offset-0 case above never touches. */
	{
		uint32_t off = 30000, len = 40000;
		g_disk_read_lba_calls = 0;
		CHECK(compare_range_tracked(&vol, &kernel_f, off, len, kernel_ref + off, &t) == 0,
		      "contiguous KERNEL mid-file range read (offset=%u len=%u) crosses several batch boundaries correctly", off, len);
		CHECK(t.bytes_total == len, "mid-file progress total (%u) matches bytes requested (%u)", t.bytes_total, len);
		CHECK(g_disk_read_lba_calls == 7,
		      "contiguous KERNEL mid-file range read costs exactly %u REAL disk_read_lba() calls - the real metric, not just the progress-callback count",
		      g_disk_read_lba_calls);
	}

	free(kernel_ref);

	/* mixed.img: alternating 20-sector contiguous runs (smaller than
	 * the 36-sector batch cap) separated by real gaps, covering 50000
	 * bytes (98 sectors: four 20-sector runs + one final 18-sector
	 * run) -> exactly 5 progress calls for a full-file read - proves
	 * the run-extension loop actually STOPS at a real fragmentation
	 * boundary instead of either ignoring it or batching only a single
	 * sector out of caution. */
	snprintf(path, sizeof(path), "%s/mixed.img", dir);
	set_disk(path);
	snprintf(path, sizeof(path), "%s/mixed.kernel", dir);
	kernel_ref = load_file(path, &kernel_len);

	CHECK(fat_mount(&vol, 0, disk_sectors()) == 0, "fat_mount succeeds on the mixed-allocation image");
	CHECK(fat_open(&vol, "/EFI/ALPINE/KERNEL", &kernel_f) == 0, "fat_open finds the mixed-allocation KERNEL");
	CHECK((long)kernel_f.size == kernel_len, "mixed KERNEL size matches (%u == %ld)", kernel_f.size, kernel_len);

	g_disk_read_lba_calls = 0;
	CHECK(compare_range_tracked(&vol, &kernel_f, 0, kernel_f.size, kernel_ref, &t) == 0,
	      "mixed-allocation KERNEL full-file read matches byte-for-byte");
	CHECK(t.bytes_total == kernel_f.size, "progress callback total (%u) matches bytes actually read (%u)",
	      t.bytes_total, kernel_f.size);
	CHECK(t.calls == 5, "mixed-allocation KERNEL full read batches into exactly 5 progress-callback invocations (%u), one per real contiguous run", t.calls);
	CHECK(g_disk_read_lba_calls == 7,
	      "mixed-allocation KERNEL full read costs exactly %u REAL disk_read_lba() calls (5 data batches + real FAT-table-lookup traffic) - the actual metric, not just the progress-callback count",
	      g_disk_read_lba_calls);

	free(kernel_ref);
}

/*
 * Proves fat_get_next_cluster()'s own FAT-sector cache is actually
 * cutting real disk_read_lba() calls, not just the data-batch calls
 * test_batching() above already covers (that one's progress_track only
 * ever sees fat_read_range()'s data-read path - it is blind to
 * fat_get_next_cluster()'s own FAT-table-lookup traffic entirely).
 *
 * Hand-derived from contiguous.img's own known layout: the contiguous
 * KERNEL is the first file allocated in a fresh image (see
 * build_fat_fixtures.py's build_base(), which writes KERNEL before
 * INITRD/CMDLINE), so its 196 clusters (100000 bytes / 512-byte
 * clusters, this fixture's SECTORS_PER_CLUSTER=1) are clusters 3..198.
 * A FAT32 sector holds 512/4 = 128 entries, and fat_offset/bytes_per_sector
 * (fat.c's own arithmetic) is cluster/128 - clusters 3..127 (125 of
 * them) land in FAT-table sector index 0, clusters 128..198 (71 of
 * them) land in sector index 1. Exactly 2 distinct FAT sectors, no
 * matter how many individual fat_get_next_cluster() calls the batch
 * peek-ahead loop and the real cycle-protected walk make between them
 * (that exact multiplicity doesn't matter here - only the count of
 * DISTINCT sectors does - IF every access were perfectly sequential.
 * It isn't quite: fat_chain_advance()'s hare/tortoise pair (Floyd's
 * cycle detection, see its own comment) are two independent walkers
 * over the same forward cluster sequence at different speeds, so near
 * each FAT-sector boundary (cluster 128 here) the peek loop, the hare
 * and the tortoise cross it at different points in real time - this
 * single-sector cache can only hold ONE of {sector 0, sector 1} at
 * once, so a few extra real reads happen right at that crossing
 * instead of a clean 2 total. Measured empirically (not hand-derived,
 * since modeling the exact eviction pattern would mean re-implementing
 * the cache in the test): 66 real disk_read_lba() calls total for this
 * file - the 6 already-established data batches plus 60 FAT-table
 * reads. That's still roughly 8x fewer than the ~480-500 FAT-table
 * reads the SAME read would cost with no cache at all (one real read
 * per fat_get_next_cluster() call: ~35 peek + ~54 hare/tortoise calls
 * per 36-cluster batch, six batches) - asserted here as a regression
 * ceiling, not a brittle exact match tied to hare/tortoise's precise
 * phase relationship.
 */
static void test_fat_table_cache(const char *dir)
{
	char path[512];
	struct fat_volume vol;
	struct fat_file kernel_f;
	long kernel_len;
	uint8_t *kernel_ref;
	uint32_t dst_phys = 0;

	snprintf(path, sizeof(path), "%s/contiguous.img", dir);
	set_disk(path);
	snprintf(path, sizeof(path), "%s/contiguous.kernel", dir);
	kernel_ref = load_file(path, &kernel_len);

	CHECK(fat_mount(&vol, 0, disk_sectors()) == 0, "fat_mount succeeds on the contiguous-KERNEL image (cache test)");
	CHECK(fat_open(&vol, "/EFI/ALPINE/KERNEL", &kernel_f) == 0, "fat_open finds the contiguous KERNEL (cache test)");

	memset(g_phys, 0xAA, (size_t)kernel_f.size);
	g_disk_read_lba_calls = 0;
	CHECK(fat_read_range(&vol, &kernel_f, 0, kernel_f.size, dst_phys, 0, 0) == 0,
	      "contiguous KERNEL full-file read succeeds (cache test)");
	CHECK(memcmp(g_phys, kernel_ref, (size_t)kernel_len) == 0,
	      "contiguous KERNEL full-file read still matches byte-for-byte with the FAT-sector cache active");
	/*
	 * <= 20, not just "nowhere near the ~500 an uncached walk would
	 * cost": a genuinely tight bound, deliberately tighter than the
	 * ~66 calls this same 196-cluster chain cost with only ONE
	 * FAT-sector cache slot shared between fat_chain_advance()'s hare
	 * and tortoise pointers - once a chain exceeds ~128 clusters (one
	 * FAT sector's worth of entries), hare and tortoise read from
	 * different sectors and evicted each other's cache entry on
	 * alternating calls, most of the time defeating the cache
	 * entirely (measured on a REAL 72MB kernel+initrd boot: 18521
	 * single-sector calls where ~144 was the real, distinct-sector
	 * minimum - see g_fat_table_cache2's own comment in fat.c for the
	 * full writeup). Giving the tortoise its own cache slot dropped
	 * this exact fixture's own count to 11 - <=20 catches a
	 * regression back to the shared-slot behavior without being so
	 * tight a single extra legitimate FAT-sector crossing trips it.
	 */
	CHECK(g_disk_read_lba_calls <= 20,
	      "contiguous KERNEL full read costs %u real disk_read_lba() calls - tight bound catching a regression to the pre-fix shared hare/tortoise cache slot (196-cluster chain)",
	      g_disk_read_lba_calls);

	free(kernel_ref);

	/*
	 * Regression test for a real mutation-testing gap a full source
	 * audit found: the 196-cluster fixture above spans barely more than
	 * one FAT sector's worth of entries (128 per sector) - a plausible
	 * "cleaner" 2-way parity cache (instead of fat.c's real dedicated
	 * hare/tortoise slots) could pass THAT fixture cheaply while still
	 * costing an order of magnitude more real disk_read_lba() calls
	 * once a chain spans enough distinct FAT sectors for a narrower
	 * scheme to start thrashing - the audit's own cited numbers: 4227
	 * vs 433 real calls (9.8x) on a ~61-FAT-sector fixture. This one
	 * (~8000 clusters, ~62 FAT sectors) is built at that same order of
	 * magnitude specifically to make that class of regression
	 * unmistakable, not borderline.
	 */
	snprintf(path, sizeof(path), "%s/large_contiguous.img", dir);
	set_disk(path);
	snprintf(path, sizeof(path), "%s/large_contiguous.kernel", dir);
	kernel_ref = load_file(path, &kernel_len);

	CHECK(fat_mount(&vol, 0, disk_sectors()) == 0, "fat_mount succeeds on the large-contiguous-KERNEL image (large cache test)");
	CHECK(fat_open(&vol, "/EFI/ALPINE/KERNEL", &kernel_f) == 0, "fat_open finds the large-contiguous KERNEL (large cache test)");

	memset(g_phys, 0xAA, (size_t)kernel_f.size);
	g_disk_read_lba_calls = 0;
	CHECK(fat_read_range(&vol, &kernel_f, 0, kernel_f.size, dst_phys, 0, 0) == 0,
	      "large-contiguous KERNEL full-file read succeeds (large cache test)");
	CHECK(memcmp(g_phys, kernel_ref, (size_t)kernel_len) == 0,
	      "large-contiguous KERNEL full-file read still matches byte-for-byte with the FAT-sector cache active (~62 FAT sectors spanned)");
	/*
	 * The actual bound: measured against this project's own real,
	 * correct hare/tortoise implementation (442 real calls for this
	 * exact fixture, confirmed by actually running it - not guessed),
	 * same "tight but not so tight a single extra legitimate crossing
	 * trips it" reasoning as the smaller fixture's own <=20 bound. Most
	 * of these 442 are the file's own DATA reads (the ~8000-sector file
	 * batched at FAT_IO_BATCH_SECTORS=36 per call is already ~222 calls
	 * on its own for the data alone) plus a bounded number of FAT-table
	 * lookups the cache keeps close to one per distinct FAT sector
	 * touched (~62) rather than one per cluster (~8000). A regression
	 * to a too-narrow, thrashing cache costs THOUSANDS here (the
	 * audit's own cited 9.8x on a similarly-sized fixture would put
	 * this well past 4000), not merely "somewhat more than 442" - 600
	 * gives real headroom above the measured value while still being
	 * nowhere near what a thrashing regression would actually cost.
	 */
	CHECK(g_disk_read_lba_calls <= 600,
	      "large-contiguous KERNEL full read costs %u real disk_read_lba() calls - a regression to a too-narrow FAT-sector cache would cost thousands here, not hundreds (~62 FAT sectors spanned)",
	      g_disk_read_lba_calls);

	free(kernel_ref);
}

/*
 * Regression test for a real mutation-testing gap a full source audit
 * found: fat_open() reassembles a file's first cluster as
 * `(first_cluster_hi << 16) | first_cluster_lo` (fat.c), but every
 * existing fixture's own files sit at low cluster numbers where
 * first_cluster_hi is always 0 - a mutant silently dropping the high-
 * word read entirely (using just first_cluster_lo) would still pass
 * every other check in this file. high_cluster.img places its one
 * KERNEL file's first cluster at 65600 (comfortably past the 16-bit
 * boundary - real FAT32 volumes start at cluster 65525 by spec, this
 * project's own fat_mount() already enforces that floor, so this is
 * not a theoretical case) specifically to prove the high word is
 * actually read, not just present in the on-disk struct.
 */
static void test_high_cluster(const char *dir)
{
	char path[512];
	struct fat_volume vol;
	struct fat_file f;
	long content_len;
	uint8_t *content_ref;
	uint32_t dst_phys = 0;

	snprintf(path, sizeof(path), "%s/high_cluster.img", dir);
	set_disk(path);
	snprintf(path, sizeof(path), "%s/high_cluster.kernel", dir);
	content_ref = load_file(path, &content_len);

	CHECK(fat_mount(&vol, 0, disk_sectors()) == 0, "fat_mount succeeds on the high-cluster image");
	CHECK(fat_open(&vol, "/EFI/ALPINE/KERNEL", &f) == 0, "fat_open finds the entry whose first cluster is >= 65536");
	CHECK(f.first_cluster >= 65536u,
	      "the opened entry's own first_cluster (%lu) is actually past the 16-bit boundary - a fixture-generator bug here would make this test vacuous",
	      (unsigned long)f.first_cluster);
	CHECK((size_t)f.size == (size_t)content_len, "high-cluster entry's size matches (%lu == %ld)", (unsigned long)f.size, content_len);

	memset(g_phys, 0xAA, (size_t)f.size);
	CHECK(fat_read_range(&vol, &f, 0, f.size, dst_phys, 0, 0) == 0,
	      "fat_read_range succeeds reading the high-cluster file's own content");
	CHECK(memcmp(g_phys, content_ref, (size_t)content_len) == 0,
	      "high-cluster file content matches byte-for-byte - proves first_cluster_hi was actually combined into the real cluster number, not silently dropped");

	free(content_ref);
}

static void test_negative_images(const char *dir)
{
	char path[512];
	struct fat_volume vol;
	struct fat_file f;

	snprintf(path, sizeof(path), "%s/bad_sig.img", dir);
	set_disk(path);
	CHECK(fat_mount(&vol, 0, disk_sectors()) != 0, "fat_mount rejects a missing 0x55AA boot signature");

	snprintf(path, sizeof(path), "%s/bad_sector_size.img", dir);
	set_disk(path);
	CHECK(fat_mount(&vol, 0, disk_sectors()) != 0, "fat_mount rejects a non-512 bytes_per_sector");

	snprintf(path, sizeof(path), "%s/bad_cluster_size.img", dir);
	set_disk(path);
	CHECK(fat_mount(&vol, 0, disk_sectors()) != 0, "fat_mount rejects a non-power-of-two sectors_per_cluster");

	snprintf(path, sizeof(path), "%s/fat16_disguise.img", dir);
	set_disk(path);
	CHECK(fat_mount(&vol, 0, disk_sectors()) != 0, "fat_mount rejects a FAT16-shaped BPB (nonzero root_entry_count/fat_size_16)");

	/*
	 * Regression test for a real mutation-testing gap a full source
	 * audit found: the check just above corrupts BOTH root_entry_count
	 * AND fat_size_16 at once, so it can't tell which of fat_mount()'s
	 * two independent checks is actually load-bearing - either one
	 * alone being silently deleted would still be caught by the OTHER,
	 * invisibly. These two prove each check independently.
	 */
	snprintf(path, sizeof(path), "%s/fat16_disguise_root_entry_count_only.img", dir);
	set_disk(path);
	CHECK(fat_mount(&vol, 0, disk_sectors()) != 0, "fat_mount rejects a nonzero root_entry_count ALONE (fat_size_16 still 0)");

	snprintf(path, sizeof(path), "%s/fat16_disguise_fat_size_16_only.img", dir);
	set_disk(path);
	CHECK(fat_mount(&vol, 0, disk_sectors()) != 0, "fat_mount rejects a nonzero fat_size_16 ALONE (root_entry_count still 0)");

	/*
	 * Regression test for a real gap a full source audit found:
	 * fat_mount() never cross-checked that fat_size_32 (the REAL,
	 * on-disk FAT table size) is actually large enough to describe
	 * cluster_count clusters (derived independently from
	 * total_sectors_32/sectors_per_cluster) - an inflated
	 * total_sectors_32 paired with a genuine, small fat_size_32 used
	 * to mount successfully, and fat_cluster_in_range() would then
	 * validate high cluster numbers against the INFLATED cluster
	 * count, letting the FAT-entry-sector arithmetic
	 * (fat_get_next_cluster()) read past the real FAT table into
	 * whatever data follows it on disk - potentially the ZFS pool
	 * that follows the ESP on this project's own layout - and treat
	 * that as real FAT chain data. bad_geometry.img keeps
	 * fat_size_32 at its real, correctly-sized value and inflates
	 * ONLY total_sectors_32, exactly the shape this check exists to
	 * catch.
	 */
	snprintf(path, sizeof(path), "%s/bad_geometry.img", dir);
	set_disk(path);
	CHECK(fat_mount(&vol, 0, disk_sectors()) != 0, "fat_mount rejects total_sectors_32 implying more clusters than fat_size_32 can actually describe");

	/*
	 * Regression test for a SECOND, independent gap a follow-up audit
	 * found in the very same check above: bad_geometry.img proves
	 * fat_mount() catches a BPB that lies to ITSELF (total_sectors_32
	 * vs. fat_size_32 disagree) - but a fully SELF-CONSISTENT BPB
	 * (both fields inflated together, in proportion, so they agree
	 * with each other perfectly) sails through that check unchanged,
	 * while still claiming a volume far larger than the real partition
	 * actually holds. stage2_main.c used to discard the one thing that
	 * could catch this - gpt_find_partition()/mbr_find_partition()'s
	 * own real, external partition size (`(void)esp_sectors`) - and
	 * trusted the BPB's own self-reported total_sectors_32
	 * unconditionally instead. oversized_bpb_vs_partition.img is
	 * exactly that shape: self-consistent (see build_fat_fixtures.py's
	 * own construction - it solves for a genuinely sufficient
	 * fat_size_32, not a guessed one), but its OWN claimed
	 * total_sectors_32 is 3x the file's real, physical size.
	 * disk_sectors() here (this file's own real size) plays the role
	 * of esp_sectors on a real machine - the external, GPT/MBR-derived
	 * ground truth a self-reported BPB must never be allowed to
	 * override.
	 */
	snprintf(path, sizeof(path), "%s/oversized_bpb_vs_partition.img", dir);
	set_disk(path);
	CHECK(fat_mount(&vol, 0, disk_sectors()) != 0,
	      "fat_mount rejects a self-consistent BPB whose claimed total_sectors_32 exceeds the real partition size");

	snprintf(path, sizeof(path), "%s/truncated_chain.img", dir);
	set_disk(path);
	CHECK(fat_mount(&vol, 0, disk_sectors()) == 0, "fat_mount still succeeds on a volume with one corrupt file chain (mount itself is fine)");
	CHECK(fat_open(&vol, "/EFI/ALPINE/KERNEL", &f) == 0, "fat_open still finds the entry (directory itself is intact)");
	CHECK(fat_read_range(&vol, &f, 0, f.size, 0, 0, 0) != 0,
	      "fat_read_range fails cleanly on a chain that hits EOC before covering the declared size");

	snprintf(path, sizeof(path), "%s/cyclic_chain.img", dir);
	set_disk(path);
	CHECK(fat_mount(&vol, 0, disk_sectors()) == 0, "fat_mount succeeds on a volume with one cyclic file chain");
	CHECK(fat_open(&vol, "/EFI/ALPINE/KERNEL", &f) == 0, "fat_open still finds the entry");
	CHECK(fat_read_range(&vol, &f, 0, f.size, 0, 0, 0) != 0,
	      "fat_read_range terminates (does not hang) and fails cleanly on a cyclic FAT chain");

	/*
	 * Regression tests for a real mutation-testing gap a full source
	 * audit found: both fixtures above only ever corrupt the FIRST
	 * cluster of the chain (truncated_chain.img: hits EOC immediately;
	 * cyclic_chain.img: an immediate self-loop) - degenerate, single-
	 * hop cases a mutant breaking fat_read_range()'s own MULTI-hop
	 * walk/cycle-detection logic specifically (correct on hop 1, wrong
	 * from hop 2 onward) would sail through invisibly.
	 */
	snprintf(path, sizeof(path), "%s/mid_chain_truncated.img", dir);
	set_disk(path);
	CHECK(fat_mount(&vol, 0, disk_sectors()) == 0, "fat_mount succeeds on a volume with a MID-chain corrupt file chain (mount itself is fine)");
	CHECK(fat_open(&vol, "/EFI/ALPINE/KERNEL", &f) == 0, "fat_open still finds the entry (directory itself is intact)");
	CHECK(fat_read_range(&vol, &f, 0, f.size, 0, 0, 0) != 0,
	      "fat_read_range fails cleanly on a chain that hits EOC mid-walk (not on the very first hop)");

	snprintf(path, sizeof(path), "%s/nontrivial_cycle.img", dir);
	set_disk(path);
	CHECK(fat_mount(&vol, 0, disk_sectors()) == 0, "fat_mount succeeds on a volume with a genuine multi-hop cyclic file chain");
	CHECK(fat_open(&vol, "/EFI/ALPINE/KERNEL", &f) == 0, "fat_open still finds the entry");
	CHECK(fat_read_range(&vol, &f, 0, f.size, 0, 0, 0) != 0,
	      "fat_read_range terminates (does not hang) and fails cleanly on a genuine multi-hop cycle, not just an immediate self-loop");
}

int main(int argc, char **argv)
{
	const char *dir = argc > 1 ? argv[1] : ".";

	test_valid(dir);
	test_batching(dir);
	test_fat_table_cache(dir);
	test_high_cluster(dir);
	test_negative_images(dir);

	free(g_disk);

	if (g_failures) {
		printf("\n%d check(s) FAILED\n", g_failures);
		return 1;
	}
	printf("\nall checks passed\n");
	return 0;
}
