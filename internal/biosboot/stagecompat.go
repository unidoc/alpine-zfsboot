package biosboot

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/unidoc/alpine-zfsboot/internal/layout"
)

// The stage2 header stage1 checks before jumping into it (bios/
// stage2_entry.S, bios/stage1.S - stagecompat_test.go greps both so these
// cannot drift):
//
//	offset 2: Stage2Magic (every stage2 since v0.2.0)
//	offset 4: Stage2SumMarker, then the image length in 16-bit words at
//	          offset 6 and a checksum word at offset 8 (stage2 builds that
//	          carry them; the 16-bit sum of the first length words is 0)
//
// A stage1 that knows the marker checks length and sum only when the
// marker is there; for an older stage2 it checks the magic word alone,
// like every older stage1. So every pairing of a v0.2.0+ stage1 with a
// v0.2.0+ stage2 boots, which is what keeps a half-finished update (or a
// mirror whose disks were updated one at a time) bootable.
const (
	Stage2Magic     = 0x325a
	Stage2SumMarker = 0x5331
)

// stage1MarkerCheck is the instruction a marker-aware stage1 uses
// (cmpw $Stage2SumMarker, 4): its presence is how Stage1Accepts tells a
// new stage1 from an older one.
var stage1MarkerCheck = []byte{0x81, 0x3e, 0x04, 0x00, Stage2SumMarker & 0xff, Stage2SumMarker >> 8}

// Stage1ChecksSum reports whether stage1 is a build that verifies the
// stage2 length/checksum (when the stage2 carries them).
func Stage1ChecksSum(stage1 []byte) bool {
	return bytes.Contains(stage1, stage1MarkerCheck)
}

// CheckStage2 applies the strictest stage1's rules to stage2 as stage1
// reads it (the whole layout.Stage2Bytes region, zero-padded the way
// WriteStage2 leaves it): nil when every stage1 since v0.2.0 would jump
// into it.
func CheckStage2(stage2 []byte) error {
	if len(stage2) < 4 || len(stage2) > layout.Stage2Bytes {
		return fmt.Errorf("stage2 is %d bytes, not 4..%d", len(stage2), layout.Stage2Bytes)
	}
	if binary.LittleEndian.Uint16(stage2[2:]) != Stage2Magic {
		return fmt.Errorf("stage2 has no %#04x magic word at offset 2 - stage1 would refuse it (halts with '!')", Stage2Magic)
	}
	if len(stage2) < 10 || binary.LittleEndian.Uint16(stage2[4:]) != Stage2SumMarker {
		return nil // an older stage2: magic word only
	}
	region := make([]byte, layout.Stage2Bytes)
	copy(region, stage2)
	words := int(binary.LittleEndian.Uint16(region[6:]))
	if words == 0 || words > layout.Stage2Bytes/2 {
		return fmt.Errorf("stage2 announces a length of %d words, not 1..%d - stage1 would refuse it", words, layout.Stage2Bytes/2)
	}
	var sum uint16
	for i := 0; i < words; i++ {
		sum += binary.LittleEndian.Uint16(region[2*i:])
	}
	if sum != 0 {
		return fmt.Errorf("stage2 checksum does not add up (sum %#04x) - corrupt or truncated image, stage1 would refuse it", sum)
	}
	return nil
}

// Stage1Accepts models whether this stage1 jumps into this stage2 (as
// read from disk: the stage2 region). Used to prove every intermediate
// state of an install/update boots.
func Stage1Accepts(stage1, stage2 []byte) bool {
	if len(stage2) < 4 || binary.LittleEndian.Uint16(stage2[2:]) != Stage2Magic {
		return false
	}
	if !Stage1ChecksSum(stage1) {
		return true
	}
	return CheckStage2(stage2) == nil
}
