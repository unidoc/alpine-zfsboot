#include "e820.h"

/*
 * Memory-layout checks stage2 runs BEFORE it loads anything (see
 * stage2_main.c): kernel and initrd go to fixed physical addresses, and
 * nothing used to confirm that the firmware's E820 map actually reports
 * those addresses as usable RAM. Pure C, no BIOS calls, so
 * bios/tests/e820_range_host_test.c runs exactly this code.
 */

#define U64_MAX 0xffffffffffffffffULL

static uint64_t end_of(uint64_t addr, uint64_t size)
{
	return (addr + size < addr) ? U64_MAX : addr + size;
}

int e820_ranges_overlap(uint64_t a, uint64_t alen, uint64_t b, uint64_t blen)
{
	if (alen == 0 || blen == 0)
		return 0;
	return a < end_of(b, blen) && b < end_of(a, alen);
}

/*
 * [start, start+len) must be covered by the union of type-1 (RAM) entries -
 * several adjacent or overlapping RAM entries together are fine - and must
 * not overlap any entry of another type: where a firmware reports the same
 * address as both, Linux treats it as reserved, and so does this check.
 */
int e820_range_check(const struct boot_e820_entry *map, uint8_t n, uint64_t start, uint64_t len, uint64_t *bad)
{
	uint64_t end = start + len, cur = start;
	uint8_t i;

	*bad = start;
	if (len == 0)
		return E820_RANGE_OK;
	if (end < start)
		return E820_RANGE_NOT_RAM; /* wraps past 2^64: cannot be RAM */

	while (cur < end) {
		uint64_t best = cur;

		for (i = 0; i < n; i++) {
			uint64_t e_end = end_of(map[i].addr, map[i].size);
			if (map[i].type != E820_TYPE_RAM || map[i].size == 0)
				continue;
			if (map[i].addr <= cur && cur < e_end && e_end > best)
				best = e_end;
		}
		if (best == cur) {
			uint64_t top = 0;

			*bad = cur;
			for (i = 0; i < n; i++)
				if (map[i].type == E820_TYPE_RAM && end_of(map[i].addr, map[i].size) > top)
					top = end_of(map[i].addr, map[i].size);
			/* Above every RAM entry: there is certainly no RAM there. A
			 * hole BELOW the top could be a firmware that left something
			 * out, so it is only E820_RANGE_NOT_RAM. */
			return cur >= top ? E820_RANGE_BEYOND_RAM : E820_RANGE_NOT_RAM;
		}
		cur = best;
	}

	for (i = 0; i < n; i++) {
		if (map[i].type == E820_TYPE_RAM)
			continue;
		if (e820_ranges_overlap(start, len, map[i].addr, map[i].size)) {
			*bad = map[i].addr > start ? map[i].addr : start;
			return E820_RANGE_RESERVED;
		}
	}
	return E820_RANGE_OK;
}

/*
 * Where the kernel itself will live once it runs: init_size bytes from its
 * "runtime start address", which Documentation/arch/x86/boot.rst (init_size)
 * defines as
 *
 *	if (relocatable_kernel) {
 *		if (load_address < pref_address)
 *			load_address = pref_address;
 *		runtime_start = align_up(load_address, kernel_alignment);
 *	} else {
 *		runtime_start = pref_address;
 *	}
 *
 * The kernel's decompressor writes all of that range before it has looked
 * at the memory map, so it has to be RAM and must not hold the initrd. Only
 * known from boot protocol 2.10 on (pref_address/init_size); returns -1 for
 * an older kernel, which this loader then does not check.
 */
int kernel_runtime_range(const struct setup_header *h, uint32_t load_addr, uint64_t *start, uint64_t *len)
{
	uint64_t a;

	if (h->version < 0x020a || h->init_size == 0)
		return -1;
	if (h->relocatable_kernel) {
		a = load_addr;
		if (a < h->pref_address)
			a = h->pref_address;
		if (h->kernel_alignment != 0 && (h->kernel_alignment & (h->kernel_alignment - 1)) == 0)
			a = (a + h->kernel_alignment - 1) & ~((uint64_t)h->kernel_alignment - 1);
	} else {
		a = h->pref_address;
	}
	*start = a;
	*len = h->init_size;
	return 0;
}
