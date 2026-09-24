/*
 * Host-buildable (normal gcc, no -m16/-ffreestanding) test harness for
 * bios/ata_atapi.c's own atapi_advance_after_phase() - the exact same
 * production function, compiled for this host instead of i386 real
 * mode. Only atapi_advance_after_phase() is called (a deliberately
 * pure function - no asm, no port I/O - see ata_atapi.h's own comment
 * on why it's exposed for exactly this). Nothing else in ata_atapi.c
 * is ever invoked here, so the file links cleanly against plain stub
 * definitions of the two console_puts* functions it also references,
 * with no need to emulate real ATA/ATAPI hardware registers at all.
 *
 * What this proves that QEMU/SeaBIOS testing structurally cannot: a
 * real ATAPI device is allowed to split one READ(10) across several
 * DRQ phases, each with its own `take` word count - but every
 * hypervisor this project has tested against always answers in a
 * single phase, so a bug in the multi-phase accumulation (data_buf's
 * pointer, or got_words' running total) is invisible to any boot test,
 * however thorough, run only under emulation. This test drives a
 * scripted multi-phase `take` sequence directly against the real
 * arithmetic and checks the exact byte offset and word count after
 * each phase - the shape a previous double-advance bug here needed
 * and didn't have (see ata_atapi.c's own comment at the call site this
 * function replaced).
 */
#include <stdio.h>
#include <string.h>

#include "../ata_atapi.h"

/* Stubs for the two symbols ata_atapi.c references that this test
 * never actually calls (atapi_init()/atapi_send_packet()'s own error
 * logging) - needed only so the real .o links, per this file's own
 * header comment. */
void console_puts(const char *s) { (void)s; }
void console_puts_hex32(unsigned int v) { (void)v; }

static int g_failures;

static void check(int cond, const char *what)
{
	if (cond) {
		printf("ok: %s\n", what);
	} else {
		printf("FAIL: %s\n", what);
		g_failures++;
	}
}

int main(void)
{
	/* A 22528-byte (11-native-sector) transfer split across 5 phases,
	 * the exact scenario this session's audit traced through the
	 * shipped object code: takes of 1024, 1024, 1024, 1024, and 512
	 * words (2048, 2048, 2048, 2048, 1024 bytes) covering the full
	 * transfer - deliberately uneven phase sizes, not a round number
	 * repeated, so a bug that only manifests on a non-uniform sequence
	 * isn't hidden. */
	uint16_t takes[] = {2560, 2560, 2560, 2560, 1024};
	const int n_phases = sizeof(takes) / sizeof(takes[0]);

	uint8_t backing[64 * 1024];
	uint8_t *data_buf = backing;
	uint8_t *const base = backing;
	uint16_t got_words = 0;
	uint32_t expected_bytes = 0;
	uint32_t expected_words = 0;
	int i;

	for (i = 0; i < n_phases; i++) {
		atapi_advance_after_phase(&data_buf, &got_words, takes[i]);
		expected_bytes += (uint32_t)takes[i] * 2;
		expected_words += takes[i];

		{
			char label[128];
			uint32_t actual_bytes = (uint32_t)(data_buf - base);
			snprintf(label, sizeof(label),
				"after phase %d (take=%u): data_buf advanced by exactly %u bytes",
				i + 1, (unsigned)takes[i], (unsigned)expected_bytes);
			check(actual_bytes == expected_bytes, label);

			snprintf(label, sizeof(label),
				"after phase %d: got_words is exactly %u",
				i + 1, (unsigned)expected_words);
			check(got_words == expected_words, label);
		}
	}

	/* The specific bug this regression-guards: a double-advance would
	 * leave data_buf 2x too far after multiple phases - assert the
	 * FINAL position precisely, not just that it's "close" or
	 * "nonzero". 11264 words = 22528 bytes, the real 11-native-sector
	 * transfer size this scenario is modeled on. */
	check(expected_bytes == 22528, "total bytes advanced across all phases matches the real 11-sector transfer size (22528)");
	check((uint32_t)(data_buf - base) == 22528,
		"final data_buf position is EXACTLY 22528 bytes from where it started - a double-advance would make this 45056");
	check(got_words == 11264, "final got_words is exactly 11264 (22528 bytes / 2)");

	/* A single zero-take phase (the "device says DRQ but reports 0
	 * bytes this phase" edge case) must not move anything. */
	{
		uint8_t *before = data_buf;
		uint16_t words_before = got_words;
		atapi_advance_after_phase(&data_buf, &got_words, 0);
		check(data_buf == before, "a zero-take phase does not advance data_buf");
		check(got_words == words_before, "a zero-take phase does not advance got_words");
	}

	/* atapi_transfer_complete() - regression guard for a real gap a
	 * full source audit found: atapi_send_packet()'s own data-phase
	 * loop trusted "the device says DRQ is clear now" as the only
	 * completion signal, with nothing checking that got_words had
	 * actually reached want_words. A device ending the phase early
	 * (short transfer, no error bit) used to return as SUCCESS, with
	 * the destination buffer's own tail left as stale/uninitialized
	 * bytes. */
	check(atapi_transfer_complete(11264, 11264) != 0,
		"a full transfer (got == want) is reported complete");
	check(atapi_transfer_complete(0, 11264) == 0,
		"zero words transferred against a nonzero want is reported incomplete, not silently OK");
	check(atapi_transfer_complete(11263, 11264) == 0,
		"one word short of the full transfer is reported incomplete - off-by-one must not slip through");
	check(atapi_transfer_complete(0, 0) != 0,
		"a genuinely zero-length transfer (data_len == 0's own TEST UNIT READY shape never reaches this check, but the pure comparison itself must still hold) is reported complete");

	if (g_failures == 0) {
		printf("\nall checks passed\n");
		return 0;
	}
	printf("\n%d check(s) FAILED\n", g_failures);
	return 1;
}
