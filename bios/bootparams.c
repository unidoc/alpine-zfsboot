#include "bootparams.h"

int bootparams_build(uint8_t boot_params[BOOT_PARAMS_SIZE],
                      const struct setup_header *kernel_hdr,
                      uint32_t initrd_addr, uint32_t initrd_size,
                      uint32_t cmdline_addr,
                      const struct boot_e820_entry *e820, uint8_t e820_count,
                      const struct boot_video *video)
{
	struct setup_header *hdr = (struct setup_header *)(boot_params + BOOT_PARAMS_HDR_OFFSET);
	struct boot_e820_entry *table =
		(struct boot_e820_entry *)(boot_params + BOOT_PARAMS_E820_TABLE_OFFSET);
	uint32_t i;
	uint8_t count;

	/*
	 * Copy the kernel's own header through byte-for-byte first - every
	 * field the kernel itself set (setup_sects, syssize, version,
	 * code32_start, ...) must reach the kernel exactly as-is; only the
	 * handful of loader-controlled fields below get overwritten after.
	 */
	{
		const uint8_t *src = (const uint8_t *)kernel_hdr;
		uint8_t *dst = (uint8_t *)hdr;
		for (i = 0; i < sizeof(*kernel_hdr); i++)
			dst[i] = src[i];
	}

	hdr->type_of_loader = TYPE_OF_LOADER_UNKNOWN;
	hdr->loadflags = (uint8_t)(hdr->loadflags | LOADFLAGS_CAN_USE_HEAP);
	/*
	 * A small, fixed heap-end offset within this stage's own segment -
	 * only meaningful if the kernel's own real-mode setup code ever
	 * ran and used it, which it never does under the modern (>=2.02)
	 * boot protocol this loader follows (no real-mode kernel code is
	 * ever executed at all - see stage2_main.c's own comment). Kept
	 * small and harmless regardless of whether anything ever reads it.
	 */
	hdr->heap_end_ptr = 0xfe00;
	hdr->ramdisk_image = initrd_addr;
	hdr->ramdisk_size = initrd_size;
	hdr->cmd_line_ptr = cmdline_addr;

	count = e820_count;
	if (count > BOOT_PARAMS_E820_MAX_ENTRIES)
		count = BOOT_PARAMS_E820_MAX_ENTRIES;
	for (i = 0; i < count; i++)
		table[i] = e820[i];
	boot_params[BOOT_PARAMS_E820_ENTRIES_OFFSET] = count;

	/*
	 * screen_info: tell the kernel a colour VGA text console exists and
	 * where the cursor is, so console=tty0 (vgacon) works. Only for the
	 * text modes this loader's own INT 0x10 output runs in (0-3, 7 is
	 * mono and not supported here); anything else leaves it zeroed, as
	 * before. 25 lines / 16-pixel cells are the standard VGA text mode
	 * values.
	 */
	/* Bit 7 of INT 0x10 AH=0x0F's AL is the "memory not cleared" flag on
	 * some BIOSes (SeaBIOS ORs it in from BDA 0x487); Linux's own real-mode
	 * setup masks it the same way (arch/x86/boot/video.c). */
	if (video && (video->mode & 0x7f) <= 3 && video->cols >= 40) {
		boot_params[BOOT_PARAMS_SCREEN_ORIG_X] = video->cursor_col;
		boot_params[BOOT_PARAMS_SCREEN_ORIG_Y] = video->cursor_row;
		boot_params[BOOT_PARAMS_SCREEN_VIDEO_PAGE] = video->page;
		boot_params[BOOT_PARAMS_SCREEN_VIDEO_MODE] = video->mode & 0x7f;
		boot_params[BOOT_PARAMS_SCREEN_VIDEO_COLS] = video->cols;
		boot_params[BOOT_PARAMS_SCREEN_VIDEO_EGA_BX] = 3;
		boot_params[BOOT_PARAMS_SCREEN_VIDEO_LINES] = 25;
		boot_params[BOOT_PARAMS_SCREEN_VIDEO_ISVGA] = 1;
		boot_params[BOOT_PARAMS_SCREEN_VIDEO_POINTS] = 16;
	}

	return 0;
}
