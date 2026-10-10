// `alpine-zfsboot esp list|add|remove|adopt` - managing the set of ESPs
// of a mirrored boot (see mirror.go and README "Mirrored boot").
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/unidoc/alpine-zfsboot/internal/biosboot"
	"github.com/unidoc/alpine-zfsboot/internal/bootenv"
	"github.com/unidoc/alpine-zfsboot/internal/espconfig"
	"github.com/unidoc/alpine-zfsboot/internal/espmember"
	"github.com/unidoc/alpine-zfsboot/internal/layout"
	"github.com/unidoc/alpine-zfsboot/internal/uefiboot"
)

func newESPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "esp",
		Short: "list and manage the ESPs of a mirrored boot (one ESP per disk)",
		Long: `A mirrored boot has one ESP per disk of the ZFS mirror. The ESPs of this
host carry EFI/ALPINE/MEMBER: this host's install id, a generation, and
the UUIDs of all ESPs of the set (see README "Mirrored boot: how members
are recognised"). install/update write every member; these commands
change WHICH ESPs are members:

  esp list            every FAT volume that looks like an alpine-zfsboot
                      ESP, and whether it is a member, missing, or why not
  esp add <esp>       make a prepared ESP (mkfs.vfat done) a member: copy
                      the boot chain, config, keys and host key from the
                      best member onto it (BIOS: stage1/stage2 onto its disk)
  esp remove <esp>    stop treating an ESP as a member: removes its MEMBER
                      marker only (never boot files); works for a dead disk
  esp adopt <esp>...  make ESPs that already carry alpine-zfsboot files but
                      no marker (installs before 0.5.0) members, as they are

<esp> is a device (/dev/sdb1) or a FAT UUID (6AC5-2367).`,
	}
	cmd.AddCommand(newESPListCmd(), newESPAddCmd(), newESPRemoveCmd(), newESPAdoptCmd())
	return cmd
}

func newESPListCmd() *cobra.Command {
	var root, firmware string
	var ho hostOpts
	cmd := &cobra.Command{
		Use:   "list",
		Short: "list every ESP-like FAT volume and whether it is a member of this host's set",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			os.Exit(runESPList(root, firmware, ho))
		},
	}
	cmd.Flags().StringVar(&root, "root", "/", "root of the target OS (rescue-initramfs use)")
	cmd.Flags().StringVar(&firmware, "firmware", "", "override firmware detection (\"uefi\"/\"bios\")")
	addHostFlags(cmd, &ho, false)
	return cmd
}

func runESPList(root, firmware string, o hostOpts) int {
	infos, err := scanESPs()
	if err != nil {
		printErr(err)
		return exitFailure
	}
	list, src, _, err := resolveList(o, root, true)
	if err != nil {
		printErr(err)
		return exitFailure
	}
	hs, selErr := bootenv.SelectHost(infos, bootenv.SelectOpts{List: list, ListSource: src, PrimaryUUID: primaryUUID(root)})
	state := map[string]string{}
	if selErr == nil {
		for n, m := range hs.Members {
			s := "MEMBER"
			if n == 0 {
				s += " (best)"
			}
			if m.Foreign {
				s = "LISTED, FOREIGN marker - written only with --force-adopt"
			}
			state[m.UUID] = s
		}
		for _, ig := range hs.Ignored {
			state[ig.UUID] = "not used: " + ig.Reason
		}
	}
	fmt.Printf("%-10s %-18s %-12s %-10s %-9s %s\n", "UUID", "DEVICE", "LABEL", "INSTALL", "GEN", "STATE")
	shown := 0
	for _, i := range infos {
		if !i.AlpineDir && i.Member == nil && i.MemberErr == nil && state[i.UUID] == "" {
			continue // a FAT volume with nothing of ours on it
		}
		id, gen := "-", "-"
		switch {
		case i.MemberErr != nil:
			id = "BROKEN"
		case i.Member != nil:
			id = i.Member.InstallID[:8]
			gen = fmt.Sprint(i.Member.Generation)
			if i.CopiedMarker() {
				id += "*"
			}
		}
		s := state[i.UUID]
		if s == "" {
			s = "not used"
		}
		fmt.Printf("%-10s %-18s %-12s %-10s %-9s %s\n", i.UUID, i.Dev, orNone(i.Label), id, gen, s)
		shown++
	}
	if selErr == nil {
		for _, u := range hs.Missing {
			fmt.Printf("%-10s %-18s %-12s %-10s %-9s %s\n", u, "-", "-", "-", "-", "MISSING - expected but not present")
		}
		fmt.Printf("\nset: %s", hs.Mode)
		if hs.InstallID != "" {
			fmt.Printf(", install id %s", hs.InstallID)
		}
		if hs.ListSource != "" {
			fmt.Printf(", list from %s", hs.ListSource)
		}
		fmt.Println()
	} else {
		fmt.Printf("\nthis host's set cannot be determined: %v\n", selErr)
	}
	if shown == 0 && selErr != nil {
		return exitFailure
	}
	if selErr != nil {
		return exitFailure
	}
	if len(hs.Missing) > 0 {
		return exitMemberMissing
	}
	return 0
}

func newESPAddCmd() *cobra.Command {
	var root, firmware string
	var ho hostOpts
	var yes bool
	cmd := &cobra.Command{
		Use:   "add <device|uuid>",
		Short: "make a prepared ESP a member: copy this host's boot chain, config, keys and host key onto it",
		Long: `esp add makes an ESP a member of this host's mirrored boot. The ESP must
exist and be formatted (mkfs.vfat -F32 -n EFI <partition>); everything on
it under EFI/ALPINE and the loader is (over)written with a copy of the
best member's - no download, the same build the host runs now. BIOS:
stage1/stage2 go onto the ESP's disk (same safety checks as install). An
ESP whose marker names another installation needs --force-adopt.

Then every member's MEMBER lists the new ESP, so status/verify/init know
the set even without an explicit list.`,
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			os.Exit(runESPAdd(root, firmware, ho, args[0], yes))
		},
	}
	cmd.Flags().StringVar(&root, "root", "/", "root of the target OS (rescue-initramfs use)")
	cmd.Flags().StringVar(&firmware, "firmware", "", "override firmware detection (\"uefi\"/\"bios\")")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().BoolVar(&ho.forceAdopt, "force-adopt", false, "also take an ESP whose MEMBER marker names another installation")
	cmd.Flags().StringVar(&ho.espUUIDs, "esp-uuids", "", "this host's set as an explicit list (see status --help)")
	return cmd
}

// copyBootChain writes src's installed boot chain onto m (BIOS: also
// stage1/stage2 onto m's disk) with the same atomic/rollback writers as
// install/update, then its config/keys/host key.
func copyBootChain(h *host, src, m *member) error {
	if h.uefi {
		rel, err := uefiboot.LoaderPath(h.arch)
		if err != nil {
			return err
		}
		loader, err := os.ReadFile(filepath.Join(src.mountpoint, rel))
		if err != nil {
			return fmt.Errorf("reading the loader of %s: %w", src.label(), err)
		}
		_, _, info, err := readInstalledKernelInitrd(src.target)
		if err != nil {
			return err
		}
		tmp, err := os.CreateTemp("", "alpine-zfsboot-esp-add-*.efi")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.Write(loader); err != nil {
			tmp.Close()
			return err
		}
		tmp.Close()
		if _, err := writeUEFIGenerationWithRollback(m.mountpoint, h.arch, loader, tmp.Name(), info); err != nil {
			return err
		}
	} else {
		s1, err := biosboot.ReadStage1(src.disk)
		if err != nil {
			return fmt.Errorf("reading stage1 of %s: %w", src.disk, err)
		}
		s2, err := biosboot.ReadStage2(src.disk)
		if err != nil {
			return fmt.Errorf("reading stage2 of %s: %w", src.disk, err)
		}
		if err := checkStagePair(s2, false); err != nil {
			return fmt.Errorf("the stage2 on %s: %w", src.disk, err)
		}
		if err := biosboot.CheckSectorSize(m.disk); err != nil {
			return fmt.Errorf("refusing to write %s: %w", m.disk, err)
		}
		if err := bootenv.CheckStage2ExtentFree(m.disk); err != nil {
			return fmt.Errorf("refusing to write %s: %w", m.disk, err)
		}
		kernel, initrd, info, err := readInstalledKernelInitrd(src.target)
		if err != nil {
			return fmt.Errorf("reading the payload of %s: %w", src.label(), err)
		}
		if err := preflightBIOSMetadata(h.arch, info, kernel, initrd); err != nil {
			return err
		}
		c, err := os.ReadFile(filepath.Join(src.mountpoint, layout.CmdlineFile))
		if err != nil {
			return err
		}
		c = setCmdlineWord(c, layout.ESPSelfKey, m.info.UUID)
		if err := checkCmdlineLength(c); err != nil {
			return err
		}
		if err := writeBIOSStagesWithRollback(m.disk, s1, s2); err != nil {
			return err
		}
		if err := writePayloadWithRollback(m.mountpoint, h.arch, info.Version, info.BuildStamp, kernel, initrd, c, updateIntegrityMode(src.mountpoint, biosOpts{})); err != nil {
			return err
		}
	}
	_, err := syncConfig(src, m)
	return err
}

func runESPAdd(root, firmware string, o hostOpts, arg string, yes bool) int {
	h, err := discoverHost(root, firmware, false, o)
	if err != nil {
		printErr(err)
		return exitFailure
	}
	defer withCleanup(h.cleanup)()
	src := h.bestSource()
	if src == nil || src.mountErr != nil {
		printErr(fmt.Errorf("no installed member to copy from"))
		return exitFailure
	}
	o.addESP = arg
	before := len(h.members)
	if err := h.addESPs(o); err != nil {
		printErr(err)
		return exitFailure
	}
	if len(h.members) == before {
		return 0 // already a member (addESPs said so)
	}
	m := h.members[len(h.members)-1]
	if err := m.usable(true, true, h.uefi); err != nil {
		printErr(fmt.Errorf("%s: %w", m.label(), err))
		return exitFailure
	}
	if err := h.checkDistinctDisks(); err != nil {
		printErr(err)
		return exitFailure
	}
	what := m.label()
	if !h.uefi {
		what += " and the boot loader on disk " + m.disk
	}
	if !yes && !confirm(fmt.Sprintf("overwrite %s with a copy of %s's boot chain, config, keys and host key?", what, src.label())) {
		fmt.Println("not added")
		return 0
	}
	if err := copyBootChain(h, src, m); err != nil {
		printErr(fmt.Errorf("%s: %w (rolled back - the ESP was not made a member)", m.label(), err))
		return exitFailure
	}
	// MEMBER on the new ESP (the source's generation: same content), and
	// the new MEMBERS list on every other member.
	gen := src.info.Generation()
	if gen == 0 {
		gen = 1
	}
	var ok []*member
	for _, x := range h.members {
		if x.target != nil && !x.foreign {
			ok = append(ok, x)
		}
	}
	failed := false
	for x, err := range h.finalizeMembers(ok, gen, poolOf(src.target)) {
		printErr(fmt.Errorf("%s: %w", x.label(), err))
		failed = true
	}
	if failed {
		return exitFailure
	}
	fmt.Printf("%s is now a member of this host's mirrored boot\n", m.label())
	return h.exitCode(false)
}

func newESPRemoveCmd() *cobra.Command {
	var root, firmware string
	var ho hostOpts
	var yes bool
	cmd := &cobra.Command{
		Use:   "remove <device|uuid>",
		Short: "stop treating an ESP as a member (removes its MEMBER marker only, never boot files)",
		Long: `esp remove takes an ESP out of this host's set: its EFI/ALPINE/MEMBER
marker is removed (nothing else - the files stay, a later 'esp adopt'
brings it back), and the other members' MEMBER no longer list it. For a
disk that is gone for good, give its FAT UUID: only the other members'
lists change. If the ESP is in an explicit alpine-zfsboot.esp-uuids list
(config, BIOS CMDLINE), it is removed from that list too.`,
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			os.Exit(runESPRemove(root, firmware, ho, args[0], yes))
		},
	}
	cmd.Flags().StringVar(&root, "root", "/", "root of the target OS (rescue-initramfs use)")
	cmd.Flags().StringVar(&firmware, "firmware", "", "override firmware detection (\"uefi\"/\"bios\")")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}

func runESPRemove(root, firmware string, o hostOpts, arg string, yes bool) int {
	h, err := discoverHost(root, firmware, false, o)
	if err != nil {
		printErr(err)
		return exitFailure
	}
	defer withCleanup(h.cleanup)()

	uuid, uerr := espmember.NormalizeFATUUID(arg)
	if uerr != nil {
		info, err := h.findInfo(arg)
		if err != nil {
			printErr(err)
			return exitFailure
		}
		uuid = info.UUID
	}
	var target *member
	var rest []*member
	for _, m := range h.members {
		if m.info.UUID == uuid {
			target = m
		} else {
			rest = append(rest, m)
		}
	}
	known := target != nil
	for _, u := range h.set.Missing {
		known = known || u == uuid
	}
	if !known {
		printErr(fmt.Errorf("ESP %s is not a member of this host's set (see 'alpine-zfsboot esp list')", uuid))
		return exitFailure
	}
	if len(rest) == 0 {
		printErr(fmt.Errorf("ESP %s is the only member present - refusing to remove the last one", uuid))
		return exitFailure
	}
	if !yes && !confirm(fmt.Sprintf("remove ESP %s from this host's set (its MEMBER marker only - no boot files are deleted)?", uuid)) {
		fmt.Println("not removed")
		return 0
	}
	failed := false
	if target != nil && target.target != nil {
		if err := espmember.Remove(target.mountpoint); err != nil {
			printErr(fmt.Errorf("%s: removing %s: %w", target.label(), layout.MemberFile, err))
			failed = true
		} else {
			fmt.Printf("%s: %s removed\n", target.label(), layout.MemberFile)
		}
	}
	// Drop it from the set everywhere it is recorded.
	var exp []string
	for _, u := range h.set.Expected {
		if u != uuid {
			exp = append(exp, u)
		}
	}
	h.set.Expected = exp
	var missing []string
	for _, u := range h.set.Missing {
		if u != uuid {
			missing = append(missing, u)
		}
	}
	h.set.Missing = missing
	h.members = rest
	for _, m := range rest {
		if m.target == nil || m.foreign {
			continue // never write to another installation's ESP
		}
		raw, _ := os.ReadFile(filepath.Join(m.mountpoint, layout.ConfigFile))
		if v, _ := espconfig.ConfigValue(raw, layout.ESPUUIDsKey); v != "" {
			if nl := dropUUID(v, uuid); nl != v {
				if err := espconfig.SetConfigKey(m.mountpoint, layout.ESPUUIDsKey, nl); err != nil {
					printErr(err)
					failed = true
				}
			}
		}
		if !h.uefi {
			c, err := os.ReadFile(filepath.Join(m.mountpoint, layout.CmdlineFile))
			if v := cmdlineValue(string(c), layout.ESPUUIDsKey); err == nil && v != "" {
				if nl := dropUUID(v, uuid); nl != v {
					if err := espconfig.WriteFile(m.mountpoint, layout.CmdlineFile, setCmdlineWord(c, layout.ESPUUIDsKey, nl), 0o644); err != nil {
						printErr(err)
						failed = true
					}
				}
			}
		}
	}
	var ok []*member
	for _, m := range rest {
		if m.target != nil && m.info.ValidMember() && !m.foreign {
			ok = append(ok, m)
		}
	}
	for m, err := range h.finalizeMembers(ok, h.maxGeneration(), poolOf(srcTarget(h.bestSource()))) {
		printErr(fmt.Errorf("%s: %w", m.label(), err))
		failed = true
	}
	if failed {
		return exitFailure
	}
	fmt.Printf("ESP %s is no longer a member of this host's set\n", uuid)
	return h.exitCode(false)
}

func dropUUID(list, uuid string) string {
	var out []string
	for _, u := range strings.Split(list, ",") {
		if !strings.EqualFold(strings.TrimSpace(u), uuid) {
			out = append(out, u)
		}
	}
	return strings.Join(out, ",")
}

func newESPAdoptCmd() *cobra.Command {
	var yes, forceAdopt bool
	cmd := &cobra.Command{
		Use:   "adopt <device|uuid>...",
		Short: "make ESPs that carry alpine-zfsboot files but no MEMBER marker (pre-0.5.0) members, as they are",
		Long: `esp adopt writes a MEMBER marker onto ESPs that already carry an
alpine-zfsboot installation but no marker - typically a mirror whose second
ESP was copied by hand before 0.5.0, which init refuses to choose between.
Nothing else is written: no boot files, no config. It first shows what it
will do; it only does it with --yes. An ESP whose marker names another
installation additionally needs --force-adopt. Run 'alpine-zfsboot verify'
afterwards: it compares the members and names any that differ.`,
		Args: cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			os.Exit(runESPAdopt(args, yes, forceAdopt))
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do it (without --yes, adopt only shows what it would do)")
	cmd.Flags().BoolVar(&forceAdopt, "force-adopt", false, "also take ESPs whose MEMBER marker names another installation")
	return cmd
}

func runESPAdopt(args []string, yes, forceAdopt bool) int {
	infos, err := scanESPs()
	if err != nil {
		printErr(err)
		return exitFailure
	}
	h := &host{arch: detectArch(), infos: infos}
	defer withCleanup(h.cleanup)()
	// The host's identity, when there is one: an existing marked set.
	if hs, err := bootenv.SelectHost(infos, bootenv.SelectOpts{PrimaryUUID: primaryUUID("/")}); err == nil {
		h.set = hs
		h.installID = hs.InstallID
	}
	var targets []bootenv.ESPInfo
	seen := map[string]bool{}
	for _, a := range args {
		i, err := h.findInfo(a)
		if err != nil {
			printErr(err)
			return exitFailure
		}
		if seen[i.UUID] {
			continue
		}
		seen[i.UUID] = true
		switch {
		case !i.AlpineDir:
			printErr(fmt.Errorf("ESP %s (%s) carries no alpine-zfsboot installation (no %s) - use 'alpine-zfsboot esp add' to make it a member", i.UUID, i.Dev, layout.ESPDir))
			return exitFailure
		case i.ValidMember() && i.Member.InstallID == h.installID && h.installID != "":
			fmt.Printf("ESP %s (%s) is already a member\n", i.UUID, i.Dev)
			continue
		case i.ValidMember() && !forceAdopt:
			printErr(fmt.Errorf("ESP %s (%s) is marked for another installation (install id %s) - refusing without --force-adopt", i.UUID, i.Dev, i.Member.InstallID))
			return exitFailure
		}
		targets = append(targets, i)
	}
	if len(targets) == 0 {
		return 0
	}
	newID := h.installID == ""
	id, err := h.ensureInstallID()
	if err != nil {
		printErr(err)
		return exitFailure
	}
	gen := h.set.MaxGeneration()
	if gen == 0 {
		gen = 1
	}
	exp := map[string]bool{}
	for _, u := range h.set.Expected {
		exp[u] = true
	}
	for _, t := range targets {
		exp[t.UUID] = true
	}
	var all []string
	for u := range exp {
		all = append(all, u)
	}
	sort.Strings(all)

	fmt.Println("esp adopt will write EFI/ALPINE/MEMBER (and nothing else) onto:")
	for _, t := range targets {
		was := "no marker"
		if t.MemberErr != nil {
			was = "unreadable marker"
		} else if t.Member != nil {
			was = "marker of install id " + t.Member.InstallID
		}
		fmt.Printf("  ESP %s (%s, %s)\n", t.UUID, t.Dev, was)
	}
	idNote := ""
	if newID {
		idNote = " (new)"
	}
	fmt.Printf("install id %s%s, generation %d, set %s\n", id, idNote, gen, strings.Join(all, ","))
	if !yes {
		fmt.Println("nothing written - run again with --yes to do it")
		return exitFailure
	}
	h.set.Expected = all
	failed := false
	var ok []*member
	for _, t := range targets {
		m := h.attach(t, false, true, false)
		h.members = append(h.members, m)
		if m.mountErr != nil {
			printErr(fmt.Errorf("%s: %w", m.label(), m.mountErr))
			failed = true
			continue
		}
		ok = append(ok, m)
	}
	// The existing members learn the new set too.
	for _, hm := range h.set.Members {
		if seen[hm.UUID] {
			continue
		}
		m := h.attach(hm.ESPInfo, false, false, false)
		h.members = append(h.members, m)
		if m.mountErr == nil {
			ok = append(ok, m)
		}
	}
	for m, err := range h.finalizeMembers(ok, gen, "") {
		printErr(fmt.Errorf("%s: %w", m.label(), err))
		failed = true
	}
	if failed {
		return exitFailure
	}
	fmt.Println("adopted - run 'alpine-zfsboot verify' to compare the members, 'alpine-zfsboot update' to bring stale ones in line")
	return 0
}
