// Package payloadsum writes and checks EFI/ALPINE/CHECKSUM: the size
// and SHA-256 of exactly the bytes the BIOS stage2 loader places in RAM
// from EFI/ALPINE/KERNEL and EFI/ALPINE/INITRD. stage2 (bios/payload_sum.c,
// bios/stage2_main.c) hashes the loaded kernel and initrd in RAM after
// loading them, so a BIOS read that reported success but delivered wrong
// bytes - one hypothesis for physical-host boot failures that is otherwise
// invisible - shows up with both digests on screen.
//
// This is an integrity check against corruption and forgotten updates,
// NOT secure boot: the manifest sits on the same unauthenticated FAT as
// the kernel, so anyone who can write the disk can change both. Real
// tamper resistance needs a root of trust outside the disk (UEFI Secure
// Boot with a signed loader), which BIOS boot does not have.
//
// The manifest is written by the install/update path together with the
// payload (cmd/tool's writePayloadWithRollback) and is part of the same
// generation. A missing manifest is not an error anywhere: stage2 boots
// without verifying (with a notice), verify reports it as a notice.
//
// MODE says what stage2 does with it (ModeWarn is the default, and what
// stage2 assumes for no MODE line or an unknown one):
//
//	off     do not hash at all (saves boot time on a huge initrd)
//	warn    hash; on a mismatch print both digests and boot anyway
//	enforce hash; refuse to boot on a mismatch (explicit opt-in only, for
//	        diagnosing a failing host that has another way to boot)
//
// alpine-zfsboot.integrity=off|warn|enforce in EFI/ALPINE/CMDLINE
// overrides it for a boot (cmdline > MODE > warn). Set with --integrity on
// install/update or `alpine-zfsboot integrity <mode>`.
//
// Format (bios/payload_sum.h is the reader; keep the two in step -
// testdata/golden.sum is checked by both the Go and the C test):
//
//	alpine-zfsboot payload-sum 1
//	MODE off|warn|enforce
//	KERNEL <file size> <offset> <sha256 of the file from offset to end>
//	INITRD <file size> <offset> <sha256 of the file from offset to end>
//
// For KERNEL, offset is the size of the bzImage's real-mode part, which
// stage2 never loads: ((setup_sects, or 4 when 0) + 1) * 512. For INITRD
// it is 0. Only x86_64 BIOS installs have a stage2, so only they get one.
package payloadsum

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Header is the manifest's first line, without its newline.
const Header = "alpine-zfsboot payload-sum 1"

// MaxBytes mirrors bios/payload_sum.h's PAYLOAD_SUM_MAX_BYTES: stage2
// reads at most this much of the file.
const MaxBytes = 512

// Entry describes one file: its size, where the loaded part starts, and
// the SHA-256 of the file from Offset to the end.
type Entry struct {
	Size   uint32
	Offset uint32
	SHA256 [32]byte
}

// The three modes; see the package comment.
const (
	ModeOff     = "off"
	ModeWarn    = "warn"
	ModeEnforce = "enforce"
)

// ValidMode reports whether m is one of the three modes.
func ValidMode(m string) bool {
	return m == ModeOff || m == ModeWarn || m == ModeEnforce
}

// Manifest is a decoded CHECKSUM.
type Manifest struct {
	Kernel Entry
	Initrd Entry
	Mode   string // ModeOff/ModeWarn/ModeEnforce
}

// bzImage setup header fields (Documentation/arch/x86/boot.rst) - the same
// ones bios/stage2_main.c checks before it loads anything.
const (
	offSetupSects = 0x1f1
	offBootFlag   = 0x1fe
	offHeader     = 0x202
)

// KernelRAMOffset returns where, inside a bzImage, the part stage2 loads
// to 1 MiB starts - exactly stage2_main.c's real_mode_bytes. A kernel that
// is not a bzImage (stage2 would refuse it too) is an error.
func KernelRAMOffset(kernel []byte) (uint32, error) {
	if len(kernel) < offHeader+4 ||
		binary.LittleEndian.Uint16(kernel[offBootFlag:]) != 0xAA55 ||
		!bytes.Equal(kernel[offHeader:offHeader+4], []byte("HdrS")) {
		return 0, fmt.Errorf("payloadsum: kernel is not an x86 bzImage (no 0xAA55 boot flag / HdrS header) - the BIOS loader could not boot it")
	}
	sects := uint32(kernel[offSetupSects])
	if sects == 0 {
		sects = 4
	}
	off := (sects + 1) * 512
	if uint64(off) >= uint64(len(kernel)) {
		return 0, fmt.Errorf("payloadsum: kernel is %d bytes, smaller than its own real-mode part (%d bytes)", len(kernel), off)
	}
	return off, nil
}

func entryFor(data []byte, offset uint32) (Entry, error) {
	if uint64(len(data)) > 0xffffffff {
		return Entry{}, fmt.Errorf("payloadsum: %d bytes does not fit the 32-bit size field", len(data))
	}
	return Entry{Size: uint32(len(data)), Offset: offset, SHA256: sha256.Sum256(data[offset:])}, nil
}

// New computes the manifest for a kernel and initrd.
func New(kernel, initrd []byte, mode string) (Manifest, error) {
	if !ValidMode(mode) {
		return Manifest{}, fmt.Errorf("payloadsum: integrity mode %q is not off, warn or enforce", mode)
	}
	off, err := KernelRAMOffset(kernel)
	if err != nil {
		return Manifest{}, err
	}
	k, err := entryFor(kernel, off)
	if err != nil {
		return Manifest{}, err
	}
	i, err := entryFor(initrd, 0)
	if err != nil {
		return Manifest{}, err
	}
	return Manifest{Kernel: k, Initrd: i, Mode: mode}, nil
}

// Encode renders m in the exact byte format stage2 parses.
func Encode(m Manifest) []byte {
	var b strings.Builder
	b.WriteString(Header + "\n")
	mode := m.Mode
	if !ValidMode(mode) {
		mode = ModeWarn
	}
	b.WriteString("MODE " + mode + "\n")
	for _, e := range []struct {
		name string
		e    Entry
	}{{"KERNEL", m.Kernel}, {"INITRD", m.Initrd}} {
		fmt.Fprintf(&b, "%s %d %d %s\n", e.name, e.e.Size, e.e.Offset, hex.EncodeToString(e.e.SHA256[:]))
	}
	return []byte(b.String())
}

// Generate is New followed by Encode.
func Generate(kernel, initrd []byte, mode string) ([]byte, error) {
	m, err := New(kernel, initrd, mode)
	if err != nil {
		return nil, err
	}
	return Encode(m), nil
}

// Decode parses a manifest with the same strictness as stage2's parser:
// every line ends in '\n', single spaces, decimal numbers, 64 lowercase
// hex digits, KERNEL and INITRD exactly once; lines naming other files are
// ignored.
func Decode(raw []byte) (Manifest, error) {
	if len(raw) > MaxBytes {
		return Manifest{}, fmt.Errorf("payloadsum: %d bytes, the BIOS loader reads at most %d", len(raw), MaxBytes)
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		return Manifest{}, fmt.Errorf("payloadsum: empty, or the last line does not end in a newline")
	}
	lines := strings.Split(string(raw[:len(raw)-1]), "\n")
	if lines[0] != Header {
		return Manifest{}, fmt.Errorf("payloadsum: first line is %q, want %q", lines[0], Header)
	}
	m := Manifest{Mode: ModeWarn}
	var haveK, haveI bool
	for _, line := range lines[1:] {
		f := strings.Split(line, " ")
		var dst *Entry
		var seen *bool
		switch f[0] {
		case "MODE":
			// Same rule as stage2: an unknown or malformed MODE is warn.
			m.Mode = ModeWarn
			if len(f) == 2 && ValidMode(f[1]) {
				m.Mode = f[1]
			}
			continue
		case "KERNEL":
			dst, seen = &m.Kernel, &haveK
		case "INITRD":
			dst, seen = &m.Initrd, &haveI
		default:
			continue
		}
		if *seen {
			return Manifest{}, fmt.Errorf("payloadsum: %s listed twice", f[0])
		}
		if len(f) != 4 {
			return Manifest{}, fmt.Errorf("payloadsum: malformed %s line %q", f[0], line)
		}
		size, err1 := parseDec(f[1])
		off, err2 := parseDec(f[2])
		sum, err3 := hex.DecodeString(f[3])
		if err1 != nil || err2 != nil || err3 != nil || len(sum) != 32 || strings.ToLower(f[3]) != f[3] || off > size {
			return Manifest{}, fmt.Errorf("payloadsum: malformed %s line %q", f[0], line)
		}
		dst.Size, dst.Offset = size, off
		copy(dst.SHA256[:], sum)
		*seen = true
	}
	if !haveK || !haveI {
		return Manifest{}, fmt.Errorf("payloadsum: KERNEL and INITRD must both be listed")
	}
	return m, nil
}

func parseDec(s string) (uint32, error) {
	if s == "" || len(s) > 10 || strings.TrimLeft(s, "0123456789") != "" {
		return 0, fmt.Errorf("not a decimal number: %q", s)
	}
	v, err := strconv.ParseUint(s, 10, 32)
	return uint32(v), err
}

// Verify checks kernel and initrd against m the way stage2 does (size,
// loaded-range offset, SHA-256 of the loaded range) and returns one error
// per mismatch, each naming both digests.
func Verify(m Manifest, kernel, initrd []byte) []error {
	var errs []error
	off, err := KernelRAMOffset(kernel)
	if err != nil {
		return []error{err}
	}
	for _, c := range []struct {
		name   string
		want   Entry
		data   []byte
		offset uint32
	}{{"KERNEL", m.Kernel, kernel, off}, {"INITRD", m.Initrd, initrd, 0}} {
		switch {
		case uint64(len(c.data)) != uint64(c.want.Size):
			errs = append(errs, fmt.Errorf("payload-sum: %s is %d bytes, CHECKSUM records %d (the BIOS loader reports this mismatch; it refuses to boot only in integrity mode enforce)", c.name, len(c.data), c.want.Size))
		case c.offset != c.want.Offset:
			errs = append(errs, fmt.Errorf("payload-sum: %s loads from offset %d, CHECKSUM records %d (the BIOS loader reports this mismatch; it refuses to boot only in integrity mode enforce)", c.name, c.offset, c.want.Offset))
		default:
			got := sha256.Sum256(c.data[c.offset:])
			if got != c.want.SHA256 {
				errs = append(errs, fmt.Errorf("payload-sum: %s SHA-256 (from offset %d) is %x, CHECKSUM records %x (the BIOS loader reports this mismatch; it refuses to boot only in integrity mode enforce)", c.name, c.offset, got, c.want.SHA256))
			}
		}
	}
	return errs
}
