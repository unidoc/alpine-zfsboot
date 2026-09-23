package parttable

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

// buildGPTDisk hand-assembles a real, byte-correct GPT disk image:
// protective MBR (LBA0, ignored by this package but present for
// realism), a real GPT header (LBA1) with a correctly computed
// header_crc32, and a partition array (starting at partitionArrayLBA)
// holding the given entries. This is a from-scratch byte assembler,
// not a round-trip through this package's OWN encoder - the point is
// proving ReadGPTHeader/GPTPartitions correctly decode bytes an
// independent process laid out, the same reasoning
// bios/tests/build_fat_fixtures.py's own header comment gives for not
// reusing mkfs.vfat/mtools.
func buildGPTDisk(t *testing.T, entries []GPTPartitionEntry) string {
	t.Helper()
	const partitionArrayLBA = 2
	const numEntries = 128 // standard GPT entry count, matches every real writer
	const entrySize = 128

	disk := make([]byte, (partitionArrayLBA+numEntries*entrySize/sectorSize+10)*sectorSize)

	// LBA 0: protective MBR signature only - this package never reads
	// LBA 0 for GPT purposes, but a real disk always has 0x55AA there.
	disk[510], disk[511] = 0x55, 0xAA

	// LBA 1: GPT header.
	hdr := make([]byte, gptHeaderSize)
	copy(hdr[0:8], gptSignature[:])
	binary.LittleEndian.PutUint32(hdr[8:12], 0x00010000) // revision 1.0
	binary.LittleEndian.PutUint32(hdr[12:16], gptHeaderSize)
	// hdr[16:20] header_crc32 - filled in after computing it below.
	binary.LittleEndian.PutUint64(hdr[24:32], 1)                 // my_lba
	binary.LittleEndian.PutUint64(hdr[72:80], partitionArrayLBA) // partition_entry_lba (offset 72: 56-byte prefix + 16-byte disk_guid)
	binary.LittleEndian.PutUint32(hdr[80:84], numEntries)        // num_partition_entries
	binary.LittleEndian.PutUint32(hdr[84:88], entrySize)         // partition_entry_size
	crc := crc32.ChecksumIEEE(hdr)                               // header_crc32 field is still zero here, matching the spec's own convention
	binary.LittleEndian.PutUint32(hdr[16:20], crc)
	copy(disk[sectorSize:sectorSize+gptHeaderSize], hdr)

	// Partition array.
	for i, e := range entries {
		off := int(partitionArrayLBA)*sectorSize + i*entrySize
		copy(disk[off:off+16], e.TypeGUID[:])
		copy(disk[off+16:off+32], e.UniqueGUID[:])
		binary.LittleEndian.PutUint64(disk[off+32:off+40], e.StartLBA)
		binary.LittleEndian.PutUint64(disk[off+40:off+48], e.EndLBA)
		binary.LittleEndian.PutUint64(disk[off+48:off+56], e.Attributes)
		for j, c := range e.Name {
			binary.LittleEndian.PutUint16(disk[off+56+j*2:off+58+j*2], c)
		}
	}

	path := filepath.Join(t.TempDir(), "gpt.img")
	if err := os.WriteFile(path, disk, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func nameField(s string) [36]uint16 {
	var out [36]uint16
	for i, r := range s {
		if i >= len(out) {
			break
		}
		out[i] = uint16(r)
	}
	return out
}

// biosBootGUID is a fixed test fixture value - this package is
// deliberately generic (no alpine-zfsboot-specific knowledge, see its
// own doc comment), so it does not import internal/layout even though
// this happens to equal that package's own BIOSBootPartitionGUIDBytes
// (the real, standard GPT "BIOS boot partition" type GUID).
var biosBootGUID = [16]byte{0x48, 0x61, 0x68, 0x21, 0x49, 0x64, 0x6f, 0x6e, 0x74, 0x4e, 0x65, 0x65, 0x64, 0x45, 0x46, 0x49}

func TestReadGPTHeader_RealHeader(t *testing.T) {
	disk := buildGPTDisk(t, []GPTPartitionEntry{
		{TypeGUID: biosBootGUID, StartLBA: 34, EndLBA: 97, Name: nameField("alpine-zfsboot-stage2")},
	})
	hdr, err := ReadGPTHeader(disk)
	if err != nil {
		t.Fatalf("ReadGPTHeader: %v", err)
	}
	if hdr.NumPartitionEntries != 128 || hdr.PartitionEntrySize != 128 {
		t.Errorf("hdr = %+v, unexpected entry count/size", hdr)
	}
}

func TestReadGPTHeader_NoSignature(t *testing.T) {
	path := filepath.Join(t.TempDir(), "msdos.img")
	if err := os.WriteFile(path, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadGPTHeader(path)
	if err != ErrNoGPTSignature {
		t.Errorf("ReadGPTHeader on an all-zero disk = %v, want ErrNoGPTSignature", err)
	}
}

func TestReadGPTHeader_CorruptedCRC(t *testing.T) {
	disk := buildGPTDisk(t, nil)
	f, err := os.OpenFile(disk, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Flip one byte inside the header (well past the signature) - the
	// CRC32 must catch this.
	if _, err := f.WriteAt([]byte{0xFF}, sectorSize+50); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, err := ReadGPTHeader(disk); err == nil {
		t.Error("ReadGPTHeader with a corrupted header: want an error, got nil")
	}
}

func TestGPTPartitions(t *testing.T) {
	espGUID := [16]byte{0x28, 0x73, 0x2a, 0xc1, 0x1f, 0xf8, 0xd2, 0x11, 0xba, 0x4b, 0x00, 0xa0, 0xc9, 0x3e, 0xc9, 0x3b}
	disk := buildGPTDisk(t, []GPTPartitionEntry{
		{TypeGUID: biosBootGUID, StartLBA: 34, EndLBA: 97, Name: nameField("alpine-zfsboot-stage2")},
		{TypeGUID: espGUID, StartLBA: 2048, EndLBA: 1050623, Name: nameField("EFI")},
	})
	hdr, err := ReadGPTHeader(disk)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := GPTPartitions(disk, hdr)
	if err != nil {
		t.Fatalf("GPTPartitions: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}

	stage2 := entries[0]
	if stage2.StartLBA != 34 || stage2.SectorCount() != 64 {
		t.Errorf("stage2 entry: start=%d count=%d, want start=34 count=64", stage2.StartLBA, stage2.SectorCount())
	}
	if stage2.NameString() != "alpine-zfsboot-stage2" {
		t.Errorf("stage2 entry name = %q, want \"alpine-zfsboot-stage2\"", stage2.NameString())
	}
	if stage2.TypeGUID != biosBootGUID {
		t.Errorf("stage2 entry TypeGUID = %x, want %x", stage2.TypeGUID, biosBootGUID)
	}

	esp := entries[1]
	if esp.NameString() != "EFI" {
		t.Errorf("esp entry name = %q, want \"EFI\"", esp.NameString())
	}
}

// TestGPTPartitions_SmallEntrySizeRejectedNotPanicked is the
// regression test for a real bug found in a full source audit:
// GPTPartitions used to accept any PartitionEntrySize in [48,
// sectorSize] but then decode each entry via a hardcoded
// sector[off:off+128] slice - a CRC-valid header declaring an entry
// size below 128 (this test uses 64, matching the audit's own example)
// made that slice run past the sector buffer on the last entry of a
// sector, panicking with "slice bounds out of range" instead of
// returning a clean error. 128 is also the true UEFI spec minimum (see
// GPTPartitions' own doc comment), so this loses no real compatibility.
func TestGPTPartitions_SmallEntrySizeRejectedNotPanicked(t *testing.T) {
	const partitionArrayLBA = 2
	const numEntries = 16 // > 512/64=8, so the loop reaches a full sector's worth of 64-byte entries
	const entrySize = 64

	disk := make([]byte, (partitionArrayLBA+numEntries*entrySize/sectorSize+10)*sectorSize)
	disk[510], disk[511] = 0x55, 0xAA

	hdr := make([]byte, gptHeaderSize)
	copy(hdr[0:8], gptSignature[:])
	binary.LittleEndian.PutUint32(hdr[8:12], 0x00010000)
	binary.LittleEndian.PutUint32(hdr[12:16], gptHeaderSize)
	binary.LittleEndian.PutUint64(hdr[24:32], 1)
	binary.LittleEndian.PutUint64(hdr[72:80], partitionArrayLBA)
	binary.LittleEndian.PutUint32(hdr[80:84], numEntries)
	binary.LittleEndian.PutUint32(hdr[84:88], entrySize)
	crc := crc32.ChecksumIEEE(hdr)
	binary.LittleEndian.PutUint32(hdr[16:20], crc)
	copy(disk[sectorSize:sectorSize+gptHeaderSize], hdr)

	// A real (non-zero-GUID) entry in the LAST slot of the first sector
	// (off=7*64=448) - the exact slot whose old sector[448:576] slice
	// ran past the 512-byte buffer.
	off := int(partitionArrayLBA)*sectorSize + 7*entrySize
	copy(disk[off:off+16], biosBootGUID[:])
	binary.LittleEndian.PutUint64(disk[off+32:off+40], 34)
	binary.LittleEndian.PutUint64(disk[off+40:off+48], 97)

	path := filepath.Join(t.TempDir(), "gpt-small-entry.img")
	if err := os.WriteFile(path, disk, 0o644); err != nil {
		t.Fatal(err)
	}

	hdrParsed, err := ReadGPTHeader(path)
	if err != nil {
		t.Fatalf("ReadGPTHeader (header itself is CRC-valid): %v", err)
	}
	if _, err := GPTPartitions(path, hdrParsed); err == nil {
		t.Error("GPTPartitions with PartitionEntrySize=64: want a clean error, got nil")
	}
	// The real assertion is simply that the line above returned via a
	// normal Go error rather than a panic - if GPTPartitions panics,
	// this whole test function fails with a runtime error, not a
	// reported t.Error, which is exactly what this test is guarding
	// against (go test reports it distinctly either way, but the
	// panic message names the source-audit-found "slice bounds out of
	// range" defect specifically if this regresses).
}

func TestGPTPartitions_EmptyArrayIgnoresZeroSlots(t *testing.T) {
	disk := buildGPTDisk(t, []GPTPartitionEntry{
		{TypeGUID: biosBootGUID, StartLBA: 34, EndLBA: 97},
	})
	hdr, err := ReadGPTHeader(disk)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := GPTPartitions(disk, hdr)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries (the other 127 zero-GUID slots should be skipped), want 1", len(entries))
	}
}

// --- MBR -------------------------------------------------------------

func buildMBRDisk(t *testing.T, entries []MBRPartitionEntry) string {
	t.Helper()
	disk := make([]byte, 4096)
	disk[mbrBootSignatureOffset] = 0x55
	disk[mbrBootSignatureOffset+1] = 0xAA

	for i, e := range entries {
		off := mbrPartitionTableOffset + i*mbrPartitionEntrySize
		disk[off] = e.Status
		disk[off+4] = e.Type
		binary.LittleEndian.PutUint32(disk[off+8:off+12], e.StartLBA)
		binary.LittleEndian.PutUint32(disk[off+12:off+16], e.SectorCount)
	}

	path := filepath.Join(t.TempDir(), "mbr.img")
	if err := os.WriteFile(path, disk, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMBRPartitions(t *testing.T) {
	disk := buildMBRDisk(t, []MBRPartitionEntry{
		{Type: 0x2D, StartLBA: 34, SectorCount: 64},                        // alpine-zfsboot's own stage2 reservation (cosmetic type byte)
		{Type: 0x2E, StartLBA: 2048, SectorCount: 1048576},                 // the FAT/ESP partition
		{Status: 0x80, Type: 0x83, StartLBA: 1050624, SectorCount: 100000}, // a normal bootable Linux partition
	})
	entries, err := MBRPartitions(disk)
	if err != nil {
		t.Fatalf("MBRPartitions: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(entries), entries)
	}
	if entries[0].Type != 0x2D || entries[0].StartLBA != 34 || entries[0].SectorCount != 64 {
		t.Errorf("entry 0 = %+v, want the stage2 reservation", entries[0])
	}
}

func TestMBRPartitions_SkipsZeroSlots(t *testing.T) {
	disk := buildMBRDisk(t, []MBRPartitionEntry{
		{Type: 0x2D, StartLBA: 34, SectorCount: 64},
	})
	entries, err := MBRPartitions(disk)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries (the other 3 zero-count slots should be skipped), want 1", len(entries))
	}
}

func TestMBRPartitions_NoBootSignature(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nosig.img")
	if err := os.WriteFile(path, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MBRPartitions(path); err == nil {
		t.Error("MBRPartitions on a disk with no 0x55AA signature: want an error, got nil")
	}
}

// TestMBRPartitions_FATBootSectorRefused is the regression test for a
// real bug found in a full source audit: a FAT boot sector ALSO ends in
// the 0x55AA signature (required by the FAT spec, same as MBR), so a
// whole-disk-formatted FAT device (mkfs.vfat directly on a block
// device, no partition table at all - a real, common shape for USB
// media) used to be silently parsed as "a valid MBR with zero partition
// entries" instead of refused. Builds a genuine FAT32 boot sector shape
// (jump instruction + BPB_BytsPerSec=512 + BPB_SecPerClus, the same
// fields/offsets bios/fat.c's own fat_mount() validates) - not just a
// single stray jump-like byte - to prove the check actually
// distinguishes real FAT geometry from ordinary MBR bootstrap bytes,
// not merely "byte 0 looks like a jump opcode".
func TestMBRPartitions_FATBootSectorRefused(t *testing.T) {
	disk := make([]byte, 4096)
	disk[0] = 0xEB // BS_jmpBoot: short jump
	disk[1] = 0x3C
	disk[2] = 0x90 // NOP
	binary.LittleEndian.PutUint16(disk[11:13], 512) // BPB_BytsPerSec
	disk[13] = 8                                    // BPB_SecPerClus (power of two)
	disk[mbrBootSignatureOffset] = 0x55
	disk[mbrBootSignatureOffset+1] = 0xAA

	path := filepath.Join(t.TempDir(), "fat-boot-sector.img")
	if err := os.WriteFile(path, disk, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MBRPartitions(path); err == nil {
		t.Error("MBRPartitions on a real FAT boot sector shape: want an error (refuse to guess), got nil")
	}
}

// TestMBRPartitions_OrdinaryBootCodeNotMistakenForFAT confirms the fix
// doesn't over-trigger: real MBR bootstrap code that happens to start
// with a byte matching a jump opcode, but WITHOUT the rest of a
// genuine FAT BPB shape at the right offsets, must still be read
// normally - looksLikeFATBootSector requires all three conditions at
// once, not just one.
func TestMBRPartitions_OrdinaryBootCodeNotMistakenForFAT(t *testing.T) {
	disk := make([]byte, 4096)
	disk[0] = 0xEB // coincidentally matches a FAT jump opcode byte
	disk[11] = 0x42
	disk[12] = 0x13 // NOT 512 when read as bytes_per_sector
	disk[13] = 0x07 // NOT a power of two
	disk[mbrBootSignatureOffset] = 0x55
	disk[mbrBootSignatureOffset+1] = 0xAA
	binary.LittleEndian.PutUint32(disk[mbrPartitionTableOffset+8:mbrPartitionTableOffset+12], 34)
	binary.LittleEndian.PutUint32(disk[mbrPartitionTableOffset+12:mbrPartitionTableOffset+16], 64)

	path := filepath.Join(t.TempDir(), "ordinary-mbr.img")
	if err := os.WriteFile(path, disk, 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := MBRPartitions(path)
	if err != nil {
		t.Fatalf("MBRPartitions on ordinary (non-FAT-shaped) boot code: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
}
