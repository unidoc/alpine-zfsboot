#include "blkverify.h"

/*
 * See blkverify.h. Plain C with no BIOS or console dependency (all of
 * that comes in through the hooks), so bios/tests/blkverify_host_test.c
 * runs exactly this code against a simulated disk that returns wrong data
 * in every way the fault-injection builds of disk.c do.
 */

static uint32_t le32(const uint8_t *p)
{
	return (uint32_t)p[0] | ((uint32_t)p[1] << 8) | ((uint32_t)p[2] << 16) | ((uint32_t)p[3] << 24);
}

static void sha_of(const uint8_t *p, uint32_t n, uint8_t out[32])
{
	struct sha256_ctx c;

	sha256_init(&c);
	sha256_update(&c, p, n);
	sha256_final(&c, out);
}

int blksum_check_sector(const uint8_t sec[512])
{
	uint8_t d[32];

	sha_of(sec, 480, d);
	return payload_sum_digest_cmp(d, sec + 480);
}

int blksum_parse_header(const uint8_t sec[512], struct blksum_hdr *h)
{
	static const char magic[8] = { 'A', 'Z', 'B', 'L', 'K', 'S', 'U', 'M' };
	int i, j;

	for (i = 0; i < 8; i++)
		if (sec[i] != (uint8_t)magic[i])
			return BLKSUM_BAD;
	if (blksum_check_sector(sec) != 0)
		return BLKSUM_BAD;
	if (le32(sec + 8) != BLKSUM_VERSION)
		return BLKSUM_UNSUPPORTED;
	if (le32(sec + 12) != BLKSUM_BLOCK || le32(sec + 16) != BLKSUM_PER_SECTOR)
		return BLKSUM_BAD;
	for (i = 0; i < 2; i++) {
		struct blksum_file *f = &h->f[i];
		const uint8_t *p = sec + 20 + 16 * i;
		uint32_t len;

		f->size = le32(p);
		f->offset = le32(p + 4);
		f->nblocks = le32(p + 8);
		f->first = le32(p + 12);
		for (j = 0; j < 32; j++)
			f->sha256[j] = sec[52 + 32 * i + j];
		if (f->offset > f->size || f->first == 0)
			return BLKSUM_BAD;
		len = f->size - f->offset;
		if (f->nblocks != len / BLKSUM_BLOCK + (len % BLKSUM_BLOCK != 0))
			return BLKSUM_BAD;
	}
	return BLKSUM_OK;
}

void blkv_start(struct blkv *b, const struct blkv_hooks *h, void *ctx, uint32_t base, uint32_t length,
                uint32_t nblocks, uint8_t *scratch, uint32_t scratch_len)
{
	uint8_t *z = (uint8_t *)b;
	uint32_t i;

	for (i = 0; i < sizeof(*b); i++)
		z[i] = 0;
	b->h = h;
	b->ctx = ctx;
	b->base = base;
	b->length = length;
	b->nblocks = nblocks;
	b->scratch = scratch;
	b->scratch_len = scratch_len;
	sha256_init(&b->sha);
}

/* SHA-256 over [off, off+len) of the range, read back from RAM. */
static void hash_range(struct blkv *b, uint32_t off, uint32_t len)
{
	while (len > 0) {
		uint32_t n = len < b->scratch_len ? len : b->scratch_len;

		b->h->peek(b->scratch, b->base + off, n);
		sha256_update(&b->sha, b->scratch, n);
		off += n;
		len -= n;
	}
}

/* The entry the running chain gives for a block: the chaining state, or
 * for the last block the final digest. */
static void chain_entry(struct blkv *b, int last, uint8_t out[32])
{
	int i;

	if (last) {
		struct sha256_ctx c = b->sha;

		sha256_final(&c, out);
		return;
	}
	for (i = 0; i < 32; i++)
		out[i] = (uint8_t)(b->sha.h[i / 4] >> (24 - 8 * (i % 4)));
}

/* Hash the block again from the chain state before it; 0 on a match. */
static int rehash(struct blkv *b, const struct sha256_ctx *before, uint32_t start, uint32_t len, int last,
                  const uint8_t want[32], uint8_t got[32])
{
	b->sha = *before;
	hash_range(b, start, len);
	chain_entry(b, last, got);
	return payload_sum_digest_cmp(got, want);
}

static void check_block(struct blkv *b, uint32_t k)
{
	uint32_t start = k * BLKSUM_BLOCK;
	uint32_t len = b->length - start < BLKSUM_BLOCK ? b->length - start : BLKSUM_BLOCK;
	int last = k + 1 == b->nblocks;
	struct sha256_ctx before;
	uint8_t want[32], got[32];
	uint16_t cap0;
	int t, i;

	if (b->off)
		return;
	if (b->h->entry(b, k, want) != 0) {
		b->off = 1; /* the table cannot be read: the final whole-file check still runs */
		return;
	}
	before = b->sha;
	if (rehash(b, &before, start, len, last, want, got) == 0)
		return;

	b->mismatches++;
	if (b->bad >= BLKV_BAD_LIMIT) {
		b->bad++; /* see BLKV_BAD_LIMIT: counted, not re-read, not reported */
		b->unreported++;
		goto resume;
	}
	cap0 = b->h->get_cap();
	for (t = 1; t <= BLKV_TRIES; t++) {
		uint16_t size = (uint16_t)(cap0 >> (t - 1));

		if (size < 1)
			size = 1;
		b->h->reset();
		b->h->set_cap(size);
		b->retries++;
		if (b->h->reread(b, start, len) != 0)
			continue;
		if (rehash(b, &before, start, len, last, want, got) == 0) {
			b->healed++; /* and keep `size` for the rest of the boot */
			return;
		}
	}
	b->h->set_cap(cap0);

	if (b->h->second && b->h->second(b, start, len) == 0 &&
	    rehash(b, &before, start, len, last, want, got) == 0) {
		b->second++;
		return;
	}

	b->bad++;
	b->h->report(b, k, want, got, BLKV_TRIES);
resume:
	/* Resume the chain from what the table says this block should have
	 * produced, so the blocks after it are judged on their own. */
	if (!last) {
		for (i = 0; i < 8; i++)
			b->sha.h[i] = ((uint32_t)want[4 * i] << 24) | ((uint32_t)want[4 * i + 1] << 16) |
			              ((uint32_t)want[4 * i + 2] << 8) | want[4 * i + 3];
		b->sha.total = start + len;
		b->sha.fill = 0;
	}
}

void blkv_landed(struct blkv *b, uint32_t bytes)
{
	b->landed += bytes;
	while (b->next < b->nblocks) {
		uint32_t end = (b->next + 1) * BLKSUM_BLOCK;

		if (end > b->length)
			end = b->length;
		if (b->landed < end)
			break;
		check_block(b, b->next++);
	}
}
