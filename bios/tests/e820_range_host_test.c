/*
 * e820_range_host_test.c - runs e820_range.c (the checks stage2 makes before
 * loading anything) on the host: is a destination range entirely E820 RAM,
 * free of reserved ranges, and where does the kernel live once it runs.
 *
 * Build/run: bios/tests/run-e820-range-host-test.sh
 */
#include <stdio.h>
#include <string.h>

#include "e820.h"

static int failures;
#define CHECK(cond, ...) do { if (!(cond)) { failures++; printf("FAIL: "); } else printf("ok: "); printf(__VA_ARGS__); printf("\n"); } while (0)

#define RAM 1
#define RES 2
#define ACPI 3

#define KERNEL_AT 0x00100000ULL
#define INITRD_AT 0x04000000ULL

int main(void)
{
	/* What QEMU/SeaBIOS reports with -m 128 (sorted, the usual shape). */
	struct boot_e820_entry q128[] = {
		{ 0x0, 0x9fc00, RAM },
		{ 0x9fc00, 0x400, RES },
		{ 0xf0000, 0x10000, RES },
		{ 0x100000, 0x7ee0000, RAM }, /* up to 0x7fe0000 */
		{ 0x7fe0000, 0x20000, RES },
		{ 0xfffc0000, 0x40000, RES },
	};
	uint64_t bad = 0;
	int r;

	r = e820_range_check(q128, 6, KERNEL_AT, 20u << 20, &bad);
	CHECK(r == E820_RANGE_OK, "a 20 MiB kernel at 1 MiB is RAM on a 128 MiB machine");
	r = e820_range_check(q128, 6, INITRD_AT, 12u << 20, &bad);
	CHECK(r == E820_RANGE_OK, "a 12 MiB initrd at 64 MiB is RAM on a 128 MiB machine");
	r = e820_range_check(q128, 6, INITRD_AT, 0x41cc5d8, &bad);
	CHECK(r == E820_RANGE_BEYOND_RAM && bad == 0x7fe0000,
	      "a 66 MiB initrd at 64 MiB runs past the end of RAM on a 128 MiB machine: BEYOND_RAM at 0x7fe0000 (got %d at %#llx)", r,
	      (unsigned long long)bad);
	r = e820_range_check(q128, 6, 0x9f000, 0x2000, &bad);
	CHECK(r == E820_RANGE_NOT_RAM && bad == 0x9fc00, "a range running into the EBDA stops where RAM ends (%#llx)",
	      (unsigned long long)bad);
	r = e820_range_check(q128, 6, 0xa0000, 0x1000, &bad);
	CHECK(r == E820_RANGE_NOT_RAM && bad == 0xa0000, "the VGA hole is not RAM");
	r = e820_range_check(q128, 6, KERNEL_AT, 0, &bad);
	CHECK(r == E820_RANGE_OK, "an empty range is trivially fine");
	r = e820_range_check(q128, 0, KERNEL_AT, 0x1000, &bad);
	CHECK(r == E820_RANGE_BEYOND_RAM, "an empty map has no RAM at all");
	r = e820_range_check(q128, 6, 0xfffffffffffff000ULL, 0x2000, &bad);
	CHECK(r == E820_RANGE_NOT_RAM, "a range that wraps past 2^64 is refused");

	/* RAM reported in several adjacent and overlapping pieces, out of order. */
	{
		struct boot_e820_entry m[] = {
			{ 0x8000000, 0x8000000, RAM },
			{ 0x100000, 0x1f00000, RAM },
			{ 0x0, 0x9fc00, RAM },
			{ 0x2000000, 0x6800000, RAM }, /* overlaps the next one */
			{ 0x6000000, 0x2000000, RAM },
		};
		r = e820_range_check(m, 5, KERNEL_AT, 0xff00000, &bad);
		CHECK(r == E820_RANGE_OK, "adjacent/overlapping/unsorted RAM entries together cover 1 MiB..256 MiB");
	}

	/* A hole between two RAM entries. */
	{
		struct boot_e820_entry m[] = {
			{ 0x100000, 0x3f00000, RAM },  /* to 0x4000000 */
			{ 0x4100000, 0x10000000, RAM }, /* hole 0x4000000..0x4100000 */
		};
		r = e820_range_check(m, 2, INITRD_AT, 0x200000, &bad);
		CHECK(r == E820_RANGE_NOT_RAM && bad == 0x4000000, "a hole below the top of RAM is NOT_RAM (warn only), found at its start (%#llx)",
		      (unsigned long long)bad);
	}

	/* A firmware that reports a reserved (or ACPI) range INSIDE a RAM entry. */
	{
		struct boot_e820_entry m[] = {
			{ 0x100000, 0x20000000, RAM },
			{ 0x5000000, 0x10000, ACPI },
		};
		r = e820_range_check(m, 2, INITRD_AT, 0x2000000, &bad);
		CHECK(r == E820_RANGE_RESERVED && bad == 0x5000000, "a reserved range inside RAM still refuses the initrd (%#llx)",
		      (unsigned long long)bad);
		r = e820_range_check(m, 2, KERNEL_AT, 0x1000000, &bad);
		CHECK(r == E820_RANGE_OK, "a range that ends before the reserved one is fine");
		r = e820_range_check(m, 2, 0x4ff0000, 0x10000, &bad);
		CHECK(r == E820_RANGE_OK, "a range that ends exactly where the reserved one starts is fine");
	}

	/* Unusual but valid layouts that must not be refused ("boots as before"). */
	{
		/* RAM split around a 1 MiB ACPI NVS hole inside the initrd range:
		 * RESERVED (warn), never BEYOND_RAM. */
		struct boot_e820_entry m[] = {
			{ 0x0, 0x9fc00, RAM },
			{ 0x100000, 0x4400000, RAM },
			{ 0x4500000, 0x100000, 4 /* ACPI NVS */ },
			{ 0x4600000, 0x40000000, RAM },
		};
		r = e820_range_check(m, 4, INITRD_AT, 0x41cc5d8, &bad);
		CHECK(r == E820_RANGE_NOT_RAM || r == E820_RANGE_RESERVED, "an ACPI NVS hole inside the initrd range: warn only (got %d)", r);
		CHECK(r != E820_RANGE_BEYOND_RAM, "...never the refusing BEYOND_RAM");
	}
	{
		/* Many tiny RAM entries, out of order, plus RAM above 4 GiB. */
		struct boot_e820_entry m[] = {
			{ 0x100000000ULL, 0x100000000ULL, RAM },
			{ 0x2000000, 0x6000000, RAM },
			{ 0x100000, 0xf00000, RAM },
			{ 0x1000000, 0x1000000, RAM },
			{ 0x0, 0x9fc00, RAM },
		};
		r = e820_range_check(m, 5, KERNEL_AT, 0x7f00000, &bad);
		CHECK(r == E820_RANGE_OK, "fragmented, unsorted RAM covering 1..128 MiB: OK");
	}
	{
		/* Firmware reporting a type-1 entry of size 0 and duplicate entries. */
		struct boot_e820_entry m[] = {
			{ 0x100000, 0, RAM },
			{ 0x100000, 0x10000000, RAM },
			{ 0x100000, 0x10000000, RAM },
		};
		r = e820_range_check(m, 3, INITRD_AT, 0x4000000, &bad);
		CHECK(r == E820_RANGE_OK, "zero-sized and duplicate RAM entries: OK");
	}

	/* Overlap helper. */
	CHECK(e820_ranges_overlap(0x1000000, 0x2c30000, INITRD_AT, 0x100000) == 0, "runtime area ending at 0x3c30000 misses the initrd");
	CHECK(e820_ranges_overlap(0x1000000, 0x3000001, INITRD_AT, 0x100000) == 1, "one byte into the initrd overlaps");
	CHECK(e820_ranges_overlap(0x1000000, 0x3000000, INITRD_AT, 0x100000) == 0, "ending exactly at the initrd does not");
	CHECK(e820_ranges_overlap(0x1000, 0, 0x1000, 0x10) == 0, "an empty range overlaps nothing");

	/* Where the kernel lives once it runs (boot.rst, init_size). */
	{
		struct setup_header h;
		uint64_t start = 0, len = 0;

		memset(&h, 0, sizeof(h));
		/* The Alpine linux-lts 6.18 bzImage this project ships (read from its header). */
		h.version = 0x020f;
		h.relocatable_kernel = 1;
		h.kernel_alignment = 0x1000000;
		h.pref_address = 0x1000000;
		h.init_size = 0x2c30000;
		CHECK(kernel_runtime_range(&h, KERNEL_AT, &start, &len) == 0 && start == 0x1000000 && len == 0x2c30000,
		      "Alpine lts kernel: runs at 16 MiB for 0x2c30000 bytes (got %#llx+%#llx)", (unsigned long long)start,
		      (unsigned long long)len);
		CHECK(!e820_ranges_overlap(start, len, INITRD_AT, 0x41cc5d8), "...which ends below the initrd at 64 MiB");

		h.init_size = 0x3100000;
		kernel_runtime_range(&h, KERNEL_AT, &start, &len);
		CHECK(e820_ranges_overlap(start, len, INITRD_AT, 0x41cc5d8), "a kernel with init_size 0x3100000 would overwrite the initrd");

		h.init_size = 0x1000000;
		h.pref_address = 0x100000;
		h.kernel_alignment = 0x200000;
		kernel_runtime_range(&h, KERNEL_AT, &start, &len);
		CHECK(start == 0x200000, "relocatable, pref below the alignment: 1 MiB aligned up to 2 MiB (%#llx)", (unsigned long long)start);

		h.relocatable_kernel = 0;
		h.pref_address = 0x1000000;
		kernel_runtime_range(&h, KERNEL_AT, &start, &len);
		CHECK(start == 0x1000000, "not relocatable: pref_address (%#llx)", (unsigned long long)start);

		h.version = 0x0209;
		CHECK(kernel_runtime_range(&h, KERNEL_AT, &start, &len) == -1, "boot protocol 2.09: not known, not checked");
		h.version = 0x020f;
		h.init_size = 0;
		CHECK(kernel_runtime_range(&h, KERNEL_AT, &start, &len) == -1, "init_size 0: not known, not checked");
	}

	if (failures) {
		printf("\n%d check(s) FAILED\n", failures);
		return 1;
	}
	printf("\nall checks passed\n");
	return 0;
}
