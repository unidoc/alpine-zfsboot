/*
 * payload_sum_host_test.c - runs payload_sum.c (stage2's CHECKSUM parser and
 * SHA-256) on the host:
 *  - the SHA-256 against the FIPS 180-4 examples and against sha256sum on a
 *    large buffer hashed in awkwardly sized pieces (argv[1], from the run script);
 *  - the parser against internal/payloadsum/testdata/golden.sum, the file the Go
 *    writer's own test produces byte-for-byte, with the C digest of the same
 *    synthetic kernel/initrd matching what the Go side recorded;
 *  - a corrupted kernel byte, a wrong size, a moved offset and malformed files.
 * A missing CHECKSUM is decided in stage2_main.c (it never reaches the
 * parser); tests/bios-hdd-entry-test.sh covers it with a real boot.
 *
 * Build/run: bios/tests/run-payload-sum-host-test.sh
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "payload_sum.h"

static int failures;
#define CHECK(cond, ...) do { if (!(cond)) { failures++; printf("FAIL: "); } else printf("ok: "); printf(__VA_ARGS__); printf("\n"); } while (0)

static void hexify(const uint8_t d[32], char out[65])
{
	int i;
	for (i = 0; i < 32; i++)
		sprintf(out + 2 * i, "%02x", d[i]);
}

static void sha(const uint8_t *p, uint32_t n, uint32_t piece, uint8_t out[32])
{
	struct sha256_ctx c;
	sha256_init(&c);
	while (n > 0) {
		uint32_t k = n < piece ? n : piece;
		sha256_update(&c, p, k);
		p += k;
		n -= k;
	}
	sha256_final(&c, out);
}

/* The same bytes internal/payloadsum's goldenKernel()/goldenInitrd() build. */
static uint8_t gk[8192], gi[5000];
static void golden_inputs(void)
{
	unsigned i;
	for (i = 0; i < sizeof(gk); i++)
		gk[i] = (uint8_t)(i * 37 + 11);
	gk[0x1f1] = 3;
	gk[0x1fe] = 0x55;
	gk[0x1ff] = 0xaa;
	memcpy(gk + 0x202, "HdrS", 4);
	for (i = 0; i < sizeof(gi); i++)
		gi[i] = (uint8_t)(i * 13 + 3);
}

static enum payload_sum_result parse_str(const char *s, struct payload_sum *m)
{
	return payload_sum_parse(s, (uint32_t)strlen(s), m);
}

int main(int argc, char **argv)
{
	uint8_t d[32];
	char hex[65];

	if (argc != 4) {
		fprintf(stderr, "usage: %s BIGFILE BIGFILE_SHA256 GOLDEN_SUM\n", argv[0]);
		return 2;
	}

	/* FIPS 180-4 / NIST examples. */
	sha((const uint8_t *)"abc", 3, 64, d);
	hexify(d, hex);
	CHECK(!strcmp(hex, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"), "SHA-256(\"abc\")");
	sha((const uint8_t *)"", 0, 64, d);
	hexify(d, hex);
	CHECK(!strcmp(hex, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"), "SHA-256(\"\")");
	{
		const char *m = "abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq"; /* 56 bytes: padding spills into a 2nd block */
		sha((const uint8_t *)m, (uint32_t)strlen(m), 7, d);
		hexify(d, hex);
		CHECK(!strcmp(hex, "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1"), "SHA-256 of the 448-bit NIST message, fed 7 bytes at a time");
	}

	/* A large buffer, in the piece size stage2 really uses (the FAT staging
	 * buffer: 32 KiB on disk) and in odd pieces, against sha256sum. */
	{
		FILE *f = fopen(argv[1], "rb");
		static uint8_t big[3u << 20];
		size_t n = f ? fread(big, 1, sizeof(big), f) : 0;
		uint32_t pieces[] = { 32768, 18432, 1, 63, 65, 4097 };
		unsigned i;

		if (f)
			fclose(f);
		CHECK(n > 0, "read %zu bytes of the large test file", n);
		for (i = 0; i < sizeof(pieces) / sizeof(pieces[0]); i++) {
			sha(big, (uint32_t)n, pieces[i], d);
			hexify(d, hex);
			CHECK(!strcmp(hex, argv[2]), "SHA-256 of %zu bytes in %u-byte pieces matches sha256sum", n, pieces[i]);
		}
	}

	/* The Go writer's golden file. */
	{
		static char text[1024];
		struct payload_sum m;
		FILE *f = fopen(argv[3], "rb");
		size_t n = f ? fread(text, 1, sizeof(text) - 1, f) : 0;
		uint8_t dk[32], di[32];

		if (f)
			fclose(f);
		golden_inputs();
		CHECK(payload_sum_parse(text, (uint32_t)n, &m) == PAYLOAD_SUM_OK, "golden.sum (written by the Go encoder) parses");
		CHECK(m.mode == PAYLOAD_SUM_MODE_WARN, "golden.sum says MODE warn");
		CHECK(m.kernel.size == 8192 && m.kernel.offset == 2048 && m.initrd.size == 5000 && m.initrd.offset == 0,
		      "golden.sum sizes/offsets: kernel %u@%u initrd %u@%u", m.kernel.size, m.kernel.offset, m.initrd.size, m.initrd.offset);
		sha(gk + 2048, sizeof(gk) - 2048, 32768, dk);
		sha(gi, sizeof(gi), 32768, di);
		CHECK(payload_sum_digest_cmp(dk, m.kernel.sha256) == 0, "C SHA-256 of the loaded kernel range equals the Go-recorded digest");
		CHECK(payload_sum_digest_cmp(di, m.initrd.sha256) == 0, "C SHA-256 of the initrd equals the Go-recorded digest");

		gk[5000] ^= 0x01;
		sha(gk + 2048, sizeof(gk) - 2048, 32768, dk);
		CHECK(payload_sum_digest_cmp(dk, m.kernel.sha256) != 0, "one flipped kernel byte is detected");
		gk[5000] ^= 0x01;

		CHECK(payload_sum_check_layout(&m.kernel, 8192, 2048) == PAYLOAD_SUM_OK, "the right size and offset pass the pre-load check");
		CHECK(payload_sum_check_layout(&m.kernel, 8193, 2048) == PAYLOAD_SUM_SIZE, "a wrong kernel size is refused before loading");
		CHECK(payload_sum_check_layout(&m.initrd, 4999, 0) == PAYLOAD_SUM_SIZE, "a wrong initrd size is refused before loading");
		CHECK(payload_sum_check_layout(&m.kernel, 8192, 2560) == PAYLOAD_SUM_OFFSET, "a different real-mode size (setup_sects) is refused");
	}

	/* Malformed / other versions. */
	{
		struct payload_sum m;
		const char *k = "KERNEL 8192 2048 94b76fcdb16f0907d0eab4c0401063d21b7a57dc1f97db321902cde8fc1cdfe6\n";
		const char *i = "INITRD 5000 0 ab914b45858228984686becdaac63b77fca308e6ca9578f04cf94ef05c644e15\n";
		const char *h = "alpine-zfsboot payload-sum 1\n";
		char buf[1024];

		snprintf(buf, sizeof(buf), "%s%s%s", h, k, i);
		CHECK(parse_str(buf, &m) == PAYLOAD_SUM_OK, "header + KERNEL + INITRD parses");
		CHECK(m.mode == PAYLOAD_SUM_MODE_WARN, "no MODE line: warn (report and boot)");
		snprintf(buf, sizeof(buf), "%sMODE warn\n%s%s", h, k, i);
		CHECK(parse_str(buf, &m) == PAYLOAD_SUM_OK && m.mode == PAYLOAD_SUM_MODE_WARN, "MODE warn");
		snprintf(buf, sizeof(buf), "%sMODE enforce\n%s%s", h, k, i);
		CHECK(parse_str(buf, &m) == PAYLOAD_SUM_OK && m.mode == PAYLOAD_SUM_MODE_ENFORCE, "MODE enforce");
		snprintf(buf, sizeof(buf), "%sMODE off\n%s%s", h, k, i);
		CHECK(parse_str(buf, &m) == PAYLOAD_SUM_OK && m.mode == PAYLOAD_SUM_MODE_OFF, "MODE off");
		snprintf(buf, sizeof(buf), "%sMODE enforce now\n%s%s", h, k, i);
		CHECK(parse_str(buf, &m) == PAYLOAD_SUM_OK && m.mode == PAYLOAD_SUM_MODE_WARN, "anything else: warn, never enforce");
		snprintf(buf, sizeof(buf), "%sMODE Enforce\n%s%s", h, k, i);
		CHECK(parse_str(buf, &m) == PAYLOAD_SUM_OK && m.mode == PAYLOAD_SUM_MODE_WARN, "MODE Enforce (case): warn");
		CHECK(payload_sum_mode("off", 3) == PAYLOAD_SUM_MODE_OFF && payload_sum_mode("enforce", 7) == PAYLOAD_SUM_MODE_ENFORCE &&
		      payload_sum_mode("warn", 4) == PAYLOAD_SUM_MODE_WARN && payload_sum_mode("1", 1) == -1 && payload_sum_mode("enforc", 6) == -1,
		      "cmdline mode words: off/warn/enforce, anything else ignored");
		snprintf(buf, sizeof(buf), "%sCMDLINE whatever\n%s%s", h, i, k);
		CHECK(parse_str(buf, &m) == PAYLOAD_SUM_OK, "unknown lines are ignored, order does not matter");
		snprintf(buf, sizeof(buf), "alpine-zfsboot payload-sum 2\n%s%s", k, i);
		CHECK(parse_str(buf, &m) == PAYLOAD_SUM_UNSUPPORTED, "version 2 is 'not for this loader', not malformed");
		snprintf(buf, sizeof(buf), "%s%s", h, k);
		CHECK(parse_str(buf, &m) == PAYLOAD_SUM_MALFORMED, "INITRD missing");
		snprintf(buf, sizeof(buf), "%s%s%s%s", h, k, k, i);
		CHECK(parse_str(buf, &m) == PAYLOAD_SUM_MALFORMED, "KERNEL twice");
		snprintf(buf, sizeof(buf), "%s%s%s", h, k, i);
		buf[strlen(buf) - 1] = '\0';
		CHECK(parse_str(buf, &m) == PAYLOAD_SUM_MALFORMED, "last line without a newline");
		snprintf(buf, sizeof(buf), "%s%s%s", h, k, i);
		buf[strlen(h) + 20] = 'B';
		CHECK(parse_str(buf, &m) == PAYLOAD_SUM_MALFORMED, "an uppercase hex digit");
		snprintf(buf, sizeof(buf), "%sKERNEL 8192 9000 %s%s", h, k + 17, i);
		CHECK(parse_str(buf, &m) == PAYLOAD_SUM_MALFORMED, "offset beyond size");
		snprintf(buf, sizeof(buf), "%sKERNEL 99999999999 0 %s%s", h, k + 17, i);
		CHECK(parse_str(buf, &m) == PAYLOAD_SUM_MALFORMED, "a size that does not fit 32 bits");
		snprintf(buf, sizeof(buf), "%sKERNEL  8192 2048 %s%s", h, k + 17, i);
		CHECK(parse_str(buf, &m) == PAYLOAD_SUM_MALFORMED, "two spaces");
		CHECK(parse_str("", &m) == PAYLOAD_SUM_MALFORMED, "an empty file");
		CHECK(parse_str("garbage\n", &m) == PAYLOAD_SUM_MALFORMED, "not a manifest at all");
	}

	if (failures) {
		printf("\n%d check(s) FAILED\n", failures);
		return 1;
	}
	printf("\nall checks passed\n");
	return 0;
}
