// Package biosboot writes and verifies the two fixed-placement BIOS
// boot artifacts - stage1 (LBA 0) and stage2 (LBA layout.Stage2LBA) -
// directly on a disk device (or, in tests, a plain file - Go's
// os.File doesn't care which). Every write here is the Go port of
// this session's own update-stage2-only.sh, already proven on real
// uniclus-01 hardware: back up the region first, verify what's
// actually written afterward (not just trust the write call
// succeeded), and for stage2 specifically, zero the WHOLE reserved
// extent before writing so a smaller new stage2.bin never leaves a
// stale, larger previous build's trailing bytes behind (see WriteStage2's
// own comment - this is the exact "zero then write" fix requested and
// hand-verified against a real loop-backed file earlier this session).
//
// install and update both call these same functions - neither
// duplicates this logic, per this project's own "shared primitives,
// not parallel implementations" requirement.
package biosboot

import (
	"bytes"
	"fmt"
	"os"

	"github.com/unidoc/alpine-zfsboot/internal/bootenv"
	"github.com/unidoc/alpine-zfsboot/internal/layout"
)

// WriteStage1 writes stage1 (must be exactly layout.Stage1Bytes) to
// disk at LBA 0, byte offset 0 - deliberately never touches bytes
// 440-511 (the MBR's own disk ID, real partition table, and 0x55AA
// boot signature, owned by whatever partitioned the disk, not
// stage1). Returns the region's own previous content for the caller
// to persist as a rollback copy, same discipline as WriteStage2 below.
//
// Gated by the same bootenv.CheckStage2ExtentFree WriteStage2 already
// calls - not because it's "about" the stage2 extent (LBA 0 and the
// stage2 extent are unrelated byte ranges), but because a full source
// audit found WriteStage1 had NO safety gate of its own at all, unlike
// WriteStage2: it would write 440 bytes to offset 0 of whatever device
// path it was handed, including a whole-disk-formatted FAT filesystem
// (mkfs.vfat directly on a block device, no partition table - a real,
// common shape for USB media), silently corrupting that filesystem's
// own boot sector/BPB. CheckStage2ExtentFree's own precondition -
// "disk has either a valid GPT, or a valid (genuinely non-FAT, see
// looksLikeFATBootSector) MBR" - is exactly the property that also
// makes LBA 0 safe to overwrite as MBR bootstrap code: a valid GPT's
// own protective MBR occupies that region by convention, and a valid,
// non-FAT MBR's first 440 bytes ARE the bootstrap code region by
// definition. A disk failing that check is refused before stage1 is
// touched either way.
func WriteStage1(disk string, stage1 []byte) (previous []byte, err error) {
	if len(stage1) != layout.Stage1Bytes {
		return nil, fmt.Errorf("stage1 is %d bytes, want exactly %d", len(stage1), layout.Stage1Bytes)
	}

	if err := bootenv.CheckStage2ExtentFree(disk); err != nil {
		return nil, fmt.Errorf("refusing to write stage1: %w", err)
	}

	f, err := os.OpenFile(disk, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", disk, err)
	}
	defer f.Close()

	previous = make([]byte, layout.Stage1Bytes)
	if _, err := f.ReadAt(previous, 0); err != nil {
		return nil, fmt.Errorf("backing up the current stage1 region of %s: %w", disk, err)
	}

	if _, err := f.WriteAt(stage1, 0); err != nil {
		return previous, fmt.Errorf("writing stage1 to %s: %w", disk, err)
	}
	if err := f.Sync(); err != nil {
		return previous, fmt.Errorf("syncing %s after writing stage1: %w", disk, err)
	}
	return previous, nil
}

// VerifyStage1 re-reads disk's own stage1 region and compares it
// byte-for-byte against want.
func VerifyStage1(disk string, want []byte) error {
	if len(want) != layout.Stage1Bytes {
		return fmt.Errorf("want is %d bytes, expected exactly %d", len(want), layout.Stage1Bytes)
	}
	got, err := readRegion(disk, 0, layout.Stage1Bytes)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("stage1 region of %s does not match: %s", disk, firstDiff(got, want))
	}
	return nil
}

// WriteStage2 writes stage2 (must fit within layout.Stage2Bytes) to
// disk at layout.Stage2LBA, zeroing the ENTIRE reserved
// layout.Stage2Bytes-byte extent first (see this package's own doc
// comment for why), then writing stage2 into the front of that now-zero
// region, then reading the whole region back to confirm both the
// written bytes AND the zero padding tail are exactly right before
// returning success. Returns the region's own previous content (the
// FULL layout.Stage2Bytes, not just stage2's old length - a smaller
// new stage2 could otherwise lose part of a rollback-worthy larger
// previous one) for the caller to persist as a rollback copy.
func WriteStage2(disk string, stage2 []byte) (previous []byte, err error) {
	if len(stage2) > layout.Stage2Bytes {
		return nil, fmt.Errorf("stage2 is %d bytes, exceeds the %d-byte (%d-sector) budget stage1 reads", len(stage2), layout.Stage2Bytes, layout.Stage2Sectors)
	}

	// Fail-closed SAFETY gate: confirm nothing on disk's own partition
	// table currently occupies the stage2 extent before writing it -
	// deliberately inside WriteStage2 itself, not left to callers to
	// remember. This is NOT an ownership check (callers - cmd/tool's
	// install/update - already establish "whose disk this is" their
	// own way: install via an explicit operator-confirmed disk
	// argument, update via the already-proven canonical FAT/ESP
	// partition's own parent disk) - see
	// bootenv.CheckStage2ExtentFree's own doc comment for exactly what
	// this checks, why it's deliberately NOT keyed on any
	// specially-named/typed partition (an earlier version of this
	// check was, incorrectly - a real production system, this
	// project's own uniclus-01, has no such partition at all and would
	// have failed it), and why an exact-match "cosmetic" partition
	// entry (what alpine-install-zfs.sh's own partition_disk() still
	// creates on a fresh install) is not treated as a conflict.
	if err := bootenv.CheckStage2ExtentFree(disk); err != nil {
		return nil, fmt.Errorf("refusing to write stage2: %w", err)
	}

	f, err := os.OpenFile(disk, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", disk, err)
	}
	defer f.Close()

	offset := int64(layout.Stage2LBA) * layout.SectorSize

	previous = make([]byte, layout.Stage2Bytes)
	if _, err := f.ReadAt(previous, offset); err != nil {
		return nil, fmt.Errorf("backing up the current stage2 region of %s: %w", disk, err)
	}

	zero := make([]byte, layout.Stage2Bytes)
	if _, err := f.WriteAt(zero, offset); err != nil {
		return previous, fmt.Errorf("zeroing the stage2 region of %s: %w", disk, err)
	}
	if _, err := f.WriteAt(stage2, offset); err != nil {
		return previous, fmt.Errorf("writing stage2 to %s: %w", disk, err)
	}
	if err := f.Sync(); err != nil {
		return previous, fmt.Errorf("syncing %s after writing stage2: %w", disk, err)
	}

	if err := verifyStage2At(f, offset, stage2); err != nil {
		return previous, err
	}
	return previous, nil
}

// ReadStage1 returns disk's own current stage1 region (exactly
// layout.Stage1Bytes) - for status/inspection use, where there is no
// "expected" value to compare against yet (see VerifyStage1 for that
// case).
func ReadStage1(disk string) ([]byte, error) {
	return readRegion(disk, 0, layout.Stage1Bytes)
}

// ReadStage2 returns disk's own current stage2 extent with its
// trailing zero bytes trimmed off - an ADVISORY, approximate size for
// human-facing status output only ("Stage2: 8.4 KiB / 32 KiB"), NOT a
// verification primitive: a real stage2.bin that happens to end in a
// genuine 0x00 byte would be undercounted here, since this cannot
// distinguish "real content that is zero" from "WriteStage2's own
// padding" after the fact - only WriteStage2 itself knows that
// boundary for certain, at write time. Verification always uses
// VerifyStage2's own exact-length comparison against a real expected
// build, never this heuristic. A region that is entirely zero
// (nothing ever written) returns a zero-length slice, not an error.
func ReadStage2(disk string) ([]byte, error) {
	region, err := readRegion(disk, int64(layout.Stage2LBA)*layout.SectorSize, layout.Stage2Bytes)
	if err != nil {
		return nil, err
	}
	end := len(region)
	for end > 0 && region[end-1] == 0 {
		end--
	}
	return region[:end], nil
}

// VerifyStage2 re-reads disk's own stage2 region: the first
// len(want) bytes must equal want exactly, and every byte after that
// (up to the full layout.Stage2Bytes extent) must be zero - the same
// "stage2.bin, then zero padding to exactly Stage2Bytes" invariant
// WriteStage2 establishes.
func VerifyStage2(disk string, want []byte) error {
	if len(want) > layout.Stage2Bytes {
		return fmt.Errorf("want is %d bytes, exceeds the %d-byte budget", len(want), layout.Stage2Bytes)
	}
	f, err := os.Open(disk)
	if err != nil {
		return fmt.Errorf("opening %s: %w", disk, err)
	}
	defer f.Close()
	return verifyStage2At(f, int64(layout.Stage2LBA)*layout.SectorSize, want)
}

// verifyStage2At does the actual region+padding comparison against an
// already-open file at a known offset - shared by WriteStage2's own
// post-write check and the standalone VerifyStage2 above, so there is
// exactly one place that defines what "correct" means for this
// region.
func verifyStage2At(f *os.File, offset int64, want []byte) error {
	region := make([]byte, layout.Stage2Bytes)
	if _, err := f.ReadAt(region, offset); err != nil {
		return fmt.Errorf("reading back the stage2 region: %w", err)
	}
	if !bytes.Equal(region[:len(want)], want) {
		return fmt.Errorf("stage2 content does not match: %s", firstDiff(region[:len(want)], want))
	}
	padding := region[len(want):]
	for i, b := range padding {
		if b != 0 {
			return fmt.Errorf("stage2 extent padding is not zero: byte %d (region offset %d) is 0x%02x", i, len(want)+i, b)
		}
	}
	return nil
}

func readRegion(disk string, offset int64, length int) ([]byte, error) {
	f, err := os.Open(disk)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", disk, err)
	}
	defer f.Close()
	buf := make([]byte, length)
	if _, err := f.ReadAt(buf, offset); err != nil {
		return nil, fmt.Errorf("reading %s at offset %d: %w", disk, offset, err)
	}
	return buf, nil
}

// firstDiff describes the first byte at which got and want disagree -
// mirrors alpine-install-zfs.sh's own check_disk_write()/this
// session's own update-stage2-only.sh MISMATCH reporting (a plain
// "they differ" with no location is much harder to debug on real
// hardware than "byte 17 differs").
func firstDiff(got, want []byte) string {
	n := len(got)
	if len(want) < n {
		n = len(want)
	}
	for i := 0; i < n; i++ {
		if got[i] != want[i] {
			return fmt.Sprintf("first difference at byte %d: got 0x%02x, want 0x%02x", i, got[i], want[i])
		}
	}
	if len(got) != len(want) {
		return fmt.Sprintf("length differs: got %d bytes, want %d bytes", len(got), len(want))
	}
	return "(no difference found - internal error in comparison)"
}

// stage2VersionPrefix is the exact, stable anchor text
// bios/stage2_main.c's own first console_puts() call compiles into
// stage2.bin's .rodata as a plain, uncompressed ASCII string (see
// that file's own comment: printed unconditionally, before anything
// else can fail, specifically so the running binary can always say
// which build it is) - ExtractStage2Version finds this same string
// directly in the installed stage2.bin's raw bytes, the same
// technique internal/kernelinfo uses for vmlinuz's own embedded
// version field, just via a plain substring search instead of a
// fixed-offset pointer (stage2.bin has no equivalent boot-protocol
// convention to point at one).
const stage2VersionPrefix = "alpine-zfsboot by UniDoc, version "

// ExtractStage2Version finds and parses the "alpine-zfsboot by
// UniDoc, version X, build=Y" string embedded in stage2Data (as
// returned by ReadStage2) and returns (version, buildID). Returns an
// error if the anchor text isn't found at all (e.g. a stage2.bin
// built before this string existed, or a truncated/corrupt read) or
// if the "build=" marker or a trailing NUL/newline terminator is
// missing.
func ExtractStage2Version(stage2Data []byte) (version, buildID string, err error) {
	idx := bytes.Index(stage2Data, []byte(stage2VersionPrefix))
	if idx < 0 {
		return "", "", fmt.Errorf("stage2 has no embedded version string (looked for %q)", stage2VersionPrefix)
	}
	rest := stage2Data[idx+len(stage2VersionPrefix):]

	const buildMarker = ", build="
	buildIdx := bytes.Index(rest, []byte(buildMarker))
	if buildIdx < 0 {
		return "", "", fmt.Errorf("stage2's version string has no %q marker", buildMarker)
	}
	version = string(rest[:buildIdx])

	afterBuild := rest[buildIdx+len(buildMarker):]
	end := bytes.IndexAny(afterBuild, "\n\x00")
	if end < 0 {
		return "", "", fmt.Errorf("stage2's build id is not terminated within the read data")
	}
	buildID = string(afterBuild[:end])
	return version, buildID, nil
}
