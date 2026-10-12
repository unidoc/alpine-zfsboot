package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/unidoc/alpine-zfsboot/internal/biosboot"
	"github.com/unidoc/alpine-zfsboot/internal/layout"
)

// stage images shaped like the real ones: an "old" stage2 (v0.2.0-v0.4.x:
// magic word only) and a "new" one (marker + length + checksum), an old
// stage1 (magic check only) and a new one (contains the marker check).
func testStage2(newHeader bool, size int, seed byte) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(int(seed) + i*7)
	}
	b[0], b[1] = 0xeb, 0x02
	binary.LittleEndian.PutUint16(b[2:], biosboot.Stage2Magic)
	if !newHeader {
		b[4] = 0xea
		return b
	}
	b[1] = 0x08
	binary.LittleEndian.PutUint16(b[4:], biosboot.Stage2SumMarker)
	binary.LittleEndian.PutUint16(b[6:], uint16(size/2))
	binary.LittleEndian.PutUint16(b[8:], 0)
	var sum uint16
	for i := 0; i < size/2; i++ {
		sum += binary.LittleEndian.Uint16(b[2*i:])
	}
	binary.LittleEndian.PutUint16(b[8:], -sum)
	return b
}

func testStage1(newStage1 bool, seed byte) []byte {
	b := bytes.Repeat([]byte{seed}, layout.Stage1Bytes)
	if newStage1 {
		copy(b[0x61:], []byte{0x81, 0x3e, 0x04, 0x00, biosboot.Stage2SumMarker & 0xff, biosboot.Stage2SumMarker >> 8})
	}
	return b
}

// testBIOSDisk is a disk image with a valid MBR whose only partition is
// the cosmetic stage2 reservation (what WriteStage2's safety gate accepts).
func testBIOSDisk(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "disk.img")
	b := make([]byte, 1<<20)
	b[510], b[511] = 0x55, 0xAA
	b[446+4] = layout.Stage2MSDOSType
	binary.LittleEndian.PutUint32(b[446+8:], uint32(layout.Stage2LBA))
	binary.LittleEndian.PutUint32(b[446+12:], uint32(layout.Stage2Sectors))
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// bootable reads the disk's stage1/stage2 regions and applies stage1's rules.
func bootable(t *testing.T, disk string) bool {
	t.Helper()
	s1, err := biosboot.ReadStage1(disk)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(disk)
	if err != nil {
		t.Fatal(err)
	}
	off := layout.Stage2LBA * layout.SectorSize
	return biosboot.Stage1Accepts(s1, raw[off:off+layout.Stage2Bytes])
}

// recordWrites replaces writeStage1/writeStage2 for one test: every write
// is recorded and, right after it, the disk's bootability is checked -
// i.e. a crash after any single step leaves a bootable disk. failStage1
// makes the first stage1 write fail (after it was written, the worst
// case), to exercise the rollback path the same way.
func recordWrites(t *testing.T, failStage1 bool) *[]string {
	t.Helper()
	var steps []string
	orig1, orig2 := writeStage1, writeStage2
	t.Cleanup(func() { writeStage1, writeStage2 = orig1, orig2 })
	failed := false
	writeStage1 = func(disk string, b []byte) ([]byte, error) {
		prev, err := orig1(disk, b)
		steps = append(steps, "stage1")
		if !bootable(t, disk) {
			t.Errorf("not bootable after step %d (%v)", len(steps), steps)
		}
		if err == nil && failStage1 && !failed {
			failed = true
			return prev, errors.New("injected failure after writing stage1")
		}
		return prev, err
	}
	writeStage2 = func(disk string, b []byte) ([]byte, error) {
		prev, err := orig2(disk, b)
		steps = append(steps, "stage2")
		if !bootable(t, disk) {
			t.Errorf("not bootable after step %d (%v)", len(steps), steps)
		}
		return prev, err
	}
	return &steps
}

func seedStages(t *testing.T, disk string, s1, s2 []byte) {
	t.Helper()
	if _, err := biosboot.WriteStage2(disk, s2); err != nil {
		t.Fatal(err)
	}
	if _, err := biosboot.WriteStage1(disk, s1); err != nil {
		t.Fatal(err)
	}
	if !bootable(t, disk) {
		t.Fatal("the seeded disk itself is not bootable")
	}
}

func TestWriteBIOSStages_EveryIntermediateStateBoots(t *testing.T) {
	for _, c := range []struct {
		name       string
		from1, to1 bool // new stage1?
		from2, to2 bool // new stage2?
	}{
		{"old install -> new release (upgrade)", false, true, false, true},
		{"new release -> old release (downgrade)", true, false, true, false},
		{"new -> new", true, true, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			disk := testBIOSDisk(t)
			seedStages(t, disk, testStage1(c.from1, 0x11), testStage2(c.from2, 10688, 1))
			steps := recordWrites(t, false)
			if err := writeBIOSStagesWithRollback(disk, testStage1(c.to1, 0x22), testStage2(c.to2, 20634, 2)); err != nil {
				t.Fatal(err)
			}
			if len(*steps) != 2 || (*steps)[0] != "stage2" || (*steps)[1] != "stage1" {
				t.Fatalf("write order = %v, want [stage2 stage1]", *steps)
			}
		})
	}
}

func TestWriteBIOSStages_FailedStage1RollsBackBootably(t *testing.T) {
	disk := testBIOSDisk(t)
	old1, old2 := testStage1(false, 0x11), testStage2(false, 10688, 1)
	seedStages(t, disk, old1, old2)
	steps := recordWrites(t, true)
	if err := writeBIOSStagesWithRollback(disk, testStage1(true, 0x22), testStage2(true, 20634, 2)); err == nil {
		t.Fatal("want the injected stage1 failure")
	}
	// stage2, stage1 (fails), rollback stage1, rollback stage2.
	if want := []string{"stage2", "stage1", "stage1", "stage2"}; len(*steps) != len(want) {
		t.Fatalf("steps = %v, want %v", *steps, want)
	}
	if got, _ := biosboot.ReadStage1(disk); !bytes.Equal(got, old1) {
		t.Fatal("stage1 is not the original after rollback")
	}
	if err := biosboot.VerifyStage2(disk, old2); err != nil {
		t.Fatalf("stage2 is not the original after rollback: %v", err)
	}
}

func TestCheckStagePair(t *testing.T) {
	good := testStage2(true, 20634, 3)
	bad := append([]byte(nil), good...)
	bad[len(bad)-50] ^= 1
	if err := checkStagePair(good, false); err != nil {
		t.Fatalf("a good stage2: %v", err)
	}
	if err := checkStagePair(testStage2(false, 10688, 4), false); err != nil {
		t.Fatalf("an older-format stage2 (magic only): %v", err)
	}
	if err := checkStagePair(bad, false); err == nil {
		t.Fatal("a stage2 whose checksum does not add up: want a refusal before any write")
	}
	if err := checkStagePair(bad, true); err != nil {
		t.Fatalf("--force: %v", err)
	}
}
