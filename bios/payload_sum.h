#ifndef ZFSBOOT_BIOS_PAYLOAD_SUM_H
#define ZFSBOOT_BIOS_PAYLOAD_SUM_H

#include "stdint_local.h"

/*
 * EFI/ALPINE/CHECKSUM (no extension: stage2's FAT reader, fat.c, resolves
 * extension-less 8-character names only) - the expected size and SHA-256 of what stage2
 * loads into RAM, written by the alpine-zfsboot tool at install/update time
 * together with KERNEL/INITRD (internal/payloadsum is the writer; keep the
 * two in step). stage2 checks the bytes actually in RAM against it after
 * loading and before jumping into the kernel, so a read that reported
 * success but delivered wrong data, or anything that changed the loaded
 * image afterwards, stops the boot with both digests on screen instead of
 * running a corrupt kernel - but only in mode enforce. The mode is
 *   warn    (default; also when there is no MODE line or an unknown one):
 *           hash, and on a mismatch print both digests and boot anyway
 *   enforce refuse to boot on a mismatch - an explicit opt-in, for
 *           diagnosing a failing host that has another way to boot
 *   off     do not hash at all (saves boot time on a huge initrd)
 * and alpine-zfsboot.integrity=off|warn|enforce on the kernel command line
 * (EFI/ALPINE/CMDLINE) overrides the file's MODE for that boot. A missing
 * file is not an error (older installs have none): the check is skipped
 * with a notice.
 *
 * This is an integrity check against corruption and forgotten updates,
 * NOT secure boot: the file sits on the same unauthenticated FAT as the
 * kernel, so anyone who can write the disk can change both.
 *
 * Format (plain text, '\n' line ends, nothing else on a line):
 *
 *   alpine-zfsboot payload-sum 1
 *   MODE off|warn|enforce    (optional; absent or anything else = warn)
 *   KERNEL <file size> <offset> <sha256 hex of the file from offset to end>
 *   INITRD <file size> <offset> <sha256 hex of the file from offset to end>
 *
 * Decimal numbers, lowercase hex. <offset> is where the bytes stage2 places
 * in RAM start inside the file: for KERNEL the size of the bzImage's
 * real-mode part ((setup_sects or 4) + 1 sectors, never loaded), for
 * INITRD 0. Lines naming other files are ignored, so later versions can
 * add some; a different version number on the first line means "not for
 * this loader" and is treated like a missing file.
 */
#define PAYLOAD_SUM_PATH "/EFI/ALPINE/CHECKSUM"
#define PAYLOAD_SUM_MAX_BYTES 512

struct payload_sum_entry {
	uint8_t present;
	uint32_t size;
	uint32_t offset;
	uint8_t sha256[32];
};

struct payload_sum {
	struct payload_sum_entry kernel;
	struct payload_sum_entry initrd;
	uint8_t mode; /* PAYLOAD_SUM_MODE_* */
};

#define PAYLOAD_SUM_MODE_WARN 0
#define PAYLOAD_SUM_MODE_ENFORCE 1
#define PAYLOAD_SUM_MODE_OFF 2

/* "off"/"warn"/"enforce" (n bytes at s) -> PAYLOAD_SUM_MODE_*, or -1. */
int payload_sum_mode(const char *s, uint32_t n);

enum payload_sum_result {
	PAYLOAD_SUM_OK = 0,
	PAYLOAD_SUM_MALFORMED,   /* not parseable, or KERNEL/INITRD missing */
	PAYLOAD_SUM_UNSUPPORTED, /* a different format version */
	PAYLOAD_SUM_SIZE,        /* the file's size differs from the recorded one */
	PAYLOAD_SUM_OFFSET,      /* the loaded range starts somewhere else */
	PAYLOAD_SUM_DIGEST,      /* the bytes differ */
};

/* Parses `len` bytes of manifest text. */
enum payload_sum_result payload_sum_parse(const char *text, uint32_t len, struct payload_sum *out);

/* Whether a file of `file_size` bytes, loaded from `offset`, is the one an
 * entry describes (before loading: catches a stale manifest or a changed
 * file without reading a byte of it). */
enum payload_sum_result payload_sum_check_layout(const struct payload_sum_entry *e, uint32_t file_size, uint32_t offset);

/* 0 when the two digests are equal. */
int payload_sum_digest_cmp(const uint8_t a[32], const uint8_t b[32]);

/* SHA-256 (FIPS 180-4), small and freestanding. */
struct sha256_ctx {
	uint32_t h[8];
	uint32_t total; /* bytes hashed so far (stage2 never hashes 4 GiB) */
	uint8_t buf[64];
	uint32_t fill;
};

void sha256_init(struct sha256_ctx *c);
void sha256_update(struct sha256_ctx *c, const uint8_t *p, uint32_t n);
void sha256_final(struct sha256_ctx *c, uint8_t out[32]);

#endif
