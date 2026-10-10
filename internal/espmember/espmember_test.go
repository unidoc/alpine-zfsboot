package espmember

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unidoc/alpine-zfsboot/internal/layout"
	"github.com/unidoc/alpine-zfsboot/internal/payloadsum"
)

var payloadsumHeader = payloadsum.Header

const testID = "0f6c1e9a-4b1d-4c7e-9a55-1d2e3f405162"

func TestEncodeDecodeRoundTrip(t *testing.T) {
	m := Member{InstallID: testID, Generation: 7, ESPUUID: "6AC5-0B94", Members: []string{"6AC5-0B94", "6AC5-2367"}, Pool: "zroot", Disk: "ata-X_1234", Written: "2026-10-09T12:00:00Z"}
	got, err := Decode(Encode(m))
	if err != nil {
		t.Fatal(err)
	}
	if got.InstallID != m.InstallID || got.Generation != 7 || got.ESPUUID != m.ESPUUID || strings.Join(got.Members, ",") != "6AC5-0B94,6AC5-2367" || got.Pool != "zroot" || got.Disk != m.Disk {
		t.Fatalf("round trip: %+v", got)
	}
	if !strings.HasPrefix(string(Encode(m)), Header+"\n") {
		t.Fatal("missing header")
	}
}

func TestDecode_CRLFAndUnknownKeys(t *testing.T) {
	raw := Header + "\r\nINSTALL_ID=" + testID + "\r\nGENERATION=3\r\nESP_UUID=1234-ABCD\r\nMEMBERS=1234-ABCD\r\nFUTURE_KEY=whatever\r\n"
	m, err := Decode([]byte(raw))
	if err != nil || m.Generation != 3 || m.ESPUUID != "1234-ABCD" {
		t.Fatalf("got %+v, %v", m, err)
	}
}

func TestDecode_Rejects(t *testing.T) {
	good := func(repl ...string) string {
		s := Header + "\nINSTALL_ID=" + testID + "\nGENERATION=3\nESP_UUID=1234-ABCD\nMEMBERS=1234-ABCD\n"
		for i := 0; i+1 < len(repl); i += 2 {
			s = strings.Replace(s, repl[i], repl[i+1], 1)
		}
		return s
	}
	for name, raw := range map[string]string{
		"newer format":     good(Header, "alpine-zfsboot esp-member 2"),
		"no header":        good(Header+"\n", ""),
		"bad install id":   good(testID, "not-a-uuid"),
		"upper install id": good(testID, strings.ToUpper(testID)),
		"negative gen":     good("GENERATION=3", "GENERATION=-3"),
		"hex gen":          good("GENERATION=3", "GENERATION=0x3"),
		"huge gen":         good("GENERATION=3", "GENERATION=99999999999999999999"),
		"bad esp uuid":     good("ESP_UUID=1234-ABCD", "ESP_UUID=1234ABCD"),
		"lower esp uuid":   good("ESP_UUID=1234-ABCD", "ESP_UUID=1234-abcd"),
		"dup members":      good("MEMBERS=1234-ABCD", "MEMBERS=1234-ABCD,1234-ABCD"),
		"missing gen":      good("GENERATION=3\n", ""),
		"missing id":       good("INSTALL_ID="+testID+"\n", ""),
		"not kv":           good("MEMBERS=1234-ABCD", "garbage line"),
		"empty":            "",
	} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseUUIDList(t *testing.T) {
	l, err := ParseUUIDList("6ac5-0b94, 6AC5-2367")
	if err != nil || strings.Join(l, ",") != "6AC5-0B94,6AC5-2367" {
		t.Fatalf("%v %v", l, err)
	}
	for _, bad := range []string{"6AC5-0B94,", "6AC5-0B94,,6AC5-2367", "6AC50B94", "6AC5-0B94,6ac5-0b94", "6AC5-0B9G"} {
		if _, err := ParseUUIDList(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if l, err := ParseUUIDList(""); err != nil || l != nil {
		t.Fatal("empty list should be nil, nil")
	}
}

func TestNewInstallID(t *testing.T) {
	a, _ := NewInstallID()
	b, _ := NewInstallID()
	if !installIDRE.MatchString(a) || a == b || a[14] != '4' {
		t.Fatalf("install ids %q %q", a, b)
	}
}

func TestReadWriteRemove(t *testing.T) {
	mp := t.TempDir()
	if _, present, err := Read(mp); present || err != nil {
		t.Fatal("empty ESP should read as absent")
	}
	m := Member{InstallID: testID, Generation: 1, ESPUUID: "1234-ABCD", Members: []string{"1234-ABCD"}}
	if err := Write(mp, m); err != nil {
		t.Fatal(err)
	}
	got, present, err := Read(mp)
	if !present || err != nil || got.Generation != 1 {
		t.Fatalf("%+v %v %v", got, present, err)
	}
	// A write that would not decode is refused before touching the file.
	if err := Write(mp, Member{InstallID: "x", Generation: 2, ESPUUID: "1234-ABCD"}); err == nil {
		t.Fatal("invalid member written")
	}
	if got, _, _ := Read(mp); got.Generation != 1 {
		t.Fatal("refused write changed the file")
	}
	os.WriteFile(filepath.Join(mp, layout.MemberFile), []byte("junk"), 0o644)
	if _, present, err := Read(mp); !present || err == nil {
		t.Fatal("junk MEMBER should be present-but-broken")
	}
	if err := Remove(mp); err != nil {
		t.Fatal(err)
	}
	if _, present, _ := Read(mp); present {
		t.Fatal("not removed")
	}
	if err := Remove(mp); err != nil {
		t.Fatal("removing twice should be fine")
	}
}

// init/esp-select.sh parses MEMBER in shell: same header, and it only
// verifies CHECKSUM files of the format internal/payloadsum writes.
func TestShellReaderUsesTheSameHeaders(t *testing.T) {
	src, err := os.ReadFile("../../init/esp-select.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`ESP_MEMBER_HEADER="` + Header + `"`, `ESP_SUM_HEADER="` + payloadsumHeader + `"`} {
		if !strings.Contains(string(src), want) {
			t.Errorf("init/esp-select.sh lacks %s", want)
		}
	}
}

func TestSanitizeInfo(t *testing.T) {
	long := strings.Repeat("nvme-Vendor_Model#1;$", 12)
	s := SanitizeInfo(long)
	if len(s) != 128 || strings.ContainsAny(s, "#;$") {
		t.Fatalf("%q", s)
	}
	m := Member{InstallID: testID, Generation: 1, ESPUUID: "1234-ABCD", Disk: SanitizeInfo(long), Pool: SanitizeInfo("we\nird pool")}
	if err := Write(t.TempDir(), m); err != nil {
		t.Fatalf("a sanitized member must always write: %v", err)
	}
}
