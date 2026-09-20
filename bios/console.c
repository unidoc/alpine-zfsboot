#include "console.h"

void console_putc(char c)
{
	unsigned short ax;

	/*
	 * BIOS teletype output moves to the next line on '\n' but does
	 * NOT return to column 0 on its own on every implementation - a
	 * real, visible symptom the first time this was tried without
	 * this: output stair-stepping rightward across the screen one
	 * line at a time instead of starting each line at the left
	 * margin. Emitting '\r' first fixes it, standard terminal
	 * convention.
	 */
	if (c == '\n')
		console_putc('\r');

	/*
	 * Real-mode BIOS interrupts are NOT bound by the C calling
	 * convention - a given BIOS's own INT 0x10 AH=0x0E implementation
	 * is free to use (and not restore) any general-purpose register
	 * internally, not just BX (already zeroed above for the page
	 * number). Declaring only "bx" as clobbered - as an earlier
	 * version of this function did - tells GCC every OTHER register,
	 * including EBP (a plain general-purpose register here under
	 * -fomit-frame-pointer, which -O2 implies), is safe to keep live
	 * across this call. It is not: a caller that holds a pointer in
	 * EBP across several console_putc() calls (e.g. printing a
	 * multi-character string via console_puts()) can have that
	 * pointer silently corrupted by the BIOS's own internal use of
	 * the same register, with GCC never reloading it because nothing
	 * told it to. Confirmed the hard way: a GPT header pointer held
	 * in EBP across a diagnostic console_puts() call read back
	 * differently than the compare that had just run against the
	 * same memory moments before, with disassembly proving no C code
	 * wrote to it in between - the only remaining actor was this
	 * call. Clobbering every register the BIOS could plausibly touch
	 * (plus "memory", since it may also write through BDA/video RAM
	 * a caller could read) forces GCC to reload everything it needs
	 * from memory after every single character, not just after
	 * console_puts() returns.
	 */
	ax = (unsigned short)(0x0e00 | (unsigned char)c);
	__asm__ __volatile__(
		"xor %%bh, %%bh\n\t"
		"int $0x10\n\t"
		:
		: "a"(ax)
		: "bx", "cx", "dx", "si", "di", "bp", "memory"
	);
}

void console_puts(const char *s)
{
	while (*s) {
		console_putc(*s);
		s++;
	}
}

void console_puts_hex32(unsigned long v)
{
	static const char digits[] = "0123456789abcdef";
	int i;

	console_puts("0x");
	for (i = 28; i >= 0; i -= 4)
		console_putc(digits[(v >> i) & 0xf]);
}

