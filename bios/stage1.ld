/*
 * stage1 loads at the traditional, universal boot-sector physical
 * address (0x7c00) with CS=DS=SS=0 (see stage1.S's own segment
 * setup) - so link-time offsets here ARE physical addresses
 * directly, unlike stage2's own link.ld (ORIGIN 0, SEG-relative).
 */
ENTRY(_start)
SECTIONS
{
	. = 0x7c00;
	.text : { *(.text) }
	/DISCARD/ : { *(.eh_frame) *(.comment) *(.note*) *(.data) *(.bss) *(COMMON) }
}
