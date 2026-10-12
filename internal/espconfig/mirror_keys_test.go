package espconfig

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/unidoc/alpine-zfsboot/internal/layout"
)

// An existing config without the new keys must render byte-for-byte as
// before (no new lines, no reordering).
func TestRender_NewKeysOnlyWhenSetAndLast(t *testing.T) {
	old := Config{Net: "static", SSHPort: "2222", Console: "ttyS0"}
	want := "alpine-zfsboot.net=static\nalpine-zfsboot.ssh.port=2222\nalpine-zfsboot.console=ttyS0\n"
	if got := string(old.render()); got != want {
		t.Fatalf("render changed for an old config:\n%q\nwant\n%q", got, want)
	}
	c := old
	c.ESPUUIDs = "6AC5-0B94,6AC5-2367"
	c.NetMAC = "52:54:00:12:34:56"
	want2 := want + "alpine-zfsboot.esp-uuids=6AC5-0B94,6AC5-2367\nalpine-zfsboot.net.mac=52:54:00:12:34:56\n"
	if got := string(c.render()); got != want2 {
		t.Fatalf("got %q want %q", got, want2)
	}
}

func TestConfigValue(t *testing.T) {
	c := []byte("alpine-zfsboot.net.mac=aa:bb:cc:dd:ee:00\r\n# comment\nalpine-zfsboot.net.mac=52:54:00:00:00:02\r\nalpine-zfsboot.net=dhcp\n")
	if v, ok := ConfigValue(c, "alpine-zfsboot.net.mac"); !ok || v != "52:54:00:00:00:02" {
		t.Fatalf("got %q %v (last line must win, CR stripped)", v, ok)
	}
	if _, ok := ConfigValue(c, "alpine-zfsboot.esp-uuids"); ok {
		t.Fatal("absent key found")
	}
	// alpine-zfsboot.net= must not match alpine-zfsboot.net.mac= or vice versa
	if v, _ := ConfigValue(c, "alpine-zfsboot.net"); v != "dhcp" {
		t.Fatalf("prefix confusion: %q", v)
	}
}

func TestSetConfigKey(t *testing.T) {
	mp := t.TempDir()
	if err := SetConfigKey(mp, "alpine-zfsboot.esp-uuids", "AAAA-0001"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(mp, layout.ConfigFile), []byte("alpine-zfsboot.net=dhcp\nalpine-zfsboot.esp-uuids=OLD\n# keep me\nalpine-zfsboot.ssh.port=22"), 0o644)
	if err := SetConfigKey(mp, "alpine-zfsboot.esp-uuids", "AAAA-0001,BBBB-0002"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(mp, layout.ConfigFile))
	want := "alpine-zfsboot.net=dhcp\n# keep me\nalpine-zfsboot.ssh.port=22\nalpine-zfsboot.esp-uuids=AAAA-0001,BBBB-0002\n"
	if string(got) != want {
		t.Fatalf("got %q want %q", got, want)
	}
	SetConfigKey(mp, "alpine-zfsboot.esp-uuids", "")
	got, _ = os.ReadFile(filepath.Join(mp, layout.ConfigFile))
	if bytes.Contains(got, []byte("esp-uuids")) {
		t.Fatalf("empty value should remove the key: %q", got)
	}
}

func TestValidateNetMAC(t *testing.T) {
	if v, err := ValidateNetMAC("52-54-00-AB-CD-EF"); err != nil || v != "52:54:00:ab:cd:ef" {
		t.Fatalf("%q %v", v, err)
	}
	if v, err := ValidateNetMAC(""); err != nil || v != "" {
		t.Fatal("empty must stay unset")
	}
	if _, err := ValidateNetMAC("52:54:00:ab:cd"); err == nil {
		t.Fatal("malformed accepted")
	}
}

func TestNewHostKeySameBytesEverywhere(t *testing.T) {
	k, err := NewHostKey()
	if err != nil || len(k) != 83 {
		t.Fatalf("len %d %v", len(k), err)
	}
	a, b := t.TempDir(), t.TempDir()
	WriteHostKey(a, k)
	WriteHostKey(b, k)
	ka, _ := os.ReadFile(filepath.Join(a, layout.SSHHostEd25519KeyFile))
	kb, _ := os.ReadFile(filepath.Join(b, layout.SSHHostEd25519KeyFile))
	if !bytes.Equal(ka, kb) || !bytes.Equal(ka, k) {
		t.Fatal("host key differs between ESPs")
	}
}
