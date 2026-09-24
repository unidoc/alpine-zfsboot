#include "console.h"

#ifdef ZFSBOOT_TEST_SERIAL
/*
 * Test-only serial mirror of every character this stage prints -
 * gated behind ZFSBOOT_TEST_SERIAL exactly like cdrom_disk.c's own
 * ZFSBOOT_FORCE_ATAPI precedent: never defined by a real build.sh/
 * iso.sh build, only by tests/bios-iso-entry-test.sh's own
 * `make ... TEST_SERIAL=1` invocation. Exists because polling the
 * QEMU monitor's `pmemsave 0xb8000` (where this stage's INT 0x10
 * teletype output ultimately lands, via SeaBIOS's own vgabios) read
 * back completely blank for an entire CI run, three times running -
 * including after two separate real, verified bugs were found and
 * fixed in the TEST's own polling code along the way (see
 * tests/bios-iso-entry-test.sh's own comments for that history) - with
 * a real `info registers` query on the SAME monitor connection proving
 * the guest was genuinely executing far past where a blank screen
 * would imply. Whatever is actually wrong with the VGA capture path in
 * that specific environment, a raw byte stream to COM1 sidesteps it
 * completely: no video BIOS, no VGA device model, no monitor
 * round-trip - nothing for a QEMU/SeaBIOS default this project doesn't
 * control to get in the way of.
 */
static inline void serial_outb(unsigned short port, unsigned char val)
{
	__asm__ __volatile__("outb %0, %%dx" : : "a"(val), "d"(port));
}

static inline unsigned char serial_inb(unsigned short port)
{
	unsigned char val;
	__asm__ __volatile__("inb %%dx, %0" : "=a"(val) : "d"(port));
	return val;
}

#define SERIAL_COM1 0x3f8

static void serial_init(void)
{
	serial_outb(SERIAL_COM1 + 1, 0x00); /* disable UART interrupts */
	serial_outb(SERIAL_COM1 + 3, 0x80); /* DLAB on, to set the baud divisor */
	serial_outb(SERIAL_COM1 + 0, 0x01); /* divisor low byte - 115200 baud */
	serial_outb(SERIAL_COM1 + 1, 0x00); /* divisor high byte */
	serial_outb(SERIAL_COM1 + 3, 0x03); /* 8N1, DLAB off */
	serial_outb(SERIAL_COM1 + 2, 0xc7); /* enable+clear FIFOs, 14-byte threshold */
	serial_outb(SERIAL_COM1 + 4, 0x0b); /* RTS/DTR/OUT2 set */
}

static void serial_putc(char c)
{
	static int initialized;
	unsigned long spins;

	if (!initialized) {
		serial_init();
		initialized = 1;
	}

	/*
	 * Bounded, not an unbounded spin: this stage must never hang
	 * waiting on a port nothing is actually listening to (real
	 * hardware with no serial cable attached, for instance, if this
	 * macro were ever accidentally left on - it isn't, but this
	 * function existing at all should never be able to hang boot).
	 * Losing a byte here is acceptable; losing forward boot progress
	 * is not.
	 */
	for (spins = 0; spins < 100000UL; spins++) {
		if (serial_inb(SERIAL_COM1 + 5) & 0x20) /* THR empty */
			break;
	}
	serial_outb(SERIAL_COM1, (unsigned char)c);
}
#endif

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

#ifdef ZFSBOOT_TEST_SERIAL
	/*
	 * After the '\r'-before-'\n' dispatch above, not before it: that
	 * recursive call mirrors its own '\r' through this same function
	 * first, so placing this here (rather than at the top of the
	 * function) keeps this stage's two output channels in the same
	 * byte order - '\r' then '\n' - instead of serial.log seeing them
	 * backwards.
	 */
	serial_putc(c);
#endif

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
	 *
	 * ES is explicitly saved/restored here too, NOT just added to the
	 * clobber list - GCC's inline-asm clobber list has no way to name
	 * a segment register at all, so there was never a way to tell the
	 * compiler about this the way "bp" etc. are declared above. Found
	 * the same way as the EBP bug this comment already describes, one
	 * register further out: a caller with ATAPI diagnostics gated
	 * behind ZFSBOOT_TEST_SERIAL added a console_puts() call in the
	 * middle of atapi_send_packet()'s data-phase loop, immediately
	 * before that loop's own `rep insw` - which writes through ES:EDI,
	 * not a plain flat pointer, since this stage runs in real mode.
	 * If SeaBIOS's own INT 0x10 handler leaves ES pointing somewhere
	 * else internally, the next `rep insw` scatters a real ATAPI
	 * sector's worth of bytes into whatever ES now is - not a crash at
	 * the call site, but memory corruption far away from it. Confirmed
	 * the hard way (again): with that diagnostic active, this stage
	 * would boot, run one real command successfully, then reprint its
	 * own startup banner and restart stage2_main from the top,
	 * repeatedly - the signature of a stray write landing somewhere
	 * that redirects control flow, not of a hang. Wrapping the BIOS
	 * call in push/pop %es (and %ds, for the same reason, since
	 * nothing in this file currently depends on DS either but a future
	 * caller easily could) made that reproduction disappear.
	 */
	ax = (unsigned short)(0x0e00 | (unsigned char)c);
	__asm__ __volatile__(
		"pushw %%ds\n\t"
		"pushw %%es\n\t"
		"xor %%bh, %%bh\n\t"
		"int $0x10\n\t"
		"popw %%es\n\t"
		"popw %%ds\n\t"
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

