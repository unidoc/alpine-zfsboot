package release

import (
	"fmt"
	neturl "net/url"
	"os"
	"strings"

	"github.com/unidoc/alpine-zfsboot/internal/minisign"
)

// Threat model (unidoc-alip's PR #10 review, F4: several comments in
// this package used to describe a key posture this code doesn't
// implement - this paragraph is the one place that's supposed to be
// accurate, everything else points here instead of re-describing it).
//
// What this protects against: a compromised or impersonated download
// origin or mirror - including an explicit --*-url override, not just
// the default GitHub-release path (issue #2, unidoc-alip's PR #1
// review: "anything able to serve a malicious .EFI can serve a
// matching checksum" - a same-origin SHA256SUMS entry, checked
// separately via verifyChecksum, proves nothing about WHO produced
// the bytes, only that they weren't corrupted in transit after the
// fact).
//
// What this does NOT protect against, today: anyone who can get
// release.yml's own "publish" job to run with ALPINE_ZFSBOOT_PRIVATE_KEY
// in scope. That is narrower than "everyone with GitHub access" - the
// "signing" Environment's own protection rules are what actually
// narrow it (see release.yml's own comment, immediately above the
// `publish` job, for exactly who that is on THIS repo right now, and
// why "an Environment secret" alone is not yet the same claim as "a
// key that does not live on GitHub" issue #2 was originally filed
// against - PR #10 review, F1). --*-file is trusted as-is, no check
// at all - see Source.resolve's own comment for why.
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

// expectedTrustedComment is exactly the -t argument release.yml's own
// signing step passes to `minisign -S` for one asset of one release
// tag (that workflow's own `-t "alpine-zfsboot ${GITHUB_REF_NAME} -
// ${f}"` line - keep the two in lockstep). minisign.Verify returns
// this string as the signature's OWN authenticated trusted comment,
// covered by the global signature and therefore not attacker-
// controllable - checking it here is what actually binds a genuine
// signature to the one asset/release slot it was issued for (PR #10
// review, F2: without this, any genuinely-signed asset verified in
// ANY slot - another arch, another asset, another release - because a
// same-origin SHA256SUMS entry, this package's own threat model
// already assumes forgeable, was otherwise the only thing tying a
// downloaded file to its name and release).
func expectedTrustedComment(tag, assetFile string) string {
	return fmt.Sprintf("alpine-zfsboot %s - %s", tag, assetFile)
}

// verifySignatureAtURL downloads url's own detached minisign signature
// (url's path + ".minisig" - the exact naming convention `minisign -S
// -m <file>` itself produces; release.yml's own signing job publishes
// every asset's signature at exactly this sibling path, nothing
// invented here; appended to the parsed URL's Path rather than the raw
// string so a presigned mirror URL's own query string, e.g.
// `?X-Amz-Signature=...`, isn't corrupted by the suffix - PR #10
// review, F7) and verifies path's real, already-downloaded content
// against it using trustedSigningKeys, returning an error on ANY
// failure: the signature can't be fetched, doesn't parse, matches no
// trusted key, doesn't verify, or (see expectedTrustedComment above)
// was genuinely signed but for a different asset or release than this
// one. The caller owns removing path on error - this function only
// ever reads it.
//
// assetFile is the canonical name being verified (BIOSAssetNames'/
// AssetName's own value for the slot, NOT the URL's basename - keeps a
// renamed mirror working, since the review's own suggested fix checks
// what the asset IS, not what the URL happened to be called). tag is
// the resolved release tag when known (the default-resolved path,
// where resolveDefaultAsset/ResolveBIOS already pinned one tag before
// downloading anything), or "" when it genuinely isn't (an explicit
// --*-url override) - in that case only the asset name is bound, not
// a specific release, since there's no tag to bind it to in the first
// place.
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
func verifySignatureAtURL(url, path, dir, assetFile, tag string) error {
	sigURL := url + ".minisig"
	if u, err := neturl.Parse(url); err == nil && u.RawQuery != "" {
		u.Path += ".minisig"
		sigURL = u.String()
	}
	sigPath, err := downloadAsset(sigURL, dir)
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
	_, comment, err := minisign.Verify(data, sigData, trustedSigningKeys)
	if err != nil {
		return fmt.Errorf("%s: signature verification failed: %w", url, err)
	}
	if tag != "" {
		if want := expectedTrustedComment(tag, assetFile); comment != want {
			return fmt.Errorf("%s: signed as %q, want %q - refusing a signature for a different asset or release", url, comment, want)
		}
	} else if !strings.HasPrefix(comment, "alpine-zfsboot ") || !strings.HasSuffix(comment, " - "+assetFile) {
		return fmt.Errorf("%s: signed as %q, which is not a signature for %s", url, comment, assetFile)
	}
	return nil
}
