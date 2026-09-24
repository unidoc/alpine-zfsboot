// Package metadata defines alpine-zfsboot's own artifact manifest - a
// small, versioned record of facts about the rescue kernel/initrd a
// given installed alpine-zfsboot boot environment actually carries.
//
// Generated LOCALLY, by cmd/tool's own install/update, immediately
// after writing (and byte-verifying) the real kernel/initrd payload -
// NOT baked in at build.sh time and not a separate downloaded release
// asset: install/update already have the exact final bytes in hand
// (having just written them), and already pay a deep-inspection cost
// once per install/update to validate them, so generating this
// manifest there is free in the sense that matters (it happens on an
// infrequent, already-somewhat-expensive operation, not on every
// single status/verify call).
//
// Why this exists: `status`/`verify` need to answer "what kernel/
// OpenZFS version does the installed rescue artifact carry" - a real
// question, independent of whatever kernel happens to be running on
// the machine at the time (see init/boot-dataset.sh's own kexec
// handoff: the rescue kernel this project ships is NEVER the kernel a
// normally-booted machine is actually running under, so comparing
// against the live system's own /proc/sys/kernel/osrelease etc. is not
// a valid shortcut here - a real architectural dead end this project
// tried and reverted, see the hardening ledger). Answering that
// question has always required decompressing the installed initrd and
// walking its cpio contents to find zfs.ko - real, necessary work
// internal/initrdinfo now does as fast as this project can reasonably
// make it (streaming decompression that stops at the match, gzip as
// the default compression - see that package's own doc comment for
// the real benchmark data behind both changes) - but even a fast
// decompression is still real CPU work this manifest lets `status`
// skip entirely on every call after the first.
//
// Read-only commands stay read-only: `status` reads this manifest
// directly if present (no decompression at all) and falls back to the
// full, real internal/kernelinfo/internal/initrdinfo extraction if
// it's missing (an older installation, predating this feature) -
// but NEVER writes or repairs it itself. Only install/update
// (already-mutating operations) create or replace it. `verify`
// additionally binds the manifest to the real installed bytes via its
// two SHA-256 fields (catching drift/corruption/manual tampering
// without decompressing anything); `verify --deep` goes further and
// re-derives the kernel/OpenZFS versions from the real payload the
// same way `status`'s own fallback does, comparing the fresh result
// against the manifest's own recorded values - the strongest check,
// proving the manifest doesn't just match the raw bytes' hash but that
// the DECODED content truly matches too.
package metadata

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Format is the only manifest format version this package currently
// produces or accepts. A future incompatible change bumps this and
// Decode refuses anything else outright, rather than guessing at a
// partially-understood older or newer shape.
const Format = 1

// Manifest is everything this project currently knows about a given
// build's rescue kernel/initrd at the moment it was built - see this
// package's own doc comment for the full rationale.
type Manifest struct {
	Version      string // alpine-zfsboot's own human-facing release number (e.g. "0.1.0") - display only, matches cmdline.Info.Version
	BuildStamp   string // the same sortable build timestamp cmdline.Info.BuildStamp carries - the two are required to agree, see Verify's own doc comment
	Arch         string // "x86_64" or "aarch64"
	Kernel       string // the rescue kernel's own version string, exactly what internal/kernelinfo.VersionFromImage would derive from the real image
	OpenZFS      string // the rescue initrd's own zfs.ko version, exactly what internal/initrdinfo.ExtractOpenZFSVersion would derive from the real initrd
	KernelSHA256 string // lowercase hex SHA-256 of the exact kernel image bytes this manifest describes
	InitrdSHA256 string // lowercase hex SHA-256 of the exact initrd bytes this manifest describes
}

// requiredFields lists every key Decode requires present and non-empty
// - a manifest missing any of these is malformed, not merely
// incomplete (every one of these is unconditionally produced by
// Encode/gen-metadata; a real build never legitimately omits one).
var requiredFields = []string{"version", "buildstamp", "arch", "kernel", "openzfs", "kernel_sha256", "initrd_sha256"}

// knownFields is requiredFields plus "format" itself (checked
// separately, before this set is consulted) - every key Decode will
// accept for format=1. Anything else is refused outright: a small,
// versioned, fully-enumerated manifest has no legitimate reason to
// carry a field this package doesn't know about, and silently
// ignoring one - the same as silently keeping only the LAST of a
// duplicate key - is exactly the "found nothing wrong because we
// didn't look at everything" trap this project's other decoders
// already guard against for their own inputs.
var knownFields = func() map[string]bool {
	m := map[string]bool{"format": true}
	for _, f := range requiredFields {
		m[f] = true
	}
	return m
}()

// sha256HexPattern is exactly what Manifest's own KernelSHA256/
// InitrdSHA256 fields (and metadata.SHA256Hex's own output) always
// look like: 64 lowercase hex characters, nothing else. Decode
// enforces this on read so a hand-edited or truncated hash (which
// would otherwise simply never match any real payload, but by
// accident rather than by a checked invariant) is refused with a
// specific, clear reason instead of just "verify failed" later.
var sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Encode serializes m as the canonical on-disk byte form - one
// "key=value" per line, LF-separated, trailing newline, `format=`
// first.
//
// One representation, one writer: internal/layout.MetadataFile on the
// ESP's own FAT filesystem, written by cmd/tool via espconfig.WriteFile
// on BOTH the BIOS and the UEFI path (UEFI derives the manifest from
// the just-written .EFI's own embedded .linux/.initrd PE sections - see
// cmd/tool's writeUEFIMetadata - but still stores the resulting
// manifest as that same plain FAT file). There is deliberately no
// second, PE-section-embedded copy and nothing build.sh generates: two
// independently-produced representations of the same facts is exactly
// the drift this project avoids elsewhere, and a manifest that has to
// be regenerable by `verify --repair` on a live machine cannot live
// inside a signed/byte-verified loader image anyway.
func Encode(m Manifest) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "format=%d\n", Format)
	fmt.Fprintf(&b, "version=%s\n", m.Version)
	fmt.Fprintf(&b, "buildstamp=%s\n", m.BuildStamp)
	fmt.Fprintf(&b, "arch=%s\n", m.Arch)
	fmt.Fprintf(&b, "kernel=%s\n", m.Kernel)
	fmt.Fprintf(&b, "openzfs=%s\n", m.OpenZFS)
	fmt.Fprintf(&b, "kernel_sha256=%s\n", m.KernelSHA256)
	fmt.Fprintf(&b, "initrd_sha256=%s\n", m.InitrdSHA256)
	return []byte(b.String())
}

// Decode parses raw (as produced by Encode - a NUL byte, if present
// from PE-section padding, ends the content the same way
// internal/cmdline's own firstLine handles a padded .cmdline section)
// back into a Manifest. Fails closed: an unrecognized format=, a
// missing required field, or trailing garbage after a NUL that isn't
// itself just more NUL padding are all reported as errors - a
// manifest this package cannot fully understand must never be
// silently partially trusted (the same "found nothing wrong because
// we didn't look at everything" trap this project's other decoders -
// internal/zfsnative's own checkElemCount/parseFeatureStats - already
// guard against for their own inputs).
func Decode(raw []byte) (Manifest, error) {
	text := raw
	if i := indexNUL(text); i >= 0 {
		// A full source audit found this function's own doc comment
		// above already PROMISED that trailing bytes after a NUL which
		// aren't themselves just more NUL padding are an error - and
		// the code did not check it: it truncated at the first NUL and
		// never looked at the rest, so a manifest file with real
		// garbage appended after a NUL (FAT slack from a torn write, a
		// partially-overwritten older/longer manifest, deliberate
		// tampering) decoded "cleanly" from its prefix and was then
		// trusted by status and verify alike. Neither command's
		// existing checks would have caught it either: the two SHA-256
		// fields bind the manifest to the KERNEL/INITRD bytes, not the
		// manifest file to itself. Checking it for real is the
		// fail-closed reading, costs nothing on a legitimate manifest
		// (espconfig.WriteFile writes exactly Encode's bytes, with no
		// NUL in them at all), and makes the documented contract true.
		for _, c := range text[i:] {
			if c != 0 {
				return Manifest{}, fmt.Errorf("metadata: real content follows the NUL padding at offset %d - the file is corrupt or has been appended to, not merely padded", i)
			}
		}
		text = text[:i]
	}

	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(string(text), "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return Manifest{}, fmt.Errorf("metadata: malformed line %q (want key=value)", line)
		}
		if !knownFields[k] {
			return Manifest{}, fmt.Errorf("metadata: unknown field %q - format=%d does not define it", k, Format)
		}
		if _, dup := fields[k]; dup {
			return Manifest{}, fmt.Errorf("metadata: duplicate field %q", k)
		}
		fields[k] = v
	}

	formatStr, ok := fields["format"]
	if !ok {
		return Manifest{}, fmt.Errorf("metadata: no format= field - not an alpine-zfsboot metadata manifest?")
	}
	format, err := strconv.Atoi(formatStr)
	if err != nil {
		return Manifest{}, fmt.Errorf("metadata: format=%q is not a number", formatStr)
	}
	if format != Format {
		return Manifest{}, fmt.Errorf("metadata: format=%d, this build only understands format=%d", format, Format)
	}

	var missing []string
	for _, k := range requiredFields {
		if fields[k] == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return Manifest{}, fmt.Errorf("metadata: missing required field(s): %s", strings.Join(missing, ", "))
	}

	for _, f := range []struct{ name, value string }{
		{"kernel_sha256", fields["kernel_sha256"]},
		{"initrd_sha256", fields["initrd_sha256"]},
	} {
		if !sha256HexPattern.MatchString(f.value) {
			return Manifest{}, fmt.Errorf("metadata: %s is not exactly 64 lowercase hex characters", f.name)
		}
	}

	return Manifest{
		Version:      fields["version"],
		BuildStamp:   fields["buildstamp"],
		Arch:         fields["arch"],
		Kernel:       fields["kernel"],
		OpenZFS:      fields["openzfs"],
		KernelSHA256: fields["kernel_sha256"],
		InitrdSHA256: fields["initrd_sha256"],
	}, nil
}

func indexNUL(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}

// SHA256Hex returns data's own SHA-256 as lowercase hex - the exact
// form Manifest's own KernelSHA256/InitrdSHA256 fields use, and what
// VerifyHash below compares against. A named helper (not just inlined
// hex.EncodeToString(sha256.Sum256(...)[:]) at each call site) so
// gen-metadata and VerifyHash are provably using the identical
// encoding, not two independently-typed-out ones that could drift.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// VerifyHash reports whether data's own SHA-256 matches want (a
// Manifest's own KernelSHA256 or InitrdSHA256 field) - a plain,
// constant-time-irrelevant equality check (this is integrity
// verification against accidental drift/corruption/an out-of-band
// edit, not a secret-comparison that needs to resist timing attacks).
func VerifyHash(data []byte, want string) bool {
	return SHA256Hex(data) == strings.ToLower(want)
}
