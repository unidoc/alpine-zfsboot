// Original work: SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, go-fsctl
//
// Modifications: Copyright (c) 2026, UniDoc
// See abi.go for this package's full provenance note.
//
// This file is upstream's ENCODE half of the NV_ENCODE_NATIVE codec
// (nvlist.go carries the DECODE half). It is a _test.go file — excluded
// from every production build — because no reachable call in this package
// (PoolConfigs/PoolNames/PoolStats) ever needs to encode an nvlist to send
// to the kernel; alpine-zfsboot only ever reads. It is kept, test-only,
// because it is the only trustworthy way to build realistic
// NV_ENCODE_NATIVE wire fixtures to test the decoder against — hand-authored
// byte literals would not actually prove the decoder handles real encoder
// output, and would make deliberately-corrupted fixtures (see
// nvlist_bounds_test.go) far more error-prone to construct.
package zfsnative

import (
	"encoding/binary"
	"fmt"
)

// nvEncodeXDR is not a real wire encoding this decoder ever accepts; it only
// exists here to build a deliberately-non-native fixture for
// TestNativeRejectsXDR.
const nvEncodeXDR = 1

// nvEncoder packs an Nvlist into NV_ENCODE_NATIVE bytes.
type nvEncoder struct {
	bo  binary.ByteOrder
	end byte
	buf []byte
}

// EncodeNative serializes nv as a top-level NV_ENCODE_NATIVE stream including
// the 4-byte outer header. This is the form the /dev/zfs ioctl expects in
// zc_nvlist_src — test-only here; see the file doc comment above.
func EncodeNative(nv Nvlist) ([]byte, error) {
	bo, end := nvHostOrder()
	e := &nvEncoder{bo: bo, end: end}
	// Outer 4-byte header.
	e.buf = append(e.buf, nvEncodeNative, end, 0, 0)
	if err := e.encodeList(nv, 0, 1); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// encodeList writes the top-level nvlist_t header (version + nvflag) then the
// body. version/nvflag mirror what the kernel produces (0 / 1 =
// NV_UNIQUE_NAME). Embedded lists do NOT use this — their version/nvflag live
// in the preceding 24-byte packed nvlist_t value slot (see encodeEmbedded).
func (e *nvEncoder) encodeList(nv Nvlist, version int32, nvflag uint32) error {
	var hdr [nvlistHdrLen]byte
	e.bo.PutUint32(hdr[0:4], uint32(version))
	e.bo.PutUint32(hdr[4:8], nvflag)
	e.buf = append(e.buf, hdr[:]...)
	return e.encodeBody(nv)
}

// encodeBody writes every pair followed by the 4-byte zero terminator
// (nvp_size == 0). This is the shared pair stream used by both top-level and
// embedded lists.
func (e *nvEncoder) encodeBody(nv Nvlist) error {
	// Stable order: sort keys for determinism in tests; the kernel does not
	// require a particular order.
	for _, name := range sortedKeys(nv) {
		if err := e.encodePair(name, nv[name]); err != nil {
			return err
		}
	}
	e.buf = append(e.buf, 0, 0, 0, 0)
	return nil
}

func (e *nvEncoder) encodePair(name string, v Value) error {
	nameSz := len(name) + 1 // includes NUL
	valOff := nvAlign8(nvpairHdrLen + nameSz)

	val, trailer, typ, nelem, err := e.encodeValue(v)
	if err != nil {
		return fmt.Errorf("nvpair %q: %w", name, err)
	}
	valSz := nvAlign8(len(val))
	size := valOff + valSz // nvp_size covers ONLY the in-pair value

	start := len(e.buf)
	e.buf = append(e.buf, make([]byte, size)...)
	b := e.buf[start : start+size]
	e.bo.PutUint32(b[0:4], uint32(size))   // nvp_size
	e.bo.PutUint16(b[4:6], uint16(nameSz)) // nvp_name_sz
	e.bo.PutUint16(b[6:8], 0)              // nvp_reserve
	e.bo.PutUint32(b[8:12], uint32(nelem)) // nvp_value_elem
	e.bo.PutUint32(b[12:16], uint32(typ))  // nvp_type
	copy(b[16:16+len(name)], name)         // name (NUL already zero)
	copy(b[valOff:valOff+len(val)], val)   // value
	// For embedded (nv)lists, the nested pair stream(s) follow the pair
	// inline in the parent buffer — they are NOT counted in nvp_size.
	if len(trailer) > 0 {
		e.buf = append(e.buf, trailer...)
	}
	return nil
}

// encodeValue returns the in-pair value bytes (unpadded), an optional trailer
// (embedded pair streams written after the pair), the data_type_t, and
// nvp_value_elem.
func (e *nvEncoder) encodeValue(v Value) (val, trailer []byte, typ, nelem int, err error) {
	switch x := v.(type) {
	case Boolean:
		return nil, nil, dataTypeBoolean, 0, nil
	case bool:
		b := make([]byte, 4) // boolean_t == int
		if x {
			e.bo.PutUint32(b, 1)
		}
		return b, nil, dataTypeBooleanValue, 1, nil
	case Byte:
		return []byte{byte(x)}, nil, dataTypeByte, 1, nil
	case []byte:
		b := make([]byte, len(x))
		copy(b, x)
		return b, nil, dataTypeByteArray, len(x), nil
	case Uint8Array:
		b := make([]byte, len(x))
		copy(b, x)
		return b, nil, dataTypeUint8Array, len(x), nil
	case uint64:
		b := make([]byte, 8)
		e.bo.PutUint64(b, x)
		return b, nil, dataTypeUint64, 1, nil
	case int64:
		b := make([]byte, 8)
		e.bo.PutUint64(b, uint64(x))
		return b, nil, dataTypeInt64, 1, nil
	case uint32:
		b := make([]byte, 4)
		e.bo.PutUint32(b, x)
		return b, nil, dataTypeUint32, 1, nil
	case int32:
		b := make([]byte, 4)
		e.bo.PutUint32(b, uint32(x))
		return b, nil, dataTypeInt32, 1, nil
	case string:
		b := make([]byte, len(x)+1) // NUL-terminated
		copy(b, x)
		return b, nil, dataTypeString, 1, nil
	case []uint64:
		b := make([]byte, 8*len(x))
		for i, u := range x {
			e.bo.PutUint64(b[i*8:], u)
		}
		return b, nil, dataTypeUint64Array, len(x), nil
	case []string:
		// value = nelem*8 zeroed pointer slots, then concatenated
		// NUL-terminated strings.
		ptrs := len(x) * 8
		var strs []byte
		for _, s := range x {
			strs = append(strs, s...)
			strs = append(strs, 0)
		}
		b := make([]byte, ptrs+len(strs))
		copy(b[ptrs:], strs)
		return b, nil, dataTypeStringArray, len(x), nil
	case Nvlist:
		v, tr, err := e.encodeEmbedded(x)
		return v, tr, dataTypeNVList, 1, err
	case []Nvlist:
		v, tr, err := e.encodeEmbeddedArray(x)
		return v, tr, dataTypeNVListArray, len(x), err
	default:
		return nil, nil, 0, 0, fmt.Errorf("unsupported value type %T", v)
	}
}

// encodeEmbedded packs a single nested nvlist. The pair's in-value region is
// exactly the 24-byte packed nvlist_t (carrying version + nvflag); this is all
// that nvp_size covers. The nested list's pair body is returned as the
// trailer, which the caller appends to the parent buffer AFTER the pair — the
// native format inlines embedded pairs into the enclosing stream rather than
// nesting them inside the pair's value (verified against kernel output).
func (e *nvEncoder) encodeEmbedded(nv Nvlist) (val, trailer []byte, err error) {
	val = packedNvlistT(e.bo)
	sub := &nvEncoder{bo: e.bo, end: e.end}
	if err := sub.encodeBody(nv); err != nil {
		return nil, nil, err
	}
	return val, sub.buf, nil
}

// nvlistTSize is NV_ALIGN(sizeof(nvlist_t)) on 64-bit: int32 version + uint32
// nvflag + uint64 priv + uint32 flag + int32 pad = 24 bytes.
const nvlistTSize = 24

// packedNvlistT builds the 24-byte packed nvlist_t value slot for an embedded
// list: nvl_version=0 (NV_VERSION), nvl_nvflag=1 (NV_UNIQUE_NAME), the rest
// (priv/flag/pad) zero — exactly what the kernel emits.
func packedNvlistT(bo binary.ByteOrder) []byte {
	b := make([]byte, nvlistTSize)
	bo.PutUint32(b[0:4], 0) // nvl_version
	bo.PutUint32(b[4:8], 1) // nvl_nvflag = NV_UNIQUE_NAME
	return b
}

// encodeEmbeddedArray packs an array of nested nvlists. The in-value region is
// nelem zeroed pointer slots followed by nelem packed nvlist_t structs (24
// bytes each); the nested pair bodies are returned as the trailer (appended
// after the pair), in element order.
func (e *nvEncoder) encodeEmbeddedArray(lists []Nvlist) (val, trailer []byte, err error) {
	n := len(lists)
	val = make([]byte, n*8) // zeroed pointer array
	for range lists {
		val = append(val, packedNvlistT(e.bo)...)
	}
	for _, nv := range lists {
		sub := &nvEncoder{bo: e.bo, end: e.end}
		if err := sub.encodeBody(nv); err != nil {
			return nil, nil, err
		}
		trailer = append(trailer, sub.buf...)
	}
	return val, trailer, nil
}

func sortedKeys(nv Nvlist) []string {
	keys := make([]string, 0, len(nv))
	for k := range nv {
		keys = append(keys, k)
	}
	// simple insertion sort to avoid importing sort for a tiny map; keeps
	// encode deterministic for round-trip tests.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}
