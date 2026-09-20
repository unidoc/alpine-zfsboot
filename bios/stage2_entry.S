/*
 * stage2_entry - the very first instruction stage1 jumps to (far
 * jump, CS:IP = STAGE2_SEGMENT:0 - see stage1.S). Everything this
 * whole stage's C code assumes (a single flat segment covering all
 * of its own code+data, near pointers being plain offsets within it,
 * .bss actually being zero) is established right here, once, before
 * any C function ever runs.
 *
 * .code16gcc (not plain .code16): this calls into gcc -m16 compiled C
 * (stage2_main) and must match its calling convention exactly - see
 * switch32.S's own header comment for the identical reasoning there.
 */
	.code16gcc
	/*
	 * Overridable via `as --defsym SEG=...`: the ISO/El-Torito build
	 * (see ../Makefile's stage2-iso.bin target) assembles this exact
	 * same file with SEG=0x7c0 instead - see stage2_main.c's own
	 * STAGE2_SEGMENT comment for why.
	 */
	.ifndef SEG
	.set SEG, 0x1000
	.endif

	.section .text
	.global stage2_entry

stage2_entry:
	/*
	 * Deliberately NO `sti` here, unlike an earlier version of this
	 * file - interrupts stay OFF for this entire stage's execution
	 * (switch32.S's own cli/sti pair around its brief protected-mode
	 * "unreal mode" transition is the one deliberate, narrowly-scoped
	 * exception, unrelated to this). Confirmed the hard way this
	 * matters: with interrupts left on for the whole stage, the BIOS's
	 * own default IRQ0 (timer) handler runs ON WHATEVER STACK IS
	 * ACTIVE AT THE TIME - this stage's own SS:SP, since nothing here
	 * switches to a separate one - and can land in the middle of any
	 * C function, including right between a struct field being
	 * written and read back moments later (observed directly: a GPT
	 * header field read back as mismatched at the exact comparison
	 * instruction, then correctly moments later from the SAME memory,
	 * with the comparison itself confirmed via disassembly to be a
	 * single direct memory-operand instruction - not a compiler-cached
	 * register value a hoisting bug could explain). Nothing this stage
	 * does needs interrupts enabled: every BIOS service it calls
	 * (INT13h in stage1 already ran before we got here; the E820 query
	 * uses INT15h) is a synchronous call that doesn't require IF=1 from
	 * the caller, and ata_atapi.c's own polling now bounds itself on a
	 * plain spin count instead of the BIOS tick counter specifically so
	 * it doesn't need interrupts serviced either.
	 */
	cli
	movw $SEG, %ax
	movw %ax, %ds
	movw %ax, %es
	movw %ax, %ss
	movw $0xfff0, %sp

	/*
	 * .bss (every uninitialized static C variable in this stage -
	 * see stage2_main.c's own buffers) is deliberately NOT part of
	 * the flat binary stage1 loads from disk (link.ld excludes it,
	 * keeping the file small rather than baking in kilobytes of
	 * literal zero bytes) - which means this memory is whatever was
	 * already in RAM at boot otherwise, not necessarily zeroed.
	 * link.ld provides _bss_start/_bss_end so it can be zeroed here,
	 * once, the same guarantee plain C's "uninitialized static
	 * storage starts at zero" semantics normally give for free under
	 * a real OS's own ELF loader - this stage has no such loader, so
	 * it does the equivalent by hand.
	 */
	movl $_bss_start, %edi
	movl $_bss_end, %ecx
	subl %edi, %ecx
	xorb %al, %al
	cld
	/*
	 * addr32: makes this genuinely use EDI/ECX (32-bit) rather than
	 * this segment's own 16-bit default (DI/CX) - see switch32.S's
	 * identical fix for the full reasoning (GAS's .code16gcc mode
	 * doesn't add this automatically for a string instruction's own
	 * implicit addressing). Not currently load-bearing for THIS exact
	 * call (this stage's whole .bss is a few KB, comfortably under
	 * 64KB, so CX alone would already hold the correct count) - kept
	 * anyway so nothing here depends on staying under that limit by
	 * coincidence as this stage's own buffers grow.
	 */
	addr32 rep stosb

	/*
	 * %dl still holds the BIOS drive number stage1 received at boot
	 * and never touched (see that file's own comment) - zero-extend
	 * it to a full 32-bit stack argument for stage2_main(uint8_t),
	 * matching gcc -m16's own stack-argument calling convention.
	 */
	xorl %eax, %eax
	movb %dl, %al
	pushl %eax
	call stage2_main

	/*
	 * stage2_main() never returns on a successful boot (it ends by
	 * jumping into the kernel - see switch32.h's jump_to_kernel) - if
	 * it somehow does anyway (should be unreachable), halting here is
	 * safer than falling through into whatever bytes happen to follow
	 * in memory.
	 */
1:
	hlt
	jmp 1b
