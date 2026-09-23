package kernelinfo

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// buildFakeBzImage hand-assembles a minimal, byte-correct bzImage
// header (boot_flag, "HdrS" magic, protocol version, kernel_version
// pointer) plus the version string itself at a real, computed offset
// - a from-scratch fixture, not a round-trip through this package's
// own reader.
func buildFakeBzImage(t *testing.T, versionString string) string {
	t.Helper()
	const versionAbsOffset = 0x400

	buf := make([]byte, versionAbsOffset+len(versionString)+1)
	buf[offBootFlag], buf[offBootFlag+1] = 0x55, 0xAA
	copy(buf[offHeaderMagic:offHeaderMagic+4], []byte("HdrS"))
	buf[offVersion], buf[offVersion+1] = 0x0c, 0x02 // protocol version 0x020c, little-endian

	relOff := uint16(versionAbsOffset - 0x200)
	buf[offKernelVersion] = byte(relOff)
	buf[offKernelVersion+1] = byte(relOff >> 8)

	copy(buf[versionAbsOffset:], versionString)
	buf[versionAbsOffset+len(versionString)] = 0 // NUL terminator

	path := filepath.Join(t.TempDir(), "vmlinuz")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVersion_RealHeader(t *testing.T) {
	want := "6.18.52-0-lts (buildd@build.alpinelinux.org) #1-Alpine SMP PREEMPT_DYNAMIC"
	path := buildFakeBzImage(t, want)

	got, err := Version(path)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got != want {
		t.Errorf("Version = %q, want %q", got, want)
	}
}

func TestVersion_NoBootFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notakernel")
	if err := os.WriteFile(path, make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Version(path); err == nil {
		t.Error("Version on a file with no 0xAA55 boot flag: want an error, got nil")
	}
}

func TestVersion_NoHdrSMagic(t *testing.T) {
	buf := make([]byte, 2048)
	buf[offBootFlag], buf[offBootFlag+1] = 0x55, 0xAA // boot flag present, but no HdrS
	path := filepath.Join(t.TempDir(), "vmlinuz")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Version(path); err == nil {
		t.Error("Version with a boot flag but no HdrS magic: want an error, got nil")
	}
}

func TestVersion_ZeroKernelVersionField(t *testing.T) {
	buf := make([]byte, 2048)
	buf[offBootFlag], buf[offBootFlag+1] = 0x55, 0xAA
	copy(buf[offHeaderMagic:offHeaderMagic+4], []byte("HdrS"))
	buf[offVersion], buf[offVersion+1] = 0x0c, 0x02
	// offKernelVersion left as zero - a real, if old/unusual, kernel build.
	path := filepath.Join(t.TempDir(), "vmlinuz")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Version(path); err == nil {
		t.Error("Version with a zero kernel_version field: want an error, got nil")
	}
}

func TestShortVersion(t *testing.T) {
	cases := map[string]string{
		"6.18.52-0-lts (buildd@build) #1-Alpine SMP": "6.18.52-0-lts",
		"6.18.52-0-lts": "6.18.52-0-lts",
		"":              "",
	}
	for in, want := range cases {
		if got := ShortVersion(in); got != want {
			t.Errorf("ShortVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestVersionFromLinuxBanner is the regression test for a real gap
// found in a full source audit: kernel-version introspection was
// entirely x86-bzImage-specific with no ARM64 equivalent, so it was
// permanently UNKNOWN on aarch64 - this project's own primary real
// hardware target (Hetzner CAX). See VersionFromLinuxBanner's own doc
// comment for what this technique assumes and why the underlying
// assumption (a real ARM64 Image is uncompressed) still needs
// real-hardware confirmation - this test only proves the SCAN logic
// itself is correct against synthetic data shaped like a real banner.
func TestVersionFromLinuxBanner(t *testing.T) {
	t.Run("finds a real-shaped banner surrounded by binary noise", func(t *testing.T) {
		data := append([]byte{0x4d, 0x5a, 0x00, 0x01, 0x02, 0x03},
			[]byte("Linux version 6.6.30-0-lts (buildd@build) (gcc 13.2.0) #1-Alpine SMP PREEMPT_DYNAMIC\nsome other binary junk after")...)
		got, err := VersionFromLinuxBanner(data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "Linux version 6.6.30-0-lts (buildd@build) (gcc 13.2.0) #1-Alpine SMP PREEMPT_DYNAMIC"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("stops at a NUL terminator too, not just newline", func(t *testing.T) {
		data := append([]byte("Linux version 6.6.30-0-lts\x00"), []byte("unrelated trailing bytes")...)
		got, err := VersionFromLinuxBanner(data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "Linux version 6.6.30-0-lts" {
			t.Errorf("got %q, want %q", got, "Linux version 6.6.30-0-lts")
		}
	})

	t.Run("no banner string at all is a clean error, not a panic", func(t *testing.T) {
		if _, err := VersionFromLinuxBanner([]byte{0x01, 0x02, 0x03}); err == nil {
			t.Error("want an error for data with no banner string, got nil")
		}
	})

	t.Run("banner with no terminator within the data is a clean error", func(t *testing.T) {
		if _, err := VersionFromLinuxBanner([]byte("Linux version 6.6.30-0-lts")); err == nil {
			t.Error("want an error for an unterminated banner, got nil")
		}
	})
}

// buildFakeZbootImage hand-assembles a minimal, byte-correct EFI
// zboot header (offZbootDOS/offZbootType/offZbootPayload/
// offZbootCompType, in exactly that layout) followed by payload,
// gzip-compressed if compType is "gzip" (payload passed through
// unchanged for any other compType, since a real decoder for it
// doesn't exist here either - see the "unsupported compression type"
// test below, which needs exactly that). This layout was verified
// against upstream Linux kernel source
// (drivers/firmware/efi/libstub/zboot-header.S) AND byte-for-byte
// against a real Alpine aarch64 linux-lts 6.6.142 vmlinuz-lts (see
// this package's own header comment and the hardening ledger's own
// entry for this fix) - not a guessed shape.
func buildFakeZbootImage(t *testing.T, compType string, payload []byte) []byte {
	t.Helper()
	const headerSize = offZbootCompType + 8 // room for compType + NUL + slack

	body := payload
	if compType == "gzip" {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		body = buf.Bytes()
	}

	header := make([]byte, headerSize)
	header[offZbootDOS], header[offZbootDOS+1] = 'M', 'Z'
	copy(header[offZbootType:offZbootType+4], []byte("zimg"))
	header[offZbootPayload] = byte(headerSize)
	header[offZbootPayload+1] = byte(headerSize >> 8)
	header[offZbootPayload+2] = byte(headerSize >> 16)
	header[offZbootPayload+3] = byte(headerSize >> 24)
	copy(header[offZbootCompType:], []byte(compType))
	// rest of header left zero, including the NUL after compType

	return append(header, body...)
}

// TestVersionFromImage_ZbootGzip_RealWorldShaped is the direct
// regression test for the real bug this fix exists for: a real Alpine
// aarch64 linux-lts 6.6.142 kernel (fetched from Alpine's own package
// repository and inspected by hand for this fix, not committed here -
// see the hardening ledger's own entry) turned out to be zboot-
// wrapped, gzip-compressed - a plain VersionFromLinuxBanner scan over
// the still-compressed outer bytes found nothing, exactly reproducing
// the real machine's own reported error. This fixture reproduces that
// SHAPE synthetically (a real gzip stream, a real zboot header layout)
// without committing an 11MB real kernel binary to this repo.
func TestVersionFromImage_ZbootGzip_RealWorldShaped(t *testing.T) {
	kernelBody := append([]byte{0x4d, 0x5a, 0xde, 0xad, 0xbe, 0xef},
		[]byte("Linux version 6.6.142-0-lts (buildozer@build) (gcc 13.2.1) #1-Alpine SMP PREEMPT_DYNAMIC\nmore kernel bytes after")...)
	img := buildFakeZbootImage(t, "gzip", kernelBody)

	got, err := VersionFromImage(img)
	if err != nil {
		t.Fatalf("VersionFromImage: %v", err)
	}
	want := "6.6.142-0-lts (buildozer@build) (gcc 13.2.1) #1-Alpine SMP PREEMPT_DYNAMIC"
	if got != want {
		t.Errorf("VersionFromImage = %q, want %q", got, want)
	}
	if short := ShortVersion(got); short != "6.6.142-0-lts" {
		t.Errorf("ShortVersion(VersionFromImage(...)) = %q, want %q - the \"Linux version \" prefix must be stripped for ShortVersion's simple up-to-first-space logic to work", short, "6.6.142-0-lts")
	}
}

func TestVersionFromImage_BzImageStillTriedFirst(t *testing.T) {
	// A bzImage-shaped input that ALSO happens to start with "MZ" (x86's
	// own real EFI-stub header does too - see this package's own header
	// comment) must still be read via VersionFromBytes, not misdetected
	// as zboot (it lacks "zimg" at offset 4, so isZbootImage correctly
	// says no) - proves the two formats don't get confused with each
	// other just because both start with the same DOS signature.
	const versionAbsOffset = 0x400
	buf := make([]byte, versionAbsOffset+64)
	buf[0], buf[1] = 'M', 'Z' // real x86 EFI-stub images do start this way too
	buf[offBootFlag], buf[offBootFlag+1] = 0x55, 0xAA
	copy(buf[offHeaderMagic:offHeaderMagic+4], []byte("HdrS"))
	buf[offVersion], buf[offVersion+1] = 0x0c, 0x02
	relOff := uint16(versionAbsOffset - 0x200)
	buf[offKernelVersion] = byte(relOff)
	buf[offKernelVersion+1] = byte(relOff >> 8)
	want := "6.18.52-0-lts"
	copy(buf[versionAbsOffset:], want)

	got, err := VersionFromImage(buf)
	if err != nil {
		t.Fatalf("VersionFromImage: %v", err)
	}
	if got != want {
		t.Errorf("VersionFromImage = %q, want %q", got, want)
	}
}

func TestVersionFromImage_ZbootUnsupportedCompression(t *testing.T) {
	img := buildFakeZbootImage(t, "zstd", []byte("Linux version 1.2.3-fake\n"))
	_, err := VersionFromImage(img)
	if err == nil {
		t.Fatal("want an error for an unsupported zboot compression type, got nil")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("zstd")) {
		t.Errorf("error = %q, want it to name the unsupported compression type", err.Error())
	}
}

func TestVersionFromImage_ZbootCorruptedGzipPayload(t *testing.T) {
	img := buildFakeZbootImage(t, "gzip", []byte("Linux version 1.2.3-fake\n"))
	// Corrupt the gzip payload itself (right after the header) - a
	// truncated/corrupted real-world download or flash write is exactly
	// what this proves doesn't panic or hang, just errors cleanly.
	for i := offZbootCompType + 8; i < len(img) && i < offZbootCompType+16; i++ {
		img[i] = 0xff
	}
	if _, err := VersionFromImage(img); err == nil {
		t.Fatal("want an error for a corrupted gzip payload, got nil")
	}
}

func TestVersionFromImage_ZbootPayloadOffsetOutOfRange(t *testing.T) {
	header := make([]byte, offZbootCompType+8)
	header[offZbootDOS], header[offZbootDOS+1] = 'M', 'Z'
	copy(header[offZbootType:offZbootType+4], []byte("zimg"))
	// A payload offset pointing past the end of the data given - a
	// real, if unlikely, truncated-file scenario.
	huge := uint32(0xFFFFFFFF)
	header[offZbootPayload] = byte(huge)
	header[offZbootPayload+1] = byte(huge >> 8)
	header[offZbootPayload+2] = byte(huge >> 16)
	header[offZbootPayload+3] = byte(huge >> 24)
	copy(header[offZbootCompType:], []byte("gzip"))

	if _, err := VersionFromImage(header); err == nil {
		t.Fatal("want an error for a zboot payload offset past the end of the data, got nil")
	}
}

func TestVersionFromImage_ZbootCompTypeNoNulTerminator(t *testing.T) {
	header := make([]byte, offZbootCompType+maxZbootCompTypeLen+8)
	header[offZbootDOS], header[offZbootDOS+1] = 'M', 'Z'
	copy(header[offZbootType:offZbootType+4], []byte("zimg"))
	header[offZbootPayload] = byte(len(header))
	for i := offZbootCompType; i < len(header); i++ {
		header[i] = 'x' // never a NUL within maxZbootCompTypeLen of offZbootCompType
	}

	if _, err := VersionFromImage(header); err == nil {
		t.Fatal("want an error for a zboot compression-type field with no NUL terminator, got nil")
	}
}

func TestIsZbootImage_MZAloneIsNotEnough(t *testing.T) {
	// A full source audit's own established concern (see fatLabelPattern
	// in internal/bootenv, PARTLABEL vs LABEL) applied here: MZ alone
	// must not be treated as "zboot", since x86's own real EFI-stub
	// bzImage header starts with MZ too and is a structurally different
	// format - only the SECOND marker ("zimg" at offset 4) actually
	// means zboot.
	data := make([]byte, offZbootCompType+4)
	data[0], data[1] = 'M', 'Z'
	copy(data[4:8], []byte("notz")) // anything other than "zimg"
	if isZbootImage(data) {
		t.Error("isZbootImage: MZ alone (without \"zimg\" at offset 4) must not count as zboot")
	}
}
