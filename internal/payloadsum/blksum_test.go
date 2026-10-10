package payloadsum

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"regexp"
	"strings"
	"testing"
)

// goldenHex renders a binary golden file as text (32 bytes per line, hex),
// so it can live in the repo and in a plain diff.
func goldenHex(b []byte) []byte {
	var out bytes.Buffer
	for i := 0; i < len(b); i += 32 {
		end := i + 32
		if end > len(b) {
			end = len(b)
		}
		out.WriteString(hex.EncodeToString(b[i:end]) + "\n")
	}
	return out.Bytes()
}

// The one golden BLKSUM, for goldenKernel()/goldenInitrd(), stored as hex
// text. The C writer (bios/tests/blksum_writer.h) is checked against the
// same file by bios/tests/blkverify_host_test.c. Regenerate with
// UPDATE_GOLDEN=1.
func TestGenerateBlkSumMatchesGolden(t *testing.T) {
	raw, err := GenerateBlkSum(goldenKernel(), goldenInitrd())
	if err != nil {
		t.Fatal(err)
	}
	got := goldenHex(raw)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile("testdata/golden.blksum.hex", got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile("testdata/golden.blksum.hex")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("GenerateBlkSum differs from testdata/golden.blksum.hex (which the C side is tested against)")
	}
}

// sha256Blocks against crypto/sha256: the chain over whole blocks, then the
// standard padding by hand, must give the standard digest.
func TestSha256BlocksMatchesStdlib(t *testing.T) {
	for _, n := range []int{0, 64, 128, 65536, 3 * 65536} {
		p := make([]byte, n)
		for i := range p {
			p[i] = byte(i*7 + 1)
		}
		h := [8]uint32{0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19}
		sha256Blocks(&h, p)
		pad := []byte{0x80}
		pad = append(pad, make([]byte, 55)...)
		pad = binary.BigEndian.AppendUint64(pad, uint64(n)*8)
		sha256Blocks(&h, pad)
		var got [32]byte
		for k := 0; k < 8; k++ {
			binary.BigEndian.PutUint32(got[4*k:], h[k])
		}
		if got != sha256.Sum256(p) {
			t.Fatalf("n=%d: chain + padding != crypto/sha256", n)
		}
	}
}

func TestBlkSumRoundTripAndVerify(t *testing.T) {
	k := make([]byte, 300000)
	for i := range k {
		k[i] = byte(i * 3)
	}
	k[offSetupSects] = 3
	k[offBootFlag], k[offBootFlag+1] = 0x55, 0xAA
	copy(k[offHeader:], "HdrS")
	in := make([]byte, 20*65536+17)
	for i := range in {
		in[i] = byte(i * 5)
	}
	raw, err := GenerateBlkSum(k, in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := DecodeBlkSum(raw)
	if err != nil {
		t.Fatal(err)
	}
	if b.Kernel.Blocks != 5 || b.Initrd.Blocks != 21 || b.Initrd.First != 2 || len(b.Initrd.Entries) != 21 {
		t.Fatalf("decoded %+v / %d blocks", b.Kernel.Blocks, b.Initrd.Blocks)
	}
	m, _ := New(k, in, ModeWarn)
	if b.Initrd.Entries[20] != m.Initrd.SHA256 {
		t.Fatal("the last entry is not the whole-file SHA-256")
	}
	if errs := VerifyBlkSum(b, m, k, in); len(errs) != 0 {
		t.Fatalf("exact payload: %v", errs)
	}
	bad := append([]byte(nil), in...)
	bad[7*65536+100] ^= 1
	errs := VerifyBlkSum(b, m, k, bad)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "INITRD block 7 ") {
		t.Fatalf("one flipped byte in block 7: %v", errs)
	}
	m2, _ := New(k, bad, ModeWarn)
	if errs := VerifyBlkSum(b, m2, k, bad); len(errs) != 1 || !strings.Contains(errs[0].Error(), "stale BLKSUM") {
		t.Fatalf("BLKSUM from another payload: %v", errs)
	}
	// Each sector checks itself.
	c := append([]byte(nil), raw...)
	c[512+40] ^= 1
	if _, err := DecodeBlkSum(c); err == nil {
		t.Fatal("a flipped bit in a table sector: want an error")
	}
	if _, err := DecodeBlkSum(raw[:len(raw)-1]); err == nil {
		t.Fatal("a truncated file: want an error")
	}
}

// The constants agree with bios/blkverify.h.
func TestBlkSumConstantsMatchC(t *testing.T) {
	h, err := os.ReadFile("../../bios/blkverify.h")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"BLKSUM_VERSION": "1", "BLKSUM_BLOCK": "65536u", "BLKSUM_PER_SECTOR": "15u", "BLKSUM_PATH": `"/EFI/ALPINE/BLKSUM"`,
	} {
		m := regexp.MustCompile(`#define ` + name + `\s+(\S+)`).FindSubmatch(h)
		if m == nil || string(m[1]) != want {
			t.Errorf("bios/blkverify.h %s = %q, want %s", name, m, want)
		}
	}
}
