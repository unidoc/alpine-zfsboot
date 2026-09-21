#ifndef ALPINE_ZFSBOOT_PE_SECTIONS_H
#define ALPINE_ZFSBOOT_PE_SECTIONS_H

#include <efi.h>

typedef struct {
	VOID  *data;
	UINTN size;
} pe_section_t;

/*
 * Locates a named section (".linux", ".initrd", ".cmdline") within a
 * PE/COFF image already mapped at image_base - our own running image,
 * after build.sh's `objcopy --add-section` has embedded the kernel,
 * initramfs and cmdline into it.
 *
 * Hand-rolled rather than pulled from gnu-efi's own <ARCH>/pe.h: that
 * header isn't guaranteed present for every architecture gnu-efi
 * otherwise supports, and the layout it would give us is a fixed wire
 * format anyway (Microsoft PE/COFF, unrelated to the host CPU this
 * loader itself runs on) - identical for every caller regardless of
 * arch, so safe to hardcode directly.
 */
EFI_STATUS pe_find_section(VOID *image_base, CHAR8 *name, pe_section_t *out);

#endif
