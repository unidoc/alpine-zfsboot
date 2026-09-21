#include "bootparams.h"

int bootparams_build(uint8_t boot_params[BOOT_PARAMS_SIZE],
                      const struct setup_header *kernel_hdr,
                      uint32_t initrd_addr, uint32_t initrd_size,
                      uint32_t cmdline_addr,
                      const struct boot_e820_entry *e820, uint8_t e820_count)
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

	return 0;
}
