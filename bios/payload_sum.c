#include "payload_sum.h"

/*
 * Plain C with no BIOS dependency, so bios/tests/payload_sum_host_test.c
 * runs exactly this code on the host (against Go's own encoder output and
 * sha256sum). See payload_sum.h for what the manifest is for.
 */

static const uint32_t k256[64] = {
	0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
	0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
	0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
	0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
	0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
	0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
	0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
	0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
};

#define ROR(x, n) (((x) >> (n)) | ((x) << (32 - (n))))

/* The message schedule, 16 words computed on the fly (w[i & 15]) instead
 * of the usual 64: static, not on the stack (the real-mode stack is the
 * scarce resource, see bios/Makefile), and small. */
static uint32_t g_sha_w[16];

static void sha256_block(struct sha256_ctx *c, const uint8_t *p)
{
	uint32_t a, b, d, e, f, g, h, cc, t1, t2;
	uint32_t *w = g_sha_w;
	int i;

	for (i = 0; i < 16; i++)
		w[i] = ((uint32_t)p[4 * i] << 24) | ((uint32_t)p[4 * i + 1] << 16) | ((uint32_t)p[4 * i + 2] << 8) | p[4 * i + 3];
	a = c->h[0]; b = c->h[1]; cc = c->h[2]; d = c->h[3];
	e = c->h[4]; f = c->h[5]; g = c->h[6]; h = c->h[7];
	for (i = 0; i < 64; i++) {
		if (i >= 16) {
			uint32_t w15 = w[(i + 1) & 15], w2 = w[(i + 14) & 15];
			w[i & 15] += (ROR(w15, 7) ^ ROR(w15, 18) ^ (w15 >> 3)) + w[(i + 9) & 15] +
			             (ROR(w2, 17) ^ ROR(w2, 19) ^ (w2 >> 10));
		}
		t1 = h + (ROR(e, 6) ^ ROR(e, 11) ^ ROR(e, 25)) + ((e & f) ^ (~e & g)) + k256[i] + w[i & 15];
		t2 = (ROR(a, 2) ^ ROR(a, 13) ^ ROR(a, 22)) + ((a & b) ^ (a & cc) ^ (b & cc));
		h = g; g = f; f = e; e = d + t1;
		d = cc; cc = b; b = a; a = t1 + t2;
	}
	c->h[0] += a; c->h[1] += b; c->h[2] += cc; c->h[3] += d;
	c->h[4] += e; c->h[5] += f; c->h[6] += g; c->h[7] += h;
}

void sha256_init(struct sha256_ctx *c)
{
	c->h[0] = 0x6a09e667; c->h[1] = 0xbb67ae85; c->h[2] = 0x3c6ef372; c->h[3] = 0xa54ff53a;
	c->h[4] = 0x510e527f; c->h[5] = 0x9b05688c; c->h[6] = 0x1f83d9ab; c->h[7] = 0x5be0cd19;
	c->total = 0;
	c->fill = 0;
}

void sha256_update(struct sha256_ctx *c, const uint8_t *p, uint32_t n)
{
	c->total += n; /* bytes; < 4 GiB for anything stage2 loads */
	while (n > 0) {
		if (c->fill == 0 && n >= 64) {
			sha256_block(c, p);
			p += 64;
			n -= 64;
			continue;
		}
		c->buf[c->fill++] = *p++;
		n--;
		if (c->fill == 64) {
			sha256_block(c, c->buf);
			c->fill = 0;
		}
	}
}

void sha256_final(struct sha256_ctx *c, uint8_t out[32])
{
	uint32_t lo = c->total << 3, hi = c->total >> 29; /* length in bits, 64-bit big-endian */
	int i;

	c->buf[c->fill++] = 0x80;
	if (c->fill > 56) {
		while (c->fill < 64)
			c->buf[c->fill++] = 0;
		sha256_block(c, c->buf);
		c->fill = 0;
	}
	while (c->fill < 56)
		c->buf[c->fill++] = 0;
	for (i = 0; i < 4; i++) {
		c->buf[56 + i] = (uint8_t)(hi >> (24 - 8 * i));
		c->buf[60 + i] = (uint8_t)(lo >> (24 - 8 * i));
	}
	sha256_block(c, c->buf);
	for (i = 0; i < 32; i++)
		out[i] = (uint8_t)(c->h[i / 4] >> (24 - 8 * (i % 4)));
}

/* ---- manifest parsing ---- */

static int str_eq(const char *a, uint32_t alen, const char *b)
{
	uint32_t i;

	for (i = 0; i < alen; i++)
		if (b[i] != a[i])
			return 0;
	return b[alen] == '\0';
}

/* One space-separated field starting at *pos within [0, end); returns its
 * length (0 = no field). */
static uint32_t next_field(const char *t, uint32_t *pos, uint32_t end, const char **field)
{
	uint32_t n = 0;

	if (*pos < end && t[*pos] == ' ')
		(*pos)++;
	*field = t + *pos;
	while (*pos < end && t[*pos] != ' ') {
		(*pos)++;
		n++;
	}
	return n;
}

static int parse_dec(const char *s, uint32_t n, uint32_t *out)
{
	uint32_t v = 0, i;

	if (n == 0 || n > 10)
		return -1;
	for (i = 0; i < n; i++) {
		uint32_t d;
		if (s[i] < '0' || s[i] > '9')
			return -1;
		d = (uint32_t)(s[i] - '0');
		if (v > (0xffffffffu - d) / 10)
			return -1;
		v = v * 10 + d;
	}
	*out = v;
	return 0;
}

static int hexval(char c)
{
	if (c >= '0' && c <= '9')
		return c - '0';
	if (c >= 'a' && c <= 'f')
		return c - 'a' + 10;
	return -1;
}

static int parse_entry(const char *t, uint32_t pos, uint32_t end, struct payload_sum_entry *e)
{
	const char *f;
	uint32_t n, i;

	n = next_field(t, &pos, end, &f);
	if (parse_dec(f, n, &e->size) != 0)
		return -1;
	n = next_field(t, &pos, end, &f);
	if (parse_dec(f, n, &e->offset) != 0)
		return -1;
	n = next_field(t, &pos, end, &f);
	if (n != 64 || pos != end)
		return -1;
	for (i = 0; i < 32; i++) {
		int hi = hexval(f[2 * i]), lo = hexval(f[2 * i + 1]);
		if (hi < 0 || lo < 0)
			return -1;
		e->sha256[i] = (uint8_t)((hi << 4) | lo);
	}
	if (e->offset > e->size)
		return -1;
	e->present = 1;
	return 0;
}

enum payload_sum_result payload_sum_parse(const char *text, uint32_t len, struct payload_sum *out)
{
	uint32_t pos = 0, line = 0;
	uint8_t *z = (uint8_t *)out;
	uint32_t i;

	for (i = 0; i < sizeof(*out); i++)
		z[i] = 0;

	while (pos < len) {
		uint32_t end = pos, p;
		const char *f;
		uint32_t n;

		while (end < len && text[end] != '\n')
			end++;
		if (end == len)
			return PAYLOAD_SUM_MALFORMED; /* every line ends in '\n', the last one too */
		p = pos;
		if (line == 0) {
			n = next_field(text, &p, end, &f);
			if (!str_eq(f, n, "alpine-zfsboot"))
				return PAYLOAD_SUM_MALFORMED;
			n = next_field(text, &p, end, &f);
			if (!str_eq(f, n, "payload-sum"))
				return PAYLOAD_SUM_MALFORMED;
			n = next_field(text, &p, end, &f);
			if (p != end || n == 0)
				return PAYLOAD_SUM_MALFORMED;
			if (!str_eq(f, n, "1"))
				return PAYLOAD_SUM_UNSUPPORTED;
		} else {
			struct payload_sum_entry *e = 0;

			n = next_field(text, &p, end, &f);
			if (str_eq(f, n, "KERNEL"))
				e = &out->kernel;
			else if (str_eq(f, n, "INITRD"))
				e = &out->initrd;
			else if (str_eq(f, n, "MODE")) {
				int m;
				n = next_field(text, &p, end, &f);
				m = p == end ? payload_sum_mode(f, n) : -1;
				out->mode = m < 0 ? PAYLOAD_SUM_MODE_WARN : (uint8_t)m;
			}
			if (e) {
				if (e->present || parse_entry(text, p, end, e) != 0)
					return PAYLOAD_SUM_MALFORMED;
			}
		}
		line++;
		pos = end + 1;
	}
	if (!out->kernel.present || !out->initrd.present)
		return PAYLOAD_SUM_MALFORMED;
	return PAYLOAD_SUM_OK;
}

int payload_sum_mode(const char *s, uint32_t n)
{
	if (str_eq(s, n, "warn"))
		return PAYLOAD_SUM_MODE_WARN;
	if (str_eq(s, n, "enforce"))
		return PAYLOAD_SUM_MODE_ENFORCE;
	if (str_eq(s, n, "off"))
		return PAYLOAD_SUM_MODE_OFF;
	return -1;
}

enum payload_sum_result payload_sum_check_layout(const struct payload_sum_entry *e, uint32_t file_size, uint32_t offset)
{
	if (e->size != file_size)
		return PAYLOAD_SUM_SIZE;
	if (e->offset != offset)
		return PAYLOAD_SUM_OFFSET;
	return PAYLOAD_SUM_OK;
}

int payload_sum_digest_cmp(const uint8_t a[32], const uint8_t b[32])
{
	int i;

	for (i = 0; i < 32; i++)
		if (a[i] != b[i])
			return 1;
	return 0;
}
