// Original work: SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, go-fsctl
//
// Modifications: Copyright (c) 2026, UniDoc
// See abi.go for this package's full provenance note.

//go:build linux

package zfsnative

import "os"

// Indirection seams over the operating-system and ioctl primitives this
// package drives. They exist so the success and error branches of every
// kernel call — which only trigger against a live /dev/zfs that is
// impractical to provoke in CI — can be exercised deterministically by
// fault-injecting fakes in tests, without a real device or root. Production
// code uses the real implementations assigned here; tests swap a var, run,
// and restore it.
var (
	osOpenFile = os.OpenFile

	// ioctlFn is the single choke point every ZFS_IOC_* call funnels through
	// (h.ioctl delegates to it). A fake can both return any errno AND mutate
	// the supplied *zfsCmd buffer to emulate the kernel's write-back (the
	// decoded dst nvlist), making the whole package coverable purely with
	// fakes.
	ioctlFn = realIoctl
)

// dstHook, when non-nil, is invoked with the freshly-allocated zc_nvlist_dst
// buffer at each call site that issues a dst-returning ioctl. It is nil in
// production (zero cost); tests set it so a fake ioctlFn can emulate the
// kernel's nvlist write-back into the very slice the caller will decode,
// without reconstructing a pointer from the packed zfs_cmd_t (which would be
// an unsafe uintptr round-trip).
var dstHook func([]byte)

func noteDst(dst []byte) {
	if dstHook != nil {
		dstHook(dst)
	}
}
