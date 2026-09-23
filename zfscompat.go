// Package zfscompat answers one narrow, read-only question: does a
// pool's own ACTIVE feature set fit within what a given OpenZFS
// release line's kernel module actually implements? It never
// upgrades, imports, or otherwise mutates a pool - see cmd/tool's own
// zfsCompat doc comment for why this exists and what it deliberately
// does not do.
//
// Lives at the repo root, not under internal/, because Go's
// `//go:embed` directives cannot reference a parent directory ("..")
// - the embedding source file must sit in or below the directory tree
// holding the files it embeds. zfs-compatibility.d/ already lives at
// the repo root (build.sh stages it into the live system's own
// /usr/share/zfs/compatibility.d - see that script's own comment)
// and stays there unmodified; this file reuses those exact bytes
// rather than moving or duplicating them anywhere.
//
// Provenance for the embedded data itself: see
// zfs-compatibility.d/README.md - verbatim upstream openzfs/zfs
// compatibility.d files, not hand-written or generated.
//
// Verified against upstream OpenZFS source before trusting this data
// as authoritative (not assumed):
//
//   - Whether compatibility.d/openzfs-X.Y is the FULL feature set a
//     release line implements, or a deliberately narrower portability
//     subset: cross-checked directly against module/zcommon/
//     zfeature_common.c's own zfeature_register() calls at the
//     matching git tag, for two separate release lines (openzfs-2.2
//     against tag zfs-2.2.0, openzfs-2.4 against tag zfs-2.4.4) -
//     zero difference either direction, both times. It is the full
//     set, not a subset. (fi_zfs_mod_supported, the field this might
//     look like it should filter by instead, turned out to be
//     runtime plumbing - libzfs comparing itself against a *loaded*
//     kernel module's sysfs - not a static per-version property; see
//     zfs_mod_supported_feature()'s own comment in that same file:
//     "The zfs module spa_feature_table[]... always supports all the
//     features" for any build of the module itself.)
//
//   - Whether an unsupported feature that's merely `enabled` (not yet
//     `active`) should block a "compatible" verdict: checked directly
//     against the real pool-open gate, module/zfs/zfeature.c's own
//     spa_features_check() (called from spa_load/spa_open on every
//     real import) - `if (za->za_first_integer != 0 &&
//     !zfeature_is_supported(...))`, where za_first_integer is the
//     feature's reference count. A refcount of 0 (enabled, not yet
//     active - no on-disk format change has actually happened yet)
//     never fails that check, regardless of support; only a nonzero
//     refcount (active) does. This package checks active features
//     only, for exactly that reason - matching the real gate, not a
//     stricter or looser approximation of it.
package zfscompat

import (
	"embed"
	"fmt"
	"regexp"
	"strings"
)

//go:embed zfs-compatibility.d/openzfs-2.0-linux
//go:embed zfs-compatibility.d/openzfs-2.1-linux
//go:embed zfs-compatibility.d/openzfs-2.2
//go:embed zfs-compatibility.d/openzfs-2.3
//go:embed zfs-compatibility.d/openzfs-2.4
var zfsCompatFS embed.FS

// Only the Linux-relevant lines this project could ever actually boot
// against - zfs-compatibility.d/ also carries FreeBSD/macOS/grub2/zol
// entries this project has no use for and deliberately does not
// embed. Update this alongside zfs-compatibility.d/ itself whenever a
// new OpenZFS release line's own compatibility.d file lands there -
// nothing here regenerates or infers the list automatically, by
// design (see this file's own package comment on provenance).
var zfsCompatReleaseLineFile = map[string]string{
	"2.0": "zfs-compatibility.d/openzfs-2.0-linux",
	"2.1": "zfs-compatibility.d/openzfs-2.1-linux",
	"2.2": "zfs-compatibility.d/openzfs-2.2",
	"2.3": "zfs-compatibility.d/openzfs-2.3",
	"2.4": "zfs-compatibility.d/openzfs-2.4",
}

var zfsVersionPrefix = regexp.MustCompile(`^(\d+)\.(\d+)`)

// zfsReleaseLine extracts "2.4" out of a raw OpenZFS module version
// string like "2.4.4-1" or "2.4.4" (zfs.ko's own .modinfo version=
// field - see internal/initrdinfo). compatibility.d files are
// versioned per MINOR release line, not per patch release, so 2.4.4
// and a hypothetical 2.4.9 both resolve to the same "2.4" set.
func zfsReleaseLine(rawVersion string) (string, error) {
	m := zfsVersionPrefix.FindStringSubmatch(rawVersion)
	if m == nil {
		return "", fmt.Errorf("%q does not start with a recognizable X.Y OpenZFS version", rawVersion)
	}
	return m[1] + "." + m[2], nil
}

// zfsSupportedFeatures returns the feature short-names (e.g.
// "async_destroy" - the same string zpool's own "feature@<name>"
// property uses) this repo's vendored compatibility.d file for
// releaseLine lists. ok is false when this repo has no vendored file
// for that release line at all - an OpenZFS version this data simply
// doesn't cover yet, never to be treated as "unsupported": the caller
// must report that as unknown.
func zfsSupportedFeatures(releaseLine string) (features map[string]bool, ok bool) {
	path, known := zfsCompatReleaseLineFile[releaseLine]
	if !known {
		return nil, false
	}
	data, err := zfsCompatFS.ReadFile(path)
	if err != nil {
		// zfsCompatReleaseLineFile and the go:embed directives above
		// are maintained together, by hand, in this same file - a
		// mismatch between them is a build-time bug in this file, not
		// something a caller can meaningfully recover from at runtime.
		panic(fmt.Sprintf("zfscompat: %s listed but not embedded: %v", path, err))
	}
	features = make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		features[line] = true
	}
	return features, true
}

// ZFSCompatResult is the outcome of checking one pool's active
// feature set against one OpenZFS release line's own vendored
// compatibility data.
type ZFSCompatResult struct {
	ReleaseLine      string
	KnownReleaseLine bool
	Unsupported      []string // active features this release line's compatibility.d doesn't list
}

// Compatible is true only when the release line is one this repo has
// vendored data for AND every active feature is in that set. A
// release line this repo doesn't cover yet is NOT compatible - it's
// unknown; callers must check KnownReleaseLine separately rather than
// treating a false Compatible as a definite incompatibility.
func (r ZFSCompatResult) Compatible() bool {
	return r.KnownReleaseLine && len(r.Unsupported) == 0
}

// CheckZFSCompat checks activeFeatures (short names, e.g.
// "async_destroy" - already filtered to feature@* properties whose
// value is exactly "active", never "enabled" - see this file's own
// package comment for why that distinction is load-bearing, not
// cosmetic) against rawOpenZFSVersion's own release line.
func CheckZFSCompat(rawOpenZFSVersion string, activeFeatures []string) (ZFSCompatResult, error) {
	line, err := zfsReleaseLine(rawOpenZFSVersion)
	if err != nil {
		return ZFSCompatResult{}, err
	}
	supported, ok := zfsSupportedFeatures(line)
	res := ZFSCompatResult{ReleaseLine: line, KnownReleaseLine: ok}
	if !ok {
		return res, nil
	}
	for _, f := range activeFeatures {
		if !supported[f] {
			res.Unsupported = append(res.Unsupported, f)
		}
	}
	return res, nil
}
