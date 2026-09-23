// Original work: SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, go-fsctl
//
// Modifications: Copyright (c) 2026, UniDoc
// See abi.go for this package's full provenance note.

//go:build linux

package zfsnative

import "fmt"

// PoolStats issues ZFS_IOC_POOL_STATS for pool and returns the kernel's freshly
// generated config nvlist. Unlike the cached config from ZFS_IOC_POOL_CONFIGS,
// this config is produced by spa_get_stats -> spa_config_generate over the live
// root vdev, so it carries the dynamic "feature_stats" nvlist — module/zfs/
// spa.c's spa_add_feature_stats() — that alpine-zfsboot's own boot-compat
// verdict is built from (see cmd/tool/zfsnative.go). zc_name carries the
// pool; the config comes back in zc_nvlist_dst.
func (h *Handle) PoolStats(pool string) (Nvlist, error) {
	nv, err := h.callWithDst(ZFS_IOC_POOL_STATS, func(c *zfsCmd) error {
		return c.setName(pool)
	}, 256*1024)
	if err != nil {
		return nil, fmt.Errorf("ZFS_IOC_POOL_STATS %q: %w", pool, err)
	}
	return nv, nil
}
