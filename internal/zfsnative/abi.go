// Original work: SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, go-fsctl
//
// Modifications: Copyright (c) 2026, UniDoc
//
// PROVENANCE: this package is a trimmed, first-party copy of the
// reachable subset of github.com/go-fsctl/zfs (upstream commit
// 555236126cd6ac4b8c983a62f92a98837ae9728a, 2026-09-09,
// https://github.com/go-fsctl/zfs), a pure-Go /dev/zfs ioctl client
// (BSD-3-Clause, no cgo, no libzfs). It was internalized rather than kept
// as an external module dependency after a supply-chain/code audit found
// a real bug in the upstream nvlist decoder (see checkElemCount in
// nvlist.go) and the project's own single-maintainer, zero-tagged-release
// status made pinning an external commit a weaker guarantee than owning
// the code directly (see temp/hardening-ledger.md for the full audit).
//
// WHAT WAS KEPT: only the read-only call graph alpine-zfsboot actually
// uses - Open/Close, PoolConfigs, PoolNames, PoolStats, and the nvlist
// DECODER (not the encoder, which no reachable call needs - see
// nvlist.go). Every write-capable ZFS_IOC_* operation (create, destroy,
// scan/scrub/resilver, trim, snapshot, key management, send/recv, ...)
// was deliberately deleted, not merely left uncalled: this package
// structurally cannot mutate a pool, not just by convention.
//
// WHAT WAS MODIFIED: nvlist.go's decoder gained a bounds check
// (checkElemCount) against a real unbounded-allocation finding from the
// same audit - a corrupted/malformed array element count previously
// reached make() unchecked. This fix has also been proposed upstream
// (see temp/hardening-ledger.md); it is not upstream-dependent here.
//
// The ZFS_IOC_* ioctl ABI itself (not upstream's Go code) was
// independently re-verified against real OpenZFS kernel source at 5
// release tags spanning 2.0.0-2.4.0 as part of the same audit; see
// temp/hardening-ledger.md for that trace. Upstream's own comments below,
// citing the 2.2.2 headers, are retained as-is.

package zfsnative

import "unsafe"

// ptrOfUint16 is a tiny unsafe helper used for host-endianness detection in
// nvlist.go.
func ptrOfUint16(p *uint16) unsafe.Pointer { return unsafe.Pointer(p) }

// ZFS_IOC_* request numbers for OpenZFS on Linux.
//
// On Linux, OpenZFS registers /dev/zfs as a misc character device and uses the
// zfs_ioc_t enum value DIRECTLY as the ioctl request number (it is not
// _IOWR-encoded — the kernel's zfsdev_ioctl computes vecnum = cmd -
// ZFS_IOC_FIRST and dispatches). ZFS_IOC_FIRST = ('Z' << 8) = 0x5a00, and the
// enum is contiguous from there.
//
// Only the two read-only ioctls this package actually issues are kept here;
// see the package doc comment above for why the rest of the (much larger)
// upstream enum was deleted rather than merely left unreferenced.
const (
	zfsIocFirst = 'Z' << 8 // 0x5a00

	ZFS_IOC_POOL_CONFIGS = zfsIocFirst + 0x04 // 0x5a04
	ZFS_IOC_POOL_STATS   = zfsIocFirst + 0x05 // 0x5a05
)

// sizeofZfsCmd is sizeof(zfs_cmd_t) for OpenZFS 2.2.2 on a 64-bit kernel.
// Confirmed by compiling the exact struct (from the 2.2.2 source) in the
// upstream target guest: sizeof == 13744, with the field offsets recorded in
// the offsets below. zfsCmd (in cmd_linux.go) is sized to match exactly.
const sizeofZfsCmd = 13744

// Field byte offsets within zfs_cmd_t actually used by this package's
// read-only ioctls, captured from upstream's C ABI probe against the 2.2.2
// source headers. Used by cmd_linux.go to read/write the packed struct
// without relying on Go's struct layout for the large, alignment-sensitive
// trailing members.
const (
	offZcName          = 0
	offZcNvlistDst     = 4112
	offZcNvlistDstSize = 4120
	offZcCookie        = 12616
)

// maxPathLen is MAXPATHLEN (ZFS path-length constant from the 2.2.2 headers,
// Linux) — the size of the zc_name field pool names are written into.
const maxPathLen = 4096
