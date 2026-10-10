package bootenv

// Mirrored boot: which ESPs belong to this host (README "Mirrored boot:
// how members are recognised"). This file is the Go side of the rules
// init/esp-select.sh applies at boot; both are tested against the same
// scenarios (cmd/tool's mirror tests and tests/run-tests.sh).
//
// Sources, strongest first:
//
//  1. An explicit list of FAT volume UUIDs (alpine-zfsboot.esp-uuids=,
//     --esp-uuids, ALPINE_ZFSBOOT_ESP_UUIDS, or the config of a trusted
//     ESP - see SelectHost).
//  2. Identity markers (EFI/ALPINE/MEMBER, internal/espmember): every ESP
//     whose marker carries this host's INSTALL_ID and its own FAT UUID.
//  3. Neither (every install before 0.5.0, "legacy"): today's single-ESP
//     auto-discovery, unchanged - exactly one qualifying LABEL=EFI ESP.
//
// An ESP that only LOOKS like ours (EFI/ALPINE present, but a different
// INSTALL_ID, a marker copied from another ESP, or no marker while
// marked ESPs exist) is never picked, and the tool never writes to it
// without an explicit act (--add-esp, esp adopt, --force-adopt).

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/unidoc/alpine-zfsboot/internal/espconfig"
	"github.com/unidoc/alpine-zfsboot/internal/espmember"
	"github.com/unidoc/alpine-zfsboot/internal/layout"
)

// FAT boot-sector fields for the volume serial number ("UUID" in blkid's
// output, printed as %04X-%04X of the little-endian 32-bit BS_VolID).
// FAT32: BS_BootSig at 66, BS_VolID at 67, BS_VolLab at 71, BS_FilSysType
// at 82. FAT12/16: BS_BootSig at 38, BS_VolID at 39, BS_VolLab at 43,
// BS_FilSysType at 54. Checked against a real mkfs.vfat -i image and
// Alpine's busybox blkid (see espset_test.go).
const (
	fat32BootSigOff = 66
	fat32VolIDOff   = 67
	fat16BootSigOff = 38
	fat16VolIDOff   = 39
	fat16LabelOff   = 43
	fat16FSTypeOff  = 54
)

// FATVolume is one FAT filesystem found on a block device.
type FATVolume struct {
	Dev   string
	UUID  string // XXXX-XXXX
	Label string
}

// fatVolumeInfo decodes a FAT12/16/32 boot sector's serial number and
// label. ok is false for anything that is not a FAT boot sector with an
// extended boot signature (0x29, or 0x28 which has the serial but no
// label/type fields).
func fatVolumeInfo(sector []byte) (uuid, label string, ok bool) {
	if len(sector) < fatBPBMinSize {
		return "", "", false
	}
	serial := func(off int) string {
		v := uint32(sector[off]) | uint32(sector[off+1])<<8 | uint32(sector[off+2])<<16 | uint32(sector[off+3])<<24
		return fmt.Sprintf("%04X-%04X", v>>16, v&0xffff)
	}
	if string(sector[fatFSTypeOff:fatFSTypeOff+fatFSTypeLen]) == "FAT32   " && (sector[fat32BootSigOff] == 0x29 || sector[fat32BootSigOff] == 0x28) {
		label, _ := fat32VolumeLabel(sector)
		return serial(fat32VolIDOff), label, true
	}
	ft := string(sector[fat16FSTypeOff : fat16FSTypeOff+8])
	if (ft == "FAT12   " || ft == "FAT16   " || ft == "FAT     ") && sector[fat16BootSigOff] == 0x29 {
		return serial(fat16VolIDOff), strings.TrimRight(string(sector[fat16LabelOff:fat16LabelOff+11]), " "), true
	}
	return "", "", false
}

// listBlockDevices / readFirstBytes / probeESP are the scan's real I/O,
// as variables so tests can run the whole scan over fixture files and
// directories (no mount, no loop device).
var (
	listBlockDevices = enumerateBlockDevices
	readFirstBytes   = func(dev string, n int) ([]byte, error) {
		f, err := os.Open(dev)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		b := make([]byte, n)
		_, err = io.ReadFull(f, b)
		return b, err
	}
	probeESP = probeESPFiles
)

// ScanFATVolumes lists every block device holding a FAT filesystem, with
// its UUID and label (no mount).
func ScanFATVolumes() ([]FATVolume, error) {
	devs, err := listBlockDevices()
	if err != nil {
		return nil, err
	}
	var out []FATVolume
	for _, dev := range devs {
		sector, err := readFirstBytes(dev, fatBPBMinSize)
		if err != nil {
			continue
		}
		if uuid, label, ok := fatVolumeInfo(sector); ok {
			out = append(out, FATVolume{Dev: dev, UUID: uuid, Label: label})
		}
	}
	return out, nil
}

// VolumeUUID returns the FAT UUID of the filesystem on dev.
func VolumeUUID(dev string) (string, error) {
	sector, err := readFirstBytes(dev, fatBPBMinSize)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", dev, err)
	}
	uuid, _, ok := fatVolumeInfo(sector)
	if !ok {
		return "", fmt.Errorf("%s does not hold a FAT filesystem", dev)
	}
	return uuid, nil
}

// ESPInfo is what the scan learned about one FAT volume.
type ESPInfo struct {
	FATVolume
	AlpineDir  bool              // EFI/ALPINE exists
	Qualifies  bool              // init's legacy markers: a boot binary/KERNEL and config/keys/host key
	Member     *espmember.Member // decoded EFI/ALPINE/MEMBER, nil when absent or broken
	MemberErr  error             // MEMBER present but does not decode
	ConfigList string            // alpine-zfsboot.esp-uuids= in its EFI/ALPINE/config, raw
	ProbeErr   error             // could not mount/read it
}

// ValidMember reports whether e carries an intact marker bound to itself.
func (e ESPInfo) ValidMember() bool {
	return e.Member != nil && e.MemberErr == nil && e.Member.ESPUUID == e.UUID
}

// CopiedMarker: a decodable MEMBER that names a different ESP_UUID - the
// EFI tree was copied from another ESP. Not a member.
func (e ESPInfo) CopiedMarker() bool {
	return e.Member != nil && e.MemberErr == nil && e.Member.ESPUUID != e.UUID
}

// Generation is the member generation, 0 without a valid marker.
func (e ESPInfo) Generation() uint64 {
	if e.ValidMember() {
		return e.Member.Generation
	}
	return 0
}

// probeESPFiles mounts dev read-only (MountESP) and reads the facts.
func probeESPFiles(v FATVolume) ESPInfo {
	info := ESPInfo{FATVolume: v}
	mp, cleanup, err := MountESP(v.Dev, true)
	if err != nil {
		info.ProbeErr = err
		return info
	}
	defer cleanup()
	return ProbeMounted(v, mp)
}

// ProbeMounted gathers ESPInfo from an already-mounted ESP at mp.
func ProbeMounted(v FATVolume, mp string) ESPInfo {
	info := ESPInfo{FATVolume: v}
	if st, err := os.Stat(filepath.Join(mp, layout.ESPDir)); err == nil && st.IsDir() {
		info.AlpineDir = true
	}
	c, err := probeMarkers(mp)
	if err == nil {
		info.Qualifies = c.qualifies()
	}
	if m, present, err := espmember.Read(mp); present {
		if err != nil {
			info.MemberErr = err
		} else {
			info.Member = &m
		}
	}
	if raw, err := os.ReadFile(filepath.Join(mp, layout.ConfigFile)); err == nil {
		info.ConfigList, _ = espconfig.ConfigValue(raw, layout.ESPUUIDsKey)
	}
	return info
}

// ScanESPs scans every FAT volume and probes each one (read-only).
func ScanESPs() ([]ESPInfo, error) {
	vols, err := ScanFATVolumes()
	if err != nil {
		return nil, err
	}
	out := make([]ESPInfo, 0, len(vols))
	for _, v := range vols {
		out = append(out, probeESP(v))
	}
	return out, nil
}

// SelectOpts are the inputs of SelectHost besides the scan itself.
type SelectOpts struct {
	List        []string // explicit list (already parsed), nil = none
	ListSource  string   // where List came from, for messages
	PrimaryUUID string   // the ESP mounted at /boot/efi, or the BIOS-booted one (alpine-zfsboot.esp-self=)
}

// Mode values of HostSet.
const (
	ModeList   = "list"   // explicit UUID list
	ModeMember = "member" // identity markers
	ModeLegacy = "legacy" // one unmarked ESP, auto-discovered as before 0.5.0
)

// HostMember is one ESP of this host's set.
type HostMember struct {
	ESPInfo
	// Foreign: listed explicitly, but its marker names another
	// installation. Reported; written only with --force-adopt.
	Foreign bool
}

// Ignored is an ESP that looks like alpine-zfsboot's but is not used.
type Ignored struct {
	ESPInfo
	Reason string
}

// HostSet is the result of SelectHost.
type HostSet struct {
	Mode       string
	ListSource string
	InstallID  string       // "" when no member carries a marker yet
	Members    []HostMember // present members, best first (highest generation, then the primary, then list/UUID order)
	Expected   []string     // every UUID that belongs to the set (present or missing), for MEMBERS=
	Missing    []string     // expected but not present
	Ignored    []Ignored
}

// UUIDs returns the present members' UUIDs, in Members order.
func (h HostSet) UUIDs() []string {
	var out []string
	for _, m := range h.Members {
		out = append(out, m.UUID)
	}
	return out
}

// MaxGeneration is the highest generation among the members.
func (h HostSet) MaxGeneration() uint64 {
	var g uint64
	for _, m := range h.Members {
		if m.Generation() > g {
			g = m.Generation()
		}
	}
	return g
}

func countUUIDs(infos []ESPInfo) map[string][]string {
	c := map[string][]string{}
	for _, i := range infos {
		c[i.UUID] = append(c[i.UUID], i.Dev)
	}
	return c
}

func dupError(uuid string, devs []string) error {
	return fmt.Errorf("FAT UUID %s is on more than one device (%s) - a cloned disk, or an md/dm RAID member; refusing to guess which one is this host's ESP (change one with 'mlabel -n' / 'fatlabel -i', or remove the clone)", uuid, strings.Join(devs, ", "))
}

func infoByUUID(infos []ESPInfo, uuid string) (ESPInfo, bool) {
	for _, i := range infos {
		if i.UUID == uuid {
			return i, true
		}
	}
	return ESPInfo{}, false
}

// orderMembers sorts best first: highest generation, then the primary,
// then the given rank (list position), then UUID - never device names,
// which can swap between boots.
func orderMembers(ms []HostMember, primary string, rank map[string]int) {
	sort.SliceStable(ms, func(a, b int) bool {
		ga, gb := ms[a].Generation(), ms[b].Generation()
		if ga != gb {
			return ga > gb
		}
		pa, pb := ms[a].UUID == primary, ms[b].UUID == primary
		if pa != pb {
			return pa
		}
		ra, oka := rank[ms[a].UUID]
		rb, okb := rank[ms[b].UUID]
		if oka && okb && ra != rb {
			return ra < rb
		}
		return ms[a].UUID < ms[b].UUID
	})
}

// SelectHost decides which ESPs belong to this host. infos is the scan
// (every FAT volume). An error means "ambiguous or nothing found" - the
// caller must not guess.
func SelectHost(infos []ESPInfo, opts SelectOpts) (HostSet, error) {
	counts := countUUIDs(infos)
	if len(opts.List) > 0 {
		return selectList(infos, counts, opts.List, opts.ListSource, opts.PrimaryUUID)
	}

	groups := map[string][]ESPInfo{}
	var ignored []Ignored
	for _, i := range infos {
		switch {
		case i.ValidMember():
			groups[i.Member.InstallID] = append(groups[i.Member.InstallID], i)
		case i.CopiedMarker():
			ignored = append(ignored, Ignored{i, fmt.Sprintf("its MEMBER marker belongs to ESP %s (copied files) - add it properly with 'alpine-zfsboot esp add %s'", i.Member.ESPUUID, i.UUID)})
		case i.MemberErr != nil:
			ignored = append(ignored, Ignored{i, fmt.Sprintf("its MEMBER marker is unreadable (%v)", i.MemberErr)})
		}
	}

	if len(groups) == 0 {
		return selectLegacy(infos, counts, ignored)
	}

	var id string
	if p, ok := infoByUUID(infos, opts.PrimaryUUID); ok && opts.PrimaryUUID != "" && p.ValidMember() {
		if len(counts[p.UUID]) > 1 {
			return HostSet{}, dupError(p.UUID, counts[p.UUID])
		}
		id = p.Member.InstallID
	} else if len(groups) == 1 {
		for k := range groups {
			id = k
		}
	} else {
		var desc []string
		var ids []string
		for k := range groups {
			ids = append(ids, k)
		}
		sort.Strings(ids)
		for _, k := range ids {
			var us []string
			for _, i := range groups[k] {
				us = append(us, i.UUID+" ("+i.Dev+")")
			}
			desc = append(desc, fmt.Sprintf("install id %s: %s", k, strings.Join(us, ", ")))
		}
		return HostSet{}, fmt.Errorf("ESPs of more than one alpine-zfsboot installation are present (%s) and nothing says which one is this host's - set %s=<this host's ESP UUIDs> (--esp-uuids), or remove the other installation's marker with 'alpine-zfsboot esp remove <uuid>'", strings.Join(desc, "; "), layout.ESPUUIDsKey)
	}

	hs := HostSet{Mode: ModeMember, InstallID: id}
	present := map[string]bool{}
	for _, i := range groups[id] {
		if len(counts[i.UUID]) > 1 {
			return HostSet{}, dupError(i.UUID, counts[i.UUID])
		}
		hs.Members = append(hs.Members, HostMember{ESPInfo: i})
		present[i.UUID] = true
	}
	orderMembers(hs.Members, opts.PrimaryUUID, nil)

	// A list in the best member's own config is an explicit list too - it
	// comes from this host's own trusted marker group, never from an
	// arbitrary ESP (a foreign stick's config could list itself).
	if best := hs.Members[0]; best.ConfigList != "" {
		l, err := espmember.ParseUUIDList(best.ConfigList)
		if err != nil {
			return HostSet{}, fmt.Errorf("%s= in the config of ESP %s: %w", layout.ESPUUIDsKey, best.UUID, err)
		}
		return selectList(infos, counts, l, "EFI/ALPINE/config of ESP "+best.UUID, opts.PrimaryUUID)
	}

	expected := map[string]bool{}
	for _, u := range hs.Members[0].Member.Members {
		expected[u] = true
	}
	for u := range present {
		expected[u] = true
	}
	for u := range expected {
		hs.Expected = append(hs.Expected, u)
		if !present[u] {
			hs.Missing = append(hs.Missing, u)
		}
	}
	sort.Strings(hs.Expected)
	sort.Strings(hs.Missing)

	for _, i := range infos {
		if present[i.UUID] {
			continue
		}
		switch {
		case i.ValidMember():
			ignored = append(ignored, Ignored{i, fmt.Sprintf("belongs to another installation (install id %s)", i.Member.InstallID)})
		case i.Member == nil && i.MemberErr == nil && i.AlpineDir:
			ignored = append(ignored, Ignored{i, "has EFI/ALPINE but no MEMBER marker (a legacy or foreign ESP) - 'alpine-zfsboot esp adopt " + i.UUID + " --yes' makes it a member"})
		}
	}
	hs.Ignored = ignored
	return hs, nil
}

func selectLegacy(infos []ESPInfo, counts map[string][]string, ignored []Ignored) (HostSet, error) {
	var qual []ESPInfo
	for _, i := range infos {
		// A copied or unreadable marker is "not a member" - and never a
		// legacy candidate either (init/esp-select.sh: the same rule).
		if i.Label == layout.FATVolumeLabel && i.Qualifies && i.Member == nil && i.MemberErr == nil {
			qual = append(qual, i)
		}
	}
	switch len(qual) {
	case 0:
		return HostSet{}, fmt.Errorf("no alpine-zfsboot ESP found (checked %d FAT volume(s): none carries a MEMBER marker, and none labeled %q carries both a boot binary and config/keys)", len(infos), layout.FATVolumeLabel)
	case 1:
		i := qual[0]
		if len(counts[i.UUID]) > 1 {
			return HostSet{}, dupError(i.UUID, counts[i.UUID])
		}
		if i.ConfigList != "" {
			l, err := espmember.ParseUUIDList(i.ConfigList)
			if err != nil {
				return HostSet{}, fmt.Errorf("%s= in the config of ESP %s: %w", layout.ESPUUIDsKey, i.UUID, err)
			}
			return selectList(infos, counts, l, "EFI/ALPINE/config of ESP "+i.UUID, "")
		}
		return HostSet{Mode: ModeLegacy, Members: []HostMember{{ESPInfo: i}}, Expected: []string{i.UUID}, Ignored: ignored}, nil
	}
	// Named in UUID order (not device order, which can change between boots).
	sort.Slice(qual, func(a, b int) bool { return qual[a].UUID < qual[b].UUID })
	// Several unmarked ESPs: the pure-legacy case may name its set in its
	// configs (the original esp-uuids design); all lists must agree.
	var list string
	agree := true
	for _, i := range qual {
		if i.ConfigList == "" {
			continue
		}
		if list != "" && i.ConfigList != list {
			agree = false
		}
		list = i.ConfigList
	}
	if list != "" && agree {
		l, err := espmember.ParseUUIDList(list)
		if err == nil {
			return selectList(infos, counts, l, "EFI/ALPINE/config of the unmarked ESPs", "")
		}
	}
	var us []string
	for _, i := range qual {
		us = append(us, i.UUID)
	}
	desc := make([]string, len(qual))
	for n, i := range qual {
		desc[n] = fmt.Sprintf("%s (UUID %s)", i.Dev, i.UUID)
	}
	return HostSet{}, fmt.Errorf("more than one alpine-zfsboot ESP found (%s) - refusing to guess. If they are this host's mirrored ESPs, adopt them: 'alpine-zfsboot esp adopt %s --yes', or list them: %s=%s (--esp-uuids, ALPINE_ZFSBOOT_ESP_UUIDS, or in EFI/ALPINE/config)",
		strings.Join(desc, ", "), strings.Join(us, " "), layout.ESPUUIDsKey, strings.Join(us, ","))
}

func selectList(infos []ESPInfo, counts map[string][]string, list []string, source, primary string) (HostSet, error) {
	hs := HostSet{Mode: ModeList, ListSource: source, Expected: append([]string(nil), list...)}
	rank := map[string]int{}
	listed := map[string]bool{}
	for n, u := range list {
		rank[u] = n
		listed[u] = true
		switch len(counts[u]) {
		case 0:
			hs.Missing = append(hs.Missing, u)
		case 1:
			i, _ := infoByUUID(infos, u)
			hs.Members = append(hs.Members, HostMember{ESPInfo: i})
		default:
			return HostSet{}, dupError(u, counts[u])
		}
	}
	if len(hs.Members) == 0 {
		return HostSet{}, fmt.Errorf("none of the ESPs listed in %s= (%s, from %s) is present", layout.ESPUUIDsKey, strings.Join(list, ","), source)
	}
	orderMembers(hs.Members, primary, rank)
	// Identity of the set: that of the FIRST listed member with a marker -
	// the operator's order decides, never a generation number another
	// installation's disk could carry. Listed members marked for another
	// installation are flagged foreign.
	for _, u := range list {
		if i, ok := infoByUUID(infos, u); ok && len(counts[u]) == 1 && i.ValidMember() {
			hs.InstallID = i.Member.InstallID
			break
		}
	}
	for n := range hs.Members {
		if hs.Members[n].ValidMember() && hs.Members[n].Member.InstallID != hs.InstallID {
			hs.Members[n].Foreign = true
		}
	}
	for _, i := range infos {
		if !listed[i.UUID] && i.AlpineDir {
			hs.Ignored = append(hs.Ignored, Ignored{i, "not in the " + layout.ESPUUIDsKey + " list"})
		}
	}
	return hs, nil
}

// sysClassBlock is /sys/class/block, a variable for tests.
var sysClassBlock = "/sys/class/block"

// DiskOfESP returns the whole disk a partition lives on, for writing the
// BIOS stage1/stage2 of that ESP's member. Refuses anything that is not a
// plain partition of a plain disk: a whole-disk FAT (stage1 would
// overwrite its boot sector) and md/dm devices (the BIOS boots the member
// disks, not the array).
func DiskOfESP(dev string) (string, error) {
	real := resolveSymlinkOrSelf(dev)
	name := filepath.Base(real)
	refuseParent := func(parent string) error {
		for _, p := range []string{"md", "dm-"} {
			if strings.HasPrefix(parent, p) {
				return fmt.Errorf("%s is on %s, a software RAID/device-mapper device - the BIOS boots from the member disks, not the array; give each disk its own ESP instead", dev, parent)
			}
		}
		return nil
	}
	sys := filepath.Join(sysClassBlock, name)
	if _, err := os.Stat(sys); err == nil {
		if _, err := os.Stat(filepath.Join(sys, "partition")); err != nil {
			if err := refuseParent(name); err != nil {
				return "", err
			}
			return "", fmt.Errorf("%s is not a partition (a filesystem on a whole disk) - the BIOS boot loader goes into a disk's first sectors and would overwrite it; refusing", dev)
		}
		link, err := filepath.EvalSymlinks(sys)
		if err != nil {
			return "", fmt.Errorf("resolving %s: %w", sys, err)
		}
		parent := filepath.Base(filepath.Dir(link))
		if err := refuseParent(parent); err != nil {
			return "", err
		}
		return "/dev/" + parent, nil
	}
	disk := DevicePartitionBase(real)
	if disk == real {
		if err := refuseParent(name); err != nil {
			return "", err
		}
		return "", fmt.Errorf("%s is not a partition (a filesystem on a whole disk) - the BIOS boot loader goes into a disk's first sectors and would overwrite it; refusing", dev)
	}
	if err := refuseParent(filepath.Base(disk)); err != nil {
		return "", err
	}
	return disk, nil
}
