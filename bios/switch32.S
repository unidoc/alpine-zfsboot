/*
 * unreal_copy() and jump_to_kernel() - see switch32.h for what each
 * does and why this is the one place in this whole BIOS path that's
 * unavoidably hand-written assembly rather than C.
 *
 * .code16gcc (not plain .code16): this file is called FROM gcc -m16
 * compiled C (stage2_main.c) and calls back into no C itself, but
 * still needs to match gcc's own -m16 codegen conventions exactly -
 * 32-bit-sized stack arguments, near call/ret using a 32-bit-wide
 * (prefixed) return address - .code16gcc is GAS's own directive
 * specifically for this interop case, distinct from plain .code16
 * (which assumes genuine 8086-style 16-bit-everything semantics with
 * no gcc-compatibility adjustments).
 *
 * SEG below is this whole stage's fixed real-mode segment - every
 * near pointer gcc-compiled code hands to these functions is an
 * offset relative to this exact segment, because stage2_entry.S sets
 * %cs = %ds = %es = %ss = SEG before ever calling into C (see that
 * file's own comment) and link.ld's ORIGIN is 0 to match (symbol
 * values are plain offsets, not physical addresses) - SEG must stay
 * in agreement across all three of those; it is NOT rederived here.
 *
 * Overridable via `as --defsym SEG=...`: the ISO/El-Torito build (see
 * ../Makefile's stage2-iso.bin target) assembles this exact same file
 * with SEG=0x7c0 instead - see stage2_main.c's own STAGE2_SEGMENT
 * comment for why.
 */
	.code16gcc
	.ifndef SEG
	.set SEG, 0x1000
	.endif

	.section .text

	.global unreal_copy
	.global jump_to_kernel
	.global enable_a20

/*
 * void enable_a20(void) - see switch32.h's own comment for why this
 * must run before the first unreal_copy() to a >=1MB address, and why
 * both methods below are used rather than just one.
 */
enable_a20:
	pushl %eax
	pushw %ds
	/*
	 * INT 15h, AX=2401h ("enable A20 gate") - a real BIOS service
	 * (SeaBIOS: src/system.c's handle_15 case 0x2401), not a fast-A20
	 * chipset trick; some BIOSes clobber %ds across INT 15h calls
	 * (same real-hardware finding cdrom_disk.c's own INT13h calls are
	 * already guarded against), so it's saved/restored here too. Return
	 * value (success/failure in %ah/carry) deliberately ignored - the
	 * port-0x92 poke right after covers the case this call did nothing.
	 */
	movw $0x2401, %ax
	int $0x15
	popw %ds
	/*
	 * Fast A20 via System Control Port A (port 0x92, bit 1) - keep bit
	 * 0 (a chipset "fast reset" trigger, unrelated to A20 and
	 * documented as best left alone) exactly as read, only ever set
	 * bit 1.
	 */
	inb $0x92, %al
	orb $0x02, %al
	andb $0xfe, %al
	outb %al, $0x92
	popl %eax
	ret

/*
 * void unreal_copy(uint32_t dst, const void *src, uint32_t len)
 *
 * "Unreal mode": briefly set CR0.PE=1 just long enough to reload %es
 * from a flat (base 0, 4GB limit) GDT descriptor, then immediately
 * clear CR0.PE again - the CPU never actually executes any code while
 * "in protected mode" here, only %es's cached base/limit changes,
 * which real x86 CPUs keep using correctly even after PE drops back
 * to 0 (a well-established, decades-old real-CPU behavior, not a
 * hack specific to any one BIOS/CPU vendor). %cs is deliberately
 * never touched, which is exactly what keeps this "unreal" rather
 * than a real, sticky mode change - see jump_to_kernel below for the
 * other, one-way kind that DOES reload %cs, for good.
 */
unreal_copy:
	pushl %ebp
	movl %esp, %ebp
	pushl %ecx
	pushl %esi
	pushl %edi
	pushw %es

	movl 8(%ebp), %edi   /* dst: arbitrary 32-bit physical address */
	movl 12(%ebp), %esi  /* src: near pointer, still DS-relative - DS is untouched by this whole function */
	movl 16(%ebp), %ecx  /* len, bytes */

	/*
	 * pushfl before cli - NOT a bare cli - so the trailing popfl below
	 * restores the CALLER's own interrupt-flag state exactly, whatever
	 * it was, instead of unconditionally forcing IF=1. A full source
	 * audit found this function used to end with a bare `sti`
	 * (confirmed the hard way, from an even earlier version of this
	 * same file's own history) - harmless on its own the very first
	 * time this is called mid-stage, but WRONG the instant a caller
	 * that already had interrupts disabled for its own reasons (this
	 * whole stage does, for its entire remaining execution, from the
	 * moment stage2_entry.S hands off - see that file's own comment)
	 * calls this function even once: the unconditional `sti` silently
	 * re-enabled interrupts for everything AFTER that first call,
	 * destroying the exact invariant this function's own comment below
	 * spends a full paragraph explaining why it depends on. pushfl/popfl
	 * is the standard idiom for "disable, do work, restore exactly what
	 * was there before" - it makes no assumption about what the caller's
	 * IF was, so it is correct whether this is ever called with
	 * interrupts already off (every real call site, today) or on.
	 */
	pushfl
	cli
	lgdt unreal_gdt_ptr
	movl %cr0, %eax
	orl $1, %eax
	movl %eax, %cr0
	movw $0x08, %ax      /* GDT index 1 (the one non-null entry) * 8 = selector 0x08 */
	movw %ax, %es
	movl %cr0, %eax
	andl $0xfffffffe, %eax
	movl %eax, %cr0
	/*
	 * NOT `sti` here - confirmed the hard way, by disassembling the
	 * actual instruction ordering: interrupts must stay disabled for
	 * the ENTIRE time %es holds the flat descriptor, not just around
	 * the CR0.PE toggle. A real BIOS timer tick ISR (18.2 Hz, so a
	 * near-certainty across the many chunks this copies a kernel+
	 * initrd through) does its own `push %es`/`pop %es` - which
	 * reloads the SELECTOR value correctly on return, but real x86
	 * CPUs only re-fetch a segment's cached base/limit from the GDT
	 * when a segment register is explicitly loaded with a selector,
	 * not on every push/pop; whether that leaves this flat mapping
	 * intact across an interrupt is exactly the kind of timing-
	 * dependent real-hardware behavior worth NOT gambling on when
	 * simply not re-enabling interrupts here removes the question
	 * entirely. sti moves to after the copy below, once %es is back
	 * to its normal real-mode value.
	 */

	/*
	 * rep movsb's own implicit segment defaults do exactly what's
	 * needed here with no extra prefix: source DS:ESI (DS is still
	 * this stage's normal, small real-mode segment - src is always
	 * < 1MB, reachable there directly), destination ES:EDI (ES is
	 * now the flat descriptor just loaded above, so EDI is read as a
	 * genuine 32-bit physical address, not scaled/wrapped at 64KB).
	 * Byte granularity, not a dword fast path - this moves a kernel
	 * and an initrd once per boot each, not a hot path worth the
	 * extra complexity of an alignment-handling fast/slow split.
	 */
	/*
	 * addr32: NOT optional - without it, `rep movsb` defaults to this
	 * segment's own 16-bit address size (SI/DI, not ESI/EDI), which
	 * would silently truncate `dst` to its low 16 bits, exactly
	 * defeating the entire point of this function (copying to an
	 * address that's usually well above 64KB). Confirmed the hard
	 * way, before this was ever run on real hardware: GAS's
	 * .code16gcc mode auto-adds the equivalent override for ordinary
	 * register+displacement operands it can see written out in the
	 * source (like `movl 8(%ebp), %edi` above), but NOT for a string
	 * instruction's own implicit (%esi)/(%edi) addressing, since
	 * there's no explicit register operand in `rep movsb`'s own
	 * syntax for it to notice.
	 */
	cld
	addr32 rep movsb

	/*
	 * The saved flags are popped into %eax FIRST, then %es, then pushed
	 * back and popfl'd - NOT a plain "popw %es; popfl" - because pushfl
	 * happened AFTER pushw %es above (chronologically the LAST push
	 * before the unreal-mode window), so strict stack LIFO order
	 * requires popping it FIRST, before %es. But the actual EFLAGS
	 * restore (the instruction that can set IF back to 1) must not
	 * happen until AFTER %es is back to its normal real-mode value -
	 * see the pushfl site's own comment for why. %eax is free to use
	 * here as scratch: this function never preserves the CALLER's own
	 * %eax (only ecx/esi/edi/es are saved/restored - %eax is already
	 * used internally above for the %cr0 toggle, confirming it was
	 * never in the "must restore for the caller" set to begin with).
	 * This shuffle satisfies both constraints at once: every stack read
	 * happens in the exact order things were pushed, and IF only
	 * changes after %es is already restored.
	 */
	popl %eax
	popw %es
	pushl %eax
	popfl
	popl %edi
	popl %esi
	popl %ecx
	movl %ebp, %esp
	popl %ebp
	ret

	.align 4
unreal_gdt:
	.quad 0                            /* GDT entry 0: must be null */
	.word 0xffff, 0x0000               /* entry 1: limit 0-15, base 0-15 */
	.byte 0x00, 0x92, 0xcf, 0x00       /* base 16-23, access (present/ring0/data/r-w), flags+limit 16-19 (G=1,D/B=1), base 24-31 */
unreal_gdt_ptr:
	.word (2 * 8) - 1
	.long (SEG << 4) + unreal_gdt

/*
 * void jump_to_kernel(uint32_t entry, uint32_t boot_params) - never
 * returns.
 *
 * The real, final, one-way switch: loads a permanent flat GDT (code
 * AND data this time), sets %esi = boot_params (exactly what
 * Documentation/arch/x86/boot.rst's 32-bit boot protocol specifies
 * the kernel's own entry point expects to find there), reloads
 * %cs/%ds/%es/%ss from it, and far-jumps to `entry` - at that point
 * this stage's job is entirely done; the kernel takes over completely,
 * same as efi_main() calling StartImage() on the UEFI path (see
 * efi/main.c) never returning either.
 *
 * No prologue/epilogue at all - this function never returns, so
 * there is no stack to restore and no caller to preserve registers
 * for; arguments are read directly off the incoming %esp (return
 * address at 0(%esp), first arg at 4(%esp), matching the same 32-bit
 * stack-argument convention unreal_copy's own %ebp-relative offsets
 * rely on, just without bothering to establish a frame for a function
 * that's about to abandon this whole stack anyway).
 */
jump_to_kernel:
	movl 4(%esp), %eax   /* entry */
	movl 8(%esp), %esi   /* boot_params - must survive untouched through everything below */

	cli
	lgdt kernel_gdt_ptr
	movl %cr0, %ecx
	orl $1, %ecx
	movl %ecx, %cr0

	movw $0x18, %cx       /* GDT index 3 (flat data) * 8 = selector 0x18 - see kernel_gdt's own layout comment for why index 3, not 2 */
	movw %cx, %ds
	movw %cx, %es
	movw %cx, %ss
	/* %fs/%gs deliberately left untouched - the boot protocol doesn't
	 * require them set to anything in particular before the kernel's
	 * own 32-bit entry code runs, and it sets up its own state before
	 * relying on them either way. */

	/*
	 * Documentation/arch/x86/boot.rst's 32-bit boot protocol states
	 * plainly (cross-checked against this box's own /usr/include
	 * kernel headers, not just recalled): "%ebp, %edi and %ebx must
	 * be zero" at the kernel's own entry point, alongside %esi already
	 * holding boot_params above - not optional/defensive, a
	 * documented MUST this function was missing entirely before.
	 */
	xorl %ebp, %ebp
	xorl %edi, %edi
	xorl %ebx, %ebx

	/*
	 * A far jump with an immediate target (`ljmp $seg,$offset`) only
	 * accepts a compile-time-constant offset - `entry` here is a
	 * runtime value (this function's own first argument), so the
	 * target is built as an in-memory 6-byte {offset:4, selector:2}
	 * far-pointer and jumped to indirectly - the standard idiom for a
	 * far jump to a non-constant address. This is also the actual %cs
	 * reload, the one that makes this switch permanent rather than
	 * "unreal": %cs becomes selector 0x10 (GDT index 2, flat code)
	 * from here on.
	 *
	 * Deliberately NOT built on the stack (an earlier version of this
	 * function did exactly that, via `subl $6,%esp` + `(%esp)`-relative
	 * stores, and crashed/reset here on real hardware) - %ss was just
	 * reloaded to the flat (base 0) descriptor a few instructions
	 * above, so by this point ANY (%esp)-relative reference is no
	 * longer interpreted against this stage's real-mode stack segment
	 * (SEG<<4 + esp) at all, but against a flat base of 0 instead -
	 * `esp`'s own small numeric value (still whatever this stage's
	 * real-mode stack left it at) then addresses essentially random
	 * low physical memory instead of the actual stack, and the far
	 * pointer gets written to (and read back from) THAT wrong address
	 * instead - self-consistent (same wrong place for both the write
	 * and the `ljmp`'s own read), which is exactly why this kept
	 * "working" right up to the jump and then reset with no warning.
	 *
	 * Using a fixed physical scratch address via an explicit %es:
	 * override instead sidesteps the whole problem: %es is ALSO flat
	 * by this point, so %es:0x7000 addresses physical 0x7000 directly,
	 * unambiguously, regardless of what %esp/%ss are doing. 0x7000 is
	 * comfortably below every load address this stage itself ever
	 * uses (0x7c00 for the ISO build's SEG=0x7c0, 0x10000 for the
	 * disk build's SEG=0x1000) and below 0x7c00's own conventional
	 * "free for the bootloader" low-memory floor, so it can't collide
	 * with anything this stage is using at the moment it jumps away
	 * from all of it for good anyway.
	 */
	movl %eax, %es:0x7000
	movw $0x10, %es:0x7004
	/*
	 * `data32`: required - confirmed via disassembly, not assumed.
	 * `.code16gcc` makes a bare indirect far jump (`ljmp *mem`) default
	 * to reading a 4-byte pointer (16-bit offset + 16-bit selector),
	 * since that's this mode's normal operand size; without this
	 * prefix, THIS jump read only the first 2 bytes of the 4-byte
	 * offset just written above as the "offset" and the NEXT 2 bytes
	 * (the upper 16 bits of `entry`, which for a real kernel loaded at
	 * 0x00100000 is 0x0010 - this GDT's own flat-code selector value,
	 * by pure coincidence) as the "selector" instead of the real one
	 * at 0x7004 - jumping to selector 0x10 : offset 0x0000 (physical
	 * address 0, not the kernel's real 0x00100000 entry point) and
	 * resetting immediately with no warning. `data32` forces the full
	 * 6-byte {offset:4, selector:2} read this code actually wrote.
	 */
	data32 ljmp *%es:0x7000
	/* unreachable */

	/*
	 * Selector values match Documentation/arch/x86/boot.rst's own
	 * literal __BOOT_CS(0x10)/__BOOT_DS(0x18), not just "some flat
	 * code/data pair" - entry 1 is unused padding solely to make that
	 * true (index 2*8=0x10, index 3*8=0x18); real kernels reload their
	 * own segments before relying on these values, so this almost
	 * certainly wasn't going to matter in practice, but matching the
	 * spec's literal numbers exactly removes the question.
	 */
	.align 4
kernel_gdt:
	.quad 0                            /* entry 0: null */
	.quad 0                            /* entry 1: unused padding, see comment above */
	.word 0xffff, 0x0000               /* entry 2: flat code */
	.byte 0x00, 0x9a, 0xcf, 0x00       /* access: present/ring0/code/exec+read */
	.word 0xffff, 0x0000               /* entry 3: flat data */
	.byte 0x00, 0x92, 0xcf, 0x00       /* access: present/ring0/data/r-w */
kernel_gdt_ptr:
	.word (4 * 8) - 1
	.long (SEG << 4) + kernel_gdt
