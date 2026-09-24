// Package uefiboot is the UEFI backend: writing, verifying, and
// comparing-against-latest-release the single self-contained
// Unified-Kernel-Image-style `.EFI` alpine-zfsboot ships for UEFI
// firmware (build.sh objcopy-embeds kernel/initrd/cmdline as PE
// sections into one binary - see internal/layout's own doc comment;
// this is a completely different, disjoint on-disk model from BIOS's
// separate stage1/stage2/FAT-payload files, which internal/biosboot
// and internal/espconfig own instead).
//
// This package's logic was originally private to cmd/tool (this
// project's very first version of this tool, UEFI-only) - moved here
// unchanged in behavior so install/update/verify/status can all call
// it, instead of update being the only caller. See this package's own
// tests, ported from cmd/tool's own original test suite, for the
// exact behaviors this move is required not to regress.
package uefiboot

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/unidoc/alpine-zfsboot/internal/cmdline"
	"github.com/unidoc/alpine-zfsboot/internal/layout"
	"github.com/unidoc/alpine-zfsboot/internal/release"
)

// LoaderPath returns the on-disk path (relative to mountpoint) where
// arch's UEFI loader lives - EFI/BOOT/BOOTX64.EFI or EFI/BOOT/
// BOOTAA64.EFI (see layout.EFILoaderName).
func LoaderPath(arch string) (string, error) {
	name := layout.EFILoaderName(arch)
	if name == "" {
		return "", fmt.Errorf("unsupported arch %q", arch)
	}
	return filepath.Join(layout.EFIBootDir, name), nil
}

// WriteLoader atomically installs efi as mountpoint's own UEFI loader
// for arch (same-directory temp file + os.Rename, so the firmware
// never sees a half-written file), backing up whatever was there
// before under a ".previous" suffix first via BackupPrevious - the
// SAME rollback convention update already used, now also what install
// produces on a fresh install (where BackupPrevious correctly reports
// "nothing to back up yet" - see its own doc comment).
func WriteLoader(mountpoint, arch string, efi []byte) (backedUp bool, err error) {
	rel, err := LoaderPath(arch)
	if err != nil {
		return false, err
	}
	target := filepath.Join(mountpoint, rel)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return false, fmt.Errorf("creating %s: %w", filepath.Dir(target), err)
	}

	backedUp = BackupPrevious(target)

	tmp, err := os.CreateTemp(filepath.Dir(target), ".alpine-zfsboot-loader-*")
	if err != nil {
		return backedUp, fmt.Errorf("creating a temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	mode := os.FileMode(0o644)
	if st, err := os.Stat(target); err == nil {
		mode = st.Mode()
	}

	if _, err := tmp.Write(efi); err != nil {
		tmp.Close()
		return backedUp, fmt.Errorf("writing %s: %w", tmpPath, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return backedUp, fmt.Errorf("setting permissions on %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return backedUp, fmt.Errorf("syncing %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return backedUp, fmt.Errorf("closing %s: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, target); err != nil {
		return backedUp, fmt.Errorf("installing %s over %s: %w", tmpPath, target, err)
	}

	// Directory fsync, not just the file's own tmp.Sync() above - a full
	// source audit found this missing here (internal/espconfig's own
	// WriteFile already closes the identical gap for FAT config writes -
	// see that function's own comment for the full reasoning). tmp.Sync()
	// makes the loader's DATA durable; nothing before this made the
	// os.Rename's own directory-entry update durable - without it, a
	// power loss shortly after a reported-successful `update` could
	// leave the ESP's FAT directory entry for BOOTX64.EFI/BOOTAA64.EFI
	// pointing at the old inode, a zero-length file, or the leftover
	// .alpine-zfsboot-loader-* temp name, on a machine the operator was
	// just told updated cleanly.
	dirFile, err := os.Open(filepath.Dir(target))
	if err != nil {
		// The rename itself already succeeded - the file is in place
		// under its real name. A directory-fsync failure here means the
		// durability guarantee is weaker than intended, but it is not a
		// reason to report the write itself as failed.
		return backedUp, fmt.Errorf("opening %s to fsync the directory entry (the loader itself was written and renamed successfully): %w", filepath.Dir(target), err)
	}
	defer dirFile.Close()
	if err := dirFile.Sync(); err != nil {
		return backedUp, fmt.Errorf("fsyncing %s (the loader itself was written and renamed successfully): %w", filepath.Dir(target), err)
	}
	return backedUp, nil
}

// VerifyLoader re-reads mountpoint's own UEFI loader for arch and
// compares it byte-for-byte against want.
func VerifyLoader(mountpoint, arch string, want []byte) error {
	rel, err := LoaderPath(arch)
	if err != nil {
		return err
	}
	got, err := os.ReadFile(filepath.Join(mountpoint, rel))
	if err != nil {
		return fmt.Errorf("reading %s: %w", rel, err)
	}
	if len(got) != len(want) {
		return fmt.Errorf("%s is %d bytes, want %d", rel, len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			return fmt.Errorf("%s does not match its expected content (first difference at byte %d)", rel, i)
		}
	}
	return nil
}

// HardLink is os.Link, swappable in tests to force BackupPrevious's
// copy fallback deterministically - the real target (the ESP, vfat)
// has no hard links at all, so that fallback is the normal case in
// production, not an edge case, but nothing in a plain tmp-filesystem
// test environment can make a real os.Link call fail to exercise it
// for real.
var HardLink = os.Link

// BackupPrevious preserves the file currently at path as
// path+".previous" before it gets overwritten, and reports whether a
// usable rollback copy actually exists there afterward - the caller
// must not claim a backup it doesn't have (a hard link silently no-ops
// on vfat, this tool's real target, so an earlier version of this
// logic that unconditionally claimed "previous build kept" regardless
// of whether the link succeeded was actively misleading - see this
// package's own test for the exact regression this guards).
//
// Tries a hard link first - not a data copy, so it's instant
// regardless of file size, and never removes path itself even
// momentarily. Falls back to a real copy when linking fails (the
// normal case on vfat).
//
// Best-effort either way: no existing file to back up (first-ever
// write, e.g. install on a fresh ESP) is not an error, just something
// the caller must not claim.
//
// The new backup is staged at a SEPARATE temp path and only replaces
// the existing path+".previous" via an atomic rename once it's fully,
// successfully built - a full source audit found the previous version
// deleted the existing .previous FIRST, unconditionally, before even
// attempting the new one. On the real production path (vfat, where
// HardLink always fails and copyFile is the normal case), a read/copy
// failure partway through (a bad sector on the ESP, a transient I/O
// error) meant BackupPrevious returned false having ALREADY destroyed
// the one rollback copy that did exist - and WriteLoader's own caller
// proceeds with the write regardless (see its own doc comment: backup
// failure is deliberately non-fatal to the update itself), leaving NO
// rollback copy on disk at all afterward. Now, the same failure leaves
// the PREVIOUS successful backup intact - strictly better than
// discarding it for nothing.
func BackupPrevious(path string) bool {
	dst := path + ".previous"
	tmp := dst + ".tmp"
	os.Remove(tmp) // a stale leftover from an earlier failed attempt, if any

	ok := HardLink(path, tmp) == nil
	if !ok {
		ok = copyFile(path, tmp) == nil
	}
	if !ok {
		os.Remove(tmp)
		return false
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return false
	}
	return true
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}

// Comparison is what Compare below found - TmpPath is always the
// downloaded "latest" build's path, valid (and the caller's to clean
// up) whenever err is nil.
type Comparison struct {
	Local, Latest cmdline.Info
	TmpPath       string
	UpToDate      bool
}

// Compare always downloads the latest build to compare against local
// - there's no cheaper metadata-only path (release asset filenames
// carry no version, only the artifact itself does, by design - see
// internal/release's own comment).
func Compare(path, downloadDir string) (Comparison, error) {
	local, err := cmdline.Read(path)
	if err != nil {
		return Comparison{}, err
	}
	tmp, err := release.Download(local.Arch, downloadDir)
	if err != nil {
		return Comparison{}, fmt.Errorf("checking for updates: %w", err)
	}
	latest, err := cmdline.Read(tmp)
	if err != nil {
		os.Remove(tmp)
		return Comparison{}, fmt.Errorf("reading the downloaded build: %w", err)
	}
	if err := CheckArchMatch(path, local, latest); err != nil {
		os.Remove(tmp)
		return Comparison{}, err
	}
	return Comparison{Local: local, Latest: latest, TmpPath: tmp, UpToDate: latest.BuildStamp <= local.BuildStamp}, nil
}

// CheckArchMatch is Compare's actual guard logic, split out so it's
// testable without a real download or PE fixture file. release.
// Download() already requests local.Arch's own asset name - this
// isn't defense against a wrong URL, it's defense against what's
// actually AT that URL not matching what its own filename promises: a
// CI matrix mistake, a partial re-upload, or a hand-fixed release
// could publish the wrong bytes under a correctly-arch-named asset.
// latest.Arch is read from the downloaded file's own PE machine type,
// independent of the filename that produced it, so this catches that
// case specifically.
func CheckArchMatch(path string, local, latest cmdline.Info) error {
	if latest.Arch != local.Arch {
		return fmt.Errorf(
			"refusing to install: %s is %s but the downloaded %s asset is %s",
			path, local.Arch, release.AssetName(local.Arch), latest.Arch)
	}
	return nil
}
