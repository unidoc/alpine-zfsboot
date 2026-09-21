package cmdline

import "testing"

func TestParseVga(t *testing.T) {
	raw := []byte("console=tty0 root=ZFS=zroot/ROOT/alpine ro quiet kexec_load_disabled=0 alpine-zfsboot.pool=zroot alpine-zfsboot.timeout=10 alpine-zfsboot.version=0.1.0 alpine-zfsboot.buildstamp=20260910T003200Z\x00\x00\x00")
	info, err := parse("x86_64", raw, "test.EFI")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Arch != "x86_64" {
		t.Errorf("Arch = %q, want x86_64", info.Arch)
	}
	if info.Console != "vga" {
		t.Errorf("Console = %q, want vga", info.Console)
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

func TestParseSerial(t *testing.T) {
	raw := []byte("console=ttyS0,115200n8 root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.buildstamp=20260910T003200Z\x00")
	info, err := parse("aarch64", raw, "test.EFI")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Console != "serial" {
		t.Errorf("Console = %q, want serial", info.Console)
	}
}

func TestParseAuto(t *testing.T) {
	raw := []byte("console=tty0 console=ttyS0,115200n8 root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.buildstamp=20260910T003200Z\x00")
	info, err := parse("x86_64", raw, "test.EFI")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Console != "auto" {
		t.Errorf("Console = %q, want auto", info.Console)
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
