/*
 * stage1 - the protective-MBR boot sector's own boot code: the first
 * 440 bytes of LBA 0, before the 16-byte protective-MBR GPT partition
 * entry and the 2-byte 0xAA55 signature that make up the rest of the
 * 512-byte sector. Neither of those is written here or by the
 * Makefile - bytes 440-509 in stage1.bin are plain zero; the
 * Makefile's own build only checks that the 0xAA55 signature this
 * file's own `.org 510` places is intact, it doesn't add anything.
 * The real protective-MBR partition entry comes from whatever GPT
 * tool actually partitions a real disk (`sgdisk`, etc. - see
 * alpine-installer's own alpine-install-zfs.sh, the project that
 * actually writes this); this file's own install step must only ever
 * overwrite the first 440 bytes of LBA 0 (the code portion), never the
 * whole sector, or it would erase that real entry along with the
 * disk's own valid signature.
 *
 * Its ENTIRE job: read a fixed number of sectors starting at LBA 34
 * into memory at STAGE2_SEGMENT:0, and jump there. Deliberately NO
 * GPT/MBR parsing here at all - that needs real code (see stage2's own
 * gpt.c/mbr.c), and this sector has nowhere near the room for it;
 * stage2, loaded via the one fixed-LBA read below, has vastly more of
 * its own budget to work with.
 *
 * The real invariant this depends on, stated precisely: whoever
 * partitions the disk (alpine-install-zfs.sh for a fresh install, or
 * a live in-place migration script for an existing host) must leave
 * LBA 34 through 34+STAGE2_SECTORS-1 genuinely free of any OTHER
 * partition's data, on GPT layouts and msdos/MBR layouts alike - this
 * is not specifically "the GPT header + partition array reserved
 * area" (that's just incidentally where free space naturally starts
 * on a freshly-`sgdisk`-partitioned disk; an msdos/MBR-labeled disk
 * has no such reserved area at all, and the same LBA range still has
 * to be free there too). No partition-table ENTRY needs to exist for
 * this range either way - stage1 never looks at the table, GPT or
 * MBR, at all; see mbr.h's own header comment in this same directory
 * for the identical point made from the MBR side.
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

	/*
	 * Same magic word stage2_entry.S itself places at its own offset 2
	 * (right after its own leading `jmp short` - see that file's own
	 * comment for the full reasoning and why 0x325a specifically) -
	 * checked below before ever jumping into what was just read, so a
	 * "successful" (carry-clear) read that actually landed wrong bytes
	 * (a BIOS bug, disk corruption at this exact LBA range) fails
	 * loudly instead of blindly executing garbage as code.
	 */
	.set STAGE2_MAGIC, 0x325a

	/*
	 * How many times to retry the stage2 read on a genuine BIOS-
	 * reported I/O error (carry set) before giving up - a full source
	 * audit found this was a single, un-retried attempt, unlike every
	 * OTHER real disk read in this project (stage2's own GPT header
	 * read retries GPT_RETRY_ATTEMPTS=10 times for exactly this reason
	 * - a transient INT13h failure on real, especially older/spinning,
	 * hardware is a well-known, real occurrence, not a hypothetical
	 * one). Matches that same constant so this codebase doesn't carry
	 * two different "how many times is reasonable" answers for the
	 * same class of question.
	 */
	.set STAGE2_READ_RETRIES, 10

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

	/*
	 * %bl: retry counter - a register, not %dl/%dh (the BIOS drive
	 * number, which must stay untouched all the way through to the far
	 * jump below) and not %si (points at the DAP for the whole retry
	 * loop - see below for why its num_blocks field is NOT simply
	 * reused unmodified across every retry, despite an earlier version
	 * of this comment claiming it was).
	 */
	movb $STAGE2_READ_RETRIES, %bl

read_retry:
	/*
	 * A follow-up review found a real gap in the ORIGINAL version of
	 * this loop: INT 13h/AH=42h's own documented behavior leaves
	 * whether a BIOS updates the DAP's own num_blocks field
	 * implementation-defined (see disk.c's own disk_read_lba() and
	 * cdrom_disk.c's own int13_read_native() - the two other real DAP
	 * call sites in this tree, both hardened against exactly this
	 * earlier this same session) - on a BIOS that DOES update it, a
	 * failed OR short attempt can leave this packet's own num_blocks
	 * holding fewer sectors than STAGE2_SECTORS. The original loop
	 * built this DAP ONCE and reused it verbatim across every retry -
	 * a corrupted count left behind by a failed attempt would then be
	 * silently RE-ISSUED on the next retry, requesting fewer sectors
	 * than the real stage2 image needs; if THAT attempt then reports
	 * success (CF=0), execution would fall straight through to the
	 * magic-word check below and far-jump into a genuinely TRUNCATED
	 * stage2 image - the magic word lives in stage2's own first
	 * sector, so it would still read back correctly even though the
	 * tail of stage2 was never actually loaded into memory at all.
	 * Resetting num_blocks to the real, full STAGE2_SECTORS at the top
	 * of every single attempt (not just the first) closes this: no
	 * attempt can ever inherit a reduced count from a previous one.
	 */
	movw $STAGE2_SECTORS, 2(%si)
	movb $0x42, %ah
	int $0x13
	jc read_attempt_failed
	/*
	 * Same reasoning as disk.c/cdrom_disk.c's own post-call check -
	 * CF=0 ("success") alone does not prove every requested sector
	 * actually landed; a BIOS that honors this convention would leave
	 * num_blocks holding the real, possibly-short, transferred count
	 * here. Treated identically to a carry-set I/O error - decrement
	 * and retry - rather than immediately failing, since a short
	 * transfer is exactly the kind of thing a transient, retriable
	 * condition (the same real-hardware reasoning STAGE2_READ_RETRIES
	 * itself already exists for) can plausibly produce too.
	 */
	cmpw $STAGE2_SECTORS, 2(%si)
	je read_ok
read_attempt_failed:
	decb %bl
	jnz read_retry
	jmp read_failed

read_ok:
	/*
	 * STAGE2_MAGIC check, at STAGE2_SEGMENT:2 - see stage2_entry.S's
	 * own comment for what's actually placed there and why. %ds is
	 * saved/restored around this rather than left pointed at
	 * STAGE2_SEGMENT - nothing past this point may assume %ds is
	 * anything other than 0 (stage2_entry.S's own invariants start
	 * fresh from its own far jump, but this code hasn't jumped yet).
	 */
	pushw %ds
	movw $STAGE2_SEGMENT, %ax
	movw %ax, %ds
	cmpw $STAGE2_MAGIC, 2
	popw %ds
	jne read_failed

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
