// Original work: SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, go-fsctl
//
// Modifications: Copyright (c) 2026, UniDoc
// See abi.go for this package's full provenance note.
//
// This file implements the DECODE half of the NV_ENCODE_NATIVE nvlist codec
// used by the /dev/zfs ioctl interface. Upstream's codec also has an ENCODE
// half (EncodeNative and friends); this package never issues a write ioctl,
// so nothing in production ever calls it — it has been moved out of the
// production build entirely, into nvlist_encode_test.go, where it exists
// solely to build realistic wire-format fixtures for this file's own tests
// (see that file's own doc comment).
//
// This is NOT the XDR encoding used by on-disk vdev labels: the ioctl path
// packs nvlists in host-endian, in-memory layout. The codec here mirrors
// OpenZFS module/nvpair/nvpair.c (nvs_native_*) wire format. The DECODER
// below is not a byte-for-byte behavioral mirror of nvs_native_native_op -
// it additionally bounds array element counts (checkElemCount) and nesting
// depth (maxNVListDepth) beyond what upstream's own C decoder checks (which
// bounds recursion via nvpair_max_recursion but had no equivalent array-count
// bound at the commit this package derives from - see checkElemCount's own
// doc comment), both real hardening fixes found in this project's own
// review, not upstream behavior being preserved.
//
// Native packing rules (verified by upstream against OpenZFS 2.2.2):
//
//	Outer stream header (4 bytes):
//	    nvh_encoding  uint8   0 = NV_ENCODE_NATIVE, 1 = NV_ENCODE_XDR
//	    nvh_endian    uint8   0 = big, 1 = little (host endianness)
//	    nvh_reserved  [2]uint8
//
//	nvlist_t packed header (8 bytes, from nvs_native_nvlist):
//	    nvl_version   int32
//	    nvl_nvflag    uint32
//	    (nvl_priv/nvl_flag/nvl_pad are NOT emitted at the top level)
//
//	Each nvpair is the in-memory nvpair_t copied verbatim (nvp_size bytes),
//	8-byte aligned:
//	    nvp_size       int32   total bytes of this pair (incl. header+name+value)
//	    nvp_name_sz    int16   strlen(name)+1 (includes NUL)
//	    nvp_reserve    int16   0
//	    nvp_value_elem int32   element count for array types (else 1, 0 for BOOLEAN)
//	    nvp_type       int32   DATA_TYPE_*
//	    name           nvp_name_sz bytes, NUL-terminated
//	    (pad to 8-byte boundary)
//	    value          type-dependent, NV_ALIGN(8)-padded
//
//	  NVP_VALOFF = NV_ALIGN8(16 + nvp_name_sz)
//	  nvp_size   = NV_ALIGN8(16 + nvp_name_sz) + NV_ALIGN8(value_sz)
//
//	The list (and each embedded list) is terminated by a 4-byte zero
//	(a zero nvp_size).
package zfsnative

import (
	"encoding/binary"
	"fmt"
)

// NV_ENCODE_* and endianness markers.
const (
	// OpenZFS sys/nvpair.h: NV_ENCODE_NATIVE = 0, NV_ENCODE_XDR = 1.
	nvEncodeNative = 0

	// nvh_endian: host_endian is 1 on little-endian, 0 on big-endian.
	nvBigEndian    = 0
	nvLittleEndian = 1
)

// data_type_t values (OpenZFS sys/nvpair.h). Enum begins at
// DATA_TYPE_DONTCARE = -1, DATA_TYPE_UNKNOWN = 0. Kept complete (not trimmed
// to only the types this decoder implements a case for) purely as a
// provenance/documentation record of the real upstream enum; any value
// without a decodeScalar/decodePair case below is rejected as an error,
// fail-closed, same as an unrecognized value would be from any other cause.
const (
	dataTypeUnknown      = 0
	dataTypeBoolean      = 1
	dataTypeByte         = 2
	dataTypeInt16        = 3
	dataTypeUint16       = 4
	dataTypeInt32        = 5
	dataTypeUint32       = 6
	dataTypeInt64        = 7
	dataTypeUint64       = 8
	dataTypeString       = 9
	dataTypeByteArray    = 10
	dataTypeInt16Array   = 11
	dataTypeUint16Array  = 12
	dataTypeInt32Array   = 13
	dataTypeUint32Array  = 14
	dataTypeInt64Array   = 15
	dataTypeUint64Array  = 16
	dataTypeStringArray  = 17
	dataTypeHRTime       = 18
	dataTypeNVList       = 19
	dataTypeNVListArray  = 20
	dataTypeBooleanValue = 21
	dataTypeInt8         = 22
	dataTypeUint8        = 23
	dataTypeBooleanArray = 24
	dataTypeInt8Array    = 25
	dataTypeUint8Array   = 26
)

// nvpairHdrLen is sizeof(nvpair_t): nvp_size(4) + nvp_name_sz(2) +
// nvp_reserve(2) + nvp_value_elem(4) + nvp_type(4).
const nvpairHdrLen = 16

// nvlistHdrLen is the packed nvlist_t header emitted at the start of every
// (sub)list: nvl_version(4) + nvl_nvflag(4).
const nvlistHdrLen = 8

// nvAlign8 rounds up to an 8-byte boundary (NV_ALIGN).
func nvAlign8(n int) int { return (n + 7) &^ 7 }

// Value is one of the supported native value kinds. A nil-valued boolean
// (the bare name) is represented by Boolean.
type Value any

// Boolean is DATA_TYPE_BOOLEAN: a name with no value (e.g. a feature flag).
type Boolean struct{}

// Uint8Array is DATA_TYPE_UINT8_ARRAY: a packed array of uint8 elements. On
// the wire it is byte-for-byte identical to a DATA_TYPE_BYTE_ARRAY but
// carries a distinct data_type_t. Kept for decode-shape fidelity with
// upstream even though nothing in this package's own reachable ioctls
// (PoolConfigs/PoolNames/PoolStats) returns one today — a real pool config
// nvlist decode must not fail closed on a type this codec's own encoder
// (test-only) can legitimately produce and OpenZFS itself defines.
type Uint8Array []byte

// Byte is DATA_TYPE_BYTE.
type Byte uint8

// Nvlist is an ordered map of name to Value.
type Nvlist map[string]Value

// hostOrderFor maps the low byte of a 1-valued uint16 to the matching
// encoding byte order: 1 means the host is little-endian, anything else
// big-endian. Split out as a pure function so both arms are testable on any
// host (nvHostOrder itself can only observe the host it runs on).
func hostOrderFor(lowByte byte) (binary.ByteOrder, byte) {
	if lowByte == 1 {
		return binary.LittleEndian, nvLittleEndian
	}
	return binary.BigEndian, nvBigEndian
}

// nvHostOrder is the encoding endianness for this host — used by
// cmd_linux.go's hostBO to pack/unpack the zfs_cmd_t scalar fields the
// kernel reads in host-endian order (a separate concern from nvlist wire
// encoding, which self-describes its own endianness in its 4-byte header).
var nvHostOrder = func() (binary.ByteOrder, byte) {
	var x uint16 = 1
	return hostOrderFor(*(*byte)(ptrOfUint16(&x)))
}

// maxDecodeElems bounds nvp_value_elem for any array type this decoder
// accepts, independent of the buffer-size check in checkElemCount below.
// 1<<20 (1,048,576) is far beyond any element count a real pool config or
// feature-stats nvlist plausibly contains (the largest real arrays observed
// — vdev_stats/scan_stats — are a few dozen uint64s), while still being
// smaller than would itself cause allocation pressure.
const maxDecodeElems = 1 << 20

// checkElemCount validates a decoded nvp_value_elem count before it is ever
// used as a make() length. This is the fix for a real vulnerability found in
// an independent source-code security review of the pre-fork upstream
// decoder: nvp_value_elem is read directly off the wire (it crosses a
// kernel/userspace ioctl boundary, so it is not under this decoder's own
// control — a kernel bug or a corrupted read could hand back any int32) and
// was previously trusted directly as an allocation length with no
// validation. A corrupted or adversarial count could demand an allocation of
// up to ~17 GiB (a uint64 array) with only 2^32 bytes of backing buffer, or
// (with the sign bit set) crash outright — Go's make() panics
// unconditionally on a negative length, and that panic happens BEFORE any
// recover() in a caller can help, because the failure mode that matters most
// (a large-but-non-panicking allocation attempt triggering a Linux OOM-kill)
// is not a Go panic at all and is categorically unrecoverable by any
// recover() call.
//
// Two independent checks are applied, deliberately not collapsed into one:
// a sanity ceiling (maxDecodeElems, catching absurd counts cheaply before
// even computing a byte size) and a check against the bytes actually
// available in this specific buffer (catching a count that is plausible in
// isolation but larger than what this message could possibly carry). Either
// one failing is a clean decode error.
func checkElemCount(nelem, elemSize, available int) error {
	if nelem < 0 {
		return fmt.Errorf("nvlist: negative element count %d", nelem)
	}
	if nelem > maxDecodeElems {
		return fmt.Errorf("nvlist: element count %d exceeds sanity limit %d", nelem, maxDecodeElems)
	}
	if elemSize > 0 && nelem > available/elemSize {
		return fmt.Errorf("nvlist: element count %d (size %d) exceeds %d bytes available", nelem, elemSize, available)
	}
	return nil
}

// maxNVListDepth bounds how deeply DATA_TYPE_NVLIST/DATA_TYPE_NVLIST_ARRAY
// pairs may nest before decodeBody/decodePair's mutual recursion is refused.
// recover() (see cmd/tool's own recoverZFSCall) is not a sufficient defense
// against pathological recursion the way it is against a single panic: deep
// enough recursion exhausts the goroutine's stack, which is a fatal runtime
// error Go does not allow recover() to catch at all (unlike an ordinary
// panic). 64 is far beyond any real OpenZFS pool/feature config's own
// nesting (a vdev tree a few levels deep is the realistic maximum), while
// still generous enough that no legitimate response is ever rejected.
const maxNVListDepth = 64

// ---- Decoder ----

type nvDecoder struct {
	bo  binary.ByteOrder
	buf []byte
	pos int
}

// DecodeNative parses an NV_ENCODE_NATIVE stream (including the 4-byte outer
// header) into an Nvlist. This is the form the kernel writes into
// zc_nvlist_dst.
func DecodeNative(b []byte) (Nvlist, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("nvlist: short buffer (%d bytes)", len(b))
	}
	enc := b[0]
	end := b[1]
	if enc != nvEncodeNative {
		return nil, fmt.Errorf("nvlist: not native encoding (got %d)", enc)
	}
	var bo binary.ByteOrder = binary.LittleEndian
	if end == nvBigEndian {
		bo = binary.BigEndian
	}
	d := &nvDecoder{bo: bo, buf: b, pos: 4}
	return d.decodeList()
}

// decodeList reads the top-level [version,nvflag] header then the pair body.
func (d *nvDecoder) decodeList() (Nvlist, error) {
	if d.pos+nvlistHdrLen > len(d.buf) {
		return nil, fmt.Errorf("nvlist: truncated header at %d", d.pos)
	}
	d.pos += nvlistHdrLen // skip nvl_version, nvl_nvflag
	return d.decodeBody(0)
}

// decodeBody reads pairs until the 4-byte zero terminator. Used by both the
// top-level list (after its header) and embedded lists (after their 24-byte
// packed nvlist_t value slot). depth is this list's own nesting depth (0 for
// the top-level list) - threaded through so decodePair can refuse to recurse
// past maxNVListDepth.
func (d *nvDecoder) decodeBody(depth int) (Nvlist, error) {
	out := make(Nvlist)
	for {
		if d.pos+4 > len(d.buf) {
			return nil, fmt.Errorf("nvlist: truncated at %d", d.pos)
		}
		size := int(int32(d.bo.Uint32(d.buf[d.pos : d.pos+4])))
		if size == 0 {
			d.pos += 4 // consume terminator
			return out, nil
		}
		if size < nvpairHdrLen || d.pos+size > len(d.buf) {
			return nil, fmt.Errorf("nvlist: bad nvp_size %d at %d", size, d.pos)
		}
		name, v, err := d.decodePair(size, depth)
		if err != nil {
			return nil, err
		}
		out[name] = v
	}
}

// decodePair reads the pair header+value at d.pos (advancing d.pos by size),
// then, for embedded (nv)list types, recursively consumes the nested pair
// stream(s) that follow inline in the same buffer (advancing d.pos further).
// depth is the ENCLOSING list's own depth (decodeBody's own param); a nested
// list decoded from here is one level deeper.
func (d *nvDecoder) decodePair(size, depth int) (string, Value, error) {
	b := d.buf[d.pos : d.pos+size]
	nameSz := int(d.bo.Uint16(b[4:6]))
	nelem := int(int32(d.bo.Uint32(b[8:12])))
	typ := int(int32(d.bo.Uint32(b[12:16])))
	if nameSz < 1 || nvpairHdrLen+nameSz > size {
		return "", nil, fmt.Errorf("nvlist: bad name_sz %d", nameSz)
	}
	name := string(b[16 : 16+nameSz-1]) // strip NUL
	valOff := nvAlign8(nvpairHdrLen + nameSz)
	if valOff > size {
		return "", nil, fmt.Errorf("nvlist %q: value offset %d > size %d", name, valOff, size)
	}
	val := b[valOff:size]
	d.pos += size // consume the pair itself

	switch typ {
	case dataTypeNVList:
		// The pair value is only the 24-byte nvlist_t struct; the nested
		// pairs follow inline in the parent buffer at d.pos.
		if depth+1 > maxNVListDepth {
			return "", nil, fmt.Errorf("nvpair %q: nesting depth exceeds %d", name, maxNVListDepth)
		}
		l, err := d.decodeBody(depth + 1)
		if err != nil {
			return "", nil, fmt.Errorf("nvpair %q: %w", name, err)
		}
		return name, l, nil
	case dataTypeNVListArray:
		// nelem inlined pair streams follow, each at least 4 bytes (an
		// empty list's own zero terminator) - see checkElemCount's own
		// doc comment for why this is checked before allocating.
		if err := checkElemCount(nelem, 4, len(d.buf)-d.pos); err != nil {
			return "", nil, fmt.Errorf("nvpair %q: %w", name, err)
		}
		if depth+1 > maxNVListDepth {
			return "", nil, fmt.Errorf("nvpair %q: nesting depth exceeds %d", name, maxNVListDepth)
		}
		out := make([]Nvlist, nelem)
		for i := 0; i < nelem; i++ {
			l, err := d.decodeBody(depth + 1)
			if err != nil {
				return "", nil, fmt.Errorf("nvpair %q[%d]: %w", name, i, err)
			}
			out[i] = l
		}
		return name, out, nil
	default:
		v, err := d.decodeScalar(typ, nelem, val)
		if err != nil {
			return "", nil, fmt.Errorf("nvpair %q: %w", name, err)
		}
		return name, v, nil
	}
}

// requireBytes fails cleanly if val is shorter than a fixed-width scalar
// type's own byte width. checkElemCount bounds ARRAY types against nelem;
// this is the equivalent guard for the plain scalar types
// (BOOLEAN_VALUE/BYTE/UINT64/INT64/UINT32/INT32), whose length is implied by
// nvp_type alone. Without it, a malformed pair whose nvp_size leaves an
// empty or undersized value region (nvp_size only bounds the pair as a
// whole, not that its value region is exactly type-sized) panics on the
// direct val[:n]/val[0] index below instead of returning a decode error.
func requireBytes(val []byte, n int) error {
	if len(val) < n {
		return fmt.Errorf("nvlist: value region too short (%d bytes, want >= %d)", len(val), n)
	}
	return nil
}

func (d *nvDecoder) decodeScalar(typ, nelem int, val []byte) (Value, error) {
	switch typ {
	case dataTypeBoolean:
		return Boolean{}, nil
	case dataTypeBooleanValue:
		if err := requireBytes(val, 4); err != nil {
			return nil, err
		}
		return d.bo.Uint32(val[:4]) != 0, nil
	case dataTypeByte:
		if err := requireBytes(val, 1); err != nil {
			return nil, err
		}
		return Byte(val[0]), nil
	case dataTypeByteArray:
		if err := checkElemCount(nelem, 1, len(val)); err != nil {
			return nil, err
		}
		out := make([]byte, nelem)
		copy(out, val[:nelem])
		return out, nil
	case dataTypeUint8Array:
		if err := checkElemCount(nelem, 1, len(val)); err != nil {
			return nil, err
		}
		out := make(Uint8Array, nelem)
		copy(out, val[:nelem])
		return out, nil
	case dataTypeUint64:
		if err := requireBytes(val, 8); err != nil {
			return nil, err
		}
		return d.bo.Uint64(val[:8]), nil
	case dataTypeInt64:
		if err := requireBytes(val, 8); err != nil {
			return nil, err
		}
		return int64(d.bo.Uint64(val[:8])), nil
	case dataTypeUint32:
		if err := requireBytes(val, 4); err != nil {
			return nil, err
		}
		return d.bo.Uint32(val[:4]), nil
	case dataTypeInt32:
		if err := requireBytes(val, 4); err != nil {
			return nil, err
		}
		return int32(d.bo.Uint32(val[:4])), nil
	case dataTypeString:
		s, err := cstr(val)
		if err != nil {
			return nil, err
		}
		return s, nil
	case dataTypeUint64Array:
		if err := checkElemCount(nelem, 8, len(val)); err != nil {
			return nil, err
		}
		out := make([]uint64, nelem)
		for i := 0; i < nelem; i++ {
			out[i] = d.bo.Uint64(val[i*8 : i*8+8])
		}
		return out, nil
	case dataTypeStringArray:
		// nelem*8 ptr slots then concatenated NUL strings - only the ptr
		// slots themselves have a fixed, checkable size; each string's own
		// length depends on where its own NUL terminator falls, which
		// cstr() now requires to actually be present (see its own doc
		// comment) rather than silently returning "everything left" for a
		// truncated final string - the offset computed from THAT string's
		// own (possibly wrong) length is what fed the next iteration's
		// val[off:] slice, which is where an untermined string previously
		// caused a slice-bounds panic instead of a clean decode error.
		if err := checkElemCount(nelem, 8, len(val)); err != nil {
			return nil, err
		}
		out := make([]string, nelem)
		off := nelem * 8
		for i := 0; i < nelem; i++ {
			if off > len(val) {
				return nil, fmt.Errorf("nvlist: string array element %d starts past the value region (off=%d, len=%d)", i, off, len(val))
			}
			s, err := cstr(val[off:])
			if err != nil {
				return nil, fmt.Errorf("nvlist: string array element %d: %w", i, err)
			}
			out[i] = s
			off += len(s) + 1
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported decode type %d", typ)
	}
}

// cstr reads a NUL-terminated string starting at the beginning of b,
// requiring the terminator to actually be present within b - a native
// nvlist string value is always NUL-terminated on the wire (both the
// single-string and string-array cases), so a buffer that runs out before
// finding one is malformed, not merely a short string. Previously this
// silently returned the whole remaining slice as the string when no NUL was
// found, which for a string ARRAY meant the next iteration's own offset
// computation (off += len(s)+1) could run past len(val), panicking on the
// following val[off:] slice instead of failing cleanly here.
func cstr(b []byte) (string, error) {
	for i, c := range b {
		if c == 0 {
			return string(b[:i]), nil
		}
	}
	return "", fmt.Errorf("nvlist: string value has no NUL terminator (%d bytes)", len(b))
}
