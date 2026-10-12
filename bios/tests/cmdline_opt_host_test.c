/*
 * cmdline_opt_host_test.c - runs cmdline_opt.c (how stage2 finds its
 * alpine-zfsboot.* keys in EFI/ALPINE/CMDLINE and parses int13chunk=) on
 * the host.
 *
 * Build/run: bios/tests/run-cmdline-opt-host-test.sh
 */
#include <stdio.h>
#include <string.h>

#include "cmdline_opt.h"

static int failures;
#define CHECK(cond, ...) do { if (!(cond)) { failures++; printf("FAIL: "); } else printf("ok: "); printf(__VA_ARGS__); printf("\n"); } while (0)

static int chunk_of(const char *cmd)
{
	uint32_t n;
	const char *v = cmdline_value(cmd, "alpine-zfsboot.int13chunk=", &n);
	return v ? cmdline_parse_chunk(v, n, 127) : -2; /* -2: key absent */
}

int main(void)
{
	CHECK(chunk_of("root=ZFS=x alpine-zfsboot.int13chunk=8 quiet") == 8, "int13chunk=8 in the middle");
	CHECK(chunk_of("alpine-zfsboot.int13chunk=127") == 127, "127 (the largest) at the start");
	CHECK(chunk_of("ro alpine-zfsboot.int13chunk=1\n") == 1, "1, followed by the file's newline");
	CHECK(chunk_of("alpine-zfsboot.int13chunk=16  alpine-zfsboot.diag=1") == 16, "followed by two spaces");
	CHECK(chunk_of("root=x") == -2, "absent");
	CHECK(chunk_of("alpine-zfsboot.int13chunk=0") == -1, "0 is invalid");
	CHECK(chunk_of("alpine-zfsboot.int13chunk=128") == -1, "128 is invalid");
	CHECK(chunk_of("alpine-zfsboot.int13chunk=") == -1, "empty is invalid");
	CHECK(chunk_of("alpine-zfsboot.int13chunk=-8") == -1, "a sign is invalid");
	CHECK(chunk_of("alpine-zfsboot.int13chunk=0x10") == -1, "hex is invalid");
	CHECK(chunk_of("alpine-zfsboot.int13chunk=16k") == -1, "a suffix is invalid");
	CHECK(chunk_of("alpine-zfsboot.int13chunk=99999999") == -1, "a huge number is invalid (no overflow)");
	CHECK(chunk_of("alpine-zfsboot.int13chunk=008") == 8, "leading zeros are fine");
	CHECK(chunk_of("xalpine-zfsboot.int13chunk=8") == -2, "only a whole word counts");
	CHECK(chunk_of("foo=alpine-zfsboot.int13chunk=8") == -2, "...not a value of another key");
	{
		uint32_t n;
		const char *v = cmdline_value("a alpine-zfsboot.diag=1 b", "alpine-zfsboot.diag=", &n);
		CHECK(v && n == 1 && *v == '1', "diag=1 found with length 1");
		v = cmdline_value("alpine-zfsboot.integrity=enforce", "alpine-zfsboot.integrity=", &n);
		CHECK(v && n == 7 && !memcmp(v, "enforce", 7), "integrity=enforce found with length 7");
	}

	if (failures) {
		printf("\n%d check(s) FAILED\n", failures);
		return 1;
	}
	printf("\nall checks passed\n");
	return 0;
}
