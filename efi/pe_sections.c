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

EFI_STATUS pe_find_section(VOID *image_base, UINTN image_size, CHAR8 *name, pe_section_t *out)
{
	UINT8 *base = (UINT8 *)image_base;
	UINT32 e_lfanew;
	UINT8 *nt_header;
	UINTN nt_header_off;
	UINT16 num_sections;
	UINT16 opt_header_size;
	UINTN sections_off;
	UINTN i;

	/* "MZ" at offset 0, e_lfanew (a UINT32) at offset 0x3C - both must
	 * actually fit within the real mapped image before either is read. */
	if (image_size < 0x3C + 4)
		return EFI_LOAD_ERROR;
	if (read_u16(base) != 0x5A4D) /* "MZ" */
		return EFI_LOAD_ERROR;

	e_lfanew = read_u32(base + 0x3C);
	nt_header_off = (UINTN)e_lfanew;
	/* The 4-byte "PE\0\0" signature itself, then the 20-byte
	 * IMAGE_FILE_HEADER right after it (Machine/NumberOfSections/
	 * TimeDateStamp/PointerToSymbolTable/NumberOfSymbols/
	 * SizeOfOptionalHeader/Characteristics) - bounded together since
	 * nothing in between is ever read on its own. */
	if (nt_header_off > image_size || image_size - nt_header_off < 4 + 20)
		return EFI_LOAD_ERROR;
	nt_header = base + nt_header_off;
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
	/* nt_header_off + 4 + 20 already proven <= image_size above;
	 * opt_header_size (a UINT16, max 65535) added on top still can't
	 * overflow a UINTN, so a plain bounds check is safe here. */
	sections_off = nt_header_off + 4 + 20 + opt_header_size;
	if (sections_off > image_size)
		return EFI_LOAD_ERROR;

	for (i = 0; i < num_sections; i++) {
		UINT8 *sh;
		UINTN sh_off;
		UINT32 va, vsize;

		/* Bounded PER ITERATION, not just once against num_sections*
		 * HEADER_SIZE up front - a corrupted/truncated image can
		 * claim more sections than actually fit, and this must stop
		 * reading the instant that claim runs past the real image,
		 * not just fail some aggregate check that still let earlier
		 * reads happen. Same reasoning applies to every other bound
		 * in this function: fail closed the moment it's provably
		 * false, not "well most of it happened to be in range." */
		sh_off = sections_off + i * PE_SECTION_HEADER_SIZE;
		if (sh_off > image_size || image_size - sh_off < PE_SECTION_HEADER_SIZE)
			return EFI_NOT_FOUND;
		sh = base + sh_off;

		if (!name_matches(sh, name))
			continue;

		/*
		 * IMAGE_SECTION_HEADER: Name[8] VirtualSize(4)@8
		 * VirtualAddress(4)@12 ... - VirtualAddress is an RVA
		 * relative to image_base, matching how build.sh's
		 * `objcopy --change-section-vma` placed it. Bounded against
		 * image_size too - a matched section whose own claimed
		 * VA/size pair falls outside the real image would otherwise
		 * hand the caller a pointer straight to it, which for
		 * .initrd/.cmdline/.linux is the exact payload handed to the
		 * kernel's own EFI stub next.
		 */
		va = read_u32(sh + 12);
		vsize = read_u32(sh + 8);
		if ((UINTN)va > image_size || image_size - (UINTN)va < (UINTN)vsize)
			return EFI_LOAD_ERROR;

		out->size = vsize;
		out->data = base + va;
		return EFI_SUCCESS;
	}

	return EFI_NOT_FOUND;
}
