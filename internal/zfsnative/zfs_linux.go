// Original work: SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, go-fsctl
//
// Modifications: Copyright (c) 2026, UniDoc
// See abi.go for this package's full provenance note.

//go:build linux

package zfsnative

import "fmt"

// PoolConfigs lists the currently imported pools by issuing
// ZFS_IOC_POOL_CONFIGS and decoding the returned nvlist. The result maps each
// pool name to its configuration nvlist (the same nvlist `zpool` consults).
//
// No pool name is supplied in zc_name; the kernel packs every imported
// pool's config into zc_nvlist_dst.
func (h *Handle) PoolConfigs() (map[string]Nvlist, error) {
	nv, err := h.callWithDst(ZFS_IOC_POOL_CONFIGS, func(c *zfsCmd) error {
		// zc_cookie carries the generation count the caller last saw; 0
		// always returns the current set.
		c.setU64(offZcCookie, 0)
		return nil
	}, 64*1024)
	if err != nil {
		return nil, fmt.Errorf("ZFS_IOC_POOL_CONFIGS: %w", err)
	}
	out := make(map[string]Nvlist, len(nv))
	for name, v := range nv {
		sub, ok := v.(Nvlist)
		if !ok {
			// Fail closed rather than silently dropping the pool: every
			// real ZFS_IOC_POOL_CONFIGS top-level entry is itself a config
			// nvlist. A non-Nvlist value here means either a genuinely
			// malformed kernel response, or this project's own assumption
			// about the response shape is wrong - both are exactly the
			// kind of "found nothing wrong because we didn't look at
			// everything" trap this project's other fail-closed checks
			// (parseFeatureStats' own unknown-GUID handling, in
			// cmd/tool/zfsnative.go) already guard against; a pool
			// silently disappearing from PoolNames() because its own
			// entry didn't decode as expected is strictly worse than
			// reporting an error naming exactly which pool and why.
			return nil, fmt.Errorf("ZFS_IOC_POOL_CONFIGS: pool %q's own top-level entry has Go type %T, want Nvlist - unexpected kernel response shape", name, v)
		}
		out[name] = sub
	}
	return out, nil
}

// PoolNames is a convenience wrapper returning just the imported pool names.
func (h *Handle) PoolNames() ([]string, error) {
	cfgs, err := h.PoolConfigs()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(cfgs))
	for n := range cfgs {
		names = append(names, n)
	}
	return names, nil
}
