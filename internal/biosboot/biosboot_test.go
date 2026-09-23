package biosboot

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/unidoc/alpine-zfsboot/internal/layout"
)

// fakeDisk creates a plain file standing in for a disk device - Go's
// os.File I/O (Open/OpenFile/ReadAt/WriteAt) behaves identically
// against a regular file and a real block device, so this exercises
// the real read/write/verify code paths without needing root or a
// loop device (unavailable in this sandbox - see this package's own
// tests for the same constraint noted elsewhere in this project).
func fakeDisk(t *testing.T, sizeBytes int, fill byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "disk.img")
	data := bytes.Repeat([]byte{fill}, sizeBytes)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeDiskWithReservation is fakeDisk plus a real, valid MBR (a real
// 0x55AA boot signature, required before WriteStage2's own fail-closed
// safety gate - bootenv.CheckStage2ExtentFree, see WriteStage2's own
// doc comment - can even determine a layout to safety-check at all)
// and a cosmetic partition-table entry at exactly the stage2 extent,
// matching what alpine-install-zfs.sh's own partition_disk() still
// writes on a fresh msdos install - TOLERATED by CheckStage2ExtentFree
// (an exact-position match, not a foreign claim), not REQUIRED by it -
// see TestWriteStage2_RejectsDiskWithNoPartitionTableAtAll for the
// real negative case (no parseable partition table at all, neither
// GPT nor MBR - a genuinely different, still-valid refusal reason from
// what this fixture's own name might suggest).
func fakeDiskWithReservation(t *testing.T, sizeBytes int, fill byte) string {
	t.Helper()
	path := fakeDisk(t, sizeBytes, fill)

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	mbr := make([]byte, 512)
	if _, err := f.ReadAt(mbr, 0); err != nil {
		t.Fatal(err)
	}
	mbr[510], mbr[511] = 0x55, 0xAA // boot signature
	const entryOff = 446
	mbr[entryOff+4] = layout.Stage2MSDOSType
	putLE32(mbr[entryOff+8:entryOff+12], uint32(layout.Stage2LBA))
	putLE32(mbr[entryOff+12:entryOff+16], uint32(layout.Stage2Sectors))
	if _, err := f.WriteAt(mbr, 0); err != nil {
		t.Fatal(err)
	}
	return path
}

func putLE32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(int(seed) + i*7 + 3)
	}
	return b
}

func TestWriteStage1_RejectsWrongSize(t *testing.T) {
	disk := fakeDisk(t, 4096, 0xAB)
	_, err := WriteStage1(disk, make([]byte, layout.Stage1Bytes-1))
	if err == nil {
		t.Fatal("WriteStage1 with the wrong length: want an error, got nil")
	}
}

func TestWriteStage1_WritesAndVerifies(t *testing.T) {
	// fakeDiskWithReservation, not plain fakeDisk - WriteStage1 now has
	// its own safety gate too (see WriteStage1's own doc comment: a
	// full source audit found it had none at all before), so it needs
	// the same valid-MBR-shaped fixture WriteStage2's own tests already
	// use.
	disk := fakeDiskWithReservation(t, 4096, 0xAB)
	stage1 := pattern(layout.Stage1Bytes, 1)

	// Snapshot bytes 440-511 (the MBR signature/cosmetic partition entry
	// fakeDiskWithReservation just wrote there) BEFORE the write under
	// test - WriteStage1 must leave this region byte-for-byte identical,
	// but it's no longer a plain fill byte the way it was against
	// fakeDisk, so the real assertion is "unchanged", not "still 0xAB".
	wantTail, err := readRegion(disk, layout.Stage1Bytes, 512-layout.Stage1Bytes)
	if err != nil {
		t.Fatal(err)
	}

	previous, err := WriteStage1(disk, stage1)
	if err != nil {
		t.Fatalf("WriteStage1: %v", err)
	}
	if len(previous) != layout.Stage1Bytes || previous[0] != 0xAB {
		t.Errorf("WriteStage1 previous content = %v, want %d bytes of 0xAB", previous, layout.Stage1Bytes)
	}

	if err := VerifyStage1(disk, stage1); err != nil {
		t.Errorf("VerifyStage1 after a correct write: %v", err)
	}

	// Bytes 440-511 (the region this write must never touch) must
	// still read back exactly as they were before this write.
	gotTail, err := readRegion(disk, layout.Stage1Bytes, 512-layout.Stage1Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotTail, wantTail) {
		t.Errorf("bytes past stage1's own region were touched - WriteStage1 must never write past %d bytes\ngot:  %x\nwant: %x", layout.Stage1Bytes, gotTail, wantTail)
	}
}

func TestVerifyStage1_DetectsMismatch(t *testing.T) {
	disk := fakeDiskWithReservation(t, 4096, 0xAB)
	stage1 := pattern(layout.Stage1Bytes, 1)
	if _, err := WriteStage1(disk, stage1); err != nil {
		t.Fatal(err)
	}
	wrong := pattern(layout.Stage1Bytes, 2)
	if err := VerifyStage1(disk, wrong); err == nil {
		t.Error("VerifyStage1 against mismatched content: want an error, got nil")
	}
}

func TestWriteStage2_SmallerThanPreviousLeavesZeroPadding(t *testing.T) {
	disk := fakeDiskWithReservation(t, (layout.Stage2LBA+layout.Stage2Sectors+10)*layout.SectorSize, 0xAB)

	// Simulate an old, LARGER stage2 already occupying most of the
	// reserved extent - the exact scenario that motivated zeroing the
	// whole region first (see WriteStage2's own doc comment).
	oldStage2 := bytes.Repeat([]byte{0xCC}, layout.Stage2Bytes-2000)
	if _, err := WriteStage2(disk, oldStage2); err != nil {
		t.Fatalf("seeding an old stage2: %v", err)
	}

	newStage2 := pattern(8529, 5) // a realistic size, well under the budget
	previous, err := WriteStage2(disk, newStage2)
	if err != nil {
		t.Fatalf("WriteStage2: %v", err)
	}
	if !bytes.Equal(previous, append(append([]byte{}, oldStage2...), make([]byte, 2000)...)) {
		t.Errorf("WriteStage2's own previous-content backup does not match what was actually there before")
	}

	if err := VerifyStage2(disk, newStage2); err != nil {
		t.Errorf("VerifyStage2 after a correct write: %v", err)
	}

	// Directly confirm the old, larger stage2's own trailing bytes
	// are gone, not just that VerifyStage2 is satisfied (belt and
	// braces - this is the actual bug being guarded against).
	region, err := readRegion(disk, int64(layout.Stage2LBA)*layout.SectorSize, layout.Stage2Bytes)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(newStage2); i < len(region); i++ {
		if region[i] != 0 {
			t.Fatalf("byte %d of the stage2 extent is 0x%02x (stale old content), want 0x00", i, region[i])
		}
	}

	// Untouched regions outside the stage2 extent must be unaffected -
	// starting at LBA 1 (byte 512), not LBA 0: fakeDiskWithReservation
	// itself writes a real MBR partition entry into LBA 0 as test
	// setup (see its own doc comment), so LBA 0's content isn't plain
	// fill in this test; LBA1-through-Stage2LBA still is, and WriteStage2
	// must never touch it either way.
	before, err := readRegion(disk, layout.SectorSize, layout.Stage2LBA*layout.SectorSize-layout.SectorSize)
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range before {
		if b != 0xAB {
			t.Fatalf("byte %d before the stage2 extent was touched (0x%02x, want 0xAB)", i, b)
		}
	}
}

func TestWriteStage2_RejectsOversized(t *testing.T) {
	disk := fakeDisk(t, (layout.Stage2LBA+layout.Stage2Sectors+10)*layout.SectorSize, 0)
	_, err := WriteStage2(disk, make([]byte, layout.Stage2Bytes+1))
	if err == nil {
		t.Fatal("WriteStage2 with too many bytes: want an error, got nil")
	}
}

func TestVerifyStage2_DetectsContentMismatch(t *testing.T) {
	disk := fakeDiskWithReservation(t, (layout.Stage2LBA+layout.Stage2Sectors+10)*layout.SectorSize, 0)
	stage2 := pattern(1000, 1)
	if _, err := WriteStage2(disk, stage2); err != nil {
		t.Fatal(err)
	}
	wrong := pattern(1000, 2)
	if err := VerifyStage2(disk, wrong); err == nil {
		t.Error("VerifyStage2 against mismatched content: want an error, got nil")
	}
}

func TestVerifyStage2_DetectsCorruptedPadding(t *testing.T) {
	disk := fakeDiskWithReservation(t, (layout.Stage2LBA+layout.Stage2Sectors+10)*layout.SectorSize, 0)
	stage2 := pattern(1000, 1)
	if _, err := WriteStage2(disk, stage2); err != nil {
		t.Fatal(err)
	}

	// Directly corrupt one byte in the padding region, bypassing
	// WriteStage2 entirely - simulates a stale leftover or an
	// external write, exactly what VerifyStage2 exists to catch.
	f, err := os.OpenFile(disk, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	corruptOffset := int64(layout.Stage2LBA)*layout.SectorSize + int64(len(stage2)) + 100
	if _, err := f.WriteAt([]byte{0x42}, corruptOffset); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := VerifyStage2(disk, stage2); err == nil {
		t.Error("VerifyStage2 with corrupted padding: want an error, got nil")
	}
}

func TestReadStage1(t *testing.T) {
	disk := fakeDiskWithReservation(t, 4096, 0xAB)
	stage1 := pattern(layout.Stage1Bytes, 3)
	if _, err := WriteStage1(disk, stage1); err != nil {
		t.Fatal(err)
	}
	got, err := ReadStage1(disk)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, stage1) {
		t.Errorf("ReadStage1 = %v, want %v", got, stage1)
	}
}

func TestReadStage2(t *testing.T) {
	disk := fakeDiskWithReservation(t, (layout.Stage2LBA+layout.Stage2Sectors+10)*layout.SectorSize, 0)
	stage2 := pattern(8529, 4)
	if _, err := WriteStage2(disk, stage2); err != nil {
		t.Fatal(err)
	}
	got, err := ReadStage2(disk)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, stage2) {
		t.Errorf("ReadStage2 = %d bytes, want %d bytes matching the written content", len(got), len(stage2))
	}
}

func TestReadStage2_EmptyExtent(t *testing.T) {
	disk := fakeDisk(t, (layout.Stage2LBA+layout.Stage2Sectors+10)*layout.SectorSize, 0)
	got, err := ReadStage2(disk)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("ReadStage2 on a never-written extent = %d bytes, want 0", len(got))
	}
}

func TestWriteStage2_RejectsDiskWithNoPartitionTableAtAll(t *testing.T) {
	// Plain all-zero content - no 0x55AA signature, so neither a GPT
	// nor an MBR layout can be determined at all. CheckStage2ExtentFree
	// can't safety-check what it can't parse, so this must still
	// refuse - NOT because a "reservation" is missing (a disk with a
	// real, valid, but empty MBR - the actual uniclus-01 shape - is
	// perfectly fine, see internal/bootenv's own
	// TestCheckStage2ExtentFree_MSDOS_NoPartitionsAtAll), but because
	// there's no readable partition table to confirm safety against
	// at all.
	disk := fakeDisk(t, (layout.Stage2LBA+layout.Stage2Sectors+10)*layout.SectorSize, 0)
	_, err := WriteStage2(disk, pattern(1000, 1))
	if err == nil {
		t.Fatal("WriteStage2 on a disk with no parseable partition table at all: want an error, got nil")
	}

	// Confirm nothing was actually written - a refused write must be
	// a true no-op, not "refused but partially applied".
	region, rerr := readRegion(disk, int64(layout.Stage2LBA)*layout.SectorSize, layout.Stage2Bytes)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for i, b := range region {
		if b != 0 {
			t.Fatalf("byte %d of the stage2 extent is 0x%02x after a REFUSED write, want 0x00 (untouched)", i, b)
		}
	}
}

func TestExtractStage2Version(t *testing.T) {
	data := []byte("junk-prefix-bytes...alpine-zfsboot-bios: alpine-zfsboot by UniDoc, version 0.1.0, build=2026-09-22T14:21:30Z\nmore-junk")
	version, buildID, err := ExtractStage2Version(data)
	if err != nil {
		t.Fatalf("ExtractStage2Version: %v", err)
	}
	if version != "0.1.0" {
		t.Errorf("version = %q, want %q", version, "0.1.0")
	}
	if buildID != "2026-09-22T14:21:30Z" {
		t.Errorf("buildID = %q, want %q", buildID, "2026-09-22T14:21:30Z")
	}
}

func TestExtractStage2Version_NoMatch(t *testing.T) {
	if _, _, err := ExtractStage2Version([]byte("totally unrelated bytes")); err == nil {
		t.Error("ExtractStage2Version with no anchor text present: want an error, got nil")
	}
}
