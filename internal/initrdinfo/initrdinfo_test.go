package initrdinfo

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildCPIOEntry appends one "newc" cpio header+name+data block
// (4-byte aligned, per the format's own spec) to buf - a from-scratch
// byte assembler, the same reasoning bios/tests/build_fat_fixtures.py's
// own header comment gives for not reusing a library that might share
// a bug with the reader under test.
func appendCPIOEntry(buf *bytes.Buffer, name string, data []byte) {
	nameZ := name + "\x00"
	header := fmt.Sprintf("070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
		0,        // ino
		0o100644, // mode
		0, 0,     // uid, gid
		1,          // nlink
		0,          // mtime
		len(data),  // filesize
		0, 0, 0, 0, // devmajor, devminor, rdevmajor, rdevminor
		len(nameZ), // namesize
		0,          // check
	)
	buf.WriteString(header)
	buf.WriteString(nameZ)
	padTo4(buf)
	buf.Write(data)
	padTo4(buf)
}

func padTo4(buf *bytes.Buffer) {
	for buf.Len()%4 != 0 {
		buf.WriteByte(0)
	}
}

func buildCPIOArchive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	for name, data := range files {
		appendCPIOEntry(&buf, name, data)
	}
	appendCPIOEntry(&buf, "TRAILER!!!", nil)
	return buf.Bytes()
}

// buildFakeKO hand-assembles a minimal, valid ELF64 relocatable
// object with exactly one real section (.modinfo) holding the given
// NUL-separated modinfo fields - just enough for debug/elf (Go
// stdlib, read-only, no writer) to parse it back, matching a real
// zfs.ko's own shape closely enough for modinfoVersion's own logic.
func buildFakeKO(t *testing.T, modinfoFields ...string) []byte {
	t.Helper()
	var modinfo bytes.Buffer
	for _, f := range modinfoFields {
		modinfo.WriteString(f)
		modinfo.WriteByte(0)
	}
	modinfoData := modinfo.Bytes()

	shstrtab := []byte("\x00.modinfo\x00.shstrtab\x00")

	const ehsize = 64
	const shentsize = 64
	modinfoOff := ehsize
	shstrtabOff := modinfoOff + len(modinfoData)
	shoff := shstrtabOff + len(shstrtab)

	buf := make([]byte, shoff+3*shentsize)

	// e_ident
	copy(buf[0:4], []byte{0x7f, 'E', 'L', 'F'})
	buf[4] = 2 // ELFCLASS64
	buf[5] = 1 // ELFDATA2LSB
	buf[6] = 1 // EV_CURRENT
	// e_type, e_machine, e_version
	binary.LittleEndian.PutUint16(buf[16:18], 1)    // ET_REL
	binary.LittleEndian.PutUint16(buf[18:20], 0x3E) // EM_X86_64
	binary.LittleEndian.PutUint32(buf[20:24], 1)    // EV_CURRENT
	// e_entry, e_phoff already zero
	binary.LittleEndian.PutUint64(buf[40:48], uint64(shoff)) // e_shoff
	binary.LittleEndian.PutUint16(buf[52:54], ehsize)        // e_ehsize
	binary.LittleEndian.PutUint16(buf[58:60], shentsize)     // e_shentsize
	binary.LittleEndian.PutUint16(buf[60:62], 3)             // e_shnum
	binary.LittleEndian.PutUint16(buf[62:64], 2)             // e_shstrndx

	copy(buf[modinfoOff:], modinfoData)
	copy(buf[shstrtabOff:], shstrtab)

	writeShdr := func(idx int, nameOff uint32, shType uint32, offset, size uint64) {
		off := shoff + idx*shentsize
		binary.LittleEndian.PutUint32(buf[off:off+4], nameOff)
		binary.LittleEndian.PutUint32(buf[off+4:off+8], shType)
		binary.LittleEndian.PutUint64(buf[off+24:off+32], offset)
		binary.LittleEndian.PutUint64(buf[off+32:off+40], size)
		binary.LittleEndian.PutUint64(buf[off+56:off+64], 1) // sh_addralign
	}
	// [0] NULL section - already all zero
	writeShdr(1, 1, 1 /* SHT_PROGBITS */, uint64(modinfoOff), uint64(len(modinfoData)))
	writeShdr(2, 10, 3 /* SHT_STRTAB */, uint64(shstrtabOff), uint64(len(shstrtab)))

	return buf
}

func TestModinfoVersion(t *testing.T) {
	ko := buildFakeKO(t, "version=2.3.1", "license=CDDL", "author=OpenZFS")
	got, err := modinfoVersion(ko)
	if err != nil {
		t.Fatalf("modinfoVersion: %v", err)
	}
	if got != "2.3.1" {
		t.Errorf("modinfoVersion = %q, want %q", got, "2.3.1")
	}
}

func TestModinfoVersion_NoVersionField(t *testing.T) {
	ko := buildFakeKO(t, "license=CDDL")
	if _, err := modinfoVersion(ko); err == nil {
		t.Error("modinfoVersion with no version= field: want an error, got nil")
	}
}

func TestParseCPIO(t *testing.T) {
	archive := buildCPIOArchive(t, map[string][]byte{
		"lib/modules/6.18.0/kernel/zfs/zfs.ko": []byte("fake-ko-content"),
		"bin/busybox":                          []byte("fake-busybox"),
	})
	entries, err := parseCPIO(archive)
	if err != nil {
		t.Fatalf("parseCPIO: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}
	found := map[string][]byte{}
	for _, e := range entries {
		found[e.name] = e.data
	}
	if string(found["lib/modules/6.18.0/kernel/zfs/zfs.ko"]) != "fake-ko-content" {
		t.Errorf("zfs.ko entry content = %q", found["lib/modules/6.18.0/kernel/zfs/zfs.ko"])
	}
	if string(found["bin/busybox"]) != "fake-busybox" {
		t.Errorf("busybox entry content = %q", found["bin/busybox"])
	}
}

func TestFindZFSModule(t *testing.T) {
	entries := []cpioEntry{
		{name: "bin/busybox", data: []byte("x")},
		{name: "lib/modules/6.18.0/kernel/zfs/zfs.ko", data: []byte("real-ko-bytes")},
	}
	got, err := findZFSModule(entries)
	if err != nil {
		t.Fatalf("findZFSModule: %v", err)
	}
	if string(got) != "real-ko-bytes" {
		t.Errorf("findZFSModule = %q, want %q", got, "real-ko-bytes")
	}
}

func TestFindZFSModule_GzipCompressed(t *testing.T) {
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write([]byte("real-ko-bytes"))
	w.Close()

	entries := []cpioEntry{
		{name: "lib/modules/6.18.0/kernel/zfs/zfs.ko.gz", data: gz.Bytes()},
	}
	got, err := findZFSModule(entries)
	if err != nil {
		t.Fatalf("findZFSModule: %v", err)
	}
	if string(got) != "real-ko-bytes" {
		t.Errorf("findZFSModule (gzip) = %q, want %q", got, "real-ko-bytes")
	}
}

func TestFindZFSModule_NotPresent(t *testing.T) {
	entries := []cpioEntry{{name: "bin/busybox", data: []byte("x")}}
	if _, err := findZFSModule(entries); err == nil {
		t.Error("findZFSModule with no zfs.ko present: want an error, got nil")
	}
}

// TestDecompress_ZstdIsIdentifiedNotSilentlyPassedThrough is the
// regression test for a real gap found in a full source audit: zstd's
// own magic bytes fell into decompress()'s default case (return data
// unchanged, no error) - a zstd-compressed initrd or zfs.ko.zst was
// silently treated as already-decompressed, producing a confusing
// generic parse error much later (an "unrecognized cpio magic" error,
// or a bare "no zfs.ko found") instead of a clear, specific one at the
// point the real cause was actually known.
func TestDecompress_ZstdIsIdentifiedNotSilentlyPassedThrough(t *testing.T) {
	zstdMagic := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x01, 0x02, 0x03}
	_, err := decompress(zstdMagic)
	if err == nil {
		t.Fatal("decompress on zstd-magic data: want an error, got nil (silently passed through unchanged)")
	}
	if !strings.Contains(err.Error(), "zstd") {
		t.Errorf("error = %q, want it to mention zstd specifically", err.Error())
	}
}

// TestDecompress_BoundEnforced_Gzip and its xz sibling below prove
// maxDecompressedSize is actually checked, for BOTH compression paths
// (the gzip path had no bound at all before this - an honest
// pre-existing gap closed alongside the xz dependency swap, not just
// something added for the new path) - a malformed or deliberately
// hostile boot artifact (a decompression bomb: a tiny compressed
// stream expanding to gigabytes) must be refused, not allowed to
// exhaust memory in a tool whose whole job is inspecting boot
// artifacts it does not control the origin of. Temporarily lowers the
// package-level bound (a var specifically so tests can do this)
// rather than genuinely allocating hundreds of megabytes to prove the
// same point.
func TestDecompress_BoundEnforced_Gzip(t *testing.T) {
	orig := maxDecompressedSize
	maxDecompressedSize = 100
	t.Cleanup(func() { maxDecompressedSize = orig })

	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(bytes.Repeat([]byte("x"), 1000)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := decompress(buf.Bytes())
	if err == nil {
		t.Fatal("decompress on a stream exceeding the bound: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %q, want it to mention the bound being exceeded", err.Error())
	}
}

func TestDecompress_BoundEnforced_XZ(t *testing.T) {
	if _, err := exec.LookPath("xz"); err != nil {
		t.Skip("xz not available in this environment to build the fixture")
	}
	orig := maxDecompressedSize
	maxDecompressedSize = 100
	t.Cleanup(func() { maxDecompressedSize = orig })

	cmd := exec.Command("xz", "-zc")
	cmd.Stdin = bytes.NewReader(bytes.Repeat([]byte("x"), 1000))
	xzData, err := cmd.Output()
	if err != nil {
		t.Fatalf("compressing fixture with xz: %v", err)
	}

	_, err = decompress(xzData)
	if err == nil {
		t.Fatal("decompress on a stream exceeding the bound: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %q, want it to mention the bound being exceeded", err.Error())
	}
}

func TestFindZFSModule_ZstdCompressedIsNamedNotJustNotFound(t *testing.T) {
	entries := []cpioEntry{
		{name: "lib/modules/6.18.0/kernel/zfs/zfs.ko.zst", data: []byte{0x28, 0xb5, 0x2f, 0xfd, 0, 0, 0}},
	}
	_, err := findZFSModule(entries)
	if err == nil {
		t.Fatal("findZFSModule with a zstd-compressed zfs.ko.zst: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "zstd") {
		t.Errorf("error = %q, want it to mention zstd specifically (found the file, just can't decompress it) rather than a generic 'not found'", err.Error())
	}
}

func TestExtractOpenZFSVersion_EndToEnd(t *testing.T) {
	ko := buildFakeKO(t, "version=2.3.1", "license=CDDL")
	archive := buildCPIOArchive(t, map[string][]byte{
		"lib/modules/6.18.0/kernel/zfs/zfs.ko": ko,
		"bin/busybox":                          []byte("fake-busybox"),
	})

	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write(archive)
	w.Close()

	path := filepath.Join(t.TempDir(), "initrd")
	if err := os.WriteFile(path, gz.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ExtractOpenZFSVersion(path)
	if err != nil {
		t.Fatalf("ExtractOpenZFSVersion: %v", err)
	}
	if got != "2.3.1" {
		t.Errorf("ExtractOpenZFSVersion = %q, want %q", got, "2.3.1")
	}
}

func TestExtractOpenZFSVersion_XZEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("xz"); err != nil {
		t.Skip("xz not available in this environment")
	}
	ko := buildFakeKO(t, "version=2.2.99")
	archive := buildCPIOArchive(t, map[string][]byte{
		"lib/modules/6.18.0/kernel/zfs/zfs.ko": ko,
	})

	cmd := exec.Command("xz", "-zc")
	cmd.Stdin = bytes.NewReader(archive)
	xzData, err := cmd.Output()
	if err != nil {
		t.Fatalf("compressing fixture with xz: %v", err)
	}

	path := filepath.Join(t.TempDir(), "initrd")
	if err := os.WriteFile(path, xzData, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ExtractOpenZFSVersion(path)
	if err != nil {
		t.Fatalf("ExtractOpenZFSVersion (xz): %v", err)
	}
	if got != "2.2.99" {
		t.Errorf("ExtractOpenZFSVersion (xz) = %q, want %q", got, "2.2.99")
	}
}

// TestExtractOpenZFSVersion_XZWithNoXZExecutableAvailable is the direct
// regression test for a real bug, found on real hardware: a genuinely
// minimal Alpine install (a real Hetzner CAX aarch64 machine, no `xz`
// package present) made `status` fail outright with `exec: "xz":
// executable file not found in $PATH`, even though the machine's own
// real initrd (always xz-compressed - see this package's own header
// comment) was otherwise perfectly readable. decompress()'s xz path is
// now an in-process pure-Go decoder (ulikunitz/xz), not a shelled-out
// `xz` binary - this test proves that directly, not by inference: the
// fixture is compressed using this sandbox's own real `xz` tool (the
// same convenience the sibling End-to-end test above uses), but PATH
// is then reset to a directory containing no executables at all
// BEFORE the actual decompression call this test asserts on, so any
// accidental regression back to exec.Command("xz", ...) would fail
// here with exactly the same "executable file not found" error the
// real machine hit - not skip, not pass by accident.
func TestExtractOpenZFSVersion_XZWithNoXZExecutableAvailable(t *testing.T) {
	if _, err := exec.LookPath("xz"); err != nil {
		t.Skip("xz not available in this environment to build the fixture")
	}
	ko := buildFakeKO(t, "version=2.4.4-1")
	archive := buildCPIOArchive(t, map[string][]byte{
		"lib/modules/6.18.0/kernel/zfs/zfs.ko": ko,
	})

	cmd := exec.Command("xz", "-zc")
	cmd.Stdin = bytes.NewReader(archive)
	xzData, err := cmd.Output()
	if err != nil {
		t.Fatalf("compressing fixture with xz: %v", err)
	}

	path := filepath.Join(t.TempDir(), "initrd")
	if err := os.WriteFile(path, xzData, 0o644); err != nil {
		t.Fatal(err)
	}

	// An empty, real directory - not "" (which would fall back to the
	// shell's own default search path on some platforms) and not a
	// nonexistent path (exec.LookPath's own behavior for that case is
	// less clearly "nothing is ever found" than an empty real dir).
	emptyPath := t.TempDir()
	t.Setenv("PATH", emptyPath)
	if _, err := exec.LookPath("xz"); err == nil {
		t.Fatal("test setup broken - xz is still findable with PATH cleared")
	}

	got, err := ExtractOpenZFSVersion(path)
	if err != nil {
		t.Fatalf("ExtractOpenZFSVersion (xz, no xz executable in PATH): %v - decompress() must not shell out to xz", err)
	}
	if got != "2.4.4-1" {
		t.Errorf("ExtractOpenZFSVersion (xz, no xz executable in PATH) = %q, want %q", got, "2.4.4-1")
	}
}

// buildOrderedCPIOArchive is buildCPIOArchive's ordered sibling - a map
// iterates in randomized order, which is fine for correctness-only
// tests but useless for the position-dependent streaming tests below,
// which need zfs.ko deliberately placed first/middle/last among other
// entries.
func buildOrderedCPIOArchive(t *testing.T, entries []struct {
	name string
	data []byte
}) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, e := range entries {
		appendCPIOEntry(&buf, e.name, e.data)
	}
	appendCPIOEntry(&buf, "TRAILER!!!", nil)
	return buf.Bytes()
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return gz.Bytes()
}

// TestExtractOpenZFSVersionFromBytes_PositionIndependent is the direct
// regression test for streamFindZFSModule: the correct version must be
// found regardless of where zfs.ko sits in the archive relative to
// other, larger, non-matching entries - first, middle, or last.
func TestExtractOpenZFSVersionFromBytes_PositionIndependent(t *testing.T) {
	ko := buildFakeKO(t, "version=2.4.4-1", "license=CDDL")
	filler := func(n int) []byte { return bytes.Repeat([]byte("x"), n) }

	for _, pos := range []string{"first", "middle", "last"} {
		t.Run(pos, func(t *testing.T) {
			var entries []struct {
				name string
				data []byte
			}
			koEntry := struct {
				name string
				data []byte
			}{"lib/modules/6.18.0/kernel/zfs/zfs.ko", ko}
			other1 := struct {
				name string
				data []byte
			}{"bin/busybox", filler(4096)}
			other2 := struct {
				name string
				data []byte
			}{"usr/lib/libc.so", filler(4096)}

			switch pos {
			case "first":
				entries = []struct {
					name string
					data []byte
				}{koEntry, other1, other2}
			case "middle":
				entries = []struct {
					name string
					data []byte
				}{other1, koEntry, other2}
			case "last":
				entries = []struct {
					name string
					data []byte
				}{other1, other2, koEntry}
			}

			archive := buildOrderedCPIOArchive(t, entries)
			got, err := ExtractOpenZFSVersionFromBytes(gzipBytes(t, archive))
			if err != nil {
				t.Fatalf("ExtractOpenZFSVersionFromBytes (zfs.ko %s): %v", pos, err)
			}
			if got != "2.4.4-1" {
				t.Errorf("ExtractOpenZFSVersionFromBytes (zfs.ko %s) = %q, want %q", pos, got, "2.4.4-1")
			}
		})
	}
}

// TestExtractOpenZFSVersionFromBytes_StreamingStopsAtMatch is the
// direct regression/negative-control test proving streamFindZFSModule
// genuinely stops reading once zfs.ko has been found, rather than
// merely happening to still work when the rest of the archive is
// well-formed: everything AFTER the matching zfs.ko entry is corrupted
// beyond any possible valid parse (a bad cpio magic). If
// ExtractOpenZFSVersionFromBytes still succeeds, the implementation
// provably never looked at those bytes - if it read the old, whole-
// buffer-first way (decompress everything, THEN parse), this would
// fail with a cpio parse error instead.
func TestExtractOpenZFSVersionFromBytes_StreamingStopsAtMatch(t *testing.T) {
	ko := buildFakeKO(t, "version=2.4.4-1")
	var buf bytes.Buffer
	appendCPIOEntry(&buf, "bin/busybox", bytes.Repeat([]byte("x"), 4096))
	appendCPIOEntry(&buf, "lib/modules/6.18.0/kernel/zfs/zfs.ko", ko)
	// Deliberately corrupt: not a valid cpio header, not TRAILER!!!, not
	// anything parseCPIO/streamFindZFSModule could ever successfully
	// consume.
	buf.WriteString("THIS IS NOT A VALID CPIO HEADER AT ALL, GARBAGE GARBAGE GARBAGE")

	got, err := ExtractOpenZFSVersionFromBytes(gzipBytes(t, buf.Bytes()))
	if err != nil {
		t.Fatalf("ExtractOpenZFSVersionFromBytes with corrupted trailing data: %v - streaming must stop at the match, not read past it", err)
	}
	if got != "2.4.4-1" {
		t.Errorf("ExtractOpenZFSVersionFromBytes = %q, want %q", got, "2.4.4-1")
	}
}

// TestExtractOpenZFSVersionFromBytes_StreamingBoundEnforced is
// streamFindZFSModule's own version of TestDecompress_BoundEnforced_*
// above: a stream whose bytes BEFORE any zfs.ko match exceed
// maxDecompressedSize must be refused cleanly, not read forever
// looking for a match that may not even exist. Uses non-matching
// filler entries (never found, so the whole archive must be scanned)
// deliberately sized past the temporarily-lowered bound.
func TestExtractOpenZFSVersionFromBytes_StreamingBoundEnforced(t *testing.T) {
	orig := maxDecompressedSize
	maxDecompressedSize = 1000
	t.Cleanup(func() { maxDecompressedSize = orig })

	var buf bytes.Buffer
	appendCPIOEntry(&buf, "bin/busybox", bytes.Repeat([]byte("x"), 5000)) // past the 1000-byte bound
	appendCPIOEntry(&buf, "TRAILER!!!", nil)

	_, err := ExtractOpenZFSVersionFromBytes(gzipBytes(t, buf.Bytes()))
	if err == nil {
		t.Fatal("ExtractOpenZFSVersionFromBytes with a stream exceeding the bound: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %q, want it to mention the bound being exceeded", err.Error())
	}
}

// TestExtractOpenZFSVersionFromBytes_StreamingMatchesWholeBufferParse
// is the negative-facing control tying streamFindZFSModule directly to
// the reference whole-buffer implementation (parseCPIO+findZFSModule):
// given the SAME real archive, both must find the exact same zfs.ko
// bytes (and therefore the same modinfo version) - not just "some"
// version, provably the SAME one.
func TestExtractOpenZFSVersionFromBytes_StreamingMatchesWholeBufferParse(t *testing.T) {
	ko := buildFakeKO(t, "version=9.9.9-rc1", "license=CDDL", "author=OpenZFS")
	archive := buildCPIOArchive(t, map[string][]byte{
		"lib/modules/6.18.0/kernel/zfs/zfs.ko": ko,
		"bin/busybox":                          []byte("fake-busybox"),
		"usr/sbin/dropbear":                    bytes.Repeat([]byte("d"), 2048),
	})

	entries, err := parseCPIO(archive)
	if err != nil {
		t.Fatal(err)
	}
	wantKo, err := findZFSModule(entries)
	if err != nil {
		t.Fatal(err)
	}
	wantVersion, err := modinfoVersion(wantKo)
	if err != nil {
		t.Fatal(err)
	}

	gotVersion, err := ExtractOpenZFSVersionFromBytes(gzipBytes(t, archive))
	if err != nil {
		t.Fatal(err)
	}
	if gotVersion != wantVersion {
		t.Errorf("streaming found version %q, whole-buffer reference found %q - want identical", gotVersion, wantVersion)
	}
}

// rawCPIOHeader hand-builds one 110-byte "newc" header with EXACTLY the
// namesize/filesize asked for, without writing the matching name/data
// bytes - appendCPIOEntry above always derives both fields from real
// content it then actually writes, which is precisely what the two
// tests below must NOT do: they need a header whose declared sizes are
// a lie the parser has to survive being told.
func rawCPIOHeader(namesize, filesize uint32) []byte {
	return []byte(fmt.Sprintf("070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
		0,        // ino
		0o100644, // mode
		0, 0,     // uid, gid
		1,          // nlink
		0,          // mtime
		filesize,   // filesize - deliberately NOT the length of anything real
		0, 0, 0, 0, // devmajor, devminor, rdevmajor, rdevminor
		namesize, // namesize - likewise
		0,        // check
	))
}

// allocDelta reports how many bytes fn caused to be allocated, via
// runtime.MemStats.TotalAlloc - which is cumulative and monotonic, so
// the delta is exact regardless of whether a GC happens in between (a
// HeapAlloc-based measurement would not be). This is the discriminating
// assertion for the two tests below: an "did it return the right error"
// check alone passes BEFORE the fix too (the pre-fix code allocated the
// oversized buffer and THEN failed with the same bound error), so only
// measuring the allocation itself can tell a real fix from a cosmetic
// one - exactly the trap this project's own "negative control" rule
// exists to catch.
func allocDelta(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// oversizedDeclaredSize is what the two tests below declare in their
// crafted cpio headers: 64 MiB, far past the temporarily-lowered
// maxDecompressedSize, comfortably above allocPreallocCeiling's own
// noise floor, and still small enough to actually allocate on any test
// machine (so the PRE-fix run fails the assertion cleanly instead of
// OOM-killing the test binary and proving nothing). The real hex8 field
// allows up to 0xFFFFFFFF - a full 4 GiB - which is what makes this a
// production hazard rather than a curiosity.
const oversizedDeclaredSize = 64 * 1024 * 1024

// allocPreallocCeiling is the largest allocation either test below
// tolerates. Well above the few KiB of genuine parser/error-formatting
// noise, and 16x below oversizedDeclaredSize - no tuning-sensitive
// middle ground.
const allocPreallocCeiling = 4 * 1024 * 1024

// TestStreamFindZFSModule_HugeNamesizeIsRefusedBeforeAllocating covers
// the worse of the two: namesize is read and allocated for EVERY entry,
// so a single corrupt 110-byte header at the very start of the stream
// is enough. On a rescue initramfs (the environment this tool exists
// for, with only the machine's RAM and no swap) a 4 GiB allocation is
// an OOM kill of `alpine-zfsboot status`, triggered by exactly the
// corrupt-ESP input an operator runs status to diagnose.
func TestStreamFindZFSModule_HugeNamesizeIsRefusedBeforeAllocating(t *testing.T) {
	orig := maxDecompressedSize
	maxDecompressedSize = 4096
	t.Cleanup(func() { maxDecompressedSize = orig })

	// A header declaring a 64 MiB name, and then nothing at all - the
	// stream is truncated immediately after the header.
	raw := rawCPIOHeader(oversizedDeclaredSize, 0)

	var err error
	delta := allocDelta(func() { _, err = ExtractOpenZFSVersionFromBytes(raw) })

	if err == nil {
		t.Fatal("ExtractOpenZFSVersionFromBytes with a 64 MiB declared namesize: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %q, want it to mention the bound being exceeded", err.Error())
	}
	if delta > allocPreallocCeiling {
		t.Errorf("parsing a header with a %d-byte declared namesize allocated %d bytes (ceiling %d) - the size is being trusted to size a buffer BEFORE it is bounds-checked", oversizedDeclaredSize, delta, allocPreallocCeiling)
	}
}

// TestStreamFindZFSModule_HugeFilesizeIsRefusedBeforeAllocating is the
// same hazard on the matched-entry path: once the name matches
// zfsKoSuffixes, filesize sizes the buffer the entry is read into.
func TestStreamFindZFSModule_HugeFilesizeIsRefusedBeforeAllocating(t *testing.T) {
	orig := maxDecompressedSize
	maxDecompressedSize = 4096
	t.Cleanup(func() { maxDecompressedSize = orig })

	// "/zfs.ko\x00" is 8 bytes, so namesize is honest here; only
	// filesize lies. 110 + 8 = 118, which needs 2 bytes of padding to
	// reach the next 4-byte boundary.
	name := "/zfs.ko\x00"
	var buf bytes.Buffer
	buf.Write(rawCPIOHeader(uint32(len(name)), oversizedDeclaredSize))
	buf.WriteString(name)
	buf.Write([]byte{0, 0}) // name padding to the 4-byte boundary
	// ...and then nothing: the declared 64 MiB of file data never follows.

	var err error
	raw := buf.Bytes()
	delta := allocDelta(func() { _, err = ExtractOpenZFSVersionFromBytes(raw) })

	if err == nil {
		t.Fatal("ExtractOpenZFSVersionFromBytes with a 64 MiB declared filesize: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %q, want it to mention the bound being exceeded", err.Error())
	}
	if delta > allocPreallocCeiling {
		t.Errorf("parsing a matched zfs.ko entry with a %d-byte declared filesize allocated %d bytes (ceiling %d) - the size is being trusted to size a buffer BEFORE it is bounds-checked", oversizedDeclaredSize, delta, allocPreallocCeiling)
	}
}
