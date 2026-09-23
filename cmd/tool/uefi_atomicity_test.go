package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/unidoc/alpine-zfsboot/internal/cmdline"
	"github.com/unidoc/alpine-zfsboot/internal/layout"
	"github.com/unidoc/alpine-zfsboot/internal/metadata"
)

func newUEFIMountpoint(t *testing.T) string {
	t.Helper()
	mountpoint := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mountpoint, layout.EFIBootDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(mountpoint, layout.ESPDir), 0o755); err != nil {
		t.Fatal(err)
	}
	return mountpoint
}

// TestWriteUEFIGenerationWithRollback_MetadataFailureRestoresLoaderToo is
// the direct regression test for the real gap a follow-up review found:
// WriteLoader/VerifyLoader could succeed, and only THEN could metadata
// generation fail - previously leaving the ESP with a NEW loader but OLD
// metadata, violating the "KERNEL+INITRD+CMDLINE+METADATA is one
// generation" invariant for the UEFI case. efiPath deliberately points at
// a file that is NOT a valid PE image at all, so writeUEFIMetadata's own
// cmdline.ReadSection calls fail for real (not mocked) - a genuine
// metadata-generation failure, exercising the real rollback path.
func TestWriteUEFIGenerationWithRollback_MetadataFailureRestoresLoaderToo(t *testing.T) {
	mountpoint := newUEFIMountpoint(t)
	loaderRel := filepath.Join(layout.EFIBootDir, layout.EFILoaderName("x86_64"))

	oldLoader := []byte("OLD-loader-bytes-generation-N")
	oldMetadata := metadata.Encode(metadata.Manifest{
		Version: "0.1.0", BuildStamp: "OLD", Arch: "x86_64",
		Kernel: "6.18.0-old", OpenZFS: "2.3.0-old",
		KernelSHA256: "aaaa", InitrdSHA256: "bbbb",
	})
	if err := os.WriteFile(filepath.Join(mountpoint, loaderRel), oldLoader, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mountpoint, layout.MetadataFile), oldMetadata, 0o644); err != nil {
		t.Fatal(err)
	}

	newLoader := []byte("NEW-loader-bytes-generation-N-plus-1")
	// A local file that is NOT a valid PE image - cmdline.ReadSection will
	// fail on it for real, driving a genuine metadata-generation failure.
	notAnEFI := filepath.Join(t.TempDir(), "not-a-real-efi-file")
	if err := os.WriteFile(notAnEFI, []byte("this is not a PE/EFI file"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := writeUEFIGenerationWithRollback(mountpoint, "x86_64", newLoader, notAnEFI, cmdline.Info{Version: "0.1.0", BuildStamp: "NEW"})
	if err == nil {
		t.Fatal("want an error (metadata generation must fail against a non-PE efiPath), got nil")
	}

	gotLoader, rerr := os.ReadFile(filepath.Join(mountpoint, loaderRel))
	if rerr != nil {
		t.Fatalf("reading back the loader after rollback: %v", rerr)
	}
	if !bytes.Equal(gotLoader, oldLoader) {
		if bytes.Equal(gotLoader, newLoader) {
			t.Fatal("the UEFI loader was left as the NEW build after metadata generation failed - rollback did not run, the ESP now holds a mixed old/new generation")
		}
		t.Fatalf("loader after rollback = %q, want the original %q", gotLoader, oldLoader)
	}

	gotMetadata, rerr := os.ReadFile(filepath.Join(mountpoint, layout.MetadataFile))
	if rerr != nil {
		t.Fatalf("reading back METADATA after rollback: %v", rerr)
	}
	if !bytes.Equal(gotMetadata, oldMetadata) {
		t.Fatalf("METADATA after rollback = %q, want the original", gotMetadata)
	}
}

// TestWriteUEFIGenerationWithRollback_FreshInstallMetadataFailureLeavesCleanESP
// covers the fresh-install case specifically: no loader/metadata existed
// before this attempt at all, so a failure must remove the newly-written
// loader too, not just leave metadata missing (a nil backup entry means
// "didn't exist", which rollback must interpret as "remove", not "leave
// whatever's there now").
func TestWriteUEFIGenerationWithRollback_FreshInstallMetadataFailureLeavesCleanESP(t *testing.T) {
	mountpoint := newUEFIMountpoint(t)
	loaderRel := filepath.Join(layout.EFIBootDir, layout.EFILoaderName("aarch64"))

	newLoader := []byte("brand-new-loader-bytes")
	notAnEFI := filepath.Join(t.TempDir(), "not-a-real-efi-file")
	if err := os.WriteFile(notAnEFI, []byte("garbage, not PE"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := writeUEFIGenerationWithRollback(mountpoint, "aarch64", newLoader, notAnEFI, cmdline.Info{Version: "0.1.0", BuildStamp: "NEW"})
	if err == nil {
		t.Fatal("want an error, got nil")
	}

	if _, statErr := os.Stat(filepath.Join(mountpoint, loaderRel)); !os.IsNotExist(statErr) {
		t.Errorf("loader after a failed fresh install: want removed (nothing existed before), stat err=%v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(mountpoint, layout.MetadataFile)); !os.IsNotExist(statErr) {
		t.Errorf("METADATA after a failed fresh install: want removed (nothing existed before), stat err=%v", statErr)
	}
}

// A full happy-path success test (a real write producing a real,
// deep-inspected metadata manifest) would need a genuinely valid PE/
// COFF fixture with named .cmdline/.linux/.initrd sections for
// cmdline.ReadSection to parse - this repo has no existing PE-writing
// test infrastructure anywhere (internal/cmdline's own tests only
// exercise the string-parsing logic, never Read()/ReadSection()
// against a real file), and hand-rolling one here would be a large,
// separate, error-prone undertaking for marginal extra confidence:
// the metadata GENERATION logic itself (deepMetadataFor) is already
// thoroughly tested via the BIOS-path tests in metadata_test.go using
// the same underlying kernelinfo/initrdinfo calls, and WriteLoader/
// VerifyLoader's own write/verify mechanics are already covered by
// internal/uefiboot's own test suite. The two failure-path tests above
// are what actually prove this fix - the rollback wiring - and don't
// need a valid PE file to do it.
