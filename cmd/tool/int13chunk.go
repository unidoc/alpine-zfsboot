package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/unidoc/alpine-zfsboot/internal/espconfig"
	"github.com/unidoc/alpine-zfsboot/internal/layout"
)

// The BIOS stage2's INT 13h transfer cap (bios/disk.h): the largest number
// of 512-byte sectors one BIOS disk read may ask for. Without
// alpine-zfsboot.int13chunk= in EFI/ALPINE/CMDLINE, stage2 uses its build
// default (layout.Int13ChunkDefault); with it, 1..127. Stage2 builds from
// before the setting existed ignore the key.

// int13FATBatch mirrors bios/Makefile's -DFAT_IO_BATCH_SECTORS for the disk
// stage2 (int13chunk_test.go greps it): no single BIOS read is larger, so
// an int13chunk above it behaves like it.
const int13FATBatch = 32

// parseInt13Chunk validates a user-given value: "default" (remove the
// key) -> "", or 1..layout.Int13ChunkMax.
func parseInt13Chunk(v string) (string, error) {
	if v == "default" {
		return "", nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > layout.Int13ChunkMax || strings.TrimLeft(v, "0123456789") != "" {
		return "", fmt.Errorf("int13chunk %q: want 1..%d (512-byte sectors per BIOS read) or \"default\" (%d)", v, layout.Int13ChunkMax, layout.Int13ChunkDefault)
	}
	return strconv.Itoa(n), nil
}

// cmdlineInt13Chunk returns the value of alpine-zfsboot.int13chunk= in a
// cmdline, and whether stage2 would accept it (the same rule as
// bios/cmdline_opt.c: digits only, 1..127; the first occurrence counts).
func cmdlineInt13Chunk(cmd string) (value string, valid bool) {
	for _, w := range strings.Fields(firstCmdlineLine(cmd)) {
		if v, ok := strings.CutPrefix(w, layout.Int13ChunkKey+"="); ok {
			n, err := strconv.Atoi(v)
			return v, err == nil && n >= 1 && n <= layout.Int13ChunkMax && len(v) <= 5 && strings.TrimLeft(v, "0123456789") == ""
		}
	}
	return "", false
}

func firstCmdlineLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// setCmdlineInt13Chunk returns raw (a CMDLINE file) with every
// alpine-zfsboot.int13chunk= word removed from its first line and, when
// value is not "", one int13chunk=value appended. Every other word, their
// order and the rest of the file stay exactly as they were.
func setCmdlineInt13Chunk(raw []byte, value string) []byte {
	s := string(raw)
	line, rest := s, ""
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		line, rest = s[:i], s[i:]
	}
	var words []string
	for _, w := range strings.Split(line, " ") {
		if strings.HasPrefix(w, layout.Int13ChunkKey+"=") {
			continue
		}
		words = append(words, w)
	}
	line = strings.Join(words, " ")
	if value != "" {
		line = strings.TrimRight(line, " ") + " " + layout.Int13ChunkKey + "=" + value
	}
	return []byte(line + rest)
}

// installedInt13Chunk is the int13chunk value in the installed CMDLINE
// ("" when absent or unreadable) - what `update` carries over.
func installedInt13Chunk(mountpoint string) string {
	raw, err := os.ReadFile(filepath.Join(mountpoint, layout.CmdlineFile))
	if err != nil {
		return ""
	}
	if v, ok := cmdlineInt13Chunk(string(raw)); ok {
		return v
	}
	return ""
}

// setInt13Chunk rewrites the installed CMDLINE (atomically, via
// espconfig.WriteFile) with int13chunk=value, or without the key for "".
func setInt13Chunk(mountpoint, value string) error {
	path := filepath.Join(mountpoint, layout.CmdlineFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", layout.CmdlineFile, err)
	}
	out := setCmdlineInt13Chunk(raw, value)
	if bytes.Equal(out, raw) {
		return nil
	}
	return espconfig.WriteFile(mountpoint, layout.CmdlineFile, out, 0o644)
}

// int13ChunkStatus is status' line: the cap the installed stage2 will use.
func int13ChunkStatus(cmdlineRaw []byte, stage2 []byte) string {
	if stage2 != nil && !bytes.Contains(stage2, []byte(layout.Int13ChunkKey+"=")) {
		return "not configurable (this stage2 predates the setting; 'alpine-zfsboot update' installs one that has it)"
	}
	v, valid := cmdlineInt13Chunk(string(cmdlineRaw))
	switch {
	case v == "":
		return fmt.Sprintf("%d sectors (stage2's build default; change with 'alpine-zfsboot int13chunk')", layout.Int13ChunkDefault)
	case !valid:
		return fmt.Sprintf("%d sectors (%s=%s in %s is invalid and ignored by stage2)", layout.Int13ChunkDefault, layout.Int13ChunkKey, v, layout.CmdlineFile)
	default:
		if n, _ := strconv.Atoi(v); n > int13FATBatch {
			return fmt.Sprintf("%d sectors requested (%s= in %s), %d in effect (the loader reads files in batches of at most %d)",
				n, layout.Int13ChunkKey, layout.CmdlineFile, int13FATBatch, int13FATBatch)
		}
		return fmt.Sprintf("%s sectors (%s= in %s)", v, layout.Int13ChunkKey, layout.CmdlineFile)
	}
}

func newInt13ChunkCmd() *cobra.Command {
	var root, firmware string
	var ho hostOpts
	cmd := &cobra.Command{
		Use:   "int13chunk <1..127|default>",
		Short: "BIOS: set the largest BIOS disk read (in 512-byte sectors) the boot loader issues",
		Long: fmt.Sprintf(`Sets %s=N in EFI/ALPINE/CMDLINE (BIOS installs): the BIOS
loader (stage2) then never asks the BIOS for more than N sectors in one
INT 13h read - for every read after the FAT partition is mounted (kernel,
initrd, their FAT metadata); the few small reads before that use the
build default. "default" removes the key (build default: %d sectors).
On a read failure stage2 still halves the size, starting from N.

Takes effect at the next boot; nothing else in CMDLINE changes, and
nothing reboots. Use it to A/B a machine that fails to boot in BIOS mode,
e.g. 8 against 64.`, layout.Int13ChunkKey, layout.Int13ChunkDefault),
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			value, err := parseInt13Chunk(args[0])
			die(err)
			// Every ESP of a mirrored boot (mirror.go), so they never drift.
			os.Exit(forEachMember(root, firmware, ho, func(t *target) error {
				if t.uefi {
					return fmt.Errorf("int13chunk applies to BIOS installs only (the UEFI loader does not use BIOS disk reads)")
				}
				if err := setInt13Chunk(t.mountpoint, value); err != nil {
					return err
				}
				raw, _ := os.ReadFile(filepath.Join(t.mountpoint, layout.CmdlineFile))
				fmt.Printf("%s: INT13 chunk %s - takes effect at the next boot\n", layout.CmdlineFile, int13ChunkStatus(raw, nil))
				return nil
			}))
		},
	}
	cmd.Flags().StringVar(&root, "root", "/", "root of the target OS (rescue-initramfs use)")
	cmd.Flags().StringVar(&firmware, "firmware", "", "override firmware detection (\"uefi\"/\"bios\")")
	addHostFlags(cmd, &ho, false)
	return cmd
}
