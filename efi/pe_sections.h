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
 *
 * image_size (EFI_LOADED_IMAGE_PROTOCOL's own ImageSize field, which
 * every real caller already has in hand at LoadImage time) bounds
 * every offset this function reads - e_lfanew, the optional-header
 * size, the section-header table, and each matched section's own
 * VirtualAddress/VirtualSize - all against it before ever
 * dereferencing. A full source audit found this function previously
 * did none of that: a truncated or corrupted .EFI (an interrupted `cp`
 * during `update`, a full ESP) that firmware still loads far enough to
 * call this on could walk arbitrary memory well past the real mapped
 * image, and could hand back a .initrd/.cmdline/.linux section pointer
 * pointing anywhere at all - the exact bytes this loader then hands
 * straight to the kernel's own EFI stub as its command line and
 * initrd. Internal function signature only - not a persisted/on-disk
 * format change, the PE layout convention itself is untouched.
 */
EFI_STATUS pe_find_section(VOID *image_base, UINTN image_size, CHAR8 *name, pe_section_t *out);

#endif
