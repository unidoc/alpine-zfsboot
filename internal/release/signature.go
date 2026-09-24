package release

import (
	"fmt"
	"os"

	"github.com/unidoc/alpine-zfsboot/internal/minisign"
)

// trustedSigningKeys is the small, rotatable set of minisign public
// keys this package trusts to have signed a downloaded release asset
// (issue #2, unidoc-alip's PR #1 review: "anything able to serve a
// malicious .EFI can serve a matching checksum" - a same-origin
// SHA256SUMS entry, already checked separately via verifyChecksum,
// proves nothing about WHO produced the bytes, only that they weren't
// corrupted in transit after the fact. A detached Ed25519 signature
// from a key release.yml's own signing job holds ONLY as a protected
// GitHub Environment secret - never a plain repository secret, never
// committed, never visible to an ordinary build/PR job - is the
// actual answer. See release.yml's own comment for the exact
// build-job/signing-job separation this enables).
//
// A slice, not one hardcoded key, specifically so a future signing-
// key rotation is additive: list the new key ALONGSIDE the old one
// for as long as any release anyone might still be updating FROM was
// signed with the old one, then remove the old one once it's no
// longer needed - never a single in-place swap, which would make
// every already-published older release's own signature suddenly
// unverifiable the moment a rotation lands, even though nothing about
// those bytes changed.
var trustedSigningKeys = mustParseTrustedKeys(
	trustedKey{
		Comment: "2026 production key - minisign public key 947F6C8225F28558",
		PublicKey: `untrusted comment: minisign public key 947F6C8225F28558
RWRYhfIlgmx/lNEckpNnD/XZl8FjxPfsHqwd3aeEwER3Yb/Cw4ZRLF8g
`,
	},
)

// trustedKey names one embedded public key for mustParseTrustedKeys -
// Comment is purely descriptive (when/why this key was generated,
// copied from minisign -G's own "untrusted comment" line), never
// parsed or trusted for anything; PublicKey is the real minisign
// public-key file content.
type trustedKey struct {
	Comment   string
	PublicKey string
}

// mustParseTrustedKeys parses every embedded trustedKey into a
// minisign.PublicKey - panics on a malformed embedded key (a build-
// time programmer error, e.g. a pasted-in-wrong public key file - not
// something any real input this package processes at runtime could
// ever trigger, and not something to discover only when the first
// real verification attempt mysteriously fails).
func mustParseTrustedKeys(keys ...trustedKey) []minisign.PublicKey {
	parsed := make([]minisign.PublicKey, 0, len(keys))
	for _, k := range keys {
		pk, err := minisign.ParsePublicKey([]byte(k.PublicKey))
		if err != nil {
			panic(fmt.Sprintf("release: embedded trusted signing key (%s) does not parse: %v", k.Comment, err))
		}
		parsed = append(parsed, pk)
	}
	return parsed
}

// verifySignatureAtURL downloads url's own detached minisign signature
// (url + ".minisig" - the exact naming convention `minisign -S -m
// <file>` itself produces; release.yml's own signing job publishes
// every asset's signature at exactly this sibling path, nothing
// invented here) and verifies path's real, already-downloaded content
// against it using trustedSigningKeys, returning an error on ANY
// failure: the signature can't be fetched, doesn't parse, matches no
// trusted key, or doesn't verify. The caller owns removing path on
// error - this function only ever reads it.
//
// Runs for EVERY url-sourced fetch, not only the resolved-default
// release: an explicit --*-url override (Source.resolve's own URL
// branch) reaches this exact same function, deliberately. A remote
// fetch of an executable boot artifact gets the real cryptographic
// check every time a URL is involved, full stop - "an operator typed
// a custom URL" is not, on its own, a reason to skip verifying WHAT
// actually came back over the wire; an operator wanting to point at
// their own internal mirror only has to also serve that mirror's own
// <file>.minisig (a straight copy of the one the real release
// publishes - the trusted key doesn't care which URL a byte-identical
// file was fetched from). --*-file is the one deliberate exception -
// see Source.resolve's own comment for why a local path already in
// hand gets no such check.
//
// Fails closed even if trustedSigningKeys were ever EMPTY (a future
// key-rotation mistake, an accidental revert) - deliberately, not an
// oversight: minisign.Verify() itself already refuses to match
// against zero keys, so this needs no special-casing to get that
// property, but it's worth being explicit that "no trusted keys"
// must mean "refuse every URL-sourced download" for a bootloader,
// never "skip this check for now" - the two look identical from a
// green `alpine-zfsboot update` run right up until the moment they
// don't.
func verifySignatureAtURL(url, path, dir string) error {
	sigPath, err := downloadAsset(url+".minisig", dir)
	if err != nil {
		return fmt.Errorf("fetching %s's detached signature: %w", url, err)
	}
	defer os.Remove(sigPath)

	sigData, err := os.ReadFile(sigPath)
	if err != nil {
		return fmt.Errorf("reading the downloaded signature for %s: %w", url, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s to verify its signature: %w", url, err)
	}
	if _, _, err := minisign.Verify(data, sigData, trustedSigningKeys); err != nil {
		return fmt.Errorf("%s: signature verification failed: %w", url, err)
	}
	return nil
}
