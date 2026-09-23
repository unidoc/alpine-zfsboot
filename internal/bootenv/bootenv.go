// Package bootenv answers exactly two questions for the rest of this
// tool - "is this machine UEFI or BIOS" and "which block device is the
// canonical alpine-zfsboot FAT/ESP partition" - and does neither of
// them by writing anything. install/update/verify/status all call the
// SAME functions here rather than re-implementing discovery per
// command (see internal/layout's own doc comment on why one source of
// truth matters to this project).
//
// FindESP deliberately ports init/init's own ESP-discovery ALGORITHM
// (FAT LABEL match, then a marker-file test, refuse to guess on
// ambiguity) rather than inventing a different one - a live-system
// `alpine-zfsboot status` run needs to find the exact same partition
// /init already found at boot, or the two would disagree about what
// "the" ESP even is. The LABEL match itself is a native FAT32 BPB read
// (see fat32VolumeLabel/findFATLabelDevices below), not a shelled-out
// `blkid` call - self-containment: a boot/recovery introspection tool
// must not depend on separately-installed userspace packages to
// understand its own boot media (same reasoning that took `xz` out of
// internal/initrdinfo - see that package's own header comment).
package bootenv

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/unidoc/alpine-zfsboot/internal/layout"
	"github.com/unidoc/alpine-zfsboot/internal/parttable"
)

// IsUEFI reports whether this machine booted via UEFI firmware - the
// one signal every command uses to pick a backend (internal/uefiboot
// vs internal/biosboot+internal/espconfig), never exposed to the user
// as a flag (see this project's own CLI design: firmware is detected,
// not chosen). root is "/" for a normal live-OS run; a caller pointed
// at a not-yet-booted target (install, via --root) has no real
// /sys of its own to check - see Firmware below for how install
// itself decides instead.
func IsUEFI(root string) bool {
	_, err := os.Stat(filepath.Join(root, "sys/firmware/efi"))
	return err == nil
}

// espCandidate is one blkid-reported LABEL="EFI" device, together with
// what selectESP() found mounted on it - split out from FindESP's own
// real mount/umount loop specifically so the SELECTION rule (which
// candidate wins, or whether to refuse) is a plain function over
// already-gathered facts, testable without a real block device or
// filesystem at all.
type espCandidate struct {
	dev              string
	hasConfigMarker  bool // config, authorized_keys, or ssh_host_ed25519_key present
	hasPayloadMarker bool // EFI/BOOT/BOOTX64.EFI, BOOTAA64.EFI, or EFI/ALPINE/KERNEL present
}

func (c espCandidate) qualifies() bool {
	return c.hasConfigMarker && c.hasPayloadMarker
}

// selectESP applies init/init's own refuse-to-guess rule: exactly one
// qualifying candidate wins; zero or more than one is an error (never
// picked by enumeration order, which blkid does not promise is
// stable across boots).
func selectESP(candidates []espCandidate) (string, error) {
	var qualifying []espCandidate
	for _, c := range candidates {
		if c.qualifies() {
			qualifying = append(qualifying, c)
		}
	}
	switch len(qualifying) {
	case 0:
		return "", fmt.Errorf("no alpine-zfsboot ESP found (checked %d device(s) labeled %q)", len(candidates), layout.FATVolumeLabel)
	case 1:
		return qualifying[0].dev, nil
	default:
		devs := make([]string, len(qualifying))
		for i, c := range qualifying {
			devs[i] = c.dev
		}
		return "", fmt.Errorf("more than one alpine-zfsboot ESP found (%s) - refusing to guess", strings.Join(devs, ", "))
	}
}

// On-disk FAT32 boot-sector (BPB) field offsets, per the Microsoft FAT
// spec's own BPB32 table - the SAME, already-proven layout
// bios/fat.c's own `struct fat32_bpb` uses (that C struct's own field
// order/comments are the reference this was checked against - these
// are independently reimplemented in Go from the public spec/that
// existing, already-verified layout, not a port of the C code itself).
// fatVolumeLabelOff/fatVolumeLabelLen locate BS_VolLab (offset 0x47,
// 11 bytes, space-padded) - the boot sector's OWN copy of the volume
// label (`libblkid`'s own "LABEL_FATBOOT", distinct from - but kept in
// sync with, by every real mkfs.vfat -n invocation, including this
// project's own build.sh - "LABEL", the root-directory volume-label
// entry `libblkid` prefers when present). Reading only the BPB copy
// here, not the root-directory entry, is a deliberate, narrower
// implementation than full libblkid: correct FOR THIS PROJECT'S OWN
// WRITER specifically (mkfs.vfat -n always sets both, verified against
// a real machine's own `blkid` output showing LABEL_FATBOOT and LABEL
// matching - see the hardening ledger's own entry for this fix), not a
// general-purpose FAT label reader for arbitrary third-party-written
// filesystems.
const (
	fatBPBMinSize     = 90
	fatFSTypeOff      = 82
	fatFSTypeLen      = 8
	fatVolumeLabelOff = 71
	fatVolumeLabelLen = 11
)

// fat32VolumeLabel reads sector (a device's own first 512 bytes) as a
// FAT32 boot sector and returns its BS_VolLab field, trimmed of the
// spec's own trailing-space padding. ok is false for anything that
// doesn't look like a real FAT32 BPB (the fs_type sanity check - "per
// spec, informational" per bios/fat.c's own comment on the same check,
// but a cheap and real enough gate to skip non-FAT32 devices without
// false-matching on coincidental bytes) - never a wrong label, only
// "not FAT32" or "no label set".
func fat32VolumeLabel(sector []byte) (label string, ok bool) {
	if len(sector) < fatBPBMinSize {
		return "", false
	}
	if string(sector[fatFSTypeOff:fatFSTypeOff+fatFSTypeLen]) != "FAT32   " {
		return "", false
	}
	return strings.TrimRight(string(sector[fatVolumeLabelOff:fatVolumeLabelOff+fatVolumeLabelLen]), " "), true
}

// enumerateBlockDevices lists every block device the running kernel
// currently reports, read directly from /proc/partitions - the same
// authoritative kernel-level enumeration blkid's own probing
// ultimately walks too, needing no separately-installed binary, cache
// file, or udev database to read. Format: a header line, then one
// "major minor #blocks name" row per device (both whole disks AND
// their own partitions - deliberately not filtered to "partitions
// only", since a whole-disk-formatted FAT filesystem with no partition
// table at all is a real, supported layout this project's own
// biosboot package already documents handling).
func enumerateBlockDevices() ([]string, error) {
	data, err := os.ReadFile("/proc/partitions")
	if err != nil {
		return nil, fmt.Errorf("reading /proc/partitions: %w", err)
	}
	return parseProcPartitions(string(data)), nil
}

// parseProcPartitions is enumerateBlockDevices' own pure parsing logic
// (same split-for-testability idiom as this file's other real-I/O
// wrappers), returning "/dev/<name>" for each real device row.
func parseProcPartitions(content string) []string {
	var devs []string
	for i, line := range strings.Split(content, "\n") {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue // the header line, or a trailing blank line
		}
		fields := strings.Fields(line)
		if len(fields) != 4 {
			continue
		}
		devs = append(devs, "/dev/"+fields[3])
	}
	return devs
}

// findFATLabelDevices returns every currently-enumerable block device
// whose own FAT32 BPB reports label - the native replacement for
// `blkid`'s own LABEL scan (see this file's own struct-offset comment
// above for the narrower, deliberate scope this covers). A device that
// fails to open or doesn't hold enough bytes for a BPB is skipped, not
// fatal to the overall scan - the same tolerant, per-device probing
// blkid itself does (an empty optical drive, a loop device with
// nothing attached, a partition of some other real filesystem type -
// none of these should abort discovery of the one device that DOES
// match).
func findFATLabelDevices(label string) ([]string, error) {
	devs, err := enumerateBlockDevices()
	if err != nil {
		return nil, err
	}
	var matches []string
	for _, dev := range devs {
		f, err := os.Open(dev)
		if err != nil {
			continue
		}
		sector := make([]byte, fatBPBMinSize)
		_, err = io.ReadFull(f, sector)
		f.Close()
		if err != nil {
			continue
		}
		if l, ok := fat32VolumeLabel(sector); ok && l == label {
			matches = append(matches, dev)
		}
	}
	return matches, nil
}

// mountFieldUnescaper undoes /proc/*/mounts' own octal escaping of
// space, tab, backslash and newline within a single whitespace-
// separated field (the kernel's own fs/proc_namespace.c writes these
// exact four escapes - anything else in a real device/mountpoint path
// is passed through literally). Order matters: \134 (backslash) must
// be unescaped LAST, or an already-unescaped \040 sitting next to a
// literal backslash in the source could be re-interpreted.
var mountFieldEscapes = []struct {
	from string
	to   string
}{
	{`\040`, " "},
	{`\011`, "\t"},
	{`\012`, "\n"},
	{`\134`, `\`},
}

func unescapeMountField(s string) string {
	for _, e := range mountFieldEscapes {
		s = strings.ReplaceAll(s, e.from, e.to)
	}
	return s
}

// parseExistingMount is findExistingMount's own pure parsing logic,
// split out (same idiom as parseBlkidLabelEFI vs. FindESP's own real
// `blkid` exec above) so it has its own test independent of a real
// /proc/self/mounts. resolveSymlink is injected for the same reason -
// a real filepath.EvalSymlinks call depends on the actual filesystem
// having those devices/symlinks, which a unit test fixture doesn't.
func parseExistingMount(mountsContent, dev string, resolveSymlink func(string) (string, error)) (mountpoint string, rw bool, ok bool) {
	devReal, devRealErr := resolveSymlink(dev)
	for _, line := range strings.Split(mountsContent, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		mdev := unescapeMountField(fields[0])
		match := mdev == dev
		if !match && devRealErr == nil {
			if mdevReal, err := resolveSymlink(mdev); err == nil && mdevReal == devReal {
				match = true
			}
		}
		if !match {
			continue
		}
		opts := strings.Split(fields[3], ",")
		rw := len(opts) > 0 && opts[0] == "rw"
		return unescapeMountField(fields[1]), rw, true
	}
	return "", false, false
}

// findExistingMount reports whether dev is already mounted somewhere
// the running kernel knows about (/proc/self/mounts - always current,
// unlike /etc/mtab which can be stale or symlinked away on a musl/
// busybox system), and if so, at what mountpoint and with what
// read/write access. Matches both the literal device string /proc/
// self/mounts reports AND, since that string is whatever path was
// literally passed to mount(2) at mount time (which may itself be a
// symlink - e.g. /dev/disk/by-label/EFI - not necessarily what blkid's
// own dev argument here looks like), the resolved real path of each,
// via filepath.EvalSymlinks. A resolution failure (dev doesn't exist,
// or isn't a symlink) falls back to the literal comparison alone
// rather than treating it as fatal - this is a best-effort lookup, not
// something that should itself block ESP discovery/mounting.
func findExistingMount(dev string) (mountpoint string, rw bool, ok bool) {
	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return "", false, false
	}
	return parseExistingMount(string(data), dev, filepath.EvalSymlinks)
}

// parseMountAtPath is FindMountAtPath's own pure parsing logic (same
// split-for-testability idiom as parseExistingMount above) - the
// REVERSE lookup direction: given a mountpoint PATH, what device and
// fstype is actually mounted there right now. Scans every line rather
// than stopping at the first match: /proc/self/mounts lists mounts in
// chronological order, and a path can legitimately be mounted over
// more than once in a process's lifetime (mount, unmount, mount
// something else there) - only the LAST entry for a given mountpoint
// is what's actually visible through that path right now.
func parseMountAtPath(mountsContent, mountpoint string) (dev, fstype string, ok bool) {
	for _, line := range strings.Split(mountsContent, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if unescapeMountField(fields[1]) == mountpoint {
			dev, fstype, ok = unescapeMountField(fields[0]), fields[2], true
		}
	}
	return dev, fstype, ok
}

// FindMountAtPath reports what device and fstype are mounted at
// mountpoint right now, per /proc/self/mounts - the reverse of
// findExistingMount's own device -> mountpoint direction.
func FindMountAtPath(mountpoint string) (dev, fstype string, ok bool) {
	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return "", "", false
	}
	return parseMountAtPath(string(data), mountpoint)
}

// VerifyESPMounted is install's own preflight (F17, unidoc-alip's PR
// #5 review): confirms mountpoint (--root/boot/efi) is a genuinely
// mounted vfat filesystem before a single byte gets written there.
// Without this, three real failure modes install could not previously
// distinguish from success:
//   - --root/boot/efi was never actually mounted at all (a caller
//     forgot the mount step, or it silently failed earlier) - the
//     payload write would land inside the plain TARGET ROOTFS
//     directory tree instead, and espconfig.VerifyPayload's own later
//     byte-for-byte check would just compare that write against
//     itself, reporting "verified byte-for-byte" with no real ESP
//     behind it at all.
//   - --root/boot/efi is mounted, but as something other than vfat (a
//     stale bind mount, an empty tmpfs placeholder) - same silent-
//     success risk.
//   - BIOS only: the mounted ESP's own parent disk does not match the
//     <disk> argument install was given - stage1/stage2 land on disk
//     A, the FAT payload on disk B's ESP, two disks now holding half
//     of one inconsistent boot chain.
//
// diskArg is the disk BIOS install is about to write stage1/stage2
// onto - pass "" for UEFI (which owns no separate disk argument of
// its own - install already never uses one there; that mismatch has
// no corresponding check to add here, since there is no second disk
// argument to compare against in the first place).
func VerifyESPMounted(mountpoint, diskArg string) error {
	dev, fstype, ok := FindMountAtPath(mountpoint)
	return checkMountInfo(mountpoint, dev, fstype, ok, diskArg, resolveSymlinkOrSelf)
}

// checkMountInfo is VerifyESPMounted's own pure decision logic, split
// out (same idiom as parseExistingMount vs. findExistingMount above)
// so it has its own test independent of a real /proc/self/mounts or
// real symlinked device paths - resolveSymlink is injected for the
// same reason.
func checkMountInfo(mountpoint, dev, fstype string, found bool, diskArg string, resolveSymlink func(string) string) error {
	if !found {
		return fmt.Errorf("%s is not a mounted filesystem at all - refusing to write the boot payload into a plain directory (mount the FAT/ESP filesystem there first)", mountpoint)
	}
	if fstype != "vfat" {
		return fmt.Errorf("%s is mounted, but as %q, not vfat - refusing to write the boot payload onto the wrong filesystem", mountpoint, fstype)
	}
	if diskArg == "" {
		return nil
	}
	gotDisk := resolveSymlink(DevicePartitionBase(dev))
	wantDisk := resolveSymlink(diskArg)
	if gotDisk != wantDisk {
		return fmt.Errorf("%s's own filesystem (%s, on disk %s) does not match the disk argument (%s) - stage1/stage2 and the boot payload would land on two different disks", mountpoint, dev, gotDisk, wantDisk)
	}
	return nil
}

// resolveSymlinkOrSelf resolves p via filepath.EvalSymlinks, falling
// back to p itself on any error (p doesn't exist, isn't a symlink,
// or - in a test fixture - simply isn't a real path at all) - same
// best-effort-resolution idiom parseExistingMount's own resolveSymlink
// parameter already uses.
func resolveSymlinkOrSelf(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

// MountESP mounts dev at a private, temporary mountpoint (same
// pattern as init/init's own /tmp/esp-config and menu.py's
// /tmp/esp-console-pref - never assumes the target OS's own fstab
// already has it mounted somewhere, since a rescue-initramfs run of
// this same code later will have no fstab at all) and returns that
// mountpoint plus a cleanup func the caller must call (unmounts and
// removes the temporary directory) once done. This is the one real
// mount primitive every command that needs to read or write actual
// FAT file content (internal/espconfig, and this package's own
// probeESPCandidate below) goes through - install is the one
// exception, since it receives an already-mounted target from the
// installer (see cmd/tool's own --root handling).
//
// Checks findExistingMount FIRST, and reuses that mountpoint verbatim
// (cleanup becomes a no-op) rather than always mounting a second,
// independent copy of dev at a fresh temp dir - a real bug, not a
// hypothetical one: found live on a real Hetzner CAX machine, where
// dev was already mounted read-write at /boot/efi by the running
// system's own fstab (exactly the common case every live-system
// status/verify/update call is FOR, not an edge case), and this
// function's own unconditional `mount` call failed outright with
// "Resource busy" - busybox's mount (this project's own target,
// Alpine/musl) refuses to mount an already-mounted block device a
// second time, unlike some util-linux configurations. That failure
// then propagated up through probeESPCandidate's own error-discarding
// `continue` (see FindESP's own comment) as a misleading "checked 0
// device(s)" - the device WAS found by label, the mount attempt on it
// silently failed. Reusing an existing mount isn't just a fix for that
// symptom, it's also more correct on its own merits: mounting the
// same non-network filesystem twice independently gives two separate
// superblock instances over one block device with no cross-mount
// cache coherency guarantee, a real (if rare) data-consistency risk
// this avoids entirely by construction. The reused mountpoint is never
// unmounted by this function's own cleanup - it wasn't this function
// that mounted it, and unmounting a live system's own /boot/efi out
// from under it would be a real, serious regression, not a cleanup nicety.
func MountESP(dev string, readonly bool) (mountpoint string, cleanup func(), err error) {
	if mp, rw, ok := findExistingMount(dev); ok {
		if readonly || rw {
			return mp, func() {}, nil
		}
		return "", nil, fmt.Errorf("%s is already mounted read-only at %s, but this operation needs write access - remount it read-write first (mount -o remount,rw %s)", dev, mp, mp)
	}

	mountpoint, err = os.MkdirTemp("", "alpine-zfsboot-esp-*")
	if err != nil {
		return "", nil, fmt.Errorf("creating a mountpoint for %s: %w", dev, err)
	}
	cleanup = func() {
		// A full source audit found this used to always os.Remove the
		// mountpoint regardless of whether umount actually succeeded -
		// on Linux, rmdir on a directory that's still an active mount
		// point fails with EBUSY (so the practical effect was usually
		// "both calls silently fail, both the mount AND the temp
		// directory leak"), but the real hazard is the SILENT part: a
		// caller had no way to know its own umount failed at all. Now
		// reported (not returned - cleanup's own signature is a plain
		// func(), unconditionally deferred by every real caller, so
		// there's no good place to surface an error to; stderr, same as
		// die()'s own diagnostics, is at least visible) and os.Remove is
		// only attempted once umount genuinely succeeded - an orphaned
		// mount left BEHIND is recoverable (a later real `umount` still
		// works); a directory silently removed out from under a mount
		// that's still live is a strictly worse, harder-to-diagnose state.
		//
		// unix.Unmount(mountpoint, 0) is the raw umount(2) syscall - no
		// flags, same as a bare `umount mountpoint` with no options.
		// This project is Linux-only throughout (real-mode boot tooling,
		// Alpine's own musl userspace) - there is no cross-platform
		// concern to abstract away here, only a reason to stop depending
		// on a separately-installed `umount` binary being on PATH (see
		// this package's own self-containment rationale, same as
		// MountESP's own mount(2) call just below).
		if err := unix.Unmount(mountpoint, 0); err != nil {
			fmt.Fprintf(os.Stderr, "alpine-zfsboot: WARNING: unmounting %s failed: %v - it remains mounted; a later run may see it as a second, independent mount of the same device\n", mountpoint, err)
			return
		}
		os.Remove(mountpoint)
	}

	// unix.Mount is the raw mount(2) syscall - source, target, fstype,
	// flags, data - exactly what a bare `mount -t vfat [-o ro] dev
	// mountpoint` (the shelled-out command this replaces) issued
	// underneath anyway. A boot/recovery introspection tool must not
	// depend on a separately-installed `mount` binary being present on
	// PATH to mount its own ESP - the same reasoning that took `xz` out
	// of internal/initrdinfo applies here (see that package's own
	// header comment) with no dropbear-key-generation-shaped complexity
	// this time: mount(2) is a stable, direct kernel interface, not a
	// third-party wire protocol to reverse-engineer.
	var flags uintptr
	if readonly {
		flags = unix.MS_RDONLY
	}
	if err := unix.Mount(dev, mountpoint, "vfat", flags, ""); err != nil {
		os.Remove(mountpoint)
		return "", nil, fmt.Errorf("mounting %s: %w", dev, err)
	}
	return mountpoint, cleanup, nil
}

// probeESPCandidate mounts dev read-only (via MountESP above) and
// checks for the same marker files init/init's own ESP-discovery
// block requires.
func probeESPCandidate(dev string) (espCandidate, error) {
	c := espCandidate{dev: dev}

	mountpoint, cleanup, err := MountESP(dev, true)
	if err != nil {
		return c, err
	}
	defer cleanup()

	for _, marker := range []string{layout.ConfigFile, layout.AuthorizedKeysFile, layout.SSHHostEd25519KeyFile} {
		if _, err := os.Stat(filepath.Join(mountpoint, marker)); err == nil {
			c.hasConfigMarker = true
			break
		}
	}
	for _, marker := range []string{
		filepath.Join(layout.EFIBootDir, "BOOTX64.EFI"),
		filepath.Join(layout.EFIBootDir, "BOOTAA64.EFI"),
		layout.KernelFile,
	} {
		if _, err := os.Stat(filepath.Join(mountpoint, marker)); err == nil {
			c.hasPayloadMarker = true
			break
		}
	}
	return c, nil
}

// FindESP discovers the canonical alpine-zfsboot FAT/ESP device on a
// live, booted system - the real-I/O counterpart of selectESP() above.
// Device discovery is always against the running kernel's own view of
// block devices (blkid), never root-relative - a not-yet-mounted
// install target has no block devices of its own to distinguish from
// the host's this way, which is exactly why `install` takes its disk
// explicitly instead of calling this (see internal/layout's own
// doc comment on that boundary).
func FindESP() (string, error) {
	devs, err := findFATLabelDevices(layout.FATVolumeLabel)
	if err != nil {
		return "", err
	}
	if len(devs) == 0 {
		return "", fmt.Errorf("no block device labeled %q found", layout.FATVolumeLabel)
	}

	candidates := make([]espCandidate, 0, len(devs))
	for _, dev := range devs {
		c, err := probeESPCandidate(dev)
		if err != nil {
			// A device that fails to mount/probe is not a fatal
			// error for the whole search - e.g. a stale/foreign
			// EFI-labeled partition from unrelated media. Skip it,
			// same as init/init's own `|| continue`.
			continue
		}
		candidates = append(candidates, c)
	}
	return selectESP(candidates)
}

// DiskLayout is GPT or msdos partitioning, in alpine-installer's own
// terms - only matters to the BIOS backend (internal/biosboot), and
// only for `install`, which needs to know whether stage2 lives on its
// own numbered partition (both layouts, per alpine-install-zfs.sh's
// own partition_disk() - see internal/layout's own comment) or must
// be addressed by a raw whole-disk LBA offset instead.
type DiskLayout int

const (
	LayoutUnknown DiskLayout = iota
	LayoutGPT
	LayoutMSDOS
)

func (l DiskLayout) String() string {
	switch l {
	case LayoutGPT:
		return "gpt"
	case LayoutMSDOS:
		return "msdos"
	default:
		return "unknown"
	}
}

// gptSignature is the exact 8-byte magic a real GPT header starts
// with, at LBA 1 (byte offset layout.SectorSize) of the disk -
// bios/gpt.h's own GPT_SIGNATURE, the SAME check bios/gpt.c performs
// at boot time (gpt_read(), see that file). Reusing the identical,
// already-proven detection primitive here instead of parsing a
// human-readable sgdisk/sfdisk text report, which is fragile across
// tool versions/locales and requires an external binary this package
// would otherwise have no other reason to depend on.
var gptSignature = [8]byte{'E', 'F', 'I', ' ', 'P', 'A', 'R', 'T'}

// parseDiskLayout decides GPT vs msdos from the raw bytes at LBA 1 -
// split out from DetectDiskLayout so the decision logic has a test
// independent of a real disk or device file.
func parseDiskLayout(lba1 []byte) DiskLayout {
	if len(lba1) >= len(gptSignature) && [8]byte(lba1[:8]) == gptSignature {
		return LayoutGPT
	}
	return LayoutMSDOS
}

// DetectDiskLayout reports which partitioning scheme disk uses, by
// reading its own LBA 1 directly (see parseDiskLayout/gptSignature
// above) - no sgdisk/sfdisk dependency, no parsing of anyone's
// human-readable report.
func DetectDiskLayout(disk string) (DiskLayout, error) {
	f, err := os.Open(disk)
	if err != nil {
		return LayoutUnknown, fmt.Errorf("opening %s: %w", disk, err)
	}
	defer f.Close()

	buf := make([]byte, 8)
	if _, err := f.ReadAt(buf, layout.SectorSize); err != nil {
		return LayoutUnknown, fmt.Errorf("reading LBA 1 of %s: %w", disk, err)
	}
	return parseDiskLayout(buf), nil
}

// devicePartitionSuffix matches a trailing partition number on a
// Linux block device path - either the plain "sda1"-style suffix
// (letters then digits) or the "pN" form nvme/mmcblk/loop devices use
// because their own base name already ends in a digit (nvme0n1p1,
// mmcblk0p1, loop0p1) - checked first, since it's the more specific
// pattern.
var (
	partitionSuffixP     = regexp.MustCompile(`^(.*\d)p(\d+)$`)
	partitionSuffixPlain = regexp.MustCompile(`^(.*[a-zA-Z])(\d+)$`)
	// nvmeOrMMCWholeDisk matches an nvme namespace or mmcblk WHOLE-DISK
	// name that itself ends in a digit (nvme0n1, mmcblk0) - see
	// DevicePartitionBase's own comment on the real, confirmed bug
	// this guards: partitionSuffixPlain's "any trailing digit is a
	// partition number" heuristic is correct for sda/vda-style names
	// (whole disk never ends in a digit there), but WRONG for these
	// two device classes, whose own whole-disk name already ends in a
	// digit by convention - a real partition of either always adds an
	// explicit "p<N>" (matched by partitionSuffixP above, tried
	// first), never a bare digit.
	nvmeOrMMCWholeDisk = regexp.MustCompile(`(nvme\d+n\d+|mmcblk\d+)$`)
)

// DevicePartitionBase returns the whole-disk device path for a
// partition device path (e.g. "/dev/sda1" -> "/dev/sda",
// "/dev/nvme0n1p1" -> "/dev/nvme0n1") - stage1/stage2's own fixed-LBA
// placement (see internal/layout) is always addressed against the
// WHOLE disk device, never a partition device node (a partition
// device's own LBA 0 is already offset from the disk's - writing "at
// LBA 34" relative to a partition starting at disk LBA 34 would land
// at disk LBA 68, not 34). Given as a plain string transform, not
// something bootenv queries the kernel for, since a partition's own
// device NAME already encodes this deterministically on Linux; no
// case here should ever fail for a real blkid-reported device path.
//
// A real gap a follow-up review found (F22, unidoc-alip's PR #5
// review): called on an ALREADY-whole-disk nvme/mmcblk name (no "p<N>"
// partition suffix at all - "/dev/nvme0n1", "/dev/mmcblk0"),
// partitionSuffixPlain's own generic "trailing digit = partition
// number" fallback used to match anyway (nvme0n1 -> nvme0n,
// mmcblk0 -> mmcblk) - wrong, and silently so, since nothing about
// that shape signals an error on its own. nvmeOrMMCWholeDisk, checked
// BEFORE falling through to that generic heuristic, recognizes this
// specific shape and returns it unchanged instead.
func DevicePartitionBase(part string) string {
	if m := partitionSuffixP.FindStringSubmatch(part); m != nil {
		return m[1]
	}
	if nvmeOrMMCWholeDisk.MatchString(part) {
		return part
	}
	if m := partitionSuffixPlain.FindStringSubmatch(part); m != nil {
		return m[1]
	}
	return part
}

// CheckStage2ExtentFree is the fail-closed SAFETY gate every BIOS
// write to the stage2 extent (internal/biosboot's own WriteStage2,
// called from both install and update) must pass before writing a
// single byte - deliberately separate from OWNERSHIP (see this
// package's own doc comment: "is this alpine-zfsboot's disk" is
// established by finding the canonical FAT/ESP partition and deriving
// its parent disk - FindESP/DevicePartitionBase - not by anything
// checked here). What THIS checks is narrower and purely structural:
// does anything currently on disk's own partition table actually
// occupy LBA [layout.Stage2LBA, layout.Stage2LBA+layout.Stage2Sectors)
// - a real partition, or (GPT only) the partition array's own on-disk
// footprint?
//
// An earlier version of this function required a specially-named GPT
// partition / specially-typed MBR partition as an OWNERSHIP marker -
// removed: that was never part of the actual established on-disk ABI
// (stage1.S itself never looks stage2 up via GPT/MBR at all - fixed
// LBA, full stop, see bios/mbr.h's own comment), and a real,
// already-migrated production system (this project's own uniclus-01)
// has NO partition-table entry covering this extent at all - the
// stage2 region was written directly into the reserved LBA range on
// the whole-disk device, exactly as this project's own
// update-stage2-only.sh already does. Requiring an invented marker
// would have made that real, correctly-functioning installation fail
// this check. Ownership for install (a disk with no FAT/ESP content
// yet, so FindESP-based discovery cannot apply) instead rests on the
// caller providing an explicit disk argument plus interactive
// confirmation (see cmd/tool's own install command) - this function's
// job is purely "prove the bytes there are safe to overwrite", not
// "prove whose disk this is".
//
// A partition (GPT or MBR) whose own range EXACTLY equals
// [layout.Stage2LBA, layout.Stage2LBA+layout.Stage2Sectors) is NOT
// treated as a conflict - alpine-install-zfs.sh's own partition_disk()
// still creates exactly such a cosmetic entry on a fresh GPT/msdos
// install (harmless, not read by stage1/stage2, but also not a
// competing claim on different data - it describes the same extent
// this function is confirming is safe, not a foreign one). Any OTHER
// overlap - a real partition only partially overlapping, or a
// different partition entirely covering part of the range - fails
// closed.
func CheckStage2ExtentFree(disk string) error {
	extentStart := uint64(layout.Stage2LBA)
	extentEnd := extentStart + uint64(layout.Stage2Sectors) - 1

	hdr, err := parttable.ReadGPTHeader(disk)
	if err == nil {
		return checkGPTExtentFree(disk, hdr, extentStart, extentEnd)
	}
	if err != parttable.ErrNoGPTSignature {
		return fmt.Errorf("reading GPT header of %s: %w", disk, err)
	}

	entries, err := parttable.MBRPartitions(disk)
	if err != nil {
		return fmt.Errorf("%s has neither a valid GPT nor a valid MBR partition table - refusing to write: %w", disk, err)
	}
	for _, e := range entries {
		pStart := uint64(e.StartLBA)
		pEnd := pStart + uint64(e.SectorCount) - 1
		if pStart == extentStart && pEnd == extentEnd {
			continue // our own cosmetic reservation - not a conflict, see this function's own doc comment
		}
		if lbaRangesOverlap(extentStart, extentEnd, pStart, pEnd) {
			return fmt.Errorf(
				"%s: MBR partition (type 0x%02x, LBA %d-%d) overlaps the stage2 extent (LBA %d-%d) - refusing to write",
				disk, e.Type, pStart, pEnd, extentStart, extentEnd)
		}
	}
	return nil
}

func checkGPTExtentFree(disk string, hdr *parttable.GPTHeader, extentStart, extentEnd uint64) error {
	// The partition array's own real on-disk footprint - a
	// non-standard GPT with a larger-than-usual array could
	// structurally extend into our extent even with zero actual
	// partitions defined; checked independently of the entries loop
	// below; entries with no room to exist should not be missed.
	if hdr.PartitionEntrySize == 0 {
		return fmt.Errorf("%s: GPT partition_entry_size is 0", disk)
	}
	entriesPerSector := layout.SectorSize / int(hdr.PartitionEntrySize)
	if entriesPerSector < 1 {
		entriesPerSector = 1
	}
	arraySectors := (uint64(hdr.NumPartitionEntries) + uint64(entriesPerSector) - 1) / uint64(entriesPerSector)
	arrayStart := hdr.PartitionEntryLBA
	arrayEnd := arrayStart + arraySectors - 1
	if arraySectors > 0 && lbaRangesOverlap(extentStart, extentEnd, arrayStart, arrayEnd) {
		return fmt.Errorf(
			"%s: the GPT partition array itself (LBA %d-%d) overlaps the stage2 extent (LBA %d-%d) - refusing to write",
			disk, arrayStart, arrayEnd, extentStart, extentEnd)
	}

	entries, err := parttable.GPTPartitions(disk, hdr)
	if err != nil {
		return fmt.Errorf("reading GPT partition array of %s: %w", disk, err)
	}
	for _, e := range entries {
		if e.StartLBA == extentStart && e.EndLBA == extentEnd {
			continue // our own cosmetic reservation - not a conflict, see CheckStage2ExtentFree's own doc comment
		}
		if lbaRangesOverlap(extentStart, extentEnd, e.StartLBA, e.EndLBA) {
			return fmt.Errorf(
				"%s: GPT partition %q (LBA %d-%d) overlaps the stage2 extent (LBA %d-%d) - refusing to write",
				disk, e.NameString(), e.StartLBA, e.EndLBA, extentStart, extentEnd)
		}
	}
	return nil
}

func lbaRangesOverlap(aStart, aEnd, bStart, bEnd uint64) bool {
	return aStart <= bEnd && bStart <= aEnd
}
