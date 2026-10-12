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

func bigBzImage(fill byte) []byte {
	k := buildFakeBzImage("6.18.53-0-lts", fill)
	return append(k, bytes.Repeat([]byte{fill ^ 0x5a}, 3*65536)...)
}

func TestWritePayloadWithRollback_WritesBlkSum(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)
	kernel, initrd := bigBzImage(0x11), buildFakeInitrdBytes(t, "2.4.4-1")
	if err := os.WriteFile(filepath.Join(mountpoint, layout.BlkSumFile), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writePayloadWithRollback(mountpoint, "x86_64", "0.1.0", "20260923T150000Z", kernel, initrd, []byte(testCmdline), "warn"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(mountpoint, layout.BlkSumFile))
	if err != nil {
		t.Fatal(err)
	}
	b, err := payloadsum.DecodeBlkSum(raw)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := payloadsum.Decode(readSum(t, mountpoint))
	if errs := payloadsum.VerifyBlkSum(b, m, kernel, initrd); len(errs) != 0 {
		t.Fatalf("written BLKSUM does not describe the written payload: %v", errs)
	}
	// aarch64: neither file.
	mp2 := newBIOSMountpoint(t)
	if err := writePayloadWithRollback(mp2, "aarch64", "0.1.0", "20260923T150000Z", buildFakeKernelBytes("6.18.53-0-lts"), initrd, []byte(testCmdline), "warn"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(mp2, layout.BlkSumFile)); !os.IsNotExist(err) {
		t.Fatal("aarch64: no BLKSUM expected")
	}
}

// A failed update restores the old pair; never CHECKSUM without its BLKSUM.
func TestWritePayloadWithRollback_FailureRestoresOldBlkSum(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)
	if err := writePayloadWithRollback(mountpoint, "x86_64", "0.1.0", "20260923T150000Z", bigBzImage(0x22), buildFakeInitrdBytes(t, "2.3.0-old"), []byte(testCmdline), "warn"); err != nil {
		t.Fatal(err)
	}
	oldBlk, _ := os.ReadFile(filepath.Join(mountpoint, layout.BlkSumFile))
	oldSum := readSum(t, mountpoint)
	if err := writePayloadWithRollback(mountpoint, "x86_64", "0.1.0", "20260923T150000Z", bigBzImage(0x33), []byte("not a real initrd at all"), []byte(testCmdline), "warn"); err == nil {
		t.Fatal("want an error")
	}
	gotBlk, _ := os.ReadFile(filepath.Join(mountpoint, layout.BlkSumFile))
	if !bytes.Equal(gotBlk, oldBlk) || !bytes.Equal(readSum(t, mountpoint), oldSum) {
		t.Fatal("rollback did not restore the old CHECKSUM/BLKSUM pair")
	}
}

func TestVerifyBlkSum(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)
	kernel, initrd := bigBzImage(0x44), buildFakeInitrdBytes(t, "2.4.4-1")
	if err := writePayloadWithRollback(mountpoint, "x86_64", "0.1.0", "20260923T150000Z", kernel, initrd, []byte(testCmdline), "warn"); err != nil {
		t.Fatal(err)
	}
	tgt := &target{arch: "x86_64", mountpoint: mountpoint}
	check := func(repair bool) (report, []error) {
		var r report
		r.payloadSumPresent, r.payloadSumMode, r.payloadSumErr = inspectPayloadSum(mountpoint)
		errs := verifyPayloadSum(tgt, &r, repair)
		return r, append(r.errs(), errs...)
	}
	if r, errs := check(false); len(errs) != 0 || !strings.HasPrefix(r.blkSum, "OK (v1, KERNEL 4 + INITRD") {
		t.Fatalf("intact: %v %q", errs, r.blkSum)
	}
	if s := inspectBlkSum(mountpoint); !strings.HasPrefix(s, "v1, KERNEL 4 + INITRD") {
		t.Fatalf("status line: %q", s)
	}
	// A damaged table: a warning in mode warn, a failure in enforce.
	blkPath := filepath.Join(mountpoint, layout.BlkSumFile)
	good, _ := os.ReadFile(blkPath)
	bad := append([]byte(nil), good...)
	bad[512+10] ^= 1
	if err := os.WriteFile(blkPath, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	if r, errs := check(false); len(errs) != 0 || !r.payloadSumWarn {
		t.Fatalf("damaged BLKSUM, warn: want a warning only, got %v", errs)
	}
	if err := setIntegrityMode(tgt, "enforce"); err != nil {
		t.Fatal(err)
	}
	if _, errs := check(false); len(errs) != 1 || !strings.Contains(errs[0].Error(), "fails its own SHA-256") {
		t.Fatalf("damaged BLKSUM, enforce: %v", errs)
	}
	// Missing (an install from the previous version): INFO; --repair adds it.
	if err := setIntegrityMode(tgt, "warn"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(blkPath); err != nil {
		t.Fatal(err)
	}
	if r, errs := check(false); len(errs) != 0 || !strings.HasPrefix(r.blkSum, "INFO: not present") {
		t.Fatalf("missing BLKSUM: %v %q", errs, r.blkSum)
	}
	if _, errs := check(true); len(errs) != 0 {
		t.Fatalf("--repair: %v", errs)
	}
	if got, _ := os.ReadFile(blkPath); !bytes.Equal(got, good) {
		t.Fatal("--repair did not write the same BLKSUM")
	}
}

func TestSetIntegrityModeCreatesBothFiles(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)
	kernel, initrd := bigBzImage(0x55), buildFakeInitrdBytes(t, "2.4.4-1")
	if err := os.WriteFile(filepath.Join(mountpoint, layout.KernelFile), kernel, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mountpoint, layout.InitrdFile), initrd, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mountpoint, layout.CmdlineFile), []byte(testCmdline), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setIntegrityMode(&target{arch: "x86_64", mountpoint: mountpoint}, "off"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(mountpoint, layout.BlkSumFile)); err != nil {
		t.Fatalf("BLKSUM not created: %v", err)
	}
}

func TestPayloadManifestFiles(t *testing.T) {
	dir := t.TempDir()
	kernel, initrd := bigBzImage(0x66), buildFakeInitrdBytes(t, "2.4.4-1")
	if err := writeManifestFiles(dir, kernel, initrd, "warn"); err != nil {
		t.Fatal(err)
	}
	want, _ := payloadsum.GenerateBlkSum(kernel, initrd)
	got, _ := os.ReadFile(filepath.Join(dir, "BLKSUM"))
	sum, _ := os.ReadFile(filepath.Join(dir, "CHECKSUM"))
	if !bytes.Equal(got, want) || !strings.Contains(string(sum), "MODE warn") {
		t.Fatal("payload-manifest output differs from the install writer's")
	}
	if err := writeManifestFiles(dir, kernel, initrd, "bogus"); err == nil {
		t.Fatal("an unknown mode: want an error")
	}
}

func TestBlkSumPathMatchesStage2(t *testing.T) {
	h, err := os.ReadFile("../../bios/blkverify.h")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(h), `#define BLKSUM_PATH "/`+layout.BlkSumFile+`"`) {
		t.Fatalf("bios/blkverify.h's BLKSUM_PATH is not /%s", layout.BlkSumFile)
	}
}
