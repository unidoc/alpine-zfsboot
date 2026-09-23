// Package release finds and downloads the latest alpine-zfsboot .EFI
// build for a given arch, straight off GitHub's releases/latest/
// download/ URL scheme (see .github/workflows/release.yml's own
// comment: asset filenames are arch-based and unversioned, so this
// URL never needs to change across releases) - no GitHub API call,
// no auth, no rate limit, needed at all for this.
package release

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// var, not const - overridden by TestDownloadRespectsClientTimeout to
// point at a local httptest.Server, so that test can exercise the real
// client-timeout behavior against a server that genuinely stalls,
// without touching the network.
var baseURL = "https://github.com/unidoc/alpine-zfsboot/releases/latest/download/"

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
// downloadAsset's own doc comment for the dir/cleanup contract.
func Download(arch, dir string) (string, error) {
	return downloadAsset(baseURL+AssetName(arch), dir)
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
func (s Source) resolve(defaultURL, dir string) (string, error) {
	if s.File != "" {
		return copyToTemp(s.File, dir)
	}
	url := s.URL
	if url == "" {
		url = defaultURL
	}
	return downloadAsset(url, dir)
}

// ResolveEFI returns a temp-file path for arch's UEFI .EFI artifact,
// sourced per src (see Source's own doc comment), defaulting to the
// latest GitHub release for that arch.
func ResolveEFI(src Source, arch, dir string) (string, error) {
	return src.resolve(baseURL+AssetName(arch), dir)
}

// BIOSSources bundles one independently-resolved Source per BIOS
// artifact - matches alpine-install-zfs.sh's own per-artifact
// ALPINE_ZFSBOOT_BIOS_*_FILE/_URL override pattern: any mix of local
// files, explicit URLs, and "use the default" is valid across the
// five fields at once.
type BIOSSources struct {
	Stage1, Stage2, Kernel, Initrd, Cmdline Source
}

// ResolveBIOS resolves all five BIOS artifacts per src, each
// independently sourced, defaulting whichever aren't overridden to
// the latest GitHub release for arch. On any single artifact's
// failure, every artifact successfully resolved so far is cleaned up
// - same all-or-nothing contract as DownloadBIOS.
func ResolveBIOS(src BIOSSources, arch, dir string) (BIOSAssets, error) {
	names := BIOSAssetName(arch)
	var assets BIOSAssets
	for _, f := range []struct {
		name       string
		source     Source
		defaultURL string
		dst        *string
	}{
		{"stage1", src.Stage1, baseURL + names.Stage1, &assets.Stage1},
		{"stage2", src.Stage2, baseURL + names.Stage2, &assets.Stage2},
		{"kernel", src.Kernel, baseURL + names.Kernel, &assets.Kernel},
		{"initrd", src.Initrd, baseURL + names.Initrd, &assets.Initrd},
		{"cmdline", src.Cmdline, baseURL + names.Cmdline, &assets.Cmdline},
	} {
		path, err := f.source.resolve(f.defaultURL, dir)
		if err != nil {
			assets.RemoveAll()
			return BIOSAssets{}, fmt.Errorf("resolving %s: %w", f.name, err)
		}
		*f.dst = path
	}
	return assets, nil
}

// DownloadBIOS fetches all five latest BIOS artifacts for arch into
// new temp files inside dir - see downloadAsset's own doc comment for
// the dir/cleanup contract, which applies to each file individually.
// On any failure, every file successfully downloaded so far is
// removed before returning, so a partial-failure caller never has to
// remember to clean up a partially-filled BIOSAssets itself.
func DownloadBIOS(arch, dir string) (BIOSAssets, error) {
	names := BIOSAssetName(arch)
	var assets BIOSAssets
	for _, f := range []struct {
		name string
		dst  *string
	}{
		{names.Stage1, &assets.Stage1},
		{names.Stage2, &assets.Stage2},
		{names.Kernel, &assets.Kernel},
		{names.Initrd, &assets.Initrd},
		{names.Cmdline, &assets.Cmdline},
	} {
		path, err := downloadAsset(baseURL+f.name, dir)
		if err != nil {
			assets.RemoveAll()
			return BIOSAssets{}, err
		}
		*f.dst = path
	}
	return assets, nil
}
