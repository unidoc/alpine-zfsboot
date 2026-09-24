// Package initrdinfo extracts the installed OpenZFS module version
// from an alpine-zfsboot initrd - NOT from the Linux kernel version
// (see this package's own ExtractOpenZFSVersion doc comment): kernel
// and OpenZFS versions are independent facts, and stage1/stage2/the
// UEFI loader never touch ZFS at all - only the initrd's own zfs.ko
// determines what pool feature set can actually be imported.
//
// Pipeline: decompress the initrd, parse the resulting cpio "newc"
// archive (mkinitfs's own output format, the standard Linux initramfs
// archive format), find zfs.ko (decompressing it too if it is itself
// .gz/.xz-compressed - some distros compress individual kernel
// modules), then read its ELF .modinfo section - the exact same
// section `modinfo`/`modprobe` themselves read, a NUL-separated list
// of "key=value" strings including "version=X.Y.Z".
//
// Decompression is STREAMED directly into the cpio parser, which
// stops reading the moment zfs.ko has been fully consumed - it never
// materializes the whole decompressed initrd into memory first. This
// matters: a real ~30MB rescue-image-shaped archive with zfs.ko near
// the end still costs a full decompression (streaming saves nothing
// if the target entry is the very last one), but with zfs.ko anywhere
// earlier the saving is substantial and, for a real mkinitfs build
// whose own feature order lists "zfs" second (see build.sh's own
// mkinitfs invocation), plausible - and it is never WORSE than the
// old whole-archive-first approach, only ever equal or better.
//
// build.sh's own rescue initramfs is gzip-compressed as of this
// writing (Go's stdlib compress/gzip, benchmarked directly against
// this project's own pure-Go xz decoder on identical realistic
// content: ~172MB/s vs ~37MB/s, a ~4.6x gap, entirely inherent to the
// LZMA algorithm's own bit-level arithmetic decoding - confirmed via
// `go tool pprof`, not a usage inefficiency in how this package drove
// ulikunitz/xz - see the hardening ledger's own entry for the full
// benchmark). xz decoding is KEPT here regardless, in-process
// (github.com/ulikunitz/xz, not a shelled-out `xz` binary - a real
// Hetzner CAX aarch64 machine, running a genuinely minimal Alpine
// install with no `xz` package present, proved the shelled-out
// version unacceptable the hard way: `status` failed outright with
// `exec: "xz": executable file not found in $PATH`) purely for
// backward compatibility with already-deployed xz-compressed
// artifacts (uniclus-01, CAX) built before this switch - both formats
// are detected automatically by magic bytes, never guessed from a
// file extension or a build-time flag.
package initrdinfo

import (
	"bytes"
	"compress/gzip"
	"debug/elf"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/ulikunitz/xz"
)

// maxDecompressedSize bounds the total bytes this package will ever
// read out of a decompressing reader before giving up - a real,
// deliberate defense against a decompression bomb (a tiny, cheaply-
// crafted compressed stream that expands to gigabytes) or a
// pathologically large/corrupt cpio stream with no real zfs.ko entry.
// Enforced incrementally (via a shared io.LimitedReader) rather than
// via one big ReadAll, since streamFindZFSModule below never
// materializes the whole stream at once. 512MiB is generous headroom
// over any real artifact this package ever legitimately decompresses
// (a full Alpine initramfs with ZFS support is tens of MiB; a single
// kernel module is smaller still) while still bounding what a
// malformed or deliberately hostile boot artifact - exactly the kind
// of input a recovery/introspection tool must expect to encounter -
// could force this process to allocate.
// var, not const: a test proving the bound is actually enforced needs
// to lower it temporarily, rather than genuinely allocating half a
// gigabyte just to exercise one comparison.
var maxDecompressedSize int64 = 512 * 1024 * 1024

// readAllBounded is io.ReadAll with maxDecompressedSize enforced via
// io.LimitReader - reads one byte past the cap so a stream that is
// EXACTLY the cap in size is still accepted (the cap itself is never
// truncated data), while anything larger is reported as refused
// rather than silently truncated (a truncated cpio archive or ELF
// object would fail its own later parse anyway, but with a confusing,
// unrelated error instead of this function's own clear one).
func readAllBounded(r io.Reader, what string) ([]byte, error) {
	out, err := io.ReadAll(io.LimitReader(r, maxDecompressedSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > maxDecompressedSize {
		return nil, fmt.Errorf("%s exceeds the %d-byte decompression bound - refusing (malformed or hostile input?)", what, maxDecompressedSize)
	}
	return out, nil
}

// decompressReader detects the compression format of data by its
// magic bytes and returns a streaming reader over the decompressed
// content - gzip via Go's stdlib, xz via ulikunitz/xz (see this
// package's own header comment for why both are kept). Returns a
// plain reader over data unchanged, with no error, if it doesn't
// match either known magic (the common case for an already-
// decompressed cpio archive, or an uncompressed .ko).
func decompressReader(data []byte) (io.Reader, error) {
	switch {
	case len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b: // gzip
		r, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("opening gzip stream: %w", err)
		}
		return r, nil
	case len(data) >= 6 && bytes.Equal(data[:6], []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}): // xz
		r, err := xz.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("opening xz stream: %w", err)
		}
		return r, nil
	case len(data) >= 4 && bytes.Equal(data[:4], []byte{0x28, 0xb5, 0x2f, 0xfd}): // zstd
		// Explicitly identified, not decompressed - a full source audit
		// found the previous default: case silently passed zstd-
		// compressed data through UNCHANGED, which then failed later
		// with a confusing "unrecognized cpio magic" error (or, for a
		// zstd-compressed zfs.ko specifically, a bare "no zfs.ko found")
		// - neither one told the operator anything about the real,
		// specific cause. zstd IS a real possibility this package's own
		// header comment already acknowledged as a risk (a TARGET
		// system's separately-built initramfs, or a kernel with
		// CONFIG_MODULE_COMPRESS_ZSTD) - genuinely decompressing it
		// would need a new `zstd` binary dependency this project
		// doesn't currently declare anywhere (build.sh's own package
		// list, CI), a bigger, separate decision than this fix -
		// reporting the real cause clearly, instead of silently mis-
		// decompressing, is the narrow, no-new-dependency half of that.
		return nil, fmt.Errorf("data is zstd-compressed - not supported (no zstd decoder dependency; only gzip and xz are)")
	default:
		return bytes.NewReader(data), nil
	}
}

// decompress fully materializes decompressReader's own output, bounded
// - used for the (small) matched zfs.ko module's own bytes, where
// streaming buys nothing (there's nothing further to look for once
// it's found) and a plain []byte is the simplest shape for
// modinfoVersion's own debug/elf parsing.
func decompress(data []byte) ([]byte, error) {
	r, err := decompressReader(data)
	if err != nil {
		return nil, err
	}
	if rc, ok := r.(io.Closer); ok {
		defer rc.Close()
	}
	out, err := readAllBounded(r, "decompressed stream")
	if err != nil {
		return nil, fmt.Errorf("decompressing: %w", err)
	}
	return out, nil
}

// cpioEntry is one file's name and (already-decompressed-if-relevant)
// content from a parsed cpio archive.
type cpioEntry struct {
	name string
	data []byte
}

// cpioHeaderSize is the fixed size of a "newc"/"crc" cpio header -
// shared by both parseCPIO (whole-buffer, used by this file's own
// tests) and streamFindZFSModule (streaming) below, so the two never
// drift apart on this one fact.
const cpioHeaderSize = 110

// parseCPIO parses a "newc" (magic "070701", the ASCII/no-checksum
// variant mkinitfs and every modern Linux initramfs tool produces)
// cpio archive - fixed 110-byte ASCII-hex headers, filename, then
// file data, each of the two variable-length parts padded to a
// 4-byte boundary measured from the start of the whole archive - per
// the format's own spec. Stops at the standard "TRAILER!!!" end
// marker.
//
// Kept as a whole-buffer parser (not used by ExtractOpenZFSVersion*
// itself any more - see streamFindZFSModule below) purely because it's
// a simpler, independently-useful reference implementation this
// package's own tests check the streaming parser's behavior against.
func parseCPIO(data []byte) ([]cpioEntry, error) {
	var entries []cpioEntry
	pos := 0
	for {
		if pos+cpioHeaderSize > len(data) {
			return nil, fmt.Errorf("cpio archive truncated at offset %d (no room for a header)", pos)
		}
		header := data[pos : pos+cpioHeaderSize]
		magic := string(header[0:6])
		if magic != "070701" && magic != "070702" {
			return nil, fmt.Errorf("unrecognized cpio magic %q at offset %d - not a newc/crc archive", magic, pos)
		}
		filesize, err := hex8(header[54:62])
		if err != nil {
			return nil, fmt.Errorf("parsing filesize at offset %d: %w", pos, err)
		}
		namesize, err := hex8(header[94:102])
		if err != nil {
			return nil, fmt.Errorf("parsing namesize at offset %d: %w", pos, err)
		}

		nameStart := pos + cpioHeaderSize
		nameEnd := nameStart + int(namesize)
		if nameEnd > len(data) {
			return nil, fmt.Errorf("cpio archive truncated at offset %d (name extends past EOF)", nameStart)
		}
		name := string(bytes.TrimRight(data[nameStart:nameEnd], "\x00"))

		dataStart := align4(nameEnd)
		dataEnd := dataStart + int(filesize)
		if dataEnd > len(data) {
			return nil, fmt.Errorf("cpio archive truncated at offset %d (file data for %q extends past EOF)", dataStart, name)
		}

		if name == "TRAILER!!!" {
			break
		}
		entries = append(entries, cpioEntry{name: name, data: data[dataStart:dataEnd]})
		pos = align4(dataEnd)
	}
	return entries, nil
}

func align4(n int) int {
	if r := n % 4; r != 0 {
		return n + (4 - r)
	}
	return n
}

func hex8(b []byte) (uint32, error) {
	v, err := strconv.ParseUint(string(b), 16, 32)
	if err != nil {
		return 0, err
	}
	return uint32(v), nil
}

// zfsKoSuffixes are the name suffixes findZFSModule/streamFindZFSModule
// both look for - any of the plain/.gz/.xz/.zst forms a distro's own
// module-compression policy might use (CONFIG_MODULE_COMPRESS_ZSTD is
// a real, real-world kernel option, not a hypothetical one, even
// though decompress() itself only reports it clearly rather than
// actually decompressing it - see decompressReader's own comment). One
// shared slice so both the whole-buffer and streaming implementations
// agree on exactly what counts as a match.
var zfsKoSuffixes = []string{"/zfs.ko", "/zfs.ko.gz", "/zfs.ko.xz", "/zfs.ko.zst"}

func matchesZFSKoSuffix(name string) bool {
	for _, suf := range zfsKoSuffixes {
		if len(name) >= len(suf) && name[len(name)-len(suf):] == suf {
			return true
		}
	}
	return false
}

// findZFSModule locates zfs.ko among entries (from parseCPIO) and
// returns its content, already decompressed. Returns an error naming
// what it DID find (e.g. a differently-compressed form this package
// doesn't fully handle) rather than a bare "not found" when something
// zfs.ko-shaped exists but couldn't be read - see this function's own
// error messages. Kept alongside parseCPIO as the reference
// implementation streamFindZFSModule's own tests check against; not
// used by ExtractOpenZFSVersion* any more.
func findZFSModule(entries []cpioEntry) ([]byte, error) {
	for _, e := range entries {
		if matchesZFSKoSuffix(e.name) {
			data, err := decompress(e.data)
			if err != nil {
				return nil, fmt.Errorf("decompressing %s: %w", e.name, err)
			}
			return data, nil
		}
	}
	return nil, fmt.Errorf("no zfs.ko found in the initrd (checked for %v)", zfsKoSuffixes)
}

// streamFindZFSModule is ExtractOpenZFSVersionFromBytes' own real
// implementation: reads r (a decompressing io.Reader, from
// decompressReader) as a "newc" cpio stream and returns the bytes of
// the first entry matching zfsKoSuffixes, WITHOUT reading past that
// entry - every entry before it is read (headers+names, cheap) or
// skipped (file data of non-matching entries, via io.CopyN to
// io.Discard - the underlying decompressor still has to do that work,
// there's no way around decoding bytes sequentially, but this package
// never allocates or copies them into a big buffer for it), and
// NOTHING after the match is touched at all. Benchmarked directly
// against this project's own realistic-content fixtures: with the
// match near the start of the archive, over 25x faster than fully
// decompressing first; with it at the very end, no better (and no
// worse) than the old approach - see the hardening ledger's own entry
// for the numbers.
//
// Errors and bounds are intended to match parseCPIO/findZFSModule's
// own behavior (same format, same checks, just walked incrementally
// instead of over an already-materialized slice) - but "intended" is
// the honest word: a later audit found this claim had NOT held for
// allocation bounds, which is why checkDeclaredSize below exists and
// why both of its call sites carry the full reasoning inline. The
// structural difference is real and permanent: parseCPIO always has
// the whole archive in hand, so every size it reads can be compared
// against a known len(data) and every "read" is a sub-slice that
// allocates nothing; this parser has only a budget and must therefore
// check each declared size against that budget EXPLICITLY, before
// acting on it.
func streamFindZFSModule(r io.Reader) ([]byte, error) {
	lr := &io.LimitedReader{R: r, N: maxDecompressedSize + 1}
	hdr := make([]byte, cpioHeaderSize)
	for {
		if err := readFullBounded(lr, hdr, "decompressed stream"); err != nil {
			return nil, fmt.Errorf("reading cpio header: %w", err)
		}
		magic := string(hdr[0:6])
		if magic != "070701" && magic != "070702" {
			return nil, fmt.Errorf("unrecognized cpio magic %q - not a newc/crc archive", magic)
		}
		filesize, err := hex8(hdr[54:62])
		if err != nil {
			return nil, fmt.Errorf("parsing filesize: %w", err)
		}
		namesize, err := hex8(hdr[94:102])
		if err != nil {
			return nil, fmt.Errorf("parsing namesize: %w", err)
		}

		// A full source audit found a real regression the streaming
		// rewrite introduced that parseCPIO above never had: namesize
		// and filesize are attacker-shaped uint32 header fields (hex8
		// accepts anything up to 0xFFFFFFFF - a full 4 GiB) and were
		// used to size a make() BEFORE anything consulted the
		// decompression budget, so a single corrupt 110-byte cpio
		// header was enough to make this process try to allocate 4 GiB.
		// parseCPIO never could: it slices an already-materialized
		// buffer behind an explicit `> len(data)` check, allocating
		// nothing. On a rescue initramfs - the environment this whole
		// tool exists for, with only the machine's RAM and no swap -
		// that is an OOM kill of `alpine-zfsboot status`, triggered by
		// exactly the corrupt-ESP input an operator runs status to
		// diagnose in the first place.
		//
		// lr.N is the decompression budget still REMAINING (see
		// maxDecompressedSize). A declared size larger than what's left
		// could never have been satisfied anyway - the read was always
		// going to fail - so refusing it here is behavior-preserving
		// for every real archive and merely moves the refusal to before
		// the allocation instead of after it. It also turns what used
		// to surface as a bare, misleading "EOF" ("truncated archive")
		// into this package's own clear, specific bound message.
		if err := checkDeclaredSize(int64(namesize), lr, "cpio entry name"); err != nil {
			return nil, err
		}
		nameBuf := make([]byte, namesize)
		if err := readFullBounded(lr, nameBuf, "decompressed stream"); err != nil {
			return nil, fmt.Errorf("reading cpio entry name: %w", err)
		}
		name := string(bytes.TrimRight(nameBuf, "\x00"))

		namePad := align4(cpioHeaderSize+int(namesize)) - (cpioHeaderSize + int(namesize))
		if namePad > 0 {
			if err := discardBounded(lr, int64(namePad)); err != nil {
				return nil, fmt.Errorf("skipping name padding for %q: %w", name, err)
			}
		}

		if name == "TRAILER!!!" {
			return nil, fmt.Errorf("no zfs.ko found in the initrd (checked for %v)", zfsKoSuffixes)
		}

		if matchesZFSKoSuffix(name) {
			// Same guard, same reasoning as the namesize one above -
			// this is the other allocation whose size comes straight
			// out of the header.
			if err := checkDeclaredSize(int64(filesize), lr, "cpio entry "+name); err != nil {
				return nil, err
			}
			data := make([]byte, filesize)
			if err := readFullBounded(lr, data, "decompressed stream"); err != nil {
				return nil, fmt.Errorf("reading %s: %w", name, err)
			}
			out, err := decompress(data)
			if err != nil {
				return nil, fmt.Errorf("decompressing %s: %w", name, err)
			}
			return out, nil
		}

		if err := discardBounded(lr, int64(align4(int(filesize)))); err != nil {
			return nil, fmt.Errorf("skipping %s: %w", name, err)
		}
	}
}

// checkDeclaredSize refuses a length a cpio header DECLARES that is
// already larger than the decompression budget lr has left, before that
// length is ever used to size an allocation. See streamFindZFSModule's
// own call sites for the real bug this closes; the separate function
// exists so both call sites are provably running the identical check
// rather than two hand-copied comparisons that could drift.
func checkDeclaredSize(declared int64, lr *io.LimitedReader, what string) error {
	if declared > lr.N {
		return fmt.Errorf("%s declares %d bytes, which exceeds the %d-byte decompression bound - refusing before allocating (malformed or hostile input?)", what, declared, maxDecompressedSize)
	}
	return nil
}

// readFullBounded is io.ReadFull against lr, translating "the shared
// bound was hit" into the same clear, specific error readAllBounded's
// own callers get (io.LimitedReader itself just reports a generic EOF
// once its budget is exhausted, which would otherwise read as
// "truncated archive" - misleading for what's actually a refused
// oversized input, not a malformed one).
func readFullBounded(lr *io.LimitedReader, buf []byte, what string) error {
	_, err := io.ReadFull(lr, buf)
	if err == nil {
		return nil
	}
	if lr.N <= 0 {
		return fmt.Errorf("%s exceeds the %d-byte decompression bound - refusing (malformed or hostile input?)", what, maxDecompressedSize)
	}
	return err
}

// discardBounded is readFullBounded's io.CopyN-shaped sibling, for
// skipping padding/non-matching file data without keeping it.
func discardBounded(lr *io.LimitedReader, n int64) error {
	copied, err := io.CopyN(io.Discard, lr, n)
	if err == nil {
		return nil
	}
	if lr.N <= 0 && copied < n {
		return fmt.Errorf("decompressed stream exceeds the %d-byte decompression bound - refusing (malformed or hostile input?)", maxDecompressedSize)
	}
	return err
}

// modinfoVersion reads koData as an ELF object and extracts the
// "version=" entry from its .modinfo section - the exact section
// `modinfo`/`modprobe` themselves read (a sequence of NUL-separated
// "key=value" ASCII strings, not a struct - see the Linux kernel's
// own MODULE_INFO() macro, which is what populates it at compile
// time).
func modinfoVersion(koData []byte) (string, error) {
	f, err := elf.NewFile(bytes.NewReader(koData))
	if err != nil {
		return "", fmt.Errorf("parsing zfs.ko as ELF: %w", err)
	}
	defer f.Close()

	sec := f.Section(".modinfo")
	if sec == nil {
		return "", fmt.Errorf("zfs.ko has no .modinfo section")
	}
	data, err := sec.Data()
	if err != nil {
		return "", fmt.Errorf("reading .modinfo section: %w", err)
	}

	for _, field := range bytes.Split(data, []byte{0}) {
		if bytes.HasPrefix(field, []byte("version=")) {
			return string(field[len("version="):]), nil
		}
	}
	return "", fmt.Errorf("zfs.ko's .modinfo section has no \"version=\" entry")
}

// ExtractOpenZFSVersion returns the OpenZFS version string baked into
// initrdPath's own zfs.ko kernel module - deliberately NOT derived
// from the Linux kernel version (a different, independent fact - see
// this package's own doc comment): this is the real determinant of
// which ZFS pool feature flags the installed boot environment can
// safely import, which is why alpine-zfsboot status/verify report it
// separately from the kernel version (see internal/kernelinfo).
func ExtractOpenZFSVersion(initrdPath string) (string, error) {
	raw, err := os.ReadFile(initrdPath)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", initrdPath, err)
	}
	v, err := ExtractOpenZFSVersionFromBytes(raw)
	if err != nil {
		return "", fmt.Errorf("%s: %w", initrdPath, err)
	}
	return v, nil
}

// ExtractOpenZFSVersionFromBytes is ExtractOpenZFSVersion's actual
// logic, over already-read bytes - works identically whether those
// bytes came from a standalone initrd FILE (ExtractOpenZFSVersion,
// BIOS's own case - see internal/layout.InitrdFile) or from a UEFI
// .EFI's own embedded ".initrd" PE section (build.sh's objcopy copies
// the same cpio/xz|gzip bytes verbatim into that section - see
// internal/cmdline's own doc comment on that embedding), so cmd/tool's
// UEFI status path can feed this the section's own bytes directly
// without a temp file. Streams decompression directly into the cpio
// parser (see streamFindZFSModule's own doc comment) rather than
// materializing the whole decompressed initrd first.
func ExtractOpenZFSVersionFromBytes(raw []byte) (string, error) {
	r, err := decompressReader(raw)
	if err != nil {
		return "", fmt.Errorf("decompressing: %w", err)
	}
	if rc, ok := r.(io.Closer); ok {
		defer rc.Close()
	}
	koData, err := streamFindZFSModule(r)
	if err != nil {
		return "", fmt.Errorf("parsing as a cpio archive: %w", err)
	}
	version, err := modinfoVersion(koData)
	if err != nil {
		return "", fmt.Errorf("zfs.ko: %w", err)
	}
	return version, nil
}
