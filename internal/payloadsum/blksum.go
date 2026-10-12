package payloadsum

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/bits"
)

// BLKSUM - the per-block table the BIOS stage2 checks every 64 KiB block of
// KERNEL/INITRD against as it lands in RAM, re-reading a block the BIOS
// delivered wrong (bios/blkverify.h is the reader and documents the
// format; keep the two in step - testdata/golden.blksum.hex is checked by the
// Go and the C tests). It sits next to CHECKSUM, which governs it: stage2
// uses BLKSUM only with a CHECKSUM that describes the same files, and
// CHECKSUM's MODE decides what a block that cannot be healed means.
//
// Entry i of a file is the SHA-256 chaining state after the first
// (i+1)*BlockSize bytes of its RAM range (H0..H7, big-endian); the last
// entry is the final SHA-256 digest - the one CHECKSUM records. So the
// loader's checks ride on the one SHA-256 pass it makes anyway, and each
// block is judged with SHA-256's full strength. Like CHECKSUM, this
// protects against corruption and forgotten updates, not tampering: it
// lives on the same unauthenticated FAT partition as the kernel.

// Layout constants, mirrored from bios/blkverify.h (blksum_test.go greps them).
const (
	BlockSize       = 65536
	BlkSumVersion   = 1
	EntriesPerSect  = 15
	blkSectorSize   = 512
	blkHashedPrefix = 480
)

// BlkFile is one file's part of a BLKSUM.
type BlkFile struct {
	Size, Offset, Blocks, First uint32
	SHA256                      [32]byte
	Entries                     [][32]byte
}

// BlkSum is a decoded BLKSUM.
type BlkSum struct {
	Kernel, Initrd BlkFile
}

var sha256K = [64]uint32{
	0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
	0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
	0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
	0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
	0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
	0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
	0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
	0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
}

// sha256Blocks runs the SHA-256 compression function over whole 64-byte
// blocks (FIPS 180-4, 6.2.2) - crypto/sha256 does not expose its chaining
// state, which is what the table records. blksum_test.go checks it against
// crypto/sha256.
func sha256Blocks(h *[8]uint32, p []byte) {
	var w [64]uint32
	for len(p) >= 64 {
		for i := 0; i < 16; i++ {
			w[i] = binary.BigEndian.Uint32(p[4*i:])
		}
		for i := 16; i < 64; i++ {
			s0 := bits.RotateLeft32(w[i-15], -7) ^ bits.RotateLeft32(w[i-15], -18) ^ (w[i-15] >> 3)
			s1 := bits.RotateLeft32(w[i-2], -17) ^ bits.RotateLeft32(w[i-2], -19) ^ (w[i-2] >> 10)
			w[i] = w[i-16] + s0 + w[i-7] + s1
		}
		a, b, c, d, e, f, g, hh := h[0], h[1], h[2], h[3], h[4], h[5], h[6], h[7]
		for i := 0; i < 64; i++ {
			t1 := hh + (bits.RotateLeft32(e, -6) ^ bits.RotateLeft32(e, -11) ^ bits.RotateLeft32(e, -25)) + ((e & f) ^ (^e & g)) + sha256K[i] + w[i]
			t2 := (bits.RotateLeft32(a, -2) ^ bits.RotateLeft32(a, -13) ^ bits.RotateLeft32(a, -22)) + ((a & b) ^ (a & c) ^ (b & c))
			hh, g, f, e, d, c, b, a = g, f, e, d+t1, c, b, a, t1+t2
		}
		h[0] += a
		h[1] += b
		h[2] += c
		h[3] += d
		h[4] += e
		h[5] += f
		h[6] += g
		h[7] += hh
		p = p[64:]
	}
}

// chainEntries computes a RAM range's table entries.
func chainEntries(p []byte) [][32]byte {
	n := (len(p) + BlockSize - 1) / BlockSize
	out := make([][32]byte, n)
	h := [8]uint32{0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19}
	for i := 0; i < n; i++ {
		if i == n-1 {
			out[i] = sha256.Sum256(p)
			break
		}
		sha256Blocks(&h, p[i*BlockSize:(i+1)*BlockSize])
		for k := 0; k < 8; k++ {
			binary.BigEndian.PutUint32(out[i][4*k:], h[k])
		}
	}
	return out
}

func sectorsFor(entries int) int {
	return (entries + EntriesPerSect - 1) / EntriesPerSect
}

// GenerateBlkSum builds the BLKSUM for a kernel (a bzImage; its RAM range
// starts after the real-mode part) and an initrd.
func GenerateBlkSum(kernel, initrd []byte) ([]byte, error) {
	koff, err := KernelRAMOffset(kernel)
	if err != nil {
		return nil, err
	}
	if uint64(len(kernel)) > 0xffffffff || uint64(len(initrd)) > 0xffffffff {
		return nil, fmt.Errorf("payloadsum: a file does not fit the 32-bit size field")
	}
	ke, ie := chainEntries(kernel[koff:]), chainEntries(initrd)
	ks, is := sectorsFor(len(ke)), sectorsFor(len(ie))
	out := make([]byte, (1+ks+is)*blkSectorSize)
	h := out[:blkSectorSize]
	copy(h, "AZBLKSUM")
	le := binary.LittleEndian
	le.PutUint32(h[8:], BlkSumVersion)
	le.PutUint32(h[12:], BlockSize)
	le.PutUint32(h[16:], EntriesPerSect)
	for i, f := range []struct {
		size, off uint32
		n, first  int
	}{{uint32(len(kernel)), koff, len(ke), 1}, {uint32(len(initrd)), 0, len(ie), 1 + ks}} {
		p := h[20+16*i:]
		le.PutUint32(p, f.size)
		le.PutUint32(p[4:], f.off)
		le.PutUint32(p[8:], uint32(f.n))
		le.PutUint32(p[12:], uint32(f.first))
	}
	ksum, isum := sha256.Sum256(kernel[koff:]), sha256.Sum256(initrd)
	copy(h[52:], ksum[:])
	copy(h[84:], isum[:])
	for i, e := range ke {
		copy(out[(1+i/EntriesPerSect)*blkSectorSize+(i%EntriesPerSect)*32:], e[:])
	}
	for i, e := range ie {
		copy(out[(1+ks+i/EntriesPerSect)*blkSectorSize+(i%EntriesPerSect)*32:], e[:])
	}
	for s := 0; s < 1+ks+is; s++ {
		sec := out[s*blkSectorSize : (s+1)*blkSectorSize]
		d := sha256.Sum256(sec[:blkHashedPrefix])
		copy(sec[blkHashedPrefix:], d[:])
	}
	return out, nil
}

// DecodeBlkSum parses and checks a BLKSUM: header and every table sector
// against their own hashes, and the counts against the sizes.
func DecodeBlkSum(raw []byte) (BlkSum, error) {
	var b BlkSum
	if len(raw) < blkSectorSize || len(raw)%blkSectorSize != 0 {
		return b, fmt.Errorf("blksum: %d bytes, not a whole number of 512-byte sectors", len(raw))
	}
	for s := 0; s < len(raw)/blkSectorSize; s++ {
		sec := raw[s*blkSectorSize : (s+1)*blkSectorSize]
		if d := sha256.Sum256(sec[:blkHashedPrefix]); !bytes.Equal(d[:], sec[blkHashedPrefix:]) {
			return b, fmt.Errorf("blksum: sector %d fails its own SHA-256", s)
		}
	}
	h := raw[:blkSectorSize]
	if string(h[:8]) != "AZBLKSUM" {
		return b, fmt.Errorf("blksum: no AZBLKSUM header")
	}
	le := binary.LittleEndian
	if v := le.Uint32(h[8:]); v != BlkSumVersion {
		return b, fmt.Errorf("blksum: version %d, this tool knows %d", v, BlkSumVersion)
	}
	if le.Uint32(h[12:]) != BlockSize || le.Uint32(h[16:]) != EntriesPerSect {
		return b, fmt.Errorf("blksum: unexpected block size or entries per sector")
	}
	for i, f := range []*BlkFile{&b.Kernel, &b.Initrd} {
		p := h[20+16*i:]
		f.Size, f.Offset, f.Blocks, f.First = le.Uint32(p), le.Uint32(p[4:]), le.Uint32(p[8:]), le.Uint32(p[12:])
		copy(f.SHA256[:], h[52+32*i:])
		if f.Offset > f.Size || f.First == 0 {
			return b, fmt.Errorf("blksum: bad file descriptor %d", i)
		}
		l := f.Size - f.Offset
		if f.Blocks != (l+BlockSize-1)/BlockSize {
			return b, fmt.Errorf("blksum: %d blocks do not fit %d bytes", f.Blocks, l)
		}
		end := int(f.First) + sectorsFor(int(f.Blocks))
		if end*blkSectorSize > len(raw) {
			return b, fmt.Errorf("blksum: table of file %d runs past the end", i)
		}
		for k := 0; k < int(f.Blocks); k++ {
			var e [32]byte
			copy(e[:], raw[(int(f.First)+k/EntriesPerSect)*blkSectorSize+(k%EntriesPerSect)*32:])
			f.Entries = append(f.Entries, e)
		}
	}
	return b, nil
}

// VerifyBlkSum checks a decoded BLKSUM against the files and against the
// CHECKSUM manifest it must agree with; one error per problem, naming the
// first block that differs.
func VerifyBlkSum(b BlkSum, m Manifest, kernel, initrd []byte) []error {
	var errs []error
	koff, err := KernelRAMOffset(kernel)
	if err != nil {
		return []error{err}
	}
	for _, c := range []struct {
		name string
		f    BlkFile
		sum  Entry
		data []byte
		off  uint32
	}{{"KERNEL", b.Kernel, m.Kernel, kernel, koff}, {"INITRD", b.Initrd, m.Initrd, initrd, 0}} {
		if c.f.Size != c.sum.Size || c.f.Offset != c.sum.Offset || c.f.SHA256 != c.sum.SHA256 {
			errs = append(errs, fmt.Errorf("blksum: %s does not describe the same file as CHECKSUM (stale BLKSUM: the BIOS loader ignores it)", c.name))
			continue
		}
		if uint64(len(c.data)) != uint64(c.f.Size) || c.off != c.f.Offset {
			errs = append(errs, fmt.Errorf("blksum: %s is %d bytes from offset %d, BLKSUM records %d from %d", c.name, len(c.data), c.off, c.f.Size, c.f.Offset))
			continue
		}
		want := chainEntries(c.data[c.off:])
		for k := range want {
			if want[k] != c.f.Entries[k] {
				errs = append(errs, fmt.Errorf("blksum: %s block %d (file offset %d) does not match its table entry", c.name, k, int(c.off)+k*BlockSize))
				break
			}
		}
	}
	return errs
}
