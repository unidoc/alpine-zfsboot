#!/bin/sh
# iso.sh EFI_FILE ARCH [BIOS_STAGE_ISO_BIN KERNEL INITRD CMDLINE] -> a
# plain bootable ISO wrapping that exact .EFI, next to it (same name,
# .iso instead of .EFI). No boot logic here at all - this is
# packaging only, for anywhere a bare .EFI file isn't a valid boot
# target on its own: BMC/IPMI virtual media, a real USB stick, a VM's
# virtual CDROM.
#
# The four extra arguments (x86_64 only - build.sh never passes them
# for aarch64, which has no legacy-BIOS equivalent at all) add a
# SECOND, independent way to boot the same ISO: legacy BIOS, via an
# El Torito "no emulation" boot catalog entry pointing at
# bios/stage-iso.bin (see that file's own header comment in
# bios/Makefile). stage-iso.bin's own FAT32 reader (bios/fat.c) finds
# KERNEL/INITRD/CMDLINE on the SAME appended FAT image the UEFI entry
# below already uses (efiboot.img, appended once as GPT partition 2) -
# alpine-zfsboot's own unified storage architecture applies here too:
# one FAT filesystem, one partition, both firmware paths, not two
# separate images/formats the way an earlier version of this project
# (a raw, filesystem-less "boot blob" partition for BIOS only) used to
# need. The UEFI path below is otherwise unmodified - these two boot
# methods are independent catalog entries, neither aware of the other.
#
# The xorrisofs invocation below is deliberately NOT a from-scratch
# read of the documentation - it mirrors archiso's own
# _add_common_xorrisofs_options_uefi() (archiso/mkarchiso in the real
# archlinux/archiso repo), the exact recipe every real Arch Linux
# install ISO ships with, adapted down to this project's own
# single-appended-partition UEFI case. Getting there took two real,
# different boot failures from two earlier from-scratch attempts at
# this - documented xorrisofs syntax in isolation was not enough to
# get this right; matching known-good, widely-used real code was:
#
# - `-partition_offset 16` moves the first partition away from byte 0
#   of the image - without it, archiso's own comment says the GPT
#   isn't valid and the ISO9660 partition isn't mountable, which lines
#   up exactly with a real OVMF failure this project hit
#   ("BdsDxe: ... Not Found" - no valid boot catalog/filesystem found
#   at all, worse than the first attempt's "Load Error").
# - `-append_partition 2 C12A7328-F81F-11D2-BA4B-00A0C93EC93B FILE` -
#   that long value is the real GPT partition-type GUID for "EFI
#   System Partition". The short MBR type byte (0xef) an earlier
#   version of this script used is the WRONG thing to pass once
#   `-appended_part_as_gpt` is in play - GPT wants its own GUID, not
#   an MBR type code, and firmware scanning the GPT for an ESP won't
#   recognize a partition that isn't correctly typed as one.
# - `-eltorito-alt-boot` IS included here even with no BIOS El Torito
#   entry present (the common, no-BIOS-args case) - archiso's own
#   working code includes it unconditionally for the UEFI entry
#   regardless of whether a BIOS entry exists, contradicting an
#   earlier, documentation-only guess in this script that it should be
#   omitted with no BIOS entry present. Real working code wins over
#   that guess. When a BIOS entry IS present (see below), this same
#   option does its normal, documented job: separating the BIOS
#   catalog entry that precedes it from the UEFI one that follows.
# - `-append_partition` must be given BEFORE the `-e
#   --interval:appended_partition_2:...` (or, for the BIOS entry, `-b
#   --interval:appended_partition_3:...`) that references it - xorriso
#   resolves that reference against whatever's already been declared
#   at the point `-e`/`-b` is parsed, not the full option set.
# - `-eltorito-catalog EFI/boot.cat` gives the (synthesized, not
#   sourced from the input tree) boot catalog file an explicit,
#   predictable location rather than leaving it to a default that
#   might collide with expectations.
#
# The BIOS entry's own ordering rule (confirmed against xorriso's real
# man page text, not guessed): a `-b` (BIOS) entry and an `-e` (UEFI)
# entry in the SAME xorrisofs invocation need the BIOS entry FIRST -
# it becomes El Torito's "Initial/Default" catalog entry, which legacy
# BIOS reads by default - THEN `-eltorito-alt-boot`, THEN the UEFI
# entry (the "alternate" one EFI firmware specifically scans for).
# Getting this backwards produces a "not bootable" on BIOS with no
# other symptom - there's no error message pointing at the real cause.
#
# The BIOS entry uses `-no-emul-boot` (the same "no emulation" mode
# every mainstream hybrid Linux ISO's BIOS entry uses - isolinux,
# GRUB2's cdboot.img), NOT `-hard-disk-boot` ("hard disk emulation") -
# xorrisofs's own man page states outright, twice, that hard-disk-boot
# is "not suitable for any known boot loader"; no real-world ISO
# builder (archiso, debian-cd, grub-mkrescue) uses it for a BIOS entry,
# for good reason. `-boot-load-size` is the ONE thing that mode
# requires that a normal, tiny 512-byte MBR sector never needs to
# think about: it tells BIOS how many 512-byte "virtual" sectors of
# stage-iso.bin to load upfront, in ONE BIOS-driven read, before
# jumping to it (El Torito's own load-size unit is always 512 bytes
# regardless of the media's real 2048-byte native block size - this is
# a SEPARATE thing from stage-iso.bin's OWN later, on-demand INT13h
# reads against the CD-ROM drive to fetch the actual boot-blob, which
# use the drive's native 2048-byte units instead - see bios/
# cdrom_disk.c's own header comment for the full reasoning and the
# real SeaBIOS/libisofs source lines that confirm both units).
# No load-segment option is given (confirmed the hard way: xorrisofs's
# own mkisofs-compatible dialect has -boot-load-size but genuinely no
# equivalent for the load SEGMENT at all - not an oversight in this
# script, that option simply doesn't exist here) - relying instead on
# El Torito's own documented default ("Load Segment = 0" means "BIOS
# uses 0x7c0"), which is exactly the value stage-iso.bin's own code
# (see bios/Makefile's ISO_SEG) is built assuming.
#
# Needs: xorriso, dosfstools (mkfs.vfat), mtools (mmd/mcopy) - not
# needed by build.sh itself, so not unconditionally required there;
# only pulled in by whoever actually wants an .iso (see Justfile's
# build-iso recipe and release.yml).
set -eu

usage="usage: iso.sh EFI_FILE ARCH [BIOS_STAGE_ISO_BIN KERNEL INITRD CMDLINE]"
EFI_FILE="${1:?$usage}"
ARCH="${2:?$usage}"
BIOS_STAGE_ISO_BIN="${3:-}"
KERNEL_FILE="${4:-}"
INITRD_FILE="${5:-}"
CMDLINE_FILE="${6:-}"
[ -f "$EFI_FILE" ] || { echo "$EFI_FILE not found" >&2; exit 1; }
ISO_FILE="${EFI_FILE%.EFI}.iso"

case "$ARCH" in
    x86_64)  BOOT_NAME=BOOTX64.EFI ;;
    aarch64) BOOT_NAME=BOOTAA64.EFI ;;
    *) echo "unsupported ARCH: $ARCH (expected x86_64 or aarch64)" >&2; exit 1 ;;
esac

if [ -n "$BIOS_STAGE_ISO_BIN" ] && [ "$ARCH" != "x86_64" ]; then
    echo "BIOS_STAGE_ISO_BIN was given but ARCH is $ARCH - legacy BIOS only exists on x86_64" >&2
    exit 1
fi
if [ -n "$BIOS_STAGE_ISO_BIN" ]; then
    [ -f "$BIOS_STAGE_ISO_BIN" ] || { echo "$BIOS_STAGE_ISO_BIN not found" >&2; exit 1; }
    [ -f "$KERNEL_FILE" ] || { echo "$KERNEL_FILE not found" >&2; exit 1; }
    [ -f "$INITRD_FILE" ] || { echo "$INITRD_FILE not found" >&2; exit 1; }
    [ -f "$CMDLINE_FILE" ] || { echo "$CMDLINE_FILE not found" >&2; exit 1; }
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# FAT image sized to everything it needs to hold - the .EFI, plus (when
# a BIOS entry is present) the loose kernel/initrd/cmdline stage-iso.bin's
# own FAT reader loads directly off this same image - plus headroom for
# the filesystem's own overhead and directory entries, rounded up to a
# whole MiB (mkfs.vfat wants a MiB-aligned size). 8MiB of headroom, not
# the previous 4MiB: this image can now carry a real kernel+initrd, not
# just a bare .EFI stub.
efi_size=$(wc -c < "$EFI_FILE")
payload_size=$efi_size
if [ -n "$BIOS_STAGE_ISO_BIN" ]; then
    payload_size=$(( payload_size + $(wc -c < "$KERNEL_FILE") + $(wc -c < "$INITRD_FILE") + $(wc -c < "$CMDLINE_FILE") ))
fi
img_size_mb=$(( (payload_size / 1024 / 1024) + 8 ))

# The ISO9660 tree carries no copy of the .EFI itself - the appended
# partition below is the only one, and El Torito points straight at it
# (a second copy living in the tree too would just double the image's
# size for nothing). It DOES need an EFI/ directory, though: xorriso
# writes the (synthesized) El Torito boot catalog to the path
# `-eltorito-catalog` names below, and - confirmed the hard way, a
# real build failed with "Cannot find directory for El Torito boot
# catalog in ISO image: '/EFI'" - it does not create that directory
# itself if the source tree doesn't already have it.
mkdir -p "$WORK/iso/EFI"

dd if=/dev/zero of="$WORK/efiboot.img" bs=1M count="$img_size_mb" status=none
mkfs.vfat -F32 -n ZFSBOOT "$WORK/efiboot.img" >/dev/null
mmd -i "$WORK/efiboot.img" ::EFI ::EFI/BOOT
mcopy -i "$WORK/efiboot.img" "$EFI_FILE" "::EFI/BOOT/$BOOT_NAME"
if [ -n "$BIOS_STAGE_ISO_BIN" ]; then
    # Short, 8.3-safe path components on purpose - stage-iso.bin's own
    # FAT reader (bios/fat.c) never parses VFAT long-filename entries,
    # deliberately, to stay small in this boot-critical code. See
    # bios/fat.h's own header comment for the full reasoning; this
    # must stay byte-for-byte in sync with the path stage2_main.c
    # actually opens.
    mmd -i "$WORK/efiboot.img" ::EFI/ALPINE
    mcopy -i "$WORK/efiboot.img" "$KERNEL_FILE" ::EFI/ALPINE/KERNEL
    mcopy -i "$WORK/efiboot.img" "$INITRD_FILE" ::EFI/ALPINE/INITRD
    mcopy -i "$WORK/efiboot.img" "$CMDLINE_FILE" ::EFI/ALPINE/CMDLINE
fi

# --- optional legacy-BIOS El Torito entry (x86_64, when the caller
# built the BIOS artifacts - see build.sh) --------------------------
bios_append_args=""
bios_boot_args=""
if [ -n "$BIOS_STAGE_ISO_BIN" ]; then
    # El Torito's own load-size unit is always 512 bytes (a fixed,
    # standardized "virtual sector" size, independent of the media's
    # real 2048-byte native block size) - see this file's own header
    # comment. 65535 is El Torito's own hard cap on this field
    # (xorrisofs's man page: "El Torito cannot represent load sizes
    # higher than 65535") - stage-iso.bin is a tiny, freestanding boot
    # stage (bios/Makefile caps it at 32768 bytes = 64 sectors), so
    # this is only ever a sanity check against something going very
    # wrong upstream, never expected to actually trip.
    stage_iso_bytes=$(wc -c < "$BIOS_STAGE_ISO_BIN")
    boot_load_size=$(( (stage_iso_bytes + 511) / 512 ))
    if [ "$boot_load_size" -gt 65535 ]; then
        echo "$BIOS_STAGE_ISO_BIN is $stage_iso_bytes bytes - its El Torito boot-load-size ($boot_load_size 512-byte sectors) exceeds El Torito's own 65535-sector cap" >&2
        exit 1
    fi

    # 21686148-6449-6E6F-744E-656564454649: GRUB's own "BIOS boot
    # partition" GUID, labeling only (same convention as the real-disk
    # BIOS path's own stage1+stage2 partition - see bios/gpt.h's own
    # comment) - nothing looks this partition up by type, BIOS finds
    # stage-iso.bin via the El Torito catalog instead.
    #
    # No separate bootblob partition any more - stage-iso.bin's own
    # gpt.c call (completely unmodified) finds the SAME appended
    # partition 2 (efiboot.img, ESP-GUID-typed, already declared above)
    # that the UEFI entry uses, and reads KERNEL/INITRD/CMDLINE off it
    # via bios/fat.c - one FAT partition on this ISO's own
    # xorriso-written GPT, exactly as on a real GPT disk.
    bios_append_args="-append_partition 3 21686148-6449-6E6F-744E-656564454649 $BIOS_STAGE_ISO_BIN"
    bios_boot_args="-b --interval:appended_partition_3:all:: -no-emul-boot -boot-load-size $boot_load_size"
fi

# shellcheck disable=SC2086 # bios_append_args/bios_boot_args are
# deliberately unquoted (each expands to zero or several separate
# xorriso arguments, never one with embedded spaces - $BIOS_STAGE_ISO_BIN
# is this project's own build output path, not untrusted/arbitrary
# input).
xorriso -as mkisofs \
    -iso-level 3 -full-iso9660-filenames \
    -volid ALPINE_ZFSBOOT \
    -partition_offset 16 \
    -append_partition 2 C12A7328-F81F-11D2-BA4B-00A0C93EC93B "$WORK/efiboot.img" \
    $bios_append_args \
    -appended_part_as_gpt \
    $bios_boot_args \
    -eltorito-alt-boot \
    -e --interval:appended_partition_2:all:: -no-emul-boot \
    -eltorito-catalog EFI/boot.cat \
    -o "$ISO_FILE" \
    "$WORK/iso"

if [ -n "$BIOS_STAGE_ISO_BIN" ]; then
    echo "built: $ISO_FILE (UEFI + legacy BIOS)"
else
    echo "built: $ISO_FILE (UEFI only)"
fi
