/*
 * blkverify_host_test.c - runs blkverify.c (stage2's verify-and-heal of each
 * 64 KiB payload block as it lands) on the host, against a simulated disk
 * that answers CF=0 with wrong data in every way disk.c's fault-injection
 * builds can (a flipped bit, a sector of zeros, stale data from the previous
 * transfer, the data of the wrong LBA, a short transfer that leaves the last
 * sector stale) - transiently, persistently, or only for transfers above a
 * size. It proves: transient and size-dependent corruption is healed (RAM
 * ends up equal to the file, and a smaller size that healed is kept);
 * persistent corruption is reported with the exact block, both digests and
 * the number of re-reads, the blocks after it are still judged on their own,
 * and a second source heals it. Also the BLKSUM header parser and the Go
 * writer's golden file.
 *
 * Build/run: bios/tests/run-blkverify-host-test.sh
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "blksum_writer.h"

static int failures;
#define CHECK(cond, ...) do { if (!(cond)) { failures++; printf("FAIL: "); } else printf("ok: "); printf(__VA_ARGS__); printf("\n"); } while (0)

#define BASE 0x04000000u
#define LEN (5u * 65536u + 1234u) /* 6 blocks, the last one partial */

static uint8_t D[LEN], R[LEN];
static uint8_t *E;
static uint32_t NB;

/* ---- the simulated BIOS ---- */
enum kind { K_NONE, K_FLIP, K_ZERO, K_STALE, K_WRONGLBA, K_SHORT };
static enum kind kind;
static uint32_t above;       /* corrupt transfers of more than this many sectors (0 = not this trigger) */
static uint32_t target = ~0u;/* corrupt transfers that cover this sector... */
static int times;            /* ...this many times (-1 = always) */
static uint32_t from = ~0u;  /* corrupt every transfer reaching sector >= this (always) */
static uint16_t cap = 16;
static int resets, entry_fail, second_mode; /* 0 none, 1 good copy, 2 copy also bad */
static uint8_t last_xfer[64 * 512];
static uint32_t last_len;

static void transfer(uint32_t off, uint32_t len)
{
	uint32_t sec = off / 512, nsec = (len + 511) / 512, hit = 0;
	int bad = 0;

	if (kind != K_NONE) {
		if (above && nsec > above)
			bad = 1, hit = nsec / 2;
		if (from != ~0u && sec + nsec > from)
			bad = 1, hit = from > sec ? from - sec : 0;
		if (target != ~0u && target >= sec && target < sec + nsec && times != 0) {
			bad = 1, hit = target - sec;
			if (times > 0)
				times--;
		}
	}
	memcpy(R + off, D + off, len);
	if (bad) {
		uint32_t ho = hit * 512, hl = len - ho < 512 ? len - ho : 512;
		switch (kind) {
		case K_FLIP: R[off + ho + 7] ^= 0x04; break;
		case K_ZERO: memset(R + off + ho, 0, hl); break;
		case K_STALE: memcpy(R + off, last_xfer, len < last_len ? len : last_len); if (len > last_len) memset(R + off + last_len, 0xee, len - last_len); break;
		case K_WRONGLBA: memcpy(R + off, D + (off + 512 + len <= LEN ? off + 512 : 0), len); break;
		case K_SHORT: memcpy(R + off + (nsec - 1) * 512, last_xfer, len - (nsec - 1) * 512); break;
		default: break;
		}
	}
	memcpy(last_xfer, D + off, len); /* what the BIOS buffer held: the real data of this transfer */
	last_len = len;
}

/* A read of [off, off+len) as the loader would do it: transfers of at most cap sectors. */
static void read_range(uint32_t off, uint32_t len)
{
	while (len) {
		uint32_t n = len < cap * 512u ? len : cap * 512u;
		transfer(off, n);
		off += n;
		len -= n;
	}
}

/* ---- hooks ---- */
static int h_reread(struct blkv *b, uint32_t off, uint32_t len) { (void)b; read_range(off, len); return 0; }
static int h_second(struct blkv *b, uint32_t off, uint32_t len)
{
	(void)b;
	if (second_mode == 0)
		return -1;
	memcpy(R + off, D + off, len);
	if (second_mode == 2)
		R[off] ^= 1;
	return 0;
}
static int h_entry(struct blkv *b, uint32_t i, uint8_t out[32])
{
	(void)b;
	if (entry_fail)
		return -1;
	memcpy(out, E + 32 * i, 32);
	return 0;
}
static void h_peek(void *dst, uint32_t phys, uint32_t len) { memcpy(dst, R + (phys - BASE), len); }
static uint16_t h_get_cap(void) { return cap; }
static void h_set_cap(uint16_t n) { cap = n; }
static void h_reset(void) { resets++; }
static int reports;
static uint32_t report_block;
static uint8_t report_tries, report_want[32], report_got[32];
static void h_report(struct blkv *b, uint32_t block, const uint8_t want[32], const uint8_t got[32], uint8_t tries)
{
	(void)b;
	reports++;
	report_block = block;
	report_tries = tries;
	memcpy(report_want, want, 32);
	memcpy(report_got, got, 32);
}
static struct blkv_hooks hooks = { h_reread, h_second, h_entry, h_peek, h_get_cap, h_set_cap, h_reset, h_report };

static uint8_t scratch[16384];
static struct blkv bv;

/* Loads the whole range the way fat_read_range() does (32 KiB runs, each
 * split into transfers of `cap` sectors), telling blkv as it lands. */
static void load(void)
{
	uint32_t off = 0;

	memset(R, 0xaa, sizeof(R));
	blkv_start(&bv, &hooks, 0, BASE, LEN, NB, scratch, sizeof(scratch));
	while (off < LEN) {
		uint32_t n = LEN - off < 32768 ? LEN - off : 32768;
		read_range(off, n);
		blkv_landed(&bv, n);
		off += n;
	}
}

static void setup(enum kind k, uint32_t abv, uint32_t tgt, int t, int second)
{
	kind = k;
	above = abv;
	target = tgt;
	times = t;
	second_mode = second;
	from = ~0u;
	cap = 16;
	resets = reports = entry_fail = 0;
	last_len = 0;
}

static const char *kname[] = { "none", "bit flip", "zero sector", "stale data", "wrong LBA", "short (DAP unchanged)" };

int main(int argc, char **argv)
{
	uint32_t i;
	int k;

	for (i = 0; i < LEN; i++)
		D[i] = (uint8_t)(i * 31 + (i >> 9) * 7 + 3);
	E = bw_entries(D, LEN, &NB);
	CHECK(NB == 6, "6 blocks for 5*64K+1234 bytes (%u)", NB);

	setup(K_NONE, 0, ~0u, 0, 0);
	load();
	CHECK(bv.mismatches == 0 && bv.retries == 0 && resets == 0 && !memcmp(R, D, LEN), "a clean load: nothing to heal, no re-read, no reset");

	for (k = K_FLIP; k <= K_SHORT; k++) {
		/* Wrong data only for transfers above 8 sectors: healed by halving, size kept. */
		setup((enum kind)k, 8, ~0u, 0, 0);
		load();
		CHECK(!memcmp(R, D, LEN) && bv.bad == 0 && bv.healed >= 1 && cap == 8,
		      "%s above 8 sectors: every block healed, RAM equals the file, cap 16 -> 8 kept (healed %u, cap %u)", kname[k],
		      bv.healed, cap);
		/* Transient: one bad transfer covering sector 300 (block 2). */
		setup((enum kind)k, 0, 300, 1, 0);
		load();
		CHECK(!memcmp(R, D, LEN) && bv.bad == 0 && bv.healed == 1 && bv.retries == 1 && cap == 16 && resets == 1,
		      "%s once (transient): healed by one same-size re-read after a reset, cap unchanged", kname[k]);
		/* Persistent at sector 300: reported, the rest still judged. */
		setup((enum kind)k, 0, 300, -1, 0);
		load();
		CHECK(bv.bad == 1 && reports == 1 && report_block == 2 && report_tries == BLKV_TRIES && bv.retries == BLKV_TRIES &&
		      resets == BLKV_TRIES && memcmp(report_want, report_got, 32) != 0 && !memcmp(report_want, E + 64, 32),
		      "%s persistent: block 2 reported after %d re-reads with want (the table's) and got", kname[k], BLKV_TRIES);
		CHECK(!memcmp(R, D, 2 * 65536) && !memcmp(R + 3 * 65536, D + 3 * 65536, LEN - 3 * 65536) && bv.mismatches == 1,
		      "%s persistent: only block 2 differs in RAM; blocks 3..5 judged on their own and fine", kname[k]);
		CHECK(cap == 16, "%s persistent: the cap goes back to 16 after the failed attempts", kname[k]);
	}

	/* Halving sequence: 16, 8, 4, 2, 1, 1, 1, 1. */
	setup(K_FLIP, 0, 300, -1, 0);
	load();
	CHECK(bv.retries == 8, "8 re-reads per bad block");

	/* Second source heals a persistent error; one that is also bad does not. */
	setup(K_ZERO, 0, 300, -1, 1);
	load();
	CHECK(!memcmp(R, D, LEN) && bv.second == 1 && bv.bad == 0 && reports == 0, "persistent error, good second source: healed from it");
	setup(K_ZERO, 0, 300, -1, 2);
	load();
	CHECK(bv.second == 0 && bv.bad == 1 && reports == 1, "second source also wrong: still reported");

	/* Every block from block 1 on stays wrong: after BLKV_BAD_LIMIT reported
	 * blocks the rest is only counted - no re-reads, no reports. */
	setup(K_FLIP, 0, ~0u, 0, 0);
	from = 128;
	load();
	CHECK(bv.bad == 5 && reports == BLKV_BAD_LIMIT && bv.unreported == 5 - BLKV_BAD_LIMIT &&
	      bv.retries == BLKV_BAD_LIMIT * BLKV_TRIES && bv.mismatches == 5,
	      "5 wrong blocks: %d reported with %d re-reads each, %u more only counted (bad %u, re-reads %u)", BLKV_BAD_LIMIT, BLKV_TRIES,
	      bv.unreported, bv.bad, bv.retries);

	/* The last, partial block (final digest, not a chaining state). */
	setup(K_FLIP, 0, (LEN - 100) / 512, -1, 0);
	load();
	CHECK(bv.bad == 1 && report_block == 5 && !memcmp(report_want, E + 5 * 32, 32), "persistent error in the last, partial block: reported as block 5");
	setup(K_FLIP, 0, (LEN - 100) / 512, 1, 0);
	load();
	CHECK(bv.bad == 0 && bv.healed == 1 && !memcmp(R, D, LEN), "transient error in the last block: healed");

	/* An unreadable table stops the per-block checks (the final whole-file SHA-256 still runs in stage2). */
	setup(K_FLIP, 0, 300, -1, 0);
	entry_fail = 1;
	load();
	CHECK(bv.off == 1 && bv.bad == 0 && reports == 0 && bv.retries == 0, "table unreadable: no per-block checks, nothing reported");

	/* BLKSUM header parsing. */
	{
		uint32_t ks = 70000, is = 3 * 65536 + 5, sz;
		uint8_t *kb = calloc(ks, 1), *ib = calloc(is, 1), *blk, sec[512];
		struct blksum_hdr h;

		for (i = 0; i < ks; i++)
			kb[i] = (uint8_t)(i * 13);
		for (i = 0; i < is; i++)
			ib[i] = (uint8_t)(i * 17);
		kb[0x1f1] = 3;
		blk = bw_blksum(kb, ks, 2048, ib, is, &sz);
		CHECK(blksum_parse_header(blk, &h) == BLKSUM_OK && h.f[0].size == ks && h.f[0].offset == 2048 && h.f[0].nblocks == 2 &&
		      h.f[0].first == 1 && h.f[1].nblocks == 4 && h.f[1].first == 2 && sz == 3 * 512,
		      "header: sizes, offsets, blocks, first table sectors (%u bytes)", sz);
		CHECK(blksum_check_sector(blk + 512) == 0 && blksum_check_sector(blk + 1024) == 0, "table sectors check themselves");
		memcpy(sec, blk + 512, 512);
		sec[40] ^= 1;
		CHECK(blksum_check_sector(sec) != 0, "a table sector with one flipped bit fails its own check");
		memcpy(sec, blk, 512);
		sec[25] ^= 1;
		CHECK(blksum_parse_header(sec, &h) == BLKSUM_BAD, "a header with one flipped bit is rejected");
		memcpy(sec, blk, 512);
		sec[0] = 'X';
		CHECK(blksum_parse_header(sec, &h) == BLKSUM_BAD, "wrong magic");
		memcpy(sec, blk, 512);
		bw_put32(sec + 8, 2);
		bw_sha(sec, 480, sec + 480);
		CHECK(blksum_parse_header(sec, &h) == BLKSUM_UNSUPPORTED, "version 2: unsupported, not malformed");
		memcpy(sec, blk, 512);
		bw_put32(sec + 28, 7);
		bw_sha(sec, 480, sec + 480);
		CHECK(blksum_parse_header(sec, &h) == BLKSUM_BAD, "a block count that does not fit the size");
	}

	/* The Go writer's golden files (internal/payloadsum/testdata). */
	if (argc == 3) {
		/* golden.blksum.hex: the binary file as hex text, 32 bytes per line. */
		FILE *f = fopen(argv[1], "r");
		static uint8_t g[1 << 16];
		size_t n = 0;
		unsigned v;

		while (f && n < sizeof(g) && fscanf(f, "%2x", &v) == 1)
			g[n++] = (uint8_t)v;
		uint8_t kb[8192], ib[5000], *mine;
		uint32_t sz;
		char sum[512], gsum[512];
		FILE *fs = fopen(argv[2], "rb");
		size_t sn = fs ? fread(gsum, 1, sizeof(gsum) - 1, fs) : 0;

		if (f)
			fclose(f);
		if (fs)
			fclose(fs);
		gsum[sn] = 0;
		for (i = 0; i < sizeof(kb); i++)
			kb[i] = (uint8_t)(i * 37 + 11);
		kb[0x1f1] = 3;
		kb[0x1fe] = 0x55;
		kb[0x1ff] = 0xaa;
		memcpy(kb + 0x202, "HdrS", 4);
		for (i = 0; i < sizeof(ib); i++)
			ib[i] = (uint8_t)(i * 13 + 3);
		mine = bw_blksum(kb, sizeof(kb), 2048, ib, sizeof(ib), &sz);
		CHECK(n == sz && !memcmp(mine, g, sz), "this writer and the Go writer produce the same BLKSUM, byte for byte (%zu/%u bytes)", n, sz);
		bw_checksum(sum, sizeof(sum), kb, sizeof(kb), 2048, ib, sizeof(ib), "warn");
		CHECK(!strcmp(sum, gsum), "...and the same CHECKSUM");
	}

	if (failures) {
		printf("\n%d check(s) FAILED\n", failures);
		return 1;
	}
	printf("\nall checks passed\n");
	return 0;
}
