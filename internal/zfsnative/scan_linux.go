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
//
// zc_cookie, read back after a successful ioctl (F20, unidoc-alip's PR
// #5 review - real libzfs checks this same field the same way, e.g.
// zpool_refresh_stats()): the ioctl itself can succeed (a config comes
// back) while zc_cookie still carries a real, nonzero errno from
// spa_get_stats()'s own attempt to actually open/scan the pool - a
// degraded or otherwise-troubled pool can report a config this way
// with no indication anything was wrong, previously silently ignored
// here entirely.
func (h *Handle) PoolStats(pool string) (Nvlist, error) {
	nv, cookie, err := h.callWithDst(ZFS_IOC_POOL_STATS, func(c *zfsCmd) error {
		return c.setName(pool)
	}, 256*1024)
	if err != nil {
		return nil, fmt.Errorf("ZFS_IOC_POOL_STATS %q: %w", pool, err)
	}
	if cookie != 0 {
		return nil, fmt.Errorf("ZFS_IOC_POOL_STATS %q: the kernel reported a config, but zc_cookie=%d (errno) - the pool itself had trouble opening/scanning, so this config should not be trusted", pool, cookie)
	}
	return nv, nil
}
