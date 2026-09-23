package main

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	zfs "github.com/unidoc/alpine-zfsboot/internal/zfsnative"
)

// TestParseFeatureStats_RealShape mirrors a real feature_stats nvlist -
// some features active (refcount > 0), some merely enabled (present,
// refcount == 0) - and asserts both the display-string and active-only
// outputs, the same contract the removed `zpool get` text parser had.
func TestParseFeatureStats_RealShape(t *testing.T) {
	stats := zfs.Nvlist{
		"com.delphix:async_destroy": uint64(1),
		"com.delphix:bookmarks":     uint64(0),
		"org.open-zfs:large_blocks": uint64(3),
	}
	poolFeatures, activeFeatures, err := parseFeatureStats(stats)
	if err != nil {
		t.Fatalf("parseFeatureStats: %v", err)
	}
	if len(poolFeatures) != 3 {
		t.Errorf("poolFeatures = %v, want 3 entries", poolFeatures)
	}
	if len(activeFeatures) != 2 {
		t.Errorf("activeFeatures = %v, want [async_destroy large_blocks] (2 entries)", activeFeatures)
	}
	want := map[string]bool{"async_destroy": true, "large_blocks": true}
	for _, f := range activeFeatures {
		if !want[f] {
			t.Errorf("unexpected active feature %q", f)
		}
	}
	hasEnabledBookmarks := false
	for _, pf := range poolFeatures {
		if pf == "bookmarks=enabled" {
			hasEnabledBookmarks = true
		}
	}
	if !hasEnabledBookmarks {
		t.Errorf("poolFeatures = %v, want \"bookmarks=enabled\" (present, refcount 0)", poolFeatures)
	}
}

// TestParseFeatureStats_Empty is this rewrite's own version of the
// original text-parser's sawFeatureLine regression test: an empty
// feature_stats must be a real error, never silently zero active
// features feeding CheckZFSCompat into a false VERIFIED (nothing
// unsupported among nothing).
func TestParseFeatureStats_Empty(t *testing.T) {
	_, activeFeatures, err := parseFeatureStats(zfs.Nvlist{})
	if err == nil {
		t.Fatal("parseFeatureStats on an empty feature_stats: want an error, got nil - this is the exact false-VERIFIED bug")
	}
	if len(activeFeatures) != 0 {
		t.Errorf("activeFeatures = %v, want none", activeFeatures)
	}
}

// TestParseFeatureStats_AllEnabledNoneActive proves a real, legitimate
// pool state (every feature merely enabled, none active yet - a
// freshly created pool before anything exercises a feature-gated code
// path) is NOT confused with the empty/parse-failure case above.
func TestParseFeatureStats_AllEnabledNoneActive(t *testing.T) {
	stats := zfs.Nvlist{
		"com.delphix:async_destroy": uint64(0),
		"com.delphix:bookmarks":     uint64(0),
	}
	poolFeatures, activeFeatures, err := parseFeatureStats(stats)
	if err != nil {
		t.Errorf("parseFeatureStats with real feature_stats entries (all enabled, none active): %v", err)
	}
	if len(poolFeatures) != 2 {
		t.Errorf("poolFeatures = %v, want 2 entries", poolFeatures)
	}
	if len(activeFeatures) != 0 {
		t.Errorf("activeFeatures = %v, want none (enabled != active)", activeFeatures)
	}
}

// TestParseFeatureStats_UnknownGUID is the direct test for the user's
// own explicit requirement: an unrecognized feature GUID must never
// silently disappear. A pool reporting a real, active feature this
// project's own vendored guid table doesn't know about must fail
// closed (an error, which bootCompatVerdict turns into UNKNOWN) -
// never silently proceed as if that feature didn't exist, which could
// let CheckZFSCompat report VERIFIED for a pool whose true feature set
// was never fully examined.
func TestParseFeatureStats_UnknownGUID(t *testing.T) {
	stats := zfs.Nvlist{
		"com.delphix:async_destroy":     uint64(1),
		"org.openzfs:new_awesome_thing": uint64(7), // not in zfsFeatureGUIDToShortName
	}
	poolFeatures, activeFeatures, err := parseFeatureStats(stats)
	if err == nil {
		t.Fatal("parseFeatureStats with an unrecognized GUID: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "org.openzfs:new_awesome_thing") {
		t.Errorf("error = %q, want it to name the exact unrecognized guid", err.Error())
	}
	if poolFeatures != nil || activeFeatures != nil {
		t.Errorf("poolFeatures=%v activeFeatures=%v, want both nil on an unknown-guid error - a partial feature list must never be used", poolFeatures, activeFeatures)
	}
}

// TestParseFeatureStats_UnknownGUID_EvenIfNotActive proves the
// fail-closed behavior is NOT limited to active (refcount > 0)
// unknown features - an unrecognized GUID that's merely "enabled" must
// also refuse, since its presence at all signals a pool state this
// project's own tooling doesn't fully understand yet.
func TestParseFeatureStats_UnknownGUID_EvenIfNotActive(t *testing.T) {
	stats := zfs.Nvlist{
		"com.delphix:async_destroy":     uint64(1),
		"org.openzfs:new_awesome_thing": uint64(0),
	}
	_, _, err := parseFeatureStats(stats)
	if err == nil {
		t.Fatal("parseFeatureStats with an unrecognized, merely-enabled GUID: want an error, got nil")
	}
}

// TestParseFeatureStats_WrongValueType covers a malformed/unexpected
// nvlist decode: a feature_stats entry whose value isn't a uint64 (the
// real kernel always writes these via nvlist_add_uint64 - see
// zfsnative.go's own provenance comment - so anything else here is a
// decode or kernel-response-shape mismatch, not a real pool state).
func TestParseFeatureStats_WrongValueType(t *testing.T) {
	stats := zfs.Nvlist{
		"com.delphix:async_destroy": "not-a-uint64",
	}
	_, _, err := parseFeatureStats(stats)
	if err == nil {
		t.Fatal("parseFeatureStats with a non-uint64 value: want an error, got nil")
	}
}

// TestParseFeatureStats_DeterministicOrder is the regression test for
// sorting poolFeatures/activeFeatures before returning: stats is a Go map,
// so iterating it directly (as the old code did) produces a randomized
// order every run - the exact reason a real CAX status run's own "Pool
// features:" line looked shuffled between runs against the SAME pool.
// Runs parseFeatureStats on the same input many times and requires
// byte-identical output every time, not just the same set of features.
func TestParseFeatureStats_DeterministicOrder(t *testing.T) {
	stats := zfs.Nvlist{
		"com.delphix:async_destroy":     uint64(1),
		"com.delphix:bookmarks":         uint64(0),
		"org.open-zfs:large_blocks":     uint64(3),
		"org.openzfs:zilsaxattr":        uint64(1),
		"com.delphix:spacemap_v2":       uint64(0),
		"org.openzfs:blake3":            uint64(0),
		"com.delphix:hole_birth":        uint64(1),
		"com.klarasystems:vdev_zaps_v2": uint64(1),
	}
	firstPF, firstAF, err := parseFeatureStats(stats)
	if err != nil {
		t.Fatal(err)
	}
	if !sort.StringsAreSorted(firstPF) {
		t.Errorf("poolFeatures = %v, want sorted", firstPF)
	}
	if !sort.StringsAreSorted(firstAF) {
		t.Errorf("activeFeatures = %v, want sorted", firstAF)
	}
	for i := 0; i < 20; i++ {
		pf, af, err := parseFeatureStats(stats)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(pf, ",") != strings.Join(firstPF, ",") {
			t.Fatalf("run %d: poolFeatures = %v, want %v (same input, different output order)", i, pf, firstPF)
		}
		if strings.Join(af, ",") != strings.Join(firstAF, ",") {
			t.Fatalf("run %d: activeFeatures = %v, want %v (same input, different output order)", i, af, firstAF)
		}
	}
}

// TestZfsFeatureGUIDToShortName_CoversExistingCompatibilityData proves
// this table is a real superset of every short name this project's own
// already-vendored, already-trusted zfs-compatibility.d/openzfs-2.4
// file uses - a mismatch here would mean a real, currently-supported
// feature would incorrectly fail closed as "unknown" the very first
// time this project ever sees it on a live pool.
func TestZfsFeatureGUIDToShortName_CoversExistingCompatibilityData(t *testing.T) {
	known := make(map[string]bool, len(zfsFeatureGUIDToShortName))
	for _, name := range zfsFeatureGUIDToShortName {
		known[name] = true
	}
	// The exact set this project's own compat data already vendors for
	// OpenZFS 2.4 (see zfs-compatibility.d/openzfs-2.4) - short names
	// only, one per real line in that file.
	for _, name := range []string{
		"async_destroy", "empty_bpobj", "lz4_compress", "multi_vdev_crash_dump",
		"spacemap_histogram", "enabled_txg", "hole_birth", "extensible_dataset",
		"bookmarks", "filesystem_limits", "embedded_data", "livelist",
		"log_spacemap", "large_blocks", "large_dnode", "sha512", "skein",
		"edonr", "redaction_bookmarks", "redacted_datasets", "bookmark_written",
		"device_removal", "obsolete_counts", "userobj_accounting", "bookmark_v2",
		"encryption", "project_quota", "allocation_classes", "resilver_defer",
		"device_rebuild", "zstd_compress", "draid", "zilsaxattr", "head_errlog",
		"blake3", "block_cloning", "vdev_zaps_v2", "spacemap_v2", "zpool_checkpoint",
	} {
		if !known[name] {
			t.Errorf("zfsFeatureGUIDToShortName is missing %q, which zfs-compatibility.d/openzfs-2.4 already vendors - a real pool using this feature would fail closed as unknown", name)
		}
	}
}

// TestRecoverZFSCall_CatchesPanic proves the panic-to-error conversion
// actually works, directly - the general defense-in-depth backstop
// recoverZFSCall provides regardless of the specific finding that
// originally motivated it (see that function's own doc comment, and
// internal/zfsnative/nvlist_bounds_test.go for the bounds-check fix
// itself): a genuinely malformed/corrupted response must never crash this
// whole process.
func TestRecoverZFSCall_CatchesPanic(t *testing.T) {
	_, err := recoverZFSCall(func() (int, error) {
		panic("simulated: make([]byte, -1) style panic from a corrupted nelem")
	})
	if err == nil {
		t.Fatal("recoverZFSCall over a panicking function: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "simulated") {
		t.Errorf("error = %q, want it to include the panic's own message", err.Error())
	}
}

func TestRecoverZFSCall_PassesThroughSuccess(t *testing.T) {
	got, err := recoverZFSCall(func() (int, error) { return 42, nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 42 {
		t.Errorf("got %d, want 42", got)
	}
}

func TestRecoverZFSCall_PassesThroughRealError(t *testing.T) {
	want := "a real, non-panic error"
	_, err := recoverZFSCall(func() (int, error) { return 0, fmt.Errorf("%s", want) })
	if err == nil || err.Error() != want {
		t.Errorf("got %v, want %q unchanged (no panic occurred, recoverZFSCall must not alter a real error)", err, want)
	}
}
