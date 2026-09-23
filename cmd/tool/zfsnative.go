// Native OpenZFS pool/feature introspection - replaces exec.Command("zpool",
// ...) with internal/zfsnative, a pure-Go /dev/zfs ioctl client (no cgo, no
// libzfs, no shelling out to zpool/zfs/zdb). Part of this project's own
// self-containment requirement: a boot/recovery introspection tool must not
// depend on separately-installed userspace packages to understand its own
// live ZFS state, the same reasoning that already took xz/mount/umount/
// blkid/dropbearkey out of production Go elsewhere in this repo.
//
// internal/zfsnative started life as an external dependency
// (github.com/go-fsctl/zfs) and was internalized after a supply-chain/code
// audit found a real unbounded-allocation bug in its nvlist decoder (fixed
// here - see internal/zfsnative/nvlist.go's checkElemCount) and its
// single-maintainer, zero-tagged-release status made pinning an external
// commit a weaker guarantee than owning the trimmed, read-only-by-
// construction subset directly. See internal/zfsnative/abi.go for the full
// provenance note.
//
// ABI due diligence, done BEFORE adopting this code, not assumed: fetched
// the real, current OpenZFS source at all five release tags this project's
// own zfs-compatibility.d already vendors data for (zfs-2.0.0 through
// zfs-2.4.0) and diffed the exact zfs_cmd_t fields and ZFS_IOC_* numbers
// these operations use (zc_name/zc_nvlist_dst*, ZFS_IOC_POOL_CONFIGS=0x5a04,
// ZFS_IOC_POOL_STATS=0x5a05) - byte-for-byte identical across all five.
// Upstream go-fsctl/zfs's own README states it targets OpenZFS 2.2.x
// specifically; this project's own independent verification found the
// SPECIFIC fields/ioctls it relies on for these read-only calls are
// unaffected by the one real internal restructuring found elsewhere in
// zfs_cmd_t (a zc_inject_record union rework, deep in the struct, nowhere
// near zc_name/zc_nvlist_dst*). A future OpenZFS release line this project
// adds support for MUST be re-checked the same way before being trusted
// here - this project does not claim forward compatibility merely because
// the current layout happens to still work.
package main

import (
	"fmt"
	"sort"
	"strings"

	zfs "github.com/unidoc/alpine-zfsboot/internal/zfsnative"
)

// zfsFeatureGUIDToShortName maps every OpenZFS pool-feature GUID this
// project knows about to the short name zfs-compatibility.d's own vendored
// files (and this project's own `feature@<name>` display convention)
// already use.
//
// PROVENANCE: extracted directly from the real, current upstream OpenZFS
// source (module/zcommon/zfeature_common.c, github.com/openzfs/zfs,
// CDDL-1.0), specifically every zfeature_register(fid, guid, name, ...)
// call's own (guid, name) argument pair - not guessed, not copied from any
// third party's own re-derivation of the same facts. This is a FACTS-ONLY
// extraction (a lookup table of two-string pairs), not a port of any
// OpenZFS implementation code - no zfeature_register logic, no feature
// dependency/activation behavior, nothing beyond the guid<->name identity
// itself is reproduced here.
//
// Cross-checked directly against this project's own already-vendored
// zfs-compatibility.d/openzfs-2.4 short names before being trusted: every
// short name that file lists appears in this table (verified by set
// comparison against the real file, not assumed).
//
// Feature GUIDs are, by the feature-flags design itself, meant to be
// permanent and never reused or renamed once registered (this is the whole
// point of using a GUID instead of a plain short name for the pool's own
// on-disk feature identification) - so unlike zfs_cmd_t's own internal
// field layout, this table does not carry the same per-OpenZFS-release
// churn risk. It can still grow (new features get new GUIDs over time,
// including several already registered upstream past this project's own
// current 2.0-2.4 compatibility range, e.g. raidz_expansion/fast_dedup/
// longname - included here anyway since recognizing a GUID's short name and
// judging whether that FEATURE is supported by a given release line are two
// separate questions, the second one still correctly handled by
// zfscompat.CheckZFSCompat's own release-line data). An entry here is never
// evidence a feature is safe to use - only evidence this code recognizes
// its name. See parseFeatureStats's own fail-closed handling for a GUID
// NOT in this table.
var zfsFeatureGUIDToShortName = map[string]string{
	"org.zfsonlinux:allocation_classes":    "allocation_classes",
	"com.delphix:async_destroy":            "async_destroy",
	"org.openzfs:blake3":                   "blake3",
	"com.fudosecurity:block_cloning":       "block_cloning",
	"com.truenas:block_cloning_endian":     "block_cloning_endian",
	"com.datto:bookmark_v2":                "bookmark_v2",
	"com.delphix:bookmark_written":         "bookmark_written",
	"com.delphix:bookmarks":                "bookmarks",
	"org.openzfs:device_rebuild":           "device_rebuild",
	"com.delphix:device_removal":           "device_removal",
	"org.openzfs:draid":                    "draid",
	"com.seagate:draid_failure_domains":    "draid_failure_domains",
	"com.klarasystems:dynamic_gang_header": "dynamic_gang_header",
	"org.illumos:edonr":                    "edonr",
	"com.delphix:embedded_data":            "embedded_data",
	"com.delphix:empty_bpobj":              "empty_bpobj",
	"com.delphix:enabled_txg":              "enabled_txg",
	"com.datto:encryption":                 "encryption",
	"com.delphix:extensible_dataset":       "extensible_dataset",
	"com.klarasystems:fast_dedup":          "fast_dedup",
	"com.joyent:filesystem_limits":         "filesystem_limits",
	"com.delphix:head_errlog":              "head_errlog",
	"com.delphix:hole_birth":               "hole_birth",
	"org.open-zfs:large_blocks":            "large_blocks",
	"org.zfsonlinux:large_dnode":           "large_dnode",
	"com.klarasystems:large_microzap":      "large_microzap",
	"com.delphix:livelist":                 "livelist",
	"com.delphix:log_spacemap":             "log_spacemap",
	"org.zfsonlinux:longname":              "longname",
	"org.illumos:lz4_compress":             "lz4_compress",
	"com.joyent:multi_vdev_crash_dump":     "multi_vdev_crash_dump",
	"com.delphix:obsolete_counts":          "obsolete_counts",
	"com.truenas:physical_rewrite":         "physical_rewrite",
	"org.zfsonlinux:project_quota":         "project_quota",
	"org.openzfs:raidz_expansion":          "raidz_expansion",
	"com.delphix:redacted_datasets":        "redacted_datasets",
	"com.delphix:redaction_bookmarks":      "redaction_bookmarks",
	"com.delphix:redaction_list_spill":     "redaction_list_spill",
	"com.datto:resilver_defer":             "resilver_defer",
	"org.illumos:sha512":                   "sha512",
	"org.illumos:skein":                    "skein",
	"com.delphix:spacemap_histogram":       "spacemap_histogram",
	"com.delphix:spacemap_v2":              "spacemap_v2",
	"org.zfsonlinux:userobj_accounting":    "userobj_accounting",
	"com.klarasystems:vdev_zaps_v2":        "vdev_zaps_v2",
	"org.openzfs:zilsaxattr":               "zilsaxattr",
	"com.delphix:zpool_checkpoint":         "zpool_checkpoint",
	"org.freebsd:zstd_compress":            "zstd_compress",
}

// parseFeatureStats interprets a pool's "feature_stats" nvlist (the nested
// nvlist ZFS_IOC_POOL_STATS returns under that exact key - see
// module/zfs/spa.c's own spa_add_feature_stats(), which populates it via
// nvlist_add_uint64(features, feature.fi_guid, refcount) for every
// registered feature with a nonzero-or-present entry - confirmed directly
// against that real source, not assumed) into the SAME shape
// inspectZFSCompat's own (now-removed) `zpool get` text parser produced:
// display strings "name=state" for every enabled-or-active feature, and
// just the active ones' short names separately - so downstream code
// (bootCompatVerdict, zfscompat.CheckZFSCompat, report.print()) needs no
// changes at all.
//
// State derivation, per the same real source comment: a feature entry with
// refcount > 0 is "active" (in real, on-disk use); an entry present with
// refcount == 0 is "enabled" (turned on, not yet used); a feature with NO
// entry at all is "disabled" - the exact three-state logic `zpool get
// feature@X` itself derives from this same raw data, just computed here
// directly instead of trusting a pre-formatted CLI string.
//
// Fail-closed on any unrecognized GUID, deliberately, per this project's
// own explicit requirement: an active-or-enabled feature this code cannot
// map to a short name must never be silently dropped from consideration -
// doing so could let zfscompat.CheckZFSCompat report VERIFIED for a pool
// whose real feature set was never fully examined (the same "found nothing
// wrong because we didn't look at everything" trap the removed text
// parser's own sawFeatureLine check already guards against for a
// different failure mode). An unknown GUID is reported as its OWN error,
// naming the exact guid, rather than a generic "something went wrong".
//
// An EMPTY stats nvlist is NOT itself an error (F20, unidoc-alip's PR #5
// review, correcting a false premise this function's own earlier version
// asserted) - real upstream spa_add_feature_stats() (module/zfs/spa.c)
// only ever adds an entry for a feature that is actually enabled or
// active; a DISABLED feature (the default state for every feature on a
// pool created with `zpool create -d`, and the initial state of any
// feature never explicitly enabled) is skipped entirely, both from the
// live cache (feature_get_refcount() returning ENOTSUP) and from what's
// actually recorded on disk (only enabled/active features ever get a
// real entry in the on-disk feature ZAP to begin with). So a pool using
// zero non-default features - a real, valid, if uncommon state, not a
// hypothetical one - legitimately reports a genuinely empty feature_stats,
// and that pool IS compatible with every OpenZFS release line this
// project knows about (it uses nothing beyond what every release line
// already supports by definition). Previously reported as UNKNOWN with a
// confusing "parse/decode problem" message for exactly this real,
// compatible case.
func parseFeatureStats(stats zfs.Nvlist) (poolFeatures, activeFeatures []string, err error) {
	var unknown []string
	for guid, v := range stats {
		refcount, ok := v.(uint64)
		if !ok {
			return nil, nil, fmt.Errorf("feature_stats[%q] has Go type %T, want uint64 (a feature reference count) - nvlist decode or kernel response shape mismatch", guid, v)
		}
		name, known := zfsFeatureGUIDToShortName[guid]
		if !known {
			unknown = append(unknown, guid)
			continue
		}
		state := "enabled"
		if refcount > 0 {
			state = "active"
		}
		poolFeatures = append(poolFeatures, name+"="+state)
		if state == "active" {
			activeFeatures = append(activeFeatures, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown) // deterministic error text, not map-iteration order
		return nil, nil, fmt.Errorf("this pool reports %d feature GUID(s) not in this project's own vendored guid-to-name table (%s) - refusing to report a compatibility verdict computed from an incomplete feature set; update the table (see this file's own provenance comment) against current OpenZFS source", len(unknown), strings.Join(unknown, ", "))
	}
	// stats is a Go map (Nvlist), so iteration order above is randomized per
	// run - sort both slices so `status`'s own "Pool features:" line (and
	// any test/diff against it) is deterministic run to run on the SAME
	// real pool, not just correct.
	sort.Strings(poolFeatures)
	sort.Strings(activeFeatures)
	return poolFeatures, activeFeatures, nil
}

// recoverZFSCall runs fn and converts any panic it raises into a plain
// error instead of letting it crash this whole process.
//
// Why this exists, originally from a real source-level code audit of
// go-fsctl/zfs's own native nvlist decoder (now internal/zfsnative/
// nvlist.go's decodeScalar/decodePair, after internalization - see that
// file's own provenance note): a DATA_TYPE_*_ARRAY or DATA_TYPE_NVLIST_ARRAY
// pair's own nvp_value_elem (element count) is read straight off the wire,
// which crosses a kernel/userspace ioctl boundary and is therefore not under
// this decoder's own control - a kernel bug, hardware bit-flip, or a
// genuinely incompatible/mismatched kernel module could hand back any int32.
// The decoder now bounds-checks this (checkElemCount) before it is ever used
// as a make() length, closing the specific unbounded-allocation/OOM-kill
// finding that check defends against. recoverZFSCall stays anyway, as
// defense-in-depth against any OTHER panic this package (or a future change
// to it) might raise - exactly this project's own established posture
// toward any data read from a source outside this process's own control
// (disk/FAT/initrd/kernel-image parsing elsewhere in this repo all get the
// same treatment). It does not, on its own, protect against a large-but-
// non-panicking allocation attempt triggering a Linux OOM-kill (categorically
// outside what any recover() can catch) - that protection is checkElemCount's
// job, not this wrapper's.
func recoverZFSCall[T any](fn func() (T, error)) (result T, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in internal/zfsnative (likely a malformed/corrupted kernel response - see zfsnative.go's own recoverZFSCall doc comment): %v", r)
		}
	}()
	return fn()
}

// liveZFSPoolNames opens /dev/zfs and lists every currently-imported pool's
// name, via ZFS_IOC_POOL_CONFIGS (internal/zfsnative's own PoolNames(), a
// thin convenience over PoolConfigs()'s own keys).
func liveZFSPoolNames() ([]string, error) {
	h, err := zfs.Open()
	if err != nil {
		return nil, fmt.Errorf("opening /dev/zfs: %w", err)
	}
	defer h.Close()
	names, err := recoverZFSCall(h.PoolNames)
	if err != nil {
		return nil, fmt.Errorf("ZFS_IOC_POOL_CONFIGS: %w", err)
	}
	return names, nil
}

// liveZFSFeatureStats opens /dev/zfs and returns pool's own "feature_stats"
// nvlist via ZFS_IOC_POOL_STATS (internal/zfsnative's own PoolStats()) - NOT
// ZFS_IOC_POOL_GET_PROPS, which this project's own upstream source tracing
// confirmed does not carry feature state at all (see this file's own
// header comment and the hardening ledger's own entry for that
// investigation).
func liveZFSFeatureStats(pool string) (zfs.Nvlist, error) {
	h, err := zfs.Open()
	if err != nil {
		return nil, fmt.Errorf("opening /dev/zfs: %w", err)
	}
	defer h.Close()
	cfg, err := recoverZFSCall(func() (zfs.Nvlist, error) { return h.PoolStats(pool) })
	if err != nil {
		return nil, fmt.Errorf("ZFS_IOC_POOL_STATS %q: %w", pool, err)
	}
	fs, ok := cfg["feature_stats"]
	if !ok {
		return nil, fmt.Errorf("pool %q's own ZFS_IOC_POOL_STATS config has no \"feature_stats\" key - unexpected kernel response shape", pool)
	}
	nv, ok := fs.(zfs.Nvlist)
	if !ok {
		return nil, fmt.Errorf("pool %q's own \"feature_stats\" has Go type %T, want zfs.Nvlist (a nested nvlist)", pool, fs)
	}
	return nv, nil
}
