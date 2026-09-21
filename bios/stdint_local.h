#ifndef ZFSBOOT_BIOS_STDINT_LOCAL_H
#define ZFSBOOT_BIOS_STDINT_LOCAL_H

/*
 * This project's own fixed-width integer typedefs, used everywhere in
 * bios/ instead of the system's <stdint.h> - deliberately, not out of
 * NIH reflex. Confirmed the hard way: under -m16 (this whole stage's
 * real-mode code generation mode, see Makefile's own comment), musl's
 * <stdint.h> (Alpine's libc) defines uint64_t as plain "unsigned
 * long" - correct on a normal 64-bit build, where long is 8 bytes,
 * but -m16 shrinks long to 4 bytes (confirmed via gcc's own
 * __SIZEOF_LONG__ macro) without musl's typedef following it down to
 * "unsigned long long" (still genuinely 8 bytes under -m16, confirmed
 * separately). glibc's <stdint.h> (used when building this same code
 * on a Debian machine, as part of this investigation) does not have
 * this problem - it defines uint64_t via the compiler's own
 * __UINT64_TYPE__ macro, which -m16 DOES correctly keep pointed at
 * "long long unsigned int". The result on musl: every uint64_t field
 * in every struct in this directory (struct gpt_header first noticed,
 * but every *.h here uses uint64_t) silently becomes 4 bytes instead
 * of 8, with no compiler warning at all - every multi-field struct
 * with more than one 64-bit member ends up laid out wrong, byte for
 * byte, from that field on.
 *
 * Only the four unsigned widths below are ever used anywhere in this
 * directory (no signed fixed-width type, no 128-bit, nothing else) -
 * this header defines exactly those, from plain built-in types whose
 * width under -m16 was directly verified (not assumed) on both
 * toolchains this project has actually been built with.
 */
typedef unsigned char uint8_t;
typedef unsigned short uint16_t;
typedef unsigned int uint32_t;
typedef unsigned long long uint64_t;

#endif
