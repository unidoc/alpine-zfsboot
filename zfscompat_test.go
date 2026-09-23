package zfscompat

import "testing"

func TestZFSReleaseLine(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"2.4.4-1", "2.4", false},
		{"2.4.4", "2.4", false},
		{"2.2.0", "2.2", false},
		{"2.4", "2.4", false},
		{"garbage", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		got, err := zfsReleaseLine(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("zfsReleaseLine(%q): expected error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("zfsReleaseLine(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("zfsReleaseLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestZfsSupportedFeaturesKnownReleaseLine(t *testing.T) {
	features, ok := zfsSupportedFeatures("2.4")
	if !ok {
		t.Fatal("expected 2.4 to be a known release line")
	}
	// async_destroy has been present since the very first OpenZFS feature
	// flag set - a stable canary that any real compatibility.d/openzfs-2.4
	// file must list.
	if !features["async_destroy"] {
		t.Errorf("expected async_destroy in openzfs-2.4 feature set, got %v", features)
	}
}

func TestZfsSupportedFeaturesUnknownReleaseLine(t *testing.T) {
	_, ok := zfsSupportedFeatures("9.9")
	if ok {
		t.Error("expected 9.9 to be an unknown release line")
	}
}

func TestCheckZFSCompatVerified(t *testing.T) {
	result, err := CheckZFSCompat("2.4.4-1", []string{"async_destroy", "large_blocks"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.KnownReleaseLine {
		t.Fatal("expected 2.4 to be a known release line")
	}
	if !result.Compatible() {
		t.Errorf("expected compatible, got unsupported=%v", result.Unsupported)
	}
}

func TestCheckZFSCompatUnsupportedFeature(t *testing.T) {
	result, err := CheckZFSCompat("2.4.4-1", []string{"async_destroy", "totally_made_up_feature"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Compatible() {
		t.Fatal("expected incompatible due to unsupported feature")
	}
	if len(result.Unsupported) != 1 || result.Unsupported[0] != "totally_made_up_feature" {
		t.Errorf("unexpected Unsupported list: %v", result.Unsupported)
	}
}

func TestCheckZFSCompatUnknownReleaseLine(t *testing.T) {
	result, err := CheckZFSCompat("9.9.0-1", []string{"async_destroy"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.KnownReleaseLine {
		t.Fatal("expected 9.9 to be unknown")
	}
	if result.Compatible() {
		t.Fatal("Compatible() must be false when the release line itself is unknown")
	}
}

func TestCheckZFSCompatBadVersionString(t *testing.T) {
	_, err := CheckZFSCompat("not-a-version", nil)
	if err == nil {
		t.Fatal("expected an error for an unparseable version string")
	}
}

// A real, uniclus-01 (this project's own live Hetzner CAX machine) `status`
// run reported "Boot compatible: UNKNOWN - no verified OpenZFS-version-to-
// pool-feature-flag mapping exists yet" for a real zroot pool running
// OpenZFS 2.4.4-1 - alarming on its face, since 2.4 is exactly the release
// line this package vendors data for. Investigated directly rather than
// assumed either way: that exact message string does not exist anywhere in
// this tree (checked via grep, and via `git log -S` across all history) -
// bootCompatVerdict's own current UNKNOWN-for-unmapped-release-line message
// reads "no vendored compatibility data for OpenZFS release line %s", a
// different string entirely. The binary that printed it predates this
// package's current release-line logic/wording; it was never rebuilt after
// this project's own ZFS-compat-verification work landed (uncommitted, this
// session, like most of this project's recent history). Confirmed the
// CURRENT source handles this exact real machine's own reported state
// correctly: this test reproduces uniclus-01's own real "Pool features:"
// line verbatim (every feature at that exact state, not just the active
// ones - see the comment above the slice), filters to active-only the same
// way inspectZFSCompat() does, and asserts VERIFIED. Not a hypothetical
// case - this is uniclus-01's actual reported feature set at the time this
// was investigated.
func TestCheckZFSCompatUniclus01RealWorldState(t *testing.T) {
	// Every "feature@name" property uniclus-01's own real `zpool get all
	// zroot` reported, exactly as `status` displayed them (name=state) -
	// only the "=active" ones become activeFeatures, matching
	// inspectZFSCompat()'s own filter (feature@* whose value is exactly
	// "active", never "enabled" - see zfscompat.go's own package comment
	// on why that distinction is load-bearing).
	poolFeatures := map[string]string{
		"async_destroy": "enabled", "empty_bpobj": "active", "lz4_compress": "active",
		"multi_vdev_crash_dump": "enabled", "spacemap_histogram": "active", "enabled_txg": "active",
		"hole_birth": "active", "extensible_dataset": "active", "embedded_data": "active",
		"bookmarks": "enabled", "filesystem_limits": "enabled", "large_blocks": "enabled",
		"large_dnode": "enabled", "sha512": "enabled", "skein": "enabled", "edonr": "enabled",
		"userobj_accounting": "active", "encryption": "enabled", "project_quota": "active",
		"device_removal": "enabled", "obsolete_counts": "enabled", "zpool_checkpoint": "enabled",
		"spacemap_v2": "active", "allocation_classes": "enabled", "resilver_defer": "enabled",
		"bookmark_v2": "enabled", "redaction_bookmarks": "enabled", "redacted_datasets": "enabled",
		"bookmark_written": "enabled", "log_spacemap": "active", "livelist": "enabled",
		"device_rebuild": "enabled", "zstd_compress": "enabled", "draid": "enabled",
		"zilsaxattr": "active", "head_errlog": "active", "blake3": "enabled",
		"block_cloning": "active", "vdev_zaps_v2": "active",
	}
	var active []string
	for name, state := range poolFeatures {
		if state == "active" {
			active = append(active, name)
		}
	}
	if len(active) != 15 {
		t.Fatalf("test fixture itself is wrong - expected 15 active features out of uniclus-01's real 39, counted %d", len(active))
	}

	result, err := CheckZFSCompat("2.4.4-1", active)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.KnownReleaseLine {
		t.Fatalf("expected release line %q (from OpenZFS 2.4.4-1) to be known - this repo vendors zfs-compatibility.d/openzfs-2.4", result.ReleaseLine)
	}
	if result.ReleaseLine != "2.4" {
		t.Errorf("expected release line \"2.4\", got %q", result.ReleaseLine)
	}
	if !result.Compatible() {
		t.Errorf("expected uniclus-01's real active feature set to be compatible with OpenZFS 2.4 - unsupported=%v", result.Unsupported)
	}
}

// All five embedded release-line files must actually be readable through
// zfsSupportedFeatures - a mismatch between zfsCompatReleaseLineFile and the
// go:embed directives above it is a build-time bug in this package (see that
// map's own doc comment), but only surfaces as a panic at first use. Exercise
// every entry so CI catches it instead of a live boot-time inspect() call.
func TestAllEmbeddedReleaseLinesLoad(t *testing.T) {
	for line := range zfsCompatReleaseLineFile {
		features, ok := zfsSupportedFeatures(line)
		if !ok {
			t.Errorf("release line %q listed in zfsCompatReleaseLineFile but zfsSupportedFeatures reports unknown", line)
			continue
		}
		if len(features) == 0 {
			t.Errorf("release line %q loaded zero features - embed likely pointing at an empty/wrong file", line)
		}
	}
}
