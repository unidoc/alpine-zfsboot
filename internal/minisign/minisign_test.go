package minisign

import (
	"strings"
	"testing"
)

// Every fixture below came from the REAL, unmodified upstream minisign
// 0.12 binary (jedisct1/minisign), not synthesized: `minisign -G -W`
// generated the key pair, `minisign -S -m small.txt -t "..."` (and,
// for the legacy case, `-l`) produced the signatures, and `minisign -V`
// independently confirmed each expected pass/fail outcome (including
// the two tamper cases below) against the real tool before this file
// was written - see this package's own doc comment on why: a hand-
// synthesized fixture could accidentally encode the same misunderstanding
// of the format as a buggy implementation, and never catch it.
const (
	testMessage = "hello world, small file\n"

	testPubKey = `untrusted comment: minisign public key 058AF5871AF97DD9
RWTZffkah/WKBbOINcG1+6Zigf42J2GPUnKSZo4geO9ZadgKR7moV4sw
`

	// The modern default ("ED", prehashed BLAKE2b-512) signature.
	testSigModern = `untrusted comment: signature from minisign secret key
RUTZffkah/WKBRs3uaf6Wh8jjyZBWVpnXIfbwUZj/10cbexgNBLy+4CXDCcorZkQJgt+exDZRwEMmMjjnRuZehk1k1w4bXwN3gQ=
trusted comment: test trusted comment small
ctNcMGyztHwvbrIz5TUKoyfY7AVYDkqQNikGIrBvRr2hLSrkZnwkKFd3zLPNj1o1HOP5Plkg8xhSN+PaEHd9Cw==
`

	// The legacy ("Ed", raw-over-message, minisign -S -l) signature -
	// same message, same key, deliberately a DIFFERENT signature (a
	// different trusted comment too) proving both algorithm tags are
	// independently handled, not just whichever one happens to be
	// tested first.
	testSigLegacy = `untrusted comment: signature from minisign secret key
RWTZffkah/WKBcKu+KOEe/AejpD9QGjbJcf12hKsN9aNXeg3QPduM+/A3slUAT9AjQsvxNuPaD5QQEAIwTxjFfRZYkHkS9FTdA8=
trusted comment: legacy
nyp9jxNOTMnMRHDVZKaxzGNMoC2B6pSzAE5RirzuxYvPYOtyxDTcnXu7eSnbQ34OJG/SaVrv5X3v58onowSgCg==
`

	// A second, UNRELATED key pair's own public key - used to prove a
	// real signature is refused when it's simply not in the trusted
	// set at all (a KeyID that matches nothing), a distinct failure
	// mode from "matched a trusted KeyID but the Ed25519 check itself
	// failed" below.
	testOtherPubKey = `untrusted comment: minisign public key 75D96941A7579443
RWRDlFenQWnZdfmZsfKSOTEeEff/shMPQF0Tu2cUgpw2ANyID7P5kQYD
`
)

func mustParsePubKey(t *testing.T, s string) PublicKey {
	t.Helper()
	pk, err := ParsePublicKey([]byte(s))
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	return pk
}

func TestVerify_RealMinisignSignature_ModernAlgorithm(t *testing.T) {
	pk := mustParsePubKey(t, testPubKey)
	matched, comment, err := Verify([]byte(testMessage), []byte(testSigModern), []PublicKey{pk})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if matched.KeyID != pk.KeyID {
		t.Errorf("matched KeyID = %x, want %x", matched.KeyID, pk.KeyID)
	}
	if comment != "test trusted comment small" {
		t.Errorf("trusted comment = %q, want %q", comment, "test trusted comment small")
	}
}

func TestVerify_RealMinisignSignature_LegacyAlgorithm(t *testing.T) {
	pk := mustParsePubKey(t, testPubKey)
	_, comment, err := Verify([]byte(testMessage), []byte(testSigLegacy), []PublicKey{pk})
	if err != nil {
		t.Fatalf("Verify (legacy \"Ed\" algorithm): %v", err)
	}
	if comment != "legacy" {
		t.Errorf("trusted comment = %q, want %q", comment, "legacy")
	}
}

// TestVerify_TamperedMessage is the regression test for the most basic
// property: a message that does not match what was actually signed
// must be refused. Real minisign itself, run against the same
// tampered content, printed "Signature verification failed" - this
// proves this package fails the same real case, not a synthetic one.
func TestVerify_TamperedMessage(t *testing.T) {
	pk := mustParsePubKey(t, testPubKey)
	tampered := testMessage + "extra\n"
	_, _, err := Verify([]byte(tampered), []byte(testSigModern), []PublicKey{pk})
	if err == nil {
		t.Fatal("Verify on a tampered message: want an error, got nil")
	}
}

// TestVerify_TamperedTrustedComment is the regression test for the
// property a naive "just check the main signature" implementation
// would miss entirely: real minisign, given a genuine signature file
// with ONLY the trusted-comment TEXT swapped (main signature line
// untouched, message untouched), refuses it with "Comment signature
// verification failed" - a distinct check from the main Ed25519
// verification, which would still pass on its own. This proves this
// package's own global-signature check (Verify's own second
// ed25519.Verify call) actually runs and actually rejects, not just
// that the function has code for it.
func TestVerify_TamperedTrustedComment(t *testing.T) {
	pk := mustParsePubKey(t, testPubKey)
	hacked := strings.Replace(testSigModern, "test trusted comment small", "HACKED comment", 1)
	if hacked == testSigModern {
		t.Fatal("test setup bug: replacement did not change anything")
	}
	_, _, err := Verify([]byte(testMessage), []byte(hacked), []PublicKey{pk})
	if err == nil {
		t.Fatal("Verify with a substituted trusted comment (main signature line untouched): want an error, got nil")
	}
	if !strings.Contains(err.Error(), "trusted-comment") && !strings.Contains(err.Error(), "global") {
		t.Errorf("error = %q, want it to specifically mention the trusted-comment/global signature check, not just any failure", err.Error())
	}
}

// TestVerify_KeyNotTrusted proves a real, validly-formed signature
// from a key that simply isn't in the caller's trusted set is refused
// - the actual security property this whole package exists for
// (verifying against an EMBEDDED key, not "any key that happens to
// accompany the download").
func TestVerify_KeyNotTrusted(t *testing.T) {
	other := mustParsePubKey(t, testOtherPubKey)
	_, _, err := Verify([]byte(testMessage), []byte(testSigModern), []PublicKey{other})
	if err == nil {
		t.Fatal("Verify with only an unrelated key in the trusted set: want an error, got nil")
	}
}

func TestVerify_EmptyTrustedSet(t *testing.T) {
	_, _, err := Verify([]byte(testMessage), []byte(testSigModern), nil)
	if err == nil {
		t.Fatal("Verify with an empty trusted-key set: want an error, got nil")
	}
}

func TestVerify_MultipleTrustedKeys_MatchesTheRightOne(t *testing.T) {
	other := mustParsePubKey(t, testOtherPubKey)
	real := mustParsePubKey(t, testPubKey)
	matched, _, err := Verify([]byte(testMessage), []byte(testSigModern), []PublicKey{other, real})
	if err != nil {
		t.Fatalf("Verify with the real key present alongside an unrelated one: %v", err)
	}
	if matched.KeyID != real.KeyID {
		t.Errorf("matched the wrong key: got %x, want %x", matched.KeyID, real.KeyID)
	}
}

func TestParsePublicKey_MalformedInputsRejected(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"empty", ""},
		{"not base64", "untrusted comment: x\nnot valid base64!!!\n"},
		{"too short", "untrusted comment: x\n" + "AAAA\n"},
		{"wrong key-type tag", "untrusted comment: x\n" + "RUTZffkah/WKBbOINcG1+6Zigf42J2GPUnKSZo4geO9ZadgKR7moV4sw\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParsePublicKey([]byte(tc.data)); err == nil {
				t.Errorf("ParsePublicKey(%q): want an error, got nil", tc.data)
			}
		})
	}
}

func TestVerify_MalformedSignatureRejected(t *testing.T) {
	pk := mustParsePubKey(t, testPubKey)
	cases := []struct {
		name string
		sig  string
	}{
		{"empty", ""},
		{"no untrusted comment line", "just some text\nAAAA\n"},
		{"no trusted comment line at all", "untrusted comment: x\n" + strings.SplitN(testSigModern, "\n", 3)[1] + "\n"},
		{"signature line not base64", "untrusted comment: x\nnot valid base64!!!\ntrusted comment: y\nAAAA\n"},
		{"global signature line not base64", "untrusted comment: x\n" + strings.SplitN(testSigModern, "\n", 3)[1] + "\ntrusted comment: y\nnot valid base64!!!\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := Verify([]byte(testMessage), []byte(tc.sig), []PublicKey{pk}); err == nil {
				t.Errorf("Verify with malformed signature data (%s): want an error, got nil", tc.name)
			}
		})
	}
}
