#include <efi.h>
#include <efilib.h>

#include "pe_sections.h"
#include "initrd.h"
#include "cmdline.h"

/*
 * alpine-zfsboot's own EFI loader.
 *
 * Deliberately dumb, by design: this does NOT implement any part of
 * the Linux/x86 boot protocol (setup_header, boot_params, the EFI
 * handover entry) - that machinery is real, but entirely x86-specific,
 * with no arm64 equivalent. Instead this loads the embedded kernel
 * image as an ordinary PE/COFF EFI application
 * (LoadImage/StartImage) and lets the kernel's OWN EFI stub
 * (CONFIG_EFI_STUB, built into vmlinuz on x86_64 and Image on arm64)
 * do the arch-specific handoff itself. The only thing this loader
 * still owns is getting the kernel its command line (LoadOptions) and
 * its initrd (EFI_LOAD_FILE2_PROTOCOL, see initrd.c) - both
 * mechanisms the kernel's stub already expects on every architecture
 * it supports, per Documentation/arch/x86/efistub.rst.
 */
EFI_STATUS EFIAPI efi_main(EFI_HANDLE image, EFI_SYSTEM_TABLE *systab)
{
	EFI_STATUS status;
	EFI_LOADED_IMAGE *loaded_image;
	EFI_LOADED_IMAGE *kernel_image;
	EFI_HANDLE kernel_handle = NULL;
	EFI_HANDLE initrd_handle = NULL;
	pe_section_t linux_sec, initrd_sec, cmdline_sec;
	CHAR16 *cmdline16;
	UINT32 cmdline16_size;

	InitializeLib(image, systab);

	Print(L"alpine-zfsboot: loader started\n");

	status = uefi_call_wrapper(BS->HandleProtocol, 3,
		image, &LoadedImageProtocol, (VOID **)&loaded_image);
	if (EFI_ERROR(status)) {
		Print(L"alpine-zfsboot: cannot get own loaded-image protocol: %r\n", status);
		return status;
	}

	status = pe_find_section(loaded_image->ImageBase, (CHAR8 *)".linux", &linux_sec);
	if (EFI_ERROR(status)) {
		Print(L"alpine-zfsboot: no .linux section found: %r\n", status);
		return status;
	}
	status = pe_find_section(loaded_image->ImageBase, (CHAR8 *)".initrd", &initrd_sec);
	if (EFI_ERROR(status)) {
		Print(L"alpine-zfsboot: no .initrd section found: %r\n", status);
		return status;
	}
	status = pe_find_section(loaded_image->ImageBase, (CHAR8 *)".cmdline", &cmdline_sec);
	if (EFI_ERROR(status)) {
		Print(L"alpine-zfsboot: no .cmdline section found: %r\n", status);
		return status;
	}

	Print(L"alpine-zfsboot: kernel at 0x%lx (%lu bytes), initrd at 0x%lx (%lu bytes)\n",
		(UINTN)linux_sec.data, linux_sec.size,
		(UINTN)initrd_sec.data, initrd_sec.size);

	status = ascii_to_cmdline16((CHAR8 *)cmdline_sec.data, &cmdline16, &cmdline16_size);
	if (EFI_ERROR(status)) {
		Print(L"alpine-zfsboot: cmdline conversion failed: %r\n", status);
		return status;
	}
	Print(L"alpine-zfsboot: cmdline: %s\n", cmdline16);

	/*
	 * Must be installed before StartImage() below - the kernel's own
	 * EFI stub calls back into this protocol during its own
	 * initialization, which happens inside StartImage(), not after
	 * it returns.
	 */
	status = initrd_install(initrd_sec.data, initrd_sec.size, &initrd_handle);
	if (EFI_ERROR(status)) {
		Print(L"alpine-zfsboot: installing initrd LoadFile2 protocol failed: %r\n", status);
		return status;
	}

	status = uefi_call_wrapper(BS->LoadImage, 6,
		FALSE, image, NULL, linux_sec.data, linux_sec.size, &kernel_handle);
	if (EFI_ERROR(status)) {
		Print(L"alpine-zfsboot: LoadImage(kernel) failed: %r\n", status);
		return status;
	}

	status = uefi_call_wrapper(BS->HandleProtocol, 3,
		kernel_handle, &LoadedImageProtocol, (VOID **)&kernel_image);
	if (EFI_ERROR(status)) {
		Print(L"alpine-zfsboot: cannot get kernel's loaded-image protocol: %r\n", status);
		return status;
	}
	kernel_image->LoadOptions = cmdline16;
	kernel_image->LoadOptionsSize = cmdline16_size;

	Print(L"alpine-zfsboot: starting kernel\n");
	status = uefi_call_wrapper(BS->StartImage, 3, kernel_handle, NULL, NULL);

	Print(L"alpine-zfsboot: kernel returned unexpectedly: %r\n", status);
	return status;
}
