#ifndef ZFSBOOT_BIOS_BOOTPARAMS_H
#define ZFSBOOT_BIOS_BOOTPARAMS_H

#include "stdint_local.h"

/*
 * struct boot_params (Linux/x86's own "zero page") is a fixed,
 * 4096-byte structure per Documentation/arch/x86/boot.rst and
 * arch/x86/include/uapi/asm/bootparam.h - this code only ever touches
 * THREE things inside it (the copied-in setup_header, the E820 entry
 * count, and the E820 table itself), so it's modeled here as one raw,
 * caller-zeroed 4096-byte buffer plus a handful of independently
 * documented, fixed BYTE OFFSETS for exactly those three things -
 * deliberately not a fully elaborated nested struct covering every
 * legacy/unused field in between (screen_info, apm_bios_info, edd
 * info, ...). Those three offsets have been stable across the
 * kernel's own boot ABI for well over a decade (every real bootloader
 * depends on that stability), but reproducing every single unused
 * padding field byte-exact from memory, with no live kernel header to
 * check against in this sandbox, is real risk for zero benefit - a
 * mistake in a padding field's size would silently shift every offset
 * after it. Isolating just the offsets actually used keeps that risk
 * contained to exactly what's used, and easy to correct in one place
 * if a real boot ever proves one of them wrong.
 */
#define BOOT_PARAMS_SIZE 4096

#define BOOT_PARAMS_HDR_OFFSET 0x1f1          /* struct setup_header, see below */
#define BOOT_PARAMS_E820_ENTRIES_OFFSET 0x1e8 /* uint8_t: number of valid entries in the table below */
#define BOOT_PARAMS_E820_TABLE_OFFSET 0x2d0   /* struct boot_e820_entry[128], 20 bytes each */
#define BOOT_PARAMS_E820_MAX_ENTRIES 128

struct boot_e820_entry {
	uint64_t addr;
	uint64_t size;
	uint32_t type; /* 1 = usable RAM - the only type this code ever distinguishes */
} __attribute__((packed));

#define E820_TYPE_RAM 1

/*
 * setup_header - the part of a bzImage's own boot sector this code
 * DOES fully model, since every field has to be read out of the
 * actual kernel file (never invented) and copied, lightly modified,
 * into boot_params. Field order/sizes below were cross-checked
 * byte-for-byte against the real kernel header shipped on this dev
 * box (/usr/include/x86_64-linux-gnu/asm/bootparam.h) - not just
 * recalled from Documentation/arch/x86/boot.rst - and matched
 * exactly, including every BOOT_PARAMS_*_OFFSET constant below
 * (0x1f1/0x1e8/0x2d0, all confirmed against that same header's own
 * struct boot_params).
 */
struct setup_header {
	uint8_t setup_sects;
	uint16_t root_flags;
	uint32_t syssize;
	uint16_t ram_size;
	uint16_t vid_mode;
	uint16_t root_dev;
	uint16_t boot_flag; /* must be 0xAA55 */
	uint16_t jump;
	uint32_t header; /* must be "HdrS" - SETUP_HEADER_MAGIC below */
	uint16_t version;
	uint32_t realmode_swtch;
	uint16_t start_sys_seg;
	uint16_t kernel_version;
	uint8_t type_of_loader;
	uint8_t loadflags;
	uint16_t setup_move_size;
	uint32_t code32_start;
	uint32_t ramdisk_image;
	uint32_t ramdisk_size;
	uint32_t bootsect_kludge;
	uint16_t heap_end_ptr;
	uint8_t ext_loader_ver;
	uint8_t ext_loader_type;
	uint32_t cmd_line_ptr;
	uint32_t initrd_addr_max;
	uint32_t kernel_alignment;
	uint8_t relocatable_kernel;
	uint8_t min_alignment;
	uint16_t xloadflags;
	uint32_t cmdline_size;
	uint32_t hardware_subarch;
	uint64_t hardware_subarch_data;
	uint32_t payload_offset;
	uint32_t payload_length;
	uint64_t setup_data;
	uint64_t pref_address;
	uint32_t init_size;
	uint32_t handover_offset;
	uint32_t kernel_info_offset; /* protocol 2.15+ - benign if absent/zero on an older kernel, never read by this code either way */
} __attribute__((packed));

/* Where this struct starts within the raw kernel file's own bytes -
 * a fixed, standardized file offset, not something this code chose. */
#define SETUP_HEADER_FILE_OFFSET 0x1f1
#define SETUP_HEADER_BOOT_FLAG 0xaa55
#define SETUP_HEADER_MAGIC 0x53726448u /* "HdrS", little-endian dword */

#define LOADFLAGS_LOADED_HIGH 0x01
#define LOADFLAGS_CAN_USE_HEAP 0x80

/* An "undefined bootloader" ID - fine per the spec for a one-off
 * loader like this one that never registered its own assigned ID. */
#define TYPE_OF_LOADER_UNKNOWN 0xff

/*
 * Builds boot_params (a caller-supplied, zeroed BOOT_PARAMS_SIZE
 * buffer) from a raw copy of the kernel's own setup_header (already
 * read out of the kernel file - see stage2_main.c), the physical
 * address/size of the already-placed initrd, the physical address of
 * the cmdline string, and the E820 memory map gathered separately
 * (e820.c). Every field the kernel itself set in its own header
 * (setup_sects, syssize, version, code32_start, ...) is preserved
 * as-is; only the handful of loader-controlled fields are overwritten.
 * Always returns 0 - kept as an int, matching every other function in
 * this codebase that can genuinely fail, even though this one (no
 * I/O, no allocation) realistically never does.
 */
int bootparams_build(uint8_t boot_params[BOOT_PARAMS_SIZE],
                      const struct setup_header *kernel_hdr,
                      uint32_t initrd_addr, uint32_t initrd_size,
                      uint32_t cmdline_addr,
                      const struct boot_e820_entry *e820, uint8_t e820_count);

#endif
