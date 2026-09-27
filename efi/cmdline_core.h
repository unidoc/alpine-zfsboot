#ifndef ALPINE_ZFSBOOT_CMDLINE_CORE_H
#define ALPINE_ZFSBOOT_CMDLINE_CORE_H

#ifdef ALPINE_ZFSBOOT_CMDLINE_TEST
typedef unsigned short CHAR16;
typedef char CHAR8;
typedef unsigned long UINTN;
#else
#include <efi.h>
#endif

/*
 * Pure buffer logic for combining this loader's own embedded ASCII
 * cmdline with firmware-supplied CHAR16 LoadOptions (whatever boot
 * manager loaded THIS image passed - e.g. rEFInd's own "edit boot
 * options" screen). No EFI calls, no allocation - every function here
 * just reads/writes caller-supplied buffers, so this can be built and
 * tested with a plain host C compiler (-DALPINE_ZFSBOOT_CMDLINE_TEST,
 * no gnu-efi headers needed at all - see cmdline_core_test.c) as well
 * as the real freestanding EFI build.
 */

/*
 * Counts CHAR16 units in `s` up to `max_chars`, stopping at the first
 * NUL - defensive against a LoadOptionsSize that doesn't actually
 * describe a NUL-terminated string (not every firmware guarantees
 * one). Returns 0 if s is NULL or max_chars is 0.
 */
UINTN load_options_strnlen16(CHAR16 *s, UINTN max_chars);

/*
 * Returns the CHAR16 capacity build_combined_cmdline16 needs
 * (INCLUDING the trailing NUL) for the given inputs.
 */
UINTN combined_cmdline16_len(CHAR8 *ascii, UINTN load_options_len);

/*
 * Writes ascii (widened to CHAR16) into out, then - only if
 * load_options_len > 0 - a single space followed by load_options
 * itself (already CHAR16, copied verbatim), then a NUL terminator.
 * `out` must be at least combined_cmdline16_len(ascii, load_options_len)
 * CHAR16 units. Returns the number of CHAR16 units written, INCLUDING
 * the trailing NUL (same count combined_cmdline16_len returns, so a
 * caller can use either one consistently for LoadOptionsSize).
 */
UINTN build_combined_cmdline16(CHAR8 *ascii, CHAR16 *load_options, UINTN load_options_len, CHAR16 *out);

#endif
