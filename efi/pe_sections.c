#include <efi.h>
#include <efilib.h>

#include "pe_sections.h"

#define PE_SECTION_HEADER_SIZE 40
#define PE_SECTION_NAME_SIZE   8

static UINT16 read_u16(VOID *p)
{
	UINT8 *b = p;
	return (UINT16)(b[0] | (b[1] << 8));
}

static UINT32 read_u32(VOID *p)
{
	UINT8 *b = p;
	return (UINT32)(b[0] | (b[1] << 8) | (b[2] << 16) | (b[3] << 24));
}

/*
 * Section names are a fixed 8-byte field, NUL-padded only if shorter
 * than 8 bytes - ".cmdline" is exactly 8 characters, so it has no
 * trailing NUL to compare against. Compare all 8 raw bytes against
 * `want` padded with NULs past its own length, rather than treating
 * either side as a NUL-terminated C string.
 */
static BOOLEAN name_matches(UINT8 *raw, CHAR8 *want)
{
	UINTN i, len;

	len = 0;
	while (want[len] != 0)
		len++;
	if (len > PE_SECTION_NAME_SIZE)
		return FALSE;

	for (i = 0; i < PE_SECTION_NAME_SIZE; i++) {
		UINT8 w = (i < len) ? (UINT8)want[i] : 0;
		if (raw[i] != w)
			return FALSE;
	}
	return TRUE;
}

EFI_STATUS pe_find_section(VOID *image_base, CHAR8 *name, pe_section_t *out)
{
	UINT8 *base = (UINT8 *)image_base;
	UINT32 e_lfanew;
	UINT8 *nt_header;
	UINT16 num_sections;
	UINT16 opt_header_size;
	UINT8 *sections;
	UINTN i;

	if (read_u16(base) != 0x5A4D) /* "MZ" */
		return EFI_LOAD_ERROR;

	e_lfanew = read_u32(base + 0x3C);
	nt_header = base + e_lfanew;
	if (read_u32(nt_header) != 0x00004550) /* "PE\0\0" */
		return EFI_LOAD_ERROR;

	/*
	 * IMAGE_FILE_HEADER starts right after the 4-byte PE signature:
	 * Machine(2) NumberOfSections(2) TimeDateStamp(4)
	 * PointerToSymbolTable(4) NumberOfSymbols(4)
	 * SizeOfOptionalHeader(2) Characteristics(2) = 20 bytes.
	 */
	num_sections = read_u16(nt_header + 4 + 2);
	opt_header_size = read_u16(nt_header + 4 + 16);
	sections = nt_header + 4 + 20 + opt_header_size;

	for (i = 0; i < num_sections; i++) {
		UINT8 *sh = sections + i * PE_SECTION_HEADER_SIZE;

		if (!name_matches(sh, name))
			continue;

		/*
		 * IMAGE_SECTION_HEADER: Name[8] VirtualSize(4)@8
		 * VirtualAddress(4)@12 ... - VirtualAddress is an RVA
		 * relative to image_base, matching how build.sh's
		 * `objcopy --change-section-vma` placed it.
		 */
		out->size = read_u32(sh + 8);
		out->data = base + read_u32(sh + 12);
		return EFI_SUCCESS;
	}

	return EFI_NOT_FOUND;
}
