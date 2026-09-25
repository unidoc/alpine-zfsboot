package release

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"testing"

	"golang.org/x/crypto/blake2b"

	"github.com/unidoc/alpine-zfsboot/internal/minisign"
)

// testSigningKeypair is an EPHEMERAL Ed25519 keypair generated fresh
// per test binary run, entirely unrelated to any real alpine-zfsboot
// signing key - see testSignMinisign's own comment for why this
// package's own tests can't just shell out to the real minisign
// binary to produce fixtures the way internal/minisign's own tests do.
var testSigningKeyID = [8]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}

func newTestSigningKeypair() (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		panic(err) // crypto/rand failing is not a case any test here can meaningfully recover from
	}
	return pub, priv
}

// testMinisignPublicKeyFile renders pub as a real minisign public-key
// file (the exact "untrusted comment:\n<base64>\n" shape
// minisign.ParsePublicKey itself expects) - used both to embed into
// trustedSigningKeys for the duration of a test and, structurally,
// matches what a real `minisign -G` run produces.
func testMinisignPublicKeyFile(pub ed25519.PublicKey) string {
	raw := append([]byte{'E', 'd'}, testSigningKeyID[:]...)
	raw = append(raw, pub...)
	return fmt.Sprintf("untrusted comment: test-only public key\n%s\n", base64.StdEncoding.EncodeToString(raw))
}

// testSignMinisign produces a real, byte-for-byte-correct minisign
// .minisig file for message, signed with priv, using the modern
// prehashed ("ED") algorithm - the same default modern minisign -S
// itself uses. Built directly from Ed25519 + BLAKE2b-512 rather than
// shelling out to a real minisign binary: this package's own tests
// run in ordinary `go test`, with no guarantee a minisign binary is
// present in whatever environment runs them (unlike internal/minisign's
// own tests, which are the ONE place in this project that vendors real
// upstream-tool-produced fixtures specifically to prove the format
// understanding itself is correct - see that package's own test file).
// The exact byte layout here (algorithm tag + KeyID + signature;
// global signature over the BARE 64-byte signature, not the 74-byte
// tagged blob, plus the trusted comment) is the same layout
// internal/minisign's own Verify() expects, confirmed against real
// minisign output there - reusing that same understanding here to
// construct fixtures, not re-deriving it independently, is exactly
// why those two package's tests together are real coverage: one
// proves the format understanding against the real tool, this one
// proves internal/release's own wiring against that SAME understanding.
func testSignMinisign(priv ed25519.PrivateKey, message []byte, trustedComment string) string {
	digest := blake2b.Sum512(message)
	sig := ed25519.Sign(priv, digest[:])

	sigRaw := append([]byte{'E', 'D'}, testSigningKeyID[:]...)
	sigRaw = append(sigRaw, sig...)

	globalSig := ed25519.Sign(priv, append(append([]byte(nil), sig...), []byte(trustedComment)...))

	return fmt.Sprintf(
		"untrusted comment: test signature\n%s\ntrusted comment: %s\n%s\n",
		base64.StdEncoding.EncodeToString(sigRaw),
		trustedComment,
		base64.StdEncoding.EncodeToString(globalSig),
	)
}

// withTestTrustedSigningKey swaps trustedSigningKeys for the duration
// of the caller's own test (t.Cleanup restores the real, embedded
// production key set afterward - see trustedSigningKeys' own comment;
// PR #10 review, F4: this comment used to say that set was "currently
// empty", which stopped being true once the real production key was
// embedded), and returns the keypair to sign fixtures with.
func withTestTrustedSigningKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv := newTestSigningKeypair()
	pk, err := minisign.ParsePublicKey([]byte(testMinisignPublicKeyFile(pub)))
	if err != nil {
		t.Fatalf("test setup: parsing the freshly-generated test public key: %v", err)
	}
	orig := trustedSigningKeys
	trustedSigningKeys = []minisign.PublicKey{pk}
	t.Cleanup(func() { trustedSigningKeys = orig })
	return pub, priv
}
