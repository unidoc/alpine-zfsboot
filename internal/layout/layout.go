// Package layout is the one place in this repo's Go code that knows
// alpine-zfsboot's on-disk boot contract: where stage1/stage2 live on
// a BIOS disk, what GPT/MBR partition types mark the canonical FAT/ESP
// partition, and what paths inside that FAT filesystem hold the boot
// payload and machine config. Every other Go package that reads or
// writes a boot artifact (internal/biosboot, internal/espconfig,
// internal/uefiboot, internal/bootenv, cmd/tool) imports these
// constants instead of repeating them - the point is exactly one
// codebase-wide source, matching this project's own stated goal for
// the CLI unification work this package was written for.
//
// stage1/stage2's own placement (LBA 34, 64 sectors) is NOT duplicated
// here as an independent guess: bios/stage1.S is the real, load-bearing
// source (its `.set STAGE2_LBA`/`.set STAGE2_SECTORS` directives are
// what stage1's own machine code actually reads at boot) and cannot
// itself be written in Go or shared via a C header without disturbing
// a hand-tuned, freestanding 16-bit assembly file. Stage1LBA/
// Stage2Sectors below are a manually-kept mirror of those two values -
// layout_test.go's TestStage2PlacementMatchesStage1S is what keeps
// them from silently drifting apart: it greps the real bios/stage1.S
// and fails the build if the numbers disagree.
package layout

// SectorSize is the one sector size this whole project assumes
// everywhere - bios/fat.c's own fat_mount() refuses to mount a FAT32
// volume with any other bytes-per-sector value, and every LBA/byte
// conversion in this package follows from it.
const SectorSize = 512

// Stage1Bytes is how many bytes of LBA 0 are stage1's own boot code -
// deliberately NOT the full 512-byte sector: bytes 440-511 are the
// MBR's own disk ID, real partition table, and 0x55AA boot signature,
// written by sgdisk/sfdisk at partition time and never stage1's to
// touch (see alpine-install-zfs.sh's own `dd bs=440 count=1` - this is
// that same 440, not re-guessed).
const Stage1Bytes = 440

// Stage2LBA and Stage2Sectors mirror bios/stage1.S's own
// `.set STAGE2_LBA, 34` / `.set STAGE2_SECTORS, 64` - see this
// package's own doc comment for why these can only be a manually-kept
// mirror, checked by TestStage2PlacementMatchesStage1S, not a shared
// source.
const (
	Stage2LBA     = 34
	Stage2Sectors = 64
)

// Stage2Bytes is the fixed size of the region stage1 reads for stage2
// - stage2.bin itself may be (and today is) considerably smaller; the
// rest of this region must read back as zero after a write (see
// internal/biosboot's own WriteStage2) so the on-disk state stays a
// simple, checkable invariant instead of depending on what a
// previous, possibly-larger stage2.bin left behind.
const Stage2Bytes = Stage2Sectors * SectorSize

// ESPTypeGUIDBytes is the real, standard EFI System Partition type
// GUID (C12A7328-F81F-11D2-BA4B-00A0C93EC93B), in the same on-disk
// GPT mixed-endian byte order as bios/gpt.h's own
// ZFSBOOT_ESP_TYPE_GUID_BYTES - copy of that exact value, not an
// independent encoding.
var ESPTypeGUIDBytes = [16]byte{
	0x28, 0x73, 0x2a, 0xc1, 0x1f, 0xf8, 0xd2, 0x11,
	0xba, 0x4b, 0x00, 0xa0, 0xc9, 0x3e, 0xc9, 0x3b,
}

// ESPTypeGUIDString is ESPTypeGUIDBytes's canonical string form, the
// same shorthand `sgdisk -t` already accepts as `EF00` for - callers
// that need to print or compare against `sgdisk -i`/`blkid` output
// use this string form rather than re-deriving it from the byte
// encoding above.
const ESPTypeGUIDString = "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"

// BIOSBootPartitionGUIDBytes is the STANDARD GPT "BIOS boot partition"
// type GUID (21686148-6449-6E6F-744E-656564454649 - the same one
// GRUB's own bios_grub partition uses), in on-disk GPT mixed-endian
// byte order, hand-derived and cross-checked field by field against
// the string form (first three fields byte-reversed, last two as-is -
// same rule ESPTypeGUIDBytes above follows). alpine-install-zfs.sh's
// own `partition_disk()` still writes a cosmetic partition-table entry
// with this type at the stage2 extent on a fresh GPT install (NOT read
// by stage1/stage2 themselves at boot - see bios/gpt.h's own comment:
// they use the fixed LBA directly, no GPT lookup at all) - kept here
// purely as documentation of that real, still-produced on-disk
// convention. NOT used as a safety or ownership check by any Go code
// in this repo (an earlier version of internal/bootenv did exactly
// that and was wrong to - see that package's own
// CheckStage2ExtentFree doc comment for why: this GUID alone is not
// project-specific, GRUB uses it too, and a real production system
// migrated via this project's own update-stage2-only.sh has no such
// entry at all despite being a completely valid installation).
var BIOSBootPartitionGUIDBytes = [16]byte{
	0x48, 0x61, 0x68, 0x21, 0x49, 0x64, 0x6f, 0x6e,
	0x74, 0x4e, 0x65, 0x65, 0x64, 0x45, 0x46, 0x49,
}

// Stage2PartitionName is the GPT partition NAME
// alpine-install-zfs.sh's own `partition_disk()` writes for stage2's
// cosmetic reservation (`sgdisk -c N:alpine-zfsboot-stage2`) on a
// fresh GPT install - like BIOSBootPartitionGUIDBytes above, kept here
// as documentation of that real on-disk convention, not as an
// ownership or safety check any Go code relies on.
const Stage2PartitionName = "alpine-zfsboot-stage2"

// Stage2MSDOSType mirrors alpine-install-zfs.sh's own
// ALPINE_ZFSBOOT_STAGE2_MBR_TYPE ("2D") - the msdos-layout
// counterpart to BIOSBootPartitionGUIDBytes/Stage2PartitionName for
// GPT, same status: real, still-written cosmetic metadata (distinct
// from FATMBRType, 0x2E, the separate FAT/ESP partition), not
// something any Go code in this repo checks or requires.
const Stage2MSDOSType = 0x2D

// FATMBRType mirrors bios/mbr.h's own ZFSBOOT_FAT_MBR_TYPE - the
// msdos-layout partition type byte for the canonical FAT/ESP
// partition on a BIOS+msdos disk (GPT layouts use ESPTypeGUIDString
// instead; msdos has no GUID concept).
const FATMBRType = 0x2E

// FATMBRTypeString is FATMBRType formatted the way sfdisk's own
// `type=` field expects it (see alpine-install-zfs.sh's own
// ALPINE_ZFSBOOT_FAT_MBR_TYPE="2E").
const FATMBRTypeString = "2E"

// FATVolumeLabel is the FAT32 volume label alpine-install-zfs.sh's
// own `format_boot_partition()` sets (`mkfs.vfat -F32 -n EFI`) and
// init/init's own ESP-discovery block matches on
// (`blkid | grep 'LABEL="EFI"'`) - internal/bootenv.FindESP ports that
// same lookup, so it must use this same label, not a re-guessed one.
const FATVolumeLabel = "EFI"

// ESPDir is the one canonical directory on the FAT/ESP partition that
// holds every alpine-zfsboot-owned file - both stage2's own 8.3-safe
// boot payload (Kernel/Initrd/Cmdline below) and /init's own
// long-named config/authorized_keys/host-key files. See this
// project's own FAT-unification architecture writeup for why these
// live side by side in one directory rather than two.
const ESPDir = "EFI/ALPINE"

// Boot-payload file names within ESPDir - short, 8.3-safe, uppercase,
// exactly what bios/stage2_main.c's own fat_open() calls resolve
// (deliberately no VFAT long-filename support in that reader - see
// bios/fat.h's own header comment).
const (
	KernelFile  = ESPDir + "/KERNEL"
	InitrdFile  = ESPDir + "/INITRD"
	CmdlineFile = ESPDir + "/CMDLINE"
)

// Config/rescue-SSH file names within ESPDir - long-named, read only
// by /init (via the real kernel's own vfat driver, LFN-transparent),
// never by stage2's own minimal reader.
const (
	ConfigFile            = ESPDir + "/config"
	AuthorizedKeysFile    = ESPDir + "/authorized_keys"
	SSHHostEd25519KeyFile = ESPDir + "/ssh_host_ed25519_key"
)

// MetadataFile holds the artifact manifest internal/metadata defines -
// see that package's own doc comment for what it holds and why. A
// PLAIN ESP FILE for BOTH firmwares (not a build-time-embedded PE
// section for UEFI): unlike KERNEL/INITRD/CMDLINE, this is generated
// LOCALLY, at install/update time, by cmd/tool itself deep-inspecting
// the payload it just wrote - not baked in at build.sh time - so
// there's no PE-embedding step to mirror it into, and one file format
// serves both firmwares identically. Long-named, same category as
// ConfigFile above: read only by cmd/tool (via the real kernel's own
// vfat driver), never by stage2's own minimal reader or efi/'s own
// loader - neither has any use for it at boot time, only
// `status`/`verify` do, after the OS is already up.
const MetadataFile = ESPDir + "/metadata"

// EFIBootDir and EFILoaderName are the UEFI-standard firmware
// entrypoint location - deliberately OUTSIDE ESPDir (see this
// project's own architecture writeup: EFI/BOOT is the UEFI standard's
// own removable-media fallback path, not alpine-zfsboot's; the one
// directory this project never touches except to place its own loader
// binary there, exactly as UEFI firmware itself requires).
const EFIBootDir = "EFI/BOOT"

// EFILoaderName returns the firmware-mandated loader filename for
// arch ("x86_64" -> "BOOTX64.EFI", "aarch64" -> "BOOTAA64.EFI") -
// matches alpine-install-zfs.sh's own EFI_FALLBACK_NAME selection and
// build.sh's own OUT_FILE naming, not a fresh convention.
func EFILoaderName(arch string) string {
	switch arch {
	case "x86_64":
		return "BOOTX64.EFI"
	case "aarch64":
		return "BOOTAA64.EFI"
	default:
		return ""
	}
}
