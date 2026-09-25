// Package release finds and downloads alpine-zfsboot's own release
// assets (the UEFI .EFI build, and BIOS's five loose stage1/stage2/
// kernel/initrd/cmdline artifacts) for a given arch.
//
// F16 (unidoc-alip's PR #5 follow-up review): this package's own doc
// comment used to say "no GitHub API call, no auth, no rate limit,
// needed at all for this" - true of the ORIGINAL bare
// releases/latest/download/ scheme (asset filenames are arch-based
// and unversioned - see .github/workflows/release.yml's own comment -
// so that URL never needed to change across releases), but stale
// since the F16 fix that pinned every default-sourced asset (BIOS's
// five, and now UEFI's one, via resolveDefaultAsset/ResolveEFI) to one
// concrete, resolved release tag: resolveTag() below DOES call
// GitHub's real REST API (apiLatestReleaseURL), unauthenticated, once
// per resolution. An explicit --*-file/--*-url override still never
// touches the API at all - only the "use the latest release" default
// path does.
package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// http.DefaultClient has no timeout at all - a server that accepts the
// connection and then stalls (not a DNS/connect failure, which would
// already return an error) leaves check/update hanging indefinitely
// with no output. check in particular is the subcommand most likely to
// run unattended (cron, fleet management), where a hung process is
// worse than a failed one. Generous on purpose - this downloads a real
// multi-MB .EFI file, not a small API response.
var httpClient = &http.Client{Timeout: 5 * time.Minute}

// AssetName is the exact filename build.sh's own OUT_FILE produces
// for a given arch (see build.sh's own "OUT_FILE=..." line) - one
// build per arch now, always both consoles active (see build.sh's own
// CONSOLE_CMDLINE comment for why the separate vga/serial/auto
// variants this used to need a console component for were dropped).
func AssetName(arch string) string {
	return fmt.Sprintf("alpine-zfsboot-%s.EFI", arch)
}

// BIOSAssetNames is the exact five loose-file asset names build.sh
// produces for arch's BIOS artifacts - matches
// alpine-install-zfs.sh's own env-var-documented URL scheme
// (ALPINE_ZFSBOOT_BIOS_STAGE1_URL's own default, etc.) field-for-field,
// so the SAME GitHub release assets serve both the shell installer
// and this tool.
type BIOSAssetNames struct {
	Stage1, Stage2, Kernel, Initrd, Cmdline string
}

func BIOSAssetName(arch string) BIOSAssetNames {
	return BIOSAssetNames{
		Stage1:  fmt.Sprintf("alpine-zfsboot-%s-bios-stage1.bin", arch),
		Stage2:  fmt.Sprintf("alpine-zfsboot-%s-bios-stage2.bin", arch),
		Kernel:  fmt.Sprintf("alpine-zfsboot-%s-vmlinuz", arch),
		Initrd:  fmt.Sprintf("alpine-zfsboot-%s-initramfs.img", arch),
		Cmdline: fmt.Sprintf("alpine-zfsboot-%s-cmdline.txt", arch),
	}
}

// downloadAsset fetches url into a new temp file inside dir and
// returns its path - the one real-I/O core both Download and
// DownloadBIOS below share. dir matters when the caller intends to
// os.Rename() the result onto a real target afterward (same directory
// means same filesystem, so that rename is atomic); pass "" to use the
// system temp directory for a check-only caller that will never rename
// it anywhere. The caller owns cleanup - this never removes the file
// itself, including on a caller-side failure after a successful
// download.
func downloadAsset(url, dir string) (string, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching %s: HTTP %s", url, resp.Status)
	}

	tmp, err := os.CreateTemp(dir, ".alpine-zfsboot-update-*")
	if err != nil {
		return "", fmt.Errorf("creating a temp file for the download: %w", err)
	}
	defer tmp.Close()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("downloading %s: %w", url, err)
	}
	return tmp.Name(), nil
}

// Download fetches the latest UEFI .EFI asset for arch - see
// downloadAsset's own doc comment for the dir/cleanup contract. A thin
// wrapper around ResolveEFI's own default (no override) path - see
// that function's own comment for why this goes through a tag-pinned,
// checksum-verified fetch (F16, unidoc-alip's PR #5 follow-up review)
// rather than a bare baseURL request.
func Download(arch, dir string) (string, error) {
	return ResolveEFI(Source{}, arch, dir)
}

// BIOSAssets holds temp file paths for all five downloaded BIOS
// artifacts - the caller owns cleanup of every path here, same
// contract as Download's own single path.
type BIOSAssets struct {
	Stage1, Stage2, Kernel, Initrd, Cmdline string
}

// RemoveAll cleans up every temp file in a, ignoring errors (best
// effort, mirrors how callers already treat a single Download()
// result's cleanup) - convenience for the common "fetched, done with
// them" path.
func (a BIOSAssets) RemoveAll() {
	for _, p := range []string{a.Stage1, a.Stage2, a.Kernel, a.Initrd, a.Cmdline} {
		if p != "" {
			os.Remove(p)
		}
	}
}

func copyToTemp(src, dir string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("opening local file %s: %w", src, err)
	}
	defer in.Close()

	tmp, err := os.CreateTemp(dir, ".alpine-zfsboot-update-*")
	if err != nil {
		return "", fmt.Errorf("creating a temp file for %s: %w", src, err)
	}
	defer tmp.Close()

	if _, err := io.Copy(tmp, in); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("copying %s: %w", src, err)
	}
	return tmp.Name(), nil
}

// Source describes where ONE artifact should come from - at most one
// of File/URL is normally set; File wins if both are (matching
// alpine-install-zfs.sh's own fetch_one()'s precedence exactly: a
// local file always wins over any URL). Both empty means "use
// whatever default the caller supplies" - normally the latest GitHub
// release asset, so a bare `alpine-zfsboot update` with no flags at
// all does the sensible thing.
type Source struct {
	File string
	URL  string
}

// resolve returns a temp-file path with this Source's own bytes,
// always a fresh file in dir regardless of which source it came from
// - a local-file source and a downloaded one are indistinguishable to
// every caller downstream, same cleanup contract either way.
//
// Security contract, made explicit per a real review question (does
// an explicit --*-url really deserve to bypass signature verification
// just because it's explicit?): File and URL are NOT treated the same
// here, deliberately.
//
//   - File: copied as-is, no signature check, no network fetch of any
//     kind. An explicit local path is an operator's OWN bytes already
//     in hand (their own build, a copy from wherever they already
//     trust it) - there is no URL to fetch a signature FROM, and
//     inventing a "look for a sibling .minisig next to the local file
//     too" convention here would silently change what --*-file has
//     always meant (use exactly this file, no questions asked) into
//     something conditionally stricter depending on what else happens
//     to sit in the same directory. If that trust model is ever wrong
//     for a given deployment, the fix is not using --*-file at all.
//   - URL, whether an explicit override OR the caller's own resolved
//     default: downloaded, THEN signature-verified via
//     verifySignatureAtURL against trustedSigningKeys, unconditionally.
//     "An operator typed a custom URL" is not a reason to skip
//     verifying what actually came back over the wire - a remote fetch
//     of an executable boot artifact gets the real cryptographic check
//     every single time a network round-trip is involved, full stop.
//     An operator who wants to point at their own internal mirror only
//     has to ALSO serve that mirror's own <file>.minisig (a straight
//     copy of the one the real release publishes - the trusted key
//     doesn't care which URL byte-identical content was fetched from).
//
// Only the CALLER-supplied defaultURL path (the tag-pinned resolved
// release, reached when neither File nor URL is set) skips the
// signature check HERE - not because it's exempt, but because
// resolveDefaultAsset/ResolveBIOS's own loop already run BOTH the
// checksum and signature checks themselves, using their own already-
// resolved tag/SHA256SUMS context; verifying again here would just be
// a second, redundant network fetch of the same .minisig.
// assetFile is the canonical asset name for this slot (AssetName's or
// BIOSAssetNames' own value) - only used to bind a URL-sourced
// signature's own trusted comment to the right slot (PR #10 review,
// F2); unused by the File and no-override-given branches.
func (s Source) resolve(defaultURL, assetFile, dir string) (string, error) {
	if s.File != "" {
		return copyToTemp(s.File, dir)
	}
	if s.URL != "" {
		path, err := downloadAsset(s.URL, dir)
		if err != nil {
			return "", err
		}
		// tag is unavoidably "" here: an explicit --*-url override has
		// no resolved release tag to bind against, only the asset name
		// it claims to be (see verifySignatureAtURL's own doc comment).
		if err := verifySignatureAtURL(s.URL, path, dir, assetFile, ""); err != nil {
			os.Remove(path)
			return "", err
		}
		return path, nil
	}
	return downloadAsset(defaultURL, dir)
}

// ResolveEFI returns a temp-file path for arch's UEFI .EFI artifact,
// sourced per src (see Source's own doc comment), defaulting to the
// latest GitHub release for that arch.
//
// F16 (unidoc-alip's PR #5 follow-up review): the default (no
// File/URL override) case now goes through the SAME tag-pinned,
// checksum-verified path ResolveBIOS's own default fields already use
// (resolveDefaultAsset, below) - it used to fetch straight from
// baseURL (releases/latest/download/), unpinned and unverified, the
// exact class of gap the original F16 fix closed for BIOS but never
// carried over to UEFI. An explicit --efi-file/--efi-url override
// still skips CHECKSUM verification (it was never claiming to BE the
// official release asset, so there's no SHA256SUMS entry legitimate
// to check it against) - but --efi-url, unlike --efi-file, DOES still
// get real SIGNATURE verification, via Source.resolve's own URL
// branch. See that function's own doc comment for the full reasoning
// on why File and URL are treated differently here.
func ResolveEFI(src Source, arch, dir string) (string, error) {
	assetFile := AssetName(arch)
	if src.File != "" || src.URL != "" {
		return src.resolve("", assetFile, dir)
	}
	path, err := resolveDefaultAsset(assetFile, dir)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", assetFile, err)
	}
	return path, nil
}

// BIOSSources bundles one independently-resolved Source per BIOS
// artifact - matches alpine-install-zfs.sh's own per-artifact
// ALPINE_ZFSBOOT_BIOS_*_FILE/_URL override pattern: any mix of local
// files, explicit URLs, and "use the default" is valid across the
// five fields at once.
type BIOSSources struct {
	Stage1, Stage2, Kernel, Initrd, Cmdline Source
}

// apiLatestReleaseURL is GitHub's own REST API for this repo's latest
// release - var, not const, so tests can point it at a local
// httptest.Server instead of the real network.
var apiLatestReleaseURL = "https://api.github.com/repos/unidoc/alpine-zfsboot/releases/latest"

// downloadBaseURLTemplate is a CONCRETE, tag-pinned download base
// (releases/download/<tag>/, not releases/latest/download/ - one %s
// for the tag) - var, same test-injection reason as apiLatestReleaseURL.
var downloadBaseURLTemplate = "https://github.com/unidoc/alpine-zfsboot/releases/download/%s/"

var sha256HexPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// resolveTag resolves GitHub's "latest" release to ONE concrete,
// immutable tag name - part of the fix for F16 (unidoc-alip's PR #5
// review): ResolveBIOS used to fetch its five assets from five
// independent releases/latest/download/ requests, each one its own,
// separate "whatever /latest/ means AT THAT EXACT MOMENT" resolution.
// If a new release was published between any two of those five
// requests, the five downloaded files could come from two DIFFERENT
// releases, mixed together with nothing to notice - a stage2 build ID
// from vN alongside a kernel/initrd from vN+1. Resolving one tag ONCE,
// before downloading anything, and building every URL from THAT SAME
// tag closes the window: all five (or none) come from one real,
// specific, immutable release.
func resolveTag() (string, error) {
	resp, err := httpClient.Get(apiLatestReleaseURL)
	if err != nil {
		return "", fmt.Errorf("resolving the latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolving the latest release: HTTP %s", resp.Status)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("parsing the latest-release API response: %w", err)
	}
	if body.TagName == "" {
		return "", fmt.Errorf("the latest-release API response had no tag_name")
	}
	return body.TagName, nil
}

// fetchChecksums downloads and parses tagBase's own SHA256SUMS asset
// (a plain `sha256sum` output file - see release.yml's own
// "sha256sum * | tee SHA256SUMS") into a filename -> lowercase-hex-
// sha256 map - the other half of the F16 fix: nothing previously
// verified a downloaded BIOS asset's own integrity at all.
func fetchChecksums(tagBase string) (map[string]string, error) {
	resp, err := httpClient.Get(tagBase + "SHA256SUMS")
	if err != nil {
		return nil, fmt.Errorf("fetching SHA256SUMS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching SHA256SUMS: HTTP %s", resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading SHA256SUMS: %w", err)
	}
	sums := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("SHA256SUMS: malformed line %q", line)
		}
		if !sha256HexPattern.MatchString(fields[0]) {
			return nil, fmt.Errorf("SHA256SUMS: %q is not a 64-character hex sha256 sum", fields[0])
		}
		// sha256sum's own binary-mode "*" prefix on the filename field
		// (see release.yml's own "sha256sum * | tee SHA256SUMS") -
		// stripped so lookups match the plain asset name.
		name := strings.TrimPrefix(fields[1], "*")
		sums[name] = strings.ToLower(fields[0])
	}
	return sums, nil
}

// verifyChecksum confirms path's own real, on-disk content hashes to
// sums[name] - fail closed: an asset missing from SHA256SUMS entirely
// is refused, not silently trusted.
func verifyChecksum(name, path string, sums map[string]string) error {
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("%s is not listed in SHA256SUMS at all - refusing to trust it", name)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s to verify its checksum: %w", name, err)
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if got != want {
		return fmt.Errorf("%s: SHA256 mismatch (downloaded %s, SHA256SUMS says %s) - possible corruption or tampering in transit", name, got, want)
	}
	return nil
}

// resolveDefaultAsset fetches assetFile from the current tag-pinned,
// checksum-verified GitHub release - the shared "no override given,
// fetch the real thing safely" primitive ResolveEFI's own default case
// uses directly (one asset, so one resolveTag/fetchChecksums pair is
// exactly right). ResolveBIOS does NOT call this: it resolves the tag
// and fetches checksums ONCE, up front, shared across all five of its
// own fields in its own loop - calling this per-field there would
// re-issue five independent resolveTag/fetchChecksums round trips and
// reintroduce the exact same-release-consistency race the original
// F16 fix closed, just one level down.
func resolveDefaultAsset(assetFile, dir string) (string, error) {
	tag, err := resolveTag()
	if err != nil {
		return "", err
	}
	tagBase := fmt.Sprintf(downloadBaseURLTemplate, tag)
	sums, err := fetchChecksums(tagBase)
	if err != nil {
		return "", err
	}
	path, err := downloadAsset(tagBase+assetFile, dir)
	if err != nil {
		return "", err
	}
	if err := verifyChecksum(assetFile, path, sums); err != nil {
		os.Remove(path)
		return "", err
	}
	if err := verifySignatureAtURL(tagBase+assetFile, path, dir, assetFile, tag); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// ResolveBIOS resolves all five BIOS artifacts per src, each
// independently sourced, defaulting whichever aren't overridden to
// the latest GitHub release for arch. On any single artifact's
// failure, every artifact successfully resolved so far is cleaned up
// - same all-or-nothing contract as DownloadBIOS.
//
// F16 (unidoc-alip's PR #5 review): whichever of the five fall back to
// the default (no explicit File/URL override) now come from ONE
// concrete, tag-pinned release - resolved once, before any asset is
// downloaded - and each one is checksum-verified against that SAME
// release's own SHA256SUMS. An explicitly-overridden source (a custom
// local file or URL) is NOT checksum-checked against the official
// release's sums - it was never claiming to BE that release's asset
// in the first place, so there's nothing legitimate to verify it
// against. SIGNATURE verification is a separate question, handled
// uniformly by Source.resolve itself (see that function's own doc
// comment): a --*-url override still gets it, a --*-file override
// still doesn't.
func ResolveBIOS(src BIOSSources, arch, dir string) (BIOSAssets, error) {
	names := BIOSAssetName(arch)
	fields := []struct {
		name      string
		assetFile string
		source    Source
		dst       *string
	}{
		{"stage1", names.Stage1, src.Stage1, nil},
		{"stage2", names.Stage2, src.Stage2, nil},
		{"kernel", names.Kernel, src.Kernel, nil},
		{"initrd", names.Initrd, src.Initrd, nil},
		{"cmdline", names.Cmdline, src.Cmdline, nil},
	}

	needDefault := false
	for _, f := range fields {
		if f.source.File == "" && f.source.URL == "" {
			needDefault = true
		}
	}

	var sums map[string]string
	var tagBase, tag string
	if needDefault {
		var err error
		tag, err = resolveTag()
		if err != nil {
			return BIOSAssets{}, fmt.Errorf("resolving BIOS assets: %w", err)
		}
		tagBase = fmt.Sprintf(downloadBaseURLTemplate, tag)
		sums, err = fetchChecksums(tagBase)
		if err != nil {
			return BIOSAssets{}, fmt.Errorf("resolving BIOS assets: %w", err)
		}
	}

	var assets BIOSAssets
	dsts := []*string{&assets.Stage1, &assets.Stage2, &assets.Kernel, &assets.Initrd, &assets.Cmdline}
	for i := range fields {
		fields[i].dst = dsts[i]
	}
	for _, f := range fields {
		isDefault := f.source.File == "" && f.source.URL == ""
		defaultURL := ""
		if isDefault {
			defaultURL = tagBase + f.assetFile
		}
		path, err := f.source.resolve(defaultURL, f.assetFile, dir)
		if err != nil {
			assets.RemoveAll()
			return BIOSAssets{}, fmt.Errorf("resolving %s: %w", f.name, err)
		}
		if isDefault {
			if err := verifyChecksum(f.assetFile, path, sums); err != nil {
				os.Remove(path)
				assets.RemoveAll()
				return BIOSAssets{}, fmt.Errorf("resolving %s: %w", f.name, err)
			}
			if err := verifySignatureAtURL(tagBase+f.assetFile, path, dir, f.assetFile, tag); err != nil {
				os.Remove(path)
				assets.RemoveAll()
				return BIOSAssets{}, fmt.Errorf("resolving %s: %w", f.name, err)
			}
		}
		*f.dst = path
	}
	return assets, nil
}
