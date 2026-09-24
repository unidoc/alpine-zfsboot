package bootenv

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/unidoc/alpine-zfsboot/internal/layout"
)

func putU16(b []byte, v uint16) { binary.LittleEndian.PutUint16(b, v) }
func putU32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }
func putU64(b []byte, v uint64) { binary.LittleEndian.PutUint64(b, v) }
func crc32IEEE(b []byte) uint32 { return crc32.ChecksumIEEE(b) }

// buildFakeFAT32BPB hand-assembles a minimal, byte-correct 90-byte
// FAT32 boot sector - fs_type "FAT32   " at the real offset (82) and
// volume_label at the real offset (71), matching bios/fat.c's own
// already-proven `struct fat32_bpb` layout (see this package's own
// struct-offset comment) - a from-scratch fixture, not a round-trip
// through this package's own reader.
func buildFakeFAT32BPB(t *testing.T, label string) []byte {
	t.Helper()
	if len(label) > fatVolumeLabelLen {
		t.Fatalf("test fixture label %q longer than %d bytes", label, fatVolumeLabelLen)
	}
	sector := make([]byte, fatBPBMinSize)
	for i := range sector {
		sector[i] = ' ' // space-fill, matching real BPB padding conventions
	}
	copy(sector[fatVolumeLabelOff:], label)
	copy(sector[fatFSTypeOff:], "FAT32   ")
	return sector
}

func TestFat32VolumeLabel(t *testing.T) {
	sector := buildFakeFAT32BPB(t, "EFI")
	label, ok := fat32VolumeLabel(sector)
	if !ok {
		t.Fatal("fat32VolumeLabel: want ok=true for a real FAT32 BPB")
	}
	if label != "EFI" {
		t.Errorf("label = %q, want %q", label, "EFI")
	}
}

func TestFat32VolumeLabel_NotFAT32(t *testing.T) {
	sector := buildFakeFAT32BPB(t, "EFI")
	copy(sector[fatFSTypeOff:], "FAT16   ") // a real, different, valid fs_type - just not FAT32
	if _, ok := fat32VolumeLabel(sector); ok {
		t.Error("fat32VolumeLabel: want ok=false for a non-FAT32 fs_type field")
	}
}

func TestFat32VolumeLabel_TooShort(t *testing.T) {
	if _, ok := fat32VolumeLabel(make([]byte, 32)); ok {
		t.Error("fat32VolumeLabel: want ok=false for data shorter than a real BPB")
	}
}

// TestFat32VolumeLabel_PARTLABELDoesNotCount is this new native
// implementation's own version of a real bug a full source audit
// found in the OLD blkid-text-parsing implementation this replaces:
// `LABEL="EFI"` was a literal substring of `PARTLABEL="EFI"`, so a
// naive match conflated a GPT partition merely NAMED "EFI"
// (`sgdisk -c 1:EFI`, unrelated to its actual filesystem) with a real
// FAT volume LABEL. Structurally impossible to reintroduce in the
// current implementation - fat32VolumeLabel only ever reads BS_VolLab
// out of a FAT32 BPB, and a GPT partition's own NAME lives in a
// completely different on-disk structure (the GPT partition entry,
// internal/parttable's own concern) this function never touches at
// all - but proven here directly anyway: a BPB whose OWN volume_label
// field holds something other than "EFI" is correctly rejected
// regardless of what any GPT partition name elsewhere on the disk
// might claim (this function has no way to even see that).
func TestFat32VolumeLabel_PARTLABELDoesNotCount(t *testing.T) {
	sector := buildFakeFAT32BPB(t, "notEFI")
	label, ok := fat32VolumeLabel(sector)
	if !ok {
		t.Fatal("fat32VolumeLabel: want ok=true for a real FAT32 BPB")
	}
	if label == "EFI" {
		t.Errorf("label = %q - must not equal EFI merely because some OTHER field elsewhere (e.g. a GPT PARTLABEL) might", label)
	}
}

func TestParseProcPartitions(t *testing.T) {
	content := `major minor  #blocks  name

   8        0  209715200 sda
   8        1     512000 sda1
  11        0     500736 sr0
`
	got := parseProcPartitions(content)
	want := []string{"/dev/sda", "/dev/sda1", "/dev/sr0"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseProcPartitions_EmptyInput(t *testing.T) {
	if got := parseProcPartitions("major minor  #blocks  name\n"); len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
}

func TestUnescapeMountField(t *testing.T) {
	cases := []struct{ in, want string }{
		{`/boot/efi`, `/boot/efi`},
		{`/mnt/My\040Volume`, `/mnt/My Volume`},
		{`/mnt/a\011b`, "/mnt/a\tb"},
		{`/mnt/a\012b`, "/mnt/a\nb"},
		{`/mnt/back\134slash`, `/mnt/back\slash`},
	}
	for _, c := range cases {
		if got := unescapeMountField(c.in); got != c.want {
			t.Errorf("unescapeMountField(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// fakeResolveSymlink is parseExistingMount's own injected symlink
// resolver for tests - a plain map lookup (missing entry = "no such
// symlink", same shape filepath.EvalSymlinks reports for a path that
// simply doesn't resolve to anything different) rather than touching
// the real filesystem.
func fakeResolveSymlink(m map[string]string) func(string) (string, error) {
	return func(p string) (string, error) {
		if r, ok := m[p]; ok {
			return r, nil
		}
		return "", os.ErrNotExist
	}
}

// TestParseExistingMount_RealCAXCase reproduces the exact real-hardware
// bug this whole fix exists for: a real Hetzner CAX machine's own
// /proc/self/mounts line for its already-mounted, fstab-owned ESP -
// `alpine-zfsboot status` on that machine used to fail its own ESP
// discovery entirely ("checked 0 device(s) labeled \"EFI\"") because
// MountESP tried to mount /dev/sdb1 a SECOND time and busybox's mount
// refused with "Resource busy" - a real, live failure, not a
// hypothetical one. This must find the existing mount instead of
// needing to mount anything at all.
func TestParseExistingMount_RealCAXCase(t *testing.T) {
	mounts := `/dev/sdb1 /boot/efi vfat rw,noatime,fmask=0022,dmask=0022,codepage=437,iocharset=utf8,shortname=mixed,errors=remount-ro 0 0
zroot/ROOT/alpine / zfs rw,relatime,xattr,posixacl,casesensitive 0 0
`
	mp, rw, ok := parseExistingMount(mounts, "/dev/sdb1", fakeResolveSymlink(nil))
	if !ok {
		t.Fatal("expected /dev/sdb1 to be found already mounted")
	}
	if mp != "/boot/efi" {
		t.Errorf("mountpoint = %q, want /boot/efi", mp)
	}
	if !rw {
		t.Error("expected rw=true - the real line's own opts field starts with \"rw\"")
	}
}

func TestParseExistingMount_ReadOnly(t *testing.T) {
	mounts := `/dev/sdb1 /boot/efi vfat ro,noatime 0 0
`
	_, rw, ok := parseExistingMount(mounts, "/dev/sdb1", fakeResolveSymlink(nil))
	if !ok {
		t.Fatal("expected /dev/sdb1 to be found")
	}
	if rw {
		t.Error("expected rw=false for a ro-mounted device")
	}
}

func TestParseExistingMount_NotMounted(t *testing.T) {
	mounts := `/dev/sda1 / ext4 rw,relatime 0 0
`
	_, _, ok := parseExistingMount(mounts, "/dev/sdb1", fakeResolveSymlink(nil))
	if ok {
		t.Fatal("expected /dev/sdb1 to be reported as not mounted")
	}
}

// TestParseExistingMount_EscapedMountpoint proves a mountpoint
// containing a literal space (rare, but real - a hand-mounted rescue
// path, or a label-derived directory) is correctly unescaped, not
// left with a stray "\040" or, worse, mis-split on the embedded space
// as if it were a field boundary.
func TestParseExistingMount_EscapedMountpoint(t *testing.T) {
	mounts := `/dev/sdb1 /mnt/My\040ESP vfat rw,noatime 0 0
`
	mp, _, ok := parseExistingMount(mounts, "/dev/sdb1", fakeResolveSymlink(nil))
	if !ok {
		t.Fatal("expected /dev/sdb1 to be found")
	}
	if mp != "/mnt/My ESP" {
		t.Errorf("mountpoint = %q, want \"/mnt/My ESP\"", mp)
	}
}

// TestParseExistingMount_SymlinkForm proves dev discovery still works
// when /proc/self/mounts reports the device under a DIFFERENT but
// equivalent path than the literal one being searched for (e.g. blkid
// reporting /dev/sdb1 while the system was actually mounted via
// /dev/disk/by-label/EFI, a real, common alternative) - matched only
// through the injected resolver, proving the literal-string fallback
// alone would have missed this case.
func TestParseExistingMount_SymlinkForm(t *testing.T) {
	mounts := `/dev/disk/by-label/EFI /boot/efi vfat rw,noatime 0 0
`
	resolve := fakeResolveSymlink(map[string]string{
		"/dev/sdb1":              "/dev/sdb1",
		"/dev/disk/by-label/EFI": "/dev/sdb1",
	})
	mp, rw, ok := parseExistingMount(mounts, "/dev/sdb1", resolve)
	if !ok {
		t.Fatal("expected /dev/sdb1 to be found via its symlink alias /dev/disk/by-label/EFI")
	}
	if mp != "/boot/efi" || !rw {
		t.Errorf("mountpoint=%q rw=%v, want /boot/efi true", mp, rw)
	}
}

// --- VerifyESPMounted / parseMountAtPath / checkMountInfo (F17, unidoc-alip's PR #5 review) ---

func TestParseMountAtPath(t *testing.T) {
	mounts := `/dev/sdb1 /mnt/alpine/boot/efi vfat rw,noatime 0 0
zroot/ROOT/alpine / zfs rw,relatime 0 0
`
	dev, fstype, ok := parseMountAtPath(mounts, "/mnt/alpine/boot/efi")
	if !ok || dev != "/dev/sdb1" || fstype != "vfat" {
		t.Errorf("dev=%q fstype=%q ok=%v, want /dev/sdb1 vfat true", dev, fstype, ok)
	}
}

func TestParseMountAtPath_NotMounted(t *testing.T) {
	mounts := `/dev/sda1 / ext4 rw,relatime 0 0
`
	_, _, ok := parseMountAtPath(mounts, "/mnt/alpine/boot/efi")
	if ok {
		t.Fatal("expected /mnt/alpine/boot/efi to be reported as not mounted")
	}
}

// TestParseMountAtPath_LaterMountWins proves a path mounted over more
// than once in the same mount table (a real, if unusual, sequence -
// mount, unmount, mount something else there) resolves to the LAST
// entry, matching what's actually visible through that path right
// now - not the first, stale one.
func TestParseMountAtPath_LaterMountWins(t *testing.T) {
	mounts := `/dev/sdb1 /mnt/alpine/boot/efi ext4 rw 0 0
/dev/sdc1 /mnt/alpine/boot/efi vfat rw 0 0
`
	dev, fstype, ok := parseMountAtPath(mounts, "/mnt/alpine/boot/efi")
	if !ok || dev != "/dev/sdc1" || fstype != "vfat" {
		t.Errorf("dev=%q fstype=%q ok=%v, want the LATER entry /dev/sdc1 vfat true", dev, fstype, ok)
	}
}

func identityResolve(p string) string { return p }

func TestCheckMountInfo_NotMounted(t *testing.T) {
	err := checkMountInfo("/mnt/alpine/boot/efi", "", "", false, "", identityResolve)
	if err == nil {
		t.Fatal("want an error when nothing is mounted at the ESP path, got nil")
	}
}

func TestCheckMountInfo_WrongFilesystem(t *testing.T) {
	err := checkMountInfo("/mnt/alpine/boot/efi", "/dev/sdb1", "ext4", true, "", identityResolve)
	if err == nil {
		t.Fatal("want an error for a mounted-but-not-vfat filesystem, got nil")
	}
}

func TestCheckMountInfo_UEFI_NoDiskArgNeeded(t *testing.T) {
	// diskArg="" (UEFI's own call shape - it owns no separate disk
	// argument) - a valid vfat mount must pass with no disk comparison
	// attempted at all.
	if err := checkMountInfo("/mnt/alpine/boot/efi", "/dev/sdb1", "vfat", true, "", identityResolve); err != nil {
		t.Errorf("want nil for a valid vfat mount with no disk to compare against, got: %v", err)
	}
}

func TestCheckMountInfo_BIOS_DiskMatches(t *testing.T) {
	resolve := func(p string) string {
		if p == "/dev/sda" {
			return "/dev/sda" // DevicePartitionBase("/dev/sda1") -> "/dev/sda" already
		}
		return p
	}
	if err := checkMountInfo("/mnt/alpine/boot/efi", "/dev/sda1", "vfat", true, "/dev/sda", resolve); err != nil {
		t.Errorf("want nil when the ESP's own parent disk matches diskArg, got: %v", err)
	}
}

// TestCheckMountInfo_BIOS_DiskMismatch is the direct regression test
// for F17's own core scenario: stage1/stage2 are about to be written
// to <disk>, but the mounted ESP is actually on a DIFFERENT disk -
// must be refused, not silently written to two different disks.
func TestCheckMountInfo_BIOS_DiskMismatch(t *testing.T) {
	err := checkMountInfo("/mnt/alpine/boot/efi", "/dev/sdb1", "vfat", true, "/dev/sda", identityResolve)
	if err == nil {
		t.Fatal("want an error when the mounted ESP's own disk (sdb) does not match the disk argument (sda), got nil")
	}
}

// TestCheckMountInfo_BIOS_DiskMatchesViaSymlink proves the comparison
// is symlink-aware on BOTH sides (an operator passing /dev/disk/by-id/
// ...-part1's own whole-disk equivalent, or the kernel reporting a
// symlinked path in /proc/self/mounts) - matching parseExistingMount's
// own established symlink-resolution behavior, not a literal-string-
// only comparison that would reject a legitimate match.
func TestCheckMountInfo_BIOS_DiskMatchesViaSymlink(t *testing.T) {
	resolve := func(p string) string {
		aliases := map[string]string{
			"/dev/disk/by-id/nvme-Model_serial": "/dev/nvme0n1",
			"/dev/nvme0n1":                      "/dev/nvme0n1",
		}
		if real, ok := aliases[p]; ok {
			return real
		}
		return p
	}
	err := checkMountInfo("/mnt/alpine/boot/efi", "/dev/nvme0n1p1", "vfat", true,
		"/dev/disk/by-id/nvme-Model_serial", resolve)
	if err != nil {
		t.Errorf("want nil when both sides resolve to the same real disk via symlink, got: %v", err)
	}
}

// TestCheckMountInfo_BIOS_ByIDAliasWithPartitionSuffix is the direct
// regression test for N5 (unidoc-alip's PR #5 follow-up review), the
// first of its two failure shapes: a by-id alias that itself embeds a
// partition suffix in its own NAME (a real, common udev naming
// convention - "ata-Model_serial-part1"), which DevicePartitionBase
// was never meant to understand (it only knows real kernel device-
// naming conventions) and must never be asked to parse BEFORE the
// symlink is resolved to a real device path.
func TestCheckMountInfo_BIOS_ByIDAliasWithPartitionSuffix(t *testing.T) {
	resolve := func(p string) string {
		if p == "/dev/disk/by-id/ata-Model_serial-part1" {
			return "/dev/sda1"
		}
		return p
	}
	err := checkMountInfo("/mnt/alpine/boot/efi", "/dev/disk/by-id/ata-Model_serial-part1", "vfat", true,
		"/dev/sda", resolve)
	if err != nil {
		t.Errorf("want nil - both sides resolve to the same real whole disk (/dev/sda) once the alias is resolved BEFORE DevicePartitionBase runs on it, got: %v", err)
	}
}

// TestCheckMountInfo_BIOS_ByUUIDAliasSuffixNeverStripped is N5's second
// failure shape: a by-uuid alias that carries no partition-like suffix
// of its own at all, resolving to a real, partition-suffixed device
// path (/dev/sda1) - DevicePartitionBase must run on THAT resolved
// path, not on the alias string it replaced.
func TestCheckMountInfo_BIOS_ByUUIDAliasSuffixNeverStripped(t *testing.T) {
	resolve := func(p string) string {
		if p == "/dev/disk/by-uuid/1234-5678" {
			return "/dev/sda1"
		}
		return p
	}
	err := checkMountInfo("/mnt/alpine/boot/efi", "/dev/disk/by-uuid/1234-5678", "vfat", true,
		"/dev/sda", resolve)
	if err != nil {
		t.Errorf("want nil - the uuid alias resolves to /dev/sda1, which DevicePartitionBase must then strip to /dev/sda to match diskArg, got: %v", err)
	}
}

// TestCanonicalizeMountpoint_RelativePathBecomesAbsolute is the other
// half of N5 (unidoc-alip's PR #5 follow-up review): a relative --root
// must resolve to the same absolute form /proc/self/mounts itself
// reports, or a genuinely-mounted ESP is refused as "not mounted at
// all".
func TestCanonicalizeMountpoint_RelativePathBecomesAbsolute(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	got := canonicalizeMountpoint("boot/efi")
	want := filepath.Join(wd, "boot/efi")
	if got != want {
		t.Errorf("canonicalizeMountpoint(%q) = %q, want %q (absolute, relative to the real working directory)", "boot/efi", got, want)
	}
}

// TestCanonicalizeMountpoint_SymlinkComponentResolved proves a
// mountpoint reached through a symlinked PARENT directory (not the
// mountpoint itself - resolving through the actual mount would follow
// into the mounted filesystem's own root, not what's being checked
// here) still canonicalizes to the same real path the kernel reports.
func TestCanonicalizeMountpoint_SymlinkComponentResolved(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(filepath.Join(real, "boot", "efi"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	got := canonicalizeMountpoint(filepath.Join(link, "boot", "efi"))
	want := filepath.Join(real, "boot", "efi")
	if got != want {
		t.Errorf("canonicalizeMountpoint through a symlinked parent = %q, want %q (the real path, matching what /proc/self/mounts itself would report)", got, want)
	}
}

func TestSelectESP(t *testing.T) {
	cases := []struct {
		name       string
		candidates []espCandidate
		wantDev    string
		wantErr    bool
	}{
		{
			name:       "no candidates at all",
			candidates: nil,
			wantErr:    true,
		},
		{
			name: "one qualifying candidate wins",
			candidates: []espCandidate{
				{dev: "/dev/sda1", hasConfigMarker: true, hasPayloadMarker: true},
			},
			wantDev: "/dev/sda1",
		},
		{
			name: "candidate with only a config marker does not qualify",
			candidates: []espCandidate{
				{dev: "/dev/sda1", hasConfigMarker: true, hasPayloadMarker: false},
			},
			wantErr: true,
		},
		{
			name: "candidate with only a payload marker does not qualify",
			candidates: []espCandidate{
				{dev: "/dev/sda1", hasConfigMarker: false, hasPayloadMarker: true},
			},
			wantErr: true,
		},
		{
			name: "two qualifying candidates refuses to guess",
			candidates: []espCandidate{
				{dev: "/dev/sda1", hasConfigMarker: true, hasPayloadMarker: true},
				{dev: "/dev/sdb1", hasConfigMarker: true, hasPayloadMarker: true},
			},
			wantErr: true,
		},
		{
			name: "one qualifying among several non-qualifying still wins",
			candidates: []espCandidate{
				{dev: "/dev/sda1", hasConfigMarker: false, hasPayloadMarker: false},
				{dev: "/dev/sdb1", hasConfigMarker: true, hasPayloadMarker: true},
			},
			wantDev: "/dev/sdb1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dev, err := selectESP(tc.candidates)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("selectESP(%v) = %q, nil; want an error", tc.candidates, dev)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectESP(%v) unexpected error: %v", tc.candidates, err)
			}
			if dev != tc.wantDev {
				t.Errorf("selectESP(%v) = %q, want %q", tc.candidates, dev, tc.wantDev)
			}
		})
	}
}

func TestParseDiskLayout(t *testing.T) {
	cases := []struct {
		name string
		lba1 []byte
		want DiskLayout
	}{
		{
			name: "real GPT signature",
			lba1: []byte("EFI PART\x00\x00\x01\x00\x5c\x00\x00\x00"), // signature + version/header-size, like a real GPT header
			want: LayoutGPT,
		},
		{
			name: "msdos disk - LBA 1 is just the first data sector, no GPT signature",
			lba1: []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
			want: LayoutMSDOS,
		},
		{
			name: "short read",
			lba1: []byte("EFI"),
			want: LayoutMSDOS,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseDiskLayout(tc.lba1)
			if got != tc.want {
				t.Errorf("parseDiskLayout(%q) = %v, want %v", tc.lba1, got, tc.want)
			}
		})
	}
}

func TestDetectDiskLayout_RealFile(t *testing.T) {
	dir := t.TempDir()

	gptPath := dir + "/gpt.img"
	gptData := make([]byte, 4096)
	copy(gptData[512:], "EFI PART\x00\x00\x01\x00\x5c\x00\x00\x00")
	if err := os.WriteFile(gptPath, gptData, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := DetectDiskLayout(gptPath)
	if err != nil {
		t.Fatalf("DetectDiskLayout(gpt): %v", err)
	}
	if got != LayoutGPT {
		t.Errorf("DetectDiskLayout(gpt) = %v, want LayoutGPT", got)
	}

	msdosPath := dir + "/msdos.img"
	msdosData := make([]byte, 4096) // all zero - no GPT signature anywhere
	if err := os.WriteFile(msdosPath, msdosData, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = DetectDiskLayout(msdosPath)
	if err != nil {
		t.Fatalf("DetectDiskLayout(msdos): %v", err)
	}
	if got != LayoutMSDOS {
		t.Errorf("DetectDiskLayout(msdos) = %v, want LayoutMSDOS", got)
	}

	if _, err := DetectDiskLayout(dir + "/does-not-exist.img"); err == nil {
		t.Error("DetectDiskLayout on a missing file: want an error, got nil")
	}
}

func TestDevicePartitionBase(t *testing.T) {
	cases := map[string]string{
		"/dev/sda1":       "/dev/sda",
		"/dev/sda12":      "/dev/sda",
		"/dev/vda2":       "/dev/vda",
		"/dev/nvme0n1p1":  "/dev/nvme0n1",
		"/dev/nvme0n1p12": "/dev/nvme0n1",
		"/dev/mmcblk0p1":  "/dev/mmcblk0",
		"/dev/loop0p1":    "/dev/loop0",
		"/dev/sda":        "/dev/sda", // no partition suffix at all - returned unchanged
		// F22 (unidoc-alip's PR #5 review): these two are already
		// WHOLE-DISK names (no "p<N>" partition suffix) - the real bug
		// this regression-tests would have stripped them to "nvme0n"/
		// "mmcblk" instead of leaving them unchanged, since their own
		// whole-disk name already ends in a digit.
		"/dev/nvme0n1": "/dev/nvme0n1",
		"/dev/mmcblk0": "/dev/mmcblk0",
		// F22, unidoc-alip's PR #5 FOLLOW-UP review: the original round
		// above only covered nvme/mmcblk - loop/nbd/md whole-disk names
		// (the same "own name already ends in a digit" shape) were
		// still silently mismatched by partitionSuffixPlain's generic
		// fallback (md127 -> "md", a real, supported RAID1 boot disk).
		"/dev/loop0": "/dev/loop0",
		"/dev/nbd0":  "/dev/nbd0",
		"/dev/md127": "/dev/md127",
		"/dev/md0":   "/dev/md0",
		// And their own real partition-suffixed forms, for symmetry
		// with nvme0n1p1/mmcblk0p1 above.
		"/dev/nbd0p1":  "/dev/nbd0",
		"/dev/md127p1": "/dev/md127",
	}
	for in, want := range cases {
		if got := DevicePartitionBase(in); got != want {
			t.Errorf("DevicePartitionBase(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- CheckStage2ExtentFree ------------------------------------------
//
// Compact, real-byte GPT/MBR fixture builders (deliberately simpler
// than internal/parttable's own, which already proves the low-level
// decode is correct - these exist to drive CheckStage2ExtentFree's own
// DECISION logic - overlap vs. no overlap, exact-match tolerance -
// through real disk bytes, not to re-prove parttable's own byte-level
// correctness a second time.

// buildTestGPTDisk builds a GPT disk with zero, one, or more partition
// entries (each a [typeGUID, startLBA, sectorCount, name] tuple) plus
// a real, correct header CRC32 - numEntries controls the header's own
// declared table size, independent of len(entries), so a test can
// construct a non-standard (larger) partition array footprint without
// needing that many real entries.
type testGPTEntry struct {
	typeGUID    [16]byte
	startLBA    uint64
	sectorCount uint64
	name        string
}

func buildTestGPTDiskN(t *testing.T, numEntries uint32, entries []testGPTEntry) string {
	t.Helper()
	const arrayLBA = 2
	const entrySize = 128
	arraySectors := (numEntries*entrySize + 511) / 512

	disk := make([]byte, (arrayLBA+uint64(arraySectors)+100)*512)
	disk[510], disk[511] = 0x55, 0xAA

	hdr := make([]byte, 92)
	copy(hdr[0:8], []byte("EFI PART"))
	putU32(hdr[8:12], 0x00010000)
	putU32(hdr[12:16], 92)
	putU64(hdr[24:32], 1)
	putU64(hdr[72:80], arrayLBA)
	putU32(hdr[80:84], numEntries)
	putU32(hdr[84:88], entrySize)
	crc := crc32IEEE(hdr)
	putU32(hdr[16:20], crc)
	copy(disk[512:512+92], hdr)

	for i, e := range entries {
		off := arrayLBA*512 + i*entrySize
		copy(disk[off:off+16], e.typeGUID[:])
		putU64(disk[off+32:off+40], e.startLBA)
		putU64(disk[off+40:off+48], e.startLBA+e.sectorCount-1)
		for j, r := range e.name {
			putU16(disk[off+56+j*2:off+58+j*2], uint16(r))
		}
	}

	path := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(path, disk, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func buildTestGPTDisk(t *testing.T, entries []testGPTEntry) string {
	t.Helper()
	return buildTestGPTDiskN(t, 128, entries)
}

func buildTestMBRDisk(t *testing.T, entries []testGPTEntry) string {
	t.Helper()
	disk := make([]byte, 4096)
	disk[510], disk[511] = 0x55, 0xAA
	for i, e := range entries {
		off := 446 + i*16
		disk[off+4] = byte(e.typeGUID[0]) // reused field: low byte of typeGUID[0] carries the MBR type for these tests
		putU32(disk[off+8:off+12], uint32(e.startLBA))
		putU32(disk[off+12:off+16], uint32(e.sectorCount))
	}
	path := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(path, disk, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func mbrEntry(partType byte, startLBA, sectorCount uint64) testGPTEntry {
	return testGPTEntry{typeGUID: [16]byte{partType}, startLBA: startLBA, sectorCount: sectorCount}
}

func TestCheckStage2ExtentFree_GPT_NoPartitionsAtAll(t *testing.T) {
	// The real uniclus-01 shape: a valid GPT disk where NOTHING covers
	// LBA 34-97 - no partition-table entry exists for that range at
	// all, because stage2 was written directly to the reserved extent
	// on the whole-disk device, never through a partition. Must pass.
	disk := buildTestGPTDisk(t, nil)
	if err := CheckStage2ExtentFree(disk); err != nil {
		t.Errorf("CheckStage2ExtentFree on a GPT disk with no partitions at all: %v", err)
	}
}

func TestCheckStage2ExtentFree_GPT_ExactMatchCosmeticEntry_Tolerated(t *testing.T) {
	// alpine-install-zfs.sh's own partition_disk() still creates
	// exactly this entry on a fresh GPT install - must NOT be treated
	// as a conflict, it describes the same extent, not a foreign one.
	disk := buildTestGPTDisk(t, []testGPTEntry{
		{typeGUID: layout.BIOSBootPartitionGUIDBytes, startLBA: layout.Stage2LBA, sectorCount: layout.Stage2Sectors, name: layout.Stage2PartitionName},
	})
	if err := CheckStage2ExtentFree(disk); err != nil {
		t.Errorf("CheckStage2ExtentFree with an exact-match cosmetic entry: %v", err)
	}
}

func TestCheckStage2ExtentFree_GPT_PartialOverlap_Refused(t *testing.T) {
	// A real partition starting inside our extent and extending past
	// it - a genuine conflict, not our own marker.
	disk := buildTestGPTDisk(t, []testGPTEntry{
		{typeGUID: layout.ESPTypeGUIDBytes, startLBA: layout.Stage2LBA + 10, sectorCount: 1000, name: "EFI"},
	})
	if err := CheckStage2ExtentFree(disk); err == nil {
		t.Error("CheckStage2ExtentFree with a partition partially overlapping the extent: want an error, got nil")
	}
}

func TestCheckStage2ExtentFree_GPT_FullyContainingOverlap_Refused(t *testing.T) {
	// A large partition that swallows our whole extent (e.g. some
	// other tool's data partition starting before LBA34) - a real
	// conflict, same as a partial overlap.
	disk := buildTestGPTDisk(t, []testGPTEntry{
		{typeGUID: layout.ESPTypeGUIDBytes, startLBA: 10, sectorCount: 1000, name: "unrelated"},
	})
	if err := CheckStage2ExtentFree(disk); err == nil {
		t.Error("CheckStage2ExtentFree with a partition fully containing the extent: want an error, got nil")
	}
}

func TestCheckStage2ExtentFree_GPT_UnrelatedPartitionElsewhere_Passes(t *testing.T) {
	// A real partition exists, just nowhere near our extent - must
	// not be flagged.
	disk := buildTestGPTDisk(t, []testGPTEntry{
		{typeGUID: layout.ESPTypeGUIDBytes, startLBA: 2048, sectorCount: 1048576, name: "EFI"},
	})
	if err := CheckStage2ExtentFree(disk); err != nil {
		t.Errorf("CheckStage2ExtentFree with an unrelated partition elsewhere on disk: %v", err)
	}
}

func TestCheckStage2ExtentFree_GPT_PartitionArrayFootprintOverlap_Refused(t *testing.T) {
	// A non-standard GPT declaring a partition array large enough to
	// structurally extend into LBA 34-97, even with zero actual
	// partition entries defined - the array's own on-disk footprint
	// is itself a real occupant of that space.
	disk := buildTestGPTDiskN(t, 4096, nil) // 4096 entries * 128 bytes = 524288 bytes = 1024 sectors, array runs LBA 2-1025, well past LBA 34-97
	if err := CheckStage2ExtentFree(disk); err == nil {
		t.Error("CheckStage2ExtentFree with a GPT partition array overlapping the extent: want an error, got nil")
	}
}

func TestCheckStage2ExtentFree_MSDOS_NoPartitionsAtAll(t *testing.T) {
	disk := buildTestMBRDisk(t, nil)
	if err := CheckStage2ExtentFree(disk); err != nil {
		t.Errorf("CheckStage2ExtentFree on an msdos disk with no partitions at all: %v", err)
	}
}

func TestCheckStage2ExtentFree_MSDOS_ExactMatchCosmeticEntry_Tolerated(t *testing.T) {
	disk := buildTestMBRDisk(t, []testGPTEntry{mbrEntry(layout.Stage2MSDOSType, layout.Stage2LBA, layout.Stage2Sectors)})
	if err := CheckStage2ExtentFree(disk); err != nil {
		t.Errorf("CheckStage2ExtentFree with an exact-match msdos cosmetic entry: %v", err)
	}
}

func TestCheckStage2ExtentFree_MSDOS_OverlappingPartition_Refused(t *testing.T) {
	disk := buildTestMBRDisk(t, []testGPTEntry{mbrEntry(layout.FATMBRType, layout.Stage2LBA+5, 1000)})
	if err := CheckStage2ExtentFree(disk); err == nil {
		t.Error("CheckStage2ExtentFree with an msdos partition overlapping the extent: want an error, got nil")
	}
}

func TestCheckStage2ExtentFree_NeitherGPTNorMBR(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blank.img")
	if err := os.WriteFile(path, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckStage2ExtentFree(path); err == nil {
		t.Error("CheckStage2ExtentFree on a disk with no partition table at all: want an error, got nil")
	}
}
