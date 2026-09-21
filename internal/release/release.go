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

// Download fetches the latest asset for arch into a new temp file
// inside dir and returns its path - dir matters when the caller
// intends to os.Rename() the result onto a real target afterward
// (same directory means same filesystem, so that rename is atomic);
// pass "" to use the system temp directory for a check-only caller
// that will never rename it anywhere. The caller owns cleanup - this
// never removes the file itself, including on a caller-side failure
// after a successful download.
func Download(arch, dir string) (string, error) {
	url := baseURL + AssetName(arch)
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
