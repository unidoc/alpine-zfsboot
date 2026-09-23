// Package parttable is a minimal, read-only GPT/MBR partition-table
// reader - the Go mirror of bios/gpt.c and bios/mbr.c's own on-disk
// struct layouts and validation rules (GPT header CRC32, the 0x55AA
// MBR boot signature, the same field offsets), used by
// internal/bootenv to answer one safety-critical question before any
// write ever touches a disk: does this disk's own partition table
// actually reserve the region alpine-zfsboot's installer created, or
// would a write there be blind - "the disk is BIOS-booted" is NOT
// sufficient justification for writing at a fixed LBA; the partition
// table itself must say so.
package parttable

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
)

const sectorSize = 512

// --- GPT ---------------------------------------------------------------

var gptSignature = [8]byte{'E', 'F', 'I', ' ', 'P', 'A', 'R', 'T'}

// GPTHeader mirrors bios/gpt.h's own struct gpt_header exactly (field
// order/sizes matter - this is the real on-disk layout, packed, no Go
// struct padding possible since every field here is already
// naturally aligned at its own offset).
type GPTHeader struct {
	Signature                [8]byte
	Revision                 uint32
	HeaderSize               uint32
	HeaderCRC32              uint32
	Reserved                 uint32
	MyLBA                    uint64
	AlternateLBA             uint64
	FirstUsableLBA           uint64
	LastUsableLBA            uint64
	DiskGUID                 [16]byte
	PartitionEntryLBA        uint64
	NumPartitionEntries      uint32
	PartitionEntrySize       uint32
	PartitionEntryArrayCRC32 uint32
}

const gptHeaderSize = 92

// GPTPartitionEntry mirrors bios/gpt.h's own struct
// gpt_partition_entry.
type GPTPartitionEntry struct {
	TypeGUID   [16]byte
	UniqueGUID [16]byte
	StartLBA   uint64
	EndLBA     uint64 // inclusive, same as the on-disk field
	Attributes uint64
	Name       [36]uint16 // UTF-16LE
}

// SectorCount is EndLBA-StartLBA+1 - the plain count form every other
// package in this project (internal/layout, internal/biosboot) uses,
// not the inclusive end LBA the on-disk format itself stores.
func (e GPTPartitionEntry) SectorCount() uint64 {
	return e.EndLBA - e.StartLBA + 1
}

// NameString decodes Name (UTF-16LE, NUL-padded) to a plain Go
// string, trimmed at the first NUL.
func (e GPTPartitionEntry) NameString() string {
	n := len(e.Name)
	for i, c := range e.Name {
		if c == 0 {
			n = i
			break
		}
	}
	return string(utf16Decode(e.Name[:n]))
}

func utf16Decode(s []uint16) []rune {
	// Minimal UTF-16LE decoder, no surrogate-pair handling needed -
	// every real-world GPT partition name alpine-installer itself
	// writes (`sgdisk -c`) is plain ASCII, well within the BMP.
	out := make([]rune, 0, len(s))
	for _, c := range s {
		out = append(out, rune(c))
	}
	return out
}

// crc32ISO is the exact algorithm bios/gpt.c's own crc32() implements
// by hand, bit-by-bit, for its freestanding build (polynomial
// 0xEDB88320, ISO 3309/zlib convention, per the UEFI spec's own
// header_crc32 field definition) - Go's stdlib crc32.IEEE is that
// same standard polynomial in its usual reflected form, used here
// instead of re-deriving it, cross-checked by this package's own
// TestReadGPTHeader_RealHeader against a real, known-good header.
func crc32ISO(data []byte) uint32 {
	return crc32.ChecksumIEEE(data)
}

// ReadGPTHeader reads and validates the primary GPT header (LBA 1) of
// disk - signature, header_size == 92, and header_crc32 (computed
// with that field itself zeroed, per the UEFI spec's own convention)
// - the exact same checks bios/gpt.c's own gpt_read_header() performs
// at boot. Returns ErrNoGPTSignature specifically when the sector
// read cleanly but had no GPT signature at all (a real, permanent
// msdos-labeled disk, not a transient failure - same distinction
// bios/gpt.h's own GPT_ERR_NO_SIGNATURE documents).
func ReadGPTHeader(disk string) (*GPTHeader, error) {
	f, err := os.Open(disk)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", disk, err)
	}
	defer f.Close()

	sector := make([]byte, sectorSize)
	if _, err := f.ReadAt(sector, sectorSize); err != nil {
		return nil, fmt.Errorf("reading LBA 1 of %s: %w", disk, err)
	}

	if !bytes.Equal(sector[:8], gptSignature[:]) {
		return nil, ErrNoGPTSignature
	}

	var hdr GPTHeader
	if err := binary.Read(bytes.NewReader(sector[:gptHeaderSize]), binary.LittleEndian, &hdr); err != nil {
		return nil, fmt.Errorf("decoding GPT header: %w", err)
	}
	if hdr.HeaderSize != gptHeaderSize {
		return nil, fmt.Errorf("GPT header_size is %d, want %d", hdr.HeaderSize, gptHeaderSize)
	}

	storedCRC := hdr.HeaderCRC32
	work := make([]byte, gptHeaderSize)
	copy(work, sector[:gptHeaderSize])
	// header_crc32 lives at byte offset 16 (signature 8 + revision 4 +
	// header_size 4) - zeroed for the CRC computation, per spec.
	work[16], work[17], work[18], work[19] = 0, 0, 0, 0
	if got := crc32ISO(work); got != storedCRC {
		return nil, fmt.Errorf("GPT header CRC32 mismatch: disk says 0x%08x, computed 0x%08x", storedCRC, got)
	}

	return &hdr, nil
}

// ErrNoGPTSignature mirrors bios/gpt.h's own GPT_ERR_NO_SIGNATURE.
var ErrNoGPTSignature = fmt.Errorf("no GPT signature present (msdos-labeled disk)")

// GPTPartitions reads every non-empty partition-array entry - strides
// by the header's own PartitionEntrySize, not a hardcoded 128 (matching
// bios/gpt.c's own gpt_find_partition() walking logic), but returns
// every entry instead of stopping at the first type-GUID match, since
// internal/bootenv's own caller needs to check MULTIPLE fields (type,
// position, size, name) together, not just type.
//
// The [128, sectorSize] bound (128, not bios/gpt.c's own smaller 48) is
// a real source-audit fix, not a style choice: this function decodes
// each entry via a single binary.Read into the FULL 128-byte
// GPTPartitionEntry struct (TypeGUID+UniqueGUID+StartLBA+EndLBA+
// Attributes+Name, all of it, unlike bios/gpt.c's own narrower manual
// field-by-field reads, which only ever dereference up to
// ending_lba at byte 40 and so can tolerate a smaller entry size) - a
// header reporting an entrySize between 48 and 127 used to pass this
// check and then panic with a slice-bounds-out-of-range on the LAST
// entry of a sector (sector[off:off+128] running past the 512-byte
// buffer). 128 is also the TRUE UEFI spec minimum regardless of this
// implementation detail - the spec requires SizeOfPartitionEntry to be
// 128*2^n for an integer n>=0 - so this loses no real compatibility:
// no spec-conformant GPT-writing tool (sgdisk, parted, Windows, ...)
// has ever produced a value below it.
func GPTPartitions(disk string, hdr *GPTHeader) ([]GPTPartitionEntry, error) {
	if hdr.PartitionEntrySize < 128 || hdr.PartitionEntrySize > sectorSize {
		return nil, fmt.Errorf("PartitionEntrySize %d out of the supported [128, %d] range", hdr.PartitionEntrySize, sectorSize)
	}
	if hdr.NumPartitionEntries == 0 {
		return nil, fmt.Errorf("GPT header reports zero partition entries")
	}

	f, err := os.Open(disk)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", disk, err)
	}
	defer f.Close()

	entriesPerSector := sectorSize / hdr.PartitionEntrySize
	totalSectors := (hdr.NumPartitionEntries + entriesPerSector - 1) / entriesPerSector

	var out []GPTPartitionEntry
	entryIndex := uint32(0)
	sector := make([]byte, sectorSize)
	for s := uint32(0); s < totalSectors; s++ {
		lba := hdr.PartitionEntryLBA + uint64(s)
		if _, err := f.ReadAt(sector, int64(lba)*sectorSize); err != nil {
			return nil, fmt.Errorf("reading GPT partition array at LBA %d: %w", lba, err)
		}
		for i := uint32(0); i < entriesPerSector && entryIndex < hdr.NumPartitionEntries; i++ {
			entryIndex++
			off := i * hdr.PartitionEntrySize
			var e GPTPartitionEntry
			// The fixed 128-byte struct fields, even if
			// PartitionEntrySize itself is larger (reserved/
			// vendor-defined trailing bytes this package doesn't
			// model, same as bios/gpt.c's own struct).
			if err := binary.Read(bytes.NewReader(sector[off:off+128]), binary.LittleEndian, &e); err != nil {
				return nil, fmt.Errorf("decoding GPT partition entry %d: %w", entryIndex-1, err)
			}
			if isZeroGUID(e.TypeGUID) {
				continue // unused slot
			}
			out = append(out, e)
		}
	}
	return out, nil
}

func isZeroGUID(g [16]byte) bool {
	for _, b := range g {
		if b != 0 {
			return false
		}
	}
	return true
}

// --- MBR -----------------------------------------------------------

const (
	mbrBootSignatureOffset  = 510
	mbrPartitionTableOffset = 446
	mbrPartitionEntrySize   = 16
	mbrMaxPartitions        = 4
)

// MBRPartitionEntry mirrors bios/mbr.c's own struct
// mbr_partition_entry (the CHS fields are skipped entirely here, same
// as that file - LBA is all this project ever uses).
type MBRPartitionEntry struct {
	Status      byte
	Type        byte
	StartLBA    uint32
	SectorCount uint32
}

// looksLikeFATBootSector checks whether sector (LBA 0, already confirmed
// to end in the 0x55AA boot signature by MBRPartitions' own caller) is
// actually a FAT boot sector rather than a real MBR - a full source
// audit found this project had no such check at all: a FAT filesystem's
// own boot sector ALSO ends in 0x55AA (the FAT spec requires it, same
// as MBR), so a whole-disk-formatted FAT device (no partition table at
// all - `mkfs.vfat` directly on a block device, a real, common shape
// for USB media) was silently parsed as "a valid MBR with zero
// partition entries" - the exact same shape CheckStage2ExtentFree
// already deliberately tolerates for a real, unpartitioned
// alpine-zfsboot disk (see that function's own doc comment) - so this
// safety gate could pass on a device whose LBA 0 is actually someone
// else's live filesystem, not an unclaimed disk at all.
//
// Distinguishes the two the same way blkid/the Linux kernel's own FAT
// driver do: a FAT boot sector's first three bytes are always a jump
// instruction (0xEB/0xE9), and bytes 11-12/13 (BPB_BytsPerSec/
// BPB_SecPerClus, the exact same fields and validation bios/fat.c's own
// fat_mount() already checks, at the same spec-defined offsets) hold a
// real, structured filesystem geometry - values that would only appear
// in a genuine x86 MBR bootstrap program's own machine code by
// astronomical coincidence, not by chance. Requiring ALL THREE
// conditions at once (not just one) is what keeps this from false-
// positiving on ordinary MBR code that happens to contain a stray 0xEB
// byte somewhere.
func looksLikeFATBootSector(sector []byte) bool {
	jmpOK := sector[0] == 0xEB || sector[0] == 0xE9
	bytesPerSector := binary.LittleEndian.Uint16(sector[11:13])
	sectorsPerCluster := sector[13]
	bytesPerSectorOK := bytesPerSector == sectorSize
	sectorsPerClusterOK := sectorsPerCluster != 0 && (sectorsPerCluster&(sectorsPerCluster-1)) == 0
	return jmpOK && bytesPerSectorOK && sectorsPerClusterOK
}

// MBRPartitions reads LBA 0, checks the 0x55AA boot signature, and
// returns every partition entry with a nonzero SectorCount (an
// all-zero entry is how an unused primary slot is conventionally
// represented, same skip bios/mbr.c's own mbr_find_partition()
// applies via its own `sector_count != 0` check).
func MBRPartitions(disk string) ([]MBRPartitionEntry, error) {
	f, err := os.Open(disk)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", disk, err)
	}
	defer f.Close()

	sector := make([]byte, sectorSize)
	if _, err := f.ReadAt(sector, 0); err != nil {
		return nil, fmt.Errorf("reading LBA 0 of %s: %w", disk, err)
	}
	if sector[mbrBootSignatureOffset] != 0x55 || sector[mbrBootSignatureOffset+1] != 0xAA {
		return nil, fmt.Errorf("no 0x55AA boot signature at offset %d", mbrBootSignatureOffset)
	}
	if looksLikeFATBootSector(sector) {
		return nil, fmt.Errorf("LBA 0 looks like a FAT boot sector, not an MBR partition table - refusing to guess")
	}

	var out []MBRPartitionEntry
	for i := 0; i < mbrMaxPartitions; i++ {
		off := mbrPartitionTableOffset + i*mbrPartitionEntrySize
		raw := sector[off : off+mbrPartitionEntrySize]
		e := MBRPartitionEntry{
			Status:      raw[0],
			Type:        raw[4],
			StartLBA:    binary.LittleEndian.Uint32(raw[8:12]),
			SectorCount: binary.LittleEndian.Uint32(raw[12:16]),
		}
		if e.SectorCount == 0 {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}
