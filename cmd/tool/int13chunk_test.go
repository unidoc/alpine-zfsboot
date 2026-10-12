package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/unidoc/alpine-zfsboot/internal/layout"
)

func TestParseInt13Chunk(t *testing.T) {
	for in, want := range map[string]string{"1": "1", "16": "16", "127": "127", "008": "8", "default": ""} {
		if got, err := parseInt13Chunk(in); err != nil || got != want {
			t.Errorf("%q: %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"0", "128", "-1", "+8", "0x10", "16k", "", "abc"} {
		if _, err := parseInt13Chunk(in); err == nil {
			t.Errorf("%q: want an error", in)
		}
	}
}

func TestSetCmdlineInt13Chunk(t *testing.T) {
	base := "console=tty0 root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.version=0.5.0 \n"
	got := string(setCmdlineInt13Chunk([]byte(base), "8"))
	if got != "console=tty0 root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.version=0.5.0 alpine-zfsboot.int13chunk=8\n" {
		t.Fatalf("add: %q", got)
	}
	got2 := string(setCmdlineInt13Chunk([]byte(got), "64"))
	if strings.Count(got2, "int13chunk=") != 1 || !strings.Contains(got2, "int13chunk=64\n") {
		t.Fatalf("replace: %q", got2)
	}
	if back := string(setCmdlineInt13Chunk([]byte(got2), "")); back != "console=tty0 root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.version=0.5.0\n" {
		t.Fatalf("remove: %q", back)
	}
	// Duplicates are collapsed; other words and extra lines untouched.
	dup := "a alpine-zfsboot.int13chunk=8 b alpine-zfsboot.int13chunk=9\nsecond line\n"
	if got := string(setCmdlineInt13Chunk([]byte(dup), "32")); got != "a b alpine-zfsboot.int13chunk=32\nsecond line\n" {
		t.Fatalf("duplicates: %q", got)
	}
	if v, ok := cmdlineInt13Chunk("x alpine-zfsboot.int13chunk=0"); v != "0" || ok {
		t.Fatalf("0 must be found but invalid: %q %v", v, ok)
	}
}

func TestInt13ChunkInstallUpdateAndCommand(t *testing.T) {
	mountpoint := newBIOSMountpoint(t)
	release := []byte(testCmdline)

	// install --int13chunk=8
	out, err := applyInt13Chunk(release, "", biosOpts{int13chunk: "8"})
	if err != nil || !strings.Contains(string(out), "alpine-zfsboot.int13chunk=8") {
		t.Fatalf("install --int13chunk=8: %q %v", out, err)
	}
	if err := os.WriteFile(filepath.Join(mountpoint, layout.CmdlineFile), out, 0o644); err != nil {
		t.Fatal(err)
	}
	// update without the flag keeps it; with the flag replaces it.
	if out, _ := applyInt13Chunk(release, mountpoint, biosOpts{}); !strings.Contains(string(out), "int13chunk=8") {
		t.Fatalf("update keeps the installed value: %q", out)
	}
	if out, _ := applyInt13Chunk(release, mountpoint, biosOpts{int13chunk: "default"}); strings.Contains(string(out), "int13chunk") {
		t.Fatalf("update --int13chunk=default removes it: %q", out)
	}
	if _, err := applyInt13Chunk(release, mountpoint, biosOpts{int13chunk: "500"}); err == nil {
		t.Fatal("--int13chunk=500: want an error before any write")
	}
	// The command.
	if err := setInt13Chunk(mountpoint, "32"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(mountpoint, layout.CmdlineFile))
	if !strings.Contains(string(raw), "int13chunk=32") || !strings.Contains(string(raw), "alpine-zfsboot.buildstamp=20260923T150000Z") {
		t.Fatalf("int13chunk 32: %q", raw)
	}
	if s := int13ChunkStatus(raw, []byte("...alpine-zfsboot.int13chunk=...")); !strings.HasPrefix(s, "32 sectors") {
		t.Fatalf("status: %q", s)
	}
	if s := int13ChunkStatus([]byte(testCmdline), []byte("alpine-zfsboot.int13chunk=")); !strings.HasPrefix(s, "16 sectors (stage2's build default") {
		t.Fatalf("status default: %q", s)
	}
	if s := int13ChunkStatus(raw, []byte("an old stage2")); !strings.HasPrefix(s, "not configurable") {
		t.Fatalf("status, old stage2: %q", s)
	}
}

func TestInt13ChunkAboveFATBatch(t *testing.T) {
	mk, err := os.ReadFile("../../bios/Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mk), "-DFAT_IO_BATCH_SECTORS="+strconv.Itoa(int13FATBatch)+" -o $@") {
		t.Fatalf("bios/Makefile's disk fat.o batch is not %d", int13FATBatch)
	}
	if s := int13ChunkStatus([]byte("x alpine-zfsboot.int13chunk=64\n"), nil); !strings.Contains(s, "64 sectors requested") || !strings.Contains(s, "32 in effect") {
		t.Fatalf("status for 64: %q", s)
	}
}
