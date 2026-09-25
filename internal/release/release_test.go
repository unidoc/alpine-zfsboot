package release

import (
	"context"
	"crypto/ed25519"
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
//
// Points apiLatestReleaseURL at the stalling server, not a plain
// download base - Download() now resolves a tag FIRST (F16, unidoc-
// alip's PR #5 follow-up review; see ResolveEFI's own comment), so
// that's the first network call it makes and the one this test needs
// to stall to prove the timeout applies at all.
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

	origClient, origAPI := httpClient, apiLatestReleaseURL
	httpClient = &http.Client{Timeout: 100 * time.Millisecond}
	apiLatestReleaseURL = srv.URL + "/"
	defer func() { httpClient, apiLatestReleaseURL = origClient, origAPI }()

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

func TestSourceResolve_LocalFileWinsOverURL(t *testing.T) {
	dir := t.TempDir()
	localPath := dir + "/local.bin"
	if err := os.WriteFile(localPath, []byte("local-content"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := Source{File: localPath, URL: "http://should-not-be-fetched.invalid/asset"}
	got, err := src.resolve("http://also-should-not-be-fetched.invalid/asset", "asset", t.TempDir())
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
	_, signPriv := withTestTrustedSigningKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/explicit":
			w.Write([]byte("explicit-content"))
		case "/explicit.minisig":
			w.Write([]byte(testSignMinisign(signPriv, []byte("explicit-content"), "alpine-zfsboot v9.9.9 - explicit")))
		default:
			w.Write([]byte("default-content"))
		}
	}))
	defer srv.Close()

	src := Source{URL: srv.URL + "/explicit"}
	got, err := src.resolve(srv.URL+"/default", "explicit", t.TempDir())
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

// TestSourceResolve_ExplicitURLWithoutSignatureRefused is the direct
// regression test for the security-contract question a real review
// raised: does an explicit --*-url override really deserve to bypass
// signature verification the same way --*-file does, just because
// it's explicit? No - see Source.resolve's own doc comment for the
// full reasoning. This proves it: an explicit URL serving perfectly
// real content, with no signature published for it at all, is
// refused, exactly like the resolved-default path already is
// (TestResolveEFI_MissingSignatureRefused).
func TestSourceResolve_ExplicitURLWithoutSignatureRefused(t *testing.T) {
	withTestTrustedSigningKey(t) // a trusted key exists, but nothing signs anything below
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/explicit" {
			w.Write([]byte("explicit-content"))
			return
		}
		w.WriteHeader(http.StatusNotFound) // no .minisig published at all
	}))
	defer srv.Close()

	src := Source{URL: srv.URL + "/explicit"}
	_, err := src.resolve("", "explicit", t.TempDir())
	if err == nil {
		t.Fatal("resolve on an explicit --*-url with no published signature: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "signature") {
		t.Errorf("error = %q, want it to mention the signature check", err.Error())
	}
}

func TestSourceResolve_FallsBackToDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("default-content"))
	}))
	defer srv.Close()

	src := Source{} // nothing overridden at all
	got, err := src.resolve(srv.URL+"/default", "default", t.TempDir())
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
// assetContent, each named asset's own content, and (when signPriv is
// non-nil - pass nil from a test that doesn't need signature coverage
// at all) a real, correctly-formed <name>.minisig for every asset,
// signed with signPriv - everything ResolveBIOS's own default
// (non-overridden) path now needs (F16, unidoc-alip's PR #5 review,
// and the minisign signature check, issue #2). Returns the base URL
// to point apiLatestReleaseURL/downloadBaseURLTemplate at.
func newFakeReleaseServer(t *testing.T, tag string, assetContent map[string]string, signPriv ed25519.PrivateKey) *httptest.Server {
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
		if signPriv != nil && strings.HasSuffix(name, ".minisig") {
			assetName := strings.TrimSuffix(name, ".minisig")
			content, ok := assetContent[assetName]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			// Matches release.yml's own real `-t "alpine-zfsboot ${GITHUB_REF_NAME} - ${f}"`
			// format exactly - since F2 (PR #10 review), verifySignatureAtURL's
			// default-path check binds against exactly this string.
			w.Write([]byte(testSignMinisign(signPriv, []byte(content), "alpine-zfsboot "+tag+" - "+assetName)))
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

	_, signPriv := withTestTrustedSigningKey(t)
	explicitSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".minisig") {
			assetPath := strings.TrimSuffix(r.URL.Path, ".minisig")
			// "alpine-zfsboot-x86_64-bios-stage2.bin" is the canonical
			// asset name ResolveBIOS binds this signature's comment
			// against (F2) - unrelated to this mirror's own URL path.
			w.Write([]byte(testSignMinisign(signPriv, []byte("from-"+assetPath[1:]), "alpine-zfsboot mirror - alpine-zfsboot-x86_64-bios-stage2.bin")))
			return
		}
		w.Write([]byte("from-" + r.URL.Path[1:]))
	}))
	defer explicitSrv.Close()

	assetContent := map[string]string{
		"alpine-zfsboot-x86_64-vmlinuz":       "real-kernel-bytes",
		"alpine-zfsboot-x86_64-initramfs.img": "real-initrd-bytes",
		"alpine-zfsboot-x86_64-cmdline.txt":   "real-cmdline-bytes",
	}
	fakeRelease := newFakeReleaseServer(t, "v9.9.9", assetContent, signPriv)
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
	_, signPriv := withTestTrustedSigningKey(t)
	assetContent := map[string]string{
		"alpine-zfsboot-x86_64-bios-stage1.bin": "stage1-bytes",
		"alpine-zfsboot-x86_64-bios-stage2.bin": "stage2-bytes",
		"alpine-zfsboot-x86_64-vmlinuz":         "kernel-bytes",
		"alpine-zfsboot-x86_64-initramfs.img":   "initrd-bytes",
		"alpine-zfsboot-x86_64-cmdline.txt":     "cmdline-bytes",
	}
	fakeRelease := newFakeReleaseServer(t, "v1.2.3", assetContent, signPriv)
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

// signedFixture is one genuinely-signed file for the tests below: the
// bytes, and the name + tag it was really signed under (what
// release.yml's real `-t` puts in the trusted comment).
type signedFixture struct {
	content   string
	signedAs  string
	signedTag string
}

func genuineFixture(tag, name string) signedFixture {
	return signedFixture{content: "bytes-of[" + tag + "/" + name + "]", signedAs: name, signedTag: tag}
}

// slotSubstitutionServer serves, at each requested x86_64-tag path,
// whatever genuinely-signed file `served` says for that name - with a
// SHA256SUMS computed from what it actually serves, exactly like a
// real attacker who controls the asset bytes and the checksum listing
// but not the signing key would (this package's own threat model,
// see signature.go's own doc comment).
func slotSubstitutionServer(t *testing.T, tag string, served map[string]signedFixture, priv ed25519.PrivateKey) *httptest.Server {
	t.Helper()
	var sums strings.Builder
	for name, f := range served {
		s := sha256.Sum256([]byte(f.content))
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(s[:]), name)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/latest" {
			fmt.Fprintf(w, `{"tag_name":%q}`, tag)
			return
		}
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if name == "SHA256SUMS" {
			w.Write([]byte(sums.String()))
			return
		}
		sig := strings.HasSuffix(name, ".minisig")
		f, ok := served[strings.TrimSuffix(name, ".minisig")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if sig {
			w.Write([]byte(testSignMinisign(priv, []byte(f.content), "alpine-zfsboot "+f.signedTag+" - "+f.signedAs)))
			return
		}
		w.Write([]byte(f.content))
	}))
}

// TestResolveBIOS_SignatureBoundToSlotAndRelease is the direct
// regression test for F2 (unidoc-alip's PR #10 review): before this
// fix, ResolveBIOS/verifySignatureAtURL checked only that a downloaded
// asset's signature came from a trusted key, never that the signature
// was actually FOR the slot/release it was served at - so a genuinely-
// signed asset from any arch, any release, or any other slot verified
// at any path. Each case below is a real signed file the trusted key
// really produced, just served at the wrong path; only "legit" should
// ever be accepted.
func TestResolveBIOS_SignatureBoundToSlotAndRelease(t *testing.T) {
	_, priv := withTestTrustedSigningKey(t)
	names := BIOSAssetName("x86_64")
	arm := BIOSAssetName("aarch64")
	const latest, older = "v1.3.0", "v1.2.0"

	legit := map[string]signedFixture{
		names.Stage1:  genuineFixture(latest, names.Stage1),
		names.Stage2:  genuineFixture(latest, names.Stage2),
		names.Kernel:  genuineFixture(latest, names.Kernel),
		names.Initrd:  genuineFixture(latest, names.Initrd),
		names.Cmdline: genuineFixture(latest, names.Cmdline),
	}
	with := func(over map[string]signedFixture) map[string]signedFixture {
		m := map[string]signedFixture{}
		for k, v := range legit {
			m[k] = v
		}
		for k, v := range over {
			m[k] = v
		}
		return m
	}

	cases := []struct {
		name    string
		served  map[string]signedFixture
		wantErr bool
	}{
		{"legit latest release", legit, false},
		{"cross-arch: aarch64 kernel+initrd+cmdline at x86_64 paths", with(map[string]signedFixture{
			names.Kernel:  genuineFixture(latest, arm.Kernel),
			names.Initrd:  genuineFixture(latest, arm.Initrd),
			names.Cmdline: genuineFixture(latest, arm.Cmdline),
		}), true},
		{"mixed release: older stage2+kernel+initrd, latest cmdline", with(map[string]signedFixture{
			names.Stage2: genuineFixture(older, names.Stage2),
			names.Kernel: genuineFixture(older, names.Kernel),
			names.Initrd: genuineFixture(older, names.Initrd),
		}), true},
		{"cross-slot: cmdline.txt bytes served as initrd", with(map[string]signedFixture{
			names.Initrd: genuineFixture(latest, names.Cmdline),
		}), true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := slotSubstitutionServer(t, latest, c.served, priv)
			defer srv.Close()
			origAPI, origDL := apiLatestReleaseURL, downloadBaseURLTemplate
			apiLatestReleaseURL = srv.URL + "/api/latest"
			downloadBaseURLTemplate = srv.URL + "/download/%s/"
			defer func() { apiLatestReleaseURL, downloadBaseURLTemplate = origAPI, origDL }()

			assets, err := ResolveBIOS(BIOSSources{}, "x86_64", t.TempDir())
			if c.wantErr {
				if err == nil {
					assets.RemoveAll()
					t.Fatal("ResolveBIOS: want an error (signature valid for a different slot/release), got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveBIOS: %v", err)
			}
			assets.RemoveAll()
		})
	}
}

// TestSourceResolve_URLOverrideSignatureBoundToAssetName is the
// --*-url half of F2: the URL is unknown to have any particular
// release tag, so only the asset NAME is bound (not the tag) - a
// legitimate renamed mirror of the right asset still works, but a
// mirror serving a genuinely-signed asset for a DIFFERENT slot is
// refused.
func TestSourceResolve_URLOverrideSignatureBoundToAssetName(t *testing.T) {
	_, priv := withTestTrustedSigningKey(t)
	names := BIOSAssetName("x86_64")
	arm := BIOSAssetName("aarch64")

	cases := []struct {
		name    string
		serve   signedFixture
		wantErr bool
	}{
		{"legit mirror (renamed path) of the x86_64 kernel", genuineFixture("v1.3.0", names.Kernel), false},
		{"mirror serving the aarch64 kernel for --kernel-url", genuineFixture("v1.3.0", arm.Kernel), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, ".minisig") {
					w.Write([]byte(testSignMinisign(priv, []byte(c.serve.content), "alpine-zfsboot "+c.serve.signedTag+" - "+c.serve.signedAs)))
					return
				}
				w.Write([]byte(c.serve.content))
			}))
			defer srv.Close()

			src := Source{URL: srv.URL + "/mirror/kernel-latest"}
			path, err := src.resolve("", names.Kernel, t.TempDir())
			if c.wantErr {
				if err == nil {
					os.Remove(path)
					t.Fatal("resolve: want an error (signed for a different asset), got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			os.Remove(path)
		})
	}
}

// TestResolveEFI_DefaultIsTagPinnedAndChecksumVerified is the direct
// regression test for F16 (unidoc-alip's PR #5 follow-up review):
// ResolveEFI's own default (no File/URL override) path must go
// through the same tag-resolution + SHA256SUMS verification
// ResolveBIOS's own default fields already use, not a bare
// releases/latest/download/ fetch.
func TestResolveEFI_DefaultIsTagPinnedAndChecksumVerified(t *testing.T) {
	_, signPriv := withTestTrustedSigningKey(t)
	assetContent := map[string]string{
		"alpine-zfsboot-x86_64.EFI": "efi-bytes",
	}
	fakeRelease := newFakeReleaseServer(t, "v1.2.3", assetContent, signPriv)
	defer fakeRelease.Close()

	origAPI, origDownload := apiLatestReleaseURL, downloadBaseURLTemplate
	apiLatestReleaseURL = fakeRelease.URL + "/api/latest"
	downloadBaseURLTemplate = fakeRelease.URL + "/download/%s/"
	defer func() { apiLatestReleaseURL, downloadBaseURLTemplate = origAPI, origDownload }()

	path, err := ResolveEFI(Source{}, "x86_64", t.TempDir())
	if err != nil {
		t.Fatalf("ResolveEFI: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "efi-bytes" {
		t.Errorf("content = %q, want %q", got, "efi-bytes")
	}
}

// TestResolveEFI_MissingSignatureRefused is the direct regression
// test for issue #2 (unidoc-alip's PR #1 review): a default-resolved
// asset with a matching SHA256SUMS entry but NO .minisig at all must
// still be refused - checksum agreement alone (same-origin, provable
// by anything able to serve the asset in the first place) is exactly
// the gap the signature check exists to close, and this test's own
// server serves a perfectly self-consistent checksum for content that
// simply has no signature published for it.
func TestResolveEFI_MissingSignatureRefused(t *testing.T) {
	withTestTrustedSigningKey(t) // a trusted key exists, but nothing signs anything below
	assetContent := map[string]string{"alpine-zfsboot-x86_64.EFI": "efi-bytes"}
	fakeRelease := newFakeReleaseServer(t, "v1.2.3", assetContent, nil) // signPriv=nil: never serves .minisig
	defer fakeRelease.Close()

	origAPI, origDownload := apiLatestReleaseURL, downloadBaseURLTemplate
	apiLatestReleaseURL = fakeRelease.URL + "/api/latest"
	downloadBaseURLTemplate = fakeRelease.URL + "/download/%s/"
	defer func() { apiLatestReleaseURL, downloadBaseURLTemplate = origAPI, origDownload }()

	dir := t.TempDir()
	_, err := ResolveEFI(Source{}, "x86_64", dir)
	if err == nil {
		t.Fatal("ResolveEFI with a checksum-valid but entirely unsigned asset: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "signature") {
		t.Errorf("error = %q, want it to mention the signature check", err.Error())
	}
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		t.Errorf("temp dir has %d leftover file(s) after a missing-signature failure, want 0: %v", len(entries), entries)
	}
}

// TestResolveEFI_SignatureFromUntrustedKeyRefused proves a real,
// well-formed, internally-consistent .minisig - correctly signed, just
// with a DIFFERENT key than the one this process actually trusts - is
// refused. The scenario this guards against: an attacker who controls
// the same origin serving the asset AND its checksums AND some
// signature, but does not hold the private key release.yml's own
// signing job holds (PR #10 review, F4: this comment used to call
// that "alpine-zfsboot's own real offline signing key" - see
// signature.go's own threat-model comment for what actually holds it
// and what does and doesn't follow from that) - exactly the threat
// issue #2 describes, and exactly why trustedSigningKeys is an
// embedded, fixed set rather than anything discovered from the
// download itself.
func TestResolveEFI_SignatureFromUntrustedKeyRefused(t *testing.T) {
	_, trustedPriv := withTestTrustedSigningKey(t)
	_ = trustedPriv // the trusted key exists, but the server below signs with a DIFFERENT one
	_, attackerPriv := newTestSigningKeypair()

	assetContent := map[string]string{"alpine-zfsboot-x86_64.EFI": "efi-bytes"}
	fakeRelease := newFakeReleaseServer(t, "v1.2.3", assetContent, attackerPriv)
	defer fakeRelease.Close()

	origAPI, origDownload := apiLatestReleaseURL, downloadBaseURLTemplate
	apiLatestReleaseURL = fakeRelease.URL + "/api/latest"
	downloadBaseURLTemplate = fakeRelease.URL + "/download/%s/"
	defer func() { apiLatestReleaseURL, downloadBaseURLTemplate = origAPI, origDownload }()

	dir := t.TempDir()
	_, err := ResolveEFI(Source{}, "x86_64", dir)
	if err == nil {
		t.Fatal("ResolveEFI with a signature from an untrusted key: want an error, got nil")
	}
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		t.Errorf("temp dir has %d leftover file(s) after an untrusted-signature failure, want 0: %v", len(entries), entries)
	}
}

// TestResolveEFI_ChecksumMismatchRefused is ResolveEFI's own version
// of TestResolveBIOS_ChecksumMismatchRefused above - proves the
// checksum verification added for F16 is real for the UEFI asset too,
// not just BIOS's five.
func TestResolveEFI_ChecksumMismatchRefused(t *testing.T) {
	tag := "v1.2.3"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q}`, tag)
	})
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[len("/download/"+tag+"/"):]
		if name == "SHA256SUMS" {
			fakeSum := sha256.Sum256([]byte("this is not the real content"))
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(fakeSum[:]), "alpine-zfsboot-x86_64.EFI")
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
	_, err := ResolveEFI(Source{}, "x86_64", dir)
	if err == nil {
		t.Fatal("ResolveEFI with content that doesn't match SHA256SUMS: want an error, got nil")
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

// TestResolveEFI_ExplicitOverrideSkipsChecksumVerification proves an
// explicit File/URL override still bypasses the API/checksum path
// entirely (same as BIOS's own TestResolveBIOS_MixedSources) - an
// override was never claiming to BE the official release asset, so a
// server that would fail resolveTag/fetchChecksums entirely must not
// stop it from being used.
func TestResolveEFI_ExplicitOverrideSkipsChecksumVerification(t *testing.T) {
	origAPI := apiLatestReleaseURL
	apiLatestReleaseURL = "http://this-must-never-be-contacted.invalid/api"
	defer func() { apiLatestReleaseURL = origAPI }()

	dir := t.TempDir()
	localPath := dir + "/my.EFI"
	if err := os.WriteFile(localPath, []byte("local-efi-content"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := ResolveEFI(Source{File: localPath}, "x86_64", t.TempDir())
	if err != nil {
		t.Fatalf("ResolveEFI with a local file override: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "local-efi-content" {
		t.Errorf("content = %q, want %q", got, "local-efi-content")
	}
}
