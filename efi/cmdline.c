#include <efi.h>
#include <efilib.h>

#include "cmdline.h"

EFI_STATUS ascii_to_cmdline16(CHAR8 *ascii, CHAR16 **out, UINT32 *out_size)
{
	UINTN len, i;
	CHAR16 *buf;

	len = 0;
	while (ascii[len] != 0)
		len++;

	buf = AllocatePool((len + 1) * sizeof(CHAR16));
	if (buf == NULL)
		return EFI_OUT_OF_RESOURCES;

	for (i = 0; i < len; i++)
		buf[i] = (CHAR16)ascii[i];
	buf[len] = 0;

	*out = buf;
	*out_size = (UINT32)((len + 1) * sizeof(CHAR16));
	return EFI_SUCCESS;
}
