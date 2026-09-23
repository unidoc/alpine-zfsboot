// Original work: SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, go-fsctl
//
// Modifications: Copyright (c) 2026, UniDoc
// See abi.go for this package's full provenance note.
//
// This is the direct regression suite for checkElemCount (nvlist.go) — the
// bounds-check fix for a real vulnerability found in an independent
// source-code security review of the pre-fork upstream decoder: a
// corrupted/malformed nvp_value_elem used to be trusted directly as a
// make() length, unbounded. A negative-control run (temporarily removing
// checkElemCount's call sites) confirmed the exact predicted failure mode:
// not a clean test failure, but a genuine unrecovered panic
// ("slice bounds out of range [:1073741824] with capacity 12") that would
// take down the whole process — see temp/hardening-ledger.md.

package zfsnative

import (
	"strings"
	"testing"
)

// buildCorruptedElemCountPair encodes a real, valid {"x": value} nvlist via
// EncodeNative, then patches the SINGLE pair's own nvp_value_elem field
// (offset 8 within the pair header, at a fixed byte offset from the start
// of the pair stream since "x" is always name_sz=2) to elem - simulating a
// corrupted/malformed count while leaving everything else (nvp_size,
// nvp_type, the actual value bytes) exactly as a real encoder produced it.
// This is the same corruption class a kernel bug, a hardware bit-flip, or
// a version-mismatched kernel module could plausibly produce - the
// surrounding stream is entirely well-formed, only this one field is
// wrong.
func buildCorruptedElemCountPair(t *testing.T, value Value, elem int32) []byte {
	t.Helper()
	b, err := EncodeNative(Nvlist{"x": value})
	if err != nil {
		t.Fatal(err)
	}
	// Layout: 4(outer header) + 8(nvlist_t header) + pair(nvp_size(4) +
	// nvp_name_sz(2) + nvp_reserve(2) + nvp_value_elem(4) + ...).
	const nelemOff = 4 + 8 + 4 + 2 + 2
	bo, _ := nvHostOrder()
	bo.PutUint32(b[nelemOff:nelemOff+4], uint32(elem))
	return b
}

func TestDecodeNative_CorruptedElemCount_ByteArray(t *testing.T) {
	b := buildCorruptedElemCountPair(t, []byte{1, 2, 3, 4}, 1<<30) // ~1GiB claimed, real payload is 4 bytes
	if _, err := DecodeNative(b); err == nil {
		t.Fatal("DecodeNative with a corrupted byte-array element count: want an error, got nil")
	}
}

func TestDecodeNative_CorruptedElemCount_Uint64Array(t *testing.T) {
	b := buildCorruptedElemCountPair(t, []uint64{1, 2, 3}, 1<<28) // would demand ~2GiB (nelem*8)
	if _, err := DecodeNative(b); err == nil {
		t.Fatal("DecodeNative with a corrupted uint64-array element count: want an error, got nil")
	}
}

func TestDecodeNative_CorruptedElemCount_StringArray(t *testing.T) {
	b := buildCorruptedElemCountPair(t, []string{"a", "b"}, 1<<20)
	if _, err := DecodeNative(b); err == nil {
		t.Fatal("DecodeNative with a corrupted string-array element count: want an error, got nil")
	}
}

// TestDecodeNative_CorruptedElemCount_Negative proves a negative element
// count (the top bit of the wire uint32 set) is rejected cleanly instead
// of reaching make() at all, where Go's own runtime would otherwise panic
// unconditionally ("makeslice: len out of range").
func TestDecodeNative_CorruptedElemCount_Negative(t *testing.T) {
	b := buildCorruptedElemCountPair(t, []byte{1, 2, 3, 4}, -1)
	_, err := DecodeNative(b)
	if err == nil {
		t.Fatal("DecodeNative with a negative element count: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "negative") {
		t.Errorf("error = %q, want it to specifically name the negative-count case", err.Error())
	}
}

// TestDecodeNative_CorruptedElemCount_NVListArray covers the
// variable-element-size embedded-list-array path separately from the
// fixed-element-size scalar arrays above - a different code path
// (decodePair, not decodeScalar) with its own, separate bound.
func TestDecodeNative_CorruptedElemCount_NVListArray(t *testing.T) {
	b := buildCorruptedElemCountPair(t, []Nvlist{{"a": uint64(1)}}, 1<<20)
	if _, err := DecodeNative(b); err == nil {
		t.Fatal("DecodeNative with a corrupted nvlist-array element count: want an error, got nil")
	}
}

// TestDecodeNative_ElemCountWithinSanityLimitButNotBuffer proves the
// buffer-size check fires even for a count that is UNDER maxDecodeElems -
// i.e. the two checks (sanity limit, and actual-bytes-available) are
// genuinely independent, not one subsuming the other by accident.
func TestDecodeNative_ElemCountWithinSanityLimitButNotBuffer(t *testing.T) {
	b := buildCorruptedElemCountPair(t, []uint64{1, 2, 3}, 1000) // well under maxDecodeElems, but 1000*8 bytes is not in this tiny buffer
	if _, err := DecodeNative(b); err == nil {
		t.Fatal("DecodeNative with an element count under the sanity limit but past the real buffer: want an error, got nil")
	}
}

// TestDecodeNative_LegitimateArraysStillDecode is the negative-facing
// control for all of the above: real, correctly-encoded arrays at
// various sizes must be entirely unaffected by the new bound.
func TestDecodeNative_LegitimateArraysStillDecode(t *testing.T) {
	in := Nvlist{
		"bytes":  []byte{1, 2, 3, 4, 5},
		"u64s":   []uint64{10, 20, 30},
		"strs":   []string{"a", "bb", "ccc"},
		"nested": []Nvlist{{"x": uint64(1)}, {"y": uint64(2)}},
	}
	b, err := EncodeNative(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeNative(b)
	if err != nil {
		t.Fatalf("DecodeNative on a legitimate, uncorrupted nvlist: %v", err)
	}
	if len(out) != len(in) {
		t.Errorf("decoded %d keys, want %d", len(out), len(in))
	}
}
