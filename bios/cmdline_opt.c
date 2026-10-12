#include "cmdline_opt.h"

const char *cmdline_value(const char *cmd, const char *key, uint32_t *n)
{
	const char *p = cmd;

	while (*p) {
		const char *k = key, *q = p;

		while (*k && *q == *k) {
			q++;
			k++;
		}
		if (!*k) {
			*n = 0;
			while (q[*n] && q[*n] != ' ' && q[*n] != '\n')
				(*n)++;
			return q;
		}
		while (*p && *p != ' ')
			p++;
		while (*p == ' ')
			p++;
	}
	return 0;
}

int cmdline_parse_chunk(const char *v, uint32_t n, uint32_t max)
{
	uint32_t i, val = 0;

	if (n == 0 || n > 5)
		return -1;
	for (i = 0; i < n; i++) {
		if (v[i] < '0' || v[i] > '9')
			return -1;
		val = val * 10 + (uint32_t)(v[i] - '0');
	}
	if (val < 1 || val > max)
		return -1;
	return (int)val;
}
