// Package espmember reads and writes EFI/ALPINE/MEMBER, the identity
// marker of a mirrored-boot ESP (README "Mirrored boot: how members are
// recognised").
//
// A host with several ESPs (one per disk of a ZFS mirror) must never
// take its rescue SSH keys, host key or network settings from a FAT
// partition that merely LOOKS like an alpine-zfsboot ESP - a disk moved
// in from another machine, an old rescue stick, a previous deployment.
// MEMBER says which installation an ESP belongs to:
//
//	alpine-zfsboot esp-member 1
//	INSTALL_ID=<random UUID, generated once per host at its first install>
//	GENERATION=<decimal, raised on every install/update that wrote this ESP>
//	ESP_UUID=<this ESP's own FAT volume UUID, XXXX-XXXX>
//	MEMBERS=<the FAT UUIDs of every ESP of this host, comma-separated>
//	POOL=<boot pool name, informational>
//	DISK=<disk hint, informational>
//	WRITTEN=<UTC time of the write, informational>
//
// Identity is INSTALL_ID, not the pool GUID: the first install runs
// before anything guarantees the pool exists or is imported, and one id
// must cover the whole host from that first write on. The tool also
// stores it in the pool property org.alpinezfsboot:install-id
// (best-effort), which init cross-checks after importing the pool.
//
// ESP_UUID binds the marker to the partition it sits on: a marker copied
// onto another ESP (cp -a EFI/ ...) does not match that ESP's own UUID and
// does not make it a member. A dd clone keeps the UUID, and is caught as a
// duplicate UUID instead (refused - see init/esp-select.sh, bootenv).
//
// GENERATION is written LAST in every update of a member (after its
// payload and config were written and verified), so a crash never leaves
// a member claiming a newer generation than its files.
//
// This protects against accidents and stale disks, not against an
// attacker: anyone who can write the ESP can write a matching MEMBER.
//
// init/esp-select.sh parses the same format in shell; keep them in step
// (tests/run-tests.sh and this package's test share the fixtures' shape).
package espmember

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/unidoc/alpine-zfsboot/internal/espconfig"
	"github.com/unidoc/alpine-zfsboot/internal/layout"
)

// Header is MEMBER's first line for format 1. A different number is a
// format this code does not know: such a marker is "not a member" (never
// guessed at), so an older tool or init cannot misread a newer layout.
const (
	headerPrefix = "alpine-zfsboot esp-member "
	Format       = 1
	Header       = "alpine-zfsboot esp-member 1"
)

// Member is one decoded MEMBER file.
type Member struct {
	InstallID  string
	Generation uint64
	ESPUUID    string
	Members    []string
	Pool       string
	Disk       string
	Written    string
}

var (
	fatUUIDRE   = regexp.MustCompile(`^[0-9A-F]{4}-[0-9A-F]{4}$`)
	installIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	genRE       = regexp.MustCompile(`^[0-9]{1,18}$`)
	// infoRE: the informational fields are single-line, printable, no '='
	// games - written by the tool only, but read back into messages.
	infoRE = regexp.MustCompile(`^[A-Za-z0-9 ._:/@+,()-]{0,128}$`)
)

// NormalizeFATUUID upper-cases a FAT volume UUID (as blkid prints it:
// XXXX-XXXX) and validates it.
func NormalizeFATUUID(s string) (string, error) {
	u := strings.ToUpper(strings.TrimSpace(s))
	if !fatUUIDRE.MatchString(u) {
		return "", fmt.Errorf("%q is not a FAT volume UUID (want XXXX-XXXX, 8 hex digits, as blkid prints it)", s)
	}
	return u, nil
}

// ParseUUIDList parses a comma-separated FAT UUID list (the
// alpine-zfsboot.esp-uuids= value and MEMBERS=), normalizing each entry.
// Empty input is an empty list; an empty entry, a malformed one or a
// repeated one is an error.
func ParseUUIDList(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		u, err := NormalizeFATUUID(part)
		if err != nil {
			return nil, err
		}
		if seen[u] {
			return nil, fmt.Errorf("%s is listed twice", u)
		}
		seen[u] = true
		out = append(out, u)
	}
	return out, nil
}

// NewInstallID returns a random (version 4) UUID, lowercase.
func NewInstallID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating an install id: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// Encode renders m. It does not validate; Write does.
func Encode(m Member) []byte {
	var b strings.Builder
	b.WriteString(Header + "\n")
	fmt.Fprintf(&b, "INSTALL_ID=%s\n", m.InstallID)
	fmt.Fprintf(&b, "GENERATION=%d\n", m.Generation)
	fmt.Fprintf(&b, "ESP_UUID=%s\n", m.ESPUUID)
	fmt.Fprintf(&b, "MEMBERS=%s\n", strings.Join(m.Members, ","))
	if m.Pool != "" {
		fmt.Fprintf(&b, "POOL=%s\n", m.Pool)
	}
	if m.Disk != "" {
		fmt.Fprintf(&b, "DISK=%s\n", m.Disk)
	}
	if m.Written != "" {
		fmt.Fprintf(&b, "WRITTEN=%s\n", m.Written)
	}
	return []byte(b.String())
}

// Decode parses a MEMBER file. Any problem - unknown format, missing or
// malformed INSTALL_ID/GENERATION/ESP_UUID, a malformed MEMBERS list - is
// an error: a marker that does not decode cleanly is "not a member".
// Unknown keys are ignored (a later format-1 writer may add informational
// ones). CRLF line endings are accepted.
func Decode(raw []byte) (Member, error) {
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], headerPrefix) {
		return Member{}, fmt.Errorf("not an alpine-zfsboot MEMBER file (first line %q)", firstN(lines[0], 40))
	}
	if v := strings.TrimPrefix(lines[0], headerPrefix); v != strconv.Itoa(Format) {
		return Member{}, fmt.Errorf("MEMBER format %q is not one this version understands (%d) - written by a newer alpine-zfsboot?", firstN(v, 10), Format)
	}
	var m Member
	var haveID, haveGen, haveUUID bool
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return Member{}, fmt.Errorf("MEMBER line %q is not KEY=value", firstN(line, 40))
		}
		switch k {
		case "INSTALL_ID":
			if !installIDRE.MatchString(v) {
				return Member{}, fmt.Errorf("MEMBER INSTALL_ID %q is not a lowercase UUID", firstN(v, 40))
			}
			m.InstallID, haveID = v, true
		case "GENERATION":
			if !genRE.MatchString(v) {
				return Member{}, fmt.Errorf("MEMBER GENERATION %q is not a number", firstN(v, 20))
			}
			m.Generation, _ = strconv.ParseUint(v, 10, 64)
			haveGen = true
		case "ESP_UUID":
			u, err := NormalizeFATUUID(v)
			if err != nil || u != v {
				return Member{}, fmt.Errorf("MEMBER ESP_UUID %q is not an upper-case FAT UUID", firstN(v, 20))
			}
			m.ESPUUID, haveUUID = u, true
		case "MEMBERS":
			l, err := ParseUUIDList(v)
			if err != nil {
				return Member{}, fmt.Errorf("MEMBER MEMBERS: %w", err)
			}
			m.Members = l
		case "POOL", "DISK", "WRITTEN":
			if !infoRE.MatchString(v) {
				return Member{}, fmt.Errorf("MEMBER %s has unexpected characters", k)
			}
			switch k {
			case "POOL":
				m.Pool = v
			case "DISK":
				m.Disk = v
			default:
				m.Written = v
			}
		}
	}
	if !haveID || !haveGen || !haveUUID {
		return Member{}, fmt.Errorf("MEMBER is missing INSTALL_ID, GENERATION or ESP_UUID")
	}
	return m, nil
}

// SanitizeInfo makes an informational value (POOL=, DISK=) safe to write:
// characters outside the accepted set become '_', and it is cut to 128
// characters - an odd disk or pool name must never make a write fail.
func SanitizeInfo(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !infoRE.MatchString(string(c)) {
			b[i] = '_'
		}
	}
	if len(b) > 128 {
		b = b[:128]
	}
	return string(b)
}

// Validate checks a Member before it is written.
func Validate(m Member) error {
	enc := Encode(m)
	got, err := Decode(enc)
	if err != nil {
		return fmt.Errorf("refusing to write a MEMBER file that would not decode: %w", err)
	}
	if got.InstallID != m.InstallID || got.ESPUUID != m.ESPUUID || got.Generation != m.Generation {
		return fmt.Errorf("refusing to write a MEMBER file that does not round-trip")
	}
	return nil
}

// Read returns the MEMBER file under mountpoint. present=false, err=nil:
// no MEMBER file (a "legacy" ESP, written before 0.5.0, or not ours).
// present=true, err!=nil: a file exists but does not decode.
func Read(mountpoint string) (m Member, present bool, err error) {
	raw, err := os.ReadFile(filepath.Join(mountpoint, layout.MemberFile))
	if os.IsNotExist(err) {
		return Member{}, false, nil
	}
	if err != nil {
		return Member{}, true, fmt.Errorf("reading %s: %w", layout.MemberFile, err)
	}
	m, err = Decode(raw)
	return m, true, err
}

// Write validates m and writes it atomically (espconfig.WriteFile).
func Write(mountpoint string, m Member) error {
	if err := Validate(m); err != nil {
		return err
	}
	return espconfig.WriteFile(mountpoint, layout.MemberFile, Encode(m), 0o644)
}

// Remove deletes the MEMBER file under mountpoint (and nothing else). A
// missing file is not an error.
func Remove(mountpoint string) error {
	err := os.Remove(filepath.Join(mountpoint, layout.MemberFile))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if d, err := os.Open(filepath.Join(mountpoint, layout.ESPDir)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
