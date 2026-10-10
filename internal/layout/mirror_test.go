package layout

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// CmdlineMaxBytes must follow bios/stage2_main.c's CMDLINE_BUF_SIZE - 1:
// the tool refuses a longer CMDLINE because stage2 refuses to boot one.
func TestCmdlineMaxBytesMatchesStage2(t *testing.T) {
	src, err := os.ReadFile("../../bios/stage2_main.c")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^#define\s+CMDLINE_BUF_SIZE\s+(\d+)`).FindSubmatch(src)
	if m == nil || string(m[1]) != fmt.Sprint(CmdlineMaxBytes+1) {
		t.Fatalf("bios/stage2_main.c CMDLINE_BUF_SIZE = %q, layout.CmdlineMaxBytes+1 = %d", m, CmdlineMaxBytes+1)
	}
}

// MemberFile is an 8.3 name (any FAT reader finds it), in ESPDir.
func TestMemberFileName(t *testing.T) {
	name, ok := strings.CutPrefix(MemberFile, ESPDir+"/")
	if !ok || !regexp.MustCompile(`^[A-Z0-9]{1,8}$`).MatchString(name) {
		t.Fatalf("MemberFile %q is not an 8.3 upper-case name in %s", MemberFile, ESPDir)
	}
}

// The shell side uses the same key names.
func TestMirrorKeysUsedByInit(t *testing.T) {
	for file, keys := range map[string][]string{
		"../../init/init":          {ESPUUIDsKey + "=", ESPSelfKey + "=", NetMACKey + "="},
		"../../init/esp-select.sh": {ESPUUIDsKey + "=", MemberFile},
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range keys {
			if !strings.Contains(string(src), k) {
				t.Errorf("%s does not use %s", file, k)
			}
		}
	}
}
