// Package minisign verifies detached minisign signatures - and ONLY
// verifies them; there is no signing capability here at all, by
// design (see Verify's own doc comment for why). This closes issue
// #2 (unidoc-alip's PR #1 review, filed separately per the reviewer's
// own recommendation): a downloaded release asset used to be trusted
// on HTTP 200 + a successful parse alone (BIOS) or a same-origin
// SHA256SUMS entry (both firmwares, added later) - "anything able to
// serve a malicious .EFI can serve a matching checksum" is exactly
// the gap this package closes. Every asset internal/release resolves
// by default is now verified against an embedded, trusted Ed25519
// public key before it's ever accepted, fully in-process: no
// dependency on the minisign binary at runtime, no os/exec.
//
// Signing happens in release.yml's own "publish" job, under a
// GitHub Environment ("signing") that scopes the private key away
// from every other job in that workflow - see internal/release/
// signature.go's own doc comment for the exact threat model this
// protects against and the one it deliberately does not (PR #10
// review, F4: this comment used to claim the key never touches CI at
// all, which release.yml's own signing step never implemented). The
// real minisign CLI is the release-side signing tool (and this
// package's own test oracle - every test fixture here is signed with
// the actual upstream binary, not synthesized), never a production
// runtime dependency.
//
// Implements the subset of minisign's real file format this project
// actually needs: Ed25519 detached signatures using the modern
// prehashed algorithm ("ED", BLAKE2b-512 of the message, minisign's
// own default since v0.10 and the only variant this project's release
// process ever produces - the legacy raw "Ed" tag is recognized only
// to name it in an error, never accepted, see Verify's own comment,
// PR #10 review F5), plus the second, "global" signature minisign uses
// to bind the trusted comment to the same signing act (defends against
// a valid old signature being replayed with a substituted comment -
// see Verify's own doc comment). No key GENERATION, no password-
// protected secret-key decryption, no signing - none of that belongs
// in production code that only ever needs to check a signature someone
// else already made.
package minisign

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// keyIDLen/pubKeyLen/sigLen are minisign's own fixed field widths -
// not configurable, not derived, exactly what the real format uses
// (confirmed against real output from the upstream minisign 0.12
// binary, not just documentation - see this package's own test file
// for the fixtures that came from).
const (
	keyIDLen     = 8
	pubKeyRawLen = ed25519.PublicKeySize // 32
	sigRawLen    = ed25519.SignatureSize // 64
)

// algoEd is the legacy, non-prehashed signature algorithm tag (a bare
// Ed25519 signature straight over the message bytes) - minisign's own
// "-l/--legacy" flag. Named here for the wire format's own
// documentation only; Verify never accepts it (PR #10 review, F5 -
// see Verify's own comment on why "Ed" is refused outright rather than
// supported alongside algoED). algoED is the modern default (Ed25519
// over the BLAKE2b-512 digest of the message, not the message itself)
// and the only algorithm this package's Verify ever accepts - what
// every signature this project's own release process actually
// produces.
var (
	algoEd = [2]byte{'E', 'd'}
	algoED = [2]byte{'E', 'D'}
)

// PublicKey is one parsed, trusted minisign public key - KeyID is
// minisign's own 8-byte identifier (NOT a security boundary by
// itself - it's a lookup hint, not a MAC; a signature is only ever
// trusted because Verify's own Ed25519 checks below pass, never
// because a KeyID merely matched), Key is the raw 32-byte Ed25519
// public key.
type PublicKey struct {
	KeyID [keyIDLen]byte
	Key   ed25519.PublicKey
}

// ParsePublicKey parses a minisign public-key file's content (the
// "untrusted comment: ..." line is optional and ignored if present -
// a bare base64 line, the shape embedTrustedKeys below actually
// stores, works equally well) into a PublicKey.
func ParsePublicKey(data []byte) (PublicKey, error) {
	line := lastNonEmptyLine(data)
	raw, err := base64.StdEncoding.DecodeString(line)
	if err != nil {
		return PublicKey{}, fmt.Errorf("minisign public key: not valid base64: %w", err)
	}
	if len(raw) != 2+keyIDLen+pubKeyRawLen {
		return PublicKey{}, fmt.Errorf("minisign public key: decoded to %d bytes, want %d", len(raw), 2+keyIDLen+pubKeyRawLen)
	}
	// Byte 0-1 (the key-type tag) is always "Ed" on a PUBLIC key
	// regardless of which signature algorithm ("Ed" or "ED") it later
	// signs with - that tag lives in the SIGNATURE's own first two
	// bytes instead (see Verify below), not here. Confirmed against a
	// real minisign -G-generated key pair, not assumed from memory of
	// the spec.
	if raw[0] != 'E' || raw[1] != 'd' {
		return PublicKey{}, fmt.Errorf("minisign public key: unrecognized key-type tag %q, want \"Ed\"", raw[0:2])
	}
	var pk PublicKey
	copy(pk.KeyID[:], raw[2:2+keyIDLen])
	pk.Key = ed25519.PublicKey(append([]byte(nil), raw[2+keyIDLen:]...))
	return pk, nil
}

// Verify checks that sigData (the exact, unmodified content of a
// .minisig file) is a valid minisign detached signature for message,
// under ONE of the trusted keys given - fails closed on every error
// path: an unparseable signature, an algorithm this package doesn't
// recognize, a KeyID that matches none of trusted, a message digest
// that doesn't match, or (the part a naive "just check the file
// signature" implementation would miss entirely) a trusted comment
// whose OWN global signature doesn't verify. Returns the matched
// PublicKey and the signature's trusted-comment text on success -
// callers that want to log/display which key/release a download was
// actually signed with have it without re-parsing anything.
//
// Why the global (trusted-comment) signature matters here, not just
// the main one: minisign's own two-signature design exists
// specifically so the trusted comment - which a real release process
// might use to bind a signature to metadata beyond the raw file bytes
// (a version string, a build timestamp) - can't be swapped onto an
// otherwise-still-valid signature by anyone who doesn't hold the
// private key. Confirmed directly against the real minisign binary:
// substituting only the trusted-comment TEXT of an otherwise-genuine
// .minisig file (leaving the main signature line untouched) makes the
// real tool refuse it with "Comment signature verification failed" -
// this package reproduces that same check, not just the main one.
func Verify(message []byte, sigData []byte, trusted []PublicKey) (PublicKey, string, error) {
	sigLine, trustedComment, globalSigB64, err := parseSignatureFile(sigData)
	if err != nil {
		return PublicKey{}, "", err
	}

	sigRaw, err := base64.StdEncoding.DecodeString(sigLine)
	if err != nil {
		return PublicKey{}, "", fmt.Errorf("minisign signature: signature line is not valid base64: %w", err)
	}
	if len(sigRaw) != 2+keyIDLen+sigRawLen {
		return PublicKey{}, "", fmt.Errorf("minisign signature: decoded to %d bytes, want %d", len(sigRaw), 2+keyIDLen+sigRawLen)
	}
	var algo [2]byte
	copy(algo[:], sigRaw[0:2])
	// Only "ED" (prehashed BLAKE2b-512) is accepted - PR #10 review, F5.
	// The algorithm tag comes from the attacker-supplied .minisig, and
	// "Ed" signs the raw MESSAGE bytes directly rather than a fixed-
	// size digest of them: take a genuine "ED" signature for asset A
	// (an Ed25519 signature over BLAKE2b-512(A)), flip its tag to "Ed",
	// and serve the 64-byte digest itself as "the asset" - the main
	// check now runs over exactly those digest bytes and passes, and
	// the global (trusted-comment) signature, which covers sig||comment
	// and neither changed, passes too. This project's own release
	// process only ever produces "ED" (see this package's own doc
	// comment), so accepting "Ed" was attack surface with no legitimate
	// user - exactly upstream minisign's own `-H`/"require prehashed"
	// behavior, confirmed against the real minisign 0.12 binary: it
	// accepts the flipped-tag digest without -H and refuses it with -H.
	if algo != algoED {
		return PublicKey{}, "", fmt.Errorf("minisign signature: algorithm %q not accepted, want \"ED\" (prehashed)", algo[:])
	}
	var keyID [keyIDLen]byte
	copy(keyID[:], sigRaw[2:2+keyIDLen])
	sig := sigRaw[2+keyIDLen:]

	var matched PublicKey
	found := false
	for _, pk := range trusted {
		if pk.KeyID == keyID {
			matched = pk
			found = true
			break
		}
	}
	if !found {
		return PublicKey{}, "", fmt.Errorf("minisign signature: key ID %x is not in the trusted key set", keyID)
	}

	// The signed payload depends on the algorithm: "Ed" signs the
	// message bytes directly, "ED" signs BLAKE2b-512(message) instead
	// - confirmed empirically (real minisign 0.12's own default output
	// decodes to the "ED" tag; -l/--legacy produces "Ed"), not merely
	// asserted from the format spec.
	signedPayload := message
	if algo == algoED {
		digest := blake2b.Sum512(message)
		signedPayload = digest[:]
	}
	if !ed25519.Verify(matched.Key, signedPayload, sig) {
		return PublicKey{}, "", fmt.Errorf("minisign signature: Ed25519 verification failed for key ID %x", keyID)
	}

	// Global signature: covers the bare 64-byte Ed25519 signature
	// (sig - NOT sigRaw; the 2-byte algorithm tag and 8-byte KeyID
	// prefix are excluded) followed immediately by the trusted
	// comment's own raw UTF-8 bytes (the comment TEXT only, not the
	// "trusted comment: " label, and no trailing newline). Determined
	// empirically against real minisign output, not from memory of the
	// spec alone: a brute-force byte-layout check against a real
	// global signature confirmed this exact boundary (sig-without-
	// prefix + comment) is the only one of several plausible
	// candidates - including the full 74-byte sigRaw + comment, which
	// seemed the more obvious guess - that actually verifies.
	globalSig, err := base64.StdEncoding.DecodeString(globalSigB64)
	if err != nil {
		return PublicKey{}, "", fmt.Errorf("minisign signature: global (comment) signature line is not valid base64: %w", err)
	}
	if len(globalSig) != sigRawLen {
		return PublicKey{}, "", fmt.Errorf("minisign signature: global signature is %d bytes, want %d", len(globalSig), sigRawLen)
	}
	signedComment := append(append([]byte(nil), sig...), []byte(trustedComment)...)
	if !ed25519.Verify(matched.Key, signedComment, globalSig) {
		return PublicKey{}, "", fmt.Errorf("minisign signature: trusted-comment (global) signature verification failed for key ID %x - the comment may have been substituted", keyID)
	}

	return matched, trustedComment, nil
}

// parseSignatureFile pulls the three fields Verify needs out of a
// .minisig file's own real four-line shape:
//
//	untrusted comment: <anything, never checked>
//	<base64 sig>
//	trusted comment: <text>
//	<base64 global sig>
//
// The "untrusted comment:"/"trusted comment:" label lines are matched
// by prefix, not by fixed line position - minisign itself allows
// (and its own real output sometimes has) blank lines around them,
// and matching by content rather than position is what every other
// real minisign-format reader does. Returns an error, not a zero
// value, if either base64 line or the "trusted comment:" line is
// missing entirely - a truncated/malformed .minisig must never
// silently verify as "no signature found, carry on".
func parseSignatureFile(data []byte) (sigLine, trustedComment, globalSigLine string, err error) {
	const trustedPrefix = "trusted comment: "
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 4096), 1<<20)

	var lines []string
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	if err := sc.Err(); err != nil {
		return "", "", "", fmt.Errorf("minisign signature: reading signature data: %w", err)
	}

	sigIdx := -1
	trustedIdx := -1
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "untrusted comment:"):
			sigIdx = i + 1
		case strings.HasPrefix(line, trustedPrefix):
			trustedComment = strings.TrimPrefix(line, trustedPrefix)
			trustedIdx = i
		}
	}
	if sigIdx < 0 || sigIdx >= len(lines) {
		return "", "", "", fmt.Errorf("minisign signature: no \"untrusted comment:\" line (or no signature line after it) found")
	}
	if trustedIdx < 0 || trustedIdx+1 >= len(lines) {
		return "", "", "", fmt.Errorf("minisign signature: no \"trusted comment:\" line (or no global-signature line after it) found")
	}
	return lines[sigIdx], trustedComment, lines[trustedIdx+1], nil
}

// lastNonEmptyLine returns the last non-empty, non-comment-prefixed
// line of data - used for a public key file, whose ONE real payload
// line (the base64-encoded key) is whatever line isn't the leading
// "untrusted comment: ..." line. Works equally on a bare single-line
// base64 string (no comment line at all) and on the real two-line
// file minisign -G actually writes.
func lastNonEmptyLine(data []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(data))
	var last string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "untrusted comment:") {
			continue
		}
		last = line
	}
	return last
}
