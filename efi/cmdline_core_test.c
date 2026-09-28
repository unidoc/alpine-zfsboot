/*
 * Standalone host test for cmdline_core.c - proves the pure string
 * logic (combining the embedded ASCII cmdline with firmware-supplied
 * CHAR16 LoadOptions) is correct, without needing the real gnu-efi
 * cross toolchain/headers at all (see cmdline_core.h's own
 * ALPINE_ZFSBOOT_CMDLINE_TEST shim). Does NOT exercise the real EFI
 * glue in main.c (AllocatePool, the real LoadedImageProtocol read) -
 * that still needs a real build (`make -C efi`) on a host with the
 * actual gnu-efi/gnu-efi-dev packages, which this sandbox does not
 * have.
 *
 * Run:  cc -std=c11 -Wall -Wextra -Werror -o /tmp/cmdline_core_test \
 *           -DALPINE_ZFSBOOT_CMDLINE_TEST cmdline_core_test.c && \
 *           /tmp/cmdline_core_test
 */
#include "cmdline_core.h"
#include "cmdline_core.c"

#include <stdio.h>
#include <string.h>

static int failed = 0;

static void check(const char *name, int ok)
{
	if (ok)
		printf("  ok - %s\n", name);
	else {
		printf("  FAIL - %s\n", name);
		failed = 1;
	}
}

/* Encodes a plain (ASCII-only, test data) C string into a CHAR16
 * buffer, NUL-terminated - the test's own stand-in for "what real
 * firmware would have put in LoadOptions". Returns the length written,
 * NOT counting the NUL. */
static UINTN encode16(const char *s, CHAR16 *out)
{
	UINTN i = 0;

	while (s[i]) {
		out[i] = (CHAR16)s[i];
		i++;
	}
	out[i] = 0;
	return i;
}

static int matches_ascii(CHAR16 *out, const char *want)
{
	UINTN i;

	for (i = 0; want[i]; i++)
		if (out[i] != (CHAR16)want[i])
			return 0;
	return out[i] == 0;
}

int main(void)
{
	CHAR16 out[512];
	CHAR16 lo[128];
	UINTN lo_len, n;

	/* 1. No load options at all (NULL, len 0) - must be byte-identical
	 * to how this loader behaved before this change (the embedded
	 * cmdline alone, nothing appended). This is the REAL common case:
	 * "Using load options ''" in the user's own pasted boot log. */
	n = combined_cmdline16_len("console=tty0 quiet", 0);
	check("len with no load options", n == strlen("console=tty0 quiet") + 1);
	build_combined_cmdline16("console=tty0 quiet", (CHAR16 *)0, 0, out);
	check("content with no load options is unchanged", matches_ascii(out, "console=tty0 quiet"));

	/* 2. The real motivating case: an operator's own
	 * alpine-zfsboot.console=ttyS1, typed into rEFInd's own "edit boot
	 * options" screen, appended after the embedded cmdline with a
	 * separating space. */
	lo_len = encode16("alpine-zfsboot.console=ttyS1", lo);
	{
		char embedded[] = "console=tty0 console=ttyS0,115200n8 quiet";
		const char *want = "console=tty0 console=ttyS0,115200n8 quiet alpine-zfsboot.console=ttyS1";
		n = combined_cmdline16_len(embedded, lo_len);
		build_combined_cmdline16(embedded, lo, lo_len, out);
		check("length matches expected combined string", n == strlen(want) + 1);
		check("load options appended with exactly one separating space", matches_ascii(out, want));
	}

	/* 3. Defensive: an empty embedded cmdline must not crash or
	 * misbehave (should never happen for real - build.sh always
	 * embeds a real cmdline - but this is pure logic, it must be safe
	 * regardless). */
	n = combined_cmdline16_len("", 0);
	check("empty ascii, no load options -> just a NUL", n == 1);

	/* 4. load_options_strnlen16 stops at an embedded NUL within
	 * max_chars, exactly like strnlen. */
	{
		CHAR16 s[8] = { 'a', 'b', 0, 'c', 'd', 0, 0, 0 };
		n = load_options_strnlen16(s, 8);
		check("load_options_strnlen16 stops at first NUL", n == 2);
	}

	/* 5. Defensive: stops at max_chars if no NUL is found at all - the
	 * real case a LoadOptionsSize that doesn't describe a properly
	 * NUL-terminated string (not every firmware guarantees one) needs
	 * to be handled safely, not walk off the end of the buffer. */
	{
		CHAR16 s[4] = { 'a', 'b', 'c', 'd' };
		n = load_options_strnlen16(s, 4);
		check("load_options_strnlen16 stops at max_chars with no NUL present", n == 4);
	}

	/* 6. NULL load_options (the real "firmware set nothing" case) with
	 * a nonzero max_chars must not dereference it. */
	n = load_options_strnlen16((CHAR16 *)0, 10);
	check("load_options_strnlen16(NULL, ...) is 0, no crash", n == 0);

	/* 7. load_options_len == 0 with a non-NULL load_options pointer
	 * (e.g. firmware set a LoadOptions buffer whose first CHAR16 is
	 * already NUL) must behave exactly like case 1 - no stray leading
	 * space, no garbage appended. */
	{
		CHAR16 empty_lo[1] = { 0 };
		n = combined_cmdline16_len("console=tty0", 0);
		build_combined_cmdline16("console=tty0", empty_lo, 0, out);
		check("zero-length (but non-NULL) load options behaves like none", matches_ascii(out, "console=tty0") && n == strlen("console=tty0") + 1);
	}

	/* 8. PR #16 review (F4): load_options_is_text - plain printable
	 * ASCII (the real, common case: an operator-typed boot option)
	 * passes. */
	{
		CHAR16 s[64];
		UINTN len = encode16("alpine-zfsboot.console=ttyS1,9600n7", s);
		check("load_options_is_text accepts plain ASCII", load_options_is_text(s, len));
	}

	/* 9. A UEFI Shell invocation's own leading image path (backslash,
	 * still plain ASCII) must also pass - this is a real, expected
	 * shape (see main.c's own comment), not something to reject. */
	{
		CHAR16 s[64];
		UINTN len = encode16("\\loader.efi alpine-zfsboot.console=ttyS1", s);
		check("load_options_is_text accepts a UEFI Shell image-path prefix", load_options_is_text(s, len));
	}

	/* 10. Tab is explicitly allowed (the one non-printable exception). */
	{
		CHAR16 s[2] = { (CHAR16)'\t', (CHAR16)'x' };
		check("load_options_is_text accepts an embedded tab", load_options_is_text(s, 2));
	}

	/* 11. Zero length is vacuously text (nothing to reject) - callers
	 * are expected to already skip the append entirely for len 0; this
	 * just documents the function doesn't need a separate guard for it. */
	check("load_options_is_text(_, 0) is text (vacuously)", load_options_is_text((CHAR16 *)0, 0));

	/* 12. The real motivating case: a GUID (or any non-ASCII CHAR16
	 * value) anywhere in the buffer must be rejected - this is
	 * EDK2's own auto-created-option shape (see main.c's comment). */
	{
		CHAR16 s[4] = { (CHAR16)'a', (CHAR16)0x8967, (CHAR16)'b', (CHAR16)'c' };
		check("load_options_is_text rejects a non-ASCII CHAR16 (GUID-shaped)", !load_options_is_text(s, 4));
	}

	/* 13. A C0 control character other than tab (e.g. a raw NUL word
	 * from Dell's full EFI_LOAD_OPTION struct shape) is rejected too. */
	{
		CHAR16 s[3] = { (CHAR16)'a', 0x0001, (CHAR16)'b' };
		check("load_options_is_text rejects a non-tab control character", !load_options_is_text(s, 3));
	}

	printf(failed ? "\n== FAILED ==\n" : "\n== all passed ==\n");
	return failed;
}
