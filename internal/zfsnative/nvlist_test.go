// Original work: SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, go-fsctl
//
// Modifications: Copyright (c) 2026, UniDoc
// See abi.go for this package's full provenance note.

package zfsnative

import (
	"reflect"
	"testing"
)

func TestNativeRoundTrip(t *testing.T) {
	in := Nvlist{
		"version":   uint64(5000),
		"name":      "testpool",
		"state":     uint64(0),
		"flag":      true,
		"abool":     Boolean{},
		"abyte":     Byte(7),
		"i32":       int32(-3),
		"u32":       uint32(42),
		"i64":       int64(-1234567890123),
		"children":  []uint64{1, 2, 3, 4},
		"strs":      []string{"alpha", "be", "gamma"},
		"vdev_tree": Nvlist{"type": "root", "id": uint64(0), "guid": uint64(0xdeadbeef)},
		"holes":     []Nvlist{{"a": uint64(1)}, {"b": "two"}},
	}
	b, err := EncodeNative(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if b[0] != nvEncodeNative {
		t.Fatalf("outer header encoding = %d, want %d", b[0], nvEncodeNative)
	}
	out, err := DecodeNative(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip mismatch:\n in=%#v\nout=%#v", in, out)
	}
}

func TestNativeEmpty(t *testing.T) {
	b, err := EncodeNative(Nvlist{})
	if err != nil {
		t.Fatal(err)
	}
	// 4-byte outer header + 8-byte list header + 4-byte terminator.
	if len(b) != 4+nvlistHdrLen+4 {
		t.Fatalf("empty nvlist len = %d, want %d", len(b), 4+nvlistHdrLen+4)
	}
	out, err := DecodeNative(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("decoded non-empty: %v", out)
	}
}

// TestNativePairLayout checks the exact on-the-wire framing of a single
// uint64 pair against the OpenZFS native layout (NVP_SIZE_CALC).
func TestNativePairLayout(t *testing.T) {
	bo, end := nvHostOrder()
	b, err := EncodeNative(Nvlist{"x": uint64(0x1122334455667788)})
	if err != nil {
		t.Fatal(err)
	}
	// outer(4) + list hdr(8) ...
	if b[0] != nvEncodeNative || b[1] != end {
		t.Fatalf("outer header = %v", b[:4])
	}
	p := 4 + nvlistHdrLen
	size := int(int32(bo.Uint32(b[p : p+4])))
	nameSz := int(bo.Uint16(b[p+4 : p+6]))
	nelem := int(bo.Uint32(b[p+8 : p+12]))
	typ := int(bo.Uint32(b[p+12 : p+16]))
	// name "x" -> name_sz = 2 (incl NUL); valOff = align8(16+2)=24;
	// value 8 bytes -> align8(8)=8; size = 24+8 = 32.
	if nameSz != 2 {
		t.Errorf("name_sz = %d, want 2", nameSz)
	}
	if nelem != 1 {
		t.Errorf("nelem = %d, want 1", nelem)
	}
	if typ != dataTypeUint64 {
		t.Errorf("type = %d, want %d", typ, dataTypeUint64)
	}
	if size != 32 {
		t.Errorf("nvp_size = %d, want 32", size)
	}
	valOff := nvAlign8(nvpairHdrLen + nameSz)
	if valOff != 24 {
		t.Errorf("valOff = %d, want 24", valOff)
	}
	got := bo.Uint64(b[p+valOff : p+valOff+8])
	if got != 0x1122334455667788 {
		t.Errorf("value = %#x", got)
	}
}

func TestNativeRejectsXDR(t *testing.T) {
	// XDR-encoded stream: first byte is encoding == 1 only when LE; the XDR
	// codec marks encoding NV_ENCODE_XDR(=1) in byte 0 too, so we detect a
	// definitely-not-native buffer instead.
	_, err := DecodeNative([]byte{nvEncodeXDR + 9, 0, 0, 0, 0, 0, 0, 0})
	if err == nil {
		t.Fatal("expected error decoding non-native buffer")
	}
}
