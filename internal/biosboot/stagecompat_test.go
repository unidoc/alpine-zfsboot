package biosboot

import (
	"encoding/binary"
	"fmt"
	"os"
	"regexp"
	"testing"
)

// The constants mirror bios/stage1.S and bios/stage2_entry.S.
func TestStageHeaderConstantsMatchAssembly(t *testing.T) {
	s1, err := os.ReadFile("../../bios/stage1.S")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int{"STAGE2_MAGIC": Stage2Magic, "STAGE2_SUM_MARKER": Stage2SumMarker} {
		m := regexp.MustCompile(`\.set\s+` + name + `,\s*(0x[0-9a-fA-F]+)`).FindSubmatch(s1)
		if m == nil || string(m[1]) != fmt.Sprintf("%#04x", want) {
			t.Errorf("bios/stage1.S %s = %q, Go has %#04x", name, m, want)
		}
	}
	e, err := os.ReadFile("../../bios/stage2_entry.S")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?s)\.word 0x325a\s*\n.*?\.word 0x5331`).Match(e) {
		t.Error("bios/stage2_entry.S no longer has .word 0x325a followed by .word 0x5331")
	}
}

// fakeStage2 builds a stage2 image: old = magic only (code at offset 4, like
// v0.2.0-v0.4.x), new = marker + length + checksum like stage2-sum.sh makes.
func fakeStage2(newHeader bool, size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i*7 + 1)
	}
	b[0], b[1] = 0xeb, 0x02
	binary.LittleEndian.PutUint16(b[2:], Stage2Magic)
	if !newHeader {
		b[4] = 0xea // v0.4.1's ljmp
		return b
	}
	b[1] = 0x08
	binary.LittleEndian.PutUint16(b[4:], Stage2SumMarker)
	binary.LittleEndian.PutUint16(b[6:], uint16(size/2))
	binary.LittleEndian.PutUint16(b[8:], 0)
	var sum uint16
	for i := 0; i < size/2; i++ {
		sum += binary.LittleEndian.Uint16(b[2*i:])
	}
	binary.LittleEndian.PutUint16(b[8:], -sum)
	return b
}

func fakeStage1(new bool) []byte {
	b := make([]byte, 440)
	if new {
		copy(b[0x61:], stage1MarkerCheck)
	}
	return b
}

func TestStage1AcceptsMatrix(t *testing.T) {
	old2, new2 := fakeStage2(false, 10688), fakeStage2(true, 20634)
	bad2 := append([]byte(nil), new2...)
	bad2[len(bad2)-100] ^= 1
	noMagic := append([]byte(nil), old2...)
	noMagic[2] = 0
	for _, c := range []struct {
		name       string
		s1new      bool
		s2         []byte
		wantAccept bool
	}{
		{"old stage1 + old stage2", false, old2, true},
		{"old stage1 + new stage2", false, new2, true},
		{"new stage1 + old stage2", true, old2, true},
		{"new stage1 + new stage2", true, new2, true},
		{"new stage1 + corrupt new stage2", true, bad2, false},
		{"old stage1 + corrupt new stage2 (old stage1 cannot tell)", false, bad2, true},
		{"new stage1 + stage2 without magic", true, noMagic, false},
	} {
		if got := Stage1Accepts(fakeStage1(c.s1new), c.s2); got != c.wantAccept {
			t.Errorf("%s: Stage1Accepts = %v, want %v", c.name, got, c.wantAccept)
		}
	}
	if CheckStage2(old2) != nil || CheckStage2(new2) != nil || CheckStage2(bad2) == nil || CheckStage2(noMagic) == nil {
		t.Error("CheckStage2 does not match the matrix above")
	}
}

// The real build output, when it exists (make -C bios stage1.bin stage2.bin).
func TestBuiltStagesAccepted(t *testing.T) {
	s1, err1 := os.ReadFile("../../bios/stage1.bin")
	s2, err2 := os.ReadFile("../../bios/stage2.bin")
	if err1 != nil || err2 != nil {
		t.Skip("bios/stage1.bin/stage2.bin not built")
	}
	if !Stage1ChecksSum(s1) {
		t.Error("the built stage1 is not recognised as a checksum-checking stage1 (stage1MarkerCheck out of step with stage1.S?)")
	}
	if err := CheckStage2(s2); err != nil {
		t.Errorf("the built stage2: %v", err)
	}
}
