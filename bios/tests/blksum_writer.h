/*
 * blksum_writer.h - a host-side writer of CHECKSUM and BLKSUM, for the host
 * tests and the QEMU/ISO tests (bios/tests/mkmanifest.c). The product
 * writer is the Go tool (internal/payloadsum); both are checked against
 * internal/payloadsum/testdata/golden.{sum,blksum.hex}.
 */
#ifndef BLKSUM_WRITER_H
#define BLKSUM_WRITER_H

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "blkverify.h"

static void bw_put32(uint8_t *p, uint32_t v)
{
	p[0] = (uint8_t)v;
	p[1] = (uint8_t)(v >> 8);
	p[2] = (uint8_t)(v >> 16);
	p[3] = (uint8_t)(v >> 24);
}

static void bw_sha(const uint8_t *p, uint32_t n, uint8_t out[32])
{
	struct sha256_ctx c;
	sha256_init(&c);
	sha256_update(&c, p, n);
	sha256_final(&c, out);
}

/* The chain entries of one RAM range (see blkverify.h). */
static uint8_t *bw_entries(const uint8_t *p, uint32_t len, uint32_t *nblocks)
{
	uint32_t n = len / BLKSUM_BLOCK + (len % BLKSUM_BLOCK != 0), i, k;
	uint8_t *e = calloc(n ? n : 1, 32);
	struct sha256_ctx c;

	sha256_init(&c);
	for (i = 0; i < n; i++) {
		uint32_t start = i * BLKSUM_BLOCK, l = len - start < BLKSUM_BLOCK ? len - start : BLKSUM_BLOCK;
		sha256_update(&c, p + start, l);
		if (i + 1 == n) {
			struct sha256_ctx f = c;
			sha256_final(&f, e + 32 * i);
		} else {
			for (k = 0; k < 32; k++)
				e[32 * i + k] = (uint8_t)(c.h[k / 4] >> (24 - 8 * (k % 4)));
		}
	}
	*nblocks = n;
	return e;
}

/* BLKSUM for kernel (RAM part from koff) and initrd; returns a malloc'd
 * buffer of *size bytes. */
static uint8_t *bw_blksum(const uint8_t *k, uint32_t ksize, uint32_t koff, const uint8_t *in, uint32_t isize, uint32_t *size)
{
	uint32_t kn, inn, ks, is, total, i;
	uint8_t *ke = bw_entries(k + koff, ksize - koff, &kn), *ie = bw_entries(in, isize, &inn);
	uint8_t *out, *h;

	ks = kn / BLKSUM_PER_SECTOR + (kn % BLKSUM_PER_SECTOR != 0);
	is = inn / BLKSUM_PER_SECTOR + (inn % BLKSUM_PER_SECTOR != 0);
	total = (1 + ks + is) * BLKSUM_SECTOR;
	out = calloc(total, 1);
	h = out;
	memcpy(h, "AZBLKSUM", 8);
	bw_put32(h + 8, BLKSUM_VERSION);
	bw_put32(h + 12, BLKSUM_BLOCK);
	bw_put32(h + 16, BLKSUM_PER_SECTOR);
	bw_put32(h + 20, ksize);
	bw_put32(h + 24, koff);
	bw_put32(h + 28, kn);
	bw_put32(h + 32, 1);
	bw_put32(h + 36, isize);
	bw_put32(h + 40, 0);
	bw_put32(h + 44, inn);
	bw_put32(h + 48, 1 + ks);
	bw_sha(k + koff, ksize - koff, h + 52);
	bw_sha(in, isize, h + 84);
	bw_sha(h, 480, h + 480);
	for (i = 0; i < kn; i++)
		memcpy(out + (1 + i / BLKSUM_PER_SECTOR) * BLKSUM_SECTOR + (i % BLKSUM_PER_SECTOR) * 32, ke + 32 * i, 32);
	for (i = 0; i < inn; i++)
		memcpy(out + (1 + ks + i / BLKSUM_PER_SECTOR) * BLKSUM_SECTOR + (i % BLKSUM_PER_SECTOR) * 32, ie + 32 * i, 32);
	for (i = 1; i < 1 + ks + is; i++)
		bw_sha(out + i * BLKSUM_SECTOR, 480, out + i * BLKSUM_SECTOR + 480);
	free(ke);
	free(ie);
	*size = total;
	return out;
}

/* CHECKSUM text (the same format internal/payloadsum writes). */
static int bw_checksum(char *buf, size_t n, const uint8_t *k, uint32_t ksize, uint32_t koff, const uint8_t *in, uint32_t isize,
                       const char *mode)
{
	uint8_t dk[32], di[32];
	char hk[65], hi[65];
	int i;

	bw_sha(k + koff, ksize - koff, dk);
	bw_sha(in, isize, di);
	for (i = 0; i < 32; i++) {
		sprintf(hk + 2 * i, "%02x", dk[i]);
		sprintf(hi + 2 * i, "%02x", di[i]);
	}
	return snprintf(buf, n, "alpine-zfsboot payload-sum 1\nMODE %s\nKERNEL %u %u %s\nINITRD %u 0 %s\n", mode, ksize, koff, hk,
	                isize, hi);
}

#endif
