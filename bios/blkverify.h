#ifndef ZFSBOOT_BIOS_BLKVERIFY_H
#define ZFSBOOT_BIOS_BLKVERIFY_H

#include "payload_sum.h"
#include "stdint_local.h"

/*
 * EFI/ALPINE/BLKSUM - per-block checks of the payload, so a block that a
 * BIOS delivered wrong (CF=0 with bad data) is caught, re-read and healed
 * as it is loaded, instead of only being noticed as a whole-file SHA-256
 * mismatch at the end. Written by the alpine-zfsboot tool (internal/
 * payloadsum - keep the two in step; testdata/golden.blksum.hex is checked by
 * both) next to CHECKSUM, whose MODE (off/warn/enforce) governs both.
 *
 * What a table entry is: the payload's bytes as stage2 places them in RAM
 * (KERNEL from its real-mode offset on, INITRD whole) are split into
 * BLKSUM_BLOCK-byte blocks; entry i is the SHA-256 chaining state (H0..H7,
 * big-endian, 32 bytes) after bytes [0, (i+1)*BLKSUM_BLOCK), and the last
 * entry is the final SHA-256 digest of the whole range - the same digest
 * CHECKSUM records. So checking a block costs nothing beyond the SHA-256
 * pass over the file that has to happen anyway, and a block is judged
 * with the full 256-bit strength of SHA-256 (any corruption of that block
 * goes unnoticed with probability about 2^-256 - for practical purposes
 * never), given the entries before it. After a block that cannot be
 * healed, checking resumes from that block's expected entry, so one bad
 * block does not make every later block look bad.
 *
 * File layout, 512-byte sectors, little-endian numbers:
 *   sector 0, header: "AZBLKSUM" | u32 version (1) | u32 block size
 *     (65536) | u32 entries per sector (15) | KERNEL: u32 size, u32
 *     offset, u32 blocks, u32 first table sector | INITRD: the same |
 *     KERNEL SHA-256 (32) | INITRD SHA-256 (32) | zeros | at 480: SHA-256
 *     of bytes 0..479
 *   table sectors: 15 entries of 32 bytes (480 bytes, unused ones zero),
 *     then at 480 the SHA-256 of those 480 bytes - each sector checks
 *     itself, so a table sector the BIOS delivered wrong is re-read too.
 * A different version is "not for this loader": skipped with a notice.
 */
#define BLKSUM_PATH "/EFI/ALPINE/BLKSUM"
#define BLKSUM_VERSION 1
#define BLKSUM_BLOCK 65536u
#define BLKSUM_PER_SECTOR 15u
#define BLKSUM_SECTOR 512u

struct blksum_file {
	uint32_t size, offset, nblocks, first;
	uint8_t sha256[32];
};

struct blksum_hdr {
	struct blksum_file f[2]; /* 0 KERNEL, 1 INITRD */
};

#define BLKSUM_OK 0
#define BLKSUM_BAD (-1)         /* not a BLKSUM header, or it fails its own hash */
#define BLKSUM_UNSUPPORTED (-2) /* another version */

int blksum_parse_header(const uint8_t sec[512], struct blksum_hdr *h);

/* 0 when a table sector's own hash matches. */
int blksum_check_sector(const uint8_t sec[512]);

/* The bytes [0, BLKSUM_PER_SECTOR*32) hold the entries; entry i of a file
 * lives in table sector (first + i / 15), slot i % 15. */

/*
 * The verify-and-heal engine. Data lands in RAM through the caller's own
 * reads; blkv_landed() is told how many bytes arrived and checks every
 * block that is now complete. A block that does not match is re-read
 * (hooks->reread) up to BLKV_TRIES times, with a controller reset before
 * each and the transfer size halved each time from the cap in force
 * (same size first, then half, ... down to 1 sector); a size that heals a
 * block is kept for the rest of the boot. Then hooks->second (another
 * drive's copy), if there is one. A block still wrong after that is
 * reported (hooks->report) and counted in `bad`; what that means for the
 * boot (warn or refuse) is the caller's decision.
 */
#define BLKV_TRIES 8
/*
 * After this many blocks of one file that could not be healed, the rest of
 * that file is only checked and counted - no more re-reads, no second
 * source, no report per block: a file whose every block is wrong (the same
 * wrong data at every transfer size, or a file replaced without updating
 * CHECKSUM/BLKSUM) would otherwise cost about 8 x (64 KiB / 1..16 sectors)
 * BIOS calls per block and bury the screen in reports. `bad` still counts
 * every wrong block; `unreported` the ones after the limit.
 */
#define BLKV_BAD_LIMIT 3

struct blkv;

struct blkv_hooks {
	/* re-read [off, off+len) of the range into RAM at base+off; 0 or -1 */
	int (*reread)(struct blkv *b, uint32_t off, uint32_t len);
	/* the same range from a second source (another drive); 0 or -1; may be 0 */
	int (*second)(struct blkv *b, uint32_t off, uint32_t len);
	/* table entry i (32 bytes); 0, or -1 when the table cannot be read */
	int (*entry)(struct blkv *b, uint32_t i, uint8_t out[32]);
	/* len bytes of RAM at physical address phys into dst */
	void (*peek)(void *dst, uint32_t phys, uint32_t len);
	/* transfer cap (512-byte sectors), controller reset */
	uint16_t (*get_cap)(void);
	void (*set_cap)(uint16_t);
	void (*reset)(void);
	/* a block that could not be healed: index, expected and actual entry,
	 * re-reads tried */
	void (*report)(struct blkv *b, uint32_t block, const uint8_t want[32], const uint8_t got[32], uint8_t tries);
};

struct blkv {
	const struct blkv_hooks *h;
	void *ctx;              /* the caller's own */
	uint32_t base;          /* physical address of the range in RAM */
	uint32_t length;        /* bytes */
	uint32_t nblocks;
	uint8_t *scratch;       /* peek buffer */
	uint32_t scratch_len;
	struct sha256_ctx sha;  /* the running chain */
	uint32_t landed;        /* bytes in RAM so far */
	uint32_t next;          /* next block to check */
	uint8_t off;            /* table unreadable: no more per-block checks */
	/* counters */
	uint16_t healed;        /* blocks fixed by a re-read */
	uint16_t second;        /* blocks fixed from the second source */
	uint16_t bad;           /* blocks still wrong */
	uint16_t retries;       /* re-reads tried */
	uint16_t mismatches;    /* blocks that did not match at first */
	uint16_t unreported;    /* wrong blocks after BLKV_BAD_LIMIT: counted only */
};

void blkv_start(struct blkv *b, const struct blkv_hooks *h, void *ctx, uint32_t base, uint32_t length,
                uint32_t nblocks, uint8_t *scratch, uint32_t scratch_len);

/* `bytes` more bytes are in RAM (in order); checks the blocks completed. */
void blkv_landed(struct blkv *b, uint32_t bytes);

#endif
