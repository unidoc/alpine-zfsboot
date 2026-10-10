package netmac

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"", "", true},
		{"aa:bb:cc:dd:ee:f0", "aa:bb:cc:dd:ee:f0", true},
		{"AA:BB:CC:DD:EE:F0", "aa:bb:cc:dd:ee:f0", true},
		{"52-54-00-12-34-56", "52:54:00:12:34:56", true},
		{"52:54:00:12:34", "", false},       // five groups
		{"52:54:00:12:34:56:78", "", false}, // seven groups
		{"52:54:00:12:34:5g", "", false},    // not hex
		{"52:54-00:12:34:56", "", false},    // mixed separators
		{"5254.0012.3456", "", false},       // Cisco dotted form not accepted
		{"525400123456", "", false},         // no separators
		{"00:00:00:00:00:00", "", false},    // all zero
		{"ff:ff:ff:ff:ff:ff", "", false},    // broadcast
		{"01:00:5e:00:00:01", "", false},    // multicast
		{" 52:54:00:12:34:56", "", false},   // stray whitespace
		{"52:54:00:12:34:56\r", "", false},  // CR
	} {
		got, err := Normalize(tc.in)
		if tc.ok && (err != nil || got != tc.want) {
			t.Errorf("Normalize(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
		if !tc.ok && err == nil {
			t.Errorf("Normalize(%q) = %q, want an error", tc.in, got)
		}
	}
}

// fakeSysNet builds a /sys/class/net look-alike: each interface is a
// symlink to ../../devices/<path>/net/<name>, like the real one, so the
// virtual-device test sees the same link shapes as on a real system.
func fakeSysNet(t *testing.T, ifaces map[string][2]string) string {
	t.Helper()
	root := t.TempDir()
	cls := filepath.Join(root, "class", "net")
	if err := os.MkdirAll(cls, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, v := range ifaces {
		devPath, mac := v[0], v[1]
		dir := filepath.Join(root, "devices", devPath, "net", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "address"), []byte(mac+"\n"), 0o644)
		typ := "1"
		if name == "lo" {
			typ = "772"
		}
		os.WriteFile(filepath.Join(dir, "type"), []byte(typ+"\n"), 0o644)
		rel, _ := filepath.Rel(cls, dir)
		if err := os.Symlink(rel, filepath.Join(cls, name)); err != nil {
			t.Fatal(err)
		}
	}
	return cls
}

func TestResolve(t *testing.T) {
	cls := fakeSysNet(t, map[string][2]string{
		"lo":    {"virtual", "00:00:00:00:00:00"},
		"eth0":  {"pci0000:00/0000:00:03.0", "52:54:00:00:00:01"},
		"eth1":  {"pci0000:00/0000:00:04.0", "52:54:00:00:00:02"},
		"br0":   {"virtual", "52:54:00:00:00:02"}, // a bridge copies its port's MAC
		"bond0": {"virtual", "52:54:00:00:00:01"},
	})
	if got, err := Resolve(cls, "52:54:00:00:00:02"); err != nil || got != "eth1" {
		t.Fatalf("Resolve eth1: %q, %v", got, err)
	}
	if got, err := Resolve(cls, "52-54-00-00-00-01"); err != nil || got != "eth0" {
		t.Fatalf("Resolve eth0 (upper/dash input): %q, %v", got, err)
	}
	_, err := Resolve(cls, "52:54:00:00:00:09")
	if err == nil || !strings.Contains(err.Error(), "eth0=52:54:00:00:00:01") || strings.Contains(err.Error(), "br0") {
		t.Fatalf("not-found error should list the physical cards only: %v", err)
	}
	if _, err := Resolve(cls, "zz"); err == nil {
		t.Fatal("malformed MAC accepted")
	}
}

func TestResolve_DuplicatePhysicalMACRefused(t *testing.T) {
	cls := fakeSysNet(t, map[string][2]string{
		"eth0": {"pci0000:00/0000:00:03.0", "52:54:00:00:00:01"},
		"eth1": {"pci0000:00/0000:00:04.0", "52:54:00:00:00:01"},
	})
	_, err := Resolve(cls, "52:54:00:00:00:01")
	if err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("want an ambiguity error, got %v", err)
	}
}

func TestScan_MissingDir(t *testing.T) {
	if _, err := Scan(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("want an error for a missing /sys/class/net")
	}
}
