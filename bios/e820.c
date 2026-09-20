#include "e820.h"

uint8_t e820_get_map(struct boot_e820_entry *entries, uint8_t max)
{
	uint32_t continuation = 0;
	uint8_t count = 0;

	while (count < max) {
		struct boot_e820_entry entry;
		uint32_t signature = 0;
		uint32_t bytes_returned = 0;
		uint8_t failed = 0;

		/*
		 * Zeroed first - some real BIOSes only fill the base 20
		 * bytes even when handed room for the optional ACPI 3.0
		 * "extended attributes" dword, leaving anything past that
		 * as whatever was already on the stack. This code only ever
		 * reads the base 20 bytes back out (matches struct
		 * boot_e820_entry's own size exactly), so this is defensive,
		 * not load-bearing - cheap enough to do anyway.
		 */
		entry.addr = 0;
		entry.size = 0;
		entry.type = 0;

		/*
		 * "+b"(continuation): EBX is both input (the BIOS's own
		 * continuation value from the previous call, 0 on the very
		 * first) and output (the value to pass on the NEXT call,
		 * with 0 meaning "that was the last entry") - letting GCC
		 * thread it through one C variable across calls rather than
		 * this code managing EBX by hand.
		 *
		 * "memory" clobber: this asm writes to `entry` via ES:EDI
		 * (the "D" operand only tells GCC that EDI holds that
		 * address, not that the asm dereferences and writes through
		 * it) - without this, nothing guarantees the `entries[count]
		 * = entry` read below actually sees what INT 0x15 just wrote,
		 * the same class of bug disk.c's own disk_read_lba() had
		 * confirmed for real at -O2 (see that file's comment) - not
		 * yet independently reproduced failing here, but the same
		 * missing-clobber pattern, fixed defensively rather than
		 * waiting for it to actually misbehave.
		 */
		__asm__ __volatile__(
			"movl $0x0000e820, %%eax\n\t"
			"movl $0x534d4150, %%edx\n\t" /* 'SMAP' - required input signature */
			"movl $20, %%ecx\n\t"          /* base entry size, no ACPI 3.0 extension requested */
			"int $0x15\n\t"
			"setc %2\n\t"
			"movl %%eax, %0\n\t"
			"movl %%ecx, %1\n\t"
			: "=m"(signature), "=m"(bytes_returned), "=m"(failed), "+b"(continuation)
			: "D"(&entry)
			: "eax", "ecx", "edx", "cc", "memory"
		);

		if (failed || signature != 0x534d4150u)
			break;

		if (bytes_returned >= 20) {
			entries[count] = entry;
			count++;
		}

		if (continuation == 0)
			break;
	}

	return count;
}
