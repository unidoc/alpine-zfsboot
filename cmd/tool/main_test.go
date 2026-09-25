package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unidoc/alpine-zfsboot/internal/biosboot"
	"github.com/unidoc/alpine-zfsboot/internal/layout"
)

// captureStderr runs fn with os.Stderr redirected to a pipe and
// returns everything written to it - used to prove what a CRITICAL-
// failure rollback path actually prints, not just that it returns
// (these functions are void - fmt.Fprintln(os.Stderr, ...) is their
// only observable output).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = orig
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// checkArchMatch, backupPrevious, and hardLink moved to
// internal/uefiboot (see that package's own tests, ported verbatim
// from this file's earlier versions) as part of generalizing this
// tool's UEFI logic so install/update/verify can all call it, not
// just update. What's left here is this file's own glue: arch
// detection and the report struct's print/pass-fail logic.

// TestRollbackPayload_CriticalFailureNeverPrintsSuccessLine is the
// regression test for F22 (unidoc-alip's PR #5 review): rollbackPayload
// used to print "restored the previous boot payload ... " unconditionally,
// even after one of its own restore attempts had already printed a real
// CRITICAL failure - an operator could see both lines back to back, the
// second directly undercutting the seriousness of the first ("do not
// reboot without investigating further" immediately followed by
// "restored ... after a failed write", which reads like recovery
// succeeded). layout.KernelFile is pre-created as a DIRECTORY - a real,
// structural way to make espconfig.WriteFile's own final rename fail
// (can't rename a regular file over an existing directory), not a mock.
func TestRollbackPayload_CriticalFailureNeverPrintsSuccessLine(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)
	if err := os.MkdirAll(filepath.Join(mountpoint, layout.KernelFile), 0o755); err != nil {
		t.Fatal(err)
	}
	backup := payloadBackup{kernel: []byte("old-kernel-bytes")}

	out := captureStderr(t, func() {
		rollbackPayload(mountpoint, backup)
	})
	if !strings.Contains(out, "CRITICAL") {
		t.Errorf("output = %q, want it to contain a CRITICAL failure line", out)
	}
	if strings.Contains(out, "restored the previous boot payload") {
		t.Errorf("output = %q, want it to NOT claim success after a CRITICAL failure", out)
	}
}

// TestRollbackUEFIGeneration_CriticalFailureNeverPrintsSuccessLine is
// rollbackUEFIGeneration's own identical regression test - see
// TestRollbackPayload_CriticalFailureNeverPrintsSuccessLine's own
// comment for the full reasoning, same bug, same fix, same shape.
func TestRollbackUEFIGeneration_CriticalFailureNeverPrintsSuccessLine(t *testing.T) {
	mountpoint := newUEFIMountpoint(t)
	loaderRel := "EFI/BOOT/BOOTX64.EFI"
	if err := os.MkdirAll(filepath.Join(mountpoint, loaderRel), 0o755); err != nil {
		t.Fatal(err)
	}
	backup := uefiGenerationBackup{loader: []byte("old-efi-bytes")}

	out := captureStderr(t, func() {
		rollbackUEFIGeneration(mountpoint, loaderRel, backup)
	})
	if !strings.Contains(out, "CRITICAL") {
		t.Errorf("output = %q, want it to contain a CRITICAL failure line", out)
	}
	if strings.Contains(out, "restored the previous UEFI loader") {
		t.Errorf("output = %q, want it to NOT claim success after a CRITICAL failure", out)
	}
}

func TestDetectArch(t *testing.T) {
	// Only the branch matching this test's own build environment is
	// meaningfully exercised here (GOARCH is fixed at compile time,
	// not swappable at runtime) - this at least confirms it never
	// returns empty for a real Go-supported arch.
	if got := detectArch(); got == "" {
		t.Error("detectArch() returned an empty string")
	}
}

func TestOrNone(t *testing.T) {
	if got := orNone(""); got != "(none)" {
		t.Errorf("orNone(\"\") = %q, want \"(none)\"", got)
	}
	if got := orNone("60"); got != "60" {
		t.Errorf("orNone(\"60\") = %q, want \"60\"", got)
	}
}

func TestReportErrs(t *testing.T) {
	clean := report{}
	if errs := clean.errs(); len(errs) != 0 {
		t.Errorf("a report with no error fields set: errs() = %v, want none", errs)
	}

	broken := report{
		stage1Err:  errors.New("stage1 problem"),
		cmdlineErr: errors.New("cmdline problem"),
	}
	if errs := broken.errs(); len(errs) != 2 {
		t.Errorf("a report with 2 error fields set: errs() = %v, want 2 entries", errs)
	}
}

// TestReportErrs_IncludesMetadataErr is the regression test for F22
// (unidoc-alip's PR #5 review): metadataErr was missing from errs()'s
// own field list entirely, despite print()'s own "Metadata:" line
// already surfacing it as a real, populated error - a report whose
// ONLY problem is a present-but-undecodable metadata manifest used to
// report zero errors from this collector.
func TestReportErrs_IncludesMetadataErr(t *testing.T) {
	r := report{metadataErr: errors.New("metadata problem")}
	errs := r.errs()
	if len(errs) != 1 {
		t.Fatalf("a report with only metadataErr set: errs() = %v, want exactly 1 entry", errs)
	}
	if errs[0].Error() != "metadata problem" {
		t.Errorf("errs()[0] = %q, want \"metadata problem\"", errs[0].Error())
	}
}

// bootCompatVerdict is print()'s own "Boot compatible:" decision (see its
// own doc comment) - real zfscompat.CheckZFSCompat data underneath, not
// fakeable fixtures, since the embedded compatibility.d files ARE the real
// vendored upstream data (see zfscompat.go's own package comment).
func TestBootCompatVerdict(t *testing.T) {
	cases := []struct {
		name       string
		r          report
		wantStatus string
	}{
		{
			name:       "no OpenZFS version",
			r:          report{openZFSVersionErr: errors.New("boom")},
			wantStatus: "UNKNOWN",
		},
		{
			name: "no imported pool",
			r: report{
				openZFSVersion: "2.4.4-1",
				zfs:            zfsCompat{poolErr: errors.New("no pool")},
			},
			wantStatus: "UNKNOWN",
		},
		{
			name:       "unrecognized OpenZFS version string",
			r:          report{openZFSVersion: "not-a-version"},
			wantStatus: "UNKNOWN",
		},
		{
			name:       "unknown release line",
			r:          report{openZFSVersion: "9.9.9-1"},
			wantStatus: "UNKNOWN",
		},
		{
			name: "verified",
			r: report{
				openZFSVersion: "2.4.4-1",
				zfs:            zfsCompat{activeFeatures: []string{"async_destroy"}},
			},
			wantStatus: "VERIFIED",
		},
		{
			name: "unsupported active feature",
			r: report{
				openZFSVersion: "2.4.4-1",
				zfs:            zfsCompat{activeFeatures: []string{"totally_made_up_feature"}},
			},
			wantStatus: "NO",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, detail := bootCompatVerdict(c.r)
			if status != c.wantStatus {
				t.Errorf("bootCompatVerdict() status = %q, want %q (detail: %q)", status, c.wantStatus, detail)
			}
			wantDetail := status != "VERIFIED"
			if hasDetail := detail != ""; hasDetail != wantDetail {
				t.Errorf("bootCompatVerdict() detail = %q, want non-empty=%v for status %q", detail, wantDetail, status)
			}
		})
	}
}

// TestAppendCompatErr is the regression test for a real bug found in a
// full source audit: bootCompatVerdict() was correctly wired into
// print(), but verify's own pass/fail decision came exclusively from
// r.errs(), which never included the compatibility verdict at all - so
// "Boot compatible: NO" / "Unsupported active: ..." could print directly
// above a "verify: OK" that contradicted it. Proves the three-way
// contract the fix establishes: VERIFIED and UNKNOWN both leave errs
// unchanged (UNKNOWN stays advisory, on purpose - see appendCompatErr's
// own doc comment), NO always adds exactly one error.
func TestAppendCompatErr(t *testing.T) {
	cases := []struct {
		name      string
		r         report
		wantAdded bool
	}{
		{
			name: "VERIFIED does not fail verify",
			r: report{
				openZFSVersion: "2.4.4-1",
				zfs:            zfsCompat{activeFeatures: []string{"async_destroy"}},
			},
			wantAdded: false,
		},
		{
			name:      "UNKNOWN (no OpenZFS version) does not fail verify",
			r:         report{openZFSVersionErr: errors.New("boom")},
			wantAdded: false,
		},
		{
			name:      "UNKNOWN (no imported pool) does not fail verify",
			r:         report{openZFSVersion: "2.4.4-1", zfs: zfsCompat{poolErr: errors.New("no pool")}},
			wantAdded: false,
		},
		{
			name:      "UNKNOWN (unrecognized release line) does not fail verify",
			r:         report{openZFSVersion: "9.9.9-1"},
			wantAdded: false,
		},
		{
			name: "NO fails verify",
			r: report{
				openZFSVersion: "2.4.4-1",
				zfs:            zfsCompat{activeFeatures: []string{"totally_made_up_feature"}},
			},
			wantAdded: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := []error{errors.New("unrelated pre-existing error")}
			got := appendCompatErr(before, c.r)
			gotAdded := len(got) == len(before)+1
			if gotAdded != c.wantAdded {
				t.Errorf("appendCompatErr() added an error = %v, want %v (result: %v)", gotAdded, c.wantAdded, got)
			}
			// The pre-existing error must never be dropped or reordered
			// away - appendCompatErr only ever appends.
			if got[0] != before[0] {
				t.Errorf("appendCompatErr() must not disturb existing errors, got %v", got)
			}
		})
	}
}

// TestWriteBIOSStagesWithRollback_Stage2FailureRestoresStage1 is the
// regression test for a real bug found in a full source audit:
// biosboot.WriteStage1/WriteStage2 both return the region's own
// previous content SPECIFICALLY so a caller can restore it on a later
// failure (see biosboot's own doc comment - this is not incidental),
// but every real call site in this file used to discard that return
// value outright (`if _, err := biosboot.WriteStage1(...)`). So a disk
// whose stage1 write succeeded and whose stage2 write then failed was
// left with a NEW stage1 and a destroyed/never-updated stage2 - an
// architecturally promised but never-implemented rollback.
//
// The disk fixture is deliberately sized to make WriteStage2 fail at
// its own FIRST step (backing up the current stage2 region before
// zeroing it) via a genuine short read, not a permission trick (which
// could behave differently if a test ever ran as root) - big enough for
// CheckStage2ExtentFree's own GPT/MBR preflight to pass cleanly (a
// 0x55AA boot signature, no partition entries - the same "no
// partition-table shape at all" case internal/integration's own
// TestBIOSInstall_AllowsDiskWithNoPartitionTableEntryAtAll already
// proves passes), but too short to contain the full 32 KiB stage2
// extent (LBA 34, so byte offset 17408) at all.
func TestWriteBIOSStagesWithRollback_Stage2FailureRestoresStage1(t *testing.T) {
	disk := filepath.Join(t.TempDir(), "disk.img")

	const diskSize = 20000 // > 17408 (stage2's own byte offset) but < 17408+32768 (its full extent)
	buf := make([]byte, diskSize)
	buf[510], buf[511] = 0x55, 0xAA
	if err := os.WriteFile(disk, buf, 0o644); err != nil {
		t.Fatal(err)
	}

	oldStage1 := bytes.Repeat([]byte{0xAA}, layout.Stage1Bytes)
	if _, err := biosboot.WriteStage1(disk, oldStage1); err != nil {
		t.Fatalf("seeding the disk's original stage1: %v", err)
	}

	newStage1 := bytes.Repeat([]byte{0xBB}, layout.Stage1Bytes)
	newStage2 := bytes.Repeat([]byte{0xCC}, 9000)

	err := writeBIOSStagesWithRollback(disk, newStage1, newStage2)
	if err == nil {
		t.Fatal("expected an error (stage2's own backup-read should fail on this deliberately short disk), got nil")
	}
	if !strings.Contains(err.Error(), "writing stage2") {
		t.Errorf("error = %q, want it to mention \"writing stage2\"", err.Error())
	}

	got, err := biosboot.ReadStage1(disk)
	if err != nil {
		t.Fatalf("reading back stage1 after the failed operation: %v", err)
	}
	if !bytes.Equal(got, oldStage1) {
		if bytes.Equal(got, newStage1) {
			t.Fatal("stage1 was left as the NEW build after stage2 failed - rollback did not run at all")
		}
		t.Fatalf("stage1 after rollback does not match the original content: got %x..., want %x...", got[:8], oldStage1[:8])
	}
}

// TestWritePayloadWithRollback_MidWriteFailureRestoresFullPreviousGeneration
// is the regression test for a real gap a follow-up review found:
// espconfig.WritePayload writes KERNEL/INITRD/CMDLINE one at a time,
// each individually durable, but with no rollback ACROSS them - a
// failure partway used to leave whichever files had already succeeded
// as the NEW build, paired with whichever hadn't yet as the OLD one -
// a mismatched boot generation, not the clean all-or-nothing update
// the individual per-file durability made it look like.
//
// CMDLINE is deliberately pre-created as a DIRECTORY (not a file) -
// forces WritePayload's own third write (os.Rename of a temp file
// over the target) to fail with a real, deterministic "is a
// directory" error. No permission trick, no root dependency, same
// "structural, not simulated" failure-injection style
// TestWriteBIOSStagesWithRollback_Stage2FailureRestoresStage1 above
// already uses (a disk too short for stage2's own full extent, not a
// mocked error).
func TestWritePayloadWithRollback_MidWriteFailureRestoresFullPreviousGeneration(t *testing.T) {
	mountpoint := t.TempDir()
	dir := filepath.Join(mountpoint, layout.ESPDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	oldKernel := []byte("old-kernel-bytes")
	oldInitrd := []byte("old-initrd-bytes")
	if err := os.WriteFile(filepath.Join(mountpoint, layout.KernelFile), oldKernel, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mountpoint, layout.InitrdFile), oldInitrd, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(mountpoint, layout.CmdlineFile), 0o755); err != nil {
		t.Fatal(err)
	}

	newKernel := []byte("NEW-kernel-bytes")
	newInitrd := []byte("NEW-initrd-bytes")
	newCmdline := []byte("NEW-cmdline-bytes")

	err := writePayloadWithRollback(mountpoint, "x86_64", "0.1.0", "20260101T000000Z", newKernel, newInitrd, newCmdline)
	if err == nil {
		t.Fatal("expected an error (CMDLINE's own rename should fail - it's a directory), got nil")
	}

	gotKernel, err := os.ReadFile(filepath.Join(mountpoint, layout.KernelFile))
	if err != nil {
		t.Fatalf("reading back KERNEL after the failed update: %v", err)
	}
	if !bytes.Equal(gotKernel, oldKernel) {
		if bytes.Equal(gotKernel, newKernel) {
			t.Fatal("KERNEL was left as the NEW build after CMDLINE's write failed - rollback did not run, the ESP now holds a mixed old/new boot generation")
		}
		t.Fatalf("KERNEL after rollback = %q, want the original %q", gotKernel, oldKernel)
	}

	gotInitrd, err := os.ReadFile(filepath.Join(mountpoint, layout.InitrdFile))
	if err != nil {
		t.Fatalf("reading back INITRD after the failed update: %v", err)
	}
	if !bytes.Equal(gotInitrd, oldInitrd) {
		if bytes.Equal(gotInitrd, newInitrd) {
			t.Fatal("INITRD was left as the NEW build after CMDLINE's write failed - rollback did not run, the ESP now holds a mixed old/new boot generation")
		}
		t.Fatalf("INITRD after rollback = %q, want the original %q", gotInitrd, oldInitrd)
	}
}

// TestTrimStage1Asset is the regression test for a real bug found on
// real hardware (uniclus-01): install/update required the RAW stage1
// asset FILE itself be exactly layout.Stage1Bytes (440) - but every
// real stage1.bin build.sh has ever produced is layout.SectorSize
// (512), the full MBR sector (bios/Makefile's own stage1.bin target
// hard-requires exactly 512) - so a genuine, unmodified official
// release asset failed update/install outright with "stage1 is 512
// bytes, want exactly 440" on every real run, never just this one
// user's own case.
func TestTrimStage1Asset(t *testing.T) {
	t.Run("a full 512-byte MBR sector is trimmed to the 440-byte boot code region", func(t *testing.T) {
		raw := bytes.Repeat([]byte{0xAA}, layout.Stage1Bytes)
		raw = append(raw, bytes.Repeat([]byte{0xFF}, layout.SectorSize-layout.Stage1Bytes)...) // partition-table-shaped tail, deliberately different bytes
		got, err := trimStage1Asset(raw)
		if err != nil {
			t.Fatalf("trimStage1Asset on a real 512-byte asset: %v", err)
		}
		if len(got) != layout.Stage1Bytes {
			t.Fatalf("trimStage1Asset returned %d bytes, want exactly %d", len(got), layout.Stage1Bytes)
		}
		if !bytes.Equal(got, raw[:layout.Stage1Bytes]) {
			t.Error("trimStage1Asset did not return the FIRST 440 bytes unchanged - the actual boot code must never be altered, only the trailing partition-table region dropped")
		}
	})

	t.Run("an already-trimmed 440-byte slice passes through unchanged", func(t *testing.T) {
		raw := bytes.Repeat([]byte{0xBB}, layout.Stage1Bytes)
		got, err := trimStage1Asset(raw)
		if err != nil {
			t.Fatalf("trimStage1Asset on an already-trimmed 440-byte slice: %v", err)
		}
		if !bytes.Equal(got, raw) {
			t.Error("trimStage1Asset altered an already-correctly-sized 440-byte input")
		}
	})

	t.Run("any other length is refused, not silently truncated or padded", func(t *testing.T) {
		for _, n := range []int{0, 439, 441, 511, 513, 1024} {
			if _, err := trimStage1Asset(make([]byte, n)); err == nil {
				t.Errorf("trimStage1Asset(%d bytes): want an error, got nil", n)
			}
		}
	})
}

// TestCheckUpdateEligible is the regression test for a real bug found
// in a full source audit: update's disk-write target (derived from
// bootenv.FindESP(), see checkUpdateEligible's own doc comment for the
// exact rescue-USB scenario) had no cross-check against the disk
// actually being a real, previously-installed alpine-zfsboot target -
// only ever proven possible for UEFI (a real "which disk booted this"
// signal), not BIOS.
func TestCheckUpdateEligible(t *testing.T) {
	t.Run("disk with a real stage2 installed is eligible", func(t *testing.T) {
		disk := filepath.Join(t.TempDir(), "disk.img")
		buf := make([]byte, int(layout.Stage2LBA+layout.Stage2Sectors+10)*layout.SectorSize)
		buf[510], buf[511] = 0x55, 0xAA
		if err := os.WriteFile(disk, buf, 0o644); err != nil {
			t.Fatal(err)
		}
		realStage2 := []byte("alpine-zfsboot by UniDoc, version 0.1.0, build=2026-01-01T00:00:00Z\n")
		if _, err := biosboot.WriteStage2(disk, realStage2); err != nil {
			t.Fatalf("seeding a real stage2: %v", err)
		}
		if err := checkUpdateEligible(disk); err != nil {
			t.Errorf("checkUpdateEligible on a disk with a real, previously-installed stage2: %v", err)
		}
	})

	t.Run("disk with no alpine-zfsboot stage2 at all is refused (the rescue-USB case)", func(t *testing.T) {
		disk := filepath.Join(t.TempDir(), "disk.img")
		buf := make([]byte, int(layout.Stage2LBA+layout.Stage2Sectors+10)*layout.SectorSize)
		buf[510], buf[511] = 0x55, 0xAA
		if err := os.WriteFile(disk, buf, 0o644); err != nil {
			t.Fatal(err)
		}
		// Deliberately never wrote a real stage2 - a rescue USB or any
		// other foreign ESP-labeled device would look exactly like this:
		// a plausible partition table, zero content at LBA 34.
		if err := checkUpdateEligible(disk); err == nil {
			t.Error("checkUpdateEligible on a disk with no alpine-zfsboot stage2 at all: want an error, got nil")
		}
	})
}

// TestCheckBIOSUpToDate is the regression test for F16 (unidoc-alip's
// PR #5 follow-up review): BIOS update used to have no build
// comparison at all, unlike UEFI's own latest.BuildStamp <=
// local.BuildStamp check - `update -y` rewrote every artifact on every
// run, and a retracted/rolled-back "latest" release applied as a
// silent downgrade.
func TestCheckBIOSUpToDate(t *testing.T) {
	writeCmdline := func(t *testing.T, mountpoint, buildStamp string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(mountpoint, layout.ESPDir), 0o755); err != nil {
			t.Fatal(err)
		}
		content := fmt.Sprintf("root=ZFS=zroot/ROOT/default ro alpine-zfsboot.version=0.1.0 alpine-zfsboot.buildstamp=%s\n", buildStamp)
		if err := os.WriteFile(filepath.Join(mountpoint, layout.CmdlineFile), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("new build strictly newer than installed - not up to date", func(t *testing.T) {
		mountpoint := t.TempDir()
		writeCmdline(t, mountpoint, "20260101T000000Z")
		installed, upToDate := checkBIOSUpToDate(mountpoint, "20260201T000000Z")
		if upToDate {
			t.Errorf("checkBIOSUpToDate with a strictly newer build: want upToDate=false, got true (installed=%q)", installed)
		}
	})

	t.Run("new build identical to installed - up to date, refuses a same-build reinstall", func(t *testing.T) {
		mountpoint := t.TempDir()
		writeCmdline(t, mountpoint, "20260101T000000Z")
		installed, upToDate := checkBIOSUpToDate(mountpoint, "20260101T000000Z")
		if !upToDate || installed != "20260101T000000Z" {
			t.Errorf("checkBIOSUpToDate with the SAME build: want upToDate=true installed=20260101T000000Z, got upToDate=%v installed=%q", upToDate, installed)
		}
	})

	t.Run("new build OLDER than installed - up to date, refuses a silent downgrade", func(t *testing.T) {
		mountpoint := t.TempDir()
		writeCmdline(t, mountpoint, "20260201T000000Z")
		installed, upToDate := checkBIOSUpToDate(mountpoint, "20260101T000000Z")
		if !upToDate || installed != "20260201T000000Z" {
			t.Errorf("checkBIOSUpToDate with an OLDER build (a retracted/rolled-back release): want upToDate=true installed=20260201T000000Z, got upToDate=%v installed=%q", upToDate, installed)
		}
	})

	t.Run("no installed CMDLINE at all - skipped, never refused", func(t *testing.T) {
		mountpoint := t.TempDir() // no CMDLINE written at all
		installed, upToDate := checkBIOSUpToDate(mountpoint, "20260101T000000Z")
		if upToDate || installed != "" {
			t.Errorf("checkBIOSUpToDate with no installed CMDLINE: want upToDate=false installed=\"\", got upToDate=%v installed=%q - a pre-feature installation must never be blocked from updating", upToDate, installed)
		}
	})

	t.Run("installed CMDLINE has no buildstamp field at all - skipped, never refused", func(t *testing.T) {
		mountpoint := t.TempDir()
		if err := os.MkdirAll(filepath.Join(mountpoint, layout.ESPDir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(mountpoint, layout.CmdlineFile), []byte("root=ZFS=zroot/ROOT/default ro\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		installed, upToDate := checkBIOSUpToDate(mountpoint, "20260101T000000Z")
		if upToDate || installed != "" {
			t.Errorf("checkBIOSUpToDate with a buildstamp-less installed CMDLINE: want upToDate=false installed=\"\", got upToDate=%v installed=%q", upToDate, installed)
		}
	})
}

func TestSelectBootPool(t *testing.T) {
	cases := []struct {
		name     string
		bootPool string
		imported []string
		want     string
		wantErr  bool
	}{
		{
			name:     "single imported pool, boot pool unknown - unambiguous anyway",
			bootPool: "",
			imported: []string{"zroot"},
			want:     "zroot",
		},
		{
			name:     "single imported pool, boot pool known and matches",
			bootPool: "zroot",
			imported: []string{"zroot"},
			want:     "zroot",
		},
		{
			name:     "multiple imported pools, boot pool known - picks the RIGHT one, not the first",
			bootPool: "zroot",
			imported: []string{"backuppool", "zroot"},
			want:     "zroot",
		},
		{
			name:     "multiple imported pools, boot pool unknown - refuses to guess",
			bootPool: "",
			imported: []string{"backuppool", "zroot"},
			wantErr:  true,
		},
		{
			name:     "boot pool known but not actually imported - refuses",
			bootPool: "zroot",
			imported: []string{"backuppool"},
			wantErr:  true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := selectBootPool(c.bootPool, c.imported)
			if c.wantErr {
				if err == nil {
					t.Errorf("selectBootPool(%q, %v) = %q, want an error", c.bootPool, c.imported, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectBootPool(%q, %v): unexpected error: %v", c.bootPool, c.imported, err)
			}
			if got != c.want {
				t.Errorf("selectBootPool(%q, %v) = %q, want %q", c.bootPool, c.imported, got, c.want)
			}
		})
	}
}

// TestWithCleanup is the regression test for a real bug found in a
// full source audit: die() calls os.Exit(1), which never runs any
// pending defer - so every command's own `defer t.cleanup()` (unmounts
// the ESP, removes the temp mountpoint) was silently skipped on every
// die() call reached after discover() succeeded, leaking a live ESP
// mount. Tests the activeCleanup/withCleanup wiring directly rather
// than die() itself, since die()'s own os.Exit(1) would kill the test
// process along with everything it's trying to prove - the same
// os.Exit constraint every other test in this file already works
// around.
func TestWithCleanup(t *testing.T) {
	t.Cleanup(func() { activeCleanup = nil }) // don't leak into other tests

	called := 0
	deferFn := withCleanup(func() { called++ })

	if activeCleanup == nil {
		t.Fatal("withCleanup did not set activeCleanup - die() would find nothing to call")
	}
	// Simulates exactly what die() does on a failure reached after
	// discover() succeeded, without invoking its own os.Exit.
	activeCleanup()
	if called != 1 {
		t.Fatalf("activeCleanup() call count = %d, want 1", called)
	}

	// The deferred closure a real Run closure actually defers - confirm
	// it ALSO calls cleanup (the normal-return path) and resets
	// activeCleanup afterward (so a later die() in some other command
	// invocation can't double-call an already-unmounted cleanup).
	deferFn()
	if called != 2 {
		t.Fatalf("deferred closure call count = %d, want 2", called)
	}
	if activeCleanup != nil {
		t.Error("the deferred closure did not reset activeCleanup to nil")
	}
}

// TestRunActiveCleanupConcurrent is the regression test for the race
// main()'s new signal handler introduces: it calls runActiveCleanup()
// from its own goroutine while a Run closure's goroutine is concurrently
// setting/resetting activeCleanup via withCleanup - two goroutines
// touching the same var, where before this fix there was only ever one.
// `go test -race` only catches an unguarded access if something in the
// test actually exercises it concurrently; this test's job is to be
// that exercise, not just to assert a final call count.
func TestRunActiveCleanupConcurrent(t *testing.T) {
	t.Cleanup(func() {
		activeCleanupMu.Lock()
		activeCleanup = nil
		activeCleanupMu.Unlock()
	})

	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 500 {
			deferFn := withCleanup(func() { calls.Add(1) })
			deferFn()
		}
	}()
	for range 500 {
		runActiveCleanup()
	}
	<-done
}
