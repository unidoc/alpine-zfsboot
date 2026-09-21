// alpine-zfsboot: a small helper for a machine ALREADY running an
// alpine-zfsboot .EFI, not the boot process itself (see /init and
// menu.py in the repo root for that). Reads the version and
// alpine-zfsboot.* boot params baked into an installed .EFI's own .cmdline
// section, and can fetch+install the latest release build over it.
//
// Deliberately NOT auto-discovering the ESP or the currently-booted
// .EFI's own path - point it at the file explicitly for now (every
// subcommand below takes a path argument). Finding/mounting the ESP
// and picking the right boot entry automatically is real, separate
// scope, left for later once this core piece (read a build's own
// version, fetch+replace it) is proven out.
//
// Built on Cobra rather than hand-rolled os.Args parsing - gets a
// real --help/usage tree, flag parsing (`update --yes`), and a
// --version flag on the root command essentially for free. That root
// --version reports THIS TOOL's own build (see the `version` var
// below) - a completely different thing from the `version <path>`
// subcommand, which reports a .EFI file's own embedded build stamp;
// Cobra's root-level version flag and a subcommand literally named
// "version" don't collide (one's a flag, the other's a positional
// command), but it's worth being explicit about the distinction here
// since the names are so close.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/unidoc/alpine-zfsboot/internal/cmdline"
	"github.com/unidoc/alpine-zfsboot/internal/release"
)

// Overridden via `-ldflags "-X main.version=..."` at release-build
// time (see .github/workflows/release.yml's own build-tool job,
// which sets this to the pushed git tag) - "dev" is what every local
// `go build`/`just build-tool` gets instead, which is exactly correct
// for those: there's no release tag to attribute a local build to.
var version = "dev"

func main() {
	rootCmd := &cobra.Command{
		Use:     "alpine-zfsboot",
		Short:   "install-time helper for an already-running alpine-zfsboot build",
		Version: version,
		Long: `alpine-zfsboot - install-time helper, not part of the boot process itself.

Reads the version and alpine-zfsboot.* boot params baked into an installed
.EFI's own .cmdline section, and can fetch+install the latest release
build over it.`,
	}

	rootCmd.AddCommand(newVersionCmd(), newCheckCmd(), newUpdateCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version <path-to.EFI>",
		Short: "show the version and boot params baked into a build",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			runVersion(args[0])
		},
	}
}

func newCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check <path-to.EFI>",
		Short: "compare a build against the latest release (no changes made)",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			runCheck(args[0])
		},
	}
}

func newUpdateCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "update <path-to.EFI>",
		Short: "fetch and install the latest release over a build",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			runUpdate(args[0], yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the overwrite confirmation prompt")
	return cmd
}

func runVersion(path string) {
	info, err := cmdline.Read(path)
	die(err)
	printInfo(path, info)
}

func printInfo(path string, info cmdline.Info) {
	fmt.Printf("path:     %s\n", path)
	fmt.Printf("arch:     %s\n", info.Arch)
	fmt.Printf("console:  %s\n", info.Console)
	fmt.Printf("version:  %s\n", info.Version)
	fmt.Printf("built:    %s (%s)\n", info.BuildStamp, cmdline.HumanVersion(info.BuildStamp))
	fmt.Printf("pool:     %s\n", orNone(info.Pool))
	fmt.Printf("timeout:  %ss\n", orNone(info.Timeout))
	// Rescue SSH configuration (authorized_keys + a persistent host key)
	// lives entirely on the per-machine ESP now, not in the shared .EFI's
	// own cmdline (see rescue-ssh.sh's own header comment for why) - this
	// tool only ever inspects one .EFI file, so it has no way to answer
	// "is rescue SSH configured" for a given machine anymore, and used to
	// show a field here (ssh_key:) that would always read "not
	// configured" now regardless of a real machine's actual state.
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// comparison is what compare() below found - tmpPath is always the
// downloaded "latest" build's path, valid (and the caller's to clean
// up) whenever err is nil.
type comparison struct {
	local, latest cmdline.Info
	tmpPath       string
	upToDate      bool
}

// compare always downloads the latest build to compare against local
// - there's no cheaper metadata-only path here (see internal/release's
// own comment: release asset filenames carry no version, only the
// artifact itself does, by design - unversioned filenames are exactly
// what lets releases/latest/download/ stay a stable URL across
// releases).
func compare(path, downloadDir string) (comparison, error) {
	local, err := cmdline.Read(path)
	if err != nil {
		return comparison{}, err
	}
	tmp, err := release.Download(local.Arch, downloadDir)
	if err != nil {
		return comparison{}, fmt.Errorf("checking for updates: %w", err)
	}
	latest, err := cmdline.Read(tmp)
	if err != nil {
		os.Remove(tmp)
		return comparison{}, fmt.Errorf("reading the downloaded build: %w", err)
	}
	// Applies to `check` too, not just `update` - deliberately. A
	// wrong-arch asset isn't a normal "out of date" comparison result
	// for check to report and move on from; it's the same "something
	// at that URL doesn't match what it claims to be" condition update
	// refuses to install, so check surfaces it as an error rather than
	// silently reporting up-to-date/out-of-date against bogus data.
	if err := checkArchMatch(path, local, latest); err != nil {
		os.Remove(tmp)
		return comparison{}, err
	}
	return comparison{local: local, latest: latest, tmpPath: tmp, upToDate: latest.BuildStamp <= local.BuildStamp}, nil
}

// checkArchMatch is compare()'s actual guard logic, split out so it's
// testable without a real download - a real HTTP fetch and PE fixture
// file - the same reasoning internal/cmdline's own parse()/Read() split
// already uses in this codebase. release.Download() already requests
// local.Arch's own asset name (release.AssetName(local.Arch)) - this
// isn't defense against a wrong URL, it's defense against what's
// actually AT that URL not matching what its own filename promises: a
// CI matrix mistake, a partial re-upload, or a hand-fixed release could
// publish the wrong bytes under a correctly-arch-named asset.
// latest.Arch is read from the downloaded file's own PE machine type
// (cmdline.Read), independent of the filename that produced it, so
// this catches that case specifically - a correctly-checksummed
// wrong-arch asset would not be caught any other way here.
func checkArchMatch(path string, local, latest cmdline.Info) error {
	if latest.Arch != local.Arch {
		return fmt.Errorf(
			"refusing to install: %s is %s but the downloaded %s asset is %s",
			path, local.Arch, release.AssetName(local.Arch), latest.Arch)
	}
	return nil
}

func runCheck(path string) {
	c, err := compare(path, "") // "" -> system temp dir; check never installs anything, so no same-filesystem-rename requirement
	die(err)
	defer os.Remove(c.tmpPath)

	if c.upToDate {
		fmt.Printf("%s is up to date (%s)\n", path, cmdline.HumanVersion(c.local.BuildStamp))
		return
	}
	fmt.Printf("%s is out of date: local %s, latest %s\n",
		path, cmdline.HumanVersion(c.local.BuildStamp), cmdline.HumanVersion(c.latest.BuildStamp))
	os.Exit(2) // scriptable: exit 2 specifically means "an update is available", distinct from a real error (exit 1)
}

func runUpdate(path string, yes bool) {
	c, err := compare(path, filepath.Dir(path)) // same directory as the real target, so the rename below is atomic
	die(err)

	if c.upToDate {
		os.Remove(c.tmpPath)
		fmt.Printf("%s is already up to date (%s)\n", path, cmdline.HumanVersion(c.local.BuildStamp))
		return
	}

	fmt.Printf("%s: local %s -> latest %s\n",
		path, cmdline.HumanVersion(c.local.BuildStamp), cmdline.HumanVersion(c.latest.BuildStamp))
	if !yes && !confirm(fmt.Sprintf("overwrite %s with the latest build?", path)) {
		os.Remove(c.tmpPath)
		fmt.Println("not updated")
		return
	}

	// Match the original file's permissions rather than whatever
	// os.CreateTemp defaulted to (0600) - this is meant to end up as
	// a normal, readable boot file, same as the one it's replacing.
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode()
	}
	if err := os.Chmod(c.tmpPath, mode); err != nil {
		os.Remove(c.tmpPath)
		die(fmt.Errorf("setting permissions on the downloaded build: %w", err))
	}
	backedUp := backupPrevious(path)

	// Rename, not copy-then-delete: atomic on the same filesystem
	// (guaranteed by downloading into filepath.Dir(path) above) -
	// there's never a moment where `path` is half-written or missing
	// entirely, which matters for a file a firmware/bootloader might
	// read at any time.
	if err := os.Rename(c.tmpPath, path); err != nil {
		os.Remove(c.tmpPath)
		die(fmt.Errorf("installing the new build over %s: %w", path, err))
	}
	if backedUp {
		fmt.Printf("%s updated to %s (previous build kept at %s.previous)\n", path, cmdline.HumanVersion(c.latest.BuildStamp), path)
	} else {
		fmt.Printf("%s updated to %s (no local rollback copy - could not preserve the previous build)\n", path, cmdline.HumanVersion(c.latest.BuildStamp))
	}
}

// hardLink is os.Link, swappable in tests to force backupPrevious's
// copy fallback deterministically - path's real target is the ESP
// (vfat), which has no hard links at all, so that fallback is the
// normal case in production, not an edge case, but nothing in this
// codebase's own test environment (a plain tmp filesystem) can make a
// real os.Link call fail to exercise it for real.
var hardLink = os.Link

// backupPrevious preserves the file currently at path as
// path+".previous" before it gets overwritten, and reports whether a
// usable rollback copy actually exists there afterward - the caller
// must not claim a backup it doesn't have (see the git history for
// why: a hard link silently no-ops on vfat, the ESP's own filesystem
// and this tool's real target, and this used to unconditionally claim
// "previous build kept" regardless of whether the link succeeded).
//
// Tries a hard link first - not a data copy, so it's instant
// regardless of file size, and (unlike a rename-based backup) never
// removes path itself even momentarily, so it can't introduce a
// missing-file window in the caller's own rename-based install. Falls
// back to a real copy when linking fails, which is the normal case
// here (vfat has no hard links; Linux's own fs/fat exposes no .link
// inode operation either, same outcome).
//
// Best-effort either way: os.Remove clears any stale link/copy from an
// earlier update (os.Link fails if the destination already exists); if
// there's no local build to back up yet (first-ever update) or the
// copy fallback also fails, the caller still proceeds with the update
// - a missing rollback copy is never a reason to block one, it's just
// something the caller must not claim.
func backupPrevious(path string) bool {
	dst := path + ".previous"
	os.Remove(dst)

	if err := hardLink(path, dst); err == nil {
		return true
	}
	return copyFile(path, dst) == nil
}

// copyFile is backupPrevious's fallback for filesystems without hard
// links (vfat, i.e. the ESP). Cleans up its own partial output on any
// failure, so a failed backup never leaves a truncated ".previous"
// file that looks usable but isn't.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}

func confirm(prompt string) bool {
	fmt.Printf("%s [y/N] ", prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "y")
}

func die(err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "alpine-zfsboot:", err)
	os.Exit(1)
}
