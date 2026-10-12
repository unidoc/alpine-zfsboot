/*
 * disk_policy_host_test.c - runs disk_policy.c's real disk_read_lba() against a
 * fake INT 13h on the host. What it pins down: a BIOS that takes 64 sectors sees
 * exactly the calls it always saw; a BIOS that rejects large transfers is
 * worked around by shrinking the chunk (and the data that comes back is still
 * right); a failure at one sector is final but reported with the BIOS's status
 * and the sector.
 *
 * Build/run: bios/tests/run-disk-policy-host-test.sh
 */
#include <stdio.h>
#include <string.h>

#include "disk.h"

/* ---- the fake BIOS ---- */

static unsigned max_ok;              /* transfers above this many sectors fail with fail_status */
static int fail_status;
static uint64_t bad_lba = (uint64_t)-1; /* a read touching this sector fails (persistently) */
static int transient_once;           /* the next single-sector call fails once, then works */
static int short_above;              /* transfers above this return DISK_ERR_SHORT (0 = never) */

static unsigned calls, resets, notifies, failures_seen;
static uint8_t last_attempt;
static unsigned call_counts[64];

static uint8_t disk_byte(uint64_t lba, unsigned off)
{
	return (uint8_t)((lba * 7 + off * 13 + 1) & 0xff);
}

int disk_int13_read(uint64_t lba, uint16_t count, void *buf)
{
	unsigned i, j;

	if (calls < 64)
		call_counts[calls] = count;
	calls++;
	if (count > max_ok)
		return fail_status;
	if (short_above && count > (unsigned)short_above)
		return DISK_ERR_SHORT;
	if (transient_once && count == 1) {
		transient_once = 0;
		return 0x20;
	}
	for (i = 0; i < count; i++) {
		if (lba + i == bad_lba)
			return 0x20;
		for (j = 0; j < DISK_SECTOR_SIZE; j++)
			((uint8_t *)buf)[i * DISK_SECTOR_SIZE + j] = disk_byte(lba + i, j);
	}
	return 0;
}

void disk_int13_reset(void) { resets++; }
void disk_notify_shrink(const struct disk_error *e, uint16_t new_chunk) { (void)e; (void)new_chunk; notifies++; }
void disk_notify_failure(const struct disk_error *e, uint8_t attempt) { (void)e; failures_seen++; last_attempt = attempt; }
struct disk_error g_disk_last_error;
void disk_policy_reset_for_test(void);

/* ---- helpers ---- */

static int failures;
#define CHECK(cond, ...) do { if (!(cond)) { failures++; printf("FAIL: "); printf(__VA_ARGS__); printf("\n"); } else { printf("ok: "); printf(__VA_ARGS__); printf("\n"); } } while (0)

static void reset_fake(unsigned ok, int status)
{
	max_ok = ok;
	fail_status = status;
	bad_lba = (uint64_t)-1;
	transient_once = 0;
	short_above = 0;
	calls = resets = notifies = failures_seen = 0;
	last_attempt = 0;
	memset(call_counts, 0, sizeof(call_counts));
	memset(&g_disk_last_error, 0, sizeof(g_disk_last_error));
	disk_policy_reset_for_test();
	/* The cases below were written for a 64-sector cap (the cap before it
	 * became configurable); the default cap has its own cases further down. */
	disk_set_chunk(64);
}

static int data_ok(uint64_t lba, unsigned count, const uint8_t *buf)
{
	unsigned i, j;
	for (i = 0; i < count; i++)
		for (j = 0; j < DISK_SECTOR_SIZE; j++)
			if (buf[i * DISK_SECTOR_SIZE + j] != disk_byte(lba + i, j))
				return 0;
	return 1;
}

static uint8_t buf[256 * DISK_SECTOR_SIZE];

int main(void)
{
	int rc;

	/* A BIOS that accepts everything: the calls are exactly what the FAT layer asked for, chunked at 64. */
	reset_fake(1000, 0);
	rc = disk_read_lba(5000, 200, buf);
	CHECK(rc == 0 && data_ok(5000, 200, buf), "a good BIOS: 200 sectors read correctly");
	CHECK(calls == 4 && call_counts[0] == 64 && call_counts[1] == 64 && call_counts[2] == 64 && call_counts[3] == 8,
	      "a good BIOS sees 64+64+64+8 sector calls (%u calls)", calls);
	CHECK(resets == 0 && notifies == 0, "no reset and no notice when nothing fails");
	CHECK(disk_read_lba(5000, 36, buf) == 0 && calls == 5 && call_counts[4] == 36, "a 36-sector request is still ONE call (not split)");
	CHECK(disk_read_lba(5000, 0, buf) == 0 && calls == 5, "a zero-sector request makes no call");

	/* A BIOS that rejects transfers above 16 sectors (status 0x09): 64 -> 32 -> 16, data intact. */
	reset_fake(16, 0x09);
	rc = disk_read_lba(123456, 64, buf);
	CHECK(rc == 0 && data_ok(123456, 64, buf), "BIOS limit 16: 64 sectors still read correctly");
	CHECK(call_counts[0] == 64 && call_counts[1] == 32 && call_counts[2] == 16 && calls == 3 + 3,
	      "BIOS limit 16: tried 64, 32, then 16-sector pieces (%u calls)", calls);
	CHECK(resets == 2 && notifies == 2, "BIOS limit 16: reset and told the operator once per shrink (%u/%u)", resets, notifies);
	calls = 0;
	rc = disk_read_lba(900, 64, buf);
	CHECK(rc == 0 && data_ok(900, 64, buf) && calls == 4 && call_counts[0] == 16, "the smaller chunk is kept: the next 64 sectors are 4 calls of 16");
	CHECK(g_disk_last_error.status == 0x09 && g_disk_last_error.count == 32, "the BIOS status and request that failed are recorded (status %#x, count %u)",
	      g_disk_last_error.status, g_disk_last_error.count);

	/* Every failed call reaches disk_notify_failure (what BIOS_DIAG prints), numbered per request. */
	reset_fake(16, 0x09);
	rc = disk_read_lba(5000, 64, buf);
	CHECK(rc == 0 && failures_seen == 2 && last_attempt == 2, "every failed call is reported, attempts numbered 1, 2 (%u calls, last attempt %u)",
	      failures_seen, last_attempt);

	/* A BIOS that rejects transfers above 32 sectors while the FAT layer asks for 64. */
	reset_fake(32, 0x01);
	rc = disk_read_lba(77777, 64, buf);
	CHECK(rc == 0 && data_ok(77777, 64, buf), "BIOS limit 32: 64 sectors still read correctly");
	CHECK(call_counts[0] == 64 && call_counts[1] == 32 && call_counts[2] == 32 && calls == 3,
	      "BIOS limit 32: tried 64 once, then two 32-sector calls (%u calls)", calls);
	CHECK(resets == 1 && notifies == 1, "BIOS limit 32: one reset, one notice (%u/%u)", resets, notifies);

	/* What one INT 13h call's outcome means (disk_int13_status, used by disk.c). */
	CHECK(disk_int13_status(0, 0x0000, 64, 64) == 0, "CF=0 and the DAP still says 64: success");
	CHECK(disk_int13_status(0, 0x0000, 40, 64) == DISK_ERR_SHORT, "CF=0 but the DAP says 40 of 64: rejected as a short transfer");
	CHECK(disk_int13_status(0, 0x0000, 0, 64) == DISK_ERR_SHORT, "CF=0 but the DAP says 0 of 64: rejected");
	CHECK(disk_int13_status(1, 0x0900, 64, 64) == 0x09, "CF=1: the BIOS status from AH (0x09)");
	CHECK(disk_int13_status(1, 0x0000, 64, 64) == 0xFF, "CF=1 with AH=0: still a failure (0xff)");
	CHECK(disk_int13_status(1, 0x2000, 3, 64) == 0x20, "CF=1 wins over a changed DAP count");

	/* The build default cap (DISK_MAX_CHUNK_SECTORS): a 64-sector request
	 * from the FAT layer never reaches the BIOS as one call. */
	reset_fake(1000, 0);
	disk_policy_reset_for_test();
	rc = disk_read_lba(3000, 64, buf);
	CHECK(rc == 0 && data_ok(3000, 64, buf), "default cap: 64 sectors read correctly");
	{
		unsigned i, over = 0;
		for (i = 0; i < calls && i < 64; i++)
			if (call_counts[i] > DISK_MAX_CHUNK_SECTORS)
				over++;
		CHECK(over == 0 && calls == (64 + DISK_MAX_CHUNK_SECTORS - 1) / DISK_MAX_CHUNK_SECTORS,
		      "default cap %d: no call above it, %u calls for 64 sectors", DISK_MAX_CHUNK_SECTORS, calls);
	}
	CHECK(DISK_MAX_CHUNK_SECTORS <= 32, "the build default cap is at most 32 sectors (16 KiB): %d", DISK_MAX_CHUNK_SECTORS);
	CHECK(g_disk_stats.calls == calls && g_disk_stats.largest == DISK_MAX_CHUNK_SECTORS && g_disk_stats.cap == DISK_MAX_CHUNK_SECTORS,
	      "stats: calls %u largest %u cap %u", g_disk_stats.calls, g_disk_stats.largest, g_disk_stats.cap);

	/* alpine-zfsboot.int13chunk=N -> disk_set_chunk(N). */
	reset_fake(1000, 0);
	disk_set_chunk(127);
	rc = disk_read_lba(10, 200, buf);
	CHECK(rc == 0 && data_ok(10, 200, buf) && calls == 2 && call_counts[0] == 127 && call_counts[1] == 73,
	      "cap 127 (the largest allowed): 200 sectors as 127+73 (%u calls)", calls);
	reset_fake(1000, 0);
	disk_set_chunk(8);
	disk_set_chunk(0);   /* ignored */
	disk_set_chunk(128); /* ignored */
	rc = disk_read_lba(10, 64, buf);
	CHECK(rc == 0 && calls == 8 && call_counts[0] == 8, "cap 8, then invalid 0 and 128 ignored: 64 sectors as 8 calls of 8 (%u)", calls);
	reset_fake(1000, 0);
	disk_set_chunk(1);
	rc = disk_read_lba(10, 5, buf);
	CHECK(rc == 0 && data_ok(10, 5, buf) && calls == 5, "cap 1: one sector per call");

	/* Shrinking below the configured cap still works: cap 32, BIOS takes 8. */
	reset_fake(8, 0x09);
	disk_set_chunk(32);
	rc = disk_read_lba(500, 64, buf);
	CHECK(rc == 0 && data_ok(500, 64, buf), "cap 32, BIOS limit 8: data still correct");
	CHECK(call_counts[0] == 32 && call_counts[1] == 16 && call_counts[2] == 8 && g_disk_stats.chunk == 8 && g_disk_stats.cap == 32 &&
	      g_disk_stats.largest == 8,
	      "...32 -> 16 -> 8, stats chunk %u cap %u largest %u", g_disk_stats.chunk, g_disk_stats.cap, g_disk_stats.largest);

	/* LBA -> CHS for the AH=02h fallback (16 heads, 63 sectors, 1024 cylinders). */
	{
		uint16_t c, left;
		uint8_t h, sec;
		CHECK(disk_lba_to_chs(0, 16, 63, 1024, &c, &h, &sec, &left) == 0 && c == 0 && h == 0 && sec == 1 && left == 63,
		      "CHS: LBA 0 = 0/0/1, a whole track left");
		CHECK(disk_lba_to_chs(3343, 16, 63, 1024, &c, &h, &sec, &left) == 0 && c == 3 && h == 5 && sec == 5 && left == 59,
		      "CHS: LBA 3343 = 3/5/5, 59 sectors left in the track (%u/%u/%u %u)", c, h, sec, left);
		CHECK(disk_lba_to_chs(16 * 63 * 1024 - 1, 16, 63, 1024, &c, &h, &sec, &left) == 0 && c == 1023 && h == 15 && sec == 63 && left == 1,
		      "CHS: the last addressable sector");
		CHECK(disk_lba_to_chs(16 * 63 * 1024, 16, 63, 1024, &c, &h, &sec, &left) == -1, "CHS: beyond cylinder 1023 is refused");
		CHECK(disk_lba_to_chs(16 * 63 * 100, 16, 63, 100, &c, &h, &sec, &left) == -1, "CHS: beyond the drive's own cylinders is refused");
		CHECK(disk_lba_to_chs(5, 0, 63, 100, &c, &h, &sec, &left) == -1 && disk_lba_to_chs(5, 16, 64, 100, &c, &h, &sec, &left) == -1,
		      "CHS: an impossible geometry is refused");
	}

	/* The BIOS interface limit caps every setting (CHS: sectors per track). */
	reset_fake(1000, 0);
	g_disk_bios_limit = 18;
	disk_set_chunk(64);
	CHECK(disk_get_chunk() == 18, "a cap above the BIOS limit (18) is clamped to it");
	disk_set_chunk(8);
	CHECK(disk_get_chunk() == 8, "below the limit it is used as is");
	g_disk_bios_limit = DISK_CHUNK_LIMIT;

	/* A BIOS that only takes single sectors. */
	reset_fake(1, 0x01);
	rc = disk_read_lba(77, 10, buf);
	CHECK(rc == 0 && data_ok(77, 10, buf), "BIOS limit 1: 10 sectors read one at a time");

	/* A BIOS that reports success for fewer sectors than asked. */
	reset_fake(1000, 0);
	short_above = 8;
	rc = disk_read_lba(10, 40, buf);
	CHECK(rc == 0 && data_ok(10, 40, buf), "a short transfer is treated as a failure and retried smaller");

	/* A bad sector: final, and says where. */
	reset_fake(1000, 0);
	bad_lba = 1030;
	rc = disk_read_lba(1000, 64, buf);
	CHECK(rc == -1, "a persistently bad sector fails the read");
	CHECK(g_disk_last_error.lba == 1030 && g_disk_last_error.count == 1 && g_disk_last_error.status == 0x20,
	      "it reports the sector (lba %llu), count %u and BIOS status %#x", (unsigned long long)g_disk_last_error.lba,
	      g_disk_last_error.count, g_disk_last_error.status);

	/* A transient single-sector error is retried once. */
	reset_fake(1000, 0);
	transient_once = 0;
	max_ok = 1000;
	{
		/* force the first single-sector call to fail once */
		reset_fake(1, 0x01);
		transient_once = 1;
		rc = disk_read_lba(40, 1, buf);
		CHECK(rc == 0 && data_ok(40, 1, buf) && calls == 2, "a transient one-sector failure is retried once and succeeds (%u calls)", calls);
	}

	if (failures) {
		printf("\n%d check(s) FAILED\n", failures);
		return 1;
	}
	printf("\nall checks passed\n");
	return 0;
}
