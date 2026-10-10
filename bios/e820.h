#ifndef ZFSBOOT_BIOS_E820_H
#define ZFSBOOT_BIOS_E820_H

#include "stdint_local.h"
#include "bootparams.h"

/*
 * Gathers the BIOS memory map via INT 0x15, EAX=0xE820 - real mode,
 * no GDT tricks needed (see switch32.h for the two things that DO
 * need those). Fills `entries` (caller-supplied, up to `max` long,
 * verbatim - every entry type BIOS reports, not just usable RAM: the
 * kernel itself needs the full map, reserved/ACPI regions included,
 * to avoid treating them as usable) and returns the number of
 * entries actually filled, or 0 if even the first call fails (a BIOS
 * old enough to lack E820 support entirely - not something this code
 * has a fallback for; every target this project cares about, real
 * hardware and Proxmox/SeaBIOS alike, supports it).
 */
uint8_t e820_get_map(struct boot_e820_entry *entries, uint8_t max);

/* e820_range.c - pure checks, host-tested (bios/tests/e820_range_host_test.c). */
#define E820_RANGE_OK 0
#define E820_RANGE_NOT_RAM 1  /* part of the range is not reported as RAM at all */
#define E820_RANGE_RESERVED 2 /* the range overlaps a non-RAM (reserved, ACPI, ...) entry */
#define E820_RANGE_BEYOND_RAM 3 /* part of the range lies above the end of the highest RAM entry */

/* E820_RANGE_* for [start, start+len); *bad = the first offending address. */
int e820_range_check(const struct boot_e820_entry *map, uint8_t n, uint64_t start, uint64_t len, uint64_t *bad);

/* Nonzero when [a, a+alen) and [b, b+blen) share at least one byte. */
int e820_ranges_overlap(uint64_t a, uint64_t alen, uint64_t b, uint64_t blen);

/* The range the kernel occupies once it runs (init_size from its runtime
 * start address, see e820_range.c); 0 = filled in, -1 = the kernel's boot
 * protocol predates those fields. */
int kernel_runtime_range(const struct setup_header *h, uint32_t load_addr, uint64_t *start, uint64_t *len);

#endif
