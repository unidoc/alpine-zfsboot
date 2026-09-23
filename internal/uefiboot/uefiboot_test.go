package uefiboot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/unidoc/alpine-zfsboot/internal/cmdline"
)

// Ported unchanged (behavior-wise) from this project's very first
// version of this logic, cmd/tool/main_test.go's own
// TestCheckArchMatch - F1 from unidoc-alip's review of PR #1:
// Compare() downloaded the latest asset and never checked its PE
// machine type against the local build's own arch before letting the
// caller install it.
func TestCheckArchMatch(t *testing.T) {
	sameArch := cmdline.Info{Arch: "x86_64"}
	if err := CheckArchMatch("/boot/alpine-zfsboot.EFI", sameArch, sameArch); err != nil {
		t.Errorf("same-arch case: unexpected error: %v", err)
	}

	local := cmdline.Info{Arch: "x86_64"}
	wrongArch := cmdline.Info{Arch: "aarch64"}
	err := CheckArchMatch("/boot/alpine-zfsboot.EFI", local, wrongArch)
	if err == nil {
		t.Fatal("wrong-arch case: expected an error refusing the install, got nil")
	}
}

// Ported from cmd/tool/main_test.go's own TestBackupPrevious - F5
// from the same review: os.Rename over the target destroyed the only
// copy of the previous build, with no local rollback path.
func TestBackupPrevious(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alpine-zfsboot.EFI")
	if err := os.WriteFile(path, []byte("build-one"), 0o644); err != nil {
		t.Fatal(err)
	}

	if ok := BackupPrevious(path); !ok {
		t.Fatal("BackupPrevious returned false on a filesystem where the hard link should have succeeded")
	}

	got, err := os.ReadFile(path + ".previous")
	if err != nil {
		t.Fatalf("path+\".previous\" was not created: %v", err)
	}
	if string(got) != "build-one" {
		t.Errorf("path+\".previous\" content = %q, want %q", got, "build-one")
	}

	newBuild := filepath.Join(dir, "new-build")
	if err := os.WriteFile(newBuild, []byte("build-two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(newBuild, path); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path + ".previous"); string(got) != "build-one" {
		t.Errorf("path+\".previous\" changed when path was renamed over (%q)", got)
	}

	if ok := BackupPrevious(path); !ok {
		t.Fatal("second BackupPrevious() call returned false")
	}
	if got, err := os.ReadFile(path + ".previous"); err != nil || string(got) != "build-two" {
		t.Errorf("second BackupPrevious() call: path+\".previous\" = %q, %v, want %q", got, err, "build-two")
	}
}

func TestBackupPreviousNoExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alpine-zfsboot.EFI")
	if ok := BackupPrevious(path); ok {
		t.Error("BackupPrevious returned true with nothing to back up from")
	}
	if _, err := os.Stat(path + ".previous"); err == nil {
		t.Error("path+\".previous\" should not exist when there was nothing to link from")
	}
}

// Ported from cmd/tool/main_test.go's own
// TestBackupPreviousFallsBackToCopyWhenLinkFails - the blocker from
// unidoc-alip's second-round review: os.Link fails on vfat (the ESP,
// this tool's actual real-world target).
func TestBackupPreviousFallsBackToCopyWhenLinkFails(t *testing.T) {
	orig := HardLink
	HardLink = func(string, string) error {
		return errors.New("simulated: no hard links on this filesystem (e.g. vfat/ESP)")
	}
	defer func() { HardLink = orig }()

	dir := t.TempDir()
	path := filepath.Join(dir, "alpine-zfsboot.EFI")
	if err := os.WriteFile(path, []byte("build-one"), 0o644); err != nil {
		t.Fatal(err)
	}

	if ok := BackupPrevious(path); !ok {
		t.Fatal("BackupPrevious returned false - the copy fallback should have succeeded")
	}
	got, err := os.ReadFile(path + ".previous")
	if err != nil {
		t.Fatalf("path+\".previous\" was not created by the copy fallback: %v", err)
	}
	if string(got) != "build-one" {
		t.Errorf("path+\".previous\" content = %q, want %q", got, "build-one")
	}

	if err := os.WriteFile(path, []byte("build-two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path + ".previous"); string(got) != "build-one" {
		t.Errorf("path+\".previous\" changed after overwriting path in place (%q)", got)
	}
}

// TestBackupPrevious_FailedNewBackupPreservesOldOne is the regression
// test for a real bug found in a full source audit: BackupPrevious used
// to unconditionally os.Remove the existing path+".previous" BEFORE
// attempting the new backup. On the real production path (vfat, where
// HardLink always fails and copyFile is the normal case - see
// TestBackupPreviousFallsBackToCopyWhenLinkFails above), a read/copy
// failure (a bad sector on the ESP, a transient I/O error) meant the
// one rollback copy that DID exist was already destroyed before the
// failure was even known, leaving WriteLoader's caller with no backup
// at all once it proceeded anyway (backup failure is deliberately
// non-fatal to the update itself). This test forces BOTH the hard-link
// and copy paths to fail (an unreadable source file, 0000 permissions)
// with a real pre-existing .previous already in place, and proves that
// old backup survives.
func TestBackupPrevious_FailedNewBackupPreservesOldOne(t *testing.T) {
	// F22 (unidoc-alip's PR #5 review): this test relies on chmod 0000
	// actually making `path` unreadable to force the copy-fallback path
	// to fail for real - root ignores file permission bits entirely, so
	// running this suite as root would silently fail to reproduce the
	// condition the test depends on (BackupPrevious would then succeed,
	// and the assertions below would fail for an unrelated reason).
	if os.Getuid() == 0 {
		t.Skip("running as root - chmod 0000 does not make a file unreadable to root, so this test cannot reproduce the condition it exists to check")
	}

	orig := HardLink
	HardLink = func(string, string) error {
		return errors.New("simulated: no hard links on this filesystem (e.g. vfat/ESP)")
	}
	defer func() { HardLink = orig }()

	dir := t.TempDir()
	path := filepath.Join(dir, "alpine-zfsboot.EFI")
	if err := os.WriteFile(path, []byte("build-two"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A real, pre-existing backup from an EARLIER successful update -
	// the thing that must survive a later failed attempt.
	if err := os.WriteFile(path+".previous", []byte("build-one"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o644) // so t.TempDir()'s own cleanup can remove it

	if ok := BackupPrevious(path); ok {
		t.Fatal("BackupPrevious returned true despite both the hard-link and copy paths being forced to fail")
	}

	got, err := os.ReadFile(path + ".previous")
	if err != nil {
		t.Fatalf("path+\".previous\" is gone after a failed backup attempt - the OLD backup was destroyed for nothing: %v", err)
	}
	if string(got) != "build-one" {
		t.Errorf("path+\".previous\" = %q, want the untouched old backup %q", got, "build-one")
	}
	if _, err := os.Stat(path + ".previous.tmp"); err == nil {
		t.Error("a stray .previous.tmp scratch file was left behind after a failed backup attempt")
	}
}

func TestLoaderPath(t *testing.T) {
	got, err := LoaderPath("x86_64")
	if err != nil || got != "EFI/BOOT/BOOTX64.EFI" {
		t.Errorf("LoaderPath(x86_64) = %q, %v, want \"EFI/BOOT/BOOTX64.EFI\", nil", got, err)
	}
	if _, err := LoaderPath("riscv64"); err == nil {
		t.Error("LoaderPath(riscv64): want an error for an unsupported arch, got nil")
	}
}

func TestWriteLoaderAndVerify(t *testing.T) {
	mnt := t.TempDir()
	first := []byte("first-build-bytes")

	backedUp, err := WriteLoader(mnt, "x86_64", first)
	if err != nil {
		t.Fatalf("WriteLoader (first write): %v", err)
	}
	if backedUp {
		t.Error("WriteLoader on a fresh ESP reported a backup, but nothing existed to back up")
	}
	if err := VerifyLoader(mnt, "x86_64", first); err != nil {
		t.Errorf("VerifyLoader after a correct write: %v", err)
	}

	second := []byte("second-build-bytes-different-length")
	backedUp, err = WriteLoader(mnt, "x86_64", second)
	if err != nil {
		t.Fatalf("WriteLoader (second write): %v", err)
	}
	if !backedUp {
		t.Error("WriteLoader over an existing loader should have backed it up")
	}
	if err := VerifyLoader(mnt, "x86_64", second); err != nil {
		t.Errorf("VerifyLoader after the second write: %v", err)
	}

	previousPath := filepath.Join(mnt, "EFI/BOOT/BOOTX64.EFI.previous")
	got, err := os.ReadFile(previousPath)
	if err != nil {
		t.Fatalf("reading the .previous backup: %v", err)
	}
	if string(got) != string(first) {
		t.Errorf(".previous content = %q, want %q (the first build)", got, first)
	}

	if err := VerifyLoader(mnt, "x86_64", first); err == nil {
		t.Error("VerifyLoader against stale content: want an error, got nil")
	}
}

func TestWriteLoaderNoStrayTempFiles(t *testing.T) {
	mnt := t.TempDir()
	if _, err := WriteLoader(mnt, "aarch64", []byte("x")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(mnt, "EFI/BOOT"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "BOOTAA64.EFI" {
		t.Errorf("EFI/BOOT contents = %v, want exactly [BOOTAA64.EFI]", entries)
	}
}
