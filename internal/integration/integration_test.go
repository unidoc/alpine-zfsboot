// Package integration exercises install/update's own real sequence -
// WriteStage1 -> WriteStage2 -> WritePayload -> WriteConfig ->
// Verify* - across a realistic GPT disk, a realistic msdos disk, and
// the UEFI loader path, calling the exact same internal/biosboot,
// internal/espconfig, and internal/uefiboot functions cmd/tool's own
// install/update commands call, in the same order. cmd/tool's own
// Run functions aren't called directly (they os.Exit on failure,
// which would kill the test process) - this is the same logic
// those functions run, without that CLI-layer side effect.
package integration

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/unidoc/alpine-zfsboot/internal/biosboot"
	"github.com/unidoc/alpine-zfsboot/internal/bootenv"
	"github.com/unidoc/alpine-zfsboot/internal/espconfig"
	"github.com/unidoc/alpine-zfsboot/internal/layout"
	"github.com/unidoc/alpine-zfsboot/internal/uefiboot"
)

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(int(seed) + i*7 + 3)
	}
	return b
}

// buildGPTDisk hand-assembles a real GPT disk image with alpine-installer's
// own exact stage2 reservation (BIOS boot GUID, LBA 34, 64 sectors,
// named "alpine-zfsboot-stage2") - what partition_disk() actually
// produces, not a simplified stand-in.
func buildGPTDisk(t *testing.T) string {
	t.Helper()
	const arrayLBA = 2
	const numEntries = 128
	const entrySize = 128
	diskSectors := layout.Stage2LBA + layout.Stage2Sectors + 100

	disk := make([]byte, diskSectors*layout.SectorSize)
	disk[510], disk[511] = 0x55, 0xAA

	hdr := make([]byte, 92)
	copy(hdr[0:8], []byte("EFI PART"))
	binary.LittleEndian.PutUint32(hdr[8:12], 0x00010000)
	binary.LittleEndian.PutUint32(hdr[12:16], 92)
	binary.LittleEndian.PutUint64(hdr[24:32], 1)
	binary.LittleEndian.PutUint64(hdr[72:80], arrayLBA)
	binary.LittleEndian.PutUint32(hdr[80:84], numEntries)
	binary.LittleEndian.PutUint32(hdr[84:88], entrySize)
	// CRC32 computed the same way internal/parttable's own test fixture
	// does - see that package for the from-scratch cross-check this
	// value was originally validated against.
	crc := crc32IEEE(hdr)
	binary.LittleEndian.PutUint32(hdr[16:20], crc)
	copy(disk[layout.SectorSize:layout.SectorSize+92], hdr)

	off := arrayLBA * layout.SectorSize
	copy(disk[off:off+16], layout.BIOSBootPartitionGUIDBytes[:])
	binary.LittleEndian.PutUint64(disk[off+32:off+40], uint64(layout.Stage2LBA))
	binary.LittleEndian.PutUint64(disk[off+40:off+48], uint64(layout.Stage2LBA+layout.Stage2Sectors-1))
	for j, r := range layout.Stage2PartitionName {
		binary.LittleEndian.PutUint16(disk[off+56+j*2:off+58+j*2], uint16(r))
	}

	path := filepath.Join(t.TempDir(), "gpt-disk.img")
	if err := os.WriteFile(path, disk, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// buildGPTDiskWithOverlappingPartition builds a GPT disk with a real,
// unrelated data partition genuinely overlapping the stage2 extent -
// the actual safety case CheckStage2ExtentFree exists for (see that
// function's own doc comment: a specially-named marker is NOT the
// invariant, an unoccupied extent is).
func buildGPTDiskWithOverlappingPartition(t *testing.T) string {
	t.Helper()
	const arrayLBA = 2
	const numEntries = 128
	const entrySize = 128
	diskSectors := layout.Stage2LBA + layout.Stage2Sectors + 100

	disk := make([]byte, diskSectors*layout.SectorSize)
	disk[510], disk[511] = 0x55, 0xAA

	hdr := make([]byte, 92)
	copy(hdr[0:8], []byte("EFI PART"))
	binary.LittleEndian.PutUint32(hdr[8:12], 0x00010000)
	binary.LittleEndian.PutUint32(hdr[12:16], 92)
	binary.LittleEndian.PutUint64(hdr[24:32], 1)
	binary.LittleEndian.PutUint64(hdr[72:80], arrayLBA)
	binary.LittleEndian.PutUint32(hdr[80:84], numEntries)
	binary.LittleEndian.PutUint32(hdr[84:88], entrySize)
	crc := crc32IEEE(hdr)
	binary.LittleEndian.PutUint32(hdr[16:20], crc)
	copy(disk[layout.SectorSize:layout.SectorSize+92], hdr)

	off := arrayLBA * layout.SectorSize
	copy(disk[off:off+16], layout.ESPTypeGUIDBytes[:]) // an ordinary ESP-typed partition, nothing alpine-zfsboot-specific
	overlapStart := uint64(layout.Stage2LBA + 10)
	overlapEnd := overlapStart + 500
	binary.LittleEndian.PutUint64(disk[off+32:off+40], overlapStart)
	binary.LittleEndian.PutUint64(disk[off+40:off+48], overlapEnd)

	path := filepath.Join(t.TempDir(), "gpt-disk-overlap.img")
	if err := os.WriteFile(path, disk, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// buildMSDOSDisk hand-assembles a real msdos disk image with
// alpine-installer's own exact stage2 reservation (type 0x2D, LBA 34,
// 64 sectors).
func buildMSDOSDisk(t *testing.T) string {
	t.Helper()
	diskSectors := layout.Stage2LBA + layout.Stage2Sectors + 100
	disk := make([]byte, diskSectors*layout.SectorSize)
	disk[510], disk[511] = 0x55, 0xAA

	const entryOff = 446
	disk[entryOff+4] = layout.Stage2MSDOSType
	binary.LittleEndian.PutUint32(disk[entryOff+8:entryOff+12], uint32(layout.Stage2LBA))
	binary.LittleEndian.PutUint32(disk[entryOff+12:entryOff+16], uint32(layout.Stage2Sectors))

	path := filepath.Join(t.TempDir(), "msdos-disk.img")
	if err := os.WriteFile(path, disk, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func crc32IEEE(b []byte) uint32 {
	const poly = 0xEDB88320
	crc := uint32(0xFFFFFFFF)
	for _, c := range b {
		crc ^= uint32(c)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ poly
			} else {
				crc >>= 1
			}
		}
	}
	return ^crc
}

// runBIOSInstallSequence is exactly install_alpine_zfsboot_bios()'s own
// sequence, from cmd/tool's installBIOS(): WriteStage1, WriteStage2,
// WritePayload, WriteConfig, verified at every step - then, separately,
// the SAME sequence again simulating `update` (a second, different
// build overwriting the first).
func runBIOSInstallSequence(t *testing.T, disk string) {
	t.Helper()
	mnt := t.TempDir()

	stage1 := pattern(layout.Stage1Bytes, 1)
	stage2 := pattern(9000, 2)
	kernel := pattern(50000, 3)
	initrd := pattern(80000, 4)
	cmdline := []byte("root=ZFS=zroot/ROOT/alpine ro console=tty0\n")
	cfg := espconfig.Config{Net: "static", IPv4: "dhcp", SSHPort: "22"}

	// install
	if _, err := biosboot.WriteStage1(disk, stage1); err != nil {
		t.Fatalf("install: WriteStage1: %v", err)
	}
	if err := biosboot.VerifyStage1(disk, stage1); err != nil {
		t.Fatalf("install: VerifyStage1: %v", err)
	}
	if _, err := biosboot.WriteStage2(disk, stage2); err != nil {
		t.Fatalf("install: WriteStage2: %v", err)
	}
	if err := biosboot.VerifyStage2(disk, stage2); err != nil {
		t.Fatalf("install: VerifyStage2: %v", err)
	}
	if err := espconfig.WritePayload(mnt, kernel, initrd, cmdline); err != nil {
		t.Fatalf("install: WritePayload: %v", err)
	}
	if err := espconfig.VerifyPayload(mnt, kernel, initrd, cmdline); err != nil {
		t.Fatalf("install: VerifyPayload: %v", err)
	}
	if err := espconfig.WriteConfig(mnt, cfg); err != nil {
		t.Fatalf("install: WriteConfig: %v", err)
	}
	if err := espconfig.VerifyConfig(mnt, cfg); err != nil {
		t.Fatalf("install: VerifyConfig: %v", err)
	}
	if err := espconfig.WriteAuthorizedKeys(mnt, "ssh-ed25519 AAAA... test@host"); err != nil {
		t.Fatalf("install: WriteAuthorizedKeys: %v", err)
	}

	// Confirm the safety gate STILL passes after a real write (it
	// must - WriteStage2 already checked it, but this proves the
	// gate's own read-back doesn't get confused by the write it just
	// did, or by its own cosmetic partition entry, where present).
	if err := bootenv.CheckStage2ExtentFree(disk); err != nil {
		t.Fatalf("CheckStage2ExtentFree after a real install: %v", err)
	}

	// update: a second, larger stage2 (exercises the zero-then-write
	// padding invariant across a real install->update transition, not
	// just biosboot's own isolated unit test) and different payload
	// content, simulating a real version bump.
	newStage1 := pattern(layout.Stage1Bytes, 10)
	newStage2 := pattern(15000, 11)
	newKernel := pattern(51000, 12)
	newInitrd := pattern(81000, 13)
	newCmdline := []byte("root=ZFS=zroot/ROOT/alpine ro console=ttyS0\n")

	if _, err := biosboot.WriteStage1(disk, newStage1); err != nil {
		t.Fatalf("update: WriteStage1: %v", err)
	}
	if err := biosboot.VerifyStage1(disk, newStage1); err != nil {
		t.Fatalf("update: VerifyStage1: %v", err)
	}
	if _, err := biosboot.WriteStage2(disk, newStage2); err != nil {
		t.Fatalf("update: WriteStage2: %v", err)
	}
	if err := biosboot.VerifyStage2(disk, newStage2); err != nil {
		t.Fatalf("update: VerifyStage2: %v", err)
	}
	// The OLD stage2's own trailing bytes (beyond the new, larger
	// stage2's own length) must be gone - direct region check, the
	// exact regression this whole feature exists to prevent.
	if err := biosboot.VerifyStage2(disk, newStage2); err != nil {
		t.Fatalf("update: stage2 region does not read back as exactly the new content + zero padding: %v", err)
	}
	if err := espconfig.WritePayload(mnt, newKernel, newInitrd, newCmdline); err != nil {
		t.Fatalf("update: WritePayload: %v", err)
	}
	if err := espconfig.VerifyPayload(mnt, newKernel, newInitrd, newCmdline); err != nil {
		t.Fatalf("update: VerifyPayload: %v", err)
	}
	// The update must not have left the OLD payload content behind
	// under the old name, or partially overwritten - VerifyPayload
	// already confirms the new content, and this confirms the old
	// content is truly gone, not still readable by coincidence.
	if err := espconfig.VerifyPayload(mnt, kernel, initrd, cmdline); err == nil {
		t.Fatal("update: old payload content still verifies after being overwritten - update did not really replace it")
	}
}

func TestBIOSInstallThenUpdate_GPT(t *testing.T) {
	disk := buildGPTDisk(t)
	runBIOSInstallSequence(t, disk)
}

func TestBIOSInstallThenUpdate_MSDOS(t *testing.T) {
	disk := buildMSDOSDisk(t)
	runBIOSInstallSequence(t, disk)
}

func TestBIOSInstall_AllowsDiskWithNoPartitionTableEntryAtAll(t *testing.T) {
	// The real uniclus-01 shape: a valid GPT disk where NOTHING covers
	// LBA 34-97 at all - no cosmetic reservation, no partition, nothing
	// - because this project's own real migration path
	// (update-stage2-only.sh) writes stage2 directly into the reserved
	// extent on the whole-disk device, never through a partition-table
	// entry (see bootenv.CheckStage2ExtentFree's own doc comment for
	// why an earlier version of this check wrongly required one).
	// Ownership for install comes from the explicit disk argument +
	// confirmation, not from partition-table content - this must
	// SUCCEED, not fail closed.
	path := filepath.Join(t.TempDir(), "unpartitioned.img")
	disk := make([]byte, (layout.Stage2LBA+layout.Stage2Sectors+100)*layout.SectorSize)
	disk[510], disk[511] = 0x55, 0xAA // boot signature only, no partition entries at all
	if err := os.WriteFile(path, disk, 0o644); err != nil {
		t.Fatal(err)
	}

	stage2 := pattern(9000, 1)
	if _, err := biosboot.WriteStage2(path, stage2); err != nil {
		t.Fatalf("WriteStage2 on a disk with no partition-table entry at all (the real uniclus-01 shape): %v", err)
	}
	if err := biosboot.VerifyStage2(path, stage2); err != nil {
		t.Fatalf("VerifyStage2 after that write: %v", err)
	}
}

func TestBIOSInstall_RefusesDiskWithOverlappingPartition(t *testing.T) {
	// The real safety case: a partition (of ANY kind - here a random
	// data partition, not anything alpine-zfsboot-related) genuinely
	// overlaps the stage2 extent. This is what fail-closed protection
	// is actually for - not the absence of a specially-named marker.
	disk := buildGPTDiskWithOverlappingPartition(t)
	stage2 := pattern(9000, 1)
	if _, err := biosboot.WriteStage2(disk, stage2); err == nil {
		t.Fatal("WriteStage2 on a disk with a real partition overlapping the stage2 extent: want an error, got nil")
	}
}

// TestBIOSInstall_OverlapRefusalLeavesStage1Untouched is the regression
// test for a real bug found (and fixed) in a full source audit: both
// cmd/tool's installBIOS() and updateBIOS() used to call
// biosboot.WriteStage1() BEFORE biosboot.WriteStage2() - and
// CheckStage2ExtentFree() lived only INSIDE WriteStage2(). So a disk
// whose stage2 extent was occupied got its MBR bytes 0-439 mutated by
// WriteStage1 FIRST, and only THEN got correctly refused by WriteStage2
// - "refusing to write stage2" printed on a disk whose stage1 had
// already changed.
//
// The fix moved the preflight (CheckStage2ExtentFree) to run before the
// first mutation in both cmd/tool functions - this test proves that
// invariant at the primitive level: calling the safety check first, the
// way cmd/tool now does, means WriteStage1 is never reached at all, so
// stage1 cannot have changed. This can't call cmd/tool's own
// installBIOS()/updateBIOS() directly (see this file's own package
// comment - they os.Exit on failure), so it proves the underlying
// contract those functions now rely on, the same limitation every other
// test in this file already accepts.
//
// A LATER, separate audit finding gave WriteStage1 its own internal
// safety gate too (see biosboot.go's own doc comment) - so this disk is
// now refused TWICE over, by both the outer preflight AND WriteStage1
// itself. That's a strictly stronger property than this test originally
// had to rely on, but it means the ORIGINAL negative control here (call
// WriteStage1 directly and confirm it mutates the disk, proving the
// assertions above aren't vacuous) no longer demonstrates anything -
// WriteStage1 now correctly refuses on THIS disk too. The negative
// control below proves the same thing a different way instead: that
// WriteStage1's own mutation IS genuinely detectable by these same
// byte-comparison assertions, using a second, ELIGIBLE disk (no
// overlap) where WriteStage1 is expected to actually succeed.
func TestBIOSInstall_OverlapRefusalLeavesStage1Untouched(t *testing.T) {
	disk := buildGPTDiskWithOverlappingPartition(t)

	before, err := os.ReadFile(disk)
	if err != nil {
		t.Fatal(err)
	}
	stage1Before := append([]byte(nil), before[:layout.Stage1Bytes]...)

	stage1 := pattern(layout.Stage1Bytes, 5)

	// The corrected cmd/tool sequence: preflight BEFORE any mutation.
	if err := bootenv.CheckStage2ExtentFree(disk); err != nil {
		// Correctly refused before WriteStage1 - i.e. exactly what
		// cmd/tool's own die() does now, one step earlier than before
		// the fix. WriteStage1 is deliberately never called on this
		// path.
	} else {
		t.Fatal("CheckStage2ExtentFree on a disk with a real overlapping partition: want an error, got nil")
	}

	after, err := os.ReadFile(disk)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stage1Before, after[:layout.Stage1Bytes]) {
		t.Fatal("stage1 region changed even though the preflight check refused the operation before any write")
	}

	// WriteStage1 now ALSO refuses on this same overlapping disk, via
	// its own internal gate - defense in depth on top of the outer
	// preflight ordering fix, not a redundant assertion: this is exactly
	// what would catch a FUTURE regression that reordered cmd/tool's own
	// calls again, even without the outer preflight's help.
	if _, err := biosboot.WriteStage1(disk, stage1); err == nil {
		t.Fatal("WriteStage1 on a disk with a real overlapping partition: want an error (its own safety gate should refuse), got nil")
	}
	afterDirectCall, err := os.ReadFile(disk)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stage1Before, afterDirectCall[:layout.Stage1Bytes]) {
		t.Fatal("stage1 region changed even though WriteStage1's own safety gate refused the write")
	}

	// Negative control: prove the byte-comparison assertions above are
	// actually capable of detecting a real mutation - on a SEPARATE,
	// eligible disk (no overlap), WriteStage1 must succeed and the
	// region must actually change.
	eligibleDisk := buildGPTDisk(t)
	eligibleBefore, err := os.ReadFile(eligibleDisk)
	if err != nil {
		t.Fatal(err)
	}
	eligibleStage1Before := append([]byte(nil), eligibleBefore[:layout.Stage1Bytes]...)
	if _, err := biosboot.WriteStage1(eligibleDisk, stage1); err != nil {
		t.Fatalf("WriteStage1 (negative control, eligible disk): %v", err)
	}
	eligibleAfter, err := os.ReadFile(eligibleDisk)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(eligibleStage1Before, eligibleAfter[:layout.Stage1Bytes]) {
		t.Fatal("negative control failed: WriteStage1 did not actually change stage1 on an eligible disk - this test's assertions prove nothing")
	}
}

func TestUEFIInstallThenUpdate(t *testing.T) {
	mnt := t.TempDir()
	arch := "x86_64"

	build1 := pattern(200000, 1)
	backedUp, err := uefiboot.WriteLoader(mnt, arch, build1)
	if err != nil {
		t.Fatalf("install: WriteLoader: %v", err)
	}
	if backedUp {
		t.Error("install: WriteLoader on a fresh ESP reported a backup, but nothing existed yet")
	}
	if err := uefiboot.VerifyLoader(mnt, arch, build1); err != nil {
		t.Fatalf("install: VerifyLoader: %v", err)
	}

	build2 := pattern(210000, 2)
	backedUp, err = uefiboot.WriteLoader(mnt, arch, build2)
	if err != nil {
		t.Fatalf("update: WriteLoader: %v", err)
	}
	if !backedUp {
		t.Error("update: WriteLoader over an existing loader should have backed it up")
	}
	if err := uefiboot.VerifyLoader(mnt, arch, build2); err != nil {
		t.Fatalf("update: VerifyLoader: %v", err)
	}
	if err := uefiboot.VerifyLoader(mnt, arch, build1); err == nil {
		t.Fatal("update: the OLD build still verifies as the installed loader - update did not really replace it")
	}

	rel, err := uefiboot.LoaderPath(arch)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(filepath.Join(mnt, rel+".previous"))
	if err != nil {
		t.Fatalf("reading the .previous rollback copy: %v", err)
	}
	if !bytes.Equal(previous, build1) {
		t.Error(".previous does not contain the first build - rollback copy is wrong")
	}
}
