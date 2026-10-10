package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unidoc/alpine-zfsboot/internal/layout"
	"github.com/unidoc/alpine-zfsboot/internal/payloadsum"
)

// buildFakeBzImage is buildFakeKernelBytes' x86 counterpart: a bzImage
// setup header (what stage2 and internal/payloadsum need) with the same
// "Linux version" banner kernelinfo finds further in.
func buildFakeBzImage(version string, fill byte) []byte {
	k := bytes.Repeat([]byte{fill}, 16384)
	k[0x1f1] = 3
	k[0x1fe], k[0x1ff] = 0x55, 0xAA
	copy(k[0x202:], "HdrS")
	copy(k[6000:], "Linux version "+version+" (build@host) #1 SMP\n")
	return k
}

const testCmdline = "root=ZFS=zroot/ROOT/default ro alpine-zfsboot.buildstamp=20260923T150000Z alpine-zfsboot.version=0.1.0\n"

func readSum(t *testing.T, mountpoint string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(mountpoint, layout.PayloadSumFile))
	if err != nil {
		t.Fatalf("reading %s: %v", layout.PayloadSumFile, err)
	}
	return raw
}

func TestWritePayloadWithRollback_WritesPayloadSum(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)
	kernel := buildFakeBzImage("6.18.53-0-lts", 0x11)
	initrd := buildFakeInitrdBytes(t, "2.4.4-1")

	// A stale manifest from an earlier generation must be replaced.
	if err := os.WriteFile(filepath.Join(mountpoint, layout.PayloadSumFile), []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writePayloadWithRollback(mountpoint, "x86_64", "0.1.0", "20260923T150000Z", kernel, initrd, []byte(testCmdline), "warn"); err != nil {
		t.Fatalf("writePayloadWithRollback: %v", err)
	}
	m, err := payloadsum.Decode(readSum(t, mountpoint))
	if err != nil {
		t.Fatalf("decoding the written CHECKSUM: %v", err)
	}
	if errs := payloadsum.Verify(m, kernel, initrd); len(errs) != 0 {
		t.Fatalf("written CHECKSUM does not describe the written payload: %v", errs)
	}
	if present, mode, err := inspectPayloadSum(mountpoint); !present || err != nil || mode != "warn" {
		t.Fatalf("inspectPayloadSum after a good write: present=%v mode=%q err=%v", present, mode, err)
	}
}

func TestWritePayloadWithRollback_NonX86WritesNoPayloadSum(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)
	if err := writePayloadWithRollback(mountpoint, "aarch64", "0.1.0", "20260923T150000Z",
		buildFakeKernelBytes("6.18.53-0-lts"), buildFakeInitrdBytes(t, "2.4.4-1"), []byte(testCmdline), "warn"); err != nil {
		t.Fatalf("writePayloadWithRollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mountpoint, layout.PayloadSumFile)); !os.IsNotExist(err) {
		t.Fatalf("aarch64 has no BIOS stage2: want no CHECKSUM, got err=%v", err)
	}
}

func TestWritePayloadWithRollback_X86NonBzImageRefusedBeforeWriting(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)
	err := writePayloadWithRollback(mountpoint, "x86_64", "0.1.0", "20260923T150000Z",
		buildFakeKernelBytes("6.18.53-0-lts"), buildFakeInitrdBytes(t, "2.4.4-1"), []byte(testCmdline), "warn")
	if err == nil {
		t.Fatal("an x86_64 kernel that is not a bzImage: want an error")
	}
	if _, statErr := os.Stat(filepath.Join(mountpoint, layout.KernelFile)); !os.IsNotExist(statErr) {
		t.Fatal("KERNEL was written although the payload could never be booted by stage2")
	}
}

// The old generation's CHECKSUM must come back together with the old
// payload when the new generation fails half-way - and never be left
// describing files that are not there.
func TestWritePayloadWithRollback_FailureRestoresOldPayloadSum(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)
	oldKernel := buildFakeBzImage("6.18.0-old", 0x22)
	oldInitrd := buildFakeInitrdBytes(t, "2.3.0-old")
	if err := writePayloadWithRollback(mountpoint, "x86_64", "0.1.0", "20260923T150000Z", oldKernel, oldInitrd, []byte(testCmdline), "warn"); err != nil {
		t.Fatal(err)
	}
	oldSum := readSum(t, mountpoint)

	// Metadata generation fails on this initrd after KERNEL/INITRD/CMDLINE were written.
	err := writePayloadWithRollback(mountpoint, "x86_64", "0.1.0", "20260923T150000Z",
		buildFakeBzImage("6.18.53-0-lts", 0x33), []byte("not a real initrd at all"), []byte(testCmdline), "warn")
	if err == nil {
		t.Fatal("want an error from metadata generation")
	}
	if got := readSum(t, mountpoint); !bytes.Equal(got, oldSum) {
		t.Fatalf("CHECKSUM after rollback = %q, want the old generation's %q", got, oldSum)
	}
	m, _ := payloadsum.Decode(oldSum)
	gotKernel, _ := os.ReadFile(filepath.Join(mountpoint, layout.KernelFile))
	gotInitrd, _ := os.ReadFile(filepath.Join(mountpoint, layout.InitrdFile))
	if errs := payloadsum.Verify(m, gotKernel, gotInitrd); len(errs) != 0 {
		t.Fatalf("after rollback, CHECKSUM and the payload disagree: %v", errs)
	}
}

func TestVerifyPayloadSum(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)
	kernel := buildFakeBzImage("6.18.53-0-lts", 0x44)
	initrd := buildFakeInitrdBytes(t, "2.4.4-1")
	if err := writePayloadWithRollback(mountpoint, "x86_64", "0.1.0", "20260923T150000Z", kernel, initrd, []byte(testCmdline), "warn"); err != nil {
		t.Fatal(err)
	}
	tgt := &target{arch: "x86_64", mountpoint: mountpoint}
	// check runs what verify runs for CHECKSUM and returns what would make
	// verify exit non-zero.
	check := func(repair bool) (report, []error) {
		var r report
		r.payloadSumPresent, r.payloadSumMode, r.payloadSumErr = inspectPayloadSum(mountpoint)
		errs := verifyPayloadSum(tgt, &r, repair)
		return r, append(r.errs(), errs...)
	}
	writeKernel := func(b []byte) {
		if err := os.WriteFile(filepath.Join(mountpoint, layout.KernelFile), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if r, errs := check(false); len(errs) != 0 || !r.payloadSumVerified || r.payloadSumMode != "warn" {
		t.Fatalf("intact payload: errs=%v verified=%v mode=%q", errs, r.payloadSumVerified, r.payloadSumMode)
	}

	// One flipped byte in the part stage2 loads: mode warn -> a warning,
	// verify does not fail on it (the loader boots anyway).
	bad := append([]byte(nil), kernel...)
	bad[9000] ^= 0x01
	writeKernel(bad)
	if r, errs := check(false); len(errs) != 0 || !r.payloadSumWarn {
		t.Fatalf("corrupted kernel byte, mode warn: want a warning and no failure, got errs=%v warn=%v", errs, r.payloadSumWarn)
	}
	// ...mode enforce -> a verify failure (the loader would refuse it).
	if err := setIntegrityMode(tgt, "enforce"); err != nil {
		t.Fatal(err)
	}
	if _, errs := check(false); len(errs) != 1 || !strings.Contains(errs[0].Error(), "KERNEL SHA-256") {
		t.Fatalf("corrupted kernel byte, mode enforce: want one KERNEL SHA-256 error, got %v", errs)
	}

	// A different size is caught without hashing (status' check): a failure
	// only in mode enforce.
	writeKernel(append(kernel, 0))
	if present, mode, err := inspectPayloadSum(mountpoint); !present || mode != "enforce" || err == nil || !strings.Contains(err.Error(), "refuse to boot") {
		t.Fatalf("wrong kernel size, enforce: present=%v mode=%q err=%v", present, mode, err)
	}
	if err := setIntegrityMode(tgt, "warn"); err != nil {
		t.Fatal(err)
	}
	if _, errs := check(false); len(errs) != 0 {
		t.Fatalf("wrong kernel size, mode warn: verify must not fail, got %v", errs)
	}

	// Missing (an older install): not an error; --repair writes it, mode warn.
	writeKernel(kernel)
	if err := os.Remove(filepath.Join(mountpoint, layout.PayloadSumFile)); err != nil {
		t.Fatal(err)
	}
	if r, errs := check(false); len(errs) != 0 || r.payloadSumPresent {
		t.Fatalf("missing CHECKSUM: want no error and not present, got errs=%v present=%v", errs, r.payloadSumPresent)
	}
	if _, errs := check(true); len(errs) != 0 {
		t.Fatalf("--repair: %v", errs)
	}
	m, err := payloadsum.Decode(readSum(t, mountpoint))
	if err != nil || len(payloadsum.Verify(m, kernel, initrd)) != 0 || m.Mode != "warn" {
		t.Fatalf("--repair wrote a manifest that does not match the payload, or not mode warn: %v %q", err, m.Mode)
	}

	// Present but undecodable: a warning (the loader boots without
	// checking), not a verify failure; --repair leaves it alone.
	if err := os.WriteFile(filepath.Join(mountpoint, layout.PayloadSumFile), []byte("junk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r, errs := check(true); len(errs) != 0 || r.payloadSumErr == nil {
		t.Fatalf("undecodable CHECKSUM: want a warning, no failure; got errs=%v err=%v", errs, r.payloadSumErr)
	}
	if got := readSum(t, mountpoint); string(got) != "junk\n" {
		t.Fatal("--repair replaced a present-but-broken CHECKSUM")
	}
}

func TestSetIntegrityMode(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)
	kernel := buildFakeBzImage("6.18.53-0-lts", 0x55)
	initrd := buildFakeInitrdBytes(t, "2.4.4-1")
	if err := writePayloadWithRollback(mountpoint, "x86_64", "0.1.0", "20260923T150000Z", kernel, initrd, []byte(testCmdline), "warn"); err != nil {
		t.Fatal(err)
	}
	tgt := &target{arch: "x86_64", mountpoint: mountpoint}
	for _, mode := range []string{"enforce", "off", "warn"} {
		if err := setIntegrityMode(tgt, mode); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		m, err := payloadsum.Decode(readSum(t, mountpoint))
		if err != nil || m.Mode != mode || len(payloadsum.Verify(m, kernel, initrd)) != 0 {
			t.Fatalf("%s: decoded mode %q err %v, or the digests changed", mode, m.Mode, err)
		}
	}
	if err := setIntegrityMode(tgt, "strict"); err == nil {
		t.Fatal("an unknown mode: want an error")
	}
	// A stale manifest only gets its MODE changed - never re-blessed.
	stale := append([]byte(nil), kernel...)
	stale[9000] ^= 1
	if err := os.WriteFile(filepath.Join(mountpoint, layout.KernelFile), stale, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setIntegrityMode(tgt, "enforce"); err != nil {
		t.Fatal(err)
	}
	m, _ := payloadsum.Decode(readSum(t, mountpoint))
	if len(payloadsum.Verify(m, stale, initrd)) == 0 {
		t.Fatal("setIntegrityMode re-blessed a changed kernel")
	}
	// Missing: created from the installed payload with the requested mode.
	if err := os.Remove(filepath.Join(mountpoint, layout.PayloadSumFile)); err != nil {
		t.Fatal(err)
	}
	if err := setIntegrityMode(tgt, "off"); err != nil {
		t.Fatal(err)
	}
	if m, err := payloadsum.Decode(readSum(t, mountpoint)); err != nil || m.Mode != "off" {
		t.Fatalf("created manifest: mode %q err %v", m.Mode, err)
	}
	// Undecodable: refused, left as is.
	if err := os.WriteFile(filepath.Join(mountpoint, layout.PayloadSumFile), []byte("junk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setIntegrityMode(tgt, "warn"); err == nil {
		t.Fatal("an undecodable CHECKSUM: want an error")
	}
}

// The tool writes the file stage2 reads.
func TestPayloadSumPathMatchesStage2(t *testing.T) {
	h, err := os.ReadFile("../../bios/payload_sum.h")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(h), `#define PAYLOAD_SUM_PATH "/`+layout.PayloadSumFile+`"`) {
		t.Fatalf("bios/payload_sum.h's PAYLOAD_SUM_PATH is not /%s", layout.PayloadSumFile)
	}
}

func TestUpdateIntegrityMode(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)
	if got := updateIntegrityMode(mountpoint, biosOpts{integrity: "warn"}); got != "warn" {
		t.Fatalf("no CHECKSUM, no --integrity: %q, want warn", got)
	}
	kernel := buildFakeBzImage("6.18.53-0-lts", 0x66)
	initrd := buildFakeInitrdBytes(t, "2.4.4-1")
	if err := writePayloadWithRollback(mountpoint, "x86_64", "0.1.0", "20260923T150000Z", kernel, initrd, []byte(testCmdline), "off"); err != nil {
		t.Fatal(err)
	}
	if got := updateIntegrityMode(mountpoint, biosOpts{integrity: "warn"}); got != "off" {
		t.Fatalf("installed mode off, no --integrity: update would write %q, want off (kept)", got)
	}
	if got := updateIntegrityMode(mountpoint, biosOpts{integrity: "enforce", integritySet: true}); got != "enforce" {
		t.Fatalf("explicit --integrity=enforce: %q", got)
	}
}

func TestCmdlineIntegrity(t *testing.T) {
	for in, want := range map[string]string{
		"root=x alpine-zfsboot.integrity=enforce quiet": "enforce",
		"alpine-zfsboot.integrity=off":                  "off",
		"alpine-zfsboot.integrity=strict":               "",
		"root=x":                                        "",
	} {
		if got := cmdlineIntegrity(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
