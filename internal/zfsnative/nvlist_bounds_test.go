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
// take down the whole process.

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

// TestCheckElemCount_SanityCeiling is a direct unit test of
// checkElemCount's own maxDecodeElems branch (F14, unidoc-alip's PR #5
// follow-up review), isolated from the "bytes available" branch by
// giving it an `available` value large enough that ONLY the sanity
// ceiling - not the buffer-size arithmetic - could possibly reject
// nelem. Every DecodeNative-level test elsewhere in this file uses a
// small, realistic buffer, where the available-bytes check alone would
// already reject any nelem this large - this is the one place that
// specifically isolates the ceiling itself. Calling checkElemCount
// directly (an unexported function, same package) rather than building
// a real multi-megabyte wire buffer just to reach it.
func TestCheckElemCount_SanityCeiling(t *testing.T) {
	err := checkElemCount(maxDecodeElems+1, 1, maxDecodeElems+1)
	if err == nil {
		t.Fatal("checkElemCount(maxDecodeElems+1, ...) with more than enough bytes available: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "sanity limit") {
		t.Errorf("error = %q, want it to specifically name the sanity-limit case", err.Error())
	}
}

// TestCheckElemCount_AtCeilingIsFine is the negative-facing control:
// exactly maxDecodeElems, with enough bytes available, must be accepted
// - the ceiling is inclusive of its own boundary.
func TestCheckElemCount_AtCeilingIsFine(t *testing.T) {
	if err := checkElemCount(maxDecodeElems, 1, maxDecodeElems); err != nil {
		t.Errorf("checkElemCount(maxDecodeElems, ...) with exactly enough bytes: want nil, got %v", err)
	}
}

// TestCheckElemCount_Negative and TestCheckElemCount_AvailableBytes
// round out direct coverage of checkElemCount's remaining two branches,
// the same way the ceiling test above does - one test per branch,
// calling the function directly rather than only ever exercising it
// indirectly through a specific decodeScalar/decodePair call site.
func TestCheckElemCount_Negative(t *testing.T) {
	err := checkElemCount(-1, 8, 1000)
	if err == nil {
		t.Fatal("checkElemCount(-1, ...): want an error, got nil")
	}
	if !strings.Contains(err.Error(), "negative") {
		t.Errorf("error = %q, want it to specifically name the negative-count case", err.Error())
	}
}

func TestCheckElemCount_AvailableBytes(t *testing.T) {
	err := checkElemCount(1000, 8, 100) // 1000 elements of 8 bytes each, only 100 bytes available
	if err == nil {
		t.Fatal("checkElemCount(1000, 8, 100): want an error, got nil")
	}
	if !strings.Contains(err.Error(), "bytes available") {
		t.Errorf("error = %q, want it to specifically name the available-bytes case", err.Error())
	}
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
	_, err := DecodeNative(b)
	if err == nil {
		t.Fatal("DecodeNative with a corrupted string-array element count: want an error, got nil")
	}
	// F14 (unidoc-alip's PR #5 follow-up review): 1<<20 alone is not
	// enough to prove decodeScalar's own STRING_ARRAY checkElemCount
	// call site (nvlist.go, dataTypeStringArray's own case) is what
	// caught this - confirmed directly: commenting out ONLY that call
	// site left this test (checking merely err == nil) green, because
	// the SAME corrupted count still ran out of real string data a few
	// iterations into decodeScalar's own for-loop and returned a
	// different, generic "starts past the value region" error instead.
	// Asserting on checkElemCount's own specific error text closes that
	// gap - a generic truncation-shaped error no longer satisfies this
	// test.
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %q, want it to be checkElemCount's own bound error (mentioning \"exceeds\"), not some other decode failure", err.Error())
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
	_, err := DecodeNative(b)
	if err == nil {
		t.Fatal("DecodeNative with a corrupted nvlist-array element count: want an error, got nil")
	}
	// F14 (unidoc-alip's PR #5 follow-up review): 1<<20 alone does not
	// prove decodePair's own dataTypeNVListArray checkElemCount call
	// site is what caught this - confirmed directly: commenting out
	// ONLY that call site left this test (checking merely err == nil)
	// green, because the same corrupted count still ran decodeBody out
	// of real buffer a couple of iterations in and returned a
	// different, generic "nvlist: truncated at %d" error instead.
	// Asserting on checkElemCount's own specific error text closes that
	// gap.
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %q, want it to be checkElemCount's own bound error (mentioning \"exceeds\"), not some other decode failure", err.Error())
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
