package main

// Mirrored boot (issue #24), end to end through the real command runners
// (runInstall/runUpdate/runVerify/runStatus/esp ...) - with directories
// standing in for mounted ESPs and image files for disks: no mount, no
// loop device, no root. Only discovery's I/O (scan, mount, disk-of-ESP)
// is faked; every write goes through the real writers.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unidoc/alpine-zfsboot/internal/biosboot"
	"github.com/unidoc/alpine-zfsboot/internal/bootenv"
	"github.com/unidoc/alpine-zfsboot/internal/cmdline"
	"github.com/unidoc/alpine-zfsboot/internal/espmember"
	"github.com/unidoc/alpine-zfsboot/internal/layout"
	"github.com/unidoc/alpine-zfsboot/internal/payloadsum"
	"github.com/unidoc/alpine-zfsboot/internal/release"
	"github.com/unidoc/alpine-zfsboot/internal/uefiboot"
)

type fakeESP struct {
	uuid, dev, dir, disk string
	present              bool
	mountFail            bool
}

type fakeHost struct {
	t        *testing.T
	esps     []*fakeESP
	uefi     bool
	proc     string
	mounts   map[string]string // canonical mountpoint -> dev
	zfsCalls []string
	zfsProp  string
}

func newFakeHost(t *testing.T, uefi bool) *fakeHost {
	t.Helper()
	f := &fakeHost{t: t, uefi: uefi, mounts: map[string]string{}, zfsProp: "-"}
	saved := []func(){}
	save := func(restore func()) { saved = append(saved, restore) }
	oScan, oMount, oDisk, oLayout, oFind, oVol, oUEFI, oProc, oZFS, oNow := scanESPs, mountESP, diskOfESP, detectDiskLayout, findMountAtPath, volumeUUID, isUEFI, readProcCmdline, runZFS, nowUTC
	save(func() {
		scanESPs, mountESP, diskOfESP, detectDiskLayout, findMountAtPath, volumeUUID, isUEFI, readProcCmdline, runZFS, nowUTC = oScan, oMount, oDisk, oLayout, oFind, oVol, oUEFI, oProc, oZFS, oNow
	})
	t.Cleanup(func() {
		for _, r := range saved {
			r()
		}
		os.Unsetenv(envESPUUIDs)
	})
	scanESPs = func() ([]bootenv.ESPInfo, error) {
		var out []bootenv.ESPInfo
		for _, e := range f.esps {
			if e.present {
				out = append(out, bootenv.ProbeMounted(bootenv.FATVolume{Dev: e.dev, UUID: e.uuid, Label: "EFI"}, e.dir))
			}
		}
		return out, nil
	}
	mountESP = func(dev string, readonly bool) (string, func(), error) {
		e := f.byDev(dev)
		if e == nil || !e.present || e.mountFail {
			return "", nil, errors.New("mount: injected failure for " + dev)
		}
		return e.dir, func() {}, nil
	}
	diskOfESP = func(dev string) (string, error) {
		if e := f.byDev(dev); e != nil && e.disk != "" {
			return e.disk, nil
		}
		return "", errors.New(dev + " is not a partition (test)")
	}
	detectDiskLayout = func(string) (bootenv.DiskLayout, error) { return bootenv.LayoutMSDOS, nil }
	findMountAtPath = func(p string) (string, string, bool) {
		dev, ok := f.mounts[p]
		return dev, "vfat", ok
	}
	volumeUUID = func(dev string) (string, error) {
		if e := f.byDev(dev); e != nil {
			return e.uuid, nil
		}
		return "", errors.New("no FAT on " + dev)
	}
	isUEFI = func(string) bool { return f.uefi }
	readProcCmdline = func() string { return f.proc }
	nowUTC = func() string { return "2026-10-09T12:00:00Z" }
	runZFS = func(args ...string) (string, error) {
		f.zfsCalls = append(f.zfsCalls, strings.Join(args, " "))
		if args[0] == "get" {
			return f.zfsProp, nil
		}
		f.zfsProp = strings.SplitN(args[1], "=", 2)[1]
		return "", nil
	}
	return f
}

func (f *fakeHost) byDev(dev string) *fakeESP {
	for _, e := range f.esps {
		if e.dev == dev {
			return e
		}
	}
	return nil
}

func (f *fakeHost) byUUID(u string) *fakeESP {
	for _, e := range f.esps {
		if e.uuid == u {
			return e
		}
	}
	return nil
}

// addESP adds an ESP (an empty, freshly formatted one) on its own disk.
func (f *fakeHost) addESP(uuid, dev string) *fakeESP {
	e := &fakeESP{uuid: uuid, dev: dev, dir: f.t.TempDir(), present: true}
	if !f.uefi {
		e.disk = testBIOSDisk(f.t)
	}
	f.esps = append(f.esps, e)
	return e
}

// mountAsRoot makes e the ESP mounted at <root>/boot/efi (install).
func (f *fakeHost) mountAsRoot(e *fakeESP) (root string) {
	root = f.t.TempDir()
	mp := filepath.Join(root, "boot/efi")
	os.MkdirAll(filepath.Dir(mp), 0o755)
	if err := os.Symlink(e.dir, mp); err != nil {
		f.t.Fatal(err)
	}
	f.mounts[canonicalPath(mp)] = e.dev
	return root
}

// captureOutput runs fn with stdout and stderr captured.
func captureOutput(t *testing.T, fn func()) string {
	t.Helper()
	r, w, _ := os.Pipe()
	oo, oe := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	done := make(chan string)
	go func() {
		var b bytes.Buffer
		io.Copy(&b, r)
		done <- b.String()
	}()
	defer func() { os.Stdout, os.Stderr = oo, oe }()
	fn()
	w.Close()
	os.Stdout, os.Stderr = oo, oe
	return <-done
}

// testStage2Versioned is testStage2(true, ...) carrying the embedded
// version string a real stage2 has (checkUpdateEligible looks for it).
func testStage2Versioned(size int, seed byte) []byte {
	b := testStage2(true, size, seed)
	copy(b[64:], "alpine-zfsboot by UniDoc, version 0.5.0, build=test\x00")
	binary.LittleEndian.PutUint16(b[8:], 0)
	var sum uint16
	for i := 0; i < size/2; i++ {
		sum += binary.LittleEndian.Uint16(b[2*i:])
	}
	binary.LittleEndian.PutUint16(b[8:], -sum)
	return b
}

// biosBuild writes a BIOS release (stage1/stage2/kernel/initrd/cmdline)
// into a directory and returns it as local-file sources.
func biosBuild(t *testing.T, stamp string, seed byte) release.BIOSSources {
	t.Helper()
	d := t.TempDir()
	s1 := append(testStage1(true, seed), make([]byte, layout.SectorSize-layout.Stage1Bytes)...)
	s1[510], s1[511] = 0x55, 0xAA
	files := map[string][]byte{
		"stage1":  s1,
		"stage2":  testStage2Versioned(4096, seed),
		"kernel":  buildFakeBzImage("6.18.0-0-lts", seed),
		"initrd":  buildFakeInitrdBytes(t, "2.3.4"),
		"cmdline": []byte("root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.pool=zroot alpine-zfsboot.version=0.5.0 alpine-zfsboot.buildstamp=" + stamp + "\n"),
	}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(d, n), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := func(n string) release.Source { return release.Source{File: filepath.Join(d, n)} }
	return release.BIOSSources{Stage1: p("stage1"), Stage2: p("stage2"), Kernel: p("kernel"), Initrd: p("initrd"), Cmdline: p("cmdline")}
}

func readESP(t *testing.T, e *fakeESP, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, rel))
	if err != nil {
		return "<absent>"
	}
	return string(b)
}

func memberOf(t *testing.T, e *fakeESP) espmember.Member {
	t.Helper()
	m, present, err := espmember.Read(e.dir)
	if !present || err != nil {
		t.Fatalf("ESP %s: no valid MEMBER (%v)", e.uuid, err)
	}
	return m
}

const testSSHKey = "ssh-ed25519 AAAATESTKEY test@host"

func installBIOSMirror(t *testing.T, f *fakeHost, root string, ho hostOpts) (int, string) {
	t.Helper()
	var code int
	out := captureOutput(t, func() {
		code = runInstall(installRun{
			disk: f.esps[0].disk, arch: "x86_64", mountpoint: filepath.Join(root, "boot/efi"), yes: true,
			bios: biosBuild(t, "20261001T000000Z", 1), sshKey: testSSHKey,
			opts: biosOpts{integrity: "warn"}, ho: ho,
		})
	})
	return code, out
}

func TestMirror_InstallBIOS_TwoListedESPs(t *testing.T) {
	f := newFakeHost(t, false)
	a := f.addESP("AAAA-0001", "/dev/sda1")
	b := f.addESP("BBBB-0002", "/dev/sdb1")
	f.addESP("CCCC-0003", "/dev/sdc1") // an unrelated FAT volume: never written
	root := f.mountAsRoot(a)
	os.Setenv(envESPUUIDs, "aaaa-0001,BBBB-0002")

	code, out := installBIOSMirror(t, f, root, hostOpts{})
	if code != 0 {
		t.Fatalf("install exit %d:\n%s", code, out)
	}
	for _, e := range []*fakeESP{a, b} {
		if !bootable(t, e.disk) {
			t.Errorf("disk of %s not bootable after install", e.uuid)
		}
		c := readESP(t, e, layout.CmdlineFile)
		if !strings.Contains(c, "alpine-zfsboot.esp-self="+e.uuid) || !strings.Contains(c, "alpine-zfsboot.esp-uuids=AAAA-0001,BBBB-0002") {
			t.Errorf("%s CMDLINE: %q", e.uuid, c)
		}
		if !strings.Contains(readESP(t, e, layout.ConfigFile), "alpine-zfsboot.esp-uuids=AAAA-0001,BBBB-0002") {
			t.Errorf("%s config lacks the list", e.uuid)
		}
		m := memberOf(t, e)
		if m.Generation != 1 || m.ESPUUID != e.uuid || strings.Join(m.Members, ",") != "AAAA-0001,BBBB-0002" || m.Pool != "zroot" {
			t.Errorf("%s MEMBER: %+v", e.uuid, m)
		}
	}
	if readESP(t, a, layout.SSHHostEd25519KeyFile) != readESP(t, b, layout.SSHHostEd25519KeyFile) || readESP(t, a, layout.SSHHostEd25519KeyFile) == "<absent>" {
		t.Error("the two ESPs got different rescue host keys")
	}
	if memberOf(t, a).InstallID != memberOf(t, b).InstallID {
		t.Error("different install ids")
	}
	if c := f.byUUID("CCCC-0003"); readESP(t, c, layout.KernelFile) != "<absent>" {
		t.Error("an unlisted ESP was written")
	}
	if f.zfsProp != memberOf(t, a).InstallID {
		t.Errorf("pool property not set: %v", f.zfsCalls)
	}

	// verify: both fine, consistent; the comparison table lists both.
	var vcode int
	vout := captureOutput(t, func() { vcode = runVerify("/", "", hostOpts{}, false, false, false, verifyFiles{}) })
	if vcode != 0 || !strings.Contains(vout, "verify: OK") || !strings.Contains(vout, "ESP comparison") ||
		!strings.Contains(vout, "ESP BBBB-0002 (/dev/sdb1) generation 1 build 20261001T000000Z: stage2 ") {
		t.Fatalf("verify after install: exit %d\n%s", vcode, vout)
	}

	// esp list: both members, the unrelated FAT volume not shown.
	var lcode int
	lout := captureOutput(t, func() { lcode = runESPList("/", "", hostOpts{}) })
	if lcode != 0 || !strings.Contains(lout, "AAAA-0001  /dev/sda1") || !strings.Contains(lout, "MEMBER") ||
		strings.Contains(lout, "CCCC-0003") || !strings.Contains(lout, "set: list") {
		t.Fatalf("esp list: %d\n%s", lcode, lout)
	}
}

func TestMirror_InstallRefusesUnsafeOrPartial(t *testing.T) {
	cases := map[string]func(f *fakeHost, a, b *fakeESP) (hostOpts, string){
		"listed ESP missing": func(f *fakeHost, a, b *fakeESP) (hostOpts, string) {
			b.present = false
			return hostOpts{espUUIDs: "AAAA-0001,BBBB-0002"}, "partial mirror"
		},
		"mounted ESP not in the list": func(f *fakeHost, a, b *fakeESP) (hostOpts, string) {
			return hostOpts{espUUIDs: "BBBB-0002"}, "is not in --esp-uuids"
		},
		"two ESPs on one disk": func(f *fakeHost, a, b *fakeESP) (hostOpts, string) {
			b.disk = a.disk
			return hostOpts{espUUIDs: "AAAA-0001,BBBB-0002"}, "both on disk"
		},
		"whole-disk FAT / md": func(f *fakeHost, a, b *fakeESP) (hostOpts, string) {
			b.disk = ""
			return hostOpts{espUUIDs: "AAAA-0001,BBBB-0002"}, "not a partition"
		},
		"foreign marker without --force-adopt": func(f *fakeHost, a, b *fakeESP) (hostOpts, string) {
			os.MkdirAll(filepath.Join(b.dir, layout.ESPDir), 0o755)
			espmember.Write(b.dir, espmember.Member{InstallID: "9a8b7c6d-1111-4222-8333-444455556666", Generation: 4, ESPUUID: b.uuid})
			espmember.Write(a.dir, espmember.Member{InstallID: "0f6c1e9a-4b1d-4c7e-9a55-1d2e3f405162", Generation: 1, ESPUUID: a.uuid})
			return hostOpts{espUUIDs: "AAAA-0001,BBBB-0002"}, "another installation"
		},
		"--add-esp of a non-FAT device": func(f *fakeHost, a, b *fakeESP) (hostOpts, string) {
			return hostOpts{addESP: "/dev/nope9"}, "holds no FAT filesystem"
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeHost(t, false)
			a := f.addESP("AAAA-0001", "/dev/sda1")
			b := f.addESP("BBBB-0002", "/dev/sdb1")
			root := f.mountAsRoot(a)
			ho, want := setup(f, a, b)
			code, out := installBIOSMirror(t, f, root, ho)
			if code != exitFailure || !strings.Contains(out, want) {
				t.Fatalf("want exit 1 with %q, got %d:\n%s", want, code, out)
			}
			for _, e := range []*fakeESP{a, b} {
				if readESP(t, e, layout.KernelFile) != "<absent>" {
					t.Errorf("%s was written although install refused", e.uuid)
				}
			}
			if bootable(t, a.disk) {
				t.Error("disk A got a boot loader although install refused")
			}
		})
	}
}

// installed mirror A+B (markers, no list), for the update/verify tests.
func markedMirror(t *testing.T) (*fakeHost, *fakeESP, *fakeESP) {
	t.Helper()
	f := newFakeHost(t, false)
	a := f.addESP("AAAA-0001", "/dev/sda1")
	b := f.addESP("BBBB-0002", "/dev/sdb1")
	root := f.mountAsRoot(a)
	code, out := installBIOSMirror(t, f, root, hostOpts{addESP: "BBBB-0002"})
	if code != 0 {
		t.Fatalf("install: %d\n%s", code, out)
	}
	f.mounts = map[string]string{}
	return f, a, b
}

func runUpdateCapture(t *testing.T, ho hostOpts, src release.BIOSSources, opts biosOpts) (int, string) {
	t.Helper()
	var code int
	out := captureOutput(t, func() { code = runUpdate("/", "", ho, true, release.Source{}, src, opts) })
	return code, out
}

func TestMirror_UpdateBIOS_AllMembersByMarker(t *testing.T) {
	f, a, b := markedMirror(t)
	_ = f
	if strings.Contains(readESP(t, a, layout.ConfigFile), "esp-uuids") {
		t.Fatal("install without a list must not record one")
	}
	code, out := runUpdateCapture(t, hostOpts{}, biosBuild(t, "20261005T000000Z", 2), biosOpts{integrity: "warn"})
	if code != 0 {
		t.Fatalf("update: %d\n%s", code, out)
	}
	for _, e := range []*fakeESP{a, b} {
		if !strings.Contains(readESP(t, e, layout.CmdlineFile), "20261005T000000Z") {
			t.Errorf("%s not updated", e.uuid)
		}
		if g := memberOf(t, e).Generation; g != 2 {
			t.Errorf("%s generation %d, want 2", e.uuid, g)
		}
		if !strings.Contains(readESP(t, e, layout.CmdlineFile), "esp-self="+e.uuid) {
			t.Errorf("%s lost its esp-self", e.uuid)
		}
	}
	// Up to date: nothing written, generation unchanged.
	code, out = runUpdateCapture(t, hostOpts{}, biosBuild(t, "20261005T000000Z", 2), biosOpts{integrity: "warn"})
	if code != 0 || memberOf(t, a).Generation != 2 || !strings.Contains(out, "already up to date") {
		t.Fatalf("second update: %d gen %d\n%s", code, memberOf(t, a).Generation, out)
	}
}

func TestMirror_UpdateFailureOnOneMemberContinuesRollsBackAndReports(t *testing.T) {
	f, a, b := markedMirror(t)
	oldB := readESP(t, b, layout.KernelFile)
	orig := writeStage2
	t.Cleanup(func() { writeStage2 = orig })
	writeStage2 = func(disk string, s2 []byte) ([]byte, error) {
		prev, err := orig(disk, s2)
		if disk == f.byUUID("AAAA-0001").disk && err == nil && !bytes.Equal(s2, prev) && len(prev) > 0 {
			// the FIRST member written fails after its stage2 write
			return prev, errors.New("injected I/O error on disk A")
		}
		return prev, err
	}
	code, out := runUpdateCapture(t, hostOpts{}, biosBuild(t, "20261005T000000Z", 2), biosOpts{integrity: "warn"})
	if code != exitFailure {
		t.Fatalf("want exit 1, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "ESP AAAA-0001 (/dev/sda1)") || !strings.Contains(out, "FAILED: ") || !strings.Contains(out, "injected I/O error") {
		t.Fatalf("summary does not name the failed ESP:\n%s", out)
	}
	// A: rolled back - still the old, bootable generation; MEMBER untouched.
	if !bootable(t, a.disk) || !strings.Contains(readESP(t, a, layout.CmdlineFile), "20261001T000000Z") || memberOf(t, a).Generation != 1 {
		t.Errorf("A not left on its previous generation: gen %d cmdline %q", memberOf(t, a).Generation, readESP(t, a, layout.CmdlineFile))
	}
	// B: the run went on - updated, generation 2.
	if readESP(t, b, layout.KernelFile) == oldB || memberOf(t, b).Generation != 2 {
		t.Errorf("B was not updated after A failed (gen %d)", memberOf(t, b).Generation)
	}
	writeStage2 = orig

	// verify names A as the stale member.
	var vcode int
	vout := captureOutput(t, func() { vcode = runVerify("/", "", hostOpts{}, false, false, false, verifyFiles{}) })
	if vcode != exitFailure || !strings.Contains(vout, "ESP AAAA-0001 (/dev/sda1) (generation 1, build 20261001T000000Z) is STALE") {
		t.Fatalf("verify does not name the stale member: %d\n%s", vcode, vout)
	}
	// A later update repairs it.
	code, out = runUpdateCapture(t, hostOpts{}, biosBuild(t, "20261005T000000Z", 2), biosOpts{integrity: "warn"})
	if code != 0 || memberOf(t, a).Generation != 3 || memberOf(t, b).Generation != 3 {
		t.Fatalf("repair update: %d A gen %d B gen %d\n%s", code, memberOf(t, a).Generation, memberOf(t, b).Generation, out)
	}
	vout = captureOutput(t, func() { vcode = runVerify("/", "", hostOpts{}, false, false, false, verifyFiles{}) })
	if vcode != 0 {
		t.Fatalf("verify after repair: %d\n%s", vcode, vout)
	}
}

func TestMirror_MissingMemberIsAWarningWithItsOwnExitCode(t *testing.T) {
	f, a, b := markedMirror(t)
	b.present = false
	code, out := runUpdateCapture(t, hostOpts{}, biosBuild(t, "20261005T000000Z", 2), biosOpts{integrity: "warn"})
	if code != exitMemberMissing || !strings.Contains(out, "BBBB-0002") || !strings.Contains(out, "MISSING") {
		t.Fatalf("update with a member missing: want exit %d, got %d\n%s", exitMemberMissing, code, out)
	}
	if memberOf(t, a).Generation != 2 {
		t.Error("the present member was not updated")
	}
	var vcode, scode int
	vout := captureOutput(t, func() { vcode = runVerify("/", "", hostOpts{}, false, false, false, verifyFiles{}) })
	if vcode != exitMemberMissing || !strings.Contains(vout, "MISSING") {
		t.Fatalf("verify: %d\n%s", vcode, vout)
	}
	captureOutput(t, func() { scode = runStatus("/", "", hostOpts{}, false) })
	if scode != exitMemberMissing {
		t.Fatalf("status: %d", scode)
	}
	// The disk comes back: stale now, named by verify, repaired by update.
	b.present = true
	vout = captureOutput(t, func() { vcode = runVerify("/", "", hostOpts{}, false, false, false, verifyFiles{}) })
	if vcode != exitFailure || !strings.Contains(vout, "ESP BBBB-0002 (/dev/sdb1) (generation 1, build 20261001T000000Z) is STALE") {
		t.Fatalf("verify after the disk returned: %d\n%s", vcode, vout)
	}
	_ = f
}

func TestMirror_UpdateAddESPSeedsANewMember(t *testing.T) {
	f, a, b := markedMirror(t)
	c := f.addESP("CCCC-0003", "/dev/sdc1")
	code, out := runUpdateCapture(t, hostOpts{addESP: "/dev/sdc1"}, biosBuild(t, "20261001T000000Z", 1), biosOpts{integrity: "warn"})
	if code != 0 {
		t.Fatalf("update --add-esp: %d\n%s", code, out)
	}
	if !bootable(t, c.disk) || readESP(t, c, layout.SSHHostEd25519KeyFile) != readESP(t, a, layout.SSHHostEd25519KeyFile) || readESP(t, c, layout.AuthorizedKeysFile) == "<absent>" {
		t.Fatalf("C not seeded with boot chain and the same keys:\n%s", out)
	}
	for _, e := range []*fakeESP{a, b, c} {
		if m := memberOf(t, e); strings.Join(m.Members, ",") != "AAAA-0001,BBBB-0002,CCCC-0003" {
			t.Errorf("%s MEMBERS=%v", e.uuid, m.Members)
		}
	}
	if memberOf(t, c).Generation != memberOf(t, a).Generation {
		t.Error("the seeded member's generation differs")
	}
}

func TestMirror_ForeignESPNeverWrittenWithoutForceAdopt(t *testing.T) {
	f, a, _ := markedMirror(t)
	x := f.addESP("DDDD-0004", "/dev/sdd1")
	os.MkdirAll(filepath.Join(x.dir, layout.ESPDir), 0o755)
	foreign := espmember.Member{InstallID: "9a8b7c6d-1111-4222-8333-444455556666", Generation: 40, ESPUUID: x.uuid, Members: []string{x.uuid}}
	espmember.Write(x.dir, foreign)
	os.WriteFile(filepath.Join(x.dir, layout.AuthorizedKeysFile), []byte("ssh-ed25519 AAAAEVIL attacker\n"), 0o600)

	// Two marked installations and nothing saying which is this host's:
	// refused, nothing written anywhere.
	code, out := runUpdateCapture(t, hostOpts{}, biosBuild(t, "20261005T000000Z", 2), biosOpts{integrity: "warn"})
	if code != exitFailure || !strings.Contains(out, "more than one alpine-zfsboot installation") || memberOf(t, a).Generation != 1 {
		t.Fatalf("ambiguous installations must be refused: %d\n%s", code, out)
	}
	// On the live system /boot/efi is mounted: that names this host's set.
	f.mounts["/boot/efi"] = a.dev
	// Not part of the set: a plain update ignores it (and never copies its keys).
	code, out = runUpdateCapture(t, hostOpts{}, biosBuild(t, "20261005T000000Z", 2), biosOpts{integrity: "warn"})
	if code != 0 || readESP(t, x, layout.KernelFile) != "<absent>" || strings.Contains(readESP(t, a, layout.AuthorizedKeysFile), "EVIL") {
		t.Fatalf("foreign ESP touched or used: %d\n%s", code, out)
	}
	// --add-esp alone refuses, nothing written.
	code, out = runUpdateCapture(t, hostOpts{addESP: "DDDD-0004"}, biosBuild(t, "20261005T000000Z", 2), biosOpts{integrity: "warn"})
	if code != exitFailure || !strings.Contains(out, "--force-adopt") || readESP(t, x, layout.KernelFile) != "<absent>" {
		t.Fatalf("--add-esp of a foreign ESP: %d\n%s", code, out)
	}
	// With --force-adopt it becomes ours: our keys, our identity.
	code, out = runUpdateCapture(t, hostOpts{addESP: "DDDD-0004", forceAdopt: true}, biosBuild(t, "20261005T000000Z", 2), biosOpts{integrity: "warn"})
	if code != 0 || strings.Contains(readESP(t, x, layout.AuthorizedKeysFile), "EVIL") || memberOf(t, x).InstallID != memberOf(t, a).InstallID {
		t.Fatalf("--force-adopt: %d\n%s", code, out)
	}
}

func TestMirror_LegacySingleESP_UnchangedThenMarked(t *testing.T) {
	f := newFakeHost(t, false)
	// A legacy ESP keeps the old name-based disk derivation: name the
	// "partition" after the disk image so it resolves to it.
	disk := filepath.Join(t.TempDir(), "sdq")
	os.Rename(testBIOSDisk(t), disk)
	a := &fakeESP{uuid: "AAAA-0001", dev: disk + "1", dir: t.TempDir(), present: true, disk: disk}
	f.esps = append(f.esps, a)
	// A pre-0.5.0 install: payload + config + stage1/stage2, no MEMBER.
	root := f.mountAsRoot(a)
	captureOutput(t, func() {
		runInstall(installRun{disk: a.disk, arch: "x86_64", mountpoint: filepath.Join(root, "boot/efi"), yes: true,
			bios: biosBuild(t, "20261001T000000Z", 1), sshKey: testSSHKey, opts: biosOpts{integrity: "warn"}})
	})
	f.mounts = map[string]string{}
	espmember.Remove(a.dir)
	c := setCmdlineWord([]byte(readESP(t, a, layout.CmdlineFile)), layout.ESPSelfKey, "")
	os.WriteFile(filepath.Join(a.dir, layout.CmdlineFile), c, 0o644)

	var scode int
	sout := captureOutput(t, func() { scode = runStatus("/", "", hostOpts{}, false) })
	if scode != 0 || strings.Contains(sout, "== ESP") || !strings.Contains(sout, "no EFI/ALPINE/MEMBER marker yet") || strings.Contains(sout, "ERROR") {
		t.Fatalf("legacy status: %d\n%s", scode, sout)
	}
	code, out := runUpdateCapture(t, hostOpts{}, biosBuild(t, "20261005T000000Z", 2), biosOpts{integrity: "warn"})
	if code != 0 || strings.Contains(out, "summary") {
		t.Fatalf("legacy update: %d\n%s", code, out)
	}
	m := memberOf(t, a)
	if m.Generation != 1 || strings.Join(m.Members, ",") != "AAAA-0001" {
		t.Fatalf("first 0.5.0 update should mark the single ESP: %+v", m)
	}
}

func TestMirror_TwoLegacyESPs_RefuseThenAdopt(t *testing.T) {
	f := newFakeHost(t, false)
	a := f.addESP("AAAA-0001", "/dev/sda1")
	b := f.addESP("BBBB-0002", "/dev/sdb1")
	root := f.mountAsRoot(a)
	installBIOSMirror(t, f, root, hostOpts{espUUIDs: "AAAA-0001,BBBB-0002"})
	f.mounts = map[string]string{}
	// Make both "legacy": no markers, no lists (a mirror copied by hand).
	for _, e := range []*fakeESP{a, b} {
		espmember.Remove(e.dir)
		os.WriteFile(filepath.Join(e.dir, layout.ConfigFile), []byte("alpine-zfsboot.net=dhcp\n"), 0o644)
	}
	var code int
	out := captureOutput(t, func() { code = runStatus("/", "", hostOpts{}, false) })
	if code != exitFailure || !strings.Contains(out, "esp adopt AAAA-0001 BBBB-0002 --yes") || !strings.Contains(out, "alpine-zfsboot.esp-uuids=AAAA-0001,BBBB-0002") {
		t.Fatalf("two legacy ESPs: %d\n%s", code, out)
	}
	// adopt without --yes: shows the plan, writes nothing.
	out = captureOutput(t, func() { code = runESPAdopt([]string{"AAAA-0001", "/dev/sdb1"}, false, false) })
	if code != exitFailure || !strings.Contains(out, "nothing written") {
		t.Fatalf("adopt without --yes: %d\n%s", code, out)
	}
	if _, present, _ := espmember.Read(a.dir); present {
		t.Fatal("adopt without --yes wrote a marker")
	}
	out = captureOutput(t, func() { code = runESPAdopt([]string{"AAAA-0001", "/dev/sdb1"}, true, false) })
	if code != 0 {
		t.Fatalf("adopt --yes: %d\n%s", code, out)
	}
	if memberOf(t, a).InstallID != memberOf(t, b).InstallID {
		t.Fatal("adopted ESPs got different ids")
	}
	out = captureOutput(t, func() { code = runStatus("/", "", hostOpts{}, false) })
	if code != 0 || !strings.Contains(out, "identity markers") {
		t.Fatalf("status after adopt: %d\n%s", code, out)
	}
}

func TestMirror_ESPAddRemove(t *testing.T) {
	f, a, b := markedMirror(t)
	c := f.addESP("CCCC-0003", "/dev/sdc1")
	var code int
	out := captureOutput(t, func() { code = runESPAdd("/", "", hostOpts{}, "CCCC-0003", true) })
	if code != 0 || !bootable(t, c.disk) || readESP(t, c, layout.KernelFile) != readESP(t, a, layout.KernelFile) {
		t.Fatalf("esp add: %d\n%s", code, out)
	}
	if !strings.Contains(readESP(t, c, layout.CmdlineFile), "esp-self=CCCC-0003") {
		t.Error("esp add: new member's CMDLINE lacks its own esp-self")
	}
	var vcode int
	vout := captureOutput(t, func() { vcode = runVerify("/", "", hostOpts{}, false, false, false, verifyFiles{}) })
	if vcode != 0 {
		t.Fatalf("verify after esp add: %d\n%s", vcode, vout)
	}

	// remove a dead disk (absent) by UUID: only the lists change.
	b.present = false
	out = captureOutput(t, func() { code = runESPRemove("/", "", hostOpts{}, "BBBB-0002", true) })
	if code != 0 {
		t.Fatalf("esp remove (absent): %d\n%s", code, out)
	}
	if m := memberOf(t, a); strings.Join(m.Members, ",") != "AAAA-0001,CCCC-0003" {
		t.Fatalf("MEMBERS after remove: %v", m.Members)
	}
	var scode int
	captureOutput(t, func() { scode = runStatus("/", "", hostOpts{}, false) })
	if scode != 0 {
		t.Fatal("status after removing the dead member should be clean")
	}
	// remove a present member: its marker goes, its files stay.
	out = captureOutput(t, func() { code = runESPRemove("/", "", hostOpts{}, "/dev/sdc1", true) })
	if code != 0 {
		t.Fatalf("esp remove (present): %d\n%s", code, out)
	}
	if _, present, _ := espmember.Read(c.dir); present || readESP(t, c, layout.KernelFile) == "<absent>" {
		t.Fatal("remove must delete the marker only")
	}
	// the last one cannot be removed
	out = captureOutput(t, func() { code = runESPRemove("/", "", hostOpts{}, "AAAA-0001", true) })
	if code != exitFailure || !strings.Contains(out, "last one") {
		t.Fatalf("removing the last member: %d\n%s", code, out)
	}
}

func TestMirror_IntegrityAppliesToEveryMember(t *testing.T) {
	_, a, b := markedMirror(t)
	var code int
	out := captureOutput(t, func() {
		code = forEachMember("/", "", hostOpts{}, func(t *target) error { return setIntegrityMode(t, "off") })
	})
	if code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	for _, e := range []*fakeESP{a, b} {
		if !strings.Contains(readESP(t, e, layout.PayloadSumFile), "MODE off") {
			t.Errorf("%s: integrity not set", e.uuid)
		}
	}
}

func TestMirror_MountFailureOfOneMemberIsReportedNotFatal(t *testing.T) {
	_, a, b := markedMirror(t)
	b.mountFail = true
	code, out := runUpdateCapture(t, hostOpts{}, biosBuild(t, "20261005T000000Z", 2), biosOpts{integrity: "warn"})
	if code != exitFailure || !strings.Contains(out, "injected failure") || memberOf(t, a).Generation != 2 {
		t.Fatalf("%d\n%s", code, out)
	}
}

func TestMirror_CmdlineTooLongRefusedBeforeWriting(t *testing.T) {
	f, a, _ := markedMirror(t)
	_ = f
	src := biosBuild(t, "20261005T000000Z", 2)
	long := "root=ZFS=zroot/ROOT/alpine alpine-zfsboot.version=0.5.0 alpine-zfsboot.buildstamp=20261005T000000Z " + strings.Repeat("x", 450) + "\n"
	os.WriteFile(src.Cmdline.File, []byte(long), 0o644)
	code, out := runUpdateCapture(t, hostOpts{}, src, biosOpts{integrity: "warn"})
	if code != exitFailure || !strings.Contains(out, "accepts at most 511") {
		t.Fatalf("%d\n%s", code, out)
	}
	if !strings.Contains(readESP(t, a, layout.CmdlineFile), "20261001T000000Z") {
		t.Fatal("written although refused")
	}
}

func TestVerifyPayloadIgnoringMirrorWords(t *testing.T) {
	mp := newBIOSMountpoint(t)
	k, i := []byte("k"), []byte("i")
	mustWriteFile(t, mp, layout.KernelFile, k)
	mustWriteFile(t, mp, layout.InitrdFile, i)
	mustWriteFile(t, mp, layout.CmdlineFile, []byte("a b alpine-zfsboot.esp-uuids=AAAA-0001,BBBB-0002 alpine-zfsboot.esp-self=AAAA-0001\n"))
	if err := verifyPayloadIgnoringMirrorWords(mp, k, i, []byte("a b\n")); err != nil {
		t.Fatalf("release cmdline must match: %v", err)
	}
	if err := verifyPayloadIgnoringMirrorWords(mp, k, i, []byte("a c\n")); err == nil {
		t.Fatal("a real difference must still fail")
	}
}

// --- UEFI ---------------------------------------------------------------

// buildFakeEFI assembles a minimal PE32+ image with the .cmdline, .linux
// and .initrd sections cmdline.Read/ReadSection read - enough for the
// UEFI writers and metadata generation, independent of build.sh.
func buildFakeEFI(cmd string, kernel, initrd []byte) []byte {
	type sec struct {
		name string
		data []byte
	}
	secs := []sec{{".cmdline", append([]byte(cmd), 0)}, {".linux", kernel}, {".initrd", initrd}}
	const peOff = 0x40
	hdrEnd := peOff + 4 + 20 + 40*len(secs)
	off := (hdrEnd + 511) &^ 511
	var out bytes.Buffer
	dos := make([]byte, peOff)
	dos[0], dos[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(dos[0x3c:], peOff)
	out.Write(dos)
	out.WriteString("PE\x00\x00")
	coff := make([]byte, 20)
	binary.LittleEndian.PutUint16(coff[0:], 0x8664)
	binary.LittleEndian.PutUint16(coff[2:], uint16(len(secs)))
	out.Write(coff)
	var body bytes.Buffer
	for _, s := range secs {
		h := make([]byte, 40)
		copy(h, s.name)
		size := (len(s.data) + 511) &^ 511
		binary.LittleEndian.PutUint32(h[8:], uint32(len(s.data)))
		binary.LittleEndian.PutUint32(h[12:], uint32(off+body.Len()))
		binary.LittleEndian.PutUint32(h[16:], uint32(size))
		binary.LittleEndian.PutUint32(h[20:], uint32(off+body.Len()))
		out.Write(h)
		body.Write(s.data)
		body.Write(make([]byte, size-len(s.data)))
	}
	out.Write(make([]byte, off-out.Len()))
	out.Write(body.Bytes())
	return out.Bytes()
}

func uefiBuild(t *testing.T, stamp string) release.Source {
	t.Helper()
	p := filepath.Join(t.TempDir(), "alpine-zfsboot-x86_64.EFI")
	efi := buildFakeEFI("root=ZFS=zroot/ROOT/alpine alpine-zfsboot.pool=zroot alpine-zfsboot.version=0.5.0 alpine-zfsboot.buildstamp="+stamp,
		buildFakeBzImage("6.18.0-0-lts", 3), buildFakeInitrdBytes(t, "2.3.4"))
	if err := os.WriteFile(p, efi, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := cmdline.Read(p); err != nil {
		t.Fatalf("fake EFI does not parse: %v", err)
	}
	return release.Source{File: p}
}

func TestMirror_UEFI_InstallUpdateVerifyAdd(t *testing.T) {
	f := newFakeHost(t, true)
	a := f.addESP("AAAA-0001", "/dev/vda1")
	b := f.addESP("BBBB-0002", "/dev/vdb1")
	root := f.mountAsRoot(a)
	var code int
	out := captureOutput(t, func() {
		code = runInstall(installRun{arch: "x86_64", uefi: true, mountpoint: filepath.Join(root, "boot/efi"), yes: true,
			efi: uefiBuild(t, "20261001T000000Z"), sshKey: testSSHKey, ho: hostOpts{addESP: "/dev/vdb1"}})
	})
	if code != 0 {
		t.Fatalf("uefi install: %d\n%s", code, out)
	}
	rel, _ := uefiboot.LoaderPath("x86_64")
	for _, e := range []*fakeESP{a, b} {
		if readESP(t, e, rel) == "<absent>" || memberOf(t, e).Generation != 1 {
			t.Fatalf("%s not installed", e.uuid)
		}
	}
	f.mounts = map[string]string{}
	var ucode int
	uout := captureOutput(t, func() {
		ucode = runUpdate("/", "", hostOpts{}, true, uefiBuild(t, "20261005T000000Z"), release.BIOSSources{}, biosOpts{})
	})
	if ucode != 0 || memberOf(t, a).Generation != 2 || memberOf(t, b).Generation != 2 {
		t.Fatalf("uefi update: %d\n%s", ucode, uout)
	}
	// B's loader is damaged by hand: verify names it.
	os.WriteFile(filepath.Join(b.dir, rel), []byte("damaged"), 0o644)
	vout := captureOutput(t, func() { code = runVerify("/", "", hostOpts{}, false, false, false, verifyFiles{}) })
	if code != exitFailure || !strings.Contains(vout, "BBBB-0002") {
		t.Fatalf("uefi verify: %d\n%s", code, vout)
	}
	// esp add of a third ESP copies the loader from the best member.
	c := f.addESP("CCCC-0003", "/dev/vdc1")
	aout := captureOutput(t, func() { code = runESPAdd("/", "", hostOpts{}, "/dev/vdc1", true) })
	if code != 0 || readESP(t, c, rel) != readESP(t, a, rel) {
		t.Fatalf("uefi esp add: %d\n%s", code, aout)
	}
	_ = biosboot.ReadStage1
}

// A listed ESP of ANOTHER installation with a higher generation must never
// become the source of config/keys (update), the verify reference, or be
// written by esp remove.
func TestMirror_ListedForeignESPIsNeverASourceNorWritten(t *testing.T) {
	f, a, b := markedMirror(t)
	x := f.addESP("CCCC-0003", "/dev/sdc1")
	os.MkdirAll(filepath.Join(x.dir, layout.ESPDir), 0o755)
	espmember.Write(x.dir, espmember.Member{InstallID: "9a8b7c6d-1111-4222-8333-444455556666", Generation: 99, ESPUUID: x.uuid, Members: []string{x.uuid}})
	os.WriteFile(filepath.Join(x.dir, layout.AuthorizedKeysFile), []byte("ssh-ed25519 AAAAEVIL attacker\n"), 0o600)
	os.WriteFile(filepath.Join(x.dir, layout.SSHHostEd25519KeyFile), []byte("EVIL-HOSTKEY"), 0o600)
	snapshot := func() string {
		return readESP(t, x, layout.MemberFile) + readESP(t, x, layout.ConfigFile) + readESP(t, x, layout.AuthorizedKeysFile) + readESP(t, x, layout.KernelFile)
	}
	before := snapshot()
	keysA := readESP(t, a, layout.AuthorizedKeysFile) + readESP(t, a, layout.SSHHostEd25519KeyFile)
	list := hostOpts{espUUIDs: "AAAA-0001,BBBB-0002,CCCC-0003"}

	code, out := runUpdateCapture(t, list, biosBuild(t, "20261005T000000Z", 2), biosOpts{integrity: "warn"})
	if code != exitFailure || !strings.Contains(out, "--force-adopt") {
		t.Fatalf("update with a listed foreign ESP: %d\n%s", code, out)
	}
	if got := readESP(t, a, layout.AuthorizedKeysFile) + readESP(t, a, layout.SSHHostEd25519KeyFile); got != keysA || strings.Contains(readESP(t, b, layout.AuthorizedKeysFile), "EVIL") {
		t.Fatalf("the foreign ESP's keys reached this host's ESPs:\n%s", out)
	}
	if snapshot() != before {
		t.Fatal("the foreign ESP was written")
	}
	if memberOf(t, a).Generation != 2 || memberOf(t, b).Generation != 2 {
		t.Fatal("this host's members were not updated")
	}
	var vcode int
	vout := captureOutput(t, func() { vcode = runVerify("/", "", list, false, false, false, verifyFiles{}) })
	if strings.Contains(vout, "STALE") || strings.Contains(vout, "CCCC-0003 (/dev/sdc1) generation 99") || !strings.Contains(vout, "FOREIGN marker") {
		t.Fatalf("verify used the foreign ESP as a reference: %d\n%s", vcode, vout)
	}
	captureOutput(t, func() { runESPRemove("/", "", list, "BBBB-0002", true) })
	if snapshot() != before {
		t.Fatal("esp remove wrote to the foreign ESP")
	}
}

func TestMirror_OddDiskHintNeverFailsAMember(t *testing.T) {
	f := newFakeHost(t, false)
	a := f.addESP("AAAA-0001", "/dev/sda1")
	b := f.addESP("BBBB-0002", "/dev/sdb1")
	root := f.mountAsRoot(a)
	_ = b
	src := biosBuild(t, "20261001T000000Z", 1)
	os.WriteFile(src.Cmdline.File, []byte("root=ZFS=x alpine-zfsboot.pool="+strings.Repeat("p#", 120)+" alpine-zfsboot.version=0.5.0 alpine-zfsboot.buildstamp=20261001T000000Z\n"), 0o644)
	var code int
	out := captureOutput(t, func() {
		code = runInstall(installRun{disk: a.disk, arch: "x86_64", mountpoint: filepath.Join(root, "boot/efi"), yes: true,
			bios: src, opts: biosOpts{integrity: "warn"}, ho: hostOpts{addESP: "BBBB-0002"}})
	})
	if code != 0 || len(memberOf(t, a).Pool) != 128 {
		t.Fatalf("%d pool %q\n%s", code, memberOf(t, a).Pool, out)
	}
}

// BLKSUM on a mirrored boot: written on every member (before MEMBER),
// verified on every member, and part of the mirror consistency check.
func TestMirror_BlkSumOnEveryMember(t *testing.T) {
	_, a, b := markedMirror(t)
	for _, e := range []*fakeESP{a, b} {
		raw := readESP(t, e, layout.BlkSumFile)
		bs, err := payloadsum.DecodeBlkSum([]byte(raw))
		if err != nil {
			t.Fatalf("%s: BLKSUM: %v", e.uuid, err)
		}
		m, err := payloadsum.Decode([]byte(readESP(t, e, layout.PayloadSumFile)))
		if err != nil {
			t.Fatal(err)
		}
		k, i := readESP(t, e, layout.KernelFile), readESP(t, e, layout.InitrdFile)
		if errs := payloadsum.VerifyBlkSum(bs, m, []byte(k), []byte(i)); len(errs) != 0 {
			t.Fatalf("%s: BLKSUM does not describe its payload: %v", e.uuid, errs)
		}
	}
	var vcode int
	vout := captureOutput(t, func() { vcode = runVerify("/", "", hostOpts{}, false, false, false, verifyFiles{}) })
	if vcode != 0 || strings.Count(vout, "Block table:") < 2 || strings.Count(vout, "OK (v1, KERNEL") < 2 {
		t.Fatalf("verify does not check the block table on both members: %d\n%s", vcode, vout)
	}
	// A BLKSUM that differs on one member: the consistency check names it.
	path := filepath.Join(b.dir, layout.BlkSumFile)
	raw, _ := os.ReadFile(path)
	raw[512+3] ^= 1
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	vout = captureOutput(t, func() { vcode = runVerify("/", "", hostOpts{}, false, false, false, verifyFiles{}) })
	if !strings.Contains(vout, layout.BlkSumFile) || vcode == 0 {
		t.Fatalf("a member with a different BLKSUM is not reported: %d\n%s", vcode, vout)
	}
}

// A BLKSUM write that fails on one member during a mirrored update: that
// member is rolled back whole (payload, CHECKSUM and BLKSUM as before, MEMBER
// generation not bumped), the other member is still updated, and verify
// names the stale one.
func TestMirror_BlkSumWriteFailureRollsBackThatMemberOnly(t *testing.T) {
	_, a, b := markedMirror(t)
	oldA := map[string]string{}
	for _, rel := range []string{layout.KernelFile, layout.InitrdFile, layout.CmdlineFile, layout.PayloadSumFile, layout.BlkSumFile} {
		oldA[rel] = readESP(t, a, rel)
	}
	orig := writeManifestFile
	t.Cleanup(func() { writeManifestFile = orig })
	var order []string
	writeManifestFile = func(mountpoint, rel string, content []byte, mode os.FileMode) error {
		order = append(order, mountpoint+" "+rel)
		if mountpoint == a.dir && rel == layout.BlkSumFile {
			// what is on disk at this moment: no CHECKSUM, MEMBER not bumped
			if _, err := os.Stat(filepath.Join(a.dir, layout.PayloadSumFile)); !os.IsNotExist(err) {
				t.Errorf("CHECKSUM present on A while BLKSUM is being written: %v", err)
			}
			if g := memberOf(t, a).Generation; g != 1 {
				t.Errorf("A's MEMBER bumped to %d before its BLKSUM was written", g)
			}
			return errors.New("injected BLKSUM write error on A")
		}
		return orig(mountpoint, rel, content, mode)
	}
	code, out := runUpdateCapture(t, hostOpts{}, biosBuild(t, "20261005T000000Z", 2), biosOpts{integrity: "warn"})
	if code != exitFailure || !strings.Contains(out, "injected BLKSUM write error on A") {
		t.Fatalf("want exit 1 naming the failure, got %d\n%s", code, out)
	}
	for rel, want := range oldA {
		if got := readESP(t, a, rel); got != want {
			t.Errorf("A's %s was not rolled back", rel)
		}
	}
	if memberOf(t, a).Generation != 1 || !bootable(t, a.disk) {
		t.Errorf("A not left on its previous, bootable generation (gen %d)", memberOf(t, a).Generation)
	}
	if memberOf(t, b).Generation != 2 {
		t.Errorf("B was not updated after A failed (gen %d)", memberOf(t, b).Generation)
	}
	bs, err := payloadsum.DecodeBlkSum([]byte(readESP(t, b, layout.BlkSumFile)))
	if err != nil {
		t.Fatalf("B: BLKSUM: %v", err)
	}
	m, err := payloadsum.Decode([]byte(readESP(t, b, layout.PayloadSumFile)))
	if err != nil {
		t.Fatal(err)
	}
	if errs := payloadsum.VerifyBlkSum(bs, m, []byte(readESP(t, b, layout.KernelFile)), []byte(readESP(t, b, layout.InitrdFile))); len(errs) != 0 {
		t.Fatalf("B: BLKSUM does not describe its new payload: %v", errs)
	}
	// B: BLKSUM written before CHECKSUM.
	var bOrder []string
	for _, o := range order {
		if strings.HasPrefix(o, b.dir+" ") {
			bOrder = append(bOrder, strings.TrimPrefix(o, b.dir+" "))
		}
	}
	if strings.Join(bOrder, ",") != layout.BlkSumFile+","+layout.PayloadSumFile {
		t.Errorf("B's manifest write order %v, want BLKSUM then CHECKSUM", bOrder)
	}
	writeManifestFile = orig
	var vcode int
	vout := captureOutput(t, func() { vcode = runVerify("/", "", hostOpts{}, false, false, false, verifyFiles{}) })
	if vcode != exitFailure || !strings.Contains(vout, "ESP AAAA-0001 (/dev/sda1) (generation 1, build 20261001T000000Z) is STALE") {
		t.Fatalf("verify does not name the stale member: %d\n%s", vcode, vout)
	}
}
