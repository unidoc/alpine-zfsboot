# Builds this project's own EFI loader (see main.c for the design)
# against the real gnu-efi/gnu-efi-dev apk packages - the standard
# gnu-efi build recipe (ld to a shared object with gnu-efi's own
# linker script, then objcopy to the efi-app-$(ARCH) PE/COFF target),
# nothing project-specific.
ARCH        ?= $(shell uname -m)
CC          ?= gcc
OBJCOPY     ?= objcopy

EFIINC       = /usr/include/efi
EFIINCS      = -I$(EFIINC) -I$(EFIINC)/$(ARCH) -I$(EFIINC)/protocol
EFILIB       = /usr/lib
EFI_CRT_OBJS = $(EFILIB)/crt0-efi-$(ARCH).o
EFI_LDS      = $(EFILIB)/elf_$(ARCH)_efi.lds

CFLAGS  = $(EFIINCS) -fpic -ffreestanding -fshort-wchar -fno-strict-aliasing \
	  -fno-stack-protector -fno-stack-check -Wall -Wextra -Werror \
	  -std=c11
LDFLAGS = -nostdlib -znocombreloc -shared -Bsymbolic -L$(EFILIB) \
	  -T $(EFI_LDS)

ifeq ($(ARCH),x86_64)
# NOT -DGNU_EFI_USE_MS_ABI: that's a BUILD-WIDE switch, and efi_main
# is called by gnu-efi's own crt0-efi-x86_64.o - a precompiled object
# this build doesn't control, built expecting the plain SysV ABI (the
# gnu-efi default). Defining it build-wide made efi_main ITSELF
# require MS-ABI incoming arguments while crt0 kept calling it via
# SysV, which is a real regression this project hit directly (efi_main
# reading its own image handle as garbage, failing its very first
# BS->HandleProtocol call). The one function that genuinely needs MS
# ABI - initrd.c's LoadFile2 callback, called directly by the kernel's
# own EFI stub rather than by anything in this build - gets it via an
# explicit, local __attribute__((ms_abi)) instead (see initrd.c).
CFLAGS += -mno-red-zone
endif

OBJS = main.o pe_sections.o initrd.o cmdline.o

all: loader.efi

%.o: %.c
	$(CC) $(CFLAGS) -c $< -o $@

loader.so: $(OBJS)
	ld $(LDFLAGS) $(EFI_CRT_OBJS) $(OBJS) -o $@ \
	  -lefi -lgnuefi $(shell $(CC) -print-libgcc-file-name)

loader.efi: loader.so
	# -j .rodata is not optional: every Print(L"...") format string
	# (any CHAR16/wide string literal) is compiler-placed there, not
	# in .data - dropping it left every Print() call reading a
	# zero-filled string, i.e. no visible output at all, confirmed by
	# grepping for a known string's bytes in the linked .so vs. the
	# object this recipe used to produce (present in one, absent in
	# the other).
	$(OBJCOPY) -j .text -j .sdata -j .data -j .rodata -j .dynamic \
	  -j .dynsym -j .rel -j .rela -j .reloc \
	  --target=efi-app-$(ARCH) $< $@

clean:
	rm -f *.o loader.so loader.efi

.PHONY: all clean
