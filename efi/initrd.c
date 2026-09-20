#include <efi.h>
#include <efilib.h>

#include "initrd.h"

/*
 * Not a UEFI-spec GUID - this is Linux's own, defined in the kernel's
 * include/linux/efi.h as LINUX_EFI_INITRD_MEDIA_GUID
 * (5568e427-68fc-4f3d-ac74-ca555231cc68). Not shipped by gnu-efi (it
 * has no reason to know about it), so defined here instead. Verified
 * against the real kernel source, not from memory.
 */
#define LINUX_EFI_INITRD_MEDIA_GUID \
	{ 0x5568e427, 0x68fc, 0x4f3d, { 0xac, 0x74, 0xca, 0x55, 0x52, 0x31, 0xcc, 0x68 } }

#pragma pack(push, 1)
typedef struct {
	VENDOR_DEVICE_PATH       vendor;
	EFI_DEVICE_PATH_PROTOCOL end;
} initrd_device_path_t;
#pragma pack(pop)

typedef struct {
	EFI_LOAD_FILE2_PROTOCOL proto;
	VOID  *data;
	UINTN size;
} initrd_load_file2_t;

/*
 * This callback is called directly by the kernel's own EFI stub, not
 * by anything in this build - so it needs the real calling convention
 * every UEFI/kernel caller on this architecture actually uses,
 * independent of whatever ABI mode the REST of this loader is built
 * with (EFIAPI would only give it that if the whole build were
 * switched to GNU_EFI_USE_MS_ABI - which also makes efi_main require
 * MS-ABI arguments, while efi_main is called by gnu-efi's own
 * precompiled crt0 object using plain SysV, a real regression this
 * project hit directly). Attaching the attribute here, locally,
 * fixes the one function that actually needs it without touching how
 * efi_main itself gets called. Harmless no-op on arm64: there is no
 * ABI split to encode there, only x86_64 has one.
 */
#if defined(__x86_64__)
#define LOAD_FILE2_ABI __attribute__((ms_abi))
#else
#define LOAD_FILE2_ABI
#endif

static EFI_STATUS LOAD_FILE2_ABI initrd_load_file(
	EFI_LOAD_FILE2_PROTOCOL *This,
	EFI_DEVICE_PATH_PROTOCOL *FilePath,
	BOOLEAN BootPolicy,
	UINTN *BufferSize,
	VOID *Buffer)
{
	initrd_load_file2_t *ctx = (initrd_load_file2_t *)This;

	(VOID)FilePath;

	/* Per the UEFI spec: the kernel always passes FALSE here. */
	if (BootPolicy)
		return EFI_INVALID_PARAMETER;
	if (BufferSize == NULL)
		return EFI_INVALID_PARAMETER;

	if (Buffer == NULL || *BufferSize < ctx->size) {
		*BufferSize = ctx->size;
		return EFI_BUFFER_TOO_SMALL;
	}

	/*
	 * NOT CopyMem() (gnu-efi's CopyMem_1, a real function exported
	 * from libefi.a): confirmed on real hardware that libefi.a's own
	 * CopyMem_1 silently failed to write anything here - it and this
	 * callback don't agree on calling convention (see LOAD_FILE2_ABI
	 * above; CopyMem_1's own prototype uses EFIAPI, a no-op without a
	 * build-wide GNU_EFI_USE_MS_ABI this project deliberately doesn't
	 * set). This loop needs no external call at all - gcc compiles it
	 * straight to a MOV loop - so there is no ABI question left to
	 * get wrong.
	 */
	{
		UINT8 *d = (UINT8 *)Buffer;
		UINT8 *s = (UINT8 *)ctx->data;
		UINTN i;

		for (i = 0; i < ctx->size; i++)
			d[i] = s[i];
	}
	*BufferSize = ctx->size;
	return EFI_SUCCESS;
}

EFI_STATUS initrd_install(VOID *data, UINTN size, EFI_HANDLE *out_handle)
{
	EFI_STATUS status;
	initrd_device_path_t *dp;
	initrd_load_file2_t *ctx;
	EFI_HANDLE handle = NULL;
	EFI_GUID initrd_guid = LINUX_EFI_INITRD_MEDIA_GUID;

	dp = AllocatePool(sizeof(*dp));
	if (dp == NULL)
		return EFI_OUT_OF_RESOURCES;

	dp->vendor.Header.Type = MEDIA_DEVICE_PATH;
	dp->vendor.Header.SubType = MEDIA_VENDOR_DP;
	SetDevicePathNodeLength(&dp->vendor.Header, sizeof(VENDOR_DEVICE_PATH));
	dp->vendor.Guid = initrd_guid;
	SetDevicePathEndNode(&dp->end);

	ctx = AllocatePool(sizeof(*ctx));
	if (ctx == NULL) {
		FreePool(dp);
		return EFI_OUT_OF_RESOURCES;
	}
	/*
	 * Cast needed because EFI_LOAD_FILE2's own typedef uses EFIAPI,
	 * which (without a build-wide GNU_EFI_USE_MS_ABI - see above)
	 * doesn't carry the ms_abi attribute LOAD_FILE2_ABI adds directly
	 * above. Only the stored TYPE differs; the actual compiled code
	 * this pointer refers to is unaffected by the cast either way.
	 */
	ctx->proto.LoadFile = (EFI_LOAD_FILE2)(void *)initrd_load_file;
	ctx->data = data;
	ctx->size = size;

	status = uefi_call_wrapper(BS->InstallProtocolInterface, 4,
		&handle, &DevicePathProtocol, EFI_NATIVE_INTERFACE, dp);
	if (EFI_ERROR(status)) {
		FreePool(dp);
		FreePool(ctx);
		return status;
	}

	status = uefi_call_wrapper(BS->InstallProtocolInterface, 4,
		&handle, &LoadFile2Protocol, EFI_NATIVE_INTERFACE, &ctx->proto);
	if (EFI_ERROR(status)) {
		uefi_call_wrapper(BS->UninstallProtocolInterface, 3,
			handle, &DevicePathProtocol, dp);
		FreePool(dp);
		FreePool(ctx);
		return status;
	}

	*out_handle = handle;
	return EFI_SUCCESS;
}
