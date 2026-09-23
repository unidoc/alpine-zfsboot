# Builds stage1.bin (the protective-MBR boot sector) and stage2.bin
# (loaded from the "BIOS boot partition" - see gpt.h) - no new build
# dependencies beyond what build.sh already installs for efi/: gcc,
# binutils (as/ld/objcopy). x86_64-only, same as legacy BIOS itself -
# never invoked at all when build.sh's own $ARCH is aarch64.
#
# -m16, NOT -m32: this whole stage executes inside a genuine 16-bit
# real-mode code segment (CS=SEG, see stage2_entry.S/switch32.S's own
# comments) - confirmed the hard way, by actually disassembling a
# first attempt at this Makefile that used -m32 instead: -m32 emits
# ordinary 32-bit-default instructions with NO 0x66/0x67 override
# prefixes anywhere (since -m32 assumes a NATIVE 32-bit code segment
# will run it), which the CPU would silently reinterpret at THIS
# segment's own 16-bit default the moment it actually ran - wrong
# operand sizes, wrong (truncated) effective addresses, throughout
# every function, not a narrow edge case. -m16 is GCC's own flag
# specifically for this exact situation: it emits the identical
# 32-bit-style code -m32 would, but with the assembler-level prefixes
# needed to make that code behave correctly when it actually executes
# in a 16-bit segment - the same interop GAS's own .code16gcc
# directive provides for the hand-written .S files in this directory.
#
# -march=i386 -mno-mmx -mno-sse -mno-sse2: this code runs before
# anything has configured the FPU/SSE state (CR4.OSFXSR is never set -
# real/protected mode straight off the BIOS has none of that set up),
# so it must never let GCC emit an MMX/SSE instruction at all, not
# just "not use floating point" in the source - -march=i386 alone
# already implies no MMX/SSE by target definition; the explicit -mno-*
# flags are redundant insurance, not because -march=i386 is expected
# to need it.
CC ?= gcc
AS ?= as
LD ?= ld
OBJCOPY ?= objcopy

CFLAGS := -m16 -march=i386 -mno-mmx -mno-sse -mno-sse2 \
          -ffreestanding -fno-pic -fno-stack-protector -fno-strict-aliasing \
          -fno-asynchronous-unwind-tables -Wall -Wextra -O2 -c

STAGE2_C_SRCS := stage2_main.c gpt.c mbr.c fat.c disk.c bootparams.c e820.c console.c
STAGE2_C_OBJS := $(STAGE2_C_SRCS:.c=.o)

# stage-iso.bin: an alternative to stage1.bin+stage2.bin, for booting
# this exact same GPT-lookup/kernel-load code from an El Torito "no
# emulation" CD-ROM boot catalog entry instead of a real, partitioned
# GPT disk - see iso.sh. No separate "stage1" here: El Torito loads
# this WHOLE image upfront in one BIOS-driven read (governed by
# -boot-load-size in iso.sh, not by any code in this directory), unlike
# a real disk's 512-byte MBR, which has no room to do more than fetch a
# second stage itself. Shares every single .c file with the stage2.bin
# above UNCHANGED except disk.c -> cdrom_disk.c + ata_atapi.c (see
# cdrom_disk.c's own header comment for why INT13h alone wasn't
# enough, and ata_atapi.c's for the driver itself) and two real-mode
# segment constants overridden via ISO_SEG below (this build's own
# load segment, 0x7c0 - El Torito's own conventional default for a
# "no emulation" boot image, physical address 0x7c00, the same address
# a normal MBR loads at - vs. 0x1000 for the stage1-loaded-stage2 case).
ISO_SEG := 0x7c0
STAGE2_ISO_C_SRCS := stage2_main.c gpt.c mbr.c fat.c cdrom_disk.c ata_atapi.c bootparams.c e820.c console.c
STAGE2_ISO_C_OBJS := $(STAGE2_ISO_C_SRCS:%.c=iso_%.o)

.PHONY: all clean build_id.h

all: stage1.bin stage2.bin stage-iso.bin

# Regenerated on every single `make` invocation (.PHONY, so this
# recipe always reruns regardless of timestamps) with the current UTC
# time - printed as the very first line of stage2_main() (see that
# file), purely so a boot log unambiguously shows which actual build
# produced the binary under test on real hardware, instead of trying
# to infer that from source-tree/sync state that might not have
# actually made it into the binary being booted.
#
# ZFSBOOT_VERSION: the project's own human-facing semver (e.g.
# "0.1.0", from the repo's own top-level version.txt file) - NOT read
# directly here via a relative path, because build.sh copies only
# this bios/ subdirectory into $BUILD_DIR before invoking `make`
# (see build.sh's own comment on that cp -r), so "../version.txt"
# wouldn't reliably resolve to the real repo root from there. Passed
# down instead as a make variable by whoever invokes this - build.sh
# passes the real repo's version.txt file content; a bare local `make` in
# this directory (a real, supported dev workflow per this Makefile's
# own header comment) gets the "dev" fallback below instead, same
# reasoning as menu.py's own _project_version() fallback.
ZFSBOOT_VERSION ?= dev

build_id.h:
	@printf '#define ZFSBOOT_BUILD_ID "%s"\n#define ZFSBOOT_VERSION "%s"\n' \
		"$$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$(ZFSBOOT_VERSION)" > $@

stage1.o: stage1.S
	$(AS) --32 -o $@ $<

stage1.elf: stage1.o stage1.ld
	$(LD) -m elf_i386 -T stage1.ld -o $@ stage1.o

# stage1 must land as EXACTLY one 512-byte sector with the boot
# signature (0x55 0xAA on disk, little-endian) at bytes 510-511 -
# checked here, not just assumed from stage1.S's own `.org 510`
# (which only guarantees the code fits before that point, not that
# objcopy's output ends up the expected overall shape).
stage1.bin: stage1.elf
	$(OBJCOPY) -O binary $< $@
	@size=$$(wc -c < $@); \
	if [ "$$size" -ne 512 ]; then \
		echo "stage1.bin is $$size bytes, expected exactly 512" >&2; exit 1; \
	fi
	@sig=$$(od -An -tx1 -j 510 -N 2 $@ | tr -d ' '); \
	if [ "$$sig" != "55aa" ]; then \
		echo "stage1.bin boot signature is $$sig, expected 55aa" >&2; exit 1; \
	fi

# fat.c's own real-disk object gets a bigger FAT_IO_BATCH_SECTORS than
# the shared default (see that file's own header comment on this
# constant) - real INT13h/disk.c only, no ATAPI command-batching
# ceiling to respect here, unlike iso_fat.o below (built by the generic
# iso_%.o rule further down, at fat.c's own unmodified default).
# check-bss-bounds (see stage2.elf's own recipe) is what actually
# proves 64 is safe for THIS build - not asserted here, just applied.
fat.o: fat.c bootparams.h console.h disk.h e820.h fat.h gpt.h mbr.h switch32.h stdint_local.h build_id.h
	$(CC) $(CFLAGS) -DFAT_IO_BATCH_SECTORS=64 -o $@ $<

%.o: %.c bootparams.h console.h disk.h e820.h fat.h gpt.h mbr.h switch32.h stdint_local.h build_id.h
	$(CC) $(CFLAGS) -o $@ $<

stage2_entry.o: stage2_entry.S
	$(AS) --32 -o $@ $<

switch32.o: switch32.S
	$(AS) --32 -o $@ $<

stage2.elf: stage2_entry.o $(STAGE2_C_OBJS) switch32.o link.ld
	$(LD) -m elf_i386 -T link.ld -o $@ stage2_entry.o $(STAGE2_C_OBJS) switch32.o
	@$(MAKE) --no-print-directory check-bss-bounds ELF=$@

# Must fit within stage1.S's own STAGE2_SECTORS budget (64 sectors =
# 32768 bytes) - checked here for real rather than trusting stage1's
# fixed-size read to just happen to be enough; bump STAGE2_SECTORS in
# stage1.S (and this check) together if stage2 ever genuinely outgrows it.
stage2.bin: stage2.elf
	$(OBJCOPY) -O binary $< $@
	@size=$$(wc -c < $@); \
	max=32768; \
	if [ "$$size" -gt "$$max" ]; then \
		echo "stage2.bin is $$size bytes, exceeds stage1.S's STAGE2_SECTORS budget ($$max bytes) - bump STAGE2_SECTORS in stage1.S to match" >&2; exit 1; \
	fi

# FORCE_ATAPI=1 make stage-iso.bin - forces cdrom_disk.c's ATAPI backend
# even under QEMU/SeaBIOS, where INT13h normally succeeds and this whole
# driver would otherwise never run during an automated boot test (see
# cdrom_disk.c's own comment on ZFSBOOT_FORCE_ATAPI). Empty by default,
# so a normal build is unaffected.
iso_%.o: %.c bootparams.h console.h disk.h e820.h fat.h gpt.h mbr.h switch32.h ata_atapi.h stdint_local.h build_id.h
	$(CC) $(CFLAGS) -DSTAGE2_SEGMENT=$(ISO_SEG) $(if $(FORCE_ATAPI),-DZFSBOOT_FORCE_ATAPI) -o $@ $<

stage2_entry_iso.o: stage2_entry.S
	$(AS) --32 --defsym SEG=$(ISO_SEG) -o $@ $<

switch32_iso.o: switch32.S
	$(AS) --32 --defsym SEG=$(ISO_SEG) -o $@ $<

# link.ld, unmodified, works unchanged for this target too - every
# symbol in it is already segment-relative (ORIGIN 0), never a
# physical address, so it has no reason to know ISO_SEG's value at all
# (see link.ld's own header comment).
stage-iso.elf: stage2_entry_iso.o $(STAGE2_ISO_C_OBJS) switch32_iso.o link.ld
	$(LD) -m elf_i386 -T link.ld -o $@ stage2_entry_iso.o $(STAGE2_ISO_C_OBJS) switch32_iso.o
	@$(MAKE) --no-print-directory check-bss-bounds ELF=$@

# check-bss-bounds ELF=<path> - real-mode segment-boundary invariant,
# not just a comment: every stage2 symbol (link.ld's own ORIGIN 0,
# single-segment layout - see its header comment) is a plain 16-bit
# offset from this stage's one fixed DS, and disk_read_lba()'s own
# INT13h AH=0x42 DAP (see disk.c) passes buffer_segment=DS,
# buffer_offset=that 16-bit offset directly. g_fat_io_buf (fat.c's own
# batched I/O staging buffer - the thing this actually protects) is
# the last and largest thing link.ld places before _bss_end, so "the
# whole stage fits under offset 0x10000" bounds every possible
# transfer's own offset range to [0, 0xFFFF].
#
# That alone only proves the transfer stays within ITS OWN segment -
# "never crosses a 64KB-ALIGNED PHYSICAL boundary" (the real compat
# rule some BIOSes care about for a DAP transfer, independent of
# segment:offset reasoning) is a stronger claim, and it happens to
# ALSO hold here, but only because of a second fact this check does
# NOT verify and that is NOT a general property of real-mode segments:
# STAGE2_SEGMENT=0x1000 (see stage2_entry.S) times 16 is exactly
# 0x10000, itself already 64KB-aligned. So physical = 0x10000 + offset
# for offset in [0, 0xFFFF] covers EXACTLY the one aligned block
# [0x10000, 0x20000) - offset never crossing 0x10000 is therefore
# equivalent to the physical transfer never crossing an aligned 64KB
# boundary, for THIS specific segment value. (The ISO build's
# SEG=0x7c0*16=0x7C00 is NOT 64KB-aligned - this equivalence does not
# hold there, and its own existing FAT_IO_BATCH_SECTORS=36 default is
# unchanged by any of this, not re-litigated here.) If STAGE2_SEGMENT
# for the real-disk build ever changes from 0x1000, this reasoning -
# not just this check - needs redoing.
#
# A SECOND, DISTINCT invariant this same check enforces: stage2_entry.S
# sets %sp = 0xfff0 as its very first act, once, and never moves it -
# there is no separate stack segment or guard page in real mode, so
# the stack simply grows DOWN from 0xfff0 into whatever memory happens
# to be there. If _bss_end ever crept up to meet or pass 0xfff0, the
# stack's own first pushes (stage2_main()'s call frame, pushed
# immediately on entry - see stage2_entry.S) would silently overwrite
# live .bss data (g_fat_io_buf and everything else placed there),
# corrupting it out from under the very code still using it - with no
# crash, no error, just quietly wrong bytes. This was a REAL, live gap
# until this check was added: the old threshold here was 0x10000, a
# full 16 bytes ABOVE 0xfff0, so a build could pass this check with
# _bss_end sitting mere bytes below the stack's own starting point and
# zero real margin for the stack to operate in - confirmed by actually
# building it that way (temporarily inflating a .bss buffer): the
# linker happily accepted a build with _bss_end == 0xffea, six bytes
# below 0xfff0, while the OLD (>= 65536) check would have reported
# that as fine. The threshold below is tightened to 0xfff0 itself,
# plus STAGE2_STACK_MARGIN bytes of real headroom for the stack to
# actually run in - not just "doesn't literally overlap yet".
# STAGE2_STACK_MARGIN=2048 is a conservative, documented estimate, not
# a precise stack-depth proof: the deepest real call chain in this
# stage (stage2_main -> gpt_find_partition/fat_mount/fat_open/
# fat_read_range -> disk_read_lba/atapi_send_packet -> the real-mode
# BIOS interrupt) is well under a dozen frames, and the largest
# per-frame locals are stage2_main()'s own struct gpt_header/
# fat_volume/setup_header (under 500 bytes combined, in ONE frame, not
# repeated across the chain) - every other frame's locals are small
# fixed structs (struct disk_address_packet, struct fat_chain_walk,
# a handful of small buffers under 20 bytes). 2KB is generous headroom
# over any realistic sum of that, while still catching the kind of
# near-zero-margin build this check exists to prevent. Both current
# real builds pass with room to spare (disk build's own _bss_end is
# comfortably low; the tighter ISO build's real _bss_end leaves ~3.6KB
# below 0xfff0 today) - if a future change to either build ever erodes
# that margin below STAGE2_STACK_MARGIN, this check is what is
# supposed to catch it, not a real-hardware stack-corruption bug report.
STAGE2_STACK_TOP := 65520
STAGE2_STACK_MARGIN := 2048
check-bss-bounds:
	@bss_end_hex=$$(nm "$(ELF)" | awk '$$3 == "_bss_end" {print $$1}'); \
	if [ -z "$$bss_end_hex" ]; then \
		echo "check-bss-bounds: could not find _bss_end in $(ELF)" >&2; exit 1; \
	fi; \
	bss_end=$$((0x$$bss_end_hex)); \
	if [ "$$bss_end" -ge 65536 ]; then \
		echo "check-bss-bounds: $(ELF) _bss_end is $$bss_end (0x$$bss_end_hex), >= 0x10000 - stage2's data area no longer fits in one real-mode segment, disk_read_lba()'s DAP buffer_offset math (see disk.c) is no longer safe" >&2; exit 1; \
	fi; \
	limit=$$(($(STAGE2_STACK_TOP) - $(STAGE2_STACK_MARGIN))); \
	if [ "$$bss_end" -gt "$$limit" ]; then \
		echo "check-bss-bounds: $(ELF) _bss_end is $$bss_end (0x$$bss_end_hex), within $(STAGE2_STACK_MARGIN) bytes of the stack's own start (sp=0x$$(printf '%x' $(STAGE2_STACK_TOP)), set once in stage2_entry.S and never moved) - .bss and the downward-growing real-mode stack are no longer guaranteed not to collide; shrink a .bss buffer or raise STAGE2_STACK_MARGIN only after re-checking the worst-case call depth this comment describes" >&2; exit 1; \
	fi

# Budget matches stage2.bin's own (32768 bytes) - not tied to anything
# El Torito itself imposes (its own -boot-load-size cap is far larger,
# 65535 512-byte sectors - see iso.sh), just kept consistent with the
# disk path's existing, generous headroom for genuinely compact C.
stage-iso.bin: stage-iso.elf
	$(OBJCOPY) -O binary $< $@
	@size=$$(wc -c < $@); \
	max=32768; \
	if [ "$$size" -gt "$$max" ]; then \
		echo "stage-iso.bin is $$size bytes, exceeds the 32768-byte budget - see this Makefile's own stage-iso.bin target" >&2; exit 1; \
	fi

clean:
	rm -f *.o *.elf stage1.bin stage2.bin stage-iso.bin build_id.h
