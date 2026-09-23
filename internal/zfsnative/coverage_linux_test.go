// Original work: SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, go-fsctl
//
// Modifications: Copyright (c) 2026, UniDoc
// See abi.go for this package's full provenance note.
//
// This file drives every branch of the trimmed zfs_linux.go/scan_linux.go/
// cmd_linux.go kernel paths through the ioctlFn/osOpenFile/dstHook seams in
// seams_linux.go, fault-injecting both success and each errno WITHOUT a real
// /dev/zfs, a zfs module, or root.

package zfsnative

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

var errInjected = errors.New("injected")

// lastDst is the most-recent zc_nvlist_dst buffer captured via the dstHook
// seam. A fake ioctlFn writes the emulated kernel nvlist into it (no unsafe
// pointer round-trip needed; dstHook hands us the real slice the caller will
// decode). Tests run serially so a package var is safe.
var lastDst []byte

// snapshotSeams captures the production seam values and returns a restore func.
func snapshotSeams() func() {
	a, c, d := osOpenFile, ioctlFn, dstHook
	dstHook = func(dst []byte) { lastDst = dst }
	return func() {
		osOpenFile, ioctlFn, dstHook = a, c, d
		lastDst = nil
	}
}

// putDst encodes nv and writes it into the caller's captured dst buffer.
// Fails the test if the buffer is too small (the test should size it
// adequately) or none was captured.
func putDst(t *testing.T, nv Nvlist) {
	t.Helper()
	b, err := EncodeNative(nv)
	if err != nil {
		t.Fatalf("putDst encode: %v", err)
	}
	if lastDst == nil {
		t.Fatal("putDst: no dst buffer captured (dstHook not invoked)")
	}
	if len(b) > len(lastDst) {
		t.Fatalf("putDst: encoded %d bytes > dst %d", len(b), len(lastDst))
	}
	copy(lastDst, b)
}

// okHandle is a Handle usable with a fake ioctlFn (h.f is never touched
// because the fake replaces realIoctl).
func okHandle() *Handle { return &Handle{} }

// ---- Open / Close ----

func TestOpenAndClose(t *testing.T) {
	defer snapshotSeams()()

	osOpenFile = func(string, int, os.FileMode) (*os.File, error) { return nil, errInjected }
	if _, err := Open(); err == nil {
		t.Fatal("want open error")
	}

	// Success: hand back a real temp file so Close() works.
	f, err := os.CreateTemp(t.TempDir(), "zfs")
	if err != nil {
		t.Fatal(err)
	}
	osOpenFile = func(string, int, os.FileMode) (*os.File, error) { return f, nil }
	h, err := Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// ---- callWithDst (PoolConfigs / PoolNames / PoolStats) ----

func TestPoolConfigsAndNames(t *testing.T) {
	defer snapshotSeams()()

	ioctlFn = func(*Handle, uintptr, *zfsCmd) error { return errInjected }
	if _, err := okHandle().PoolConfigs(); err == nil {
		t.Fatal("want ioctl error")
	}
	if _, err := okHandle().PoolNames(); err == nil {
		t.Fatal("want PoolNames error")
	}

	// Success: kernel returns exactly the real shape - every top-level
	// entry is itself a config Nvlist.
	ioctlFn = func(_ *Handle, _ uintptr, cmd *zfsCmd) error {
		putDst(t, Nvlist{
			"tank": Nvlist{"pool_guid": uint64(0x1234)},
		})
		return nil
	}
	cfgs, err := okHandle().PoolConfigs()
	if err != nil {
		t.Fatalf("PoolConfigs: %v", err)
	}
	if _, ok := cfgs["tank"]; !ok {
		t.Fatalf("missing tank config: %v", cfgs)
	}
	names, err := okHandle().PoolNames()
	if err != nil || len(names) != 1 || names[0] != "tank" {
		t.Fatalf("PoolNames = %v, %v", names, err)
	}
}

// TestPoolConfigsFailsClosedOnNonNvlistEntry is the regression test for
// PoolConfigs' fail-closed handling of a malformed top-level entry: every
// real ZFS_IOC_POOL_CONFIGS response has ONLY Nvlist values at the top
// level (one per imported pool). A scalar (or any other non-Nvlist value)
// there is unexpected kernel response shape, not a pool to silently omit -
// silently dropping it previously meant a malformed/partial kernel
// response could make PoolNames() under-report the real imported set with
// no error at all.
func TestPoolConfigsFailsClosedOnNonNvlistEntry(t *testing.T) {
	defer snapshotSeams()()
	ioctlFn = func(_ *Handle, _ uintptr, cmd *zfsCmd) error {
		putDst(t, Nvlist{
			"tank": Nvlist{"pool_guid": uint64(0x1234)},
			"junk": uint64(7), // malformed: not an Nvlist
		})
		return nil
	}
	if _, err := okHandle().PoolConfigs(); err == nil {
		t.Fatal("PoolConfigs with a non-Nvlist top-level entry: want an error, got nil")
	}
	if _, err := okHandle().PoolNames(); err == nil {
		t.Fatal("PoolNames with a non-Nvlist top-level entry: want an error, got nil")
	}
}

func TestPoolStats(t *testing.T) {
	defer snapshotSeams()()

	ioctlFn = func(*Handle, uintptr, *zfsCmd) error { return errInjected }
	if _, err := okHandle().PoolStats("tank"); err == nil {
		t.Fatal("want ioctl error")
	}

	ioctlFn = func(_ *Handle, _ uintptr, cmd *zfsCmd) error {
		putDst(t, Nvlist{
			"feature_stats": Nvlist{"org.openzfs:large_blocks": uint64(1)},
		})
		return nil
	}
	nv, err := okHandle().PoolStats("tank")
	if err != nil {
		t.Fatalf("PoolStats: %v", err)
	}
	if _, ok := nv["feature_stats"]; !ok {
		t.Fatalf("missing feature_stats: %v", nv)
	}
}

// TestPoolStats_NonzeroZcCookieRefused is the regression test for F20
// (unidoc-alip's PR #5 review): a successful ZFS_IOC_POOL_STATS ioctl
// (a config genuinely comes back) can still leave zc_cookie holding a
// real, nonzero errno from the kernel's own attempt to open/scan the
// pool - real libzfs checks this same field the same way. Before this
// fix, PoolStats never read it back at all, so a troubled pool's
// config was trusted exactly like a clean one.
func TestPoolStats_NonzeroZcCookieRefused(t *testing.T) {
	defer snapshotSeams()()

	ioctlFn = func(_ *Handle, _ uintptr, cmd *zfsCmd) error {
		putDst(t, Nvlist{
			"feature_stats": Nvlist{"org.openzfs:large_blocks": uint64(1)},
		})
		cmd.setU64(offZcCookie, 5) // EIO, say - the pool had real trouble opening
		return nil
	}
	if _, err := okHandle().PoolStats("tank"); err == nil {
		t.Fatal("PoolStats with a nonzero post-call zc_cookie: want an error, got nil - a troubled pool's config was trusted as clean")
	}
}

func TestCallWithDstEnomemGrowThenSucceed(t *testing.T) {
	defer snapshotSeams()()
	calls := 0
	ioctlFn = func(_ *Handle, _ uintptr, cmd *zfsCmd) error {
		calls++
		if calls == 1 {
			// Report a needed size larger than current to trigger one retry.
			cmd.setU64(offZcNvlistDstSize, 512*1024)
			return unix.ENOMEM
		}
		putDst(t, Nvlist{"a": uint64(1)})
		return nil
	}
	if _, err := okHandle().PoolStats("tank"); err != nil {
		t.Fatalf("PoolStats: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (grow then succeed)", calls)
	}
}

func TestCallWithDstEnomemNoGrowReturnsError(t *testing.T) {
	defer snapshotSeams()()
	// ENOMEM but the reported need is NOT larger than dstSize: the loop falls
	// through and returns the ENOMEM error.
	ioctlFn = func(_ *Handle, _ uintptr, cmd *zfsCmd) error {
		cmd.setU64(offZcNvlistDstSize, 0) // need == 0, not > dstSize
		return unix.ENOMEM
	}
	if _, err := okHandle().PoolStats("tank"); err == nil {
		t.Fatal("want ENOMEM surfaced")
	}
}

// TestCallWithDstEnomemAboveGrowCeilingIsRefused is the regression test for
// maxDstGrowSize: a kernel-reported need at or above the ceiling must NOT
// be trusted for a retry allocation - the exact defensive bound closing the
// enabler an independent review found for a real stack-overflow finding
// elsewhere in this package (a huge kernel-directed dst buffer made deep
// nvlist nesting cheap to reach; see maxNVListDepth's own doc comment).
func TestCallWithDstEnomemAboveGrowCeilingIsRefused(t *testing.T) {
	defer snapshotSeams()()
	calls := 0
	ioctlFn = func(_ *Handle, _ uintptr, cmd *zfsCmd) error {
		calls++
		cmd.setU64(offZcNvlistDstSize, maxDstGrowSize+1) // at/above the ceiling
		return unix.ENOMEM
	}
	if _, err := okHandle().PoolStats("tank"); err == nil {
		t.Fatal("want the ENOMEM error surfaced, not a retry at an above-ceiling size")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (no retry attempted above the ceiling)", calls)
	}
}

func TestCallWithDstBuildError(t *testing.T) {
	defer snapshotSeams()()
	ioctlFn = func(*Handle, uintptr, *zfsCmd) error { return nil }
	// A name longer than MAXPATHLEN makes the build callback (setName) fail.
	long := string(make([]byte, maxPathLen))
	if _, err := okHandle().PoolStats(long); err == nil {
		t.Fatal("want build (setName) error")
	}
}

func TestCallWithDstKeptGrowing(t *testing.T) {
	defer snapshotSeams()()
	// Every attempt reports a larger need than before: after the 2-attempt
	// budget, callWithDst must give up with its own error rather than loop
	// forever.
	need := uint64(128 * 1024)
	ioctlFn = func(_ *Handle, _ uintptr, cmd *zfsCmd) error {
		need *= 2
		cmd.setU64(offZcNvlistDstSize, need)
		return unix.ENOMEM
	}
	if _, err := okHandle().PoolStats("tank"); err == nil {
		t.Fatal("want error after repeated growth")
	}
}

// TestRealIoctlSeam covers realIoctl's errno branch (unconditionally, any
// environment) and its success branch (best-effort — see below) with a
// benign ioctl on a regular file fd. This is the production ioctlFn
// closure: a 6-line passthrough to unix.Syscall, so nothing here is
// environment-dependent in realIoctl's OWN logic — only the kernel's
// response to FIGETBSZ on a regular file is.
//
// On a real Linux kernel, FIGETBSZ (request 0x2, include/uapi/linux/fs.h)
// on a regular file succeeds at the VFS layer regardless of filesystem.
// Upstream's own version of this test documents ENOTTY as a known
// exception under QEMU user-mode emulation. Verifying this fork's copy in
// this project's own development sandbox surfaced a THIRD outcome, EINVAL
// — neither of the above, and consistent with a syscall-intercepting
// sandbox (e.g. gVisor) that answers unimplemented/filtered ioctls with
// EINVAL rather than ENOTTY, distinct from both a genuine kernel and QEMU
// user-mode emulation. Rather than pick one specific errno to assert (which
// would just relocate the same fragility) or silently drop the assertion
// (which would hide a real regression in realIoctl itself), this test
// tolerates the two KNOWN non-kernel outcomes explicitly and fails loudly,
// naming the exact errno, on anything else — so a real CI runner (a genuine
// kernel, where success is expected) still catches an actual bug.
func TestRealIoctlSeam(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "fd")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := &Handle{f: f}

	cmd := &zfsCmd{}
	switch err := realIoctl(h, 0x2 /* FIGETBSZ */, cmd); err {
	case nil:
		// Real kernel: success is the expected, asserted outcome.
	case unix.ENOTTY, unix.EINVAL:
		t.Logf("FIGETBSZ returned %v on this environment's ioctl handling (known non-kernel sandbox behavior, not a realIoctl bug) — success branch not exercised here", err)
	default:
		t.Fatalf("FIGETBSZ on a regular file: unexpected errno %v (want nil, ENOTTY, or EINVAL)", err)
	}

	// Error: a bogus request number yields an errno, regardless of
	// environment. This is the branch that actually proves realIoctl
	// propagates a kernel errno instead of swallowing it.
	if err := realIoctl(h, 0xDEAD, &zfsCmd{}); err == nil {
		t.Fatal("want errno from bogus ioctl request")
	}
}
