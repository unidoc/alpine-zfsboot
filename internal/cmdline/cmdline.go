// Package cmdline reads and parses the .cmdline PE section build.sh
// embeds into every alpine-zfsboot .EFI (see build.sh's own "UEFI
// stub assembly" comment) - the same NUL-terminated ASCII string
// efi/cmdline.c reads at boot, carrying alpine-zfsboot.buildstamp= and
// console= among its other kernel cmdline parameters. Reading it
// back out is the whole reason cmd/tool never needs to unpack an
// initramfs just to answer "what version is this .EFI": the answer
// is one small PE section away, using nothing but Go's own stdlib
// debug/pe package - no third-party PE parser needed.
package cmdline

import (
	"debug/pe"
	"fmt"
	"strings"
	"time"
)

// Info is everything cmd/tool needs out of a built .EFI - not just
// the version, but every alpine-zfsboot.* option build.sh bakes into the
// cmdline (see build.sh's own "UEFI stub assembly" comment for where
// each of these actually comes from), so `alpine-zfsboot version`
// can show the whole picture of what a given .EFI was built with,
// not just when.
type Info struct {
	Arch       string // "x86_64" or "aarch64" - matches build.sh's own ARCH values
	Console    string // "vga", "serial", or "auto" - matches build.sh's own CONSOLE_NAME values
	Version    string // alpine-zfsboot.version= - the project's own human-facing release number (e.g. "0.1.0", from the repo's own VERSION file) - display only, never compared
	BuildStamp string // alpine-zfsboot.buildstamp= - a sortable YYYYMMDDTHHMMSSZ build timestamp - what upToDate actually compares
	Pool       string // alpine-zfsboot.pool=
	Timeout    string // alpine-zfsboot.timeout= - seconds, as a string (never parsed as a number here)
	Raw        string // the full decoded cmdline, for diagnostics
}

// buildStampLayout is Go's reference-time spelling of the same
// "%Y%m%dT%H%M%SZ" format build.sh's `date -u` call writes (see its
// own BUILD_STAMP comment) - one format, two languages describing it.
const buildStampLayout = "20060102T150405Z"

// HumanVersion reformats a raw alpine-zfsboot.buildstamp= value into the same
// human-friendly form menu.py's own _build_stamp() shows on the boot
// screen ("2026-09-10 00:32 UTC") - falls back to the raw value
// verbatim if it doesn't parse as the expected format, same as
// menu.py's own fallback. Named for its ORIGINAL caller (displaying
// Info.Version, back when that field held the build stamp itself) -
// still takes a raw stamp string directly, now usually Info.BuildStamp.
func HumanVersion(raw string) string {
	t, err := time.Parse(buildStampLayout, raw)
	if err != nil {
		return raw
	}
	return t.Format("2006-01-02 15:04 UTC")
}

// Read opens path as a PE/COFF file, locates its .cmdline section,
// and parses it. Returns an error if path isn't a PE file, has no
// .cmdline section (not an alpine-zfsboot build), or that section
// has no alpine-zfsboot.buildstamp= (built before this tool existed, or
// before this field was renamed from alpine-zfsboot.version=).
func Read(path string) (Info, error) {
	f, err := pe.Open(path)
	if err != nil {
		return Info{}, fmt.Errorf("opening %s as a PE/EFI file: %w", path, err)
	}
	defer f.Close()

	arch, err := archFromMachine(f.FileHeader.Machine)
	if err != nil {
		return Info{}, fmt.Errorf("%s: %w", path, err)
	}

	sect := f.Section(".cmdline")
	if sect == nil {
		return Info{}, fmt.Errorf("%s has no .cmdline section - not an alpine-zfsboot .EFI?", path)
	}
	raw, err := sect.Data()
	if err != nil {
		return Info{}, fmt.Errorf("reading .cmdline section of %s: %w", path, err)
	}

	return parse(arch, raw, path)
}

// parse is Read()'s actual logic, split out so it's testable without
// a real PE fixture file - only the section bytes and the
// already-resolved arch matter from here on.
func parse(arch string, raw []byte, path string) (Info, error) {
	// NUL-terminated ASCII C string (see build.sh's own
	// cmdline.section construction, and efi/cmdline.c which reads it
	// the same way at boot) - objcopy pads the section's raw data out
	// to file alignment past that NUL with more zero bytes, so
	// cutting at the first one is always correct and never truncates
	// real content.
	line := string(raw)
	if i := strings.IndexByte(line, 0); i >= 0 {
		line = line[:i]
	}

	info := Info{
		Arch:       arch,
		Console:    consoleFromCmdline(line),
		Version:    valueOf(line, "alpine-zfsboot.version"),
		BuildStamp: valueOf(line, "alpine-zfsboot.buildstamp"),
		Pool:       valueOf(line, "alpine-zfsboot.pool"),
		Timeout:    valueOf(line, "alpine-zfsboot.timeout"),
		Raw:        line,
	}
	if info.BuildStamp == "" {
		return info, fmt.Errorf("%s's .cmdline has no alpine-zfsboot.buildstamp= - built before this tool existed?", path)
	}
	return info, nil
}

func archFromMachine(m uint16) (string, error) {
	switch m {
	case pe.IMAGE_FILE_MACHINE_AMD64:
		return "x86_64", nil
	case pe.IMAGE_FILE_MACHINE_ARM64:
		return "aarch64", nil
	default:
		return "", fmt.Errorf("unrecognized PE machine type 0x%x - not an alpine-zfsboot x86_64/aarch64 build", m)
	}
}

// consoleFromCmdline mirrors build.sh's own CONSOLE_CMDLINE
// construction in reverse: vga is a bare console=tty0, serial is a
// bare console=ttyS0/ttyAMA0/etc, auto is both together on the same
// cmdline (see build.sh's own CONSOLE_NAME case statement).
func consoleFromCmdline(line string) string {
	hasVga := false
	hasSerial := false
	for _, tok := range strings.Fields(line) {
		switch {
		case tok == "console=tty0":
			hasVga = true
		case strings.HasPrefix(tok, "console=tty"):
			hasSerial = true
		}
	}
	switch {
	case hasVga && hasSerial:
		return "auto"
	case hasSerial:
		return "serial"
	case hasVga:
		return "vga"
	default:
		return ""
	}
}

// valueOf returns the value of the first "key=value" cmdline token
// matching key, or "" if absent - the same word-splitting convention
// /init's own cmdline parser uses (see its own alpine-zfsboot.pool=/
// alpine-zfsboot.timeout= parsing).
func valueOf(line, key string) string {
	prefix := key + "="
	for _, tok := range strings.Fields(line) {
		if v, ok := strings.CutPrefix(tok, prefix); ok {
			return v
		}
	}
	return ""
}
