#include "disk.h"

static uint8_t g_drive_number;

void disk_init(uint8_t drive_number)
{
	g_drive_number = drive_number;
}

/*
 * The "disk address packet" INT 0x13, AH=0x42 reads its arguments
 * from - exact on-disk/in-memory layout the BIOS service itself
 * defines (Ralf Brown's Interrupt List, INT 13h/AH=42h), not this
 * project's own choice.
 */
struct disk_address_packet {
	uint8_t size;         /* sizeof(this struct) = 0x10 */
	uint8_t reserved;     /* must be 0 */
	uint16_t num_blocks;  /* sectors to transfer */
	uint16_t buffer_offset;
	uint16_t buffer_segment;
	uint64_t start_lba;
} __attribute__((packed));

/*
 * Same guard as gpt.c's own GPT header struct - a wrong-sized
 * uint64_t here silently corrupts the on-the-wire DAP BIOS actually
 * reads (see cdrom_disk_int13.c's own header comment for the real
 * bug this class of mistake caused once, for the exact same struct
 * shape, on the CD-ROM boot path).
 */
_Static_assert(sizeof(struct disk_address_packet) == 16,
               "disk_address_packet must be exactly 16 bytes (BIOS AH=0x42 DAP format)");

int disk_read_lba(uint64_t lba, uint16_t count, void *buf)
{
	struct disk_address_packet dap;
	uint16_t buf_off, dap_off, ds_seg;
	uint8_t failed;

	/*
	 * buf is a plain near pointer into this stage's own data area -
	 * under -m16, GCC's pointers are still 32-bit values, but only
	 * the low 16 bits matter here (this stage's whole code+data
	 * fits well under 64KB - see link.ld's own layout comment), which
	 * is exactly the real-mode segment:offset split this BIOS service
	 * itself expects: the segment half is simply the CURRENT %ds
	 * (stage2_entry.S sets %ds once, before calling into any C code
	 * here, to cover this stage's whole data area - see its own
	 * comment), read directly here rather than assumed to be some
	 * fixed constant.
	 */
	buf_off = (uint16_t)(unsigned long)buf;
	dap_off = (uint16_t)(unsigned long)&dap;
	__asm__ __volatile__("mov %%ds, %0" : "=r"(ds_seg));

	dap.size = sizeof(dap);
	dap.reserved = 0;
	dap.num_blocks = count;
	dap.buffer_offset = buf_off;
	dap.buffer_segment = ds_seg;
	dap.start_lba = lba;

	/*
	 * DS:SI -> the DAP, AH=0x42, DL = drive number, INT 0x13. Carry
	 * flag set on error (BIOS convention for every INT 13h service) -
	 * read via SETC into a plain byte rather than relying on the
	 * flag surviving back out to C, since nothing about inline asm's
	 * "cc" clobber promises flags are still readable after the asm
	 * block ends.
	 *
	 * "memory" clobber - NOT optional. This asm only takes `dap_off`
	 * (a plain integer address) as an operand, with no way for GCC to
	 * see that INT 0x13 itself reads the DAP bytes at that address -
	 * confirmed the hard way, by actually building this at the real
	 * -O2 the Makefile uses and disassembling the result: GCC's own
	 * alias analysis concluded every `dap.*` field store above was
	 * dead code (nothing "observably" read them) and deleted all six
	 * of them entirely, leaving SI pointing at whatever garbage
	 * happened to already be on the stack. A "memory" clobber tells
	 * GCC this asm may read/write memory it can't enumerate, which is
	 * exactly true here and is what makes it keep the stores - the
	 * `-O0` build (which happened to keep them anyway, since -O0
	 * barely optimizes at all) never actually exercised this bug,
	 * which is why it went unnoticed until built the same way the
	 * real Makefile does.
	 *
	 * "ebp" clobber (F18, unidoc-alip's PR #5 review): the same real
	 * hardware finding console.c's own console_putc() and e820.c's own
	 * INT 0x15 call already carry a clobber for - a BIOS's own
	 * interrupt handler is free to use, and not restore, EBP
	 * internally, a plain GPR here under -fomit-frame-pointer. "esi"
	 * is deliberately NOT also added - unlike e820.c's case, this asm
	 * already explicitly preserves it itself (push/pop around the
	 * call), a stronger guarantee than a clobber would add (the real
	 * original value survives, not just "GCC no longer trusts it").
	 * Confirmed to compile cleanly at this file's own real -O2 build
	 * flags and boot for real under QEMU (tests/bios-hdd-entry-test.sh)
	 * - not just reasoned about.
	 */
	__asm__ __volatile__(
		"push %%si\n\t"
		"mov %1, %%si\n\t"
		"movb $0x42, %%ah\n\t"
		"int $0x13\n\t"
		"setc %0\n\t"
		"pop %%si\n\t"
		: "=q"(failed)
		: "r"(dap_off), "d"(g_drive_number)
		: "ah", "ebp", "cc", "memory"
	);

	if (failed)
		return -1;

	/*
	 * Not every real BIOS updates the DAP's own sector-count field to
	 * reflect a SHORT transfer (INT 13h/AH=42h's own documented
	 * behavior leaves this implementation-defined - the carry flag is
	 * the one universally-reliable signal, already checked above) -
	 * but on the ones that DO, this is a real, free extra check: safe
	 * either way, since dap.num_blocks was set to `count` as an INPUT
	 * before the call, so a BIOS that never touches it leaves this
	 * comparison trivially true. Reading it back here (rather than
	 * trusting the local `count` parameter) is what actually proves
	 * the read - a BIOS that reports success (CF clear) but silently
	 * transferred fewer sectors than requested used to be
	 * indistinguishable from a genuine full read, with the caller
	 * treating a short/corrupt buffer as complete, real disk content.
	 */
	if (dap.num_blocks != count)
		return -1;

	return 0;
}
