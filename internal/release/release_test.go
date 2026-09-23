package release

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestResolveBIOS_MixedSources(t *testing.T) {
	dir := t.TempDir()
	localStage1 := dir + "/my-stage1.bin"
	if err := os.WriteFile(localStage1, []byte("local-stage1"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("from-" + r.URL.Path[1:]))
	}))
	defer srv.Close()
	origBase := baseURL
	baseURL = srv.URL + "/"
	defer func() { baseURL = origBase }()

	src := BIOSSources{
		Stage1: Source{File: localStage1},                   // local file override
		Stage2: Source{URL: srv.URL + "/custom-stage2.bin"}, // explicit URL override
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
	check(assets.Kernel, "from-alpine-zfsboot-x86_64-vmlinuz")
	check(assets.Initrd, "from-alpine-zfsboot-x86_64-initramfs.img")
	check(assets.Cmdline, "from-alpine-zfsboot-x86_64-cmdline.txt")
}
