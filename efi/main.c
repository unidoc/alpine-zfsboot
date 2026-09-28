#include <efi.h>
#include <efilib.h>

#include "pe_sections.h"
#include "initrd.h"
#include "cmdline_core.h"

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

	status = pe_find_section(loaded_image->ImageBase, (UINTN)loaded_image->ImageSize, (CHAR8 *)".linux", &linux_sec);
	if (EFI_ERROR(status)) {
		Print(L"alpine-zfsboot: no .linux section found: %r\n", status);
		return status;
	}
	status = pe_find_section(loaded_image->ImageBase, (UINTN)loaded_image->ImageSize, (CHAR8 *)".initrd", &initrd_sec);
	if (EFI_ERROR(status)) {
		Print(L"alpine-zfsboot: no .initrd section found: %r\n", status);
		return status;
	}
	status = pe_find_section(loaded_image->ImageBase, (UINTN)loaded_image->ImageSize, (CHAR8 *)".cmdline", &cmdline_sec);
	if (EFI_ERROR(status)) {
		Print(L"alpine-zfsboot: no .cmdline section found: %r\n", status);
		return status;
	}

	Print(L"alpine-zfsboot: kernel at 0x%lx (%lu bytes), initrd at 0x%lx (%lu bytes)\n",
		(UINTN)linux_sec.data, linux_sec.size,
		(UINTN)initrd_sec.data, initrd_sec.size);

	/*
	 * Firmware-supplied boot options for THIS image - whatever a boot
	 * manager's own "edit boot options" screen passed (e.g. rEFInd's
	 * own, shown in its log as "Using load options '...'" right before
	 * this loader starts). loaded_image->LoadOptions was already being
	 * read above (for ImageBase/ImageSize) but never otherwise used -
	 * every operator-typed boot option was silently discarded, with no
	 * way to override anything in this project's own embedded
	 * .cmdline short of rebuilding the whole .EFI. Real gap, found
	 * live: an operator on OVH/Kimsufi-class bare metal (whose serial-
	 * console redirection lands on a different port than this build's
	 * own embedded console= assumes) had no way to pass
	 * alpine-zfsboot.console=ttyS1 at boot time to fix it - editing
	 * rEFInd's own boot options did nothing, because nothing here ever
	 * looked at them.
	 *
	 * Appended after the embedded cmdline (space-separated), not
	 * prepended - matching every other alpine-zfsboot.* key's own
	 * "last one wins" convention (see init/init's own
	 * apply_zfsboot_kv()/select_console()): an operator's own boot-time
	 * addition should override the build's own baked-in default, the
	 * same way it already does for every other project-defined key.
	 *
	 * PR #16 review (F4): launched from a UEFI Shell instead, LoadOptions
	 * starts with this image's own path (e.g. "\loader.efi
	 * alpine-zfsboot.console=ttyS1") - a harmless extra token once
	 * appended, since apply_zfsboot_kv() (init/init) already ignores
	 * anything it doesn't recognize as one of its own keys.
	 */
	{
		UINTN load_options_len = load_options_strnlen16(
			(CHAR16 *)loaded_image->LoadOptions,
			loaded_image->LoadOptionsSize / sizeof(CHAR16));
		UINTN total_chars;

		/*
		 * PR #16 review (F4): LoadOptions is an opaque buffer, not
		 * guaranteed to be text - see load_options_is_text()'s own
		 * comment for the two real non-text shapes this guards
		 * against (Dell's full EFI_LOAD_OPTION struct, EDK2's
		 * auto-created-option GUID). Appending either verbatim would
		 * put binary garbage into what the kernel then parses as its
		 * own command line - worse than just ignoring it.
		 */
		if (load_options_len > 0 &&
		    !load_options_is_text((CHAR16 *)loaded_image->LoadOptions, load_options_len)) {
			Print(L"alpine-zfsboot: ignoring non-text LoadOptions (%d bytes)\n",
				(UINTN)loaded_image->LoadOptionsSize);
			load_options_len = 0;
		}

		total_chars = combined_cmdline16_len((CHAR8 *)cmdline_sec.data, load_options_len);

		cmdline16 = AllocatePool(total_chars * sizeof(CHAR16));
		if (cmdline16 == NULL) {
			Print(L"alpine-zfsboot: out of memory building cmdline\n");
			return EFI_OUT_OF_RESOURCES;
		}
		build_combined_cmdline16((CHAR8 *)cmdline_sec.data,
			(CHAR16 *)loaded_image->LoadOptions, load_options_len, cmdline16);
		cmdline16_size = (UINT32)(total_chars * sizeof(CHAR16));
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
