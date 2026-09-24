#include "e820.h"

/*
 * A bound on BIOS CALLS, deliberately separate from `count` (entries
 * actually accepted) or `max` (the caller's own buffer size) - a full
 * source audit found the loop below had no other way to stop if a
 * malformed BIOS kept reporting bytes_returned < 20 (the "skip this
 * entry" branch, which never advances count) together with a nonzero
 * continuation value on every call: count never reaches max, and
 * continuation never legitimately becomes 0 either, so the loop spun
 * forever. Real BIOSes report on the order of a few dozen entries at
 * most (confirmed against this project's own real Hetzner CAX/QEMU
 * testing) - generous enough to never bound a real, well-behaved BIOS,
 * while still giving every call a hard ceiling regardless of what a
 * malfunctioning one does.
 */
#define E820_MAX_CALLS 256

uint8_t e820_get_map(struct boot_e820_entry *entries, uint8_t max)
{
	uint32_t continuation = 0;
	uint8_t count = 0;
	uint16_t calls = 0;

	while (count < max) {
		if (calls++ >= E820_MAX_CALLS)
			break;

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
		 *
		 * "esi"/"ebp" clobbers: a full source audit found this was the
		 * one BIOS-call site in the tree that hadn't gotten the same
		 * treatment console.c's own console_putc() already carries a
		 * detailed comment about, from a real, confirmed prior
		 * hardware finding there - real-mode BIOS interrupts are NOT
		 * bound by the C calling convention, and a given BIOS's own
		 * INT 0x15 implementation is free to use (and not restore) any
		 * general-purpose register internally, EBP included (a plain
		 * GPR here under -fomit-frame-pointer, which -O2 implies).
		 * "edi" is deliberately NOT also added here, despite being the
		 * exact same class of register - it doesn't need to be: EDI is
		 * already the "D" input operand below, and GCC's own semantics
		 * for a plain (non-"+", non-"=") input operand already treat
		 * its register as dead/undefined immediately after the asm
		 * returns, with nothing relying on it surviving - unlike
		 * esi/ebp (which carry no operand role at all here, and could
		 * otherwise hold some OTHER live C variable silently corrupted
		 * by the BIOS's own internal use of the same register), there
		 * is no live value in EDI for the BIOS to corrupt in the first
		 * place. Confirmed empirically, not just reasoned: adding
		 * "edi" here on top of the "D" constraint made this exact asm
		 * block fail to compile at the real -O2 flags this Makefile
		 * uses ("asm operand has impossible constraints or there are
		 * not enough registers") - this file's small, tightly-packed
		 * operand set genuinely has no register budget left over for a
		 * redundant clobber that adds no real safety here.
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
			: "eax", "ecx", "edx", "esi", "ebp", "cc", "memory"
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
