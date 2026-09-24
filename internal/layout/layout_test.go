package layout

import (
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestStage2PlacementMatchesStage1S is the drift detector this
// package's own doc comment promises: Stage2LBA/Stage2Sectors above
// are a manually-kept mirror of bios/stage1.S's own `.set` directives
// (the actual, load-bearing source - stage1 is hand-written 16-bit
// assembly, not something this package can generate or #include from
// Go), so this test greps the real source file and fails loudly the
// moment the two disagree, instead of trusting a comment to stay
// accurate.
func TestStage2PlacementMatchesStage1S(t *testing.T) {
	data, err := os.ReadFile("../../bios/stage1.S")
	if err != nil {
		t.Fatalf("reading bios/stage1.S: %v", err)
	}

	lba := mustMatchInt(t, data, `\.set\s+STAGE2_LBA,\s*(\d+)`, "STAGE2_LBA")
	sectors := mustMatchInt(t, data, `\.set\s+STAGE2_SECTORS,\s*(\d+)`, "STAGE2_SECTORS")

	if lba != Stage2LBA {
		t.Errorf("bios/stage1.S's STAGE2_LBA is %d, but layout.Stage2LBA is %d - these must match exactly, update whichever is stale", lba, Stage2LBA)
	}
	if sectors != Stage2Sectors {
		t.Errorf("bios/stage1.S's STAGE2_SECTORS is %d, but layout.Stage2Sectors is %d - these must match exactly, update whichever is stale", sectors, Stage2Sectors)
	}
}

func mustMatchInt(t *testing.T, data []byte, pattern, name string) int {
	t.Helper()
	re := regexp.MustCompile(pattern)
	m := re.FindSubmatch(data)
	if m == nil {
		t.Fatalf("could not find %q in bios/stage1.S (pattern %s) - has its .set directive syntax changed?", name, pattern)
	}
	var n int
	if _, err := fmt.Sscanf(string(m[1]), "%d", &n); err != nil {
		t.Fatalf("parsing %q value %q: %v", name, m[1], err)
	}
	return n
}

func TestStage2BytesIsSectorsTimesSectorSize(t *testing.T) {
	if Stage2Bytes != Stage2Sectors*SectorSize {
		t.Errorf("Stage2Bytes (%d) != Stage2Sectors*SectorSize (%d)", Stage2Bytes, Stage2Sectors*SectorSize)
	}
}

func TestEFILoaderName(t *testing.T) {
	cases := map[string]string{
		"x86_64":  "BOOTX64.EFI",
		"aarch64": "BOOTAA64.EFI",
		"riscv64": "",
	}
	for arch, want := range cases {
		if got := EFILoaderName(arch); got != want {
			t.Errorf("EFILoaderName(%q) = %q, want %q", arch, got, want)
		}
	}
}

// guidStringToMixedEndianBytes independently parses a standard
// RFC4122-style GUID string into GPT's own on-disk mixed-endian byte
// order (first three fields byte-reversed, last two as-is) - a
// second, from-scratch implementation of the same rule
// BIOSBootPartitionGUIDBytes/ESPTypeGUIDBytes were hand-derived
// against, so a shared transcription mistake in both wouldn't be
// hidden by comparing one against the other.
func guidStringToMixedEndianBytes(t *testing.T, s string) [16]byte {
	t.Helper()
	parts := strings.Split(s, "-")
	if len(parts) != 5 {
		t.Fatalf("not a GUID string: %q", s)
	}
	hexBytes := func(h string) []byte {
		b, err := hex.DecodeString(h)
		if err != nil {
			t.Fatalf("decoding %q: %v", h, err)
		}
		return b
	}
	reversed := func(b []byte) []byte {
		out := make([]byte, len(b))
		for i, v := range b {
			out[len(b)-1-i] = v
		}
		return out
	}

	var out [16]byte
	copy(out[0:4], reversed(hexBytes(parts[0])))
	copy(out[4:6], reversed(hexBytes(parts[1])))
	copy(out[6:8], reversed(hexBytes(parts[2])))
	copy(out[8:10], hexBytes(parts[3]))
	copy(out[10:16], hexBytes(parts[4]))
	return out
}

func TestBIOSBootPartitionGUIDBytes(t *testing.T) {
	want := guidStringToMixedEndianBytes(t, "21686148-6449-6E6F-744E-656564454649")
	if BIOSBootPartitionGUIDBytes != want {
		t.Errorf("BIOSBootPartitionGUIDBytes = %x, want %x (independently derived from the string form)", BIOSBootPartitionGUIDBytes, want)
	}
}

func TestESPTypeGUIDBytes(t *testing.T) {
	want := guidStringToMixedEndianBytes(t, ESPTypeGUIDString)
	if ESPTypeGUIDBytes != want {
		t.Errorf("ESPTypeGUIDBytes = %x, want %x (independently derived from ESPTypeGUIDString)", ESPTypeGUIDBytes, want)
	}
}
