package bootenv

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unidoc/alpine-zfsboot/internal/espmember"
	"github.com/unidoc/alpine-zfsboot/internal/layout"
)

// readHexSector reads testdata/<name>.hex (the sector as hex text, so the
// fixture stays reviewable and diff/patch-friendly).
func readHexSector(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name+".hex"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := hex.DecodeString(strings.Join(strings.Fields(string(raw)), ""))
	if err != nil || len(b) != 512 {
		t.Fatalf("%s: %d bytes, %v", name, len(b), err)
	}
	return b
}

// The golden boot sectors are the first 512 bytes of real images made by
// `mkfs.vfat -F32 -n EFI -i 6AC50B94` and `mkfs.vfat -F16 -n BOOTX -i
// 1234ABCD`; Alpine 3.24's busybox blkid reports them as
// LABEL="EFI" UUID="6AC5-0B94" and LABEL="BOOTX" UUID="1234-ABCD".
func TestFATVolumeInfo_MatchesBlkid(t *testing.T) {
	for file, want := range map[string][2]string{
		"mkfs-vfat-F32-EFI-6AC50B94.bootsector":   {"6AC5-0B94", "EFI"},
		"mkfs-vfat-F16-BOOTX-1234ABCD.bootsector": {"1234-ABCD", "BOOTX"},
	} {
		sector := readHexSector(t, file)
		uuid, label, ok := fatVolumeInfo(sector)
		if !ok || uuid != want[0] || label != want[1] {
			t.Errorf("%s: got %q %q %v, blkid says %q %q", file, uuid, label, ok, want[0], want[1])
		}
		// Negative control: a different serial must not read as the same UUID.
		bad := append([]byte(nil), sector...)
		if strings.Contains(file, "F32") {
			bad[fat32VolIDOff] ^= 0x01
		} else {
			bad[fat16VolIDOff] ^= 0x01
		}
		if u, _, _ := fatVolumeInfo(bad); u == want[0] {
			t.Errorf("%s: flipped serial still reads %s", file, u)
		}
	}
	if _, _, ok := fatVolumeInfo(make([]byte, 512)); ok {
		t.Error("an all-zero sector read as FAT")
	}
	if _, _, ok := fatVolumeInfo([]byte("short")); ok {
		t.Error("a short read read as FAT")
	}
}

const (
	idA = "0f6c1e9a-4b1d-4c7e-9a55-1d2e3f405162"
	idB = "9a8b7c6d-1111-4222-8333-444455556666"
)

func esp(dev, uuid string, opts ...func(*ESPInfo)) ESPInfo {
	e := ESPInfo{FATVolume: FATVolume{Dev: dev, UUID: uuid, Label: "EFI"}, AlpineDir: true, Qualifies: true}
	for _, o := range opts {
		o(&e)
	}
	return e
}

func member(id string, gen uint64, siblings ...string) func(*ESPInfo) {
	return func(e *ESPInfo) {
		e.Member = &espmember.Member{InstallID: id, Generation: gen, ESPUUID: e.UUID, Members: siblings}
	}
}

func copiedFrom(uuid string) func(*ESPInfo) {
	return func(e *ESPInfo) { e.Member = &espmember.Member{InstallID: idA, Generation: 9, ESPUUID: uuid} }
}

func configList(l string) func(*ESPInfo) { return func(e *ESPInfo) { e.ConfigList = l } }
func label(l string) func(*ESPInfo)      { return func(e *ESPInfo) { e.Label = l } }
func bare(e *ESPInfo)                    { e.AlpineDir, e.Qualifies = false, false }

func uuidsOf(h HostSet) string { return strings.Join(h.UUIDs(), ",") }

// The scenario table shared (in spirit and names) with tests/run-tests.sh's
// init/esp-select.sh tests.
func TestSelectHost(t *testing.T) {
	type tc struct {
		name    string
		infos   []ESPInfo
		opts    SelectOpts
		mode    string
		members string // best first
		missing string
		errSub  string
		ignored int
	}
	for _, c := range []tc{
		{name: "legacy single ESP - unchanged",
			infos: []ESPInfo{esp("/dev/sda1", "AAAA-0001"), esp("/dev/sdb1", "BBBB-0001", bare)},
			mode:  ModeLegacy, members: "AAAA-0001"},
		{name: "legacy single ESP needs the EFI label, as before",
			infos:  []ESPInfo{esp("/dev/sda1", "AAAA-0001", label("BOOT"))},
			errSub: "no alpine-zfsboot ESP found"},
		{name: "two legacy ESPs, no list: refuse, name UUIDs and the way out",
			infos:  []ESPInfo{esp("/dev/sda1", "AAAA-0001"), esp("/dev/sdb1", "BBBB-0001")},
			errSub: "esp adopt AAAA-0001 BBBB-0001 --yes"},
		{name: "two legacy ESPs agreeing on a config list",
			infos: []ESPInfo{esp("/dev/sda1", "AAAA-0001", configList("BBBB-0001,AAAA-0001")), esp("/dev/sdb1", "BBBB-0001", configList("BBBB-0001,AAAA-0001"))},
			mode:  ModeList, members: "BBBB-0001,AAAA-0001"},
		{name: "two legacy ESPs with conflicting config lists: refuse",
			infos:  []ESPInfo{esp("/dev/sda1", "AAAA-0001", configList("AAAA-0001")), esp("/dev/sdb1", "BBBB-0001", configList("BBBB-0001"))},
			errSub: "refusing to guess"},
		{name: "same install id, generations differ: newest first",
			infos: []ESPInfo{esp("/dev/sda1", "AAAA-0001", member(idA, 4, "AAAA-0001", "BBBB-0001")), esp("/dev/sdb1", "BBBB-0001", member(idA, 5, "AAAA-0001", "BBBB-0001"))},
			mode:  ModeMember, members: "BBBB-0001,AAAA-0001"},
		{name: "equal generations: ordered by UUID, not by device name",
			infos: []ESPInfo{esp("/dev/sda1", "BBBB-0001", member(idA, 5)), esp("/dev/sdb1", "AAAA-0001", member(idA, 5))},
			mode:  ModeMember, members: "AAAA-0001,BBBB-0001"},
		{name: "equal generations: the primary (mounted /boot/efi or BIOS-booted) first",
			infos: []ESPInfo{esp("/dev/sda1", "AAAA-0001", member(idA, 5)), esp("/dev/sdb1", "BBBB-0001", member(idA, 5))},
			opts:  SelectOpts{PrimaryUUID: "BBBB-0001"},
			mode:  ModeMember, members: "BBBB-0001,AAAA-0001"},
		{name: "missing member detected from MEMBERS= without any list",
			infos: []ESPInfo{esp("/dev/sdb1", "BBBB-0001", member(idA, 5, "AAAA-0001", "BBBB-0001"))},
			mode:  ModeMember, members: "BBBB-0001", missing: "AAAA-0001"},
		{name: "foreign ESP (other install id) beside ours: ours only, foreign ignored",
			infos: []ESPInfo{esp("/dev/sda1", "AAAA-0001", member(idA, 5, "AAAA-0001")), esp("/dev/sdc1", "CCCC-0001", member(idB, 50, "CCCC-0001"))},
			opts:  SelectOpts{PrimaryUUID: "AAAA-0001"},
			mode:  ModeMember, members: "AAAA-0001", ignored: 1},
		{name: "two installations and nothing to choose: refuse",
			infos:  []ESPInfo{esp("/dev/sda1", "AAAA-0001", member(idA, 5)), esp("/dev/sdc1", "CCCC-0001", member(idB, 50))},
			errSub: "more than one alpine-zfsboot installation"},
		{name: "legacy ESP beside members: ignored with adopt hint",
			infos: []ESPInfo{esp("/dev/sda1", "AAAA-0001", member(idA, 1)), esp("/dev/sdb1", "BBBB-0001")},
			mode:  ModeMember, members: "AAAA-0001", ignored: 1},
		{name: "copied marker is not a member",
			infos: []ESPInfo{esp("/dev/sda1", "AAAA-0001", member(idA, 3)), esp("/dev/sdb1", "BBBB-0001", copiedFrom("AAAA-0001"))},
			mode:  ModeMember, members: "AAAA-0001", ignored: 1},
		{name: "copied marker alone is not a legacy candidate either",
			infos:  []ESPInfo{esp("/dev/sdb1", "BBBB-0001", copiedFrom("AAAA-0001"))},
			errSub: "no alpine-zfsboot ESP found"},
		{name: "dd clone (duplicate UUID) of a member: refuse",
			infos:  []ESPInfo{esp("/dev/sda1", "AAAA-0001", member(idA, 3)), esp("/dev/sdb1", "AAAA-0001", member(idA, 3))},
			errSub: "more than one device"},
		{name: "config list of our best member is used",
			infos: []ESPInfo{esp("/dev/sda1", "AAAA-0001", member(idA, 3), configList("BBBB-0001,AAAA-0001")), esp("/dev/sdb1", "BBBB-0001", member(idA, 3))},
			mode:  ModeList, members: "BBBB-0001,AAAA-0001"}, // equal generations: list order
		{name: "a foreign ESP's config list is never used",
			infos: []ESPInfo{esp("/dev/sda1", "AAAA-0001", member(idA, 3)), esp("/dev/sdc1", "CCCC-0001", member(idB, 99), configList("CCCC-0001,AAAA-0001"))},
			opts:  SelectOpts{PrimaryUUID: "AAAA-0001"},
			mode:  ModeMember, members: "AAAA-0001", ignored: 1},
		{name: "explicit list: listed only, missing reported, unlisted ignored",
			infos: []ESPInfo{esp("/dev/sda1", "AAAA-0001"), esp("/dev/sdc1", "CCCC-0001")},
			opts:  SelectOpts{List: []string{"BBBB-0001", "AAAA-0001"}},
			mode:  ModeList, members: "AAAA-0001", missing: "BBBB-0001", ignored: 1},
		{name: "explicit list: an ESP not yet prepared (no EFI/ALPINE) is still a member",
			infos: []ESPInfo{esp("/dev/sda1", "AAAA-0001", member(idA, 2)), esp("/dev/sdb1", "BBBB-0001", bare)},
			opts:  SelectOpts{List: []string{"AAAA-0001", "BBBB-0001"}},
			mode:  ModeList, members: "AAAA-0001,BBBB-0001"},
		{name: "explicit list: none present",
			infos:  []ESPInfo{esp("/dev/sda1", "AAAA-0001")},
			opts:   SelectOpts{List: []string{"BBBB-0001"}},
			errSub: "none of the ESPs listed"},
		{name: "explicit list: listed UUID on two devices",
			infos:  []ESPInfo{esp("/dev/sda1", "AAAA-0001"), esp("/dev/sdb1", "AAAA-0001")},
			opts:   SelectOpts{List: []string{"AAAA-0001"}},
			errSub: "more than one device"},
	} {
		t.Run(c.name, func(t *testing.T) {
			hs, err := SelectHost(c.infos, c.opts)
			if c.errSub != "" {
				if err == nil || !strings.Contains(err.Error(), c.errSub) {
					t.Fatalf("want error containing %q, got %v (%+v)", c.errSub, err, hs)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if hs.Mode != c.mode || uuidsOf(hs) != c.members || strings.Join(hs.Missing, ",") != c.missing || len(hs.Ignored) != c.ignored {
				t.Fatalf("got mode %s members %s missing %v ignored %d (%v); want %s %s %s %d", hs.Mode, uuidsOf(hs), hs.Missing, len(hs.Ignored), hs.Ignored, c.mode, c.members, c.missing, c.ignored)
			}
		})
	}
}

func TestSelectHost_ListFlagsForeignMember(t *testing.T) {
	hs, err := SelectHost([]ESPInfo{
		esp("/dev/sda1", "AAAA-0001", member(idA, 4)),
		esp("/dev/sdc1", "CCCC-0001", member(idB, 9)),
	}, SelectOpts{List: []string{"AAAA-0001", "CCCC-0001"}})
	if err != nil {
		t.Fatal(err)
	}
	// The FIRST listed marked ESP decides the identity (idA), even though
	// CCCC carries a higher generation; CCCC is flagged, never merged.
	if hs.InstallID != idA {
		t.Fatalf("identity %s, want the first listed one %s", hs.InstallID, idA)
	}
	for _, m := range hs.Members {
		if m.Foreign != (m.UUID == "CCCC-0001") {
			t.Fatalf("foreign flags wrong: %+v", hs.Members)
		}
	}
}

// End to end over fixture files: the scan reads real boot sectors from
// files standing in for /dev nodes and probes directories standing in for
// mounted ESPs - no mount, no loop device.
func TestScanESPs_Fixtures(t *testing.T) {
	dir := t.TempDir()
	golden := readHexSector(t, "mkfs-vfat-F32-EFI-6AC50B94.bootsector")
	devA := filepath.Join(dir, "sda1")
	os.WriteFile(devA, golden, 0o644)
	devX := filepath.Join(dir, "sdx")
	os.WriteFile(devX, make([]byte, 512), 0o644)
	mpA := filepath.Join(dir, "mnt-a")
	os.MkdirAll(filepath.Join(mpA, layout.ESPDir), 0o755)
	os.MkdirAll(filepath.Join(mpA, layout.EFIBootDir), 0o755)
	os.WriteFile(filepath.Join(mpA, layout.EFIBootDir, "BOOTX64.EFI"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(mpA, layout.ConfigFile), []byte("alpine-zfsboot.esp-uuids=6AC5-0B94\r\n"), 0o644)
	espmember.Write(mpA, espmember.Member{InstallID: idA, Generation: 2, ESPUUID: "6AC5-0B94", Members: []string{"6AC5-0B94"}})

	oldList, oldProbe := listBlockDevices, probeESP
	defer func() { listBlockDevices, probeESP = oldList, oldProbe }()
	listBlockDevices = func() ([]string, error) { return []string{devX, devA, filepath.Join(dir, "missing")}, nil }
	probeESP = func(v FATVolume) ESPInfo { return ProbeMounted(v, mpA) }

	infos, err := ScanESPs()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].UUID != "6AC5-0B94" || !infos[0].ValidMember() || !infos[0].Qualifies || infos[0].ConfigList != "6AC5-0B94" {
		t.Fatalf("scan: %+v", infos)
	}
	hs, err := SelectHost(infos, SelectOpts{})
	if err != nil || hs.Mode != ModeList || uuidsOf(hs) != "6AC5-0B94" || hs.InstallID != idA {
		t.Fatalf("%+v %v", hs, err)
	}
}

func TestDiskOfESP(t *testing.T) {
	root := t.TempDir()
	old := sysClassBlock
	defer func() { sysClassBlock = old }()
	sysClassBlock = filepath.Join(root, "class", "block")
	os.MkdirAll(sysClassBlock, 0o755)
	mk := func(path string, partition bool) {
		full := filepath.Join(root, "devices", path)
		os.MkdirAll(full, 0o755)
		if partition {
			os.WriteFile(filepath.Join(full, "partition"), []byte("1\n"), 0o644)
		}
		rel, _ := filepath.Rel(sysClassBlock, full)
		os.Symlink(rel, filepath.Join(sysClassBlock, filepath.Base(path)))
	}
	mk("pci/ata1/block/sda", false)
	mk("pci/ata1/block/sda/sda1", true)
	mk("pci/nvme/block/nvme0n1", false)
	mk("pci/nvme/block/nvme0n1/nvme0n1p2", true)
	mk("virtual/block/md127", false)
	mk("virtual/block/md126/md126p1", true)
	mk("pci/ata2/block/sdb", false) // whole-disk FAT

	for dev, want := range map[string]string{"/dev/sda1": "/dev/sda", "/dev/nvme0n1p2": "/dev/nvme0n1"} {
		if got, err := DiskOfESP(dev); err != nil || got != want {
			t.Errorf("DiskOfESP(%s) = %q, %v; want %s", dev, got, err, want)
		}
	}
	for dev, sub := range map[string]string{"/dev/md127": "software RAID", "/dev/md126p1": "software RAID", "/dev/sdb": "not a partition"} {
		if _, err := DiskOfESP(dev); err == nil || !strings.Contains(err.Error(), sub) {
			t.Errorf("DiskOfESP(%s): want %q, got %v", dev, sub, err)
		}
	}
	// No sysfs entry (a fixture file): name-based fallback, same refusals.
	sysClassBlock = filepath.Join(root, "nonexistent")
	if got, err := DiskOfESP("/dev/vdb1"); err != nil || got != "/dev/vdb" {
		t.Errorf("fallback: %q %v", got, err)
	}
	if _, err := DiskOfESP("/dev/vdb"); err == nil {
		t.Error("fallback accepted a whole disk")
	}
	if _, err := DiskOfESP("/dev/md0p1"); err == nil {
		t.Error("fallback accepted an md partition")
	}
}
