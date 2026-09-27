#include "cmdline_core.h"

UINTN load_options_strnlen16(CHAR16 *s, UINTN max_chars)
{
	UINTN n = 0;

	if (s == 0)
		return 0;
	while (n < max_chars && s[n] != 0)
		n++;
	return n;
}

static UINTN ascii_strlen(CHAR8 *ascii)
{
	UINTN n = 0;

	while (ascii[n] != 0)
		n++;
	return n;
}

UINTN combined_cmdline16_len(CHAR8 *ascii, UINTN load_options_len)
{
	UINTN ascii_len = ascii_strlen(ascii);

	if (load_options_len == 0)
		return ascii_len + 1;
	/* +1 separating space, +1 trailing NUL */
	return ascii_len + 1 + load_options_len + 1;
}

UINTN build_combined_cmdline16(CHAR8 *ascii, CHAR16 *load_options, UINTN load_options_len, CHAR16 *out)
{
	UINTN i, pos = 0;

	for (i = 0; ascii[i] != 0; i++)
		out[pos++] = (CHAR16)ascii[i];

	if (load_options_len > 0) {
		out[pos++] = (CHAR16)' ';
		for (i = 0; i < load_options_len; i++)
			out[pos++] = load_options[i];
	}

	out[pos] = 0;
	return pos + 1;
}
