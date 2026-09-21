/*
 * stage2 (stage2_entry.S + every C file it calls into) lives entirely
 * within ONE real-mode segment (STAGE2_SEGMENT = 0x1000, see
 * stage2_entry.S/switch32.S/stage2_main.c - all three must agree)
 * addressed via CS=DS=ES=SS all set to that same value - so every
 * symbol here is a plain OFFSET relative to that segment's base, not
 * a physical address, hence ORIGIN 0 rather than the physical
 * 0x10000 that segment actually maps to.
 */
ENTRY(stage2_entry)
SECTIONS
{
	. = 0;
	.text : { *(.text) }
	.rodata : { *(.rodata*) }
	.data : { *(.data) }
	. = ALIGN(4);
	_bss_start = .;
	.bss : { *(.bss) *(COMMON) }
	_bss_end = .;
	/DISCARD/ : { *(.eh_frame) *(.comment) *(.note*) }
}
