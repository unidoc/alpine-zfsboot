#ifndef ZFSBOOT_BIOS_CMDLINE_OPT_H
#define ZFSBOOT_BIOS_CMDLINE_OPT_H

#include "stdint_local.h"

/*
 * The alpine-zfsboot.* keys stage2 reads from EFI/ALPINE/CMDLINE (diag,
 * int13chunk, integrity). Pure C, host-tested
 * (bios/tests/cmdline_opt_host_test.c).
 */

/* The value of `key` (e.g. "alpine-zfsboot.diag=") in the NUL-terminated
 * command line, as a pointer and length (*n), or 0 when absent. Only a
 * whole space-separated word that starts with key counts. */
const char *cmdline_value(const char *cmd, const char *key, uint32_t *n);

/* A decimal 1..max (n bytes at v, digits only, no sign, no leading '+'),
 * or -1. */
int cmdline_parse_chunk(const char *v, uint32_t n, uint32_t max);

#endif
