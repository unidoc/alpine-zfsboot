package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// F3 from unidoc-alip's review of PR #1: Download() used
// http.DefaultClient, which has no timeout at all - a server that
// accepts the connection and then stalls left check/update hanging
// indefinitely. Fixed with an explicit client Timeout; this test
// confirms that timeout is actually applied against a real server that
// genuinely never responds, not just that the client value is set.
//
// The reviewer's original harness swapped the transport for one that
// stalls but must itself honor req.Context() (http.Client.Timeout is
// implemented by cancelling the request context - a RoundTripper that
// ignores it blocks regardless, which is exactly the bug the reviewer
// found in their own first draft of that harness). Using a real
// httptest.Server instead sidesteps that entirely: its connection goes
// through the real net/http transport, which does honor context
// cancellation correctly, so this test only needs to swap where
// Download() points and how long it's willing to wait.
func TestDownloadRespectsClientTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // accepts the connection, then never responds - the actual failure mode this guards against
	}))
	// Deferred LIFO, deliberately in this order: close(block) must run
	// BEFORE srv.Close(), or Close() blocks forever waiting for the
	// still-parked handler goroutine to return - it has no idea the
	// client already gave up via its own timeout.
	defer srv.Close()
	defer close(block)

	origClient, origBase := httpClient, baseURL
	httpClient = &http.Client{Timeout: 100 * time.Millisecond}
	baseURL = srv.URL + "/"
	defer func() { httpClient, baseURL = origClient, origBase }()

	start := time.Now()
	_, err := Download("x86_64", t.TempDir())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Download succeeded against a server that never responds - the client timeout isn't being applied")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Download took %s to fail - looks like it hung rather than respecting the client timeout", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected an error wrapping context.DeadlineExceeded (what http.Client.Timeout produces), got: %v", err)
	}
}

func TestBIOSAssetName(t *testing.T) {
	got := BIOSAssetName("x86_64")
	want := BIOSAssetNames{
		Stage1:  "alpine-zfsboot-x86_64-bios-stage1.bin",
		Stage2:  "alpine-zfsboot-x86_64-bios-stage2.bin",
		Kernel:  "alpine-zfsboot-x86_64-vmlinuz",
		Initrd:  "alpine-zfsboot-x86_64-initramfs.img",
		Cmdline: "alpine-zfsboot-x86_64-cmdline.txt",
	}
	if got != want {
		t.Errorf("BIOSAssetName(x86_64) = %+v, want %+v", got, want)
	}
}

func TestDownloadBIOS(t *testing.T) {
	content := map[string]string{
		"alpine-zfsboot-x86_64-bios-stage1.bin": "stage1-bytes",
		"alpine-zfsboot-x86_64-bios-stage2.bin": "stage2-bytes",
		"alpine-zfsboot-x86_64-vmlinuz":         "kernel-bytes",
		"alpine-zfsboot-x86_64-initramfs.img":   "initrd-bytes",
		"alpine-zfsboot-x86_64-cmdline.txt":     "cmdline-bytes",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[1:]
		body, ok := content[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()

	origBase := baseURL
	baseURL = srv.URL + "/"
	defer func() { baseURL = origBase }()

	dir := t.TempDir()
	assets, err := DownloadBIOS("x86_64", dir)
	if err != nil {
		t.Fatalf("DownloadBIOS: %v", err)
	}
	defer assets.RemoveAll()

	check := func(path, want string) {
		t.Helper()
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if string(got) != want {
			t.Errorf("%s content = %q, want %q", path, got, want)
		}
	}
	check(assets.Stage1, "stage1-bytes")
	check(assets.Stage2, "stage2-bytes")
	check(assets.Kernel, "kernel-bytes")
	check(assets.Initrd, "initrd-bytes")
	check(assets.Cmdline, "cmdline-bytes")
}

func TestDownloadBIOS_CleansUpOnPartialFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only stage1 succeeds - every other asset 404s, simulating a
		// partial/broken release.
		if r.URL.Path == "/alpine-zfsboot-x86_64-bios-stage1.bin" {
			w.Write([]byte("stage1-bytes"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	origBase := baseURL
	baseURL = srv.URL + "/"
	defer func() { baseURL = origBase }()

	dir := t.TempDir()
	_, err := DownloadBIOS("x86_64", dir)
	if err == nil {
		t.Fatal("DownloadBIOS with a partially-broken release: want an error, got nil")
	}

	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		t.Errorf("temp dir has %d leftover file(s) after a partial-failure cleanup, want 0: %v", len(entries), entries)
	}
}

func TestSourceResolve_LocalFileWinsOverURL(t *testing.T) {
	dir := t.TempDir()
	localPath := dir + "/local.bin"
	if err := os.WriteFile(localPath, []byte("local-content"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := Source{File: localPath, URL: "http://should-not-be-fetched.invalid/asset"}
	got, err := src.resolve("http://also-should-not-be-fetched.invalid/asset", t.TempDir())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	content, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "local-content" {
		t.Errorf("content = %q, want %q", content, "local-content")
	}
}

func TestSourceResolve_ExplicitURLWinsOverDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/explicit" {
			w.Write([]byte("explicit-content"))
			return
		}
		w.Write([]byte("default-content"))
	}))
	defer srv.Close()

	src := Source{URL: srv.URL + "/explicit"}
	got, err := src.resolve(srv.URL+"/default", t.TempDir())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	content, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "explicit-content" {
		t.Errorf("content = %q, want %q", content, "explicit-content")
	}
}

func TestSourceResolve_FallsBackToDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("default-content"))
	}))
	defer srv.Close()

	src := Source{} // nothing overridden at all
	got, err := src.resolve(srv.URL+"/default", t.TempDir())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	content, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "default-content" {
		t.Errorf("content = %q, want %q", content, "default-content")
	}
}

// newFakeReleaseServer serves a fake "latest release" - a tag-
// resolution API response, a real SHA256SUMS computed from
// assetContent, and each named asset's own content - everything
// ResolveBIOS's own default (non-overridden) path now needs (F16,
// unidoc-alip's PR #5 review). Returns the base URL to point
// apiLatestReleaseURL/downloadBaseURLTemplate at.
func newFakeReleaseServer(t *testing.T, tag string, assetContent map[string]string) *httptest.Server {
	t.Helper()
	var sums strings.Builder
	for name, content := range assetContent {
		sum := sha256.Sum256([]byte(content))
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q}`, tag)
	})
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[len("/download/"+tag+"/"):]
		if name == "SHA256SUMS" {
			w.Write([]byte(sums.String()))
			return
		}
		content, ok := assetContent[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(content))
	})
	return httptest.NewServer(mux)
}

func TestResolveBIOS_MixedSources(t *testing.T) {
	dir := t.TempDir()
	localStage1 := dir + "/my-stage1.bin"
	if err := os.WriteFile(localStage1, []byte("local-stage1"), 0o644); err != nil {
		t.Fatal(err)
	}

	explicitSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("from-" + r.URL.Path[1:]))
	}))
	defer explicitSrv.Close()

	assetContent := map[string]string{
		"alpine-zfsboot-x86_64-vmlinuz":       "real-kernel-bytes",
		"alpine-zfsboot-x86_64-initramfs.img": "real-initrd-bytes",
		"alpine-zfsboot-x86_64-cmdline.txt":   "real-cmdline-bytes",
	}
	fakeRelease := newFakeReleaseServer(t, "v9.9.9", assetContent)
	defer fakeRelease.Close()

	origAPI, origDownload := apiLatestReleaseURL, downloadBaseURLTemplate
	apiLatestReleaseURL = fakeRelease.URL + "/api/latest"
	downloadBaseURLTemplate = fakeRelease.URL + "/download/%s/"
	defer func() { apiLatestReleaseURL, downloadBaseURLTemplate = origAPI, origDownload }()

	src := BIOSSources{
		Stage1: Source{File: localStage1},                           // local file override
		Stage2: Source{URL: explicitSrv.URL + "/custom-stage2.bin"}, // explicit URL override
		// Kernel/Initrd/Cmdline: no override, default to "latest GitHub release"
	}
	assets, err := ResolveBIOS(src, "x86_64", t.TempDir())
	if err != nil {
		t.Fatalf("ResolveBIOS: %v", err)
	}
	defer assets.RemoveAll()

	check := func(path, want string) {
		t.Helper()
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("content = %q, want %q", got, want)
		}
	}
	check(assets.Stage1, "local-stage1")
	check(assets.Stage2, "from-custom-stage2.bin")
	check(assets.Kernel, "real-kernel-bytes")
	check(assets.Initrd, "real-initrd-bytes")
	check(assets.Cmdline, "real-cmdline-bytes")
}

// TestResolveBIOS_DefaultAssetsAreTagPinnedAndChecksumVerified is the
// direct regression test for F16 (unidoc-alip's PR #5 review): all
// five default-sourced assets must come from ONE resolved tag and
// pass SHA256SUMS verification.
func TestResolveBIOS_DefaultAssetsAreTagPinnedAndChecksumVerified(t *testing.T) {
	assetContent := map[string]string{
		"alpine-zfsboot-x86_64-bios-stage1.bin": "stage1-bytes",
		"alpine-zfsboot-x86_64-bios-stage2.bin": "stage2-bytes",
		"alpine-zfsboot-x86_64-vmlinuz":         "kernel-bytes",
		"alpine-zfsboot-x86_64-initramfs.img":   "initrd-bytes",
		"alpine-zfsboot-x86_64-cmdline.txt":     "cmdline-bytes",
	}
	fakeRelease := newFakeReleaseServer(t, "v1.2.3", assetContent)
	defer fakeRelease.Close()

	origAPI, origDownload := apiLatestReleaseURL, downloadBaseURLTemplate
	apiLatestReleaseURL = fakeRelease.URL + "/api/latest"
	downloadBaseURLTemplate = fakeRelease.URL + "/download/%s/"
	defer func() { apiLatestReleaseURL, downloadBaseURLTemplate = origAPI, origDownload }()

	assets, err := ResolveBIOS(BIOSSources{}, "x86_64", t.TempDir())
	if err != nil {
		t.Fatalf("ResolveBIOS: %v", err)
	}
	defer assets.RemoveAll()

	for path, want := range map[string]string{
		assets.Stage1:  "stage1-bytes",
		assets.Stage2:  "stage2-bytes",
		assets.Kernel:  "kernel-bytes",
		assets.Initrd:  "initrd-bytes",
		assets.Cmdline: "cmdline-bytes",
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("content = %q, want %q", got, want)
		}
	}
}

// TestResolveBIOS_ChecksumMismatchRefused proves the checksum
// verification is real, not decorative: an asset whose content does
// NOT match what SHA256SUMS claims must be refused, not silently
// accepted.
func TestResolveBIOS_ChecksumMismatchRefused(t *testing.T) {
	tag := "v1.2.3"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q}`, tag)
	})
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[len("/download/"+tag+"/"):]
		if name == "SHA256SUMS" {
			// A real, correctly-shaped hash - but for content that is
			// NOT what any asset below actually serves.
			fakeSum := sha256.Sum256([]byte("this is not the real content"))
			names := []string{
				"alpine-zfsboot-x86_64-bios-stage1.bin",
				"alpine-zfsboot-x86_64-bios-stage2.bin",
				"alpine-zfsboot-x86_64-vmlinuz",
				"alpine-zfsboot-x86_64-initramfs.img",
				"alpine-zfsboot-x86_64-cmdline.txt",
			}
			for _, n := range names {
				fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(fakeSum[:]), n)
			}
			return
		}
		w.Write([]byte("real-but-mismatched-content"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	origAPI, origDownload := apiLatestReleaseURL, downloadBaseURLTemplate
	apiLatestReleaseURL = srv.URL + "/api/latest"
	downloadBaseURLTemplate = srv.URL + "/download/%s/"
	defer func() { apiLatestReleaseURL, downloadBaseURLTemplate = origAPI, origDownload }()

	dir := t.TempDir()
	_, err := ResolveBIOS(BIOSSources{}, "x86_64", dir)
	if err == nil {
		t.Fatal("ResolveBIOS with content that doesn't match SHA256SUMS: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "SHA256 mismatch") {
		t.Errorf("error = %q, want it to mention a SHA256 mismatch", err.Error())
	}
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		t.Errorf("temp dir has %d leftover file(s) after a checksum-mismatch failure, want 0: %v", len(entries), entries)
	}
}
