// Original work: SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, go-fsctl
//
// Modifications: Copyright (c) 2026, UniDoc
// See abi.go for this package's full provenance note.

//go:build linux

package zfsnative

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// hostBO is the native byte order used for the zfs_cmd_t scalar fields (the
// kernel reads them in host-endian).
var hostBO = func() binary.ByteOrder {
	bo, _ := nvHostOrder()
	return bo
}()

// zfsCmd is the in-memory zfs_cmd_t, held as a raw byte buffer sized to
// sizeof(zfs_cmd_t) for OpenZFS 2.2.2 (13744 bytes). Scalar fields are read
// and written at the exact offsets captured from upstream's C ABI probe.
// Using a flat buffer (rather than a Go struct) sidesteps any divergence
// between Go's struct layout and the C ABI for the large alignment-sensitive
// trailing members this package does not otherwise touch.
type zfsCmd struct {
	buf [sizeofZfsCmd]byte
}

func (c *zfsCmd) setU64(off int, v uint64) {
	hostBO.PutUint64(c.buf[off:off+8], v)
}

func (c *zfsCmd) getU64(off int) uint64 {
	return hostBO.Uint64(c.buf[off : off+8])
}

// setName writes a NUL-terminated string into the zc_name[MAXPATHLEN] field.
func (c *zfsCmd) setName(s string) error {
	if len(s)+1 > maxPathLen {
		return fmt.Errorf("name too long: %d bytes", len(s))
	}
	copy(c.buf[offZcName:offZcName+maxPathLen], make([]byte, maxPathLen))
	copy(c.buf[offZcName:], s)
	return nil
}

// Handle is an open file descriptor to /dev/zfs.
type Handle struct {
	f *os.File
}

// Open opens /dev/zfs read-write (matching the kernel driver's own
// requirement — it rejects O_RDONLY opens outright), but this package never
// issues an ioctl that mutates pool or dataset state; see abi.go. Requires
// CAP_SYS_ADMIN (effectively root) for most ops, including the read-only
// ones this package uses.
func Open() (*Handle, error) {
	f, err := osOpenFile("/dev/zfs", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/zfs: %w", err)
	}
	return &Handle{f: f}, nil
}

// Close releases the /dev/zfs handle.
func (h *Handle) Close() error { return h.f.Close() }

// ioctl issues a single ZFS ioctl. cmd is mutated in place (the kernel writes
// back zc_nvlist_dst_size etc.). It funnels through the ioctlFn seam so
// tests can fault-inject any errno and emulate the kernel's buffer
// write-back.
func (h *Handle) ioctl(req uintptr, cmd *zfsCmd) error {
	return ioctlFn(h, req, cmd)
}

// realIoctl is the production ioctlFn seam: it performs the raw SYS_IOCTL
// syscall against /dev/zfs.
func realIoctl(h *Handle, req uintptr, cmd *zfsCmd) error {
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		h.f.Fd(),
		req,
		uintptr(unsafe.Pointer(&cmd.buf[0])),
	)
	runtime.KeepAlive(cmd)
	if errno != 0 {
		return errno
	}
	return nil
}

// callWithDst runs an ioctl that returns an nvlist in zc_nvlist_dst. It
// allocates a dst buffer of dstSize, retries once with the kernel-reported
// size if the buffer was too small (ENOMEM), and decodes the result.
// maxDstGrowSize bounds how large a kernel-REPORTED zc_nvlist_dst_size
// callWithDst will trust enough to actually allocate for a retry. The
// starting sizes (PoolConfigs 64KiB, PoolStats 256KiB) already cover every
// real config this project has observed (a real 46-feature pool's full
// PoolStats response, CAX, decoded within the starting buffer with no
// retry needed at all). 16MiB is a reasoned, deliberately generous ceiling
// - 64x the larger starting size - not a measurement against an actual
// enormous pool (none was available to test against): even a pool with
// hundreds of vdevs and every registered feature reporting would be
// expected to stay in the low single-digit MiB range, since the config's
// size scales with vdev/feature COUNT, not with pool capacity. This
// replaces a previous, far less defensible 1<<30 (1GiB) ceiling, which was
// large enough that a malformed/corrupted zc_nvlist_dst_size report from a
// buggy or incompatible kernel module could itself have caused an OOM-kill
// (the same class of unrecoverable failure - outside recover()'s reach -
// that motivated checkElemCount and maxNVListDepth elsewhere in this
// package) purely from this retry path, independent of anything the
// nvlist decoder itself does with the resulting buffer.
const maxDstGrowSize = 16 * 1024 * 1024

// The third return value is the ioctl's own zc_cookie field, exactly as
// the kernel left it after a SUCCESSFUL call - meaning varies per ioctl
// (PoolStats' own caller checks it as a real post-open errno; every other
// current caller ignores it) so interpreting it is each caller's own job,
// not this shared primitive's.
func (h *Handle) callWithDst(req uintptr, build func(*zfsCmd) error, dstSize uint64) (Nvlist, uint64, error) {
	for attempt := 0; attempt < 2; attempt++ {
		cmd := &zfsCmd{}
		if err := build(cmd); err != nil {
			return nil, 0, err
		}
		dst := make([]byte, dstSize)
		cmd.setU64(offZcNvlistDst, uint64(uintptr(unsafe.Pointer(&dst[0]))))
		cmd.setU64(offZcNvlistDstSize, dstSize)
		noteDst(dst)

		err := h.ioctl(req, cmd)
		runtime.KeepAlive(dst)
		if err == unix.ENOMEM {
			// Kernel wrote the required size into zc_nvlist_dst_size.
			need := cmd.getU64(offZcNvlistDstSize)
			if need > dstSize && need < maxDstGrowSize {
				dstSize = need
				continue
			}
		}
		if err != nil {
			return nil, 0, err
		}
		// The kernel may report the actual packed size; decode the whole
		// buffer (DecodeNative stops at the list terminator regardless).
		nv, err := DecodeNative(dst)
		if err != nil {
			return nil, 0, err
		}
		return nv, cmd.getU64(offZcCookie), nil
	}
	return nil, 0, fmt.Errorf("ioctl dst buffer kept growing")
}
