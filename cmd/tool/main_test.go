package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unidoc/alpine-zfsboot/internal/cmdline"
)

// F1 from unidoc-alip's review of PR #1: compare() downloaded the
// latest asset and never checked its PE machine type against the
// local build's own arch before letting the caller install it -
// release.Download() requesting the right-named asset proves nothing
// about what bytes actually arrived at that URL (a CI matrix mistake,
// a partial re-upload, or a hand-fixed release could all publish the
// wrong ones). checkArchMatch is compare()'s actual guard, split out
// so it's testable without a real download or PE fixture file - same
// reasoning internal/cmdline's own parse()/Read() split already uses.
func TestCheckArchMatch(t *testing.T) {
	sameArch := cmdline.Info{Arch: "x86_64"}
	if err := checkArchMatch("/boot/alpine-zfsboot.EFI", sameArch, sameArch); err != nil {
		t.Errorf("same-arch case: unexpected error: %v", err)
	}

	local := cmdline.Info{Arch: "x86_64"}
	wrongArch := cmdline.Info{Arch: "aarch64"}
	err := checkArchMatch("/boot/alpine-zfsboot.EFI", local, wrongArch)
	if err == nil {
		t.Fatal("wrong-arch case: expected an error refusing the install, got nil - this is exactly the class of bug that lets an unbootable .EFI overwrite a working one")
	}
	if !strings.Contains(err.Error(), "x86_64") || !strings.Contains(err.Error(), "aarch64") {
		t.Errorf("error message should name both arches so the operator can tell what happened, got: %v", err)
	}
}

// F5 from the same review: os.Rename over the target destroyed the
// only copy of the previous build, with no local rollback path if the
// new one doesn't boot. backupPrevious must produce a real, readable
// second copy of the CURRENT content under path+".previous" - and,
// critically, must not touch path itself (the caller's own rename-based
// install right after this call depends on path never having a
// missing-file window).
func TestBackupPrevious(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alpine-zfsboot.EFI")
	if err := os.WriteFile(path, []byte("build-one"), 0o644); err != nil {
		t.Fatal(err)
	}

	backupPrevious(path)

	got, err := os.ReadFile(path + ".previous")
	if err != nil {
		t.Fatalf("path+\".previous\" was not created: %v", err)
	}
	if string(got) != "build-one" {
		t.Errorf("path+\".previous\" content = %q, want %q", got, "build-one")
	}
	if orig, err := os.ReadFile(path); err != nil || string(orig) != "build-one" {
		t.Errorf("backupPrevious must not touch path itself - got %q, %v", orig, err)
	}

	// Simulate the caller's own next step - a rename over path, exactly
	// what runUpdate() actually does (NOT an in-place write: os.Rename
	// repoints the path directory entry at a whole new inode, leaving
	// the old one - and this hard link - untouched; an in-place
	// truncate+write to the SAME inode, by contrast, would be visible
	// through both names at once, since a hard link isn't a copy) - and
	// confirm the backup is a genuinely independent copy, not left
	// dangling at an inode that no longer exists.
	newBuild := filepath.Join(dir, "new-build")
	if err := os.WriteFile(newBuild, []byte("build-two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(newBuild, path); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path + ".previous"); string(got) != "build-one" {
		t.Errorf("path+\".previous\" changed when path was renamed over (%q) - it's supposed to be an independent copy, not still linked to the live file", got)
	}

	// A second update (a stale .previous from an earlier run) must not
	// make os.Link fail and silently skip the new backup - this is
	// exactly the "os.Remove clears any stale link" case the function's
	// own comment describes.
	backupPrevious(path)
	if got, err := os.ReadFile(path + ".previous"); err != nil || string(got) != "build-two" {
		t.Errorf("second backupPrevious() call: path+\".previous\" = %q, %v, want %q (a stale link from the first call must not block this one)", got, err, "build-two")
	}
}

// backupPrevious must never be the reason an update fails - no local
// build to link from yet is a completely ordinary first-ever-update
// case, not an error.
func TestBackupPreviousNoExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alpine-zfsboot.EFI")
	// Deliberately never created - simulates the first update ever run
	// against this path.
	backupPrevious(path) // must not panic or block

	if _, err := os.Stat(path + ".previous"); err == nil {
		t.Error("path+\".previous\" should not exist when there was nothing to link from")
	}
}
