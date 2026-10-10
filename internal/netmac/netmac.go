// Package netmac validates the optional alpine-zfsboot.net.mac= setting
// (the MAC address of the network card the rescue network uses instead
// of the default eth0) and resolves it to an interface name the same way
// init/net-config.sh's net_resolve_iface does at boot: scan
// /sys/class/net/*/address, case-insensitively, ignoring the loopback
// device and virtual interfaces (bridges, bonds, VLANs, veth, ... copy
// or invent a MAC and are never the card itself).
//
// The two implementations must agree; tests/run-tests.sh and this
// package's tests use the same fake /sys trees for that reason.
package netmac

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// macRE is the one accepted input shape: six two-digit hex groups, all
// separated by ':' or all by '-', any case.
var macRE = regexp.MustCompile(`^[0-9A-Fa-f]{2}([:-])[0-9A-Fa-f]{2}(?:[:-][0-9A-Fa-f]{2}){4}$`)

// Normalize returns mac in the canonical form init compares against
// (lowercase, colon-separated) or an error for anything that is not a
// usable unicast hardware address. The empty string means "not set" and
// is returned unchanged.
func Normalize(mac string) (string, error) {
	if mac == "" {
		return "", nil
	}
	if !macRE.MatchString(mac) {
		return "", fmt.Errorf("MAC address %q is malformed: want six two-digit hex groups, e.g. aa:bb:cc:dd:ee:ff", mac)
	}
	sep := mac[2]
	if strings.Count(mac, string(sep)) != 5 {
		return "", fmt.Errorf("MAC address %q mixes ':' and '-' separators", mac)
	}
	norm := strings.ToLower(strings.ReplaceAll(mac, "-", ":"))
	if norm == "00:00:00:00:00:00" {
		return "", fmt.Errorf("MAC address %q is all zeros - not a network card's address", mac)
	}
	var first int
	fmt.Sscanf(norm[:2], "%x", &first)
	if first&1 == 1 {
		return "", fmt.Errorf("MAC address %q is a multicast/broadcast address - a network card's own address is unicast (lowest bit of the first octet clear)", mac)
	}
	return norm, nil
}

// Interface is one entry of /sys/class/net.
type Interface struct {
	Name     string
	MAC      string // lowercase, as the kernel prints it
	Virtual  bool   // sysfs link target under /devices/virtual/
	Loopback bool
}

// Scan lists sysClassNet (normally /sys/class/net), sorted by name. An
// entry whose address cannot be read is skipped.
func Scan(sysClassNet string) ([]Interface, error) {
	entries, err := os.ReadDir(sysClassNet)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", sysClassNet, err)
	}
	var out []Interface
	for _, e := range entries {
		name := e.Name()
		p := filepath.Join(sysClassNet, name)
		raw, err := os.ReadFile(filepath.Join(p, "address"))
		if err != nil {
			continue
		}
		ifc := Interface{Name: name, MAC: strings.ToLower(strings.TrimSpace(string(raw)))}
		if target, err := os.Readlink(p); err == nil && strings.Contains(target, "/virtual/") {
			ifc.Virtual = true
		}
		if name == "lo" {
			ifc.Loopback = true
		}
		if t, err := os.ReadFile(filepath.Join(p, "type")); err == nil && strings.TrimSpace(string(t)) == "772" {
			ifc.Loopback = true
		}
		out = append(out, ifc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Resolve returns the name of the one physical interface whose address is
// mac. No match, or more than one physical card with that address, is an
// error (never a fallback to eth0 - see init/net-config.sh).
func Resolve(sysClassNet, mac string) (string, error) {
	norm, err := Normalize(mac)
	if err != nil {
		return "", err
	}
	if norm == "" {
		return "", fmt.Errorf("no MAC address given")
	}
	ifaces, err := Scan(sysClassNet)
	if err != nil {
		return "", err
	}
	var matches, seen []string
	for _, ifc := range ifaces {
		if ifc.Loopback || ifc.Virtual {
			continue
		}
		seen = append(seen, ifc.Name+"="+ifc.MAC)
		if ifc.MAC == norm {
			matches = append(matches, ifc.Name)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		if len(seen) == 0 {
			return "", fmt.Errorf("no network card has MAC %s (no physical network interfaces found)", norm)
		}
		return "", fmt.Errorf("no network card has MAC %s (found: %s)", norm, strings.Join(seen, " "))
	default:
		return "", fmt.Errorf("MAC %s is on more than one network card (%s) - refusing to guess", norm, strings.Join(matches, ", "))
	}
}
