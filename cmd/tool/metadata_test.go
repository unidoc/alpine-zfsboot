package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unidoc/alpine-zfsboot/internal/layout"
	"github.com/unidoc/alpine-zfsboot/internal/metadata"
)

// buildFakeKernelBytes builds the simplest input kernelinfo.VersionFromImage
// actually accepts: a bare "Linux version ..." banner string, its own
// documented fallback path for anything that isn't a real bzImage/zboot
// header (see that package's own VersionFromLinuxBanner doc comment).
func buildFakeKernelBytes(version string) []byte {
	return []byte("Linux version " + version + " (build@host) #1 SMP\n")
}

// appendCPIOEntry/padTo4 mirror internal/initrdinfo's own test helpers of
// the same name (unexported there, so not importable - a small, deliberate
// duplication rather than exporting test-only helpers across a package
// boundary just for this).
func appendCPIOEntry(buf *bytes.Buffer, name string, data []byte) {
	nameZ := name + "\x00"
	header := fmt.Sprintf("070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
		0, 0o100644, 0, 0, 1, 0, len(data), 0, 0, 0, 0, len(nameZ), 0)
	buf.WriteString(header)
	buf.WriteString(nameZ)
	padTo4(buf)
	buf.Write(data)
	padTo4(buf)
}

func padTo4(buf *bytes.Buffer) {
	for buf.Len()%4 != 0 {
		buf.WriteByte(0)
	}
}

// buildFakeKO hand-assembles a minimal, valid ELF64 relocatable object
// with exactly one real section (.modinfo) - mirrors internal/initrdinfo's
// own test helper of the same name.
func buildFakeKO(t *testing.T, modinfoFields ...string) []byte {
	t.Helper()
	var modinfo bytes.Buffer
	for _, f := range modinfoFields {
		modinfo.WriteString(f)
		modinfo.WriteByte(0)
	}
	modinfoData := modinfo.Bytes()
	shstrtab := []byte("\x00.modinfo\x00.shstrtab\x00")

	const ehsize = 64
	const shentsize = 64
	modinfoOff := ehsize
	shstrtabOff := modinfoOff + len(modinfoData)
	shoff := shstrtabOff + len(shstrtab)

	buf := make([]byte, shoff+3*shentsize)
	copy(buf[0:4], []byte{0x7f, 'E', 'L', 'F'})
	buf[4] = 2
	buf[5] = 1
	buf[6] = 1
	binary.LittleEndian.PutUint16(buf[16:18], 1)
	binary.LittleEndian.PutUint16(buf[18:20], 0x3E)
	binary.LittleEndian.PutUint32(buf[20:24], 1)
	binary.LittleEndian.PutUint64(buf[40:48], uint64(shoff))
	binary.LittleEndian.PutUint16(buf[52:54], ehsize)
	binary.LittleEndian.PutUint16(buf[58:60], shentsize)
	binary.LittleEndian.PutUint16(buf[60:62], 3)
	binary.LittleEndian.PutUint16(buf[62:64], 2)

	copy(buf[modinfoOff:], modinfoData)
	copy(buf[shstrtabOff:], shstrtab)

	writeShdr := func(idx int, nameOff uint32, shType uint32, offset, size uint64) {
		off := shoff + idx*shentsize
		binary.LittleEndian.PutUint32(buf[off:off+4], nameOff)
		binary.LittleEndian.PutUint32(buf[off+4:off+8], shType)
		binary.LittleEndian.PutUint64(buf[off+24:off+32], offset)
		binary.LittleEndian.PutUint64(buf[off+32:off+40], size)
		binary.LittleEndian.PutUint64(buf[off+56:off+64], 1)
	}
	writeShdr(1, 1, 1, uint64(modinfoOff), uint64(len(modinfoData)))
	writeShdr(2, 10, 3, uint64(shstrtabOff), uint64(len(shstrtab)))
	return buf
}

// buildFakeInitrdBytes builds a real, parseable gzip(cpio(zfs.ko)) initrd -
// exactly what internal/initrdinfo.ExtractOpenZFSVersionFromBytes needs to
// find a real "version=" entry.
func buildFakeInitrdBytes(t *testing.T, openzfsVersion string) []byte {
	t.Helper()
	ko := buildFakeKO(t, "version="+openzfsVersion, "license=CDDL")
	var cpio bytes.Buffer
	appendCPIOEntry(&cpio, "bin/busybox", []byte("fake-busybox"))
	appendCPIOEntry(&cpio, "lib/modules/6.18.0/kernel/zfs/zfs.ko", ko)
	appendCPIOEntry(&cpio, "TRAILER!!!", nil)

	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write(cpio.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return gz.Bytes()
}

func newBIOSMountpoint(t *testing.T) string {
	t.Helper()
	mountpoint := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mountpoint, layout.ESPDir), 0o755); err != nil {
		t.Fatal(err)
	}
	return mountpoint
}

// TestWriteMetadata_MissingVersionOrBuildStamp_RefusesToWrite is the
// regression test for F8 (unidoc-alip's PR #5 review): metadata.Decode
// requires version/buildstamp non-empty, but metadata.Encode validated
// nothing, and this project's BIOS install/update path takes both
// values from cmdline.ParseText, which does not require them either -
// a custom --cmdline-file or a hand-edited CMDLINE missing either
// field used to write a manifest that decoded fine at write time but
// that every LATER verify rejected as malformed, with verify --repair
// then refusing to touch it (since it EXISTS) - a stuck installation
// with no clean way out. writeMetadata must now refuse to write such
// a manifest at all, at the one point a caller can still do something
// about it (roll back).
func TestWriteMetadata_MissingVersionOrBuildStamp_RefusesToWrite(t *testing.T) {
	for _, tc := range []struct {
		name                string
		version, buildStamp string
	}{
		{"missing_version", "", "20260923T150000Z"},
		{"missing_buildstamp", "0.1.0", ""},
		{"missing_both", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mountpoint := newBIOSMountpoint(t)
			kernel := buildFakeKernelBytes("6.18.53-0-lts")
			initrd := buildFakeInitrdBytes(t, "2.4.4-1")

			err := writeMetadata(mountpoint, "aarch64", tc.version, tc.buildStamp, kernel, initrd)
			if err == nil {
				t.Fatal("writeMetadata with a missing version/buildstamp: want an error, got nil - this would write a manifest verify could never accept")
			}
			if _, statErr := os.Stat(filepath.Join(mountpoint, layout.MetadataFile)); !os.IsNotExist(statErr) {
				t.Error("writeMetadata must not leave a manifest file behind when it refuses to write one")
			}
		})
	}
}

// TestWritePayloadWithRollback_MissingVersionRollsBackWholeGeneration
// proves the full end-to-end consequence of the same F8 fix: the real
// install/update path (writePayloadWithRollback) must not silently
// "succeed" (return nil) with a CMDLINE that omits version/buildstamp
// - it must fail and roll the whole generation back, leaving nothing
// on the ESP a later verify could ever find stuck.
func TestWritePayloadWithRollback_MissingVersionRollsBackWholeGeneration(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)
	kernel := buildFakeKernelBytes("6.18.53-0-lts")
	initrd := buildFakeInitrdBytes(t, "2.4.4-1")
	cmdlineTxt := []byte("root=ZFS=zroot/ROOT/default ro\n") // no alpine-zfsboot.version=/buildstamp=

	err := writePayloadWithRollback(mountpoint, "aarch64", "", "", kernel, initrd, cmdlineTxt)
	if err == nil {
		t.Fatal("writePayloadWithRollback with no version/buildstamp: want an error, got nil")
	}
	for _, rel := range []string{layout.KernelFile, layout.InitrdFile, layout.CmdlineFile, layout.MetadataFile} {
		if _, statErr := os.Stat(filepath.Join(mountpoint, rel)); !os.IsNotExist(statErr) {
			t.Errorf("writePayloadWithRollback must roll back %s on a fresh install with no prior generation to restore - found it left behind", rel)
		}
	}
}

// TestWritePayloadWithRollback_WritesRealMetadata is the direct happy-path
// regression test: a successful write must leave a metadata manifest whose
// fields genuinely came from deep-inspecting the real kernel/initrd bytes
// just written, not placeholders.
func TestWritePayloadWithRollback_WritesRealMetadata(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)
	kernel := buildFakeKernelBytes("6.18.53-0-lts")
	initrd := buildFakeInitrdBytes(t, "2.4.4-1")
	cmdlineTxt := []byte("root=ZFS=zroot/ROOT/default ro alpine-zfsboot.buildstamp=20260923T150000Z alpine-zfsboot.version=0.1.0\n")

	if err := writePayloadWithRollback(mountpoint, "aarch64", "0.1.0", "20260923T150000Z", kernel, initrd, cmdlineTxt); err != nil {
		t.Fatalf("writePayloadWithRollback: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(mountpoint, layout.MetadataFile))
	if err != nil {
		t.Fatalf("reading METADATA after a successful write: %v", err)
	}
	m, err := metadata.Decode(raw)
	if err != nil {
		t.Fatalf("decoding METADATA: %v", err)
	}
	if m.Kernel != "6.18.53-0-lts (build@host) #1 SMP" {
		t.Errorf("m.Kernel = %q", m.Kernel)
	}
	if m.OpenZFS != "2.4.4-1" {
		t.Errorf("m.OpenZFS = %q, want 2.4.4-1", m.OpenZFS)
	}
	if m.Arch != "aarch64" || m.Version != "0.1.0" || m.BuildStamp != "20260923T150000Z" {
		t.Errorf("m = %#v", m)
	}
	if !metadata.VerifyHash(kernel, m.KernelSHA256) {
		t.Error("m.KernelSHA256 does not match the real kernel bytes")
	}
	if !metadata.VerifyHash(initrd, m.InitrdSHA256) {
		t.Error("m.InitrdSHA256 does not match the real initrd bytes")
	}
}

// TestWritePayloadWithRollback_MetadataFailureRollsBackWholeGeneration is
// the direct regression test for the atomicity requirement: KERNEL+INITRD+
// CMDLINE+METADATA are one generation - if metadata generation fails AFTER
// the other three already wrote and verified successfully, ALL FOUR must
// roll back to their pre-call state, not just leave a good payload with no
// metadata.
func TestWritePayloadWithRollback_MetadataFailureRollsBackWholeGeneration(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)

	oldKernel := buildFakeKernelBytes("6.18.0-old")
	oldInitrd := buildFakeInitrdBytes(t, "2.3.0-old")
	oldCmdline := []byte("OLD-cmdline\n")
	if err := os.WriteFile(filepath.Join(mountpoint, layout.KernelFile), oldKernel, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mountpoint, layout.InitrdFile), oldInitrd, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mountpoint, layout.CmdlineFile), oldCmdline, 0o644); err != nil {
		t.Fatal(err)
	}
	// No pre-existing METADATA - proves the rollback correctly REMOVES a
	// stray new one rather than merely "restoring" a nonexistent old one.

	// newInitrd is deliberately NOT a real gzip/xz/cpio stream at all -
	// espconfig.WritePayload/VerifyPayload have no opinion on its
	// content (they just byte-write/byte-compare), so this write
	// SUCCEEDS - but the metadata-generation step that runs AFTER it
	// will fail for real when it tries to actually deep-inspect these
	// bytes, exactly the failure this test exists to drive.
	newKernel := buildFakeKernelBytes("6.18.53-0-lts")
	newInitrd := []byte("not a real initrd at all")
	newCmdline := []byte("NEW-cmdline\n")

	err := writePayloadWithRollback(mountpoint, "x86_64", "0.1.0", "20260923T150000Z", newKernel, newInitrd, newCmdline)
	if err == nil {
		t.Fatal("want an error (metadata generation must fail on an unparseable initrd), got nil")
	}

	gotKernel, _ := os.ReadFile(filepath.Join(mountpoint, layout.KernelFile))
	if !bytes.Equal(gotKernel, oldKernel) {
		t.Errorf("KERNEL after rollback = %q, want the original", gotKernel)
	}
	gotInitrd, _ := os.ReadFile(filepath.Join(mountpoint, layout.InitrdFile))
	if !bytes.Equal(gotInitrd, oldInitrd) {
		t.Errorf("INITRD after rollback = %q, want the original", gotInitrd)
	}
	gotCmdline, _ := os.ReadFile(filepath.Join(mountpoint, layout.CmdlineFile))
	if !bytes.Equal(gotCmdline, oldCmdline) {
		t.Errorf("CMDLINE after rollback = %q, want the original", gotCmdline)
	}
	if _, err := os.Stat(filepath.Join(mountpoint, layout.MetadataFile)); !os.IsNotExist(err) {
		t.Errorf("METADATA after rollback: want removed (none existed before), got err=%v", err)
	}
}

// TestVerifyMetadata_Missing_NoRepair proves a simply-absent manifest is
// NOT itself a verify failure.
func TestVerifyMetadata_Missing_NoRepair(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)
	kernel := buildFakeKernelBytes("6.18.53-0-lts")
	initrd := buildFakeInitrdBytes(t, "2.4.4-1")
	mustWriteFile(t, mountpoint, layout.KernelFile, kernel)
	mustWriteFile(t, mountpoint, layout.InitrdFile, initrd)
	mustWriteFile(t, mountpoint, layout.CmdlineFile, []byte("root=ZFS=zroot/ROOT/default ro\n"))

	tgt := &target{uefi: false, arch: "x86_64", mountpoint: mountpoint}
	errs := verifyMetadata(tgt, false, false)
	if len(errs) != 0 {
		t.Errorf("verifyMetadata on a missing manifest, no --repair: want no errors, got %v", errs)
	}
	if _, err := os.Stat(filepath.Join(mountpoint, layout.MetadataFile)); !os.IsNotExist(err) {
		t.Error("verifyMetadata without --repair must not write anything")
	}
}

// TestVerifyMetadata_Missing_Repair proves --repair safely reconstructs a
// missing manifest from the real, current payload.
func TestVerifyMetadata_Missing_Repair(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)
	kernel := buildFakeKernelBytes("6.18.53-0-lts")
	initrd := buildFakeInitrdBytes(t, "2.4.4-1")
	mustWriteFile(t, mountpoint, layout.KernelFile, kernel)
	mustWriteFile(t, mountpoint, layout.InitrdFile, initrd)
	mustWriteFile(t, mountpoint, layout.CmdlineFile, []byte("root=ZFS=zroot/ROOT/default ro alpine-zfsboot.buildstamp=X alpine-zfsboot.version=0.1.0\n"))

	tgt := &target{uefi: false, arch: "x86_64", mountpoint: mountpoint}
	errs := verifyMetadata(tgt, false, true)
	if len(errs) != 0 {
		t.Fatalf("verifyMetadata --repair on a missing manifest: want no errors, got %v", errs)
	}
	raw, err := os.ReadFile(filepath.Join(mountpoint, layout.MetadataFile))
	if err != nil {
		t.Fatalf("METADATA not written by --repair: %v", err)
	}
	m, err := metadata.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.OpenZFS != "2.4.4-1" {
		t.Errorf("reconstructed m.OpenZFS = %q, want 2.4.4-1", m.OpenZFS)
	}
}

// TestVerifyMetadata_HashMismatch_NeverAutoRepaired proves a genuine
// mismatch (something changed) is a hard failure regardless of --repair -
// the exact "do not silently bless a changed payload" requirement.
func TestVerifyMetadata_HashMismatch_NeverAutoRepaired(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)
	kernel := buildFakeKernelBytes("6.18.53-0-lts")
	initrd := buildFakeInitrdBytes(t, "2.4.4-1")
	mustWriteFile(t, mountpoint, layout.KernelFile, kernel)
	mustWriteFile(t, mountpoint, layout.InitrdFile, initrd)
	mustWriteFile(t, mountpoint, layout.CmdlineFile, []byte("root=ZFS=zroot/ROOT/default ro\n"))

	// A manifest recorded against DIFFERENT (old) kernel bytes than what's
	// actually installed now - simulates the kernel file being replaced
	// out-of-band after the manifest was written.
	staleManifest := metadata.Manifest{
		Version: "0.1.0", BuildStamp: "X", Arch: "x86_64",
		Kernel: "6.18.0-old", OpenZFS: "2.4.4-1",
		KernelSHA256: metadata.SHA256Hex([]byte("completely different kernel bytes")),
		InitrdSHA256: metadata.SHA256Hex(initrd),
	}
	mustWriteFile(t, mountpoint, layout.MetadataFile, metadata.Encode(staleManifest))

	tgt := &target{uefi: false, arch: "x86_64", mountpoint: mountpoint}

	for _, repair := range []bool{false, true} {
		errs := verifyMetadata(tgt, false, repair)
		if len(errs) == 0 {
			t.Fatalf("verifyMetadata (repair=%v) on a real hash mismatch: want an error, got none", repair)
		}
	}

	// Confirm --repair genuinely did NOT overwrite the mismatched manifest.
	raw, err := os.ReadFile(filepath.Join(mountpoint, layout.MetadataFile))
	if err != nil {
		t.Fatal(err)
	}
	got, err := metadata.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got != staleManifest {
		t.Error("--repair overwrote a hash-mismatched manifest - it must only ever reconstruct a MISSING one, never one that exists but disagrees")
	}
}

// TestVerifyMetadata_HashMatch_NoDeep_Passes proves the common healthy
// case: a manifest whose hashes genuinely match the real installed
// payload produces no errors without --deep.
func TestVerifyMetadata_HashMatch_NoDeep_Passes(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)
	kernel := buildFakeKernelBytes("6.18.53-0-lts")
	initrd := buildFakeInitrdBytes(t, "2.4.4-1")
	mustWriteFile(t, mountpoint, layout.KernelFile, kernel)
	mustWriteFile(t, mountpoint, layout.InitrdFile, initrd)
	mustWriteFile(t, mountpoint, layout.CmdlineFile, []byte("root=ZFS=zroot/ROOT/default ro alpine-zfsboot.version=0.1.0 alpine-zfsboot.buildstamp=X\n"))

	m := metadata.Manifest{
		Version: "0.1.0", BuildStamp: "X", Arch: "x86_64",
		Kernel: "6.18.53-0-lts (build@host) #1 SMP", OpenZFS: "2.4.4-1",
		KernelSHA256: metadata.SHA256Hex(kernel),
		InitrdSHA256: metadata.SHA256Hex(initrd),
	}
	mustWriteFile(t, mountpoint, layout.MetadataFile, metadata.Encode(m))

	tgt := &target{uefi: false, arch: "x86_64", mountpoint: mountpoint}
	if errs := verifyMetadata(tgt, false, false); len(errs) != 0 {
		t.Errorf("verifyMetadata on a genuinely matching manifest: want no errors, got %v", errs)
	}
}

// TestVerifyMetadata_Deep_CatchesDecodedMismatchEvenIfHashesMatch proves
// --deep is a strictly stronger check than the hash comparison alone: a
// manifest can have CORRECT hashes (nothing tampered with the raw bytes)
// but WRONG recorded version strings (e.g. a bug in whatever generated it,
// or the manifest was hand-edited) - --deep re-derives the real versions
// and catches that, where a hash-only check would not.
func TestVerifyMetadata_Deep_CatchesDecodedMismatchEvenIfHashesMatch(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)
	kernel := buildFakeKernelBytes("6.18.53-0-lts")
	initrd := buildFakeInitrdBytes(t, "2.4.4-1")
	mustWriteFile(t, mountpoint, layout.KernelFile, kernel)
	mustWriteFile(t, mountpoint, layout.InitrdFile, initrd)
	mustWriteFile(t, mountpoint, layout.CmdlineFile, []byte("root=ZFS=zroot/ROOT/default ro alpine-zfsboot.version=0.1.0 alpine-zfsboot.buildstamp=X\n"))

	m := metadata.Manifest{
		Version: "0.1.0", BuildStamp: "X", Arch: "x86_64",
		Kernel:       "9.9.9-WRONG", // hashes match the real bytes, but this doesn't
		OpenZFS:      "2.4.4-1",
		KernelSHA256: metadata.SHA256Hex(kernel),
		InitrdSHA256: metadata.SHA256Hex(initrd),
	}
	mustWriteFile(t, mountpoint, layout.MetadataFile, metadata.Encode(m))

	tgt := &target{uefi: false, arch: "x86_64", mountpoint: mountpoint}

	if errs := verifyMetadata(tgt, false, false); len(errs) != 0 {
		t.Fatalf("verifyMetadata WITHOUT --deep on a hash-valid-but-decode-wrong manifest: want no errors (hash check alone can't catch this), got %v", errs)
	}
	if errs := verifyMetadata(tgt, true, false); len(errs) == 0 {
		t.Fatal("verifyMetadata WITH --deep on a hash-valid-but-decode-wrong manifest: want an error, got none")
	}
}

// TestVerifyMetadata_IdentityFieldMismatch_CatchesEvenWithValidHashes is
// the direct regression test for a real gap a follow-up review found:
// KERNEL_SHA256/INITRD_SHA256 bind the manifest to the PAYLOAD bytes, but
// nothing checked version/buildstamp/arch against the installed artifact's
// OWN cmdline - despite Manifest's own doc comment on BuildStamp stating
// "the two are required to agree." A manifest with byte-correct hashes
// (nothing touched the real kernel/initrd) but a stale/hand-edited
// version, buildstamp, or arch field previously passed silently, even
// under --deep (which only re-derives Kernel/OpenZFS, not these three).
// Mutates each field independently, one at a time, with hashes and
// KERNEL/INITRD bytes completely untouched - proving the new checks
// aren't accidentally piggybacking on the hash check.
func TestVerifyMetadata_IdentityFieldMismatch_CatchesEvenWithValidHashes(t *testing.T) {
	kernel := buildFakeKernelBytes("6.18.53-0-lts")
	initrd := buildFakeInitrdBytes(t, "2.4.4-1")
	cmdlineTxt := []byte("root=ZFS=zroot/ROOT/default ro alpine-zfsboot.version=0.1.0 alpine-zfsboot.buildstamp=X\n")

	base := metadata.Manifest{
		Version: "0.1.0", BuildStamp: "X", Arch: "x86_64",
		Kernel: "6.18.53-0-lts (build@host) #1 SMP", OpenZFS: "2.4.4-1",
		KernelSHA256: metadata.SHA256Hex(kernel),
		InitrdSHA256: metadata.SHA256Hex(initrd),
	}

	cases := []struct {
		name   string
		mutate func(m *metadata.Manifest)
	}{
		{"version", func(m *metadata.Manifest) { m.Version = "666.0.0" }},
		{"buildstamp", func(m *metadata.Manifest) { m.BuildStamp = "20991231T235959Z" }},
		{"arch", func(m *metadata.Manifest) { m.Arch = "aarch64" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mountpoint := newBIOSMountpoint(t)
			mustWriteFile(t, mountpoint, layout.KernelFile, kernel)
			mustWriteFile(t, mountpoint, layout.InitrdFile, initrd)
			mustWriteFile(t, mountpoint, layout.CmdlineFile, cmdlineTxt)

			m := base
			c.mutate(&m)
			mustWriteFile(t, mountpoint, layout.MetadataFile, metadata.Encode(m))

			tgt := &target{uefi: false, arch: "x86_64", mountpoint: mountpoint}
			errs := verifyMetadata(tgt, false, false)
			if len(errs) == 0 {
				t.Fatalf("verifyMetadata with a mutated %s field (hashes/payload untouched): want an error, got none", c.name)
			}
		})
	}
}

// TestVerifyMetadata_PresentButUndecodable_NeverAutoRepaired proves a
// corrupt (not merely absent) manifest is a hard failure that --repair
// does NOT silently paper over.
func TestVerifyMetadata_PresentButUndecodable_NeverAutoRepaired(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)
	kernel := buildFakeKernelBytes("6.18.53-0-lts")
	initrd := buildFakeInitrdBytes(t, "2.4.4-1")
	mustWriteFile(t, mountpoint, layout.KernelFile, kernel)
	mustWriteFile(t, mountpoint, layout.InitrdFile, initrd)
	mustWriteFile(t, mountpoint, layout.CmdlineFile, []byte("root=ZFS=zroot/ROOT/default ro\n"))
	mustWriteFile(t, mountpoint, layout.MetadataFile, []byte("this is not a valid manifest at all"))

	tgt := &target{uefi: false, arch: "x86_64", mountpoint: mountpoint}
	for _, repair := range []bool{false, true} {
		errs := verifyMetadata(tgt, false, repair)
		if len(errs) == 0 {
			t.Fatalf("verifyMetadata (repair=%v) on an undecodable manifest: want an error, got none", repair)
		}
	}
	got, _ := os.ReadFile(filepath.Join(mountpoint, layout.MetadataFile))
	if string(got) != "this is not a valid manifest at all" {
		t.Error("--repair must not touch a present-but-undecodable manifest")
	}
}

// TestVerifyMetadata_DirectoryInPlaceOfMetadata_NeverFalsePasses is the
// regression test for F7 (unidoc-alip's PR #5 review): tryReadMetadata
// used to treat ANY os.ReadFile error - not just os.IsNotExist - as
// "the manifest simply doesn't exist", which broke the property
// verifyMetadata's own doc comment promises for --repair ("never
// touches one that exists and disagrees"). A directory sitting where
// METADATA should be a plain file is a real, concrete instance of
// "exists but is not readable as a manifest" (os.ReadFile on a
// directory fails with EISDIR, not ENOENT) - before the fix, this was
// silently reported as no manifest at all: plain `verify` returned NO
// errors (a false pass), and `verify --repair` would have happily
// written a fresh manifest INTO that same path (os.WriteFile on an
// existing directory also fails, so --repair should itself surface a
// real error here too, not quietly "succeed").
func TestVerifyMetadata_DirectoryInPlaceOfMetadata_NeverFalsePasses(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)
	kernel := buildFakeKernelBytes("6.18.53-0-lts")
	initrd := buildFakeInitrdBytes(t, "2.4.4-1")
	mustWriteFile(t, mountpoint, layout.KernelFile, kernel)
	mustWriteFile(t, mountpoint, layout.InitrdFile, initrd)
	mustWriteFile(t, mountpoint, layout.CmdlineFile, []byte("root=ZFS=zroot/ROOT/default ro\n"))
	if err := os.MkdirAll(filepath.Join(mountpoint, layout.MetadataFile), 0o755); err != nil {
		t.Fatal(err)
	}

	tgt := &target{uefi: false, arch: "x86_64", mountpoint: mountpoint}

	errs := verifyMetadata(tgt, false, false)
	if len(errs) == 0 {
		t.Fatal("verifyMetadata(repair=false) with a DIRECTORY at the metadata path: want an error (a real unreadable manifest), got a false pass")
	}

	errs = verifyMetadata(tgt, false, true)
	if len(errs) == 0 {
		t.Fatal("verifyMetadata(repair=true) with a DIRECTORY at the metadata path: want an error, got a silent (false) success")
	}
	info, statErr := os.Stat(filepath.Join(mountpoint, layout.MetadataFile))
	if statErr != nil || !info.IsDir() {
		t.Error("--repair must not have replaced the directory at the metadata path with a file - it should have hard-failed instead of 'repairing' something that exists but isn't readable")
	}
}

// TestVerifyMetadata_UnreadableManifest_NeverAutoRepaired is the second
// half of F7's regression coverage: an EXISTING manifest this process
// cannot even READ (permissions, not corruption) must be treated the
// same as "present but undecodable" - a hard failure, never silently
// treated as missing-and-safe-to-regenerate. Skipped as root: root
// ignores file permission bits entirely, so chmod 0000 would not
// actually reproduce the unreadable condition this test depends on
// (the same real gap F22, this same review, flagged in
// internal/uefiboot's TestBackupPrevious_FailedNewBackupPreservesOldOne
// - fixed there too, not just avoided here).
func TestVerifyMetadata_UnreadableManifest_NeverAutoRepaired(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root - chmod 0000 does not make a file unreadable to root, so this test cannot reproduce the condition it exists to check")
	}

	mountpoint := newBIOSMountpoint(t)
	kernel := buildFakeKernelBytes("6.18.53-0-lts")
	initrd := buildFakeInitrdBytes(t, "2.4.4-1")
	mustWriteFile(t, mountpoint, layout.KernelFile, kernel)
	mustWriteFile(t, mountpoint, layout.InitrdFile, initrd)
	mustWriteFile(t, mountpoint, layout.CmdlineFile, []byte("root=ZFS=zroot/ROOT/default ro\n"))
	// A manifest whose hashes deliberately do NOT match kernel/initrd
	// above - if this were readable, plain `verify` would correctly
	// fail on it. The point of this test is that making it unreadable
	// must not turn that real, existing mismatch into a silent pass.
	badManifest := metadata.Encode(metadata.Manifest{
		Version: "0.1.0", BuildStamp: "20260101T000000Z", Arch: "x86_64",
		Kernel: "6.18.53-0-lts", OpenZFS: "2.4.4-1",
		KernelSHA256: "0000000000000000000000000000000000000000000000000000000000000000",
		InitrdSHA256: "1111111111111111111111111111111111111111111111111111111111111111",
	})
	metadataPath := filepath.Join(mountpoint, layout.MetadataFile)
	mustWriteFile(t, mountpoint, layout.MetadataFile, badManifest)
	if err := os.Chmod(metadataPath, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(metadataPath, 0o644) // let t.TempDir() clean up afterward

	tgt := &target{uefi: false, arch: "x86_64", mountpoint: mountpoint}

	if errs := verifyMetadata(tgt, false, false); len(errs) == 0 {
		t.Fatal("verifyMetadata(repair=false) on an unreadable (chmod 0000) manifest: want an error, got a false pass")
	}
	if errs := verifyMetadata(tgt, false, true); len(errs) == 0 {
		t.Fatal("verifyMetadata(repair=true) on an unreadable manifest: want an error (never auto-repaired), got a silent success")
	}

	if err := os.Chmod(metadataPath, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, badManifest) {
		t.Error("--repair must not have overwritten a manifest it could not prove was actually missing - the original (mismatched) content should be untouched")
	}
}

func mustWriteFile(t *testing.T, mountpoint, rel string, content []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(mountpoint, rel), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestVerifyMountReadonly is the regression test for a real bug a final
// pre-release audit found: verify passed a hardcoded readonly=true to
// discover(), so `verify --repair` - the ONE verify mode that writes -
// mounted the ESP with unix.MS_RDONLY whenever it had to mount it
// itself, and its own espconfig.WriteFile then failed EROFS. That broke
// exactly the case internal/metadata's doc comment names as the
// documented recovery path (a rescue initramfs / an --root invocation,
// where the ESP is precisely NOT already mounted).
//
// The wiring itself (verify's Run closure -> discover -> bootenv.
// MountESP -> unix.Mount) cannot be exercised in a sandbox: there is no
// real ESP to find, and die() would os.Exit the test binary. What CAN
// be pinned, and is the entire content of the bug, is the decision:
// read-only for every verify mode EXCEPT --repair.
// TestVerifyCmd_PartialKernelFileFlags_RejectedBeforeRun is the
// regression test for F12 (unidoc-alip's PR #5 review): VerifyPayload
// checks kernel/initrd/cmdline together as one unit, but nothing used
// to enforce that at the CLI layer - `verify --kernel-file X` alone
// used to silently compare nothing at all and still print "verify:
// OK", even though the flag's own --help text already promised "needs
// all three ... together". cmd.MarkFlagsRequiredTogether makes cobra
// itself refuse to run the command at all when only some of the three
// are given - this validation runs BEFORE the Run closure, so (unlike
// most of this command's own behavior - see TestVerifyMountReadonly's
// own comment on why THAT can't be exercised here) this is safe to
// call directly: a bad flag combination never reaches Run at all,
// never touches disk, never calls die()/os.Exit.
func TestVerifyCmd_PartialKernelFileFlags_RejectedBeforeRun(t *testing.T) {
	for _, args := range [][]string{
		{"--kernel-file", "/tmp/x"},
		{"--initrd-file", "/tmp/x"},
		{"--cmdline-file", "/tmp/x"},
		{"--kernel-file", "/tmp/x", "--initrd-file", "/tmp/x"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			cmd := newVerifyCmd()
			cmd.SetArgs(args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("verify %v: want a rejection before Run ever executes, got nil (this would have silently compared nothing and still printed verify: OK)", args)
			}
			if !strings.Contains(err.Error(), "kernel-file") {
				t.Errorf("error = %q, want it to mention the flag group", err.Error())
			}
		})
	}
}

func TestVerifyMountReadonly(t *testing.T) {
	if got := verifyMountReadonly(false); got != true {
		t.Errorf("verifyMountReadonly(repair=false) = %v, want true - plain verify must never mount the ESP writable", got)
	}
	if got := verifyMountReadonly(true); got != false {
		t.Errorf("verifyMountReadonly(repair=true) = %v, want false - --repair writes the reconstructed manifest and cannot do so on a read-only mount", got)
	}
}

// TestVerifyMetadata_Missing_Repair_CorruptPayloadWritesNothing closes a
// coverage gap a final pre-release audit found. TestVerifyMetadata_
// Missing_Repair above proves --repair reconstructs a manifest from a
// GOOD payload; nothing pinned what happens when the manifest is
// missing AND the payload it would be reconstructed from is itself
// corrupt - which is a realistic combination, not a contrived one (both
// are symptoms of the same interrupted update or failing ESP).
//
// The requirement: --repair must never leave a manifest behind that it
// could not genuinely derive. Writing a half-filled or placeholder
// manifest would be the worst outcome available here, because a
// manifest is precisely what makes a LATER `verify` pass - a bogus one
// converts "this installation is broken" into "this installation
// verifies clean", permanently.
func TestVerifyMetadata_Missing_Repair_CorruptPayloadWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name           string
		kernel, initrd []byte
	}{
		{
			name:   "corrupt initrd",
			kernel: buildFakeKernelBytes("6.18.53-0-lts"),
			initrd: []byte("this is not a cpio archive, compressed or otherwise"),
		},
		{
			name:   "corrupt kernel",
			kernel: []byte("not a kernel image and no Linux version banner either"),
			initrd: buildFakeInitrdBytes(t, "2.4.4-1"),
		},
		{
			name:   "both corrupt",
			kernel: []byte("garbage"),
			initrd: []byte("garbage"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mountpoint := newBIOSMountpoint(t)
			mustWriteFile(t, mountpoint, layout.KernelFile, tc.kernel)
			mustWriteFile(t, mountpoint, layout.InitrdFile, tc.initrd)
			mustWriteFile(t, mountpoint, layout.CmdlineFile, []byte("root=ZFS=zroot/ROOT/default ro alpine-zfsboot.buildstamp=X alpine-zfsboot.version=0.1.0\n"))

			tgt := &target{uefi: false, arch: "x86_64", mountpoint: mountpoint}
			errs := verifyMetadata(tgt, false, true)
			if len(errs) == 0 {
				t.Fatal("verifyMetadata --repair over a corrupt payload: want an error, got none")
			}
			if _, err := os.Stat(filepath.Join(mountpoint, layout.MetadataFile)); !os.IsNotExist(err) {
				t.Errorf("--repair wrote a manifest it could not genuinely derive from the payload (stat err = %v) - a future `verify` would then pass against a payload that is actually broken", err)
			}
		})
	}
}

// TestBackupPayload_UnreadableExistingFileRefusesBeforeWriting is the
// regression test for a real data-loss bug a final pre-release audit
// found in the atomicity machinery itself.
//
// backupPayload used to swallow EVERY os.ReadFile error into a nil
// entry, and rollbackPayload acts on nil by DELETING ("this file did
// not exist before this attempt"). Together: on a machine with a
// healthy installation and an ESP throwing read errors - exactly the
// condition that sends someone to `update` in the first place - a
// failed read during BACKUP made the entry nil, and any subsequent
// failure then made rollback delete the working KERNEL that was
// actually still sitting right there. A cleanly-recoverable failed
// update became an ESP with no bootable payload.
//
// A directory standing where layout.KernelFile should be is the
// deterministic, root-free way to produce a non-NotExist read error
// (EISDIR) - no chmod tricks, no reliance on the test not running as
// root. The requirement it pins: refuse BEFORE the first write, the
// same fail-before-mutating shape as CheckStage2ExtentFree and
// checkUpdateEligible, rather than proceed on a backup known to be
// incomplete.
func TestBackupPayload_UnreadableExistingFileRefusesBeforeWriting(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)

	// A real, healthy previous generation...
	mustWriteFile(t, mountpoint, layout.InitrdFile, buildFakeInitrdBytes(t, "2.4.4-1"))
	mustWriteFile(t, mountpoint, layout.CmdlineFile, []byte("root=ZFS=zroot/ROOT/default ro\n"))
	// ...except KERNEL, which is unreadable rather than absent.
	if err := os.MkdirAll(filepath.Join(mountpoint, layout.KernelFile), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := backupPayload(mountpoint); err == nil {
		t.Fatal("backupPayload with an existing-but-unreadable KERNEL: want an error, got nil - an unreadable file must never be recorded as 'absent', because rollback DELETES on absent")
	}

	// And the whole write must refuse before mutating anything.
	initrdBefore, err := os.ReadFile(filepath.Join(mountpoint, layout.InitrdFile))
	if err != nil {
		t.Fatal(err)
	}
	err = writePayloadWithRollback(mountpoint, "x86_64", "0.1.0", "20260923T150000Z",
		buildFakeKernelBytes("6.18.53-0-lts"), buildFakeInitrdBytes(t, "9.9.9-1"),
		[]byte("root=ZFS=zroot/ROOT/default ro\n"))
	if err == nil {
		t.Fatal("writePayloadWithRollback on an untrustworthy backup: want an error, got nil")
	}

	initrdAfter, readErr := os.ReadFile(filepath.Join(mountpoint, layout.InitrdFile))
	if readErr != nil {
		t.Fatalf("the pre-existing INITRD is gone after a refused write: %v", readErr)
	}
	if !bytes.Equal(initrdBefore, initrdAfter) {
		t.Error("the pre-existing INITRD was modified by a write that should have refused before touching anything")
	}
	if _, err := os.Stat(filepath.Join(mountpoint, layout.MetadataFile)); !os.IsNotExist(err) {
		t.Errorf("a METADATA manifest appeared despite the write refusing (stat err = %v)", err)
	}
}
