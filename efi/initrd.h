#ifndef ALPINE_ZFSBOOT_INITRD_H
#define ALPINE_ZFSBOOT_INITRD_H

#include <efi.h>

/*
 * Registers an EFI_LOAD_FILE2_PROTOCOL instance on a fresh handle,
 * device-pathed with LINUX_EFI_INITRD_MEDIA_GUID - the mechanism the
 * Linux kernel's own EFI stub (CONFIG_EFI_STUB, x86_64 and arm64
 * alike) uses to fetch an initrd from whatever loaded it, per the
 * kernel's own drivers/firmware/efi/libstub/efi-stub-helper.c and
 * include/linux/efi.h. This is why this loader itself never touches
 * Linux boot_params/setup_header at all - the kernel's stub does that
 * part, on every architecture, once it can find its initrd this way.
 *
 * `data`/`size` must stay valid for as long as the returned handle
 * exists: the callback runs later, from inside the kernel image's own
 * StartImage() call - do not free them beforehand.
 */
EFI_STATUS initrd_install(VOID *data, UINTN size, EFI_HANDLE *out_handle);

#endif
