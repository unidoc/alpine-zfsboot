package metadata

import (
	"strings"
	"testing"
)

func sample() Manifest {
	return Manifest{
		Version:      "0.1.0",
		BuildStamp:   "20260923T150000Z",
		Arch:         "aarch64",
		Kernel:       "6.18.53-0-lts",
		OpenZFS:      "2.4.4-1",
		KernelSHA256: "aaaa111100000000000000000000000000000000000000000000000000000000",
		InitrdSHA256: "bbbb222200000000000000000000000000000000000000000000000000000000",
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	in := sample()
	got, err := Decode(Encode(in))
	if err != nil {
		t.Fatalf("Decode(Encode(in)): %v", err)
	}
	if got != in {
		t.Fatalf("round-trip mismatch:\n in=%#v\nout=%#v", in, got)
	}
}

func TestEncode_FormatFieldFirst(t *testing.T) {
	b := Encode(sample())
	if !strings.HasPrefix(string(b), "format=1\n") {
		t.Errorf("Encode output doesn't start with format=1: %q", string(b)[:30])
	}
}

func TestDecode_NULTerminatedPadding(t *testing.T) {
	// Simulates a PE-section-shaped read: real content, then NUL padding
	// (objcopy's own section-padding convention, see internal/cmdline's
	// firstLine handling of the same shape) - even though this manifest
	// is a plain ESP file today (see internal/layout.MetadataFile's own
	// doc comment for why), Decode still tolerates this shape since
	// nothing about the format itself assumes one particular storage
	// medium.
	b := Encode(sample())
	b = append(b, make([]byte, 16)...) // NUL padding
	got, err := Decode(b)
	if err != nil {
		t.Fatalf("Decode with NUL padding: %v", err)
	}
	if got != sample() {
		t.Errorf("Decode with NUL padding = %#v, want %#v", got, sample())
	}
}

func TestDecode_WrongFormat(t *testing.T) {
	_, err := Decode([]byte("format=99\nversion=0.1.0\n"))
	if err == nil {
		t.Fatal("Decode with an unrecognized format=: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "format") {
		t.Errorf("error = %q, want it to mention format", err.Error())
	}
}

func TestDecode_NoFormatField(t *testing.T) {
	_, err := Decode([]byte("version=0.1.0\nkernel=6.18.53-0-lts\n"))
	if err == nil {
		t.Fatal("Decode with no format= field at all: want an error, got nil")
	}
}

func TestDecode_MissingRequiredField(t *testing.T) {
	// A real manifest missing kernel_sha256 - e.g. a hand-edited or
	// truncated file.
	raw := []byte("format=1\nversion=0.1.0\nbuildstamp=X\narch=aarch64\nkernel=6.18.53-0-lts\nopenzfs=2.4.4-1\ninitrd_sha256=bbbb\n")
	_, err := Decode(raw)
	if err == nil {
		t.Fatal("Decode missing kernel_sha256: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "kernel_sha256") {
		t.Errorf("error = %q, want it to name the missing field", err.Error())
	}
}

func TestDecode_MalformedLine(t *testing.T) {
	_, err := Decode([]byte("format=1\nthis line has no equals sign\n"))
	if err == nil {
		t.Fatal("Decode with a malformed (no '=') line: want an error, got nil")
	}
}

func TestDecode_EmptyInput(t *testing.T) {
	_, err := Decode([]byte(""))
	if err == nil {
		t.Fatal("Decode on empty input: want an error, got nil")
	}
}

func TestSHA256Hex_KnownVector(t *testing.T) {
	// SHA-256("") - a standard, independently-verifiable test vector,
	// not just "round-trips with itself".
	got := SHA256Hex([]byte(""))
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Errorf("SHA256Hex(\"\") = %q, want %q", got, want)
	}
}

func TestVerifyHash(t *testing.T) {
	data := []byte("real kernel bytes go here")
	hash := SHA256Hex(data)

	if !VerifyHash(data, hash) {
		t.Error("VerifyHash with the correct hash: want true")
	}
	if !VerifyHash(data, strings.ToUpper(hash)) {
		t.Error("VerifyHash should be case-insensitive on the recorded hash")
	}
	if VerifyHash(data, "0000000000000000000000000000000000000000000000000000000000000") {
		t.Error("VerifyHash with a wrong hash: want false")
	}
	if VerifyHash([]byte("different bytes"), hash) {
		t.Error("VerifyHash against different data with the original hash: want false")
	}
}

// TestVerifyHash_DetectsTamperedPayload is the direct scenario this
// whole mechanism exists for: a manifest recorded against the real
// installed bytes, then one of those bytes changes (manual edit,
// corruption, a partial/interrupted write) - VerifyHash must catch it.
func TestVerifyHash_DetectsTamperedPayload(t *testing.T) {
	original := []byte("the real initrd bytes, byte for byte")
	recordedHash := SHA256Hex(original)

	tampered := make([]byte, len(original))
	copy(tampered, original)
	tampered[10] ^= 0xFF // flip one byte

	if VerifyHash(tampered, recordedHash) {
		t.Fatal("VerifyHash did not detect a single-byte change - the whole point of recording a hash")
	}
}

// TestDecode_GarbageAfterNULIsRefused is the regression test for a real
// gap a final pre-release audit found: Decode's own doc comment already
// promised that trailing bytes after a NUL which are not themselves
// more NUL padding are an error, and the code never checked - it
// truncated at the first NUL and silently decoded the prefix.
//
// Why it matters concretely: nothing else in the system would have
// caught it. tryReadMetadata/status trust whatever Decode returns, and
// verify's two SHA-256 fields bind the manifest to the KERNEL/INITRD
// bytes - they say nothing about the integrity of the manifest FILE
// itself. So a manifest with real content appended after a NUL (FAT
// slack left by a torn write, a partially-overwritten longer previous
// manifest, deliberate tampering) decoded "cleanly" and was then
// believed by every consumer.
//
// The paired positive control is TestDecode_NULTerminatedPadding above:
// genuine all-NUL padding must still decode fine, so this check cannot
// have been implemented by simply rejecting every NUL.
func TestDecode_GarbageAfterNULIsRefused(t *testing.T) {
	b := Encode(sample())
	b = append(b, 0, 0, 0)                          // plausible padding...
	b = append(b, []byte("kernel=6.0.0-EVIL\n")...) // ...and then real content
	b = append(b, make([]byte, 8)...)               // more padding after it

	if _, err := Decode(b); err == nil {
		t.Fatal("Decode with real content hidden after NUL padding: want an error, got nil - a corrupt/appended manifest was silently accepted from its prefix")
	}
}

// TestDecode_DuplicateKeyIsRefused is the regression test for hardening
// requested after the final pre-release audit: a duplicate key previously
// silently kept only the LAST occurrence (plain Go map assignment) - the
// same class of "quietly ignore something wrong" gap as the unchecked
// NUL-padding bug above, for a manifest that's supposed to be small and
// fully enumerated.
func TestDecode_DuplicateKeyIsRefused(t *testing.T) {
	raw := []byte("format=1\nversion=0.1.0\nversion=6.6.6-EVIL\nbuildstamp=X\narch=x86_64\nkernel=k\nopenzfs=z\nkernel_sha256=" +
		"aaaa111100000000000000000000000000000000000000000000000000000000\ninitrd_sha256=bbbb222200000000000000000000000000000000000000000000000000000000\n")
	_, err := Decode(raw)
	if err == nil {
		t.Fatal("Decode with a duplicate 'version' field: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error = %q, want it to say 'duplicate'", err.Error())
	}
}

// TestDecode_UnknownFieldIsRefused proves an unrecognized key is rejected
// outright rather than silently ignored - a small, versioned,
// fully-enumerated manifest has no legitimate reason to carry a field
// this package doesn't define for format=1.
func TestDecode_UnknownFieldIsRefused(t *testing.T) {
	b := Encode(sample())
	b = append(b, []byte("totally_unknown_field=surprise\n")...)
	_, err := Decode(b)
	if err == nil {
		t.Fatal("Decode with an unrecognized field: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error = %q, want it to say 'unknown'", err.Error())
	}
}

// TestDecode_HashFormatValidation proves both SHA-256 fields are checked
// for shape (exactly 64 lowercase hex characters), not merely "present
// and non-empty" - a hand-edited, truncated, or wrong-case hash is
// refused with a specific reason rather than simply never matching any
// real payload later, by accident rather than by a checked invariant.
func TestDecode_HashFormatValidation(t *testing.T) {
	base := sample()
	cases := []struct {
		name   string
		break_ func(m *Manifest)
	}{
		{"too_short", func(m *Manifest) { m.KernelSHA256 = "aaaa1111" }},
		{"too_long", func(m *Manifest) { m.KernelSHA256 = m.KernelSHA256 + "00" }},
		{"uppercase", func(m *Manifest) { m.KernelSHA256 = strings.ToUpper(m.KernelSHA256) }},
		{"non_hex", func(m *Manifest) {
			m.KernelSHA256 = "gggg111100000000000000000000000000000000000000000000000000000000"[:64]
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := base
			c.break_(&m)
			if _, err := Decode(Encode(m)); err == nil {
				t.Fatalf("Decode with a malformed kernel_sha256 (%s): want an error, got nil", c.name)
			}
		})
	}
}

// TestDecode_LegitimateHashesStillDecode is the negative-facing control
// for the hash-format check: real, correctly-shaped hashes (as
// metadata.SHA256Hex actually produces) must be entirely unaffected.
func TestDecode_LegitimateHashesStillDecode(t *testing.T) {
	m := sample()
	m.KernelSHA256 = SHA256Hex([]byte("real kernel bytes"))
	m.InitrdSHA256 = SHA256Hex([]byte("real initrd bytes"))
	got, err := Decode(Encode(m))
	if err != nil {
		t.Fatalf("Decode with legitimate SHA256Hex-produced hashes: %v", err)
	}
	if got != m {
		t.Errorf("got = %#v, want %#v", got, m)
	}
}
