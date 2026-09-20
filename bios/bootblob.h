#ifndef ZFSBOOT_BIOS_BOOTBLOB_H
#define ZFSBOOT_BIOS_BOOTBLOB_H

#include "stdint_local.h"

/*
 * The boot-blob format stage2 reads out of the "alpine-zfsboot boot blob" GPT
 * partition (see gpt.h's ZFSBOOT_BOOTBLOB_TYPE_GUID) - a flat header
 * immediately followed by the raw kernel bytes, then the raw initrd
 * bytes, then a NUL-terminated cmdline string. No filesystem of any
 * kind - stage2 never parses one, same reasoning efi/main.c's own
 * header comment gives for reading PE sections directly rather than a
 * real filesystem.
 *
 * SECTOR-ALIGNED, not just concatenated: the header occupies exactly
 * one full 512-byte sector (zero-padded past its own real size), and
 * the kernel/initrd/cmdline sections that follow are each padded with
 * trailing zero bytes out to the next sector boundary too. disk_read_
 * lba() only ever reads whole sectors - this framing means every
 * section's start is a plain LBA offset from the partition's own
 * starting LBA (header is 1 sector; kernel starts at +1, spans
 * ceil(kernel_size/512) sectors; initrd starts right after that;
 * cmdline right after THAT), with no sub-sector byte-shifting logic
 * needed anywhere in stage2 to find any of them.
 *
 * build.sh's own packer writes EXACTLY this layout - this struct (and
 * the sector-padding convention above) is stage2's side of that
 * contract. build.sh is shell, not C, so it can't #include this file
 * directly - its own boot-blob packing comment points back here, and
 * the two must be kept in sync by hand if this ever changes.
 */

/* Exactly 8 bytes, no NUL - compared as raw bytes, not a C string. */
#define ZFSBOOT_BOOTBLOB_MAGIC "ZFSBOOT1"
#define ZFSBOOT_BOOTBLOB_MAGIC_LEN 8

#define ZFSBOOT_BOOTBLOB_SECTOR_SIZE 512

struct zfsboot_bootblob_header {
	char magic[ZFSBOOT_BOOTBLOB_MAGIC_LEN]; /* ZFSBOOT_BOOTBLOB_MAGIC */
	uint32_t header_size;  /* sizeof(struct zfsboot_bootblob_header) - lets stage2 confirm it's reading what it thinks it is before trusting the sizes below */
	uint32_t kernel_size;  /* real byte length, before sector padding, of the kernel section starting 1 sector after this header */
	uint32_t initrd_size;  /* real byte length, before sector padding, of the initrd section right after the (padded) kernel section */
	uint32_t cmdline_size; /* real byte length INCLUDING the NUL terminator, before sector padding, of the cmdline section right after the (padded) initrd section */
} __attribute__((packed));

#endif
