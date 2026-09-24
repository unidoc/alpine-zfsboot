// Original work: SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, go-fsctl
//
// Modifications: Copyright (c) 2026, UniDoc
// See abi.go for this package's full provenance note.
//
// This is the regression suite for a second round of decoder hardening,
// found by an independent Opus-model source review AND separately by the
// user's own advisor, after the first checkElemCount fix (see
// nvlist_bounds_test.go) closed the unbounded-array-allocation finding:
//
//  1. requireBytes (nvlist.go): decodeScalar's fixed-width types
//     (BOOLEAN_VALUE/BYTE/UINT64/INT64/UINT32/INT32) indexed val[:4]/val[:8]/
//     val[0] with no length check - checkElemCount only bounds ARRAY types
//     against nelem, not these. A malformed nvp_size/nvp_type combination
//     leaving a too-short value region panicked instead of erroring cleanly.
//  2. cstr (nvlist.go): a string value with no NUL terminator was silently
//     accepted as "the rest of the buffer" instead of rejected. For a
//     STRING_ARRAY specifically, the resulting (wrong) computed length fed
//     the NEXT iteration's own offset, which could run past len(val) and
//     panic on that following slice operation - not merely a wrong string.
//  3. maxNVListDepth (nvlist.go): decodeBody/decodePair's mutual recursion
//     for DATA_TYPE_NVLIST/DATA_TYPE_NVLIST_ARRAY had no depth bound.
//     Unlike an ordinary panic, Go stack exhaustion is a `fatal error` that
//     recover() (cmd/tool's own recoverZFSCall) CANNOT catch at all - a
//     more severe class than the panics checkElemCount/requireBytes/cstr
//     guard against, independently confirmed via a real proof-of-concept
//     (a well-formed, deeply-nested stream reliably produced
//     "fatal error: stack overflow" with the depth bound removed).
package zfsnative

import (
	"strings"
	"testing"
)

// buildMinimalPairBuffer hand-assembles a complete NV_ENCODE_NATIVE stream
// (outer header + list header + exactly one pair + terminator) with
// caller-controlled value bytes - unlike buildCorruptedElemCountPair (which
// encodes via the real encoder then patches one field), this builds the
// pair's value region directly, with EXACT (not 8-byte-aligned-padded)
// control over nvp_size - a real encoder always 8-byte-aligns nvp_size, and
// naively deriving nvp_size from nvAlign8(len(valueBytes)) here would
// silently zero-pad short/unterminated value bytes back up to the type's
// own natural width, defeating the entire point of this fixture (a
// zero-padded "abc" IS NUL-terminated; a zero-padded 3-byte UINT64 value
// IS 8 bytes long - neither would reproduce the malformed shape under
// test). nvp_size is instead taken directly from the caller.
//
// A from-scratch byte assembler, not the encoder, so a bug shared between
// encoder and decoder can't hide a real decoder bug - same reasoning this
// project's other from-scratch fixture builders use (see e.g.
// internal/initrdinfo's appendCPIOEntry).
func buildMinimalPairBuffer(t *testing.T, name string, typ int32, nelem int32, declaredSize int, valueBytes []byte) []byte {
	t.Helper()
	bo, end := nvHostOrder()

	buf := []byte{nvEncodeNative, end, 0, 0} // outer header

	var listHdr [nvlistHdrLen]byte
	bo.PutUint32(listHdr[0:4], 0) // nvl_version
	bo.PutUint32(listHdr[4:8], 1) // nvl_nvflag
	buf = append(buf, listHdr[:]...)

	nameZ := name + "\x00"
	nameSz := len(nameZ)
	valOff := nvAlign8(nvpairHdrLen + nameSz)
	if declaredSize < valOff+len(valueBytes) {
		t.Fatalf("buildMinimalPairBuffer: declaredSize %d too small to hold valOff(%d)+len(valueBytes)(%d)", declaredSize, valOff, len(valueBytes))
	}

	pair := make([]byte, declaredSize)
	bo.PutUint32(pair[0:4], uint32(declaredSize))
	bo.PutUint16(pair[4:6], uint16(nameSz))
	bo.PutUint16(pair[6:8], 0)
	bo.PutUint32(pair[8:12], uint32(nelem))
	bo.PutUint32(pair[12:16], uint32(typ))
	copy(pair[16:16+len(nameZ)], nameZ)
	copy(pair[valOff:valOff+len(valueBytes)], valueBytes)
	buf = append(buf, pair...)

	buf = append(buf, 0, 0, 0, 0) // terminator
	return buf
}

func TestDecodeNative_ShortValue_BooleanValue(t *testing.T) {
	b := buildMinimalPairBuffer(t, "x", dataTypeBooleanValue, 1, 24, nil) // declaredSize=valOff(24)+0, want 4
	if _, err := DecodeNative(b); err == nil {
		t.Fatal("DecodeNative with a 0-byte BOOLEAN_VALUE: want an error, got nil")
	}
}

func TestDecodeNative_ShortValue_Byte(t *testing.T) {
	b := buildMinimalPairBuffer(t, "x", dataTypeByte, 1, 24, nil) // declaredSize=valOff(24)+0, want 1
	if _, err := DecodeNative(b); err == nil {
		t.Fatal("DecodeNative with a 0-byte BYTE: want an error, got nil")
	}
}

func TestDecodeNative_ShortValue_Uint64(t *testing.T) {
	b := buildMinimalPairBuffer(t, "x", dataTypeUint64, 1, 27, []byte{1, 2, 3}) // declaredSize=valOff(24)+3, want 8
	if _, err := DecodeNative(b); err == nil {
		t.Fatal("DecodeNative with a 3-byte UINT64: want an error, got nil")
	}
}

func TestDecodeNative_ShortValue_Int64(t *testing.T) {
	b := buildMinimalPairBuffer(t, "x", dataTypeInt64, 1, 24, nil) // declaredSize=valOff(24)+0, want 8
	if _, err := DecodeNative(b); err == nil {
		t.Fatal("DecodeNative with a 0-byte INT64: want an error, got nil")
	}
}

func TestDecodeNative_ShortValue_Uint32(t *testing.T) {
	b := buildMinimalPairBuffer(t, "x", dataTypeUint32, 1, 25, []byte{1}) // declaredSize=valOff(24)+1, want 4
	if _, err := DecodeNative(b); err == nil {
		t.Fatal("DecodeNative with a 1-byte UINT32: want an error, got nil")
	}
}

func TestDecodeNative_ShortValue_Int32(t *testing.T) {
	b := buildMinimalPairBuffer(t, "x", dataTypeInt32, 1, 24, nil) // declaredSize=valOff(24)+0, want 4
	if _, err := DecodeNative(b); err == nil {
		t.Fatal("DecodeNative with a 0-byte INT32: want an error, got nil")
	}
}

// TestDecodeNative_LegitimateScalarsStillDecode is the negative-facing
// control for the above: real, correctly-sized scalar values of every
// fixed-width type must be entirely unaffected by requireBytes.
func TestDecodeNative_LegitimateScalarsStillDecode(t *testing.T) {
	in := Nvlist{
		"bv":  true,
		"by":  Byte(9),
		"u64": uint64(0x1122334455667788),
		"i64": int64(-42),
		"u32": uint32(7),
		"i32": int32(-7),
	}
	b, err := EncodeNative(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeNative(b)
	if err != nil {
		t.Fatalf("DecodeNative on legitimate scalars: %v", err)
	}
	if len(out) != len(in) {
		t.Errorf("decoded %d keys, want %d", len(out), len(in))
	}
}

func TestDecodeNative_String_NoNULTerminator(t *testing.T) {
	b := buildMinimalPairBuffer(t, "x", dataTypeString, 1, 27, []byte("abc")) // declaredSize=valOff(24)+3, no trailing NUL
	_, err := DecodeNative(b)
	if err == nil {
		t.Fatal("DecodeNative with a non-NUL-terminated string: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "NUL terminator") {
		t.Errorf("error = %q, want it to mention the missing NUL terminator", err.Error())
	}
}

// TestDecodeNative_StringArray_LastElementNotTerminated is the direct
// regression test for the real slice-bounds panic path found in review: a
// string array whose LAST string has no NUL terminator used to make the
// loop's own off += len(s)+1 run past len(val), panicking on the next
// (nonexistent) iteration's val[off:] slice - except with only one element
// there IS no next iteration, so this specifically requires a genuinely
// out-of-bounds off computed on the very last legitimate-looking element
// (a string whose bytes fill exactly to len(val), leaving cstr nothing to
// find a NUL in).
func TestDecodeNative_StringArray_LastElementNotTerminated(t *testing.T) {
	// One element: "hello" (5 bytes, no NUL) exactly filling the value
	// region after the 8-byte pointer slot.
	val := append([]byte{0, 0, 0, 0, 0, 0, 0, 0}, []byte("hello")...)
	b := buildMinimalPairBuffer(t, "x", dataTypeStringArray, 1, 24+len(val), val)
	_, err := DecodeNative(b)
	if err == nil {
		t.Fatal("DecodeNative with a non-NUL-terminated string array element: want an error, got nil")
	}
}

// TestDecodeNative_StringArray_MultiElementLastNotTerminated forces the
// SECOND iteration's val[off:] to actually be evaluated past a corrupted
// offset (off > len(val), not just off == len(val)), the case that panicked
// ("slice bounds out of range") before this fix rather than merely
// returning a wrong result.
func TestDecodeNative_StringArray_MultiElementLastNotTerminated(t *testing.T) {
	// Two elements, 2*8=16 byte ptr region, then "a\x00" (terminated) then
	// "bcdef" (NOT terminated, fills to the exact end of val).
	val := make([]byte, 16)
	val = append(val, 'a', 0)
	val = append(val, []byte("bcdef")...)
	b := buildMinimalPairBuffer(t, "x", dataTypeStringArray, 2, 24+len(val), val)
	_, err := DecodeNative(b)
	if err == nil {
		t.Fatal("DecodeNative with a multi-element string array's last element non-NUL-terminated: want an error, got nil")
	}
}

// TestDecodeNative_LegitimateStringArrayStillDecodes is the negative-facing
// control: a real, correctly NUL-terminated string array is unaffected.
func TestDecodeNative_LegitimateStringArrayStillDecodes(t *testing.T) {
	in := Nvlist{"strs": []string{"alpha", "", "gamma"}}
	b, err := EncodeNative(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeNative(b)
	if err != nil {
		t.Fatalf("DecodeNative on a legitimate string array: %v", err)
	}
	got, ok := out["strs"].([]string)
	if !ok || len(got) != 3 || got[0] != "alpha" || got[1] != "" || got[2] != "gamma" {
		t.Errorf("strs = %#v", out["strs"])
	}
}

// buildNestedNvlist builds a Boolean-leaf Nvlist nested depth levels deep
// under key "n", for driving maxNVListDepth.
func buildNestedNvlist(depth int) Nvlist {
	leaf := Nvlist{"leaf": uint64(1)}
	cur := leaf
	for i := 0; i < depth; i++ {
		cur = Nvlist{"n": cur}
	}
	return cur
}

// TestDecodeNative_NestingDepthExceeded is the regression test for
// maxNVListDepth: a well-formed (not corrupted in any other way) but
// pathologically deep nvlist must be refused with a clean error rather
// than recursing until the goroutine's stack is exhausted (a fatal error,
// not a panic - unrecoverable even by cmd/tool's own recoverZFSCall; see
// this file's own header comment).
func TestDecodeNative_NestingDepthExceeded(t *testing.T) {
	nv := buildNestedNvlist(maxNVListDepth + 10)
	b, err := EncodeNative(nv)
	if err != nil {
		t.Fatal(err)
	}
	_, err = DecodeNative(b)
	if err == nil {
		t.Fatal("DecodeNative on an nvlist nested past maxNVListDepth: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "nesting depth") {
		t.Errorf("error = %q, want it to mention nesting depth", err.Error())
	}
}

// TestDecodeNative_NestingWithinDepthStillDecodes is the negative-facing
// control: real, shallow nesting (the only kind any real OpenZFS pool
// config ever produces - a vdev tree a few levels deep) is unaffected.
func TestDecodeNative_NestingWithinDepthStillDecodes(t *testing.T) {
	nv := buildNestedNvlist(5)
	b, err := EncodeNative(nv)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeNative(b)
	if err != nil {
		t.Fatalf("DecodeNative on legitimately-shallow nesting: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("decoded %d top-level keys, want 1", len(out))
	}
}

// buildNestedNvlistViaArrays is buildNestedNvlist's own DATA_TYPE_NVLIST_ARRAY
// analogue - nests depth levels DEEP, every single level through a
// one-element array ([]Nvlist{...}), never falling through to plain
// DATA_TYPE_NVLIST nesting at any level.
func buildNestedNvlistViaArrays(depth int) Nvlist {
	cur := Nvlist{"leaf": uint64(1)}
	for i := 0; i < depth; i++ {
		cur = Nvlist{"arr": []Nvlist{cur}}
	}
	return cur
}

// TestDecodeNative_NestingDepthExceeded_NVListArray is the direct
// regression test for the SECOND of F14's four unguarded call sites
// (unidoc-alip's PR #5 follow-up review): decodePair's two nesting
// branches (DATA_TYPE_NVLIST, DATA_TYPE_NVLIST_ARRAY) each have their own
// separate depth check - the ORIGINAL version of this test wrapped a
// DATA_TYPE_NVLIST_ARRAY around a deeply-nested chain built entirely out
// of plain DATA_TYPE_NVLIST pairs (buildNestedNvlist), so the actual deep
// recursion it exercised ran through DATA_TYPE_NVLIST's own check, not
// DATA_TYPE_NVLIST_ARRAY's - confirmed directly: commenting out ONLY the
// array branch's own depth check (nvlist.go, decodePair's
// dataTypeNVListArray case) left the ORIGINAL version of this test
// (and the whole rest of `go test ./internal/zfsnative/...`) green.
// Nesting through buildNestedNvlistViaArrays instead - every level a
// one-element array, never a plain nested list - routes the entire deep
// chain through DATA_TYPE_NVLIST_ARRAY's own recursive decodeBody call,
// so only ITS OWN depth check can catch this.
func TestDecodeNative_NestingDepthExceeded_NVListArray(t *testing.T) {
	nv := buildNestedNvlistViaArrays(maxNVListDepth + 10)
	b, err := EncodeNative(nv)
	if err != nil {
		t.Fatal(err)
	}
	_, err = DecodeNative(b)
	if err == nil {
		t.Fatal("DecodeNative on an nvlist-array nested past maxNVListDepth: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "nesting depth") {
		t.Errorf("error = %q, want it to mention nesting depth", err.Error())
	}
}

// TestDecodeNative_NestingWithinDepthStillDecodes_NVListArray is the
// negative-facing control for the array-nesting test above: real,
// shallow array nesting must be entirely unaffected.
func TestDecodeNative_NestingWithinDepthStillDecodes_NVListArray(t *testing.T) {
	nv := buildNestedNvlistViaArrays(5)
	b, err := EncodeNative(nv)
	if err != nil {
		t.Fatal(err)
	}
	_, err = DecodeNative(b)
	if err != nil {
		t.Fatalf("DecodeNative on legitimately-shallow array nesting: %v", err)
	}
}
