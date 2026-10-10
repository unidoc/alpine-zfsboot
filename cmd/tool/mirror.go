// Mirrored boot (issue #24): every command that touches the ESP works on
// the SET of this host's ESPs - one per disk of a ZFS mirror - instead of
// "the one ESP". Which ESPs belong to the set is decided by
// internal/bootenv.SelectHost (explicit list, else EFI/ALPINE/MEMBER
// identity markers, else the single legacy ESP exactly as before 0.5.0);
// see README "Mirrored boot: how members are recognised".
//
// Rules this file implements:
//   - writes go to set members only; a new ESP joins only by an explicit
//     act (--add-esp, esp add, esp adopt), an ESP marked for ANOTHER
//     installation only with --force-adopt;
//   - one ESP at a time, each with the existing atomic/rollback writers,
//     so a failure on one ESP leaves that ESP on its previous generation
//     and the run continues with the others, then reports and exits 1;
//   - EFI/ALPINE/MEMBER is written LAST on each ESP (payload and config
//     first), with GENERATION = highest generation in the set + 1;
//   - config, authorized_keys and the rescue host key are the same on every
//     member (copied from the best member);
//   - a member that is expected but not present (a dead or detached disk)
//     is a warning with its own exit status (exitMemberMissing).
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/unidoc/alpine-zfsboot/internal/biosboot"
	"github.com/unidoc/alpine-zfsboot/internal/bootenv"
	"github.com/unidoc/alpine-zfsboot/internal/cmdline"
	"github.com/unidoc/alpine-zfsboot/internal/espconfig"
	"github.com/unidoc/alpine-zfsboot/internal/espmember"
	"github.com/unidoc/alpine-zfsboot/internal/layout"
	"github.com/unidoc/alpine-zfsboot/internal/netmac"
	"github.com/unidoc/alpine-zfsboot/internal/release"
	"github.com/unidoc/alpine-zfsboot/internal/uefiboot"
)

// Exit statuses of status/verify/update/install/integrity/int13chunk/esp.
const (
	exitFailure = 1
	// exitMemberMissing: everything that is present is fine, but an ESP of
	// the set is not (a dead or detached disk - a degraded mirror). Never
	// returned without a mirrored boot, so callers that only know 0/1
	// (alpine-installer) see no change on a single ESP.
	exitMemberMissing = 3
)

const exitCodesHelp = `1 when anything failed (on a mirrored boot: on any ESP), 3 when
everything present is fine but an ESP of the set is missing (a dead or
detached disk - a degraded mirror; never returned with a single ESP).`

const (
	envESPUUIDs = "ALPINE_ZFSBOOT_ESP_UUIDS"
	envNetMAC   = "ALPINE_ZFSBOOT_NET_MAC"
	// poolInstallIDProp: the pool user property the tool sets (best-effort)
	// to the set's INSTALL_ID; init warns after importing the pool when it
	// differs from the ESP it took its config from.
	poolInstallIDProp = "org.alpinezfsboot:install-id"
)

// The real I/O of discovery, as variables so the tests can run every
// command's member loop over directories and image files.
var (
	scanESPs         = bootenv.ScanESPs
	mountESP         = bootenv.MountESP
	diskOfESP        = bootenv.DiskOfESP
	detectDiskLayout = bootenv.DetectDiskLayout
	findMountAtPath  = bootenv.FindMountAtPath
	volumeUUID       = bootenv.VolumeUUID
	isUEFI           = bootenv.IsUEFI
	readProcCmdline  = func() string { b, _ := os.ReadFile("/proc/cmdline"); return string(b) }
	sysClassNet      = "/sys/class/net"
	nowUTC           = func() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }
	// runZFS runs the zfs binary for the pool property cross-check only
	// (internal/zfsnative stays read-only); a missing binary is an error
	// the callers report and ignore.
	runZFS = func(args ...string) (string, error) {
		path, err := exec.LookPath("zfs")
		if err != nil {
			return "", err
		}
		out, err := exec.Command(path, args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
)

// hostOpts are the mirrored-boot flags.
type hostOpts struct {
	espUUIDs   string // --esp-uuids
	addESP     string // --add-esp
	forceAdopt bool   // --force-adopt
}

func addHostFlags(cmd *cobra.Command, o *hostOpts, writer bool) {
	cmd.Flags().StringVar(&o.espUUIDs, "esp-uuids", "", "the FAT volume UUIDs of this host's ESPs, comma-separated (alpine-zfsboot.esp-uuids=; same as "+envESPUUIDs+") - an explicit set that overrides the MEMBER markers; see README \"Mirrored boot\"")
	if writer {
		cmd.Flags().StringVar(&o.addESP, "add-esp", "", "devices or FAT UUIDs (comma-separated) of ESPs to make members of this host's mirrored boot and write too - an ESP that is not a member is never written without this, 'esp add' or 'esp adopt'")
		cmd.Flags().BoolVar(&o.forceAdopt, "force-adopt", false, "also write to an ESP whose MEMBER marker names ANOTHER installation (e.g. a disk moved from another machine); it becomes a member of this host")
	}
}

func flagOrEnv(flag, env string) string {
	if flag != "" {
		return flag
	}
	return os.Getenv(env)
}

// cmdlineValue returns the value of the LAST key= word of a command line
// (the kernel's own and init's convention), "" when absent.
func cmdlineValue(line, key string) string {
	v := ""
	for _, w := range strings.Fields(firstCmdlineLine(line)) {
		if x, ok := strings.CutPrefix(w, key+"="); ok {
			v = x
		}
	}
	return v
}

// setCmdlineWord returns raw with every key= word removed from its first
// line and, when value is not empty, key=value appended - the rest of the
// file unchanged. Same shape as setCmdlineInt13Chunk.
func setCmdlineWord(raw []byte, key, value string) []byte {
	s := string(raw)
	line, rest := s, ""
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		line, rest = s[:i], s[i:]
	}
	var words []string
	for _, w := range strings.Fields(line) {
		if !strings.HasPrefix(w, key+"=") {
			words = append(words, w)
		}
	}
	if value != "" {
		words = append(words, key+"="+value)
	}
	return []byte(strings.Join(words, " ") + rest)
}

// stripMirrorWords removes the tool's per-ESP mirror words from a
// CMDLINE, for comparing CMDLINEs of different members (or against a
// release cmdline.txt).
func stripMirrorWords(raw []byte) []byte {
	return setCmdlineWord(setCmdlineWord(raw, layout.ESPSelfKey, ""), layout.ESPUUIDsKey, "")
}

// checkCmdlineLength refuses a CMDLINE stage2 would refuse to boot.
func checkCmdlineLength(raw []byte) error {
	if len(raw) > layout.CmdlineMaxBytes {
		return fmt.Errorf("%s would be %d bytes, the BIOS loader accepts at most %d", layout.CmdlineFile, len(raw), layout.CmdlineMaxBytes)
	}
	return nil
}

// verifyPayloadIgnoringMirrorWords is espconfig.VerifyPayload with the
// CMDLINE compared after stripMirrorWords on both sides.
func verifyPayloadIgnoringMirrorWords(mountpoint string, kernel, initrd, cmdlineBytes []byte) error {
	if err := espconfig.VerifyFile(mountpoint, layout.KernelFile, kernel); err != nil {
		return err
	}
	if err := espconfig.VerifyFile(mountpoint, layout.InitrdFile, initrd); err != nil {
		return err
	}
	got, err := os.ReadFile(filepath.Join(mountpoint, layout.CmdlineFile))
	if err != nil {
		return fmt.Errorf("reading %s: %w", layout.CmdlineFile, err)
	}
	if !bytes.Equal(stripMirrorWords(got), stripMirrorWords(cmdlineBytes)) {
		return fmt.Errorf("%s does not match its expected content", layout.CmdlineFile)
	}
	return nil
}

// --- the host's set ---------------------------------------------------

// member is one ESP of the set (or one being added to it).
type member struct {
	*target // nil when the ESP could not be mounted (mountErr)
	info    bootenv.ESPInfo
	foreign bool // marked for another installation (written only with --force-adopt)
	added   bool // --add-esp / esp add: not a member yet
	// mountErr/diskErr: this ESP cannot be used / (BIOS) its disk cannot
	// take stage1/stage2. Reported per ESP; the others go on.
	mountErr error
	diskErr  error
}

func (m *member) label() string {
	return fmt.Sprintf("ESP %s (%s)", m.info.UUID, m.info.Dev)
}

// usable reports why m cannot be read (write=false) or written.
func (m *member) usable(write, forceAdopt bool, uefi bool) error {
	if m.mountErr != nil {
		return m.mountErr
	}
	if !write {
		return nil
	}
	if m.foreign && !forceAdopt {
		return fmt.Errorf("its MEMBER marker names another installation (install id %s) - not written without --force-adopt", m.info.Member.InstallID)
	}
	if !uefi && m.diskErr != nil {
		return m.diskErr
	}
	return nil
}

// host is the discovered set plus its mounted members.
type host struct {
	uefi      bool
	arch      string
	root      string
	set       bootenv.HostSet
	infos     []bootenv.ESPInfo
	members   []*member // set order: best (highest generation) first, then added ESPs
	installID string    // "" until generated for a set without markers
	// explicitList: given with --esp-uuids/ALPINE_ZFSBOOT_ESP_UUIDS -
	// persisted by install/update into config (and BIOS CMDLINE).
	explicitList []string
	cleanups     []func()
}

func (h *host) cleanup() {
	for i := len(h.cleanups) - 1; i >= 0; i-- {
		h.cleanups[i]()
	}
	h.cleanups = nil
}

// mirrored: more than a plain single ESP - output gets per-ESP headers.
func (h *host) mirrored() bool {
	return len(h.members) > 1 || len(h.set.Missing) > 0 || h.set.Mode == bootenv.ModeList
}

func (h *host) exitCode(failed bool) int {
	switch {
	case failed:
		return exitFailure
	case len(h.set.Missing) > 0:
		return exitMemberMissing
	}
	return 0
}

// expected is the full set for MEMBERS=: the set's expected UUIDs plus
// any ESP being added, in a stable order.
func (h *host) expected() []string {
	seen := map[string]bool{}
	var out []string
	for _, u := range h.set.Expected {
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	for _, m := range h.members {
		if !seen[m.info.UUID] {
			seen[m.info.UUID] = true
			out = append(out, m.info.UUID)
		}
	}
	if h.set.Mode != bootenv.ModeList {
		sort.Strings(out)
	}
	return out
}

func firmwareOf(root, firmware string) (bool, error) {
	switch firmware {
	case "uefi":
		return true, nil
	case "bios":
		return false, nil
	case "":
		return isUEFI(root), nil
	}
	return false, fmt.Errorf("--firmware must be \"uefi\" or \"bios\" (got %q)", firmware)
}

// resolveList: --esp-uuids, then ALPINE_ZFSBOOT_ESP_UUIDS, then (live
// system or rescue initramfs only, allowCmdline) /proc/cmdline. explicit:
// given by the operator for this run (persisted by install/update).
func resolveList(o hostOpts, root string, allowCmdline bool) (list []string, source string, explicit bool, err error) {
	parse := func(v, src string) ([]string, string, bool, error) {
		l, err := espmember.ParseUUIDList(v)
		if err != nil {
			return nil, "", false, fmt.Errorf("%s: %w", src, err)
		}
		return l, src, src != "/proc/cmdline", nil
	}
	if o.espUUIDs != "" {
		return parse(o.espUUIDs, "--esp-uuids")
	}
	if v := os.Getenv(envESPUUIDs); v != "" {
		return parse(v, envESPUUIDs)
	}
	if allowCmdline && root == "/" {
		if v := cmdlineValue(readProcCmdline(), layout.ESPUUIDsKey); v != "" {
			return parse(v, "/proc/cmdline")
		}
	}
	return nil, "", false, nil
}

func canonicalPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	return p
}

// primaryUUID: the ESP mounted at <root>/boot/efi, else (live system or
// rescue initramfs) the BIOS-booted one named by alpine-zfsboot.esp-self=.
func primaryUUID(root string) string {
	if dev, fstype, ok := findMountAtPath(canonicalPath(filepath.Join(root, "boot/efi"))); ok && fstype == "vfat" {
		if u, err := volumeUUID(dev); err == nil {
			return u
		}
	}
	if root == "/" {
		if u, err := espmember.NormalizeFATUUID(cmdlineValue(readProcCmdline(), layout.ESPSelfKey)); err == nil {
			return u
		}
	}
	return ""
}

// attach mounts one ESP and builds its target.
func (h *host) attach(info bootenv.ESPInfo, foreign, added, readonly bool) *member {
	m := &member{info: info, foreign: foreign, added: added}
	t := &target{uefi: h.uefi, arch: h.arch, espDev: info.Dev, disk: bootenv.DevicePartitionBase(info.Dev)}
	if !h.uefi && (h.set.Mode != bootenv.ModeLegacy || added) {
		// A mirrored set: refuse whole-disk FATs and md/dm (see DiskOfESP).
		// A legacy single ESP keeps exactly its old disk derivation.
		if d, err := diskOfESP(info.Dev); err != nil {
			m.diskErr = err
		} else {
			t.disk = d
		}
	}
	if !h.uefi {
		if l, err := detectDiskLayout(t.disk); err == nil {
			t.layoutKind = l
		}
	}
	mp, cleanup, err := mountESP(info.Dev, readonly)
	if err != nil {
		m.mountErr = err
		return m
	}
	t.mountpoint, t.cleanup = mp, cleanup
	h.cleanups = append(h.cleanups, cleanup)
	m.target = t
	return m
}

// discoverHost finds and mounts this host's ESP set (status, verify,
// update, integrity, int13chunk, esp).
func discoverHost(root, firmware string, readonly bool, o hostOpts) (*host, error) {
	uefi, err := firmwareOf(root, firmware)
	if err != nil {
		return nil, err
	}
	list, src, explicit, err := resolveList(o, root, true)
	if err != nil {
		return nil, err
	}
	infos, err := scanESPs()
	if err != nil {
		return nil, err
	}
	hs, err := bootenv.SelectHost(infos, bootenv.SelectOpts{List: list, ListSource: src, PrimaryUUID: primaryUUID(root)})
	if err != nil {
		return nil, err
	}
	h := &host{uefi: uefi, arch: detectArch(), root: root, set: hs, infos: infos, installID: hs.InstallID}
	if explicit {
		h.explicitList = list
	}
	for _, hm := range hs.Members {
		h.members = append(h.members, h.attach(hm.ESPInfo, hm.Foreign, false, readonly))
	}
	return h, nil
}

// findInfo resolves a --add-esp/esp argument: a FAT UUID or a device path.
func (h *host) findInfo(arg string) (bootenv.ESPInfo, error) {
	if u, err := espmember.NormalizeFATUUID(arg); err == nil {
		var hits []bootenv.ESPInfo
		for _, i := range h.infos {
			if i.UUID == u {
				hits = append(hits, i)
			}
		}
		switch len(hits) {
		case 0:
			return bootenv.ESPInfo{}, fmt.Errorf("no FAT filesystem with UUID %s is present", u)
		case 1:
			return hits[0], nil
		}
		return bootenv.ESPInfo{}, fmt.Errorf("FAT UUID %s is on more than one device - refusing to guess", u)
	}
	want := canonicalPath(arg)
	for _, i := range h.infos {
		if i.Dev == arg || canonicalPath(i.Dev) == want {
			return i, nil
		}
	}
	return bootenv.ESPInfo{}, fmt.Errorf("%s holds no FAT filesystem (create the ESP first: mkfs.vfat -F32 -n EFI <partition>)", arg)
}

// addESPs attaches the --add-esp ESPs (read-write) as new members.
func (h *host) addESPs(o hostOpts) error {
	if o.addESP == "" {
		return nil
	}
	for _, arg := range strings.Split(o.addESP, ",") {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}
		info, err := h.findInfo(arg)
		if err != nil {
			return fmt.Errorf("--add-esp %s: %w", arg, err)
		}
		dup := false
		for _, m := range h.members {
			if m.info.UUID == info.UUID {
				dup = true
			}
		}
		if dup {
			fmt.Printf("--add-esp %s: ESP %s is already a member\n", arg, info.UUID)
			continue
		}
		foreign := info.ValidMember() && info.Member.InstallID != h.installID
		if foreign && !o.forceAdopt {
			return fmt.Errorf("--add-esp %s: ESP %s carries the MEMBER marker of another installation (install id %s) - refusing without --force-adopt", arg, info.UUID, info.Member.InstallID)
		}
		h.members = append(h.members, h.attach(info, false, true, false))
	}
	return nil
}

// checkDistinctDisks refuses (BIOS) two members on one disk: stage1/
// stage2 exist once per disk.
func (h *host) checkDistinctDisks() error {
	if h.uefi {
		return nil
	}
	seen := map[string]string{}
	for _, m := range h.members {
		if m.target == nil || m.diskErr != nil {
			continue
		}
		if other, ok := seen[m.disk]; ok {
			return fmt.Errorf("ESPs %s and %s are both on disk %s - a BIOS disk has one boot loader; refusing (put each ESP of a mirrored boot on its own disk)", other, m.info.UUID, m.disk)
		}
		seen[m.disk] = m.info.UUID
	}
	return nil
}

// bestSource is the member config/keys and seed settings are taken from:
// the best present member that is already installed (not one being added)
// and never one marked for another installation - a foreign ESP's keys
// must never reach this host's ESPs, whatever its generation.
func (h *host) bestSource() *member {
	for _, m := range h.members {
		if m.target != nil && !m.added && !m.foreign {
			return m
		}
	}
	return nil
}

func (h *host) ensureInstallID() (string, error) {
	if h.installID == "" {
		id, err := espmember.NewInstallID()
		if err != nil {
			return "", err
		}
		h.installID = id
	}
	return h.installID, nil
}

// maxGeneration is the highest generation among this installation's own
// members - a foreign ESP's number means nothing here.
func (h *host) maxGeneration() uint64 {
	var g uint64
	for _, m := range h.members {
		if !m.foreign && m.info.Generation() > g {
			g = m.info.Generation()
		}
	}
	return g
}

// poolOf reads the boot pool name an ESP's build carries.
func poolOf(t *target) string {
	if t == nil {
		return ""
	}
	if t.uefi {
		rel, err := uefiboot.LoaderPath(t.arch)
		if err != nil {
			return ""
		}
		info, err := cmdline.Read(filepath.Join(t.mountpoint, rel))
		if err != nil {
			return ""
		}
		return info.Pool
	}
	raw, err := os.ReadFile(filepath.Join(t.mountpoint, layout.CmdlineFile))
	if err != nil {
		return ""
	}
	return cmdline.ParseText(raw).Pool
}

// diskHint is MEMBER's informational DISK= value: the disk's
// /dev/disk/by-id name when there is one, else the device name.
func diskHint(m *member) string {
	if m.target == nil {
		return ""
	}
	disk := m.disk
	if ents, err := os.ReadDir("/dev/disk/by-id"); err == nil {
		real := canonicalPath(disk)
		for _, e := range ents {
			p := filepath.Join("/dev/disk/by-id", e.Name())
			if canonicalPath(p) == real && !strings.Contains(e.Name(), "-part") {
				return e.Name()
			}
		}
	}
	return filepath.Base(disk)
}

// finalizeMembers writes EFI/ALPINE/MEMBER - LAST - on every member in ok
// that does not already carry exactly this record. gen is the generation
// to record. A failure is that member's failure.
func (h *host) finalizeMembers(ok []*member, gen uint64, pool string) map[*member]error {
	errs := map[*member]error{}
	id, err := h.ensureInstallID()
	if err != nil {
		for _, m := range ok {
			errs[m] = err
		}
		return errs
	}
	exp := h.expected()
	for _, m := range ok {
		// POOL/DISK are informational: sanitized, so an odd by-id name or
		// pool name can never fail a member whose payload was written.
		want := espmember.Member{InstallID: id, Generation: gen, ESPUUID: m.info.UUID, Members: exp,
			Pool: espmember.SanitizeInfo(pool), Disk: espmember.SanitizeInfo(diskHint(m)), Written: nowUTC()}
		if cur, present, err := espmember.Read(m.mountpoint); present && err == nil &&
			cur.InstallID == want.InstallID && cur.Generation == want.Generation && cur.ESPUUID == want.ESPUUID &&
			strings.Join(cur.Members, ",") == strings.Join(want.Members, ",") {
			continue
		}
		if err := espmember.Write(m.mountpoint, want); err != nil {
			errs[m] = fmt.Errorf("writing %s: %w", layout.MemberFile, err)
			continue
		}
		fmt.Printf("%s: %s generation %d (install id %s)\n", m.label(), layout.MemberFile, gen, id)
	}
	return errs
}

// syncFiles are the per-host files every member carries identically.
var syncFiles = []struct {
	rel  string
	mode os.FileMode
}{
	{layout.ConfigFile, 0o644},
	{layout.AuthorizedKeysFile, 0o600},
	{layout.SSHHostEd25519KeyFile, 0o600},
}

// syncConfig makes config/authorized_keys/host key on dst identical to
// src (copy, or remove what src does not have). wrote: something changed.
func syncConfig(src, dst *member) (wrote bool, err error) {
	for _, f := range syncFiles {
		want, werr := os.ReadFile(filepath.Join(src.mountpoint, f.rel))
		got, gerr := os.ReadFile(filepath.Join(dst.mountpoint, f.rel))
		switch {
		case werr != nil && !os.IsNotExist(werr):
			return wrote, fmt.Errorf("reading %s on %s: %w", f.rel, src.label(), werr)
		case os.IsNotExist(werr):
			if gerr == nil {
				if err := os.Remove(filepath.Join(dst.mountpoint, f.rel)); err != nil {
					return wrote, err
				}
				fmt.Printf("%s: removed %s (not on %s)\n", dst.label(), f.rel, src.label())
				wrote = true
			}
		case gerr == nil && bytes.Equal(want, got):
		default:
			if err := espconfig.WriteFile(dst.mountpoint, f.rel, want, f.mode); err != nil {
				return wrote, err
			}
			fmt.Printf("%s: %s copied from %s\n", dst.label(), f.rel, src.label())
			wrote = true
		}
	}
	return wrote, nil
}

// setPoolInstallID records the set's install id in the boot pool
// (best-effort cross-check for init; never fails the run).
func setPoolInstallID(pool, id string) {
	if pool == "" || id == "" {
		return
	}
	cur, err := runZFS("get", "-H", "-o", "value", poolInstallIDProp, pool)
	if err != nil {
		fmt.Printf("pool %s: %s not checked (%v) - optional, only a boot-time cross-check\n", pool, poolInstallIDProp, firstLineOf(err.Error()+" "+cur))
		return
	}
	if cur == id {
		return
	}
	if out, err := runZFS("set", poolInstallIDProp+"="+id, pool); err != nil {
		fmt.Printf("pool %s: could not set %s (%v %s) - optional, only a boot-time cross-check\n", pool, poolInstallIDProp, err, firstLineOf(out))
		return
	}
	fmt.Printf("pool %s: %s=%s\n", pool, poolInstallIDProp, id)
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// cmdlineEditor returns the per-ESP CMDLINE adjustment for m: its own
// alpine-zfsboot.esp-self=, the explicit esp-uuids list (or the one m
// already has), and - for a new member - the int13chunk value of src.
func (h *host) cmdlineEditor(m, src *member, opts biosOpts) func([]byte) ([]byte, error) {
	return func(c []byte) ([]byte, error) {
		installed, _ := os.ReadFile(filepath.Join(m.mountpoint, layout.CmdlineFile))
		from := installed
		if len(installed) == 0 && src != nil && src != m {
			from, _ = os.ReadFile(filepath.Join(src.mountpoint, layout.CmdlineFile))
			if opts.int13chunk == "" {
				if v := cmdlineValue(string(from), layout.Int13ChunkKey); v != "" {
					c = setCmdlineWord(c, layout.Int13ChunkKey, v)
				}
			}
		}
		list := cmdlineValue(string(from), layout.ESPUUIDsKey)
		if h.explicitList != nil {
			list = strings.Join(h.explicitList, ",")
		}
		c = setCmdlineWord(c, layout.ESPUUIDsKey, list)
		c = setCmdlineWord(c, layout.ESPSelfKey, m.info.UUID)
		return c, checkCmdlineLength(c)
	}
}

// installed reports whether m already carries an alpine-zfsboot boot
// chain (BIOS: a recognizable stage2 on its disk; UEFI: the loader).
func installed(m *member) bool {
	if m.uefi {
		rel, err := uefiboot.LoaderPath(m.arch)
		if err != nil {
			return false
		}
		_, err = os.Stat(filepath.Join(m.mountpoint, rel))
		return err == nil
	}
	// Any stage2 stage1 would jump into (the magic word) - also one from
	// before stage2 carried a version string: updateBIOSTarget's own
	// checkUpdateEligible then decides, exactly as for a single ESP.
	s2, err := biosboot.ReadStage2(m.disk)
	return err == nil && len(s2) >= 4 && binary.LittleEndian.Uint16(s2[2:]) == biosboot.Stage2Magic
}

func printErr(err error) {
	fmt.Fprintln(os.Stderr, "alpine-zfsboot:", err)
}

// result is one ESP's outcome of a writing run.
type result struct {
	wrote bool
	err   error
}

// report prints the per-ESP summary of a writing run (mirrored boot only)
// and returns whether anything failed.
func (h *host) summarize(what string, res map[*member]*result) bool {
	failed := false
	for _, m := range h.members {
		if r := res[m]; r != nil && r.err != nil {
			failed = true
		}
	}
	if !h.mirrored() {
		for _, m := range h.members {
			if r := res[m]; r != nil && r.err != nil {
				printErr(r.err)
			}
		}
		return failed
	}
	fmt.Printf("\n%s summary (%d ESP(s)):\n", what, len(h.members))
	for _, m := range h.members {
		r := res[m]
		switch {
		case r == nil:
			fmt.Printf("  %-34s not touched\n", m.label())
		case r.err != nil:
			fmt.Printf("  %-34s FAILED: %v\n", m.label(), r.err)
		case r.wrote:
			fmt.Printf("  %-34s written\n", m.label())
		default:
			fmt.Printf("  %-34s already up to date\n", m.label())
		}
	}
	for _, u := range h.set.Missing {
		fmt.Printf("  %-34s MISSING - not present, not written (a dead or detached disk? it stays a member; 'alpine-zfsboot esp remove %s' drops it)\n", "ESP "+u, u)
	}
	if failed {
		fmt.Println("  an ESP that FAILED was either not written at all or rolled back to its previous boot payload - it still boots its previous build")
	}
	return failed
}

// --- forEachMember (integrity, int13chunk) ------------------------------

// forEachMember runs fn on every ESP of the set (read-write), going on
// after a failure, and returns the exit status.
func forEachMember(root, firmware string, o hostOpts, fn func(*target) error) int {
	h, err := discoverHost(root, firmware, false, o)
	if err != nil {
		printErr(err)
		return exitFailure
	}
	defer withCleanup(h.cleanup)()
	failed := false
	for _, m := range h.members {
		if h.mirrored() {
			fmt.Printf("== %s ==\n", m.label())
		}
		err := m.usable(true, o.forceAdopt, h.uefi)
		if err == nil {
			err = fn(m.target)
		}
		if err != nil {
			failed = true
			if h.mirrored() {
				printErr(fmt.Errorf("%s: %w", m.label(), err))
			} else {
				printErr(err)
			}
		}
	}
	h.warnMissing()
	return h.exitCode(failed)
}

func (h *host) warnMissing() {
	for _, u := range h.set.Missing {
		fmt.Fprintf(os.Stderr, "alpine-zfsboot: WARNING: ESP %s of this host's set is not present (a dead or detached disk?) - not checked, not written\n", u)
	}
}

// --- status -----------------------------------------------------------

func runStatus(root, firmware string, o hostOpts, verbose bool) int {
	h, err := discoverHost(root, firmware, true, o)
	if err != nil {
		printErr(err)
		return exitFailure
	}
	defer withCleanup(h.cleanup)()
	for _, m := range h.members {
		if h.mirrored() {
			fmt.Printf("== %s ==\n", m.label())
		}
		if m.target == nil {
			fmt.Printf("  %-20s ERROR (%v)\n", "ESP:", m.mountErr)
			continue
		}
		inspect(m.target).print(verbose)
	}
	h.printSet()
	h.printRescueNIC()
	h.warnMissing()
	return h.exitCode(false)
}

// printSet is status/verify's "ESP set" section.
func (h *host) printSet() {
	fmt.Println("ESP set")
	switch h.set.Mode {
	case bootenv.ModeList:
		fmt.Printf("  %-20s explicit list (%s= from %s)\n", "Members by:", layout.ESPUUIDsKey, h.set.ListSource)
	case bootenv.ModeMember:
		fmt.Printf("  %-20s %s identity markers\n", "Members by:", layout.MemberFile)
	default:
		fmt.Printf("  %-20s single ESP, no %s marker yet (written by the next install/update)\n", "Members by:", layout.MemberFile)
	}
	if h.installID != "" {
		fmt.Printf("  %-20s %s\n", "Install id:", h.installID)
	}
	maxGen := h.maxGeneration()
	for _, m := range h.members {
		state := ""
		switch {
		case m.mountErr != nil:
			state = fmt.Sprintf("ERROR (%v)", m.mountErr)
		case m.foreign:
			state = "FOREIGN marker (install id " + m.info.Member.InstallID + ") - written only with --force-adopt"
		case m.info.MemberErr != nil:
			state = fmt.Sprintf("marker unreadable (%v)", m.info.MemberErr)
		case !m.info.ValidMember():
			state = "no marker yet"
		case m.info.Generation() < maxGen:
			state = fmt.Sprintf("generation %d - STALE (newest is %d)", m.info.Generation(), maxGen)
		default:
			state = fmt.Sprintf("generation %d", m.info.Generation())
		}
		disk := ""
		if m.target != nil && !h.uefi {
			disk = " disk " + m.disk
		}
		if m.diskErr != nil && !h.uefi {
			disk = fmt.Sprintf(" disk ERROR (%v)", m.diskErr)
		}
		fmt.Printf("  %-20s %s (%s)%s: %s\n", "ESP:", m.info.UUID, m.info.Dev, disk, state)
	}
	for _, u := range h.set.Missing {
		fmt.Printf("  %-20s %s: MISSING - expected but not present (degraded)\n", "ESP:", u)
	}
	for _, ig := range h.set.Ignored {
		fmt.Printf("  %-20s %s (%s): %s\n", "Not used:", ig.UUID, ig.Dev, ig.Reason)
	}
	if best := h.bestSource(); best != nil && h.installID != "" {
		pool := poolOf(best.target)
		if pool != "" {
			v, err := runZFS("get", "-H", "-o", "value", poolInstallIDProp, pool)
			switch {
			case err != nil:
				fmt.Printf("  %-20s not checked (pool %s not imported, or no zfs command)\n", "Pool identity:", pool)
			case v == h.installID:
				fmt.Printf("  %-20s %s matches (%s)\n", "Pool identity:", pool, poolInstallIDProp)
			case v == "-" || v == "":
				fmt.Printf("  %-20s %s has no %s yet (set by the next install/update)\n", "Pool identity:", pool, poolInstallIDProp)
			default:
				fmt.Printf("  %-20s WARNING: pool %s says %s=%s, the ESPs say %s - these ESPs may belong to another installation\n", "Pool identity:", pool, poolInstallIDProp, v, h.installID)
			}
		}
	}
}

// rescueNIC returns the configured alpine-zfsboot.net.mac= and where it
// comes from: a BIOS CMDLINE wins over EFI/ALPINE/config (cmdline beats
// config at boot).
func rescueNIC(t *target) (mac, source string) {
	if t == nil {
		return "", ""
	}
	if !t.uefi {
		if raw, err := os.ReadFile(filepath.Join(t.mountpoint, layout.CmdlineFile)); err == nil {
			if v := cmdlineValue(string(raw), layout.NetMACKey); v != "" {
				return v, layout.CmdlineFile
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(t.mountpoint, layout.ConfigFile)); err == nil {
		if v, ok := espconfig.ConfigValue(raw, layout.NetMACKey); ok && v != "" {
			return v, layout.ConfigFile
		}
	}
	return "", ""
}

func (h *host) printRescueNIC() {
	best := h.bestSource()
	if best == nil {
		return
	}
	fmt.Println("Rescue network")
	mac, src := rescueNIC(best.target)
	if mac == "" {
		fmt.Printf("  %-20s eth0 (default - no %s set)\n", "Interface:", layout.NetMACKey)
		return
	}
	norm, err := netmac.Normalize(mac)
	if err != nil {
		fmt.Printf("  %-20s ERROR: %s=%s in %s: %v - rescue SSH will not start\n", "Interface:", layout.NetMACKey, mac, src, err)
		return
	}
	fmt.Printf("  %-20s card with MAC %s (%s in %s; never a fallback to eth0)\n", "Interface:", norm, layout.NetMACKey, src)
	if h.root != "/" {
		return
	}
	if name, err := netmac.Resolve(sysClassNet, norm); err == nil {
		fmt.Printf("  %-20s %s on this running system (the rescue environment finds it by MAC, whatever it is called there)\n", "Resolves to:", name)
	} else {
		fmt.Printf("  %-20s WARNING: %v - on this hardware rescue SSH would not start\n", "Resolves to:", err)
	}
}

// --- verify -----------------------------------------------------------

func runVerify(root, firmware string, o hostOpts, verbose, deep, repair bool, f verifyFiles) int {
	h, err := discoverHost(root, firmware, verifyMountReadonly(repair), o)
	if err != nil {
		printErr(err)
		return exitFailure
	}
	defer withCleanup(h.cleanup)()

	var problems []string
	for _, m := range h.members {
		prefix := ""
		if h.mirrored() {
			fmt.Printf("== %s ==\n", m.label())
			prefix = m.label() + ": "
		}
		if m.target == nil {
			fmt.Printf("  %-20s ERROR (%v)\n", "ESP:", m.mountErr)
			problems = append(problems, prefix+m.mountErr.Error())
			continue
		}
		r, errs := verifyTarget(m.target, deep, repair, f)
		r.print(verbose)
		for _, e := range errs {
			problems = append(problems, prefix+e.Error())
		}
	}
	cmpErrs, cmpWarns, table := compareMembers(h)
	problems = append(problems, cmpErrs...)
	if h.mirrored() || h.set.Mode != bootenv.ModeLegacy {
		h.printSet()
	}
	if len(table) > 0 {
		fmt.Println("ESP comparison (SHA-256, first 12 hex digits; newest first)")
		for _, line := range table {
			fmt.Println("  " + line)
		}
	}
	h.printRescueNIC()
	for _, w := range cmpWarns {
		fmt.Fprintf(os.Stderr, "alpine-zfsboot: WARNING: %s\n", w)
	}
	h.warnMissing()

	if len(problems) == 0 {
		if len(h.set.Missing) > 0 {
			fmt.Printf("\nverify: OK for every ESP present - %d ESP(s) of the set MISSING (exit %d)\n", len(h.set.Missing), exitMemberMissing)
		} else {
			fmt.Println("\nverify: OK")
		}
		return h.exitCode(false)
	}
	fmt.Printf("\nverify: %d problem(s) found:\n", len(problems))
	for _, p := range problems {
		fmt.Println("  -", p)
	}
	return exitFailure
}

// memberDigest is one member's comparable content.
type memberDigest struct {
	m          *member
	buildStamp string
	files      map[string]string // name -> sha256 hex, "absent", or "unreadable: ..."
}

func digestBytes(b []byte, err error) string {
	switch {
	case os.IsNotExist(err):
		return "absent"
	case err != nil:
		return "unreadable: " + err.Error()
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func digestMember(m *member) memberDigest {
	d := memberDigest{m: m, files: map[string]string{}}
	read := func(rel string) ([]byte, error) { return os.ReadFile(filepath.Join(m.mountpoint, rel)) }
	if m.uefi {
		if rel, err := uefiboot.LoaderPath(m.arch); err == nil {
			b, err := read(rel)
			d.files["UEFI loader "+rel] = digestBytes(b, err)
			if info, err := cmdline.Read(filepath.Join(m.mountpoint, rel)); err == nil {
				d.buildStamp = info.BuildStamp
			}
		}
	} else {
		s1, err := biosboot.ReadStage1(m.disk)
		d.files["stage1 (MBR of "+"disk)"] = digestBytes(s1, err)
		s2, err := biosboot.ReadStage2(m.disk)
		d.files["stage2"] = digestBytes(s2, err)
		for _, rel := range []string{layout.KernelFile, layout.InitrdFile, layout.PayloadSumFile, layout.BlkSumFile} {
			b, err := read(rel)
			d.files[rel] = digestBytes(b, err)
		}
		c, err := read(layout.CmdlineFile)
		if err == nil {
			d.buildStamp = cmdline.ParseText(c).BuildStamp
			c = stripMirrorWords(c)
		}
		d.files[layout.CmdlineFile+" (without esp-self/esp-uuids)"] = digestBytes(c, err)
	}
	for _, rel := range []string{layout.MetadataFile, layout.ConfigFile, layout.AuthorizedKeysFile, layout.SSHHostEd25519KeyFile} {
		b, err := read(rel)
		d.files[rel] = digestBytes(b, err)
	}
	return d
}

// compareMembers is verify's mirror consistency check: every present
// member against the reference (highest generation, then newest build).
// Content differences are failures naming the ESP and whether it is
// stale; a generation difference with identical content is a warning.
func compareMembers(h *host) (errs, warns, table []string) {
	var ds []memberDigest
	for _, m := range h.members {
		// A foreign (listed, other installation) ESP is reported by
		// printSet, never a reference or a comparison partner.
		if m.target != nil && !m.added && !m.foreign {
			ds = append(ds, digestMember(m))
		}
	}
	if len(ds) < 2 {
		return nil, nil, nil
	}
	sort.SliceStable(ds, func(a, b int) bool {
		ga, gb := ds[a].m.info.Generation(), ds[b].m.info.Generation()
		if ga != gb {
			return ga > gb
		}
		return ds[a].buildStamp > ds[b].buildStamp
	})
	// The per-ESP table verify prints: generation, build and the first
	// 12 hex digits of the main artifacts' SHA-256.
	short := func(v string) string {
		if len(v) == 64 {
			return v[:12]
		}
		return v
	}
	for _, d := range ds {
		var parts []string
		for _, name := range []string{"stage2", layout.KernelFile, layout.InitrdFile, layout.MetadataFile, layout.ConfigFile} {
			if v, ok := d.files[name]; ok {
				parts = append(parts, filepath.Base(name)+" "+short(v))
			}
		}
		for name, v := range d.files {
			if strings.HasPrefix(name, "UEFI loader ") {
				parts = append([]string{"loader " + short(v)}, parts...)
			}
		}
		table = append(table, fmt.Sprintf("%s generation %d build %s: %s", d.m.label(), d.m.info.Generation(), orNone(d.buildStamp), strings.Join(parts, ", ")))
	}
	ref := ds[0]
	refDesc := fmt.Sprintf("%s (generation %d, build %s)", ref.m.label(), ref.m.info.Generation(), orNone(ref.buildStamp))
	for _, d := range ds[1:] {
		var diff []string
		for name, sum := range ref.files {
			if d.files[name] != sum {
				diff = append(diff, name)
			}
		}
		sort.Strings(diff)
		gen := d.m.info.Generation()
		desc := fmt.Sprintf("%s (generation %d, build %s)", d.m.label(), gen, orNone(d.buildStamp))
		switch {
		case len(diff) > 0 && (gen < ref.m.info.Generation() || d.buildStamp < ref.buildStamp):
			errs = append(errs, fmt.Sprintf("%s is STALE - differs from %s in: %s ('alpine-zfsboot update' brings it in line)", desc, refDesc, strings.Join(diff, ", ")))
		case len(diff) > 0:
			errs = append(errs, fmt.Sprintf("%s DIFFERS from %s in: %s, although both carry the same generation and build - one of them was damaged or edited by hand ('alpine-zfsboot update' rewrites the members)", desc, refDesc, strings.Join(diff, ", ")))
		case gen != ref.m.info.Generation():
			warns = append(warns, fmt.Sprintf("%s has the same content as %s but a different generation", desc, refDesc))
		}
	}
	return errs, warns, table
}

// --- update -----------------------------------------------------------

func runUpdate(root, firmware string, o hostOpts, yes bool, efiSrc release.Source, biosSrc release.BIOSSources, opts biosOpts) int {
	h, err := discoverHost(root, firmware, false, o)
	if err != nil {
		printErr(err)
		return exitFailure
	}
	defer withCleanup(h.cleanup)()
	if err := h.addESPs(o); err != nil {
		printErr(err)
		return exitFailure
	}
	if err := h.checkDistinctDisks(); err != nil {
		printErr(err)
		return exitFailure
	}

	workdir, err := os.MkdirTemp("", "alpine-zfsboot-update-*")
	if err != nil {
		printErr(err)
		return exitFailure
	}
	defer os.RemoveAll(workdir)

	var uefiA uefiAsset
	var biosA biosAssets
	if h.uefi {
		uefiA, err = loadUEFIAsset(efiSrc, h.arch, workdir)
	} else {
		biosA, err = loadBIOSAssets(biosSrc, h.arch, workdir)
	}
	if err != nil {
		printErr(err)
		return exitFailure
	}

	memberYes := yes
	if h.mirrored() && !yes {
		var names []string
		for _, m := range h.members {
			names = append(names, m.label())
		}
		if !confirm(fmt.Sprintf("update every ESP of this host that is not up to date - %s?", strings.Join(names, ", "))) {
			fmt.Println("not updated")
			return 0
		}
		memberYes = true // one question for the whole set
	}

	src := h.bestSource()
	res := map[*member]*result{}
	anyWrite := false
	for _, m := range h.members {
		r := &result{}
		res[m] = r
		if h.mirrored() {
			fmt.Printf("== %s ==\n", m.label())
		}
		if r.err = m.usable(true, o.forceAdopt, h.uefi); r.err != nil {
			continue
		}
		// A new member - added now, or a set member never installed - is
		// written without the "is it ours / is it newer" checks: the
		// operator named it. A legacy single ESP never is (as before).
		seed := m.added || (h.set.Mode != bootenv.ModeLegacy && !installed(m))
		if h.uefi {
			r.wrote, r.err = updateUEFITarget(m.target, uefiA, memberYes, seed)
		} else {
			mo := memberOpts{seed: seed, editCmdline: h.cmdlineEditor(m, src, opts)}
			if seed && src != nil && src != m {
				mo.integrityFrom = src.mountpoint
			}
			r.wrote, r.err = updateBIOSTarget(m.target, biosA, memberYes, opts, mo)
		}
		if r.wrote {
			anyWrite = true
		}
	}

	// Config, keys and host key: identical on every member, from the
	// best one (the explicit list, when given, recorded there first).
	if src != nil && src.mountErr == nil {
		if h.explicitList != nil {
			raw, _ := os.ReadFile(filepath.Join(src.mountpoint, layout.ConfigFile))
			if v, _ := espconfig.ConfigValue(raw, layout.ESPUUIDsKey); v != strings.Join(h.explicitList, ",") {
				if err := espconfig.SetConfigKey(src.mountpoint, layout.ESPUUIDsKey, strings.Join(h.explicitList, ",")); err != nil {
					res[src].err = err
				} else {
					fmt.Printf("%s: %s=%s recorded in %s\n", src.label(), layout.ESPUUIDsKey, strings.Join(h.explicitList, ","), layout.ConfigFile)
					res[src].wrote, anyWrite = true, true
				}
			}
		}
		for _, m := range h.members {
			if m == src || res[m].err != nil {
				continue
			}
			w, err := syncConfig(src, m)
			if err != nil {
				res[m].err = fmt.Errorf("copying config/keys from %s: %w", src.label(), err)
				continue
			}
			if w {
				res[m].wrote, anyWrite = true, true
			}
		}
	}

	// MEMBER last, on every member that ended this run consistent.
	gen := h.maxGeneration()
	if anyWrite || gen == 0 {
		gen++
	}
	var ok []*member
	for _, m := range h.members {
		if res[m].err == nil {
			ok = append(ok, m)
		}
	}
	pool := poolOf(srcTarget(src))
	for m, err := range h.finalizeMembers(ok, gen, pool) {
		res[m].err = err
	}
	failed := h.summarize("update", res)
	if !failed {
		setPoolInstallID(pool, h.installID)
	}
	h.warnMissing()
	return h.exitCode(failed)
}

func srcTarget(m *member) *target {
	if m == nil {
		return nil
	}
	return m.target
}

// --- install ----------------------------------------------------------

// installRun is install's parsed command line.
type installRun struct {
	disk, arch string
	uefi       bool
	mountpoint string
	yes        bool
	efi        release.Source
	bios       release.BIOSSources
	cfg        espconfig.Config
	sshKey     string
	opts       biosOpts
	ho         hostOpts
}

func runInstall(ir installRun) int {
	// Never /proc/cmdline here: install runs from an installer/ISO
	// environment whose cmdline says nothing about the target.
	list, src, _, err := resolveList(ir.ho, "", false)
	if err != nil {
		printErr(err)
		return exitFailure
	}
	dev, _, mounted := findMountAtPath(canonicalPath(ir.mountpoint))
	if !mounted {
		printErr(fmt.Errorf("%s is not mounted", ir.mountpoint))
		return exitFailure
	}
	rootUUID, err := volumeUUID(dev)
	if err != nil {
		printErr(err)
		return exitFailure
	}
	rootInfo := bootenv.ProbeMounted(bootenv.FATVolume{Dev: dev, UUID: rootUUID}, ir.mountpoint)
	h := &host{uefi: ir.uefi, arch: ir.arch, set: bootenv.HostSet{Mode: bootenv.ModeLegacy, Expected: []string{rootUUID}}}
	defer withCleanup(h.cleanup)()
	rootM := &member{info: rootInfo, target: &target{uefi: ir.uefi, arch: ir.arch, espDev: dev, disk: ir.disk, mountpoint: ir.mountpoint, cleanup: func() {}}}
	if rootInfo.ValidMember() {
		h.installID = rootInfo.Member.InstallID
	}
	h.members = []*member{rootM}

	if len(list) > 0 || ir.ho.addESP != "" || rootInfo.ValidMember() {
		if h.infos, err = scanESPs(); err != nil {
			printErr(err)
			return exitFailure
		}
		h.set.Mode = bootenv.ModeMember
		if !ir.uefi {
			// A mirrored set: the same DiskOfESP refusals as everywhere.
			if _, err := diskOfESP(dev); err != nil {
				printErr(err)
				return exitFailure
			}
		}
		if len(list) > 0 {
			hs, err := bootenv.SelectHost(h.infos, bootenv.SelectOpts{List: list, ListSource: src, PrimaryUUID: rootUUID})
			if err != nil {
				printErr(err)
				return exitFailure
			}
			if len(hs.Missing) > 0 {
				printErr(fmt.Errorf("ESP(s) %s listed in %s are not present - refusing to install a partial mirror (attach the disk, or drop it from the list)", strings.Join(hs.Missing, ", "), src))
				return exitFailure
			}
			inList := false
			for _, u := range list {
				inList = inList || u == rootUUID
			}
			if !inList {
				printErr(fmt.Errorf("the ESP mounted at %s (UUID %s) is not in %s (%s)", ir.mountpoint, rootUUID, src, strings.Join(list, ",")))
				return exitFailure
			}
			h.set.Mode, h.set.ListSource, h.set.Expected = bootenv.ModeList, src, list
			h.explicitList = list
			if h.installID == "" {
				h.installID = hs.InstallID
			}
			for _, hm := range hs.Members {
				if hm.UUID == rootUUID {
					continue
				}
				foreign := hm.ValidMember() && h.installID != "" && hm.Member.InstallID != h.installID
				if foreign && !ir.ho.forceAdopt {
					printErr(fmt.Errorf("ESP %s (%s) carries the MEMBER marker of another installation (install id %s) - refusing without --force-adopt", hm.UUID, hm.Dev, hm.Member.InstallID))
					return exitFailure
				}
				h.members = append(h.members, h.attach(hm.ESPInfo, false, false, false))
			}
		} else if rootInfo.ValidMember() {
			// Re-install of a marked set: its other present members too.
			for _, i := range h.infos {
				if i.UUID != rootUUID && i.ValidMember() && i.Member.InstallID == h.installID {
					h.members = append(h.members, h.attach(i, false, false, false))
				}
			}
			h.set.Expected = append(h.set.Expected, rootInfo.Member.Members...)
		}
		if err := h.addESPs(ir.ho); err != nil {
			printErr(err)
			return exitFailure
		}
	}
	for _, m := range h.members {
		if m.mountErr != nil {
			printErr(fmt.Errorf("%s: %w", m.label(), m.mountErr))
			return exitFailure
		}
		if !ir.uefi && m.diskErr != nil {
			printErr(fmt.Errorf("%s: %w", m.label(), m.diskErr))
			return exitFailure
		}
	}
	if err := h.checkDistinctDisks(); err != nil {
		printErr(err)
		return exitFailure
	}
	if h.explicitList != nil {
		ir.cfg.ESPUUIDs = strings.Join(h.explicitList, ",")
	}

	workdir, err := os.MkdirTemp("", "alpine-zfsboot-install-*")
	if err != nil {
		printErr(err)
		return exitFailure
	}
	defer os.RemoveAll(workdir)

	var hostKey []byte
	if ir.sshKey != "" {
		// One host key for every ESP: the rescue SSH identity must not
		// depend on which disk booted.
		if hostKey, err = espconfig.NewHostKey(); err != nil {
			printErr(err)
			return exitFailure
		}
	}

	var names []string
	for _, m := range h.members {
		names = append(names, m.label())
	}
	res := map[*member]*result{}
	if ir.uefi {
		a, err := loadUEFIAsset(ir.efi, ir.arch, workdir)
		if err != nil {
			printErr(err)
			return exitFailure
		}
		if !ir.yes && !confirm(fmt.Sprintf("install this UEFI build onto %s?", strings.Join(names, ", "))) {
			fmt.Println("not installed")
			return 0
		}
		for _, m := range h.members {
			res[m] = &result{wrote: true, err: writeUEFIInstall(ir.arch, m.mountpoint, a, ir.cfg, ir.sshKey, hostKey)}
		}
	} else {
		a, err := loadBIOSAssets(ir.bios, ir.arch, workdir)
		if err != nil {
			printErr(err)
			return exitFailure
		}
		// Every disk checked before the first byte is written to any.
		for _, m := range h.members {
			if err := biosboot.CheckSectorSize(m.disk); err != nil {
				printErr(fmt.Errorf("refusing to install: %w", err))
				return exitFailure
			}
			if err := bootenv.CheckStage2ExtentFree(m.disk); err != nil {
				printErr(fmt.Errorf("refusing to install: %w", err))
				return exitFailure
			}
		}
		info, cmdlineTxt, err := prepareBIOSInstall(ir.arch, a, ir.opts)
		if err != nil {
			printErr(err)
			return exitFailure
		}
		var disks []string
		for _, m := range h.members {
			disks = append(disks, m.disk)
		}
		if !ir.yes && !confirm(fmt.Sprintf("install stage1/stage2 onto %s and the FAT payload onto %s?", strings.Join(disks, ", "), strings.Join(names, ", "))) {
			fmt.Println("not installed")
			return 0
		}
		for _, m := range h.members {
			r := &result{wrote: true}
			res[m] = r
			c := setCmdlineWord(cmdlineTxt, layout.ESPSelfKey, m.info.UUID)
			if h.explicitList != nil {
				c = setCmdlineWord(c, layout.ESPUUIDsKey, strings.Join(h.explicitList, ","))
			}
			if r.err = checkCmdlineLength(c); r.err == nil {
				r.err = writeBIOSInstall(m.disk, ir.arch, m.mountpoint, a, info, c, ir.opts, ir.cfg, ir.sshKey, hostKey)
			}
		}
	}

	var ok []*member
	for _, m := range h.members {
		if res[m].err == nil {
			ok = append(ok, m)
		}
	}
	pool := poolOf(rootM.target)
	for m, err := range h.finalizeMembers(ok, h.maxGeneration()+1, pool) {
		res[m].err = err
	}
	failed := h.summarize("install", res)
	if !failed {
		setPoolInstallID(pool, h.installID)
	}
	return h.exitCode(failed)
}
