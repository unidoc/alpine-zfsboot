// Package kernelinfo reads the version of a boot kernel image without
// invoking any external tool (file/strings/uname/...) - this is a
// boot/recovery introspection tool, and must describe the boot
// PAYLOAD (what will actually boot next), not the currently running
// kernel, which uname -r would report instead and which proves
// nothing about the artifact actually sitting in EFI/ALPINE/KERNEL or
// embedded in a UEFI .EFI's own ".linux" section.
//
// x86 bzImage carries a small, deliberately-uncompressed ASCII
// version string baked into its own setup-header area at build time -
// the SAME field GRUB/syslinux/every other x86 bootloader reads to
// show a kernel's version in a boot menu, without decompressing
// anything. Field offsets are from the Linux kernel's own boot
// protocol documentation (Documentation/x86/boot.rst) - stable by
// design, since every existing bootloader's own compatibility depends
// on these offsets never moving. See VersionFromBytes.
//
// ARM64 has no equivalent dedicated field - Image (Documentation/
// arch/arm64/booting.rst) carries no build-time version string at a
// fixed offset the way bzImage does. A real Alpine aarch64
// linux-lts 6.6.142 kernel proved two further facts empirically, not
// assumed: (1) its own "Linux version ..." banner string (the same
// one printk emits at boot, init/version.c's own linux_banner) IS
// present verbatim if the image is genuinely uncompressed - see
// VersionFromLinuxBanner - but (2) real Alpine aarch64 builds are
// NOT plain uncompressed Image at all: they use the EFI "zboot"
// self-decompressing wrapper format (a real, upstream Linux kernel
// format - drivers/firmware/efi/libstub/zboot-header.S/zboot.c,
// verified directly against that source, not guessed), a small
// header (starting "MZ" + literal ASCII "zimg") wrapping a compressed
// payload an EFI stub decompresses at boot time. A plain byte-string
// scan over the OUTER, still-compressed file finds nothing - the
// banner text only exists inside the decompressed payload. See
// unwrapZboot/VersionFromImage.
//
// VersionFromBytes/VersionFromImage are the actual logic, over
// already-read bytes - both work identically whether those bytes came
// from a standalone vmlinuz FILE (Version, BIOS's own case - see
// internal/layout.KernelFile) or from a UEFI .EFI's own embedded
// ".linux" PE section (build.sh's objcopy copies the same kernel
// image bytes verbatim into that section, no transformation - see
// internal/cmdline's own doc comment on that embedding), so cmd/tool's
// UEFI status path can feed this the section's own bytes directly
// without a temp file.
package kernelinfo

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

const (
	offBootFlag      = 0x1FE // 2 bytes, must be 0xAA55
	offHeaderMagic   = 0x202 // 4 bytes, must be "HdrS"
	offVersion       = 0x206 // 2 bytes, boot protocol version (e.g. 0x020c)
	offKernelVersion = 0x20E // 2 bytes, offset (relative to file offset 0x200) to a NUL-terminated version string
)

// VersionFromBytes extracts the embedded kernel-version string (e.g.
// "6.18.52-0-lts (buildd@build) #1-Alpine SMP ...") from data - the
// same substring `file vmlinuz`/GRUB's own menu entry generation
// reads, not the full uname -r-style short version alone; callers
// that only want the leading "X.Y.Z-flavor" token should parse that
// out of the returned string themselves (see ShortVersion below).
// data must include at least the bzImage's own setup-header area
// (the first ~1KB) AND the version string itself, wherever within
// data that ends up - the whole file/section, not just a truncated
// prefix.
func VersionFromBytes(data []byte) (string, error) {
	if len(data) < offKernelVersion+2 {
		return "", fmt.Errorf("only %d bytes given, too short to be a bzImage header", len(data))
	}

	if data[offBootFlag] != 0x55 || data[offBootFlag+1] != 0xAA {
		return "", fmt.Errorf("no 0xAA55 boot flag at offset 0x%x - not a bzImage", offBootFlag)
	}
	if !bytes.Equal(data[offHeaderMagic:offHeaderMagic+4], []byte("HdrS")) {
		return "", fmt.Errorf("no \"HdrS\" header magic at offset 0x%x - not a bzImage", offHeaderMagic)
	}

	protoVersion := uint16(data[offVersion]) | uint16(data[offVersion+1])<<8
	if protoVersion < 0x0200 {
		return "", fmt.Errorf("boot protocol version 0x%04x predates the kernel_version field (needs >= 0x0200)", protoVersion)
	}

	relOff := uint16(data[offKernelVersion]) | uint16(data[offKernelVersion+1])<<8
	if relOff == 0 {
		return "", fmt.Errorf("no kernel_version string (field is zero)")
	}
	absOff := int(relOff) + 0x200
	if absOff >= len(data) {
		return "", fmt.Errorf("kernel_version points at offset %d, past the %d bytes given", absOff, len(data))
	}

	nul := bytes.IndexByte(data[absOff:], 0)
	if nul < 0 {
		return "", fmt.Errorf("version string at offset %d has no NUL terminator within the data given", absOff)
	}
	return string(data[absOff : absOff+nul]), nil
}

// VersionFromLinuxBanner scans data for the kernel's own "Linux
// version X.Y.Z ..." banner string (init/version.c's own
// linux_banner, the same one printk emits at the very start of boot)
// and returns everything from "Linux" up to the next NUL/newline.
// Unlike VersionFromBytes above, this has no fixed offset to look at -
// a generic scan, not a boot-protocol field read - so it works for any
// genuinely UNCOMPRESSED kernel image regardless of architecture. Real
// aarch64 hardware testing (a real Alpine linux-lts 6.6.142 kernel)
// proved this alone is NOT sufficient for a real Alpine aarch64
// build - see VersionFromImage, which is what callers should actually
// use; this function stays exported and usable standalone for a
// caller that already knows its input is genuinely uncompressed.
func VersionFromLinuxBanner(data []byte) (string, error) {
	const prefix = "Linux version "
	idx := bytes.Index(data, []byte(prefix))
	if idx < 0 {
		return "", fmt.Errorf("no %q banner string found in %d bytes given", prefix, len(data))
	}
	rest := data[idx:]
	end := bytes.IndexAny(rest, "\n\x00")
	if end < 0 {
		return "", fmt.Errorf("%q banner string found at offset %d has no newline/NUL terminator within the data given", prefix, idx)
	}
	return string(rest[:end]), nil
}

// Offsets into an EFI "zboot" self-decompressing image header -
// drivers/firmware/efi/libstub/zboot-header.S in the real upstream
// Linux kernel source (verified directly against that source, AND
// against a real Alpine aarch64 linux-lts 6.6.142 vmlinuz-lts byte
// for byte, not assumed from the assembly alone): the DOS/PE-COFF
// "MZ" signature at offset 0 (the same one x86 bzImage's own EFI-stub
// header also starts with, for the same "loadable as a PE/COFF EFI
// application" reason - MZ alone does NOT mean zboot), immediately
// followed at offset 4 by the literal 4-byte ASCII marker "zimg" -
// THIS second field is what actually, specifically identifies a
// zboot image, not just "some EFI-loadable kernel". offZbootPayload
// is a little-endian uint32 - Ldoshdr's own `.long
// __efistub__gzdata_start - .Ldoshdr` - the byte offset from the
// START OF THIS HEADER to the compressed payload; offZbootCompType is
// a NUL-terminated ASCII string (the assembly's own `.asciz
// COMP_TYPE`) naming the compression algorithm, e.g. "gzip".
const (
	offZbootDOS      = 0  // 2 bytes, "MZ"
	offZbootType     = 4  // 4 bytes, "zimg"
	offZbootPayload  = 8  // 4 bytes, LE u32, byte offset to the compressed payload
	offZbootCompType = 24 // NUL-terminated ASCII compression-algorithm name
)

// maxZbootCompTypeLen bounds the search for offZbootCompType's own NUL
// terminator - the real field is always short ("gzip", "lz4", "lzma",
// "zstd" are the upstream-supported values as of this writing), this
// is generous headroom over any of those while still refusing to scan
// unboundedly into a malformed/truncated header with no NUL at all.
const maxZbootCompTypeLen = 32

// maxZbootDecompressedSize bounds the decompressed payload the same
// way internal/initrdinfo's own readAllBounded does, and for the
// identical reason: a malformed or deliberately hostile boot artifact
// must not be able to force this process to allocate without bound.
// 256MiB is generous headroom over any real Linux kernel image (even
// an uncompressed arm64 Image with every driver built in rarely
// exceeds 40-50MiB).
const maxZbootDecompressedSize = 256 * 1024 * 1024

// isZbootImage reports whether data starts with a real zboot header -
// checked via BOTH magic fields (MZ and "zimg"), not just MZ alone,
// since x86 bzImage's own EFI-stub header is also MZ-prefixed but is
// a structurally different format entirely (see VersionFromBytes).
func isZbootImage(data []byte) bool {
	return len(data) >= offZbootCompType+4 &&
		data[offZbootDOS] == 'M' && data[offZbootDOS+1] == 'Z' &&
		bytes.Equal(data[offZbootType:offZbootType+4], []byte("zimg"))
}

// unwrapZboot decompresses a zboot-wrapped image's real payload -
// data must already satisfy isZbootImage. Only "gzip" is decoded (Go
// stdlib, no new dependency); any other compression type upstream
// might use is reported by name, clearly, rather than silently
// mis-decompressed or reported as a generic parse failure - same
// discipline as internal/initrdinfo's own zstd handling.
func unwrapZboot(data []byte) ([]byte, error) {
	payloadOff := binary.LittleEndian.Uint32(data[offZbootPayload:])
	if int(payloadOff) >= len(data) {
		return nil, fmt.Errorf("zboot payload offset %d is past the %d bytes given", payloadOff, len(data))
	}

	compTypeField := data[offZbootCompType:min(offZbootCompType+maxZbootCompTypeLen, len(data))]
	nul := bytes.IndexByte(compTypeField, 0)
	if nul < 0 {
		return nil, fmt.Errorf("zboot compression-type field has no NUL terminator within %d bytes", maxZbootCompTypeLen)
	}
	compType := string(compTypeField[:nul])

	switch compType {
	case "gzip":
		r, err := gzip.NewReader(bytes.NewReader(data[payloadOff:]))
		if err != nil {
			return nil, fmt.Errorf("opening zboot gzip payload: %w", err)
		}
		defer r.Close()
		// The zboot header's own payload-size trailer (see this
		// function's own doc comment) sits immediately after the real
		// gzip stream ends - without this, Go's gzip.Reader defaults to
		// Multistream(true) and tries to parse those trailing bytes as
		// the START of a SECOND gzip member, failing with a confusing
		// "invalid header" instead of cleanly stopping at the first
		// (and only real) member's own end. Confirmed empirically
		// against a real Alpine aarch64 vmlinuz-lts, not assumed.
		r.Multistream(false)
		out, err := io.ReadAll(io.LimitReader(r, maxZbootDecompressedSize+1))
		if err != nil {
			return nil, fmt.Errorf("decompressing zboot gzip payload: %w", err)
		}
		if len(out) > maxZbootDecompressedSize {
			return nil, fmt.Errorf("zboot payload decompresses to more than %d bytes - refusing (malformed or hostile input?)", maxZbootDecompressedSize)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("zboot payload is %q-compressed - not supported (only gzip is)", compType)
	}
}

// VersionFromImage is the composed, authoritative entry point every
// real caller should use (Version/cmd/tool's own UEFI status path) -
// it tries every real format this package knows how to identify, in
// order, and only reports UNKNOWN once none of them match:
//
//  1. x86 bzImage (VersionFromBytes) - unchanged behavior for every
//     existing x86_64 case, since this is tried first and is a strict
//     superset of the old behavior.
//  2. EFI zboot (unwrapZboot, then VersionFromLinuxBanner on the
//     unwrapped payload) - the real format a real Alpine aarch64
//     linux-lts 6.6.142 build turned out to use (see this package's
//     own header comment) - a plain banner scan over the STILL-
//     COMPRESSED outer bytes never finds anything, by construction.
//  3. A genuinely uncompressed image (VersionFromLinuxBanner on data
//     directly) - covers the case the ORIGINAL, pre-zboot-discovery
//     assumption described (some other distro/config producing a
//     real, uncompressed arch/arm64/boot/Image), kept as the final
//     fallback rather than removed, since it costs nothing to keep
//     trying and there is no other format-specific signal to gate it
//     on.
//
// Deliberately does NOT invoke uname, file, strings, or any other
// external tool - see this package's own header comment for why that
// matters specifically for a boot/recovery introspection tool.
func VersionFromImage(data []byte) (string, error) {
	if v, err := VersionFromBytes(data); err == nil {
		return v, nil
	}
	if isZbootImage(data) {
		unwrapped, err := unwrapZboot(data)
		if err != nil {
			return "", fmt.Errorf("zboot image: %w", err)
		}
		v, err := VersionFromLinuxBanner(unwrapped)
		return stripLinuxVersionPrefix(v), err
	}
	v, err := VersionFromLinuxBanner(data)
	return stripLinuxVersionPrefix(v), err
}

// stripLinuxVersionPrefix normalizes VersionFromLinuxBanner's own
// "Linux version X.Y.Z ..." shape down to VersionFromBytes' own
// convention (leading directly with the X.Y.Z[-flavor] token, no
// "Linux version " prefix) - so every caller of VersionFromImage
// (ShortVersion in particular, which just takes everything up to the
// first space) gets ONE consistent contract regardless of which
// internal path actually produced the string, rather than needing to
// know or care. VersionFromLinuxBanner itself keeps its own
// documented, tested return shape unchanged - this normalization is
// deliberately local to VersionFromImage, not pushed down into it.
func stripLinuxVersionPrefix(v string) string {
	const prefix = "Linux version "
	if len(v) >= len(prefix) && v[:len(prefix)] == prefix {
		return v[len(prefix):]
	}
	return v
}

// Version reads path (a standalone vmlinuz file) and extracts its
// embedded kernel-version string via VersionFromImage.
func Version(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	v, err := VersionFromImage(data)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}

// ShortVersion extracts just the leading "X.Y.Z[-flavor]" token from
// a full Version() string (up to the first space) - e.g.
// "6.18.52-0-lts" from "6.18.52-0-lts (buildd@build) #1-Alpine SMP ...".
func ShortVersion(full string) string {
	if i := bytes.IndexByte([]byte(full), ' '); i >= 0 {
		return full[:i]
	}
	return full
}
