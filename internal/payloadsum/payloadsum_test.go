package payloadsum

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
)

// goldenKernel/goldenInitrd are the inputs testdata/golden.sum describes.
// bios/tests/payload_sum_host_test.c builds the same bytes and checks that
// the C parser reads the same file and the C SHA-256 gets the same digests,
// so the writer (this package) and the reader (stage2) cannot drift apart.
func goldenKernel() []byte {
	k := make([]byte, 8192)
	for i := range k {
		k[i] = byte(i*37 + 11)
	}
	k[offSetupSects] = 3 // real-mode part = (3+1)*512 = 2048 bytes
	k[offBootFlag], k[offBootFlag+1] = 0x55, 0xAA
	copy(k[offHeader:], "HdrS")
	return k
}

func goldenInitrd() []byte {
	b := make([]byte, 5000)
	for i := range b {
		b[i] = byte(i*13 + 3)
	}
	return b
}

func TestGenerateMatchesGolden(t *testing.T) {
	want, err := os.ReadFile("testdata/golden.sum")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Generate(goldenKernel(), goldenInitrd(), ModeWarn)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Generate output differs from testdata/golden.sum (which the C reader is tested against):\n got: %q\nwant: %q", got, want)
	}
	if len(got) > MaxBytes {
		t.Fatalf("manifest is %d bytes, stage2 reads at most %d", len(got), MaxBytes)
	}
}

func TestDecodeRoundTripAndVerify(t *testing.T) {
	k, i := goldenKernel(), goldenInitrd()
	raw, err := Generate(k, i, ModeEnforce)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.Mode != ModeEnforce {
		t.Fatalf("Generate(..., enforce) round-tripped as mode %q", m.Mode)
	}
	if _, err := Generate(k, i, "strict"); err == nil {
		t.Fatal("an unknown mode: want an error")
	}
	if m.Kernel.Offset != 2048 || m.Kernel.Size != 8192 || m.Initrd.Offset != 0 || m.Initrd.Size != 5000 {
		t.Fatalf("decoded %+v", m)
	}
	if errs := Verify(m, k, i); len(errs) != 0 {
		t.Fatalf("Verify on the exact bytes: %v", errs)
	}

	// One flipped byte in the loaded part of the kernel.
	bad := append([]byte(nil), k...)
	bad[5000] ^= 0x01
	if errs := Verify(m, bad, i); len(errs) != 1 || !strings.Contains(errs[0].Error(), "KERNEL SHA-256") {
		t.Fatalf("a corrupted kernel byte: want one KERNEL digest error, got %v", errs)
	}
	// A changed byte in the real-mode part is not loaded by stage2 and not covered.
	rm := append([]byte(nil), k...)
	rm[100] ^= 0xff
	if errs := Verify(m, rm, i); len(errs) != 0 {
		t.Fatalf("a change in the never-loaded real-mode part: want no error, got %v", errs)
	}
	// Wrong size.
	if errs := Verify(m, k, i[:4999]); len(errs) != 1 || !strings.Contains(errs[0].Error(), "INITRD is 4999 bytes") {
		t.Fatalf("a short initrd: want one size error, got %v", errs)
	}
	// A kernel whose real-mode part moved.
	moved := append([]byte(nil), k...)
	moved[offSetupSects] = 5
	if errs := Verify(m, moved, i); len(errs) != 1 || !strings.Contains(errs[0].Error(), "offset") {
		t.Fatalf("a different setup_sects: want one offset error, got %v", errs)
	}
}

func TestSetupSectsZeroMeansFour(t *testing.T) {
	k := goldenKernel()
	k[offSetupSects] = 0
	off, err := KernelRAMOffset(k)
	if err != nil || off != 5*512 {
		t.Fatalf("setup_sects=0: got %d, %v, want 2560", off, err)
	}
}

func TestNotABzImage(t *testing.T) {
	if _, err := Generate([]byte("Linux version 6.18 (build@host)\n"), goldenInitrd(), ModeWarn); err == nil {
		t.Fatal("a non-bzImage kernel: want an error")
	}
	k := goldenKernel()[:2048] // ends exactly at its real-mode part
	if _, err := Generate(k, goldenInitrd(), ModeWarn); err == nil {
		t.Fatal("a kernel with no protected-mode part: want an error")
	}
}

func TestDecodeRejects(t *testing.T) {
	good, err := os.ReadFile("testdata/golden.sum")
	if err != nil {
		t.Fatal(err)
	}
	g := string(good)
	lines := strings.SplitAfter(g, "\n")
	lines = append(lines[:1], lines[2:]...) // without the MODE line: header, KERNEL, INITRD
	for name, raw := range map[string]string{
		"no trailing newline":  strings.TrimSuffix(g, "\n"),
		"wrong header":         strings.Replace(g, "payload-sum 1", "payload-sum 2", 1),
		"uppercase hex":        lines[0] + strings.ToUpper(lines[1][:len(lines[1])-1]) + "\n" + lines[2],
		"missing INITRD":       lines[0] + lines[1],
		"duplicate KERNEL":     lines[0] + lines[1] + lines[1] + lines[2],
		"offset beyond size":   strings.Replace(g, "KERNEL 8192 2048", "KERNEL 8192 9000", 1),
		"double space":         strings.Replace(g, "KERNEL 8192", "KERNEL  8192", 1),
		"short digest":         regexp.MustCompile(`[0-9a-f]{2}\n`).ReplaceAllString(g, "\n"),
		"size overflows 32bit": strings.Replace(g, "INITRD 5000", "INITRD 4294967296", 1),
	} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Errorf("%s: Decode accepted %q", name, raw)
		}
	}
	// No MODE line, or an unknown one: warn - never enforce.
	for raw, want := range map[string]string{
		lines[0] + lines[1] + lines[2]:                    ModeWarn,
		lines[0] + "MODE warn\n" + lines[1] + lines[2]:    ModeWarn,
		lines[0] + "MODE Enforce\n" + lines[1] + lines[2]: ModeWarn,
		lines[0] + "MODE enforce\n" + lines[1] + lines[2]: ModeEnforce,
		lines[0] + "MODE off\n" + lines[1] + lines[2]:     ModeOff,
	} {
		if m, err := Decode([]byte(raw)); err != nil || m.Mode != want {
			t.Errorf("%q: want mode %q, got %q (err %v)", raw, want, m.Mode, err)
		}
	}
	// Lines naming other files are ignored (room for later additions).
	if _, err := Decode([]byte(lines[0] + "CMDLINE 1 0 x\n" + lines[1] + lines[2])); err != nil {
		t.Errorf("an extra, unknown line: %v", err)
	}
}

// TestStage2ReadsTheSamePath keeps the Go writer's path and the C reader's
// in step (layout.PayloadSumFile is checked against this in cmd/tool).
func TestStage2ReadsTheSamePath(t *testing.T) {
	h, err := os.ReadFile("../../bios/payload_sum.h")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(h), `#define PAYLOAD_SUM_PATH "/EFI/ALPINE/CHECKSUM"`) {
		t.Fatal("bios/payload_sum.h no longer reads /EFI/ALPINE/CHECKSUM - update layout.PayloadSumFile and this test together")
	}
	if !strings.Contains(string(h), "#define PAYLOAD_SUM_MAX_BYTES 512") || MaxBytes != 512 {
		t.Fatal("PAYLOAD_SUM_MAX_BYTES and MaxBytes disagree")
	}
}
