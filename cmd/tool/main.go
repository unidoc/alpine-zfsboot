// alpine-zfsboot: the authoritative management interface for an
// installed alpine-zfsboot boot environment, on both firmwares.
// Firmware (BIOS vs UEFI) is detected, never a user-facing choice -
// see internal/bootenv.IsUEFI - so every subcommand below dispatches
// internally between internal/uefiboot (a single self-contained
// Unified-Kernel-Image-style .EFI) and internal/biosboot+
// internal/espconfig (stage1/stage2 plus the separate FAT payload/
// config files) rather than exposing that split to the caller.
//
// install/update/verify all share the same low-level primitives
// (internal/biosboot's WriteStage1/WriteStage2, internal/espconfig's
// WritePayload/WriteConfig, internal/uefiboot's WriteLoader) - see
// each package's own doc comment - rather than duplicating per-command
// logic. status and verify additionally share one fact-gathering pass
// (inspect, below) - status prints it, verify additionally treats any
// gap in it as a failure.
//
// Built on Cobra - gets a real --help/usage tree, flag parsing, and a
// --version flag on the root command essentially for free. That root
// --version reports THIS TOOL's own build (see the `version` var
// below) - a different thing from the `version <path>` subcommand,
// which reports a .EFI file's own embedded build stamp.
package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/unidoc/alpine-zfsboot/internal/biosboot"
	"github.com/unidoc/alpine-zfsboot/internal/bootenv"
	"github.com/unidoc/alpine-zfsboot/internal/cmdline"
	"github.com/unidoc/alpine-zfsboot/internal/espconfig"
	"github.com/unidoc/alpine-zfsboot/internal/initrdinfo"
	"github.com/unidoc/alpine-zfsboot/internal/kernelinfo"
	"github.com/unidoc/alpine-zfsboot/internal/layout"
	"github.com/unidoc/alpine-zfsboot/internal/metadata"
	"github.com/unidoc/alpine-zfsboot/internal/release"
	"github.com/unidoc/alpine-zfsboot/internal/uefiboot"

	zfscompat "github.com/unidoc/alpine-zfsboot"
)

// Overridden via `-ldflags "-X main.version=..."`: a real release
// build (.github/workflows/release.yml's own build-tool job) sets
// this to the pushed git tag; `just build-tool` (also what `just
// build ARCH` uses to bake this binary into the rescue image itself -
// see build.sh) sets it to the repo's own version.txt instead, same
// as every other artifact that file bakes a version into. "dev" is
// what a bare `go build ./cmd/tool` still gets, with neither of those
// - correct for that one case, since there's genuinely no version to
// attribute it to.
var version = "dev"

func main() {
	rootCmd := &cobra.Command{
		Use:     "alpine-zfsboot",
		Short:   "alpine-zfsboot by UniDoc - the authoritative management interface for an installed alpine-zfsboot boot environment",
		Version: version,
		Long: `alpine-zfsboot by UniDoc - install, update, verify, and report on this
machine's own alpine-zfsboot boot environment (BIOS or UEFI, detected
automatically).

dd at a fixed LBA is an implementation detail behind these commands, not
something an operator needs to know or do by hand.`,
	}

	rootCmd.AddCommand(
		newVersionCmd(),
		newStatusCmd(),
		newVerifyCmd(),
		newUpdateCmd(),
		newInstallCmd(),
	)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// detectArch maps this binary's own compiled GOARCH to alpine-zfsboot's
// own arch naming ("x86_64"/"aarch64" - matches build.sh's own ARCH
// values, see internal/layout/internal/release's own naming) - a live
// target's own architecture, not something read from anywhere else,
// since this tool only ever runs natively on the machine it manages.
func detectArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return runtime.GOARCH
	}
}

// activeCleanup, if non-nil, is called by die() before os.Exit - a full
// source audit found os.Exit bypasses every deferred t.cleanup() a
// command already registered (Go never runs pending defers across
// os.Exit), so any die() call anywhere after discover() succeeded -
// which every status/verify/update/install Run closure does - leaked
// the ESP mount: MountESP's own temp mountpoint directory, and worse,
// the ESP itself staying mounted with nothing left in this process
// able to ever unmount it. A later run of this same tool then mounts
// the SAME device a second time, independently - two separate mounts
// of one vfat filesystem have two separate page caches, so a write
// through one is invisible to a read through the other, a real
// corruption risk for exactly the boot-payload partition this tool
// exists to manage safely. Set at each of this file's three
// discover()-calling Run closures right after discover() succeeds;
// reset to nil by the same closure's own deferred cleanup, so a
// (currently unreachable, but not structurally prevented) later die()
// can't double-call an already-unmounted cleanup.
var activeCleanup func()

func die(err error) {
	if err == nil {
		return
	}
	if activeCleanup != nil {
		activeCleanup()
	}
	fmt.Fprintln(os.Stderr, "alpine-zfsboot:", err)
	os.Exit(1)
}

// withCleanup registers cleanup as die()'s own activeCleanup for the
// remainder of the calling Run closure, and returns a func the caller
// must immediately `defer` itself (defer only ever delays until its
// OWN enclosing function returns, so this can't set the defer up on
// the caller's behalf - only hand back what to defer).
func withCleanup(cleanup func()) func() {
	activeCleanup = cleanup
	return func() {
		activeCleanup = nil
		cleanup()
	}
}

func confirm(prompt string) bool {
	fmt.Printf("%s [y/N] ", prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "y")
}

// --- version (unchanged - inspects one .EFI file directly, a
// different question from status, which reports on the whole
// installed system) --------------------------------------------------

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version <path-to.EFI>",
		Short: "show the version and boot params baked into a UEFI .EFI build",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			info, err := cmdline.Read(args[0])
			die(err)
			printInfo(args[0], info)
		},
	}
}

func printInfo(path string, info cmdline.Info) {
	fmt.Printf("path:     %s\n", path)
	fmt.Printf("arch:     %s\n", info.Arch)
	fmt.Printf("console:  %s\n", info.Console)
	fmt.Printf("version:  %s\n", info.Version)
	fmt.Printf("built:    %s (%s)\n", info.BuildStamp, cmdline.HumanVersion(info.BuildStamp))
	fmt.Printf("pool:     %s\n", orNone(info.Pool))
	fmt.Printf("timeout:  %ss\n", orNone(info.Timeout))
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// --- shared discovery/report ------------------------------------------

// target is what status/verify/update discovered about this machine's
// own installation before doing anything else - one gathering pass,
// not a separate implementation per command (see this file's own doc
// comment).
type target struct {
	uefi bool
	arch string

	// BIOS only
	espDev     string
	disk       string
	layoutKind bootenv.DiskLayout
	mountpoint string
	cleanup    func()
}

// discover gathers what status/verify/update need before doing
// anything else. firmwareOverride ("uefi"/"bios"/"") exists for the
// SAME reason install's own --firmware flag is required, not
// auto-detected (see newInstallCmd's own doc comment): a caller
// invoking these against a not-yet-booted --root (alpine-installer's
// own verify_installation(), right after install) has no live
// /sys/firmware/efi under that root to check - bootenv.IsUEFI(root)
// would silently misdetect there. Empty string keeps the normal,
// correct behavior for the common case this WAS built for: a live,
// already-booted system, where root is "/" and its own real
// /sys/firmware/efi is exactly the right thing to trust.
func discover(root, firmwareOverride string, readonly bool) (*target, error) {
	var uefi bool
	switch firmwareOverride {
	case "uefi":
		uefi = true
	case "bios":
		uefi = false
	case "":
		uefi = bootenv.IsUEFI(root)
	default:
		return nil, fmt.Errorf("--firmware must be \"uefi\" or \"bios\" (got %q)", firmwareOverride)
	}
	t := &target{uefi: uefi, arch: detectArch()}

	espDev, err := bootenv.FindESP()
	if err != nil {
		return nil, err
	}
	t.espDev = espDev
	t.disk = bootenv.DevicePartitionBase(espDev)
	if !t.uefi {
		if l, err := bootenv.DetectDiskLayout(t.disk); err == nil {
			t.layoutKind = l
		}
	}

	mountpoint, cleanup, err := bootenv.MountESP(espDev, readonly)
	if err != nil {
		return nil, err
	}
	t.mountpoint, t.cleanup = mountpoint, cleanup
	return t, nil
}

// report is one fact-finding pass over an already-discovered target -
// status prints it as-is; verify additionally fails on any non-empty
// error field. Covers the whole chain the user asked status to
// answer in one place: firmware/bootstrap identity, the boot
// environment's own kernel/OpenZFS identity (NOT the same thing as
// the bootstrap's own version - see zfsCompat's own doc comment), and
// - separately, honestly - whether that's provably compatible with a
// live pool's own feature set.
type report struct {
	arch       string
	firmware   string
	disk       string
	layoutKind string
	espDev     string

	// Bootstrap: stage1/stage2 (BIOS) or the UEFI loader - what
	// actually runs before Linux does. NEVER touches ZFS itself (see
	// zfsCompat's own doc comment) - installedVersion here is
	// alpine-zfsboot's OWN release version (extracted from the
	// bootstrap artifact's own embedded string - biosboot.
	// ExtractStage2Version for BIOS, cmdline.Info.Version for UEFI),
	// not a kernel or OpenZFS version.
	installedVersion string
	stage1Desc       string
	stage1Err        error
	stage2Desc       string
	stage2Bytes      int
	stage2Err        error
	loaderPath       string
	loaderErr        error

	// Boot environment: the kernel + initrd the bootstrap hands off
	// to via kexec/UEFI LoadImage - a completely separate identity
	// from the bootstrap's own version above.
	kernelErr, initrdErr, cmdlineErr, configErr error
	kernelVersion                               string
	kernelVersionErr                            error
	openZFSVersion                              string
	openZFSVersionErr                           error

	// metadataPresent is true when internal/layout.MetadataFile existed
	// and decoded cleanly - in which case kernelVersion/openZFSVersion
	// above came from it directly (the fast path - see internal/
	// metadata's own doc comment), not from decompressing/parsing the
	// real kernel/initrd. metadataErr is set instead when the file
	// existed but failed to decode (a real, reportable problem,
	// distinct from simply being absent - an older installation
	// predating this feature has neither field set). Neither field is
	// ever set by inspect() WRITING anything - see internal/metadata's
	// own "read-only commands stay read-only" requirement.
	metadataPresent bool
	metadataErr     error

	// metadataVerified is set by newVerifyCmd's Run closure, never by
	// inspect() itself - true only when verifyMetadata() actually ran
	// (verify always calls it; status never does) and found no
	// discrepancy. Distinct from metadataPresent (a manifest file
	// merely existing and decoding is not the same claim as its
	// recorded hash(es) having just been checked against the real
	// installed payload) - print()'s own "Metadata:" line uses this to
	// tell a genuinely-confirmed result apart from the "from manifest,
	// unconfirmed" case, so `verify` never tells the user to go run
	// `verify` while reporting the real answer as `verify: OK` two
	// lines later.
	metadataVerified bool

	zfs zfsCompat
}

// zfsCompat gathers the raw, independently-established facts (OpenZFS
// version, pool's own feature states) - see the zfscompat package
// (repo root, github.com/unidoc/alpine-zfsboot) for how those facts
// get turned into an actual VERIFIED/NO/UNKNOWN verdict, and its own
// doc comment for why this is no longer a guess: both the "is
// compatibility.d the full feature set" and "does only ACTIVE (not
// just enabled) matter" questions are verified there directly against
// OpenZFS source, not assumed. Stage1/stage2/the UEFI loader never
// implement ZFS themselves either way - they only locate/load the FAT
// payload or PE sections; it is the boot environment's own initrd
// (openZFSVersion above) that determines what a real `zpool import`
// at boot time can actually handle.
type zfsCompat struct {
	poolName       string
	poolFeatures   []string // display strings "name=state", for the existing Pool features: line
	activeFeatures []string // just the active (not merely enabled) ones' short names - see zfscompat's own doc comment for why only active ones matter
	poolErr        error
}

func inspect(t *target) report {
	r := report{arch: t.arch, disk: t.disk, espDev: t.espDev}
	if t.uefi {
		r.firmware = "UEFI"
		rel, err := uefiboot.LoaderPath(t.arch)
		r.loaderPath = rel
		if err != nil {
			r.loaderErr = err
			return r
		}
		loaderFullPath := filepath.Join(t.mountpoint, rel)
		info, err := cmdline.Read(loaderFullPath)
		if err != nil {
			r.loaderErr = err
			return r
		}
		r.installedVersion = info.Version

		if m, present, err := tryReadMetadata(t.mountpoint); present {
			r.metadataPresent = true
			r.kernelVersion = m.Kernel
			r.openZFSVersion = m.OpenZFS
		} else {
			r.metadataErr = err // nil if simply absent, non-nil if present-but-broken
			if kernelBytes, err := cmdline.ReadSection(loaderFullPath, ".linux"); err != nil {
				r.kernelVersionErr = err
			} else if v, err := kernelinfo.VersionFromImage(kernelBytes); err == nil {
				r.kernelVersion = v
			} else {
				r.kernelVersionErr = err
			}
			if initrdBytes, err := cmdline.ReadSection(loaderFullPath, ".initrd"); err != nil {
				r.openZFSVersionErr = err
			} else if v, err := initrdinfo.ExtractOpenZFSVersionFromBytes(initrdBytes); err != nil {
				r.openZFSVersionErr = err
			} else {
				r.openZFSVersion = v
			}
		}
		r.zfs = inspectZFSCompat(info.Pool)
		return r
	}

	r.firmware = "BIOS"
	r.layoutKind = t.layoutKind.String()
	r.stage1Desc = fmt.Sprintf("MBR %s", t.disk)
	r.stage2Desc = fmt.Sprintf("%s LBA %d-%d", t.disk, layout.Stage2LBA, layout.Stage2LBA+layout.Stage2Sectors-1)

	if _, err := biosboot.ReadStage1(t.disk); err != nil {
		r.stage1Err = err
	}
	stage2, err := biosboot.ReadStage2(t.disk)
	if err != nil {
		r.stage2Err = err
	} else if len(stage2) == 0 {
		r.stage2Err = fmt.Errorf("stage2 extent is empty - nothing installed")
	} else {
		r.stage2Bytes = len(stage2)
		if v, _, err := biosboot.ExtractStage2Version(stage2); err != nil {
			// Not fatal to the stage2 health check itself - an older
			// stage2.bin built before this string existed is still a
			// perfectly working bootstrap, just not self-describing.
			r.installedVersion = "(unknown)"
		} else {
			r.installedVersion = v
		}
	}

	for path, dst := range map[string]*error{
		layout.CmdlineFile: &r.cmdlineErr,
		layout.ConfigFile:  &r.configErr,
	} {
		if _, err := os.Stat(filepath.Join(t.mountpoint, path)); err != nil {
			*dst = fmt.Errorf("missing: %w", err)
		}
	}

	kernelPath := filepath.Join(t.mountpoint, layout.KernelFile)
	if _, err := os.Stat(kernelPath); err != nil {
		r.kernelErr = fmt.Errorf("missing: %w", err)
	}
	initrdPath := filepath.Join(t.mountpoint, layout.InitrdFile)
	if _, err := os.Stat(initrdPath); err != nil {
		r.initrdErr = fmt.Errorf("missing: %w", err)
	}

	if m, present, err := tryReadMetadata(t.mountpoint); present {
		r.metadataPresent = true
		if r.kernelErr == nil {
			r.kernelVersion = m.Kernel
		}
		if r.initrdErr == nil {
			r.openZFSVersion = m.OpenZFS
		}
	} else {
		r.metadataErr = err // nil if simply absent, non-nil if present-but-broken
		if r.kernelErr == nil {
			if v, err := kernelinfo.Version(kernelPath); err != nil {
				r.kernelVersionErr = err
			} else {
				r.kernelVersion = v
			}
		}
		if r.initrdErr == nil {
			if v, err := initrdinfo.ExtractOpenZFSVersion(initrdPath); err != nil {
				r.openZFSVersionErr = err
			} else {
				r.openZFSVersion = v
			}
		}
	}

	// bootPool stays empty (never a fatal error on its own - r.cmdlineErr
	// above already covers a genuinely missing/unreadable cmdline.txt)
	// if this read fails - inspectZFSCompat/selectBootPool below treat
	// an empty bootPool as "unknown", not as an error in itself.
	var bootPool string
	if raw, err := os.ReadFile(filepath.Join(t.mountpoint, layout.CmdlineFile)); err == nil {
		bootPool = cmdline.ParseText(raw).Pool
	}

	r.zfs = inspectZFSCompat(bootPool)
	return r
}

// selectBootPool picks which of the currently-imported pools (imported,
// from a real `zpool list`) the compatibility check should actually
// run against - a full source audit found this used to be "whichever
// pool zpool list happens to print first" unconditionally, with no
// connection at all to which pool this specific build's own cmdline
// (alpine-zfsboot.pool=, parsed by inspect()'s two call sites) says it
// boots. On a machine with a boot pool PLUS a separate data pool (a
// completely ordinary setup - `zpool list` prints them in whatever
// order the kernel happens to enumerate them, not boot-relevance
// order), the compatibility verdict could be computed against the
// WRONG pool entirely - "Boot compatible: VERIFIED" while the actual
// boot pool has an unsupported feature active. A single imported pool
// is unambiguous regardless of whether bootPool is known (the
// overwhelmingly common real case - see this project's own single-pool
// examples throughout); multiple imported pools without a known
// bootPool, or a bootPool that isn't among what's actually imported,
// both refuse rather than guess.
func selectBootPool(bootPool string, imported []string) (string, error) {
	if bootPool == "" {
		if len(imported) == 1 {
			return imported[0], nil
		}
		return "", fmt.Errorf("multiple pools imported (%s) and the boot pool is unknown (no alpine-zfsboot.pool= recorded on this build) - refusing to guess which one to check", strings.Join(imported, ", "))
	}
	for _, n := range imported {
		if n == bootPool {
			return bootPool, nil
		}
	}
	return "", fmt.Errorf("boot pool %q (from alpine-zfsboot.pool=) is not among the imported pools (%s)", bootPool, strings.Join(imported, ", "))
}

// inspectZFSCompat gathers the LIVE pool side of the picture - read-only
// (ZFS_IOC_POOL_CONFIGS/ZFS_IOC_POOL_STATS via internal/zfsnative, a
// pure-Go /dev/zfs client internalized from github.com/go-fsctl/zfs - see
// zfsnative.go's own header comment for the ABI due diligence behind this),
// never a pool-lifecycle ioctl (this tool
// diagnoses, it does not manage ZFS - see this package's own doc comment).
// Absence of an importable pool is not treated as an error worth failing
// verify over - a rescue/pre-import boot state is a normal thing to run
// `alpine-zfsboot status` during.
//
// bootPool (from alpine-zfsboot.pool=, already parsed out of this
// build's own cmdline by inspect()'s two call sites) is which pool
// selectBootPool below actually checks - see that function's own doc
// comment for why "whichever pool happens to be listed first" was a
// real bug, not just an edge case.
func inspectZFSCompat(bootPool string) zfsCompat {
	var z zfsCompat
	names, err := liveZFSPoolNames()
	if err != nil {
		z.poolErr = err
		return z
	}
	if len(names) == 0 {
		z.poolErr = fmt.Errorf("no imported pool found")
		return z
	}
	z.poolName, err = selectBootPool(bootPool, names)
	if err != nil {
		z.poolErr = err
		return z
	}

	stats, err := liveZFSFeatureStats(z.poolName)
	if err != nil {
		z.poolErr = fmt.Errorf("%s: %w", z.poolName, err)
		return z
	}
	poolFeatures, activeFeatures, err := parseFeatureStats(stats)
	z.poolFeatures, z.activeFeatures = poolFeatures, activeFeatures
	if err != nil {
		z.poolErr = fmt.Errorf("%s: %w", z.poolName, err)
	}
	return z
}

// bootCompatVerdict is print()'s own "Boot compatible:" decision, pulled
// out as a pure function so it's unit-testable without capturing stdout -
// status is one of "UNKNOWN"/"VERIFIED"/"NO"; detail is the accompanying
// "Reason:" text for UNKNOWN, the "Unsupported active:" list for NO
// (comma-joined already), or empty for VERIFIED.
func bootCompatVerdict(r report) (status, detail string) {
	switch {
	case r.openZFSVersionErr != nil:
		return "UNKNOWN", "could not determine the boot environment's OpenZFS version"
	case r.zfs.poolErr != nil:
		return "UNKNOWN", "no imported pool to check features against"
	}

	result, err := zfscompat.CheckZFSCompat(r.openZFSVersion, r.zfs.activeFeatures)
	switch {
	case err != nil:
		return "UNKNOWN", err.Error()
	case !result.KnownReleaseLine:
		return "UNKNOWN", "no vendored compatibility data for OpenZFS release line " + result.ReleaseLine
	case result.Compatible():
		return "VERIFIED", ""
	default:
		return "NO", strings.Join(result.Unsupported, ", ")
	}
}

// appendCompatErr is verify's own pass/fail extension of bootCompatVerdict:
// a PROVEN incompatibility (NO) is a real reason this machine will fail to
// boot, so verify must fail on it, not just print it and say OK. UNKNOWN
// stays advisory/non-fatal on purpose - there's no vendored data, no
// imported pool, or no parseable version to judge against, which is "can't
// prove either way", not "proven broken"; failing verify on every UNKNOWN
// would make it impossible to verify a healthy pre-import/rescue boot at
// all. Pulled out of newVerifyCmd's Run closure, same reason
// bootCompatVerdict itself is pulled out of print(): testable without
// exercising the cobra command (which os.Exits on failure).
func appendCompatErr(errs []error, r report) []error {
	if status, detail := bootCompatVerdict(r); status == "NO" {
		errs = append(errs, fmt.Errorf("ZFS boot compatibility: NO - unsupported active features: %s", detail))
	}
	return errs
}

func (r report) print(verbose bool) {
	fmt.Println("alpine-zfsboot by UniDoc")
	fmt.Printf("  %-20s %s\n", "Version:", orNone(r.installedVersion))
	fmt.Printf("  %-20s %s\n", "Architecture:", r.arch)
	fmt.Printf("  %-20s %s\n", "Firmware:", r.firmware)
	fmt.Printf("  %-20s %s\n", "Boot disk:", r.disk)
	if r.firmware == "BIOS" {
		fmt.Printf("  %-20s %s\n", "Partitioning:", r.layoutKind)
	}

	fmt.Println("Bootstrap")
	if r.firmware == "BIOS" {
		printField("Stage1", r.stage1Err, "OK ("+r.stage1Desc+")")
		printField("Stage2", r.stage2Err, "OK ("+r.stage2Desc+")")
		fmt.Printf("  %-20s %s\n", "Stage2 version:", orNone(r.installedVersion))
	} else {
		printField("EFI loader", r.loaderErr, "OK ("+r.loaderPath+")")
	}
	if len(r.errs()) == 0 {
		fmt.Printf("  %-20s OK\n", "Status:")
	} else {
		fmt.Printf("  %-20s %d problem(s) - see above\n", "Status:", len(r.errs()))
	}

	fmt.Println("Boot environment")
	fmt.Printf("  %-20s %s\n", "FAT:", r.espDev)
	fmt.Printf("  %-20s %s\n", "Namespace:", layout.ESPDir)
	if r.firmware == "BIOS" {
		printField("Kernel", r.kernelErr, layout.KernelFile)
		printField("Initramfs", r.initrdErr, layout.InitrdFile)
		printField("Cmdline", r.cmdlineErr, "OK")
		printField("Config", r.configErr, "OK")
	}
	printField("Kernel version", r.kernelVersionErr, kernelinfo.ShortVersion(r.kernelVersion))
	printField("OpenZFS", r.openZFSVersionErr, r.openZFSVersion)
	switch {
	case r.metadataVerified:
		// Only ever true when the caller is `verify` (status never sets
		// this field) and verifyMetadata() actually re-derived and
		// compared the real payload's hash(es) against the manifest,
		// finding no discrepancy - the genuine, confirmed answer, not
		// the "from manifest, unconfirmed" case below.
		fmt.Printf("  %-20s OK (confirmed against the real installed payload)\n", "Metadata:")
	case r.metadataErr != nil:
		fmt.Printf("  %-20s ERROR (%v)\n", "Metadata:", r.metadataErr)
	case r.metadataPresent:
		// Deliberately NOT "OK": status never hashes the real installed
		// kernel/initrd (that's the whole latency win) - this line only
		// means the kernel/OpenZFS versions above came from the manifest,
		// not that the manifest has been confirmed to still match the
		// real payload. A final pre-release audit demonstrated this gap
		// for real: after a crash between a payload write and its own
		// metadata write, status silently reported a stale generation
		// while `verify` correctly caught it via both SHA-256 fields -
		// `verify` is the actual acceptance check, not `status`. When
		// the caller IS `verify`, this branch only prints if the real
		// check actually found a discrepancy (metadataVerified stays
		// false) - that mismatch is then spelled out explicitly in the
		// "problem(s) found" list below, so the advice here isn't
		// circular even in that case; it's only ever circular-sounding
		// noise in the common, healthy `status` case this line exists
		// for.
		fmt.Printf("  %-20s from manifest (run 'alpine-zfsboot verify' to confirm it matches the real payload)\n", "Metadata:")
	default:
		fmt.Printf("  %-20s MISSING (deep-inspected instead - run 'alpine-zfsboot verify --repair' to add it)\n", "Metadata:")
	}

	fmt.Println("ZFS compatibility")
	if r.zfs.poolErr != nil {
		fmt.Printf("  %-20s %v\n", "Pool:", r.zfs.poolErr)
	} else {
		fmt.Printf("  %-20s %s\n", "Pool:", r.zfs.poolName)
		if verbose {
			if len(r.zfs.poolFeatures) == 0 {
				fmt.Printf("  %-20s (none active)\n", "Pool features:")
			} else {
				fmt.Printf("  %-20s %s\n", "Pool features:", strings.Join(r.zfs.poolFeatures, ", "))
			}
		} else {
			// Summary by default - the full "name=state, name=state, ..."
			// line gets long fast on a real pool (46+ entries is normal;
			// see the hardening ledger's own real CAX status output) and
			// most of it isn't what a human scanning `status` wants to
			// read every time. --verbose restores the full line above.
			activeN := len(r.zfs.activeFeatures)
			enabledN := len(r.zfs.poolFeatures) - activeN
			fmt.Printf("  %-20s %d active, %d enabled (use --verbose for the full list)\n", "Pool features:", activeN, enabledN)
		}
	}
	fmt.Printf("  %-20s %s\n", "Boot environment:", openZFSSummary(r))

	status, detail := bootCompatVerdict(r)
	fmt.Printf("  %-20s %s\n", "Boot compatible:", status)
	if detail != "" {
		label := "Reason:"
		if status == "NO" {
			label = "Unsupported active:"
		}
		fmt.Printf("  %-20s %s\n", label, detail)
	}
}

func openZFSSummary(r report) string {
	if r.openZFSVersionErr != nil {
		return fmt.Sprintf("ERROR (%v)", r.openZFSVersionErr)
	}
	return "OpenZFS " + r.openZFSVersion
}

func printField(name string, err error, okMsg string) {
	label := name + ":"
	if err != nil {
		fmt.Printf("  %-20s ERROR (%v)\n", label, err)
		return
	}
	fmt.Printf("  %-20s %s\n", label, okMsg)
}

// errs collects every non-nil error field in r that represents an
// actual problem with the INSTALLED boot artifacts (verify's own
// pass/fail decision) - deliberately excludes zfs.poolErr (no
// imported pool is normal during a rescue/pre-import boot, not an
// installation defect) and the version-string extraction errors
// (kernelVersionErr/openZFSVersionErr - advisory identification, not
// install-health signals: a real kernel/initrd that verify has
// already confirmed present and byte-correct is not "broken" merely
// because this tool couldn't parse a version banner out of it).
func (r report) errs() []error {
	var out []error
	// metadataErr included (F22, unidoc-alip's PR #5 review) - it was
	// missing from this list despite print()'s own "Metadata:" line
	// already surfacing it as ERROR(...) when present-but-undecodable.
	// verify's own real pass/fail decision currently happens not to
	// miss this in practice (verifyMetadata() re-checks the same
	// manifest independently and adds its own error either way), but
	// errs() is this report's one general-purpose "everything wrong"
	// collector - a populated error field it silently skips is a real
	// gap in the collector itself, not something that should depend on
	// every future caller separately re-deriving the same check.
	for _, e := range []error{r.stage1Err, r.stage2Err, r.loaderErr, r.kernelErr, r.initrdErr, r.cmdlineErr, r.configErr, r.metadataErr} {
		if e != nil {
			out = append(out, e)
		}
	}
	return out
}

// --- status --------------------------------------------------------

func newStatusCmd() *cobra.Command {
	var root, firmware string
	var verbose bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "report this machine's own alpine-zfsboot installation",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			t, err := discover(root, firmware, true)
			die(err)
			defer withCleanup(t.cleanup)()
			inspect(t).print(verbose)
		},
	}
	cmd.Flags().StringVar(&root, "root", "/", "root of the target OS to inspect (rescue-initramfs use)")
	cmd.Flags().StringVar(&firmware, "firmware", "", "override firmware detection (\"uefi\"/\"bios\") - only needed when --root isn't a live, booted system (see install's own --help)")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show the full per-feature Pool features list instead of just the active/enabled counts")
	return cmd
}

// --- verify ----------------------------------------------------------

// readInstalledKernelInitrd returns the ACTUAL, currently-installed
// kernel/initrd bytes and the cmdline.Info (version/buildstamp) they
// were installed with - real fresh reads off disk, independent of
// whatever inspect() may or may not have fast-pathed via metadata, for
// callers (verify's own hash/--deep check, verify --repair) that need
// the genuine bytes regardless of what's already cached anywhere.
func readInstalledKernelInitrd(t *target) (kernelBytes, initrdBytes []byte, info cmdline.Info, err error) {
	if t.uefi {
		rel, err := uefiboot.LoaderPath(t.arch)
		if err != nil {
			return nil, nil, cmdline.Info{}, err
		}
		loaderFullPath := filepath.Join(t.mountpoint, rel)
		info, err = cmdline.Read(loaderFullPath)
		if err != nil {
			return nil, nil, cmdline.Info{}, err
		}
		kernelBytes, err = cmdline.ReadSection(loaderFullPath, ".linux")
		if err != nil {
			return nil, nil, cmdline.Info{}, err
		}
		initrdBytes, err = cmdline.ReadSection(loaderFullPath, ".initrd")
		if err != nil {
			return nil, nil, cmdline.Info{}, err
		}
		return kernelBytes, initrdBytes, info, nil
	}
	kernelBytes, err = os.ReadFile(filepath.Join(t.mountpoint, layout.KernelFile))
	if err != nil {
		return nil, nil, cmdline.Info{}, err
	}
	initrdBytes, err = os.ReadFile(filepath.Join(t.mountpoint, layout.InitrdFile))
	if err != nil {
		return nil, nil, cmdline.Info{}, err
	}
	cmdlineBytes, err := os.ReadFile(filepath.Join(t.mountpoint, layout.CmdlineFile))
	if err != nil {
		return nil, nil, cmdline.Info{}, err
	}
	return kernelBytes, initrdBytes, cmdline.ParseText(cmdlineBytes), nil
}

// verifyMetadata implements verify's own metadata checking, per
// internal/metadata's own design:
//
//   - MISSING (no file at all): not itself a failure - an older
//     installation, or one from before this feature existed. --repair
//     reconstructs it from the real, current payload (this IS the safe
//     case: nothing existing is being overwritten or second-guessed).
//   - PRESENT BUT UNDECODABLE (the file exists but is corrupt/malformed
//   - not simply a hash mismatch): a hard failure, and --repair does
//     NOT touch it - unlike "missing", this is evidence something is
//     already wrong, and silently replacing it would hide that.
//   - PRESENT AND DECODES, but its recorded hash(es) don't match the
//     REAL installed payload: a hard failure, always, regardless of
//     --repair - "something changed" must never be quietly blessed by
//     regenerating a manifest that now agrees with the changed bytes.
//   - PRESENT, DECODES, HASHES MATCH: with --deep, additionally re-
//     derives the real kernel/OpenZFS versions from the payload and
//     compares them against the manifest's own recorded values - the
//     strongest check, proving the manifest doesn't just match the raw
//     bytes' hash but that the DECODED content truly matches too.
func verifyMetadata(t *target, deep, repair bool) []error {
	var errs []error

	m, present, decodeErr := tryReadMetadata(t.mountpoint)
	if decodeErr != nil {
		return []error{fmt.Errorf("metadata: %w (not auto-repaired - a corrupt manifest is evidence something is already wrong, not something safe to silently regenerate)", decodeErr)}
	}

	kernelBytes, initrdBytes, info, readErr := readInstalledKernelInitrd(t)
	if readErr != nil {
		// The real payload itself couldn't even be read - report that
		// plainly rather than trying to say anything about metadata at
		// all (nothing to check it against).
		return []error{fmt.Errorf("metadata: could not read the installed kernel/initrd to check against: %w", readErr)}
	}

	if !present {
		if !repair {
			return nil // MISSING is not itself a failure - see this function's own doc comment.
		}
		// Routed through writeMetadata (not a second, separate
		// deepMetadataFor+Encode+WriteFile here) so --repair gets the
		// exact same round-trip-through-Decode validation the
		// install/update path already does (F8, unidoc-alip's PR #5
		// review) - one writer, not two independently-maintained ones
		// that could drift.
		if err := writeMetadata(t.mountpoint, t.arch, info.Version, info.BuildStamp, kernelBytes, initrdBytes); err != nil {
			return []error{fmt.Errorf("metadata --repair: %w", err)}
		}
		fmt.Println("metadata --repair: reconstructed and wrote a new manifest from the real, current payload")
		return nil
	}

	if !metadata.VerifyHash(kernelBytes, m.KernelSHA256) {
		errs = append(errs, fmt.Errorf("metadata: recorded kernel_sha256 does not match the actual installed kernel - possible tampering, corruption, or a partial/interrupted update"))
	}
	if !metadata.VerifyHash(initrdBytes, m.InitrdSHA256) {
		errs = append(errs, fmt.Errorf("metadata: recorded initrd_sha256 does not match the actual installed initrd - possible tampering, corruption, or a partial/interrupted update"))
	}
	// Identity fields, bound against the installed artifact's OWN
	// cmdline (info, from readInstalledKernelInitrd - an independent
	// source from the manifest itself) - not just the two hashes. A
	// follow-up review found these were never checked at all: a
	// manifest with byte-correct hashes (nothing touched the real
	// kernel/initrd) but a hand-edited or stale version/buildstamp/arch
	// field passed both the plain and --deep checks silently, despite
	// Manifest's own doc comment on BuildStamp explicitly stating "the
	// two are required to agree." Runs in ORDINARY verify, not only
	// --deep - unlike re-deriving Kernel/OpenZFS (real decompression),
	// this costs nothing: info is already in hand from the read above.
	if m.Version != info.Version {
		errs = append(errs, fmt.Errorf("metadata: recorded version %q does not match the installed artifact's own cmdline version %q", m.Version, info.Version))
	}
	if m.BuildStamp != info.BuildStamp {
		errs = append(errs, fmt.Errorf("metadata: recorded buildstamp %q does not match the installed artifact's own cmdline buildstamp %q", m.BuildStamp, info.BuildStamp))
	}
	if m.Arch != t.arch {
		errs = append(errs, fmt.Errorf("metadata: recorded arch %q does not match this target's own arch %q", m.Arch, t.arch))
	}
	if len(errs) > 0 {
		// Never repair a hash mismatch, even if --repair was passed -
		// see this function's own doc comment: this is "something
		// changed", not "something missing".
		return errs
	}

	if deep {
		fresh, err := deepMetadataFor(t.arch, info.Version, info.BuildStamp, kernelBytes, initrdBytes)
		if err != nil {
			return []error{fmt.Errorf("metadata --deep: deep-inspecting the real payload failed: %w", err)}
		}
		if fresh.Kernel != m.Kernel {
			errs = append(errs, fmt.Errorf("metadata --deep: recorded kernel version %q does not match what the actual payload decodes to (%q)", m.Kernel, fresh.Kernel))
		}
		if fresh.OpenZFS != m.OpenZFS {
			errs = append(errs, fmt.Errorf("metadata --deep: recorded OpenZFS version %q does not match what the actual payload decodes to (%q)", m.OpenZFS, fresh.OpenZFS))
		}
	}
	return errs
}

// verifyMountReadonly is the readonly argument verify passes to
// discover - false exactly when --repair was given, because --repair
// is the one verify mode that genuinely WRITES (the reconstructed
// manifest, via verifyMetadata's own espconfig.WriteFile).
//
// A full source audit found verify passed a hardcoded `true` here, so
// --repair could only ever have worked on a machine where the ESP
// happened to be mounted read-write ALREADY: bootenv.MountESP mounts
// with unix.MS_RDONLY whenever readonly is true and it has to do the
// mount itself, and every write onto that mount then fails EROFS. The
// case that broke is precisely the one internal/metadata's own doc
// comment names as the documented recovery path - a rescue initramfs
// or an --root invocation, where the ESP is exactly NOT already
// mounted.
//
// Pulled out as a pure function rather than inlined as `!repair` for
// the same reason bootCompatVerdict/appendCompatErr/checkUpdateEligible
// above are: the Run closure itself calls discover() and die(), neither
// of which a unit test can exercise in a sandbox (no real ESP, and
// os.Exit would take the test process with it), so the decision this
// bug was actually IN has to be reachable on its own to be regression-
// tested at all.
//
// Consequence worth stating plainly: `verify --repair` against an ESP
// that is already mounted read-only now fails up front in MountESP
// ("remount it read-write first") instead of running the read-only
// checks and only then failing at the write. That is the fail-closed
// direction - the operator asked for a mutating operation this machine
// cannot currently perform, and finding that out before rather than
// after is strictly better - but it IS a behavior change for that one
// combination. Plain `verify` (no --repair) is completely unaffected
// and still mounts read-only, as it must.
func verifyMountReadonly(repair bool) bool {
	return !repair
}

func newVerifyCmd() *cobra.Command {
	var root, firmware string
	var efiFile, stage1File, stage2File, kernelFile, initrdFile, cmdlineFile string
	var verbose, deep, repair bool
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "read-only check of this machine's own alpine-zfsboot installation (no changes made)",
		Long: `verify is the read-only truth source install/update/status all build on.
With no flags, it checks that every expected artifact is present and
parseable. Passing one or more --*-file flags additionally byte-compares
the INSTALLED artifact against that exact reference file - the strong
check install/update already perform on themselves immediately after
writing (see internal/biosboot/internal/uefiboot's own Verify* calls),
available here standalone too, e.g. to re-confirm nothing drifted after
a later step like a partition-table reread or a reboot.`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			t, err := discover(root, firmware, verifyMountReadonly(repair))
			die(err)
			defer withCleanup(t.cleanup)()

			r := inspect(t)
			errs := appendCompatErr(r.errs(), r)
			metaErrs := verifyMetadata(t, deep, repair)
			r.metadataVerified = len(metaErrs) == 0
			errs = append(errs, metaErrs...)

			if t.uefi {
				if efiFile != "" {
					want, err := os.ReadFile(efiFile)
					die(err)
					if err := uefiboot.VerifyLoader(t.mountpoint, t.arch, want); err != nil {
						errs = append(errs, err)
					}
				}
			} else {
				if stage1File != "" {
					want, err := os.ReadFile(stage1File)
					die(err)
					if err := biosboot.VerifyStage1(t.disk, want); err != nil {
						errs = append(errs, err)
					}
				}
				if stage2File != "" {
					want, err := os.ReadFile(stage2File)
					die(err)
					if err := biosboot.VerifyStage2(t.disk, want); err != nil {
						errs = append(errs, err)
					}
				}
				// cmd.MarkFlagsRequiredTogether below guarantees these
				// three are either all empty or all set by the time
				// this runs (F12, unidoc-alip's PR #5 review) - no
				// partial-set case to special-case here anymore.
				if kernelFile != "" {
					kernel, err := os.ReadFile(kernelFile)
					die(err)
					initrd, err := os.ReadFile(initrdFile)
					die(err)
					cmdlineBytes, err := os.ReadFile(cmdlineFile)
					die(err)
					if err := espconfig.VerifyPayload(t.mountpoint, kernel, initrd, cmdlineBytes); err != nil {
						errs = append(errs, err)
					}
				}
			}

			r.print(verbose)
			if len(errs) == 0 {
				fmt.Println("\nverify: OK")
				return
			}
			fmt.Printf("\nverify: %d problem(s) found:\n", len(errs))
			for _, e := range errs {
				fmt.Println("  -", e)
			}
			os.Exit(1)
		},
	}
	cmd.Flags().StringVar(&root, "root", "/", "root of the target OS to verify (rescue-initramfs use)")
	cmd.Flags().StringVar(&firmware, "firmware", "", "override firmware detection (\"uefi\"/\"bios\") - only needed when --root isn't a live, booted system (see install's own --help)")
	cmd.Flags().StringVar(&efiFile, "efi-file", "", "UEFI: byte-compare the installed loader against this exact local file")
	cmd.Flags().StringVar(&stage1File, "stage1-file", "", "BIOS: byte-compare the installed stage1 against this exact local file")
	cmd.Flags().StringVar(&stage2File, "stage2-file", "", "BIOS: byte-compare the installed stage2 against this exact local file")
	cmd.Flags().StringVar(&kernelFile, "kernel-file", "", "BIOS: byte-compare the installed kernel (needs all three of kernel/initrd/cmdline together)")
	cmd.Flags().StringVar(&initrdFile, "initrd-file", "", "BIOS: byte-compare the installed initrd (needs all three of kernel/initrd/cmdline together)")
	cmd.Flags().StringVar(&cmdlineFile, "cmdline-file", "", "BIOS: byte-compare the installed cmdline (needs all three of kernel/initrd/cmdline together)")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show the full per-feature Pool features list instead of just the active/enabled counts")
	cmd.Flags().BoolVar(&deep, "deep", false, "re-derive kernel/OpenZFS versions from the real installed payload (full decompression) and require them to match the metadata manifest's own recorded values - the strongest check, beyond the always-on hash comparison")
	cmd.Flags().BoolVar(&repair, "repair", false, "if the metadata manifest is simply MISSING, safely reconstruct it from the real, current payload; never touches a manifest that exists but doesn't match (that fails instead - see this command's own metadata checking)")
	// F12 (unidoc-alip's PR #5 review): the three flags' own --help text
	// already documented "needs all three ... together", but nothing
	// enforced it - espconfig.VerifyPayload checks all three as one
	// unit, and the Run closure above only ever surfaces ITS error when
	// all three were given, so `verify --kernel-file X` alone silently
	// compared nothing at all and still printed "verify: OK". Cobra's
	// own flag-group validation enforces the contract the help text
	// already promised, refusing to even start the command with a
	// clear usage error instead of a silent no-op.
	cmd.MarkFlagsRequiredTogether("kernel-file", "initrd-file", "cmdline-file")
	return cmd
}

// --- update ----------------------------------------------------------

func newUpdateCmd() *cobra.Command {
	var root, firmware string
	var yes bool
	var efiFile, efiURL string
	var stage1File, stage1URL, stage2File, stage2URL string
	var kernelFile, kernelURL, initrdFile, initrdURL, cmdlineFile, cmdlineURL string

	cmd := &cobra.Command{
		Use:   "update",
		Short: "fetch and install the latest alpine-zfsboot build over this machine's own installation",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			t, err := discover(root, firmware, false)
			die(err)
			defer withCleanup(t.cleanup)()

			workdir, err := os.MkdirTemp("", "alpine-zfsboot-update-*")
			die(err)
			defer os.RemoveAll(workdir)

			if t.uefi {
				updateUEFI(t, workdir, yes, release.Source{File: efiFile, URL: efiURL})
				return
			}
			updateBIOS(t, workdir, yes, release.BIOSSources{
				Stage1:  release.Source{File: stage1File, URL: stage1URL},
				Stage2:  release.Source{File: stage2File, URL: stage2URL},
				Kernel:  release.Source{File: kernelFile, URL: kernelURL},
				Initrd:  release.Source{File: initrdFile, URL: initrdURL},
				Cmdline: release.Source{File: cmdlineFile, URL: cmdlineURL},
			})
		},
	}
	cmd.Flags().StringVar(&root, "root", "/", "root of the target OS to update (rescue-initramfs use)")
	cmd.Flags().StringVar(&firmware, "firmware", "", "override firmware detection (\"uefi\"/\"bios\") - only needed when --root isn't a live, booted system (see install's own --help)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the overwrite confirmation prompt")
	cmd.Flags().StringVar(&efiFile, "efi-file", "", "UEFI: install this local .EFI file instead of fetching the latest release")
	cmd.Flags().StringVar(&efiURL, "efi-url", "", "UEFI: fetch the .EFI from this URL instead of the latest release (must be minisign-signed: <url>.minisig must exist and verify)")
	cmd.Flags().StringVar(&stage1File, "stage1-file", "", "BIOS: local stage1.bin instead of the latest release")
	cmd.Flags().StringVar(&stage1URL, "stage1-url", "", "BIOS: fetch stage1.bin from this URL instead of the latest release (must be minisign-signed: <url>.minisig must exist and verify)")
	cmd.Flags().StringVar(&stage2File, "stage2-file", "", "BIOS: local stage2.bin instead of the latest release")
	cmd.Flags().StringVar(&stage2URL, "stage2-url", "", "BIOS: fetch stage2.bin from this URL instead of the latest release (must be minisign-signed: <url>.minisig must exist and verify)")
	cmd.Flags().StringVar(&kernelFile, "kernel-file", "", "BIOS: local kernel instead of the latest release")
	cmd.Flags().StringVar(&kernelURL, "kernel-url", "", "BIOS: fetch the kernel from this URL instead of the latest release (must be minisign-signed: <url>.minisig must exist and verify)")
	cmd.Flags().StringVar(&initrdFile, "initrd-file", "", "BIOS: local initrd instead of the latest release")
	cmd.Flags().StringVar(&initrdURL, "initrd-url", "", "BIOS: fetch the initrd from this URL instead of the latest release (must be minisign-signed: <url>.minisig must exist and verify)")
	cmd.Flags().StringVar(&cmdlineFile, "cmdline-file", "", "BIOS: local cmdline.txt instead of the latest release")
	cmd.Flags().StringVar(&cmdlineURL, "cmdline-url", "", "BIOS: fetch cmdline.txt from this URL instead of the latest release (must be minisign-signed: <url>.minisig must exist and verify)")
	return cmd
}

func updateUEFI(t *target, workdir string, yes bool, src release.Source) {
	rel, err := uefiboot.LoaderPath(t.arch)
	die(err)
	target := filepath.Join(t.mountpoint, rel)

	path, err := release.ResolveEFI(src, t.arch, workdir)
	die(err)

	local, err := cmdline.Read(target)
	die(err)
	latest, err := cmdline.Read(path)
	die(err)
	die(uefiboot.CheckArchMatch(target, local, latest))

	if latest.BuildStamp <= local.BuildStamp {
		fmt.Printf("%s is already up to date (%s)\n", rel, cmdline.HumanVersion(local.BuildStamp))
		return
	}
	fmt.Printf("%s: local %s -> latest %s\n", rel, cmdline.HumanVersion(local.BuildStamp), cmdline.HumanVersion(latest.BuildStamp))
	if !yes && !confirm(fmt.Sprintf("overwrite %s with the latest build?", rel)) {
		fmt.Println("not updated")
		return
	}

	efiBytes, err := os.ReadFile(path)
	die(err)
	backedUp, err := writeUEFIGenerationWithRollback(t.mountpoint, t.arch, efiBytes, path, latest)
	die(err)

	if backedUp {
		fmt.Printf("%s updated to %s (previous build kept at %s.previous)\n", rel, cmdline.HumanVersion(latest.BuildStamp), rel)
	} else {
		fmt.Printf("%s updated to %s (no local rollback copy - could not preserve the previous build)\n", rel, cmdline.HumanVersion(latest.BuildStamp))
	}
}

// writeBIOSStagesWithRollback writes stage1 then stage2 to disk (via
// biosboot.WriteStage1/WriteStage2, each already verified against real
// hardware - see that package's own doc comment), rolling back whatever
// was already written if a LATER step fails, so a failure here never
// leaves disk in a state that never existed - part of the old build,
// part of the new one. Shared by installBIOS and updateBIOS - the exact
// same all-or-nothing contract either way, not two parallel copies of
// it (per this project's own "shared primitives" requirement).
//
// A full source audit found this rollback capability did not actually
// exist despite biosboot.WriteStage1/WriteStage2 being explicitly
// designed to support it: both functions return the region's own
// previous content specifically so a caller can restore it, and every
// real call site discarded that return value outright
// (`if _, err := biosboot.WriteStage1(...)`) - so a failure between
// writing stage1 and finishing stage2 (WriteStage2 zeroes its ENTIRE
// 32 KiB extent before writing the new content - a failed write or
// sync after that point destroys the old stage2 with nothing left to
// restore from) left a disk with a NEW stage1 and a destroyed/stale
// stage2, an architecturally promised but never-implemented safety
// property.
// Returns an error rather than calling die() itself - deliberately, so
// the rollback behavior on each failure branch is unit-testable without
// exercising die()'s own os.Exit (which would kill the test process
// along with everything it's trying to prove). Both real call sites
// just wrap this in die(...).
func writeBIOSStagesWithRollback(disk string, stage1, stage2 []byte) error {
	stage1Prev, err := biosboot.WriteStage1(disk, stage1)
	if err != nil {
		return fmt.Errorf("writing stage1: %w", err)
	}
	if err := biosboot.VerifyStage1(disk, stage1); err != nil {
		rollbackStage1(disk, stage1Prev)
		return fmt.Errorf("verifying stage1 after write: %w", err)
	}

	stage2Prev, err := biosboot.WriteStage2(disk, stage2)
	if err != nil {
		rollbackStage1(disk, stage1Prev)
		// F6 (unidoc-alip's PR #5 review): WriteStage2 zeroes its
		// entire 32KiB extent before writing the new content, so a
		// write/sync/readback failure AFTER that point returns a
		// non-nil `stage2Prev` - the exact real pre-write bytes this
		// function's own doc comment says a caller should restore.
		// This branch used to drop it, leaving a rolled-back stage1
		// paired with a zeroed-or-partial stage2 (unbootable) while
		// stderr still said "restored the previous stage1" - exactly
		// the mixed-generation state this whole function exists to
		// prevent. rollbackStage2 is already a no-op on a nil
		// previous (the earlier, pre-zeroing failure branches), so
		// this is safe to call unconditionally on every error here.
		rollbackStage2(disk, stage2Prev)
		return fmt.Errorf("writing stage2: %w", err)
	}
	if err := biosboot.VerifyStage2(disk, stage2); err != nil {
		rollbackStage1(disk, stage1Prev)
		rollbackStage2(disk, stage2Prev)
		return fmt.Errorf("verifying stage2 after write: %w", err)
	}
	return nil
}

// rollbackStage1/rollbackStage2 - best-effort restore, called only while
// already unwinding toward die() after a real failure. previous == nil
// means WriteStage1/WriteStage2's own failure happened before anything
// was actually mutated (see each function's own doc comment on exactly
// which failure branches return a nil previous) - nothing to restore,
// not an error. A rollback attempt that itself fails is reported
// distinctly and loudly: at that point the disk may be in a genuinely
// broken state this tool cannot fix, which is a categorically more
// serious situation than the original failure alone and must not be
// swallowed into the same, calmer "writing stageN: ..." message die()
// already prints.
func rollbackStage1(disk string, previous []byte) {
	if previous == nil {
		return
	}
	if _, err := biosboot.WriteStage1(disk, previous); err != nil {
		fmt.Fprintf(os.Stderr, "alpine-zfsboot: CRITICAL: restoring the previous stage1 after a failed write ALSO failed: %v - %s's stage1 may now be corrupt, do not reboot this disk without investigating further\n", err, disk)
		return
	}
	fmt.Fprintln(os.Stderr, "alpine-zfsboot: restored the previous stage1 after a failed write")
}

func rollbackStage2(disk string, previous []byte) {
	if previous == nil {
		return
	}
	if _, err := biosboot.WriteStage2(disk, previous); err != nil {
		fmt.Fprintf(os.Stderr, "alpine-zfsboot: CRITICAL: restoring the previous stage2 after a failed write ALSO failed: %v - %s's stage2 may now be corrupt, do not reboot this disk without investigating further\n", err, disk)
		return
	}
	fmt.Fprintln(os.Stderr, "alpine-zfsboot: restored the previous stage2 after a failed write")
}

// payloadBackup captures whichever of KERNEL/INITRD/CMDLINE/METADATA
// already existed on the ESP before a write attempt - a byte slice for
// a file that existed, nil for one that didn't (a fresh install's ESP
// has none of these yet; a real update's ESP always has at least the
// first three - METADATA only once this feature has run at least
// once). Read BEFORE any write starts, so a rollback always has the
// true pre-write state to restore to, not a guess made after
// something has already changed - and, per backupPayload's own comment
// below, nil means PROVABLY absent (os.IsNotExist and nothing else),
// never merely "could not be read", because rollbackPayload acts on
// nil by DELETING.
type payloadBackup struct {
	kernel, initrd, cmdline, metadata []byte
}

// A full source audit found this function used to swallow EVERY read
// error into a nil entry - and rollbackPayload below treats nil as
// "this file did not exist before, so REMOVE it". The two together
// turned a transient read failure into data loss: on a machine with a
// healthy installation and a disk throwing intermittent EIO (or an ESP
// whose FAT is going bad - exactly the conditions under which someone
// reaches for `update` in the first place), a failed read of the
// existing KERNEL during backup made backup.kernel nil; if the update
// then failed for any reason at all, rollback DELETED the working
// kernel that was there, turning a cleanly-recoverable failed update
// into an ESP with no bootable payload. That is precisely the failure
// class writePayloadWithRollback exists to close, reintroduced through
// the back door of its own backup step.
//
// So: os.IsNotExist is the ONE error that legitimately means nil ("a
// fresh install's ESP has none of these yet"). Every other error means
// this function does not know the true pre-write state, and a rollback
// built on a guess is worse than no rollback at all - it is reported,
// and writePayloadWithRollback refuses BEFORE its first write, the same
// fail-before-mutating shape as CheckStage2ExtentFree/
// checkUpdateEligible above.
func backupPayload(mountpoint string) (payloadBackup, error) {
	var b payloadBackup
	for _, f := range []struct {
		rel string
		dst *[]byte
	}{
		{layout.KernelFile, &b.kernel},
		{layout.InitrdFile, &b.initrd},
		{layout.CmdlineFile, &b.cmdline},
		{layout.MetadataFile, &b.metadata},
	} {
		content, err := os.ReadFile(filepath.Join(mountpoint, f.rel))
		if err != nil {
			if os.IsNotExist(err) {
				continue // genuinely absent - nil is the truth here
			}
			return payloadBackup{}, fmt.Errorf("reading the existing %s to back it up before writing: %w (refusing to continue: without a trustworthy copy of the current payload, a rollback could DELETE a file that is actually still there)", f.rel, err)
		}
		*f.dst = content
	}
	return b, nil
}

// rollbackPayload restores mountpoint's KERNEL/INITRD/CMDLINE/METADATA
// to whatever backup captured, ALL FOUR regardless of which ones this
// attempt actually got to changing - the whole point is that the ESP
// ends up provably either the full new generation or the full old
// one, never a mix (see writePayloadWithRollback's own comment for
// why a mix is the actual failure mode this closes). A nil entry (the
// file didn't exist before this attempt) is removed rather than
// restored, so a failed fresh install leaves a clean, empty ESP
// instead of a stray half-written file. Best-effort and loud on its
// own failure, same shape as rollbackStage1/rollbackStage2 above -
// restoring the previous state after a failed write is itself a
// write that can fail, and swallowing that silently would be worse
// than the original failure.
func rollbackPayload(mountpoint string, backup payloadBackup) {
	// ok tracks whether every restore() call actually succeeded (F22,
	// unidoc-alip's PR #5 review) - previously the "restored the
	// previous boot payload" success line printed UNCONDITIONALLY
	// after all four calls, even on a run where one or more of them
	// had just printed their own CRITICAL failure - an operator could
	// see both "CRITICAL: ... ALSO failed ... do not reboot" and
	// "restored the previous boot payload ... " back to back, the
	// second directly undercutting the seriousness of the first.
	ok := true
	restore := func(rel string, want []byte) {
		if want == nil {
			path := filepath.Join(mountpoint, rel)
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "alpine-zfsboot: CRITICAL: removing %s while restoring the previous boot payload ALSO failed: %v - the ESP may now hold a mismatched KERNEL/INITRD/CMDLINE set, do not reboot without investigating further\n", path, err)
				ok = false
			}
			return
		}
		if err := espconfig.WriteFile(mountpoint, rel, want, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "alpine-zfsboot: CRITICAL: restoring the previous %s while restoring the boot payload ALSO failed: %v - the ESP may now hold a mismatched KERNEL/INITRD/CMDLINE set, do not reboot without investigating further\n", rel, err)
			ok = false
		}
	}
	restore(layout.KernelFile, backup.kernel)
	restore(layout.InitrdFile, backup.initrd)
	restore(layout.CmdlineFile, backup.cmdline)
	restore(layout.MetadataFile, backup.metadata)
	if ok {
		fmt.Fprintln(os.Stderr, "alpine-zfsboot: restored the previous boot payload (KERNEL/INITRD/CMDLINE/METADATA) after a failed write")
	}
}

// writePayloadWithRollback closes a real gap a follow-up review found:
// espconfig.WritePayload already makes each of KERNEL/INITRD/CMDLINE
// individually durable (WriteFile's own temp-write-fsync-rename-
// fsync-dir sequence), but writes the three files ONE AT A TIME with
// no rollback across them - a failure partway (a full FAT filesystem
// on the second file, an I/O error on the third, or VerifyPayload
// catching a mismatch after all three writes "succeeded") used to
// leave the ESP holding a MIX of new and old files: e.g. a new KERNEL
// paired with the OLD INITRD/CMDLINE - individually valid files that
// together are not the boot generation either one was ever tested
// as, and can be silently unbootable (a new kernel expecting modules
// or a cmdline the old initrd/cmdline don't provide) with no error at
// update time - the worst possible place for this kind of gap, since
// it only surfaces on the NEXT reboot, by which point the machine may
// be unattended. Returns an error rather than calling die() itself,
// same reasoning as writeBIOSStagesWithRollback above - testable
// without os.Exit. Both real call sites just wrap this in die(...).
// writePayloadWithRollback writes KERNEL/INITRD/CMDLINE, then generates
// and writes METADATA (see internal/metadata's own doc comment) from
// those exact same just-verified bytes - all FOUR files are one
// generation, one atomic unit: any failure at any step, including
// metadata generation/write, rolls ALL of them back to their pre-call
// state (see rollbackPayload's own comment). A new installation or a
// successful update therefore NEVER ends up with a good payload but no
// metadata - the two other, deliberately separate ways a manifest can
// still end up missing (an installation from before this feature
// existed; a metadata write that failed BEFORE this atomicity was
// added) are handled by `verify --repair`, not by this function.
func writePayloadWithRollback(mountpoint, arch, version, buildStamp string, kernel, initrd, cmdline []byte) error {
	backup, err := backupPayload(mountpoint)
	if err != nil {
		// Before ANY write - see backupPayload's own comment.
		return err
	}
	if err := espconfig.WritePayload(mountpoint, kernel, initrd, cmdline); err != nil {
		rollbackPayload(mountpoint, backup)
		return fmt.Errorf("writing boot payload: %w", err)
	}
	if err := espconfig.VerifyPayload(mountpoint, kernel, initrd, cmdline); err != nil {
		rollbackPayload(mountpoint, backup)
		return fmt.Errorf("verifying boot payload after write: %w", err)
	}
	if err := writeMetadata(mountpoint, arch, version, buildStamp, kernel, initrd); err != nil {
		rollbackPayload(mountpoint, backup)
		return fmt.Errorf("generating/writing metadata manifest: %w", err)
	}
	return nil
}

// writeMetadata deep-inspects kernelBytes/initrdBytes and writes the
// resulting manifest to mountpoint - see internal/metadata's own doc
// comment for the full design. version/buildStamp are the same values
// already embedded in the cmdline this exact build carries (cmdline.
// Info's own Version/BuildStamp) - one source, not re-derived or
// re-guessed. Callers decide what a failure here means for them:
// writePayloadWithRollback (BIOS) treats it as fatal to the whole
// generation; UEFI's own call sites (no equivalent atomic-rollback
// primitive exists for the single .EFI file today - see uefiboot's own
// WriteLoader/.previous backup, which is a passive recovery copy, not
// an active rollback) report it as a hard error pointing at
// `verify --repair` instead.
func writeMetadata(mountpoint, arch, version, buildStamp string, kernelBytes, initrdBytes []byte) error {
	m, err := deepMetadataFor(arch, version, buildStamp, kernelBytes, initrdBytes)
	if err != nil {
		return err
	}
	enc, err := checkMetadataEncodable(m)
	if err != nil {
		return err
	}
	return espconfig.WriteFile(mountpoint, layout.MetadataFile, enc, 0o644)
}

// checkMetadataEncodable is the Encode-then-Decode round trip shared by
// writeMetadata (right before its own write) and preflightBIOSMetadata
// (before ANY disk write at all, BIOS install/update - see that
// function's own comment for why it needs to run this early). Pure,
// no I/O - m is already fully assembled by the caller (deepMetadataFor
// or preflightBIOSMetadata's own equivalent call to it).
//
// F8 (unidoc-alip's PR #5 review): metadata.Decode requires
// version/buildstamp non-empty, but metadata.Encode validates nothing,
// and the BIOS install/update path takes both values from
// cmdline.ParseText, which does not require them either - a custom
// --cmdline-file or a hand-edited CMDLINE missing either field used to
// write a manifest that decoded cleanly at the moment of writing
// (Encode doesn't check) but that every LATER `verify` then rejected
// with "missing required field(s)" - and, because it EXISTS, `verify
// --repair` correctly refuses to touch it (see verifyMetadata's own
// doc comment on why a present-but-broken manifest is never auto-
// repaired) - leaving the installation stuck failing verify until an
// operator deletes the file by hand. A real round-trip through Decode
// before ever writing catches this at the moment it's actually
// preventable.
func checkMetadataEncodable(m metadata.Manifest) ([]byte, error) {
	enc := metadata.Encode(m)
	if _, err := metadata.Decode(enc); err != nil {
		return nil, fmt.Errorf("refusing to write a metadata manifest that would not decode: %w", err)
	}
	return enc, nil
}

// preflightBIOSMetadata runs the exact same deepMetadataFor +
// checkMetadataEncodable round trip writeMetadata itself will do
// later, but here purely to fail BEFORE writeBIOSStagesWithRollback
// ever touches the disk (F8, unidoc-alip's PR #5 follow-up review).
//
// Without this, installBIOS/updateBIOS called writeBIOSStagesWithRollback
// first and only reached writeMetadata's own round-trip check several
// steps later, inside writePayloadWithRollback. writePayloadWithRollback
// correctly rolls back its OWN write (the KERNEL/INITRD/CMDLINE
// payload) on a writeMetadata failure - but it has no way to also
// undo a stage1/stage2 write that already succeeded and returned
// before it was ever called. A cmdline/kernel/initrd combination that
// fails this round trip (the same "custom --cmdline-file missing
// version/buildstamp" case writeMetadata's own comment describes)
// left the disk with the NEW build's stage1/stage2 paired with the
// OLD build's still-rolled-back KERNEL/INITRD/CMDLINE/METADATA - new
// boot loader stages, old payload, a real mismatched-generation state
// no single write actually caused but the ORDERING did. Running the
// identical check here, before any write at all, means a manifest
// that will not decode is refused up front, exactly like
// bootenv.CheckStage2ExtentFree's own "fail before ANY mutation"
// preflight just above this call at each of its two real call sites.
func preflightBIOSMetadata(arch string, newCmdline cmdline.Info, kernel, initrd []byte) error {
	m, err := deepMetadataFor(arch, newCmdline.Version, newCmdline.BuildStamp, kernel, initrd)
	if err != nil {
		return err
	}
	_, err = checkMetadataEncodable(m)
	return err
}

// tryReadMetadata reads and decodes internal/layout.MetadataFile from
// mountpoint - the ONE place in this whole codebase that reads it for
// display purposes (status/verify's own fast path), read-only, never
// writing anything back (see internal/metadata's own "read-only
// commands stay read-only" requirement). Three distinct outcomes,
// deliberately not collapsed into a single bool: present=true (file
// existed and decoded cleanly, err always nil) is the fast-path case;
// present=false with err=nil means the file simply doesn't exist (an
// older installation, or one from before this feature existed) - not
// a problem, just nothing to use; present=false with err!=nil means
// the file EXISTED (or existing couldn't be ruled out) but couldn't be
// read or decoded - a real, reportable problem a caller should
// surface distinctly from "simply absent" (see report's own
// metadataErr field and print()'s own "Metadata:" line).
//
// os.IsNotExist(readErr), NOT "any read error", is what maps to
// present=false/err=nil (F7, unidoc-alip's PR #5 review) - EIO from a
// bad sector, EACCES, or the file's own path being a directory
// (ISDIR) are real problems distinct from "there was never a
// manifest here", and collapsing them into plain "absent" broke the
// exact property verifyMetadata's own doc comment promises for
// --repair: "never touches one that exists and disagrees". A manifest
// this process cannot even READ might be a mismatch it can no longer
// prove - treating that as safely-missing let --repair silently
// overwrite it (erasing whatever evidence a hash mismatch or
// corruption it might have recorded) instead of hard-failing the way
// an actually-undecodable-but-readable manifest already correctly
// does. verifyMetadata's own existing decodeErr!=nil check (this
// function's third return value) already hard-fails on exactly this
// shape - no caller-side change needed beyond fixing this function's
// own over-broad readErr handling.
func tryReadMetadata(mountpoint string) (m metadata.Manifest, present bool, err error) {
	raw, readErr := os.ReadFile(filepath.Join(mountpoint, layout.MetadataFile))
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return metadata.Manifest{}, false, nil
		}
		return metadata.Manifest{}, false, fmt.Errorf("metadata file exists but could not be read: %w", readErr)
	}
	m, decodeErr := metadata.Decode(raw)
	if decodeErr != nil {
		return metadata.Manifest{}, false, fmt.Errorf("metadata file exists but failed to decode: %w", decodeErr)
	}
	return m, true, nil
}

// deepMetadataFor builds a Manifest by ACTUALLY deep-inspecting
// kernelBytes/initrdBytes (internal/kernelinfo.VersionFromImage,
// internal/initrdinfo.ExtractOpenZFSVersionFromBytes) and hashing them
// - the one place this whole feature's own "derived from the exact
// final payload, not guessed" requirement is implemented, shared by
// writeMetadata (install/update) and runVerifyRepair (backfill) so
// both produce a manifest exactly the same way.
func deepMetadataFor(arch, version, buildStamp string, kernelBytes, initrdBytes []byte) (metadata.Manifest, error) {
	kv, err := kernelinfo.VersionFromImage(kernelBytes)
	if err != nil {
		return metadata.Manifest{}, fmt.Errorf("extracting kernel version: %w", err)
	}
	zv, err := initrdinfo.ExtractOpenZFSVersionFromBytes(initrdBytes)
	if err != nil {
		return metadata.Manifest{}, fmt.Errorf("extracting OpenZFS version: %w", err)
	}
	return metadata.Manifest{
		Version:      version,
		BuildStamp:   buildStamp,
		Arch:         arch,
		Kernel:       kv,
		OpenZFS:      zv,
		KernelSHA256: metadata.SHA256Hex(kernelBytes),
		InitrdSHA256: metadata.SHA256Hex(initrdBytes),
	}, nil
}

// writeUEFIMetadata extracts the just-written .EFI's own embedded
// .linux/.initrd PE sections (from efiPath - the local downloaded copy
// uefiboot.WriteLoader just wrote to the ESP; PE-section reads work
// identically against either copy since they're byte-identical, and
// efiPath is already open-able without a temp file) and writes the
// resulting metadata manifest. A plain error return - the CALLER
// (writeUEFIGenerationWithRollback) decides what a failure here means,
// same shape as writeMetadata's own doc comment describes for its BIOS
// callers.
func writeUEFIMetadata(mountpoint, arch, efiPath string, info cmdline.Info) error {
	kernelBytes, err := cmdline.ReadSection(efiPath, ".linux")
	if err != nil {
		return fmt.Errorf("writing metadata manifest: reading .linux section: %w", err)
	}
	initrdBytes, err := cmdline.ReadSection(efiPath, ".initrd")
	if err != nil {
		return fmt.Errorf("writing metadata manifest: reading .initrd section: %w", err)
	}
	if err := writeMetadata(mountpoint, arch, info.Version, info.BuildStamp, kernelBytes, initrdBytes); err != nil {
		return fmt.Errorf("writing metadata manifest: %w", err)
	}
	return nil
}

// uefiGenerationBackup captures the real, pre-write bytes of BOTH the
// UEFI loader and its metadata manifest - read BEFORE any write
// starts, mirroring BIOS's own payloadBackup (see that type's own doc
// comment) for the same reason: a rollback needs the true pre-write
// state, not a guess made after something has already changed.
//
// This closes a real gap a follow-up review found: UEFI's install/
// update previously had NO equivalent to BIOS's own generation-
// atomicity. WriteLoader could succeed and be verified, and ONLY THEN
// could metadata generation/write fail - leaving the ESP with a NEW
// loader but OLD (or, on a fresh install, no) metadata. `verify` would
// still have caught this via a hash mismatch, not silent corruption -
// but it violated this project's own "KERNEL+INITRD+CMDLINE+METADATA
// is one generation" invariant for the UEFI case specifically, which
// this type and writeUEFIGenerationWithRollback below now close the
// same way BIOS already did.
type uefiGenerationBackup struct {
	loader, metadata []byte // nil = didn't exist before this attempt
}

func backupUEFIGeneration(mountpoint, loaderRel string) (uefiGenerationBackup, error) {
	read := func(rel string) ([]byte, error) {
		b, err := os.ReadFile(filepath.Join(mountpoint, rel))
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		return b, nil
	}
	loader, err := read(loaderRel)
	if err != nil {
		return uefiGenerationBackup{}, fmt.Errorf("backing up the current UEFI loader: %w", err)
	}
	meta, err := read(layout.MetadataFile)
	if err != nil {
		return uefiGenerationBackup{}, fmt.Errorf("backing up the current metadata manifest: %w", err)
	}
	return uefiGenerationBackup{loader: loader, metadata: meta}, nil
}

// rollbackUEFIGeneration restores mountpoint's UEFI loader/METADATA to
// whatever backup captured, BOTH regardless of which one this attempt
// actually got to changing - the whole point is that the ESP ends up
// provably either the full new generation or the full old one, never a
// mix (same reasoning as BIOS's own rollbackPayload). A nil entry (the
// file didn't exist before this attempt) is removed rather than
// restored. Best-effort and loud on its own failure, same shape as
// rollbackPayload/rollbackStage1/rollbackStage2.
func rollbackUEFIGeneration(mountpoint, loaderRel string, backup uefiGenerationBackup) {
	// ok tracks whether every restore() call actually succeeded (F22,
	// unidoc-alip's PR #5 review) - same real gap, same fix, as
	// rollbackPayload's own identical BIOS-side pattern: the success
	// line used to print unconditionally even after a CRITICAL restore
	// failure was already printed for one of the two files here.
	ok := true
	restore := func(rel string, want []byte, what string) {
		path := filepath.Join(mountpoint, rel)
		if want == nil {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "alpine-zfsboot: CRITICAL: removing %s while rolling back a failed UEFI update ALSO failed: %v - the ESP may now hold a mismatched loader/metadata generation, do not reboot without investigating further\n", path, err)
				ok = false
			}
			return
		}
		// espconfig.WriteFile, not a raw write: the same temp-write-
		// fsync-rename-fsync-dir primitive already used for every other
		// atomic write in this project (BIOS's own KERNEL/INITRD/
		// CMDLINE/METADATA, and METADATA itself in the normal write
		// path) - one write primitive, not a second bespoke one for
		// this specific restore.
		if err := espconfig.WriteFile(mountpoint, rel, want, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "alpine-zfsboot: CRITICAL: restoring the previous %s while rolling back a failed UEFI update ALSO failed: %v - the ESP may now hold a mismatched loader/metadata generation, do not reboot without investigating further\n", what, err)
			ok = false
		}
	}
	restore(loaderRel, backup.loader, "UEFI loader")
	restore(layout.MetadataFile, backup.metadata, "metadata manifest")
	if ok {
		fmt.Fprintln(os.Stderr, "alpine-zfsboot: restored the previous UEFI loader/metadata generation after a failed write")
	}
}

// writeUEFIGenerationWithRollback is UEFI's own equivalent of BIOS's
// writePayloadWithRollback: writes the loader, verifies it, then
// generates and writes metadata from its own embedded .linux/.initrd
// sections - ALL as one rollback unit; any failure at any step
// restores both the loader and the metadata manifest to their pre-call
// state. backedUp reports whether WriteLoader's own separate,
// unrelated ".previous" manual-recovery copy was made - a passive,
// best-effort convenience kept for the existing "previous build kept
// at .previous" display message, orthogonal to (not a substitute for)
// the active rollback this function performs.
func writeUEFIGenerationWithRollback(mountpoint, arch string, efiBytes []byte, efiPath string, info cmdline.Info) (backedUp bool, err error) {
	rel, err := uefiboot.LoaderPath(arch)
	if err != nil {
		return false, err
	}
	backup, err := backupUEFIGeneration(mountpoint, rel)
	if err != nil {
		return false, fmt.Errorf("backing up the current UEFI generation: %w", err)
	}

	backedUp, err = uefiboot.WriteLoader(mountpoint, arch, efiBytes)
	if err != nil {
		rollbackUEFIGeneration(mountpoint, rel, backup)
		return backedUp, fmt.Errorf("writing the UEFI loader: %w", err)
	}
	if err := uefiboot.VerifyLoader(mountpoint, arch, efiBytes); err != nil {
		rollbackUEFIGeneration(mountpoint, rel, backup)
		return backedUp, fmt.Errorf("verifying the UEFI loader after write: %w", err)
	}
	if err := writeUEFIMetadata(mountpoint, arch, efiPath, info); err != nil {
		rollbackUEFIGeneration(mountpoint, rel, backup)
		return backedUp, err
	}
	return backedUp, nil
}

// checkUpdateEligible requires real evidence that disk already has
// alpine-zfsboot installed before update is allowed to write to it -
// "update" means exactly that, by definition. Returns an error rather
// than calling die() itself, same reasoning/pattern as
// writeBIOSStagesWithRollback: testable without os.Exit.
//
// A full source audit found this gap: disk (t.disk in updateBIOS) is
// derived from whatever bootenv.FindESP() selects, and BIOS (unlike
// UEFI) exposes no "which disk did this machine actually boot from"
// signal to cross-check against at all. FindESP()'s own ambiguity
// refusal only helps when the WRONG candidate (e.g. an alpine-zfsboot
// rescue USB left plugged in - it carries the exact marker files
// probeESPCandidate looks for) is visible ALONGSIDE the real one; it
// does nothing if the real ESP fails to probe for any reason and
// silently drops out of the candidate list, leaving the rogue USB as
// the sole "qualifying" one with nothing left to be ambiguous against.
// A disk that has never had alpine-zfsboot's own stage2 written to it
// (any real rescue/foreign USB) won't have this signature at LBA 34 -
// refusing here, before any write, is a concrete, disk-content-based
// check "install" doesn't need (it's explicitly for a not-yet-installed
// disk) but "update" can and should require.
// trimStage1Asset accepts a stage1 asset either as the real, only
// shape build.sh ever produces (layout.SectorSize bytes - a full MBR
// sector, 0x55AA boot signature and all - see bios/Makefile's own
// stage1.bin target, which hard-requires exactly 512) or as an
// already-trimmed layout.Stage1Bytes-byte boot-code-only slice (a
// custom --stage1-file someone pre-trimmed by hand), and returns
// exactly the layout.Stage1Bytes bytes biosboot.WriteStage1 itself
// writes.
//
// Found on real hardware (uniclus-01): install/update used to require
// the RAW asset file itself be exactly layout.Stage1Bytes (440) -
// which no real release asset from this project's own build.sh has
// EVER been (always 512, the full sector) - so `update`/`install`
// against a genuine, unmodified official release asset failed outright
// with "stage1 is 512 bytes, want exactly 440" on every real run. The
// shell installer (alpine-installer's own install_alpine_zfsboot_bios())
// has always handled this correctly via `dd bs=440 count=1` - only
// this Go port never replicated that trim step, going straight from
// "read the whole asset file" to "hand it to WriteStage1 unmodified"
// with no truncation in between. WriteStage1 itself is NOT the bug -
// its own 440-byte-only contract (never touching the disk's real
// partition table at bytes 440-511) is exactly right; this caller-side
// trim is what was missing.
func trimStage1Asset(raw []byte) ([]byte, error) {
	switch len(raw) {
	case layout.Stage1Bytes:
		return raw, nil
	case layout.SectorSize:
		return raw[:layout.Stage1Bytes], nil
	default:
		return nil, fmt.Errorf("stage1 is %d bytes, want %d (a full MBR sector, the real shape every release asset ships as) or %d (just the boot code region, pre-trimmed)", len(raw), layout.SectorSize, layout.Stage1Bytes)
	}
}

func checkUpdateEligible(disk string) error {
	existing, err := biosboot.ReadStage2(disk)
	if err != nil {
		return fmt.Errorf("refusing to update: reading %s's current stage2: %w", disk, err)
	}
	if _, _, err := biosboot.ExtractStage2Version(existing); err != nil {
		return fmt.Errorf("refusing to update: %s does not look like a disk with alpine-zfsboot already installed (no recognizable stage2 found) - if this is a fresh disk, use 'install' instead: %w", disk, err)
	}
	return nil
}

// checkBIOSUpToDate is updateBIOS's own downgrade-check primitive
// (F16, unidoc-alip's PR #5 follow-up review) - see that function's
// own call site for the full reasoning. Split out, like
// checkUpdateEligible just above, so it's testable without exercising
// die()'s own os.Exit. Returns ("", false) - never refuse, just skip
// the check - when the installed CMDLINE can't be read at all or
// carries no buildstamp (an installation from before this project
// recorded one).
func checkBIOSUpToDate(mountpoint, newBuildStamp string) (installedBuildStamp string, upToDate bool) {
	installed, err := os.ReadFile(filepath.Join(mountpoint, layout.CmdlineFile))
	if err != nil {
		return "", false
	}
	localInfo := cmdline.ParseText(installed)
	if localInfo.BuildStamp == "" {
		return "", false
	}
	return localInfo.BuildStamp, newBuildStamp <= localInfo.BuildStamp
}

func updateBIOS(t *target, workdir string, yes bool, src release.BIOSSources) {
	assets, err := release.ResolveBIOS(src, t.arch, workdir)
	die(err)
	defer assets.RemoveAll()

	stage1, err := os.ReadFile(assets.Stage1)
	die(err)
	stage1, err = trimStage1Asset(stage1)
	die(err)
	stage2, err := os.ReadFile(assets.Stage2)
	die(err)
	kernel, err := os.ReadFile(assets.Kernel)
	die(err)
	initrd, err := os.ReadFile(assets.Initrd)
	die(err)
	cmdlineTxt, err := os.ReadFile(assets.Cmdline)
	die(err)

	if len(stage2) > layout.Stage2Bytes {
		die(fmt.Errorf("stage2 is %d bytes, exceeds the %d-byte budget", len(stage2), layout.Stage2Bytes))
	}

	// Full non-mutating preflight BEFORE the first write, not just before
	// WriteStage2's own call to it (which is still there too, as
	// defense-in-depth - see that function's own comment). Without this,
	// a disk with a real partition overlapping the stage2 extent would
	// get stage1 (MBR bytes 0-439) overwritten by WriteStage1 below,
	// THEN get correctly refused by WriteStage2's own check - leaving a
	// disk with a mutated MBR and an untouched, now-mismatched stage2.
	// The whole point of failing before mutating is failing before ANY
	// mutation, not just before the specific write that happens to be
	// guarded.
	if err := bootenv.CheckStage2ExtentFree(t.disk); err != nil {
		die(fmt.Errorf("refusing to update: %w", err))
	}

	die(checkUpdateEligible(t.disk))

	newCmdline := cmdline.ParseText(cmdlineTxt)

	// F16 (unidoc-alip's PR #5 follow-up review): BIOS update used to
	// have no build comparison at all, unlike UEFI (updateUEFI's own
	// latest.BuildStamp <= local.BuildStamp check above) - `update -y`
	// rewrote stage1/stage2/KERNEL/INITRD/CMDLINE on every run
	// regardless of whether the new build was actually newer, and a
	// retracted/rolled-back "latest" release applied as a silent
	// downgrade with nothing to notice or refuse. Same comparison, same
	// unconditional behavior (no --force/-y bypass, matching UEFI's own
	// - see that check's own comment) - the installed CMDLINE's own
	// buildstamp is the one signal guaranteed to be in the SAME format
	// as the new build's (both written by the exact same build.sh
	// printf line - see that script's own BUILD_STAMP comment),
	// unlike stage2's own separately-generated ZFSBOOT_BUILD_ID (a
	// different build step, a different timestamp format, not
	// comparable here). Skipped, not refused, when the installed
	// CMDLINE can't be read or carries no buildstamp at all (an
	// installation from before this project recorded one) - treating
	// that as a hard refusal would regress every such installation's
	// own first update under this fix, not improve its safety.
	if installedBuildStamp, upToDate := checkBIOSUpToDate(t.mountpoint, newCmdline.BuildStamp); upToDate {
		fmt.Printf("%s is already up to date (%s)\n", t.disk, cmdline.HumanVersion(installedBuildStamp))
		return
	}

	// F8 (unidoc-alip's PR #5 follow-up review): preflighted here,
	// before writeBIOSStagesWithRollback below, not after it - see
	// preflightBIOSMetadata's own comment for the full reasoning (a
	// cmdline/kernel/initrd combination that fails this check must never
	// be discovered only after stage1/stage2 already have the new
	// build's bytes on disk).
	if err := preflightBIOSMetadata(t.arch, newCmdline, kernel, initrd); err != nil {
		die(fmt.Errorf("refusing to update: %w", err))
	}

	if !yes && !confirm(fmt.Sprintf("overwrite %s's stage1/stage2 and %s/{KERNEL,INITRD,CMDLINE} with this build?", t.disk, layout.ESPDir)) {
		fmt.Println("not updated")
		return
	}

	die(writeBIOSStagesWithRollback(t.disk, stage1, stage2))

	die(writePayloadWithRollback(t.mountpoint, t.arch, newCmdline.Version, newCmdline.BuildStamp, kernel, initrd, cmdlineTxt))

	fmt.Printf("%s updated: stage1 (%d bytes), stage2 (%d bytes), %s/{KERNEL,INITRD,CMDLINE,METADATA} - every write verified byte-for-byte.\n",
		t.disk, len(stage1), len(stage2), layout.ESPDir)
}

// --- install -----------------------------------------------------------

func newInstallCmd() *cobra.Command {
	var root string
	var firmware string
	var yes bool
	var efiFile, efiURL string
	var stage1File, stage1URL, stage2File, stage2URL string
	var kernelFile, kernelURL, initrdFile, initrdURL, cmdlineFile, cmdlineURL string
	var sshKey, net, ipv4, ipv4Address, ipv4Gateway, ipv6, ipv6Address, ipv6Gateway, sshListen, sshPort, sshAllow string

	cmd := &cobra.Command{
		Use:   "install <disk>",
		Short: "install alpine-zfsboot's own boot artifacts onto an already-partitioned, already-formatted disk",
		Long: `install writes ONLY alpine-zfsboot's own boot artifacts (stage1/stage2 on
BIOS, the UEFI loader, and the FAT payload/config) - it does not partition
disks or create filesystems. The disk's partitions and the FAT filesystem
at --root/boot/efi must already exist (alpine-installer's own
partition_disk()/format_boot_partition() do that first).

--firmware is REQUIRED, not auto-detected: unlike status/verify/update
(which run on an already-booted target and can trust that target's own
live /sys/firmware/efi), install runs against a not-yet-bootable target
that has no running kernel of its own yet - "does --root's own /sys say
UEFI" is not a meaningful question to ask of a directory tree.
alpine-installer already knows definitively which firmware the target
should use (its own USE_UEFI) and must pass it explicitly.`,
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			disk := args[0]
			arch := detectArch()
			var uefi bool
			switch firmware {
			case "uefi":
				uefi = true
			case "bios":
				uefi = false
			default:
				die(fmt.Errorf("--firmware must be \"uefi\" or \"bios\" (got %q)", firmware))
			}
			mountpoint := filepath.Join(root, "boot/efi")

			// F17 (unidoc-alip's PR #5 review): without this, an ESP
			// that was never actually mounted (or mounted as the wrong
			// filesystem, or - BIOS only - mounted from a different
			// disk than <disk>) let install write its payload
			// somewhere that isn't the real boot filesystem at all,
			// with nothing downstream (espconfig.VerifyPayload
			// compares the write against itself either way) able to
			// tell the difference. diskArg is "" for UEFI - see
			// VerifyESPMounted's own doc comment for why that firmware
			// has no corresponding disk-match half of this check.
			diskArg := ""
			if !uefi {
				diskArg = disk
			}
			die(bootenv.VerifyESPMounted(mountpoint, diskArg))

			workdir, err := os.MkdirTemp("", "alpine-zfsboot-install-*")
			die(err)
			defer os.RemoveAll(workdir)

			cfg := espconfig.Config{
				Net: net, IPv4: ipv4, IPv4Address: ipv4Address, IPv4Gateway: ipv4Gateway,
				IPv6: ipv6, IPv6Address: ipv6Address, IPv6Gateway: ipv6Gateway,
				SSHListen: sshListen, SSHPort: sshPort, SSHAllow: sshAllow,
			}

			if uefi {
				installUEFI(arch, mountpoint, workdir, yes, release.Source{File: efiFile, URL: efiURL}, cfg, sshKey)
				return
			}
			installBIOS(disk, arch, mountpoint, workdir, yes, release.BIOSSources{
				Stage1:  release.Source{File: stage1File, URL: stage1URL},
				Stage2:  release.Source{File: stage2File, URL: stage2URL},
				Kernel:  release.Source{File: kernelFile, URL: kernelURL},
				Initrd:  release.Source{File: initrdFile, URL: initrdURL},
				Cmdline: release.Source{File: cmdlineFile, URL: cmdlineURL},
			}, cfg, sshKey)
		},
	}
	cmd.Flags().StringVar(&root, "root", "", "root of the target install (e.g. /mnt/alpine) - required")
	cmd.MarkFlagRequired("root")
	cmd.Flags().StringVar(&firmware, "firmware", "", "\"uefi\" or \"bios\" - required, never auto-detected (see this command's own --help text for why)")
	cmd.MarkFlagRequired("firmware")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the overwrite confirmation prompt")
	cmd.Flags().StringVar(&efiFile, "efi-file", "", "UEFI: install this local .EFI file instead of fetching the latest release")
	cmd.Flags().StringVar(&efiURL, "efi-url", "", "UEFI: fetch the .EFI from this URL instead of the latest release (must be minisign-signed: <url>.minisig must exist and verify)")
	cmd.Flags().StringVar(&stage1File, "stage1-file", "", "BIOS: local stage1.bin instead of the latest release")
	cmd.Flags().StringVar(&stage1URL, "stage1-url", "", "BIOS: fetch stage1.bin from this URL instead of the latest release (must be minisign-signed: <url>.minisig must exist and verify)")
	cmd.Flags().StringVar(&stage2File, "stage2-file", "", "BIOS: local stage2.bin instead of the latest release")
	cmd.Flags().StringVar(&stage2URL, "stage2-url", "", "BIOS: fetch stage2.bin from this URL instead of the latest release (must be minisign-signed: <url>.minisig must exist and verify)")
	cmd.Flags().StringVar(&kernelFile, "kernel-file", "", "BIOS: local kernel instead of the latest release")
	cmd.Flags().StringVar(&kernelURL, "kernel-url", "", "BIOS: fetch the kernel from this URL instead of the latest release (must be minisign-signed: <url>.minisig must exist and verify)")
	cmd.Flags().StringVar(&initrdFile, "initrd-file", "", "BIOS: local initrd instead of the latest release")
	cmd.Flags().StringVar(&initrdURL, "initrd-url", "", "BIOS: fetch the initrd from this URL instead of the latest release (must be minisign-signed: <url>.minisig must exist and verify)")
	cmd.Flags().StringVar(&cmdlineFile, "cmdline-file", "", "BIOS: local cmdline.txt instead of the latest release")
	cmd.Flags().StringVar(&cmdlineURL, "cmdline-url", "", "BIOS: fetch cmdline.txt from this URL instead of the latest release (must be minisign-signed: <url>.minisig must exist and verify)")
	cmd.Flags().StringVar(&sshKey, "ssh-key", "", "rescue-SSH authorized_keys content (same as ALPINE_ZFSBOOT_SSH_KEY) - a host key is generated automatically when set")
	cmd.Flags().StringVar(&net, "net", "", "alpine-zfsboot.net= default (e.g. static, dhcp, off)")
	cmd.Flags().StringVar(&ipv4, "ipv4", "", "alpine-zfsboot.ipv4=")
	cmd.Flags().StringVar(&ipv4Address, "ipv4-address", "", "alpine-zfsboot.ipv4.address=")
	cmd.Flags().StringVar(&ipv4Gateway, "ipv4-gateway", "", "alpine-zfsboot.ipv4.gateway=")
	cmd.Flags().StringVar(&ipv6, "ipv6", "", "alpine-zfsboot.ipv6=")
	cmd.Flags().StringVar(&ipv6Address, "ipv6-address", "", "alpine-zfsboot.ipv6.address=")
	cmd.Flags().StringVar(&ipv6Gateway, "ipv6-gateway", "", "alpine-zfsboot.ipv6.gateway=")
	cmd.Flags().StringVar(&sshListen, "ssh-listen", "", "alpine-zfsboot.ssh.listen=")
	cmd.Flags().StringVar(&sshPort, "ssh-port", "", "alpine-zfsboot.ssh.port=")
	cmd.Flags().StringVar(&sshAllow, "ssh-allow", "", "alpine-zfsboot.ssh.allow=")
	return cmd
}

func installUEFI(arch, mountpoint, workdir string, yes bool, src release.Source, cfg espconfig.Config, sshKey string) {
	path, err := release.ResolveEFI(src, arch, workdir)
	die(err)
	efiBytes, err := os.ReadFile(path)
	die(err)
	info, err := cmdline.Read(path)
	die(err)

	if !yes && !confirm(fmt.Sprintf("install this UEFI build onto %s?", mountpoint)) {
		fmt.Println("not installed")
		return
	}

	_, err = writeUEFIGenerationWithRollback(mountpoint, arch, efiBytes, path, info)
	die(err)

	writeConfig(mountpoint, cfg, sshKey)
	fmt.Printf("UEFI loader installed at %s - verified byte-for-byte.\n", mountpoint)
}

func installBIOS(disk, arch, mountpoint, workdir string, yes bool, src release.BIOSSources, cfg espconfig.Config, sshKey string) {
	assets, err := release.ResolveBIOS(src, arch, workdir)
	die(err)
	defer assets.RemoveAll()

	stage1, err := os.ReadFile(assets.Stage1)
	die(err)
	stage1, err = trimStage1Asset(stage1)
	die(err)
	stage2, err := os.ReadFile(assets.Stage2)
	die(err)
	kernel, err := os.ReadFile(assets.Kernel)
	die(err)
	initrd, err := os.ReadFile(assets.Initrd)
	die(err)
	cmdlineTxt, err := os.ReadFile(assets.Cmdline)
	die(err)

	if len(stage2) > layout.Stage2Bytes {
		die(fmt.Errorf("stage2 is %d bytes, exceeds the %d-byte budget", len(stage2), layout.Stage2Bytes))
	}

	// Same full non-mutating preflight as updateBIOS, and for the same
	// reason - see that function's own comment on this exact call.
	if err := bootenv.CheckStage2ExtentFree(disk); err != nil {
		die(fmt.Errorf("refusing to install: %w", err))
	}

	// F8 (unidoc-alip's PR #5 follow-up review): same preflight, same
	// reasoning, as updateBIOS's own identical call - see
	// preflightBIOSMetadata's own comment.
	newCmdline := cmdline.ParseText(cmdlineTxt)
	if err := preflightBIOSMetadata(arch, newCmdline, kernel, initrd); err != nil {
		die(fmt.Errorf("refusing to install: %w", err))
	}

	if !yes && !confirm(fmt.Sprintf("install stage1/stage2 onto %s and the FAT payload onto %s?", disk, mountpoint)) {
		fmt.Println("not installed")
		return
	}

	die(writeBIOSStagesWithRollback(disk, stage1, stage2))

	die(writePayloadWithRollback(mountpoint, arch, newCmdline.Version, newCmdline.BuildStamp, kernel, initrd, cmdlineTxt))

	writeConfig(mountpoint, cfg, sshKey)
	fmt.Printf("%s: stage1 (%d bytes), stage2 (%d bytes) installed; %s/{KERNEL,INITRD,CMDLINE,METADATA} installed at %s - every write verified byte-for-byte.\n",
		disk, len(stage1), len(stage2), layout.ESPDir, mountpoint)
}

// writeConfig writes EFI/ALPINE/config plus, when sshKey is set, the
// rescue-SSH authorized_keys and a freshly-generated host key -
// matches alpine-install-zfs.sh's own write_alpine_zfsboot_esp_config()
// field-for-field (same env-var-derived inputs, same "only write a
// key line when sshKey itself is set" gate), now transactional (see
// internal/espconfig's own doc comment) where the original was not.
func writeConfig(mountpoint string, cfg espconfig.Config, sshKey string) {
	die(espconfig.WriteConfig(mountpoint, cfg))
	if sshKey == "" {
		return
	}
	die(espconfig.WriteAuthorizedKeys(mountpoint, sshKey))
	die(espconfig.GenerateHostKey(mountpoint))
}
