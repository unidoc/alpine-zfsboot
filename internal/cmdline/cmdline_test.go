package cmdline

import "testing"

// Console used to be a 3-way "vga"/"serial"/"auto" classifier matching
// a build-time CONSOLE_NAME choice that no longer exists (see
// consoleFromCmdline()'s own comment in cmdline.go) - these tests now
// assert the real, verbatim console= value(s) instead.

func TestParseTty0Only(t *testing.T) {
	raw := []byte("console=tty0 root=ZFS=zroot/ROOT/alpine ro quiet kexec_load_disabled=0 alpine-zfsboot.pool=zroot alpine-zfsboot.timeout=10 alpine-zfsboot.version=0.1.0 alpine-zfsboot.buildstamp=20260910T003200Z\x00\x00\x00")
	info, err := parse("x86_64", raw, "test.EFI")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Arch != "x86_64" {
		t.Errorf("Arch = %q, want x86_64", info.Arch)
	}
	if info.Console != "tty0" {
		t.Errorf("Console = %q, want tty0", info.Console)
	}
	if info.Version != "0.1.0" {
		t.Errorf("Version = %q, want 0.1.0", info.Version)
	}
	if info.BuildStamp != "20260910T003200Z" {
		t.Errorf("BuildStamp = %q, want 20260910T003200Z", info.BuildStamp)
	}
	if info.Raw != "console=tty0 root=ZFS=zroot/ROOT/alpine ro quiet kexec_load_disabled=0 alpine-zfsboot.pool=zroot alpine-zfsboot.timeout=10 alpine-zfsboot.version=0.1.0 alpine-zfsboot.buildstamp=20260910T003200Z" {
		t.Errorf("Raw was not trimmed at the NUL terminator correctly: %q", info.Raw)
	}
}

func TestParseSerialOnly(t *testing.T) {
	// Not what build.sh's own default case statement produces anymore
	// (tty0-only, every arch - see its own CONSOLE_CMDLINE comment),
	// but parse() itself must still handle whatever a real .cmdline
	// section actually contains, verbatim - a hand-edited build.sh, or
	// an older .EFI built before today, could still carry this.
	raw := []byte("console=ttyS0,115200n8 root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.buildstamp=20260910T003200Z\x00")
	info, err := parse("aarch64", raw, "test.EFI")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Console != "ttyS0,115200n8" {
		t.Errorf("Console = %q, want ttyS0,115200n8", info.Console)
	}
}

func TestParseMultipleConsoles(t *testing.T) {
	// Same "an older/hand-edited build" reasoning as TestParseSerialOnly.
	raw := []byte("console=tty0 console=ttyS0,115200n8 root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.buildstamp=20260910T003200Z\x00")
	info, err := parse("x86_64", raw, "test.EFI")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Console != "tty0 ttyS0,115200n8" {
		t.Errorf("Console = %q, want %q", info.Console, "tty0 ttyS0,115200n8")
	}
}

func TestParseMissingVersion(t *testing.T) {
	raw := []byte("console=tty0 root=ZFS=zroot/ROOT/alpine ro\x00")
	_, err := parse("x86_64", raw, "test.EFI")
	if err == nil {
		t.Fatal("expected an error for a cmdline with no alpine-zfsboot.buildstamp=, got nil")
	}
}

func TestParseNoTrailingNUL(t *testing.T) {
	// objcopy always NUL-terminates cmdline.section (see build.sh),
	// but parse() shouldn't depend on that - a section with no NUL at
	// all should still parse the whole thing rather than erroring.
	raw := []byte("console=tty0 alpine-zfsboot.buildstamp=20260910T003200Z")
	info, err := parse("x86_64", raw, "test.EFI")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.BuildStamp != "20260910T003200Z" {
		t.Errorf("BuildStamp = %q, want 20260910T003200Z", info.BuildStamp)
	}
}

func TestArchFromMachine(t *testing.T) {
	if a, err := archFromMachine(0x8664); err != nil || a != "x86_64" {
		t.Errorf("amd64 machine type: got (%q, %v), want (x86_64, nil)", a, err)
	}
	if a, err := archFromMachine(0xaa64); err != nil || a != "aarch64" {
		t.Errorf("arm64 machine type: got (%q, %v), want (aarch64, nil)", a, err)
	}
	if _, err := archFromMachine(0x014c); err == nil {
		t.Error("i386 (unsupported) machine type: expected an error, got nil")
	}
}

func TestParseText(t *testing.T) {
	line := "root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.pool=zroot alpine-zfsboot.version=0.1.0 console=tty0\n"
	info := ParseText([]byte(line))
	if info.Pool != "zroot" {
		t.Errorf("Pool = %q, want zroot", info.Pool)
	}
	if info.Version != "0.1.0" {
		t.Errorf("Version = %q, want 0.1.0", info.Version)
	}
}

func TestParseText_EmptyInputIsNotAnError(t *testing.T) {
	info := ParseText(nil)
	if info.Pool != "" {
		t.Errorf("Pool = %q, want empty", info.Pool)
	}
}

func TestParseText_StopsAtFirstNewline(t *testing.T) {
	// A plain text file (EFI/ALPINE/CMDLINE) can have a trailing
	// newline, or even stray content on a second line if hand-edited -
	// only the first line is the real cmdline.
	info := ParseText([]byte("alpine-zfsboot.pool=zroot\nsomething unrelated alpine-zfsboot.pool=wrong\n"))
	if info.Pool != "zroot" {
		t.Errorf("Pool = %q, want zroot (must not read past the first line)", info.Pool)
	}
}
