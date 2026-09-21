/*
 * stage1 - the protective-MBR boot sector's own boot code: the first
 * 440 bytes of LBA 0, before the 16-byte protective-MBR GPT partition
 * entry and the 2-byte 0xAA55 signature that make up the rest of the
 * 512-byte sector. Neither of those is written here or by the
 * Makefile - bytes 440-509 in stage1.bin are plain zero; the
 * Makefile's own build only checks that the 0xAA55 signature this
 * file's own `.org 510` places is intact, it doesn't add anything.
 * The real protective-MBR partition entry comes from whatever GPT
 * tool actually partitions a real disk (`sgdisk`, etc. - see the
 * future disk-partitioning step this project doesn't have yet); this
 * file's own install step must only ever overwrite the first 446
 * bytes of LBA 0 (the code portion), never the whole sector, or it
 * would erase that real entry along with the disk's own valid
 * signature.
 *
 * Its ENTIRE job: read a fixed number of sectors starting at LBA 34
 * (immediately after the GPT header + partition array reserved area
 * - a build-time INVARIANT this project's own future disk-
 * partitioning step must always honor, not something discovered at
 * boot) into memory at STAGE2_SEGMENT:0, and jump there. Deliberately
 * NO GPT parsing here at all - that needs real code (see stage2's own
 * gpt.c), and this sector has nowhere near the room for it; stage2,
 * loaded via the one fixed-LBA read below, has vastly more of its own
 * budget to work with.
 *
 * .code16, not .code16gcc - this is pure, standalone, hand-written
 * real-mode code with zero C interop (no calls into or from any
 * gcc-compiled function anywhere in this file), so it has no reason
 * to match gcc -m16's own stack/call-argument conventions.
 */
	.code16
	.section .text
	.global _start

	.set STAGE2_SEGMENT, 0x1000
	.set STAGE2_LBA, 34
	/*
	 * Sectors to read for stage2 - generous headroom over its actual
	 * expected size (stage2 is compact C, nowhere near this large),
	 * not tuned to the exact byte count on purpose: the Makefile
	 * itself fails the build loudly if the real, linked stage2.bin
	 * ever exceeds this, rather than this silently reading too little
	 * and truncating it at boot.
	 */
	.set STAGE2_SECTORS, 64

_start:
	cli
	xorw %ax, %ax
	movw %ax, %ds
	movw %ax, %es
	movw %ax, %ss
	movw $0x7c00, %sp
	sti

	/*
	 * %dl: the BIOS drive number, handed to every boot sector at
	 * entry - untouched by anything above, carried straight through
	 * to stage2 via the same register once we jump there (stage2
	 * itself passes it on to stage2_main() - see stage2_entry.S).
	 */

	/*
	 * Disk Address Packet for INT 13h/AH=42h, built directly on the
	 * stack via a sequence of pushes in reverse field order (each
	 * push decrements SP then writes, so the LAST value pushed ends
	 * up at the LOWEST address - exactly where the packet's own
	 * "size" byte, its first field, belongs) - avoids needing any
	 * separate, statically-addressed buffer for a structure this
	 * short-lived.
	 */
	pushw $0                  /* start_lba bits 63:48 */
	pushw $0                  /* start_lba bits 47:32 */
	pushw $0                  /* start_lba bits 31:16 */
	pushw $STAGE2_LBA         /* start_lba bits 15:0 */
	pushw $STAGE2_SEGMENT     /* buffer_segment */
	pushw $0                  /* buffer_offset */
	pushw $STAGE2_SECTORS     /* num_blocks */
	pushw $0x0010             /* low byte 0x10 = size, high byte 0x00 = reserved */
	movw %sp, %si

	movb $0x42, %ah
	int $0x13
	jc read_failed

	/* Far jump into stage2_entry - %dl (drive number) still intact. */
	ljmp $STAGE2_SEGMENT, $0

read_failed:
	/* No room in this sector for a real message - a single visible
	 * character is the entire diagnostic budget stage1 can afford;
	 * stage2 (see console.c) carries the real, readable error
	 * reporting for everything past this one fixed read. */
	movb $0x0e, %ah
	movb $'!', %al
	xorb %bh, %bh
	int $0x10
1:
	hlt
	jmp 1b

	.org 510
	.word 0xaa55
