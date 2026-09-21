#ifndef ALPINE_ZFSBOOT_CMDLINE_H
#define ALPINE_ZFSBOOT_CMDLINE_H

#include <efi.h>

/*
 * Converts a NUL-terminated ASCII string into a freshly AllocatePool'd
 * CHAR16 string, suitable for use as EFI_LOADED_IMAGE_PROTOCOL's
 * LoadOptions - the Linux EFI stub reads its command line from there
 * (as CHAR16), not from any section of ours directly.
 */
EFI_STATUS ascii_to_cmdline16(CHAR8 *ascii, CHAR16 **out, UINT32 *out_size);

#endif
