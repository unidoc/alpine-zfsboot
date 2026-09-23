/*
 * stage2_entry - the very first instruction control reaches this
 * stage at. Everything this whole stage's C code assumes (a single
 * flat segment covering all of its own code+data, near pointers being
 * plain offsets within it, .bss actually being zero) is established
 * right here, once, before any C function ever runs.
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
	 * A 2-byte `jmp short` past a 2-byte magic word, before anything
	 * else - a full source audit found stage1.S's own fixed-LBA read
	 * of this whole image had no way to tell "the BIOS reported success
	 * (carry clear) but the bytes landed wrong anyway" (a real, if rare,
	 * possibility: a BIOS bug, an unlucky disk-controller glitch, plain
	 * corruption at that exact LBA range) from a genuine, correct
	 * stage2 load - it would far-jump straight into whatever was
	 * actually there and execute it as code regardless, for the single
	 * most boot-critical handoff in this entire project. stage1.S now
	 * checks this exact magic word (at STAGE2_SEGMENT:2, right where
	 * this places it) before ever jumping in - see that file's own
	 * comment for the read-side half of this fix. Harmless for the ISO/
	 * El-Torito build too (SEG=0x7c0, entered directly by BIOS, no
	 * stage1 of its own, nothing there ever reads this word) - the jump
	 * still executes first either way, identical to before this fix,
	 * just 4 bytes later in the file; nothing here changes any
	 * link-time-computed offset's own MEANING, only its numeric value,
	 * which every reference to it already resolves automatically.
	 * 0x325A is arbitrary but deliberately not 0x0000/0xFFFF (both
	 * common "uninitialized/erased flash" patterns that could
	 * coincidentally already be sitting at this offset on a genuinely
	 * corrupt/blank read, which would otherwise defeat the whole point
	 * of this check).
	 */
	jmp past_stage2_magic
	.word 0x325a
past_stage2_magic:
	/*
	 * Explicit far jump to reload %cs = SEG, BEFORE anything else -
	 * this stage OWNS the CS=SEG invariant every near call/jmp and
	 * every "near pointer" (a plain link-time offset, e.g. a C
	 * function passed as a callback) implicitly depends on, rather
	 * than trusting whoever handed off control to have already set it
	 * up correctly.
	 *
	 * Real, found-the-hard-way bug this closes: stage1.S's own
	 * handoff to the disk build (STAGE2_SEGMENT=0x1000) IS a real far
	 * jump with an explicit CS:IP = SEG:0 target (see stage1.S), so
	 * %cs was already correct there - this instruction is a pure
	 * no-op transition for that build, CS:IP goes from SEG:0 to
	 * SEG:(next instruction), nothing changes. The ISO/El-Torito
	 * build (SEG=0x7c0) has no stage1 of its own - BIOS hands off
	 * directly to this exact symbol - and BIOS's El Torito
	 * "no emulation" handoff uses the same convention as a classic
	 * MBR boot sector: %cs:%ip = 0x0000:0x7c00, NOT 0x07c0:0x0000.
	 * Both address the identical PHYSICAL byte, so every direct/
	 * PC-relative call or jump in this whole stage kept working
	 * completely normally (their displacements are self-relative,
	 * entirely independent of what %cs actually is) - which is
	 * exactly why this went unnoticed through GPT parsing, FAT mount,
	 * file lookups, and the kernel-header read. It only broke the one
	 * place code needs an ABSOLUTE near-pointer value (a link-time
	 * offset baked in as a plain immediate, e.g. fat_read_range()'s
	 * own `progress` callback argument in fat.c, passed as
	 * `progress_dot` - just that function's own offset, 0x6c here)
	 * to actually resolve against %cs: with %cs left at BIOS's 0x0000
	 * instead of this stage's own SEG (0x7c0), `call progress_dot`'s
	 * indirect near call jumped to physical 0x0000:0x006c - deep in
	 * the real-mode IVT - instead of SEG:0x006c, hit whatever
	 * garbage/IVT bytes happened to be there as "code", and can raise
	 * #UD repeatedly from that point on. Confirmed at the CPU-register
	 * level, not guessed: a breakpoint at this exact symbol's own
	 * first byte, before this jump ever ran, showed %cs=0x0000 on the
	 * ISO build; the disk build's own equivalent entry point already
	 * showed %cs=SEG correctly, matching stage1.S's explicit far jump
	 * there. A real hardware/loop-device build of the disk path
	 * (STAGE2_SEGMENT=0x1000, unmodified) was used as the discriminator
	 * that proved this was ISO-entry-specific, not a bug in fat.c/
	 * switch32.S's shared logic - it completed a real, non-NULL-
	 * callback kernel+initrd load end to end with no error at all.
	 */
	ljmp $SEG, $1f
1:
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
