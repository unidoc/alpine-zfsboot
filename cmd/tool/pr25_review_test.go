package main

// Regression tests for the PR #25 review findings (F1, F5, F6, F9, F14, F15, F16).

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unidoc/alpine-zfsboot/internal/layout"
	"github.com/unidoc/alpine-zfsboot/internal/payloadsum"
)

// needX86BIOS skips a test that builds the x86_64 BIOS payload (stage1/stage2, CHECKSUM, BLKSUM): the
// fixtures assume it, so on another architecture the test would fail for the wrong reason.
func needX86BIOS(t *testing.T) {
	t.Helper()
	if detectArch() != "x86_64" {
		t.Skipf("the BIOS payload fixtures are x86_64 (this is %s)", detectArch())
	}
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}

func verifyOut(t *testing.T) (int, string) {
	t.Helper()
	var code int
	out := captureOutput(t, func() { code = runVerify("/", "", hostOpts{}, false, false, false, verifyFiles{}) })
	return code, out
}

// F5: int13chunk set while member B is missing; B returns: same
// generation, different CMDLINE. 'update' (as verify advises) must rewrite B.
func TestUpdate_HealsSettingWrittenWhileMemberWasMissing(t *testing.T) {
	_, a, b := markedMirror(t)
	b.present = false
	captureOutput(t, func() {
		forEachMember("/", "", hostOpts{}, func(t *target) error { return setInt13Chunk(t.mountpoint, "8") })
	})
	b.present = true
	if vcode, vout := verifyOut(t); vcode == 0 {
		t.Fatalf("setup: verify should see B DIFFERS\n%s", vout)
	}
	ucode, uout := runUpdateCapture(t, hostOpts{}, biosBuild(t, "20261001T000000Z", 1), biosOpts{integrity: "warn"})
	if ucode != 0 {
		t.Fatalf("update: exit %d\n%s", ucode, uout)
	}
	if !strings.Contains(readESP(t, b, layout.CmdlineFile), "int13chunk=8") {
		t.Errorf("B still lacks int13chunk=8 after 'update'\n%s", uout)
	}
	if vcode, vout := verifyOut(t); vcode != 0 {
		t.Errorf("verify after update: exit %d\n%s", vcode, lastLines(vout, 8))
	}
	if memberOf(t, a).Generation != memberOf(t, b).Generation {
		t.Errorf("generations differ after the repair: A %d, B %d", memberOf(t, a).Generation, memberOf(t, b).Generation)
	}
}

// F5: B's KERNEL corrupted (bit rot), same build: update rewrites it from A.
func TestUpdate_RepairsCorruptMemberAtSameBuild(t *testing.T) {
	_, a, b := markedMirror(t)
	p := filepath.Join(b.dir, layout.KernelFile)
	k, _ := os.ReadFile(p)
	k[len(k)-1] ^= 0xff
	os.WriteFile(p, k, 0o644)
	if vcode, _ := verifyOut(t); vcode == 0 {
		t.Fatal("setup: verify should fail on the corrupted KERNEL")
	}
	ucode, uout := runUpdateCapture(t, hostOpts{}, biosBuild(t, "20261001T000000Z", 1), biosOpts{integrity: "warn"})
	if ucode != 0 {
		t.Fatalf("update: exit %d\n%s", ucode, uout)
	}
	if got, _ := os.ReadFile(p); string(got) != readESP(t, a, layout.KernelFile) {
		t.Errorf("B's corrupted KERNEL left in place by update\n%s", uout)
	}
	if vcode, vout := verifyOut(t); vcode != 0 {
		t.Errorf("verify after update: exit %d\n%s", vcode, lastLines(vout, 8))
	}
}

// F5: the damaged copy is never spread - when the best member itself fails
// its CHECKSUM, update refuses to copy it over the other one.
func TestUpdate_DoesNotCopyFromDamagedSource(t *testing.T) {
	needX86BIOS(t)
	_, a, b := markedMirror(t)
	p := filepath.Join(a.dir, layout.KernelFile)
	k, _ := os.ReadFile(p)
	k[len(k)-1] ^= 0xff
	os.WriteFile(p, k, 0o644)
	good := readESP(t, b, layout.KernelFile)
	ucode, uout := runUpdateCapture(t, hostOpts{}, biosBuild(t, "20261001T000000Z", 1), biosOpts{integrity: "warn"})
	if ucode == 0 || !strings.Contains(uout, "not copying a payload that cannot be verified") {
		t.Errorf("want a refusal, got exit %d\n%s", ucode, uout)
	}
	if readESP(t, b, layout.KernelFile) != good {
		t.Error("B's good KERNEL was overwritten from the damaged A")
	}
}

// F6: CMDLINE alpine-zfsboot.integrity= wins over CHECKSUM's MODE at boot;
// verify judges a CHECKSUM mismatch by that effective mode, both ways.
func TestVerify_JudgesChecksumMismatchByEffectiveIntegrityMode(t *testing.T) {
	needX86BIOS(t)
	for _, c := range []struct {
		name, cmdlineMode, sumMode string
		refuses                    bool
	}{
		{"cmdline enforce beats MODE warn", "enforce", payloadsum.ModeWarn, true},
		{"cmdline warn beats MODE enforce", "warn", payloadsum.ModeEnforce, false},
		{"no override: MODE enforce", "", payloadsum.ModeEnforce, true},
		{"no override: MODE warn", "", payloadsum.ModeWarn, false},
		{"invalid override is ignored (stage2 ignores it)", "bogus", payloadsum.ModeEnforce, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, a, _ := markedMirror(t)
			if c.cmdlineMode != "" {
				cl := strings.Replace(readESP(t, a, layout.CmdlineFile), "\n", " alpine-zfsboot.integrity="+c.cmdlineMode+"\n", 1)
				os.WriteFile(filepath.Join(a.dir, layout.CmdlineFile), []byte(cl), 0o644)
			}
			raw, _ := os.ReadFile(filepath.Join(a.dir, layout.PayloadSumFile))
			m, err := payloadsum.Decode(raw)
			if err != nil {
				t.Fatal(err)
			}
			m.Mode = c.sumMode
			m.Kernel.SHA256[0] ^= 1
			os.WriteFile(filepath.Join(a.dir, layout.PayloadSumFile), payloadsum.Encode(m), 0o644)
			_, vout := verifyOut(t)
			warned := strings.Contains(vout, "does not refuse it")
			if c.refuses && warned {
				t.Errorf("verify calls a mismatch harmless although stage2 refuses to boot it\n%s", vout)
			}
			if !c.refuses && !warned {
				t.Errorf("verify treats a mismatch as fatal although stage2 boots it\n%s", vout)
			}
		})
	}
}

// F9: the int13chunk command must not grow CMDLINE past what stage2 accepts.
func TestInt13Chunk_RefusesCmdlineOverLimit(t *testing.T) {
	d := t.TempDir()
	os.MkdirAll(filepath.Join(d, layout.ESPDir), 0o755)
	line := "root=ZFS=zroot/ROOT/alpine ro " + strings.Repeat("x", 500-31) + "\n"
	p := filepath.Join(d, layout.CmdlineFile)
	os.WriteFile(p, []byte(line), 0o644)
	err := setInt13Chunk(d, "127")
	if err == nil || !strings.Contains(err.Error(), "accepts at most") {
		t.Fatalf("want a length refusal, got %v", err)
	}
	if got, _ := os.ReadFile(p); string(got) != line {
		t.Errorf("CMDLINE was modified (%d bytes)", len(got))
	}
}

// F1: a host installed with an explicit list: 'esp add C' must put C into
// the list on every member, so the next update still updates C.
func TestESPAdd_OnListedHostJoinsTheList(t *testing.T) {
	f := newFakeHost(t, false)
	a := f.addESP("AAAA-0001", "/dev/sda1")
	b := f.addESP("BBBB-0002", "/dev/sdb1")
	c := f.addESP("CCCC-0003", "/dev/sdc1")
	root := f.mountAsRoot(a)
	os.Setenv(envESPUUIDs, "AAAA-0001,BBBB-0002")
	if code, out := installBIOSMirror(t, f, root, hostOpts{}); code != 0 {
		t.Fatalf("install %d\n%s", code, out)
	}
	os.Unsetenv(envESPUUIDs)
	f.mounts = map[string]string{}
	var code int
	out := captureOutput(t, func() { code = runESPAdd("/", "", hostOpts{}, "CCCC-0003", true) })
	if code != 0 {
		t.Fatalf("esp add: exit %d\n%s", code, out)
	}
	for _, e := range []*fakeESP{a, b, c} {
		if l := configListOf(readESP(t, e, layout.ConfigFile)); l != "AAAA-0001,BBBB-0002,CCCC-0003" {
			t.Errorf("%s: config list %q", e.uuid, l)
		}
		if l := cmdlineValue(readESP(t, e, layout.CmdlineFile), layout.ESPUUIDsKey); l != "AAAA-0001,BBBB-0002,CCCC-0003" {
			t.Errorf("%s: CMDLINE list %q", e.uuid, l)
		}
	}
	ucode, uout := runUpdateCapture(t, hostOpts{}, biosBuild(t, "20261005T000000Z", 2), biosOpts{integrity: "warn"})
	if ucode != 0 {
		t.Fatalf("update: exit %d\n%s", ucode, uout)
	}
	for _, e := range []*fakeESP{a, b, c} {
		if !strings.Contains(readESP(t, e, layout.CmdlineFile), "20261005T000000Z") {
			t.Errorf("%s was not updated after 'esp add'", e.uuid)
		}
	}
}

// F1: 'update --add-esp' goes through the same path.
func TestUpdateAddESP_OnListedHostJoinsTheList(t *testing.T) {
	f := newFakeHost(t, false)
	a := f.addESP("AAAA-0001", "/dev/sda1")
	b := f.addESP("BBBB-0002", "/dev/sdb1")
	c := f.addESP("CCCC-0003", "/dev/sdc1")
	root := f.mountAsRoot(a)
	os.Setenv(envESPUUIDs, "AAAA-0001,BBBB-0002")
	if code, out := installBIOSMirror(t, f, root, hostOpts{}); code != 0 {
		t.Fatalf("install %d\n%s", code, out)
	}
	os.Unsetenv(envESPUUIDs)
	f.mounts = map[string]string{}
	ucode, uout := runUpdateCapture(t, hostOpts{addESP: "CCCC-0003"}, biosBuild(t, "20261001T000000Z", 1), biosOpts{integrity: "warn"})
	if ucode != 0 {
		t.Fatalf("update --add-esp: exit %d\n%s", ucode, uout)
	}
	for _, e := range []*fakeESP{a, b, c} {
		if l := configListOf(readESP(t, e, layout.ConfigFile)); l != "AAAA-0001,BBBB-0002,CCCC-0003" {
			t.Errorf("%s: config list %q", e.uuid, l)
		}
		if l := cmdlineValue(readESP(t, e, layout.CmdlineFile), layout.ESPUUIDsKey); l != "AAAA-0001,BBBB-0002,CCCC-0003" {
			t.Errorf("%s: CMDLINE list %q", e.uuid, l)
		}
	}
	ucode, uout = runUpdateCapture(t, hostOpts{}, biosBuild(t, "20261005T000000Z", 2), biosOpts{integrity: "warn"})
	if ucode != 0 || !strings.Contains(readESP(t, c, layout.CmdlineFile), "20261005T000000Z") {
		t.Errorf("C not updated by the next update (exit %d)\n%s", ucode, uout)
	}
}

func configListOf(config string) string {
	for _, l := range strings.Split(config, "\n") {
		if v, ok := strings.CutPrefix(l, layout.ESPUUIDsKey+"="); ok {
			return v
		}
	}
	return ""
}

// F14a: an adopted ESP (possibly stale, copied by hand) must never tie with
// the existing members for the best source.
func TestESPAdopt_AdoptedESPNeverTiesForBestSource(t *testing.T) {
	f, a, b := markedMirror(t)
	c := f.addESP("CCCC-0003", "/dev/sdc1")
	// a hand-copied, stale tree: A's files without the MEMBER marker
	if err := copyDir(filepath.Join(a.dir, layout.ESPDir), filepath.Join(c.dir, layout.ESPDir)); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(c.dir, layout.MemberFile))
	// make it recognisably different, as stale content would be
	os.WriteFile(filepath.Join(c.dir, layout.ConfigFile), []byte("stale=1\n"), 0o644)
	var code int
	out := captureOutput(t, func() { code = runESPAdopt([]string{"CCCC-0003"}, true, false) })
	if code != 0 {
		t.Fatalf("adopt: exit %d\n%s", code, out)
	}
	ga, gb, gc := memberOf(t, a).Generation, memberOf(t, b).Generation, memberOf(t, c).Generation
	if gc >= ga || gc >= gb {
		t.Errorf("adopted C has generation %d, existing members %d/%d: it can tie for best source", gc, ga, gb)
	}
	h, err := discoverHost("/", "", true, hostOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.cleanup()
	if src := h.bestSource(); src == nil || src.info.UUID == "CCCC-0003" {
		t.Errorf("best source is the adopted ESP: %v", src)
	}
	// and update brings it in line instead of spreading its config
	ucode, uout := runUpdateCapture(t, hostOpts{}, biosBuild(t, "20261001T000000Z", 1), biosOpts{integrity: "warn"})
	if ucode != 0 {
		t.Fatalf("update: exit %d\n%s", ucode, uout)
	}
	if readESP(t, a, layout.ConfigFile) == "stale=1\n" || readESP(t, c, layout.ConfigFile) != readESP(t, a, layout.ConfigFile) {
		t.Errorf("stale config spread or not repaired: A %q, C %q", readESP(t, a, layout.ConfigFile), readESP(t, c, layout.ConfigFile))
	}
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, info.Mode())
	})
}

// F14b: CHECKSUM removed, BLKSUM removal fails: the old payload and both
// manifests come back (enforce must not silently become "unverified").
func TestWritePayload_BlkSumRemovalFailureRestoresChecksum(t *testing.T) {
	_, a, _ := markedMirror(t)
	old := map[string]string{}
	for _, rel := range []string{layout.KernelFile, layout.InitrdFile, layout.CmdlineFile, layout.MetadataFile, layout.PayloadSumFile, layout.BlkSumFile} {
		old[rel] = readESP(t, a, rel)
	}
	orig := removeManifestFile
	t.Cleanup(func() { removeManifestFile = orig })
	removeManifestFile = func(path string) error {
		if filepath.Dir(path) == filepath.Join(a.dir, layout.ESPDir) && filepath.Base(path) == filepath.Base(layout.BlkSumFile) {
			return errors.New("injected BLKSUM removal error")
		}
		return orig(path)
	}
	var err error
	captureOutput(t, func() {
		err = writePayloadWithRollback(a.dir, detectArch(), "9.9.9", "20261009T000000Z", buildFakeBzImage("6.18.0-0-lts", 9), buildFakeInitrdBytes(t, "2.3.4"), []byte("c\n"), payloadsum.ModeWarn)
	})
	if err == nil || !strings.Contains(err.Error(), "injected BLKSUM removal error") {
		t.Fatalf("want the injected failure, got %v", err)
	}
	for rel, want := range old {
		if got := readESP(t, a, rel); got != want {
			t.Errorf("%s was not restored", rel)
		}
	}
}

// F15: the setting reached B only (A was away). A is first in host order, but B is the newer copy: update
// must bring A to the setting, never revert B to A's old CMDLINE.
func TestUpdate_KeepsSettingWrittenWhileFirstMemberWasMissing(t *testing.T) {
	_, a, b := markedMirror(t)
	a.present = false
	captureOutput(t, func() {
		forEachMember("/", "", hostOpts{}, func(t *target) error { return setInt13Chunk(t.mountpoint, "8") })
	})
	a.present = true
	if !strings.Contains(readESP(t, b, layout.CmdlineFile), "int13chunk=8") || strings.Contains(readESP(t, a, layout.CmdlineFile), "int13chunk=8") {
		t.Fatal("setup: B should carry int13chunk=8 and A not")
	}
	if memberOf(t, b).Generation <= memberOf(t, a).Generation {
		t.Errorf("the member that was written must be the newer one: A generation %d, B generation %d", memberOf(t, a).Generation, memberOf(t, b).Generation)
	}
	ucode, uout := runUpdateCapture(t, hostOpts{}, biosBuild(t, "20261001T000000Z", 1), biosOpts{integrity: "warn"})
	if ucode != 0 {
		t.Fatalf("update: exit %d\n%s", ucode, uout)
	}
	for _, e := range []*fakeESP{a, b} {
		if !strings.Contains(readESP(t, e, layout.CmdlineFile), "int13chunk=8") {
			t.Errorf("a member lacks int13chunk=8 after update\n%s", lastLines(uout, 8))
		}
	}
	if memberOf(t, a).Generation != memberOf(t, b).Generation {
		t.Errorf("generations differ after update: A %d, B %d", memberOf(t, a).Generation, memberOf(t, b).Generation)
	}
}

// The same setting written on every member leaves them at one generation.
func TestSettingOnEveryMemberKeepsOneGeneration(t *testing.T) {
	_, a, b := markedMirror(t)
	captureOutput(t, func() {
		forEachMember("/", "", hostOpts{}, func(t *target) error { return setInt13Chunk(t.mountpoint, "8") })
	})
	if memberOf(t, a).Generation != memberOf(t, b).Generation {
		t.Errorf("generations differ: A %d, B %d", memberOf(t, a).Generation, memberOf(t, b).Generation)
	}
}

// F16: a source whose CHECKSUM is missing or unreadable is not a repair source (the guard fails closed).
func TestUpdate_RefusesToRepairFromAnUnverifiableSource(t *testing.T) {
	needX86BIOS(t)
	for _, tc := range []struct {
		name string
		mod  func(p string)
	}{
		{"CHECKSUM removed", func(p string) { os.Remove(p) }},
		{"CHECKSUM garbage", func(p string) { os.WriteFile(p, []byte("garbage\n"), 0o644) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, a, b := markedMirror(t)
			p := filepath.Join(a.dir, layout.KernelFile)
			k, _ := os.ReadFile(p)
			k[len(k)-1] ^= 0xff
			os.WriteFile(p, k, 0o644)
			tc.mod(filepath.Join(a.dir, layout.PayloadSumFile))
			good := readESP(t, b, layout.KernelFile)
			ucode, uout := runUpdateCapture(t, hostOpts{}, biosBuild(t, "20261001T000000Z", 1), biosOpts{integrity: "warn"})
			if readESP(t, b, layout.KernelFile) != good {
				t.Errorf("B's good KERNEL was overwritten from an unverifiable A (update exit %d)\n%s", ucode, lastLines(uout, 8))
			}
			if ucode == 0 {
				t.Errorf("update must not report success when it refused to repair (exit 0)\n%s", lastLines(uout, 8))
			}
		})
	}
}
