# alpine-zfsboot

A self-contained Alpine Linux ZFS bootloader, installer, and remote
recovery environment for BIOS and UEFI systems.

alpine-zfsboot owns the complete path from installing Alpine on ZFS,
through firmware boot and encrypted boot environments, to diagnosing
and recovering a machine that failed to boot. It provides its own BIOS
and UEFI boot paths and does not require GRUB.

"Installer" above means this project's own native `zfs send`/`recv`
deployment workflow — standing up a new machine from a golden image
with nothing beyond this project's own tooling (see
[Deploying a new machine (zfs recv)](#deploying-a-new-machine-zfs-recv)).
Partitioning a bare disk and laying down the very first Alpine-on-ZFS
install from scratch is handled by the separate, companion
[`alpine-installer`](https://github.com/unidoc/alpine-installer)
project — see [Quick start / installation](#quick-start--installation).

```
Install Alpine on ZFS
        |
        +-- x86_64 BIOS
        |      +-- MBR
        |      `-- GPT
        |
        +-- x86_64 UEFI/GPT
        `-- aarch64 UEFI/GPT
                |
                v
         ZFS Boot Environment
                |
         Native ZFS encryption
                |
         Target Alpine Linux
                |
         +------+------+
         |             |
      Success       Failure
                       |
                Boot diagnostics
                       |
                Rescue SSH / TUI
                       |
                Repair and retry
```

Built and maintained by [UniDoc](https://unidoc.io).

## What it provides

**Its own boot path, on real and virtual hardware, with no GRUB
dependency.** Native UEFI boot on `x86_64` and `aarch64`, and a
from-scratch legacy BIOS path on `x86_64` (its own stage1/stage2 boot
code, MBR and GPT partition-table parsing, and — for firmware whose
`INT 13h` has no working read path on a "no emulation" El Torito boot
drive — its own ATA/ATAPI PIO driver). Both boot paths reach the exact
same kernel, initramfs, and menu.

**Alpine installed directly onto ZFS**, with real Boot Environments —
independent, bootable clones of the root dataset, not just snapshots —
and a `zfs send`/`recv` deployment workflow for standing up a new
machine from a golden image rather than reinstalling one from scratch.

**Native ZFS encryption with no permanently unencrypted `/boot`
requirement.** The kernel and initramfs a machine boots from live on
the encrypted pool itself; an ephemeral copy of the unlock passphrase
is handed across the `kexec` boundary into the target's own initramfs
and discarded, so a single passphrase entry unlocks and boots without
ever writing the key to disk.

**Conditional, key-only remote rescue SSH**, with a persistent host
identity, started only when it's actually needed — never on a healthy,
unattended boot — and the same interactive recovery menu whether
reached from the local console or over that SSH connection.

**Failed-boot detection and boot-attempt accounting.** A boot
environment that kept panicking or hanging is noticed on the *next*
boot, not silently retried forever, and — once detected — the
machine's own diagnostics can be inspected remotely: boot-attempt
metadata (what was actually attempted), OpenRC's own service log, and
persistent kernel-crash evidence where the platform actually supports
it. A recovery shell and full ZFS tooling are available even when the
installed Alpine system itself cannot boot at all.

## Compatibility

| Architecture | Firmware | Partition table | Status |
|---|---|---|---|
| `x86_64` | BIOS | MBR | Supported |
| `x86_64` | BIOS | GPT | Supported |
| `x86_64` | UEFI | GPT | Supported |
| `aarch64` | UEFI | GPT | Supported |

`aarch64` is UEFI-only — there is no legacy BIOS equivalent on that
architecture at all.

## Remote recovery

This is the scenario alpine-zfsboot is built around, not an
afterthought bolted onto a bootloader:

```
target fails to boot
       |
alpine-zfsboot detects an unconfirmed boot
       |
rescue networking + SSH
       |
operator connects remotely
       |
inspect Last Boot Diagnostics
       |
unlock/mount ZFS BE if necessary
       |
repair from recovery shell
       |
retry boot
```

Rescue SSH is a break-glass facility, not a standing service:
public-key authentication only (no passwords, ever), a persistent host
key so the connection's fingerprint doesn't change every reboot, an
optional source-CIDR restriction, and conditional startup — it comes
up only when a real trigger condition is detected (an encrypted root
waiting on a key, a boot that failed to reach a confirmed-good state),
never on a routine, healthy boot. See
[Rescue SSH](#rescue-ssh) for the full trust model.

This matters most on remote physical or cloud hardware, where recovery
would otherwise depend on the provider's own rescue image having
compatible ZFS tooling, or on a trip to a datacenter. If the machine
can reach the network at all, an operator can inspect what actually
happened and fix it without either of those.

## Last Boot Diagnostics

When a target boot fails to confirm success, the next thing an
operator connecting to rescue SSH sees is not a bare shell prompt but
a summary built from every evidence source this project has, combined
without guessing at what any of them don't say:

- **Bootcheck** answers whether the previous target boot was confirmed
  — the one authoritative signal for "did the last boot succeed,"
  never inferred from any other evidence source being present or
  absent.
- **Boot-attempt metadata** records exactly which kernel, initramfs,
  and cmdline were actually attempted, and when — the anchor every
  other evidence source is checked against so old evidence is never
  shown as if it belonged to the attempt that just failed.
- **OpenRC's own `rc.log`**, read directly off the target's real ZFS
  filesystem, can show what userspace/startup evidence exists — the
  most common real-world failure class (a bad service, a bad fstab
  entry, broken networking), and one that needs no special persistence
  mechanism at all, since the evidence already lives on disk.
- **EFI pstore**, on platforms that actually support it, can provide
  persistent kernel-crash evidence surviving a real reboot — firmware-
  owned UEFI NVRAM, addressed by name rather than a hand-picked
  physical memory address, so it doesn't need per-machine tuning to be
  safe. Legacy BIOS boots have no EFI variables at all, so this source
  is honestly unavailable there, not silently faked.

The one rule every part of this is built around: **never claim a cause
that was not observed.** A missing crash record is not evidence that
the previous boot was clean — it might mean the boot genuinely
succeeded, or it might mean this platform simply has no way to capture
that evidence at all. Where the difference matters, the diagnostics
screen says which one it actually knows, distinguishing
`AVAILABLE`/`NOT AVAILABLE`/`UNSUPPORTED` rather than collapsing all
three into a single "no evidence" state. See
[Failed-boot handling and diagnostics](#failed-boot-handling-and-diagnostics)
for the full mechanism.

## Design philosophy

- **Stock Alpine kernels.** No custom kernel build, and no feature
  whose only path to working depends on a kernel config option Alpine
  doesn't already ship.
- **Minimal dependencies**, assembled from Alpine's own packages
  wherever possible rather than vendored or patched third-party code —
  see [How this is built](#how-this-is-built) for what that meant in
  practice for the EFI loader and the BIOS boot code.
- **No GRUB dependency** for the primary boot path (loose
  kernel+initramfs+cmdline files are still produced for anyone who
  wants to chainload from an existing GRUB stage 1 instead).
- **No permanently plaintext encryption key.** A ZFS-native passphrase
  is captured once, handed across the `kexec` boundary as an ephemeral
  in-memory secret, and never written to persistent storage.
- **No magic reserved RAM addresses.** Kernel-crash evidence uses EFI
  pstore specifically because it needs no hand-picked, per-machine
  physical memory reservation to be safe — see
  [Last Boot Diagnostics](#last-boot-diagnostics-1).
- **Generic artifacts, not machine-specific binaries.** One `.EFI` per
  architecture, built once and booted on any standard-UEFI/ACPI
  machine of that architecture — no per-machine build step required.
- **Never claim a cause that was not observed.** The single rule
  everything above ultimately serves: an explicit `UNKNOWN`/
  `UNAVAILABLE`/`UNSUPPORTED` instead of a guessed answer (see
  [Last Boot Diagnostics](#last-boot-diagnostics-1)), no RAM address
  presented as safe without a real reservation behind it, no security
  restriction reported as enforced without a mechanism that actually
  enforces it, no boot-attempt claimed successful without a real,
  independent confirmation. Where this project can't observe a fact
  directly, it says so, rather than inferring a plausible-looking
  answer from something adjacent.
- **Recovery mechanisms fail safely.** A recovery action that can't
  complete refuses cleanly or drops to a shell rather than silently
  modifying the installed system in a way an operator didn't ask for.

alpine-zfsboot is not merely a bootloader for Alpine on ZFS. It is an
Alpine-on-ZFS installation, boot, diagnostics, and recovery
environment that remains useful precisely when the installed operating
system cannot boot.

## Contents

Everything above this point is the landing-page pitch — what this
project provides and why. Everything below is reference
documentation for actually building, installing, and operating it.

**Technical documentation**
- [Architecture / supported platforms](#architecture--supported-platforms)
- [Quick start / installation](#quick-start--installation)
- [ZFS layout and Boot Environments](#zfs-layout-and-boot-environments)
- [Encryption](#encryption)
- [Rescue SSH](#rescue-ssh)
- [Failed-boot handling and diagnostics](#failed-boot-handling-and-diagnostics)
- [Recovery operations](#recovery-operations)
- [Building](#building)
- [Testing](#testing)

---

## Architecture / supported platforms

```
UEFI firmware
  -> this .EFI (the alpine-zfsboot EFI loader with an embedded Linux
     kernel, initramfs, and command line - its own PE executable, not
     a systemd-stub-style Unified Kernel Image)
  -> zpool import -N
  -> read the pool's bootfs property
  -> menu.py: a dialog-based TUI, FreeBSD-loader-style - always shown,
     own countdown with the default entry pre-highlighted, auto-boots
     it if left alone, fully interactive if touched (boot environments,
     kernel selection, BE lifecycle management, chroot, guided
     zfs-recv deployment, recovery shell, live console switching) -
     every path through it ends up calling boot-dataset.sh: mount the
     chosen dataset read-only, find its kernel/initramfs pair, kexec
     into it
```

One consistent first frame every boot, FreeBSD-loader-style, rather
than a fast automatic path that only shows a menu if a key is pressed
in time. If `menu.py` itself ever crashes, `/init` falls back to
running `boot-dataset.sh` directly, so a menu bug can never strand a
boot with no way forward. Standard UEFI only on the UEFI path,
`x86_64` -> `BOOTX64.EFI`, `aarch64` -> `BOOTAA64.EFI` - works equally
on any standard-UEFI/ACPI `x86_64` or
`aarch64` machine, real or virtual - no embedded-board device-tree
support, deliberately: that belongs to a different kind of project
than this one.

### Legacy BIOS boot (x86_64 only)

A second, completely independent way to reach the exact same kernel+
initramfs the UEFI path already builds, on hardware with no UEFI at
all - `bios/`. Both boot methods read from the SAME canonical FAT/ESP
partition - alpine-zfsboot's own unified storage architecture: there
is exactly one persistent boot-store format, not one for UEFI and a
separate one for BIOS. GPT is the primary target (`sgdisk`-
partitioned, same as the install instructions below), the partition
identified by the real, standard EFI System Partition type GUID
(`C12A7328-F81F-11D2-BA4B-00A0C93EC93B` - the same one UEFI firmware
itself looks for); stage2 also falls back to reading a classic MBR
partition table (up to 4 primary partitions, no extended chain - see
`bios/mbr.h`) if no valid GPT is found, for disks partitioned the
older way instead. Built entirely in-house (hand-written stage1/
stage2, no vendored bootloader stage code from anywhere else), same as
`efi/` is this project's own loader rather than a patched third-party
stub - including, for the El Torito CD-ROM boot path, its own
from-scratch ATA/ATAPI PIO driver (`bios/ata_atapi.c`), written
directly against the ATA/ATAPI command-set spec rather than any other
BIOS/OS driver's source: real-hardware testing established that INT
13h has no working read path at all for a "no emulation" El Torito
boot drive on some firmware, so that path talks to the IDE controller
directly instead.

```
protective MBR (LBA 0)                                 <- stage1
  -> fixed-LBA (34) extended INT13h read -> jump

BIOS boot partition (type GUID 21686148-6449-6E6F-744E-656564454649,
starts at LBA 34 by construction - stage1 never parses GPT at all)
                                                         <- stage2
  -> real-mode GPT parse, finds the canonical alpine-zfsboot FAT/ESP
     partition (the SAME partition UEFI firmware boots BOOTX64.EFI/
     BOOTAA64.EFI from - see bios/gpt.h's ZFSBOOT_ESP_TYPE_GUID)
  -> a small, deliberately minimal read-only FAT32 reader (bios/fat.c -
     real BPB/geometry validation, FAT-chain walking with cycle
     detection, no writes, no long-filename support) locates and reads
     EFI/ALPINE/{KERNEL,INITRD,CMDLINE} - the exact same kernel/
     initramfs/cmdline build.sh already produces for the UEFI path,
     as ordinary files, not a separate packed format
  -> "unreal mode" (bios/switch32.S) to place the kernel at 1MB and
     the initrd above it, gathers the E820 memory map, builds a
     Linux/x86 boot_params, and jumps into the kernel per
     Documentation/arch/x86/boot.rst's 32-bit boot protocol

kernel + initramfs - identical to the UEFI path from here on: /init,
menu.py, boot-dataset.sh neither know nor care which boot path got
them running. /init remains the sole reader of
EFI/ALPINE/{config,authorized_keys,ssh_host_ed25519_key} on
this same partition - stage1/stage2 never touch machine configuration
or rescue-SSH policy at all.
```

`build.sh` produces `alpine-zfsboot-x86_64-bios-{stage1,stage2}.bin`
alongside the usual `.EFI`/`.iso` assets and the loose
`vmlinuz`/`initramfs.img`/`cmdline.txt` files - the SAME loose files
both the UEFI GRUB-chainload use case and a BIOS install's
EFI/ALPINE/* payload are populated from. MBR mode supports up to 4
primary partitions, no extended/logical chain.

## Quick start / installation

alpine-zfsboot itself is the boot/recovery layer, not an OS installer
in the traditional sense — it does not partition a disk or lay down an
Alpine root filesystem on its own. Two paths reach a real, bootable
machine:

**Using `alpine-installer`** (recommended for a first machine): the
companion [`alpine-installer`](https://github.com/unidoc/alpine-installer)
project's `alpine-install-zfs.sh` partitions a disk (BIOS/MBR,
BIOS/GPT, or UEFI/GPT — see [Compatibility](#compatibility)), installs
Alpine onto a ZFS root, and installs an alpine-zfsboot release
directly, in one run. It handles the boot-layout details this project
expects (pool name, root dataset, `bootfs` property, the canonical FAT/
ESP partition every layout carries) so nothing needs to be matched up
by hand.

**Deploying a clone of an existing machine**: once one machine is
running Alpine on ZFS with alpine-zfsboot installed, the fastest way to
stand up another is `zfs send`/`recv` of a golden image, not a second
full install — see
[Deploying a new machine (zfs recv)](#deploying-a-new-machine-zfs-recv).

Either way, once a machine has both an Alpine-on-ZFS install and an
alpine-zfsboot boot image in place, boot behavior is the same
regardless of how it got there: firmware loads alpine-zfsboot, it
imports the pool, and either auto-boots the default boot environment
or drops into the interactive menu — see
[Menu](#menu-always-shown-freebsd-loader-style) below.

Pre-built images are published on the
[Releases page](https://github.com/unidoc/alpine-zfsboot/releases),
versioned independently (`v0.1.0`, `v0.2.0`, ...) - bumped on any
change worth shipping, or whenever a new Alpine version needs
supporting, on this project's own schedule:

```
alpine-zfsboot-x86_64.EFI
alpine-zfsboot-aarch64.EFI
```

One build per arch, always both consoles active: the kernel sends
boot messages to every listed console, but only the *last* one
becomes `/dev/console` - `init/init`'s own `select_console()` picks
which one `menu.py` actually renders to, whichever was last used (see
below), or the first one found if there's no persisted preference yet
- and `menu.py` itself can switch live between them at any time from
its own "Switch console" menu item, FreeBSD-loader-style, rather than
only ever racing for one once at the very start of boot.

Each of the two arches above also ships as:

- **`....iso`** - the exact same `.EFI`, wrapped as a plain bootable
  ISO (El Torito, UEFI only, no legacy/BIOS entry) - for anywhere a
  bare `.EFI` file isn't itself a valid boot target: BMC/IPMI virtual
  media, a real USB stick, a VM's virtual CDROM. Pure packaging, no
  boot-logic difference from the `.EFI` at all.
- **`...-vmlinuz` / `...-initramfs.img` / `...-cmdline.txt`** - the
  same kernel/initramfs/cmdline as loose files instead of bundled into
  the `.EFI`, for chainloading from an existing GRUB stage 1 (or any
  other bootloader that loads a Linux kernel + initrd directly, given
  the cmdline separately).

Each release also ships `alpine-zfsboot-x86_64` / `alpine-zfsboot-aarch64`
(see [`cmd/tool`](#cmdtool-alpine-zfsboot-the-boot-management-cli)
below) alongside the `.EFI`/`.iso`/loose-file assets above. Each
release includes a `SHA256SUMS` covering every asset.

### Integration with alpine-installer

`alpine-installer`'s `alpine-install-zfs.sh` already installs a
boot `.EFI` at `/boot/efi/EFI/BOOT/BOOTX64.EFI` /
`.../BOOTAA64.EFI` and uses the same pool (`zroot`), root dataset
(`zroot/ROOT/alpine`), and `bootfs` property this project reads.
Pointing that installer at this project's release assets instead of
its current boot-image source is a small, separate change to that
repo, not done here.

## ZFS layout and Boot Environments

Boot environments are whatever datasets live directly under
`<pool>/ROOT`, across **every pool currently importable** - not just
the one this image was built for. `/init` always does a best-effort
`zpool import` of every other pool it can find (a second disk plugged
in for a rescue, a golden-image source pool, ...) before starting
`menu.py`, so boot environment selection, management, and deploy all
see the full picture. `/init` also runs `zpool status -x` right after
importing the named pool and prints a clear warning if it isn't
reported healthy (DEGRADED, FAULTED, a resilver in progress, ...)
rather than silently proceeding as if nothing were wrong.

### Managing boot environments

Take a snapshot, clone a snapshot into a brand-new independent boot
environment, roll back to a snapshot (destroying anything newer),
delete a snapshot or an entire boot environment, set any boot
environment as the pool's default (`bootfs`), or set a persistent
kernel cmdline addition for a boot environment (an
`org.alpinezfsboot:commandline` ZFS property, readable immediately
after `zpool import` with no mount needed - see
[Editing the boot cmdline](#editing-the-boot-cmdline-for-one-boot-only))
- all interactively, nothing hand-typed. Deleting an entire boot
environment asks you to type its name back as confirmation, since
`zfs destroy -r` is permanent.

### Deploying a new machine (zfs recv)

No installer, in the traditional sense. A new machine is a target
pool plus `zfs recv` of a golden image plus a handful of per-machine
identity values - that's the whole thing, and it's the menu's "Deploy
new machine" option:

```
zpool create -f <pool> <vdev spec>                 # e.g. /dev/sda
ssh|dbclient <host> zfs send -R -c <dataset@snap>  \
    | zfs recv -u <target dataset>                  # or a local source
zpool set bootfs=<target dataset> <pool>
```

then personalization - exactly the handful of things that must be
unique per machine, nothing else:

```
echo <hostname> > <target>/etc/hostname
rm <target>/etc/hostid; zgenhostid -o <target>/etc/hostid
rm <target>/etc/ssh/ssh_host_*     # Alpine's own sshd OpenRC script
                                    # regenerates these on first start
rm <target>/etc/machine-id
```

The golden image itself is never "sysprepped" beforehand - it stays an
ordinary, immutable snapshot; every machine deployed from it goes
through this identical personalization step once, here, after receive,
not before snapshotting. `/etc/hostid` matters specifically because
OpenZFS uses it to tell which host last imported a given pool, and it
must be unique across machines sharing that golden image.

`dbclient` (dropbear's own ssh client - a genuinely separate package,
`dropbear-dbclient`, from the `dropbear` server package - no separate
`openssh-client` dependency) is used for the outbound leg when the
source is remote (`host:dataset@snap`); a bare `dataset@snap` (no `:`)
is treated as already-locally-importable (e.g. a plugged-in USB disk).

## Encryption

Full support for native ZFS encryption - a locked root dataset isn't a
dead end, and unlocking it is a first-class rescue operation, not a
side effect of picking a kernel or chrooting in.

`zfs-unlock.sh` (sourced by `boot-dataset.sh`) is the ONE
implementation of "acquire a passphrase interactively and stage it for
the kexec handoff" and "lock it back up again" - `boot-dataset.sh`'s
own automatic unlock, `menu.py`'s explicit **Unlock encrypted
root**/**Lock encrypted root** menu actions (via the standalone
`zfs-unlock` executable, since Python can't source a shell function
directly), and `chroot_be()`/kernel-listing's own `ensure_key_loaded()`
all go through it, so unlocking via the menu, inspecting/chrooting,
then choosing **Boot** always keeps the single-passphrase-boot
property intact rather than making the *target* initramfs prompt
again.

Three states, tracked independently of each other:

- **LOCKED** - `keystatus` unavailable, no handoff secret staged.
- **UNLOCKED, handoff READY** - `keystatus` available, and a passphrase
  is staged in tmpfs for `boot-dataset.sh`'s own kexec handoff - boot
  will not prompt again.
- **UNLOCKED, handoff NOT READY** - `keystatus` available, but nothing
  is staged (e.g. someone ran a bare `zfs load-key` by hand over the
  rescue shell) - a legal state, but boot *will* re-prompt in the
  target initramfs unless fixed via another unlock first.

Before mounting any dataset (in `boot-dataset.sh`'s single shared
implementation, so this covers the automatic path, every menu boot
path, and `deploy()`'s target alike), its `encryptionroot` and
`keystatus` are checked:

- Unencrypted (`encryptionroot` is `-`) - one extra, cheap `zfs get`,
  otherwise unchanged: no prompt, no delay, exactly as before
  encryption support existed.
- Encrypted, already unlocked, handoff ready - proceeds immediately.
- Encrypted, already unlocked, handoff NOT ready (see the three states
  above) - reprompts for the passphrase and verifies it with `zfs
  load-key -n` (a dry run, documented in `zfs-load-key(8)` as checking
  that the provided key is correct without disturbing an
  already-loaded key), so an already-unlocked dataset is never
  disturbed just to stage a secret for it. A wrong guess here leaves
  the dataset unlocked but the handoff still not ready - not a hard
  failure, since the dataset genuinely *is* readable either way.
- Encrypted and locked, `keylocation=prompt` - prompts for the
  passphrase up to 3 times before giving up, since a mistyped
  passphrase is worth retrying. Every keystroke is captured with
  terminal echo explicitly disabled (never left to `dialog` alone), and
  the input queue is flushed after every attempt so a stray typed
  passphrase can never be replayed into whatever runs next. Echo
  suppression is fail-CLOSED: if the terminal's current state can't be
  saved, or echo can't actually be disabled, no passphrase is ever
  accepted and no dialog is even shown - a missing/broken controlling
  terminal refuses the prompt outright rather than risk echoing a typed
  secret. Restoring the terminal afterward is a separate concern from
  that fail-closed check: by the time restore runs, the secret was
  already captured with echo confirmed off, so a restore failure can't
  expose it - but it's still reported loudly (not silently) and falls
  back to `stty sane`, rather than leaving the terminal stuck echo-off
  with no indication why. A restore failure never discards an
  otherwise-successful unlock.
- Encrypted and locked, `keylocation=file://...` or `https://...` -
  tried exactly once. An unreachable key file or URL fails the same
  way on every attempt, so retrying with no new input three times
  would just repeat the identical failure.

`zfs load-key` always targets the dataset's actual `encryptionroot`,
not necessarily the dataset itself - encryption is inherited from
whichever ancestor dataset is the real encryption root, and loading
the key there is what actually unlocks every descendant at once.

Publishing a staged handoff secret is atomic: the passphrase is
written to a private temporary file in the same tmpfs directory first,
and only renamed onto the canonical stage path once that write has
fully succeeded, so a writer killed mid-write can leave an orphaned
temp file but never a partial/corrupt secret at the path anything else
trusts.

Multiple simultaneous frontends are a supported state, not an edge
case: the local console and any number of rescue SSH sessions are all
independent frontends to the same per-encryptionroot ZFS state, and
automatic boot orchestration (`boot-dataset.sh`'s own unlock, on every
boot) is just another one of them, using the same shared
`zfs_unlock()` primitive - there is no separate "SSH path". Every
unlock/lock/reacquire transaction for a given encryption root is
serialized behind a per-encryptionroot operation lock (a separate lock
from the per-dataset boot lock that prevents double-kexec), and
**that lock is never held while waiting on a human** - a passphrase is
captured with no lock held at all, and only once it's in hand is the
lock acquired (polled for up to ~10 seconds, since contention here is
expected to be another terminal's own brief transition, not another
human typing), with the current state re-read immediately under the
lock since it can have changed while this terminal's own operator was
typing. This is what lets a local console sitting idle at a passphrase
prompt never block a rescue operator from unlocking the same machine
over SSH.

**Lock encrypted root** is the deliberate inverse: refuses cleanly (and
never force-unmounts) if anything sharing the encryption root's own key
is still mounted, otherwise removes the staged handoff secret, runs
`zfs unload-key`, and verifies `keystatus` actually became unavailable
before ever reporting LOCKED - so a plaintext handoff secret can never
outlive a reported "locked" state. The busy check is cryptographically,
not just hierarchically, authoritative: OpenZFS allows a clone to live
anywhere in the pool while still sharing its origin's encryption key,
so this walks every dataset in the pool and asks ZFS which ones
actually report this same `encryptionroot`, rather than assuming
"descendant of the encryption root" and "shares its key" are the same
thing.

A key that never loads (wrong passphrase three times, or an
unreachable file/URL) falls back to a recovery shell rather than
kexec-ing into nothing.

The **Diagnostics** screen's own "rescue status" section shows pool
health, bootfs, encryption/handoff state, bootcheck state, rescue SSH
status, and the current network address(es) together - so connecting to
a machine over rescue SSH answers "what state is this in" in ten
seconds without piecing it together from several separate commands.

## Rescue SSH

If `/EFI/ALPINE/authorized_keys` and `/EFI/ALPINE/ssh_host_ed25519_key`
are both present on the EFI System Partition, `dropbear` sshd is available for
break-glass access - but only started **on demand**, exactly when it's
needed, never on a healthy, unattended boot:

```
Healthy boot:              no SSH at all, ever
Needs the ZFS encryption
  key (keylocation=prompt): network up -> dropbear starts -> operator
                            unlocks -> dropbear stopped -> boot continues
Recoverable boot failure
  (import failed, no
  matching kernel, mount/
  kexec failed, ...):        network up -> dropbear starts -> rescue
                            shell -> repair -> boot continues (dropbear
                            stays up until whoever fixed it is done)
```

No authorized_keys on the ESP, no listening sshd at all, ever: nothing
to scan, nothing to harden, because it isn't there. And even WITH
material configured, a boot that never hits one of the two trigger conditions
above never opens the port either - "configured for break-glass use"
and "actually listening right now" are deliberately not the same
thing.

**Scope**: this recovers from the class of "boot broken, but the
initramfs itself (this shell, or menu.py) is still alive and running"
- ZFS import failure, an encrypted root waiting on a key, a missing
boot environment, a mount/kexec failure. A genuine kernel panic is
**not** recoverable this way - a panicked kernel runs nothing,
including dropbear. See
[Failed-boot handling and diagnostics](#failed-boot-handling-and-diagnostics)
below for how `panic=N` and a per-boot-environment attempt counter
together turn that case into a recoverable one too, once the target
reboots.

Network bring-up (`net-config.sh`) and the dropbear listener itself
(`rescue-ssh.sh`) are two independent, separately testable pieces -
dropbear never decides or drives network configuration itself, it only
starts once the network step has already finished. Configurable via
their own `alpine-zfsboot.*` cmdline options:

| Option | Values | Default |
|---|---|---|
| `alpine-zfsboot.net=` | `auto`\|`static`\|`off` | `auto` - sets the default for both families below |
| `alpine-zfsboot.ipv4=` | `dhcp`\|`static`\|`off` | from `net=` |
| `alpine-zfsboot.ipv4.address=` | CIDR, e.g. `203.0.113.5/24` | - (required if `ipv4=static`) |
| `alpine-zfsboot.ipv4.gateway=` | address | - (optional even under static) |
| `alpine-zfsboot.ipv6=` | `auto`\|`dhcp`\|`static`\|`off` | from `net=` |
| `alpine-zfsboot.ipv6.address=` | CIDR, e.g. `2001:db8::5/64` | - (required if `ipv6=static`) |
| `alpine-zfsboot.ipv6.gateway=` | address | - (optional even under static) |
| `alpine-zfsboot.ssh.listen=` | `ipv4`\|`ipv6`\|`both` | `both` |
| `alpine-zfsboot.ssh.port=` | 1-65535 | `22` - noise reduction only, **not** a security control |
| `alpine-zfsboot.ssh.allow=` | CIDR, e.g. `2a01:db8::123/128` | unset (reachable from anywhere, key-only auth still applies) |

`ipv6=auto` is kernel-native SLAAC (router-advertisement autoconfig,
no daemon) - genuinely different from `ipv6=dhcp` (a real, separate
DHCPv6 client, `udhcpc6`).

`alpine-zfsboot.ssh.allow=` is enforced with a real `nft` (nftables)
rule applied before dropbear ever starts listening, not a dropbear-
side option - dropbear's own `authorized_keys` restriction set is a
documented *subset* of OpenSSH's and has no `from=` equivalent; an
unknown restriction there is silently ignored, not enforced, which
would have been a dangerous way to rely on it. If `nft` isn't
available in a given build, or the CIDR doesn't parse, rescue SSH
**refuses to start** rather than silently listening unrestricted -
default-deny, never default-open, on any part of this feature failing.

This is deliberately separate from the menu's own **Network** item,
which brings up the same DHCP on demand, from the local console, with
no rescue-SSH material involved at all - for reaching a golden-image
host for `deploy()`, or just checking connectivity, on a boot that
never had a key baked in.

`apk` itself lives in the recovery shell too - `apk add curl`,
`dosfstools`, anything else Alpine packages, over whatever network the
boot already has. Nothing installed this way survives a reboot (this
whole rescue root is a tmpfs, rebuilt fresh from the initramfs every
boot).

**Trust material lives entirely on the ESP, in two files with two
separate jobs** (alongside `config`, see
[Failed-boot handling and diagnostics](#failed-boot-handling-and-diagnostics)
for that one):

| File | Job |
|---|---|
| `/EFI/ALPINE/authorized_keys` | who may connect - one **bare** public key per line (blank lines/comments OK); this is *not* full OpenSSH `authorized_keys` syntax - operator options (`from=`, `restrict`, `expiry-time=`, ...) are rejected outright, not silently stripped, so alpine-zfsboot stays the sole owner of what restrictions apply |
| `/EFI/ALPINE/ssh_host_ed25519_key` | this one machine's own persistent SSH host identity |

`ssh_host_ed25519_key` is generated **once**, by the installer
(`dropbearkey -t ed25519`), and persists with the machine for its
whole lifetime - a genuine per-machine identity, the same semantics as
any normal server's own host key. If it's missing or fails to
validate, rescue SSH refuses to start rather than silently falling
back to a throwaway one.

- Two separate trust domains, on purpose: "can SSH into the installed
  Alpine system" (the target OS's own, ordinary `/root/.ssh/authorized_keys`,
  set by the installer's `PUBKEY`) must never silently double as "can open
  this pre-boot rescue environment with raw ZFS access to every dataset" -
  `rescue-ssh.sh` never reads or copies anything from a target boot
  environment's own SSH configuration.
- Password logins are disabled outright (`dropbear -s -g`), an
  explicit configuration choice - public-key auth against the
  configured key(s) is the only way in, by design.
- Every accepted `authorized_keys` line gets
  `no-port-forwarding,no-agent-forwarding,no-X11-forwarding` injected
  **unconditionally**, but deliberately **no** `command=`, and
  deliberately **not** the bare `restrict` keyword dropbear also
  supports: `restrict` is that same set *plus* `no-pty`, and denying
  pty allocation would break the one interactive workflow this access
  exists for (`ssh host` with no command landing in `menu.py`, e.g. to
  unlock an encrypted root remotely). Instead, root's login shell is
  set to `/alpine-zfsboot-shell`, which dispatches per-connection: a
  plain interactive session (`ssh host`) gets the same `menu.py` TUI as
  the local console; an explicit command (`ssh host "some command"`) is
  checked against an **allowlist** - only `zfs send ...`/`zfs recv ...`,
  and only if the whole command is free of shell metacharacters, is
  actually run; anything else is refused outright, never reaching a
  real shell. This is what makes outbound `zfs send`/`recv` and inbound
  `zfs send ... | ssh host zfs recv ...` work, without giving every
  accepted key unrestricted root command execution in this pre-boot
  environment.

## Failed-boot handling and diagnostics

Once `kexec -e` hands off to the target kernel, alpine-zfsboot's own
kernel cannot regain control under any circumstances - not a missing
feature, the actual semantics of kexec. If the target then panics or
hangs, nothing here can intervene *during* that boot. What it can do is
notice, on the **next** boot, that the previous attempt never
confirmed success, offer rescue SSH once enough consecutive failures
have accumulated, and - once an operator connects - show whatever real
evidence survived, from whichever sources actually captured something.

### Bootcheck

- **`alpine-zfsboot.panic_timeout=<seconds>`** (default `10`, `0` is a
  legal, explicit "never auto-reboot on panic") is injected into the
  *target*'s own cmdline as `panic=<seconds>`, unless the persisted/
  overridden cmdline already names its own `panic=`. This is what
  turns a real kernel panic on the target into an automatic reboot at
  all - without it, a panic with no watchdog just hangs forever. This
  is general boot policy, independent of bootcheck below - it applies
  even with bootcheck disabled, and it does **not** help a plain hang
  with no panic (e.g. a rejected ZFS encryption passphrase blocking
  forever on the target's own prompt) - that needs an external
  hardware/BMC/hypervisor watchdog, out of scope here.
- **Bootcheck** tracks, per boot environment, how many kexec attempts
  in a row have gone by without the target confirming it actually
  reached its own `default` runlevel. Given a reboot does eventually
  happen (via `panic=`, a watchdog, or a human/BMC power-cycle), this
  is what decides what alpine-zfsboot does about it *next* boot - and
  it is the ONLY signal Last Boot Diagnostics' own "previous boot
  FAILED/OK" headline is ever derived from, never any evidence
  source's own presence or absence.

State lives in a ZFS user property on the boot environment's own
dataset, `org.alpinezfsboot:bootcheck` - readable/writable pre-mount,
even with the encryption key not yet loaded (property metadata is
never encrypted), and works identically under BIOS and UEFI:

| Value | Meaning |
|---|---|
| absent | Not participating - always auto-boot, nothing read or written. |
| `off` | Explicitly disabled by the operator - same effect as absent. |
| `armed:K` | Participating; `K` = consecutive un-confirmed kexec attempts since the last confirmed success. |

alpine-zfsboot increments `K` right after a successful `kexec -l` into
an `armed:K` boot environment - but **only** if that boot environment's
own mounted filesystem actually has the target-side confirm service
(`alpine-zfsboot-bootcheck`, installed by the installer, registered in
the `default` runlevel) present. This is deliberately *content*-derived,
not just property-derived: a hand-cloned boot environment, one rolled
back to a pre-service snapshot, or one that had the service removed
later must never accumulate a count it can never reset - if the confirm
service isn't there, alpine-zfsboot disarms back to `armed:0` instead
of incrementing. The confirm service itself resets `K` back to `0`
once the target reaches `default`, and also clears any stale
kernel-crash evidence at the same time (see below) - an ordinary
confirmed boot always re-arms at `0`.

Once `K` reaches the threshold (default `3`), `/init` tries rescue SSH
before deciding anything: if it actually starts and stays up, boot
stops there and the menu opens with a warning naming the boot
environment and attempt count; if rescue SSH can't actually be reached,
alpine-zfsboot auto-boots anyway rather than stranding a headless
machine unreachably - unless `alpine-zfsboot.bootcheck=force` says to
stay in rescue regardless, for an operator who has local/IPMI console
access and wants the machine to stop rather than keep silently
retrying.

| Option | Values | Default |
|---|---|---|
| `alpine-zfsboot.bootcheck=` | `off`\|`force` | unset - `off` disables gating for this one boot; `force` stays in rescue even if rescue SSH itself fails to start |
| `alpine-zfsboot.bootcheck.max=` | 1-99 | `3` - out-of-range/non-numeric falls back to the default with a warning, not silently coerced |
| `alpine-zfsboot.panic_timeout=` | seconds, `0` is legal | `10` |

The menu's boot-environment management screen has its own "View /
reset failed-boot tracking" action for inspecting or manually clearing
`K` by hand. `deploy()` (zfs recv) always arms a freshly received boot
environment at `armed:0`, regardless of whatever the source stream's
own property said. Cloning a boot environment copies its bootcheck
*policy* (`armed:*` or `off`) as a fresh `armed:0`, never the origin's
own attempt history.

### Last Boot Diagnostics

The `Previous boot diagnostics` menu item (available locally and over
rescue SSH) combines three independent evidence sources into one
screen, none of which are allowed to speak for the others:

**Boot-attempt metadata.** Right before every `kexec -l`,
`boot-dataset.sh` records the exact kernel path, initramfs path, and
cmdline used, and a wall-clock timestamp, as ZFS properties on the
boot environment's own dataset
(`org.alpinezfsboot:attempt_time`/`attempt_kernel`/`attempt_initrd`/
`attempt_cmdline`). This is provenance, not a verdict - it records
*what was tried*, never why it did or didn't work, and it's the anchor
every other evidence source below is checked against for freshness.

**OpenRC's `rc.log`.** The installer enables OpenRC's own stock
service logger (`rc_logger="YES"` in `/etc/rc.conf`), which writes
every service's start/stop output to `/var/log/rc.log` on the target's
real ZFS filesystem. On the next rescue boot, alpine-zfsboot mounts the
failed boot environment read-only and reads it directly - no special
persistence mechanism needed, since the evidence already lives on
disk. This is the primary evidence source for userspace/startup
failures (a bad service, a bad fstab entry, broken networking) - not a
secondary one alongside kernel-crash evidence. If the log's own
modification time predates the recorded boot attempt, it's shown as
`STALE` rather than as this attempt's evidence: that fact alone means
the log wasn't written to during this attempt, and nothing more
specific than that is claimed about why.

**EFI pstore.** On UEFI platforms where the kernel actually registers
a pstore backend (`efi_pstore`, loaded with `pstore_disable=0` - it
ships disabled by default on Alpine's own kernel), a kernel panic or
oops is captured into firmware-owned NVRAM and survives a real reboot.
Capability is read live from `/sys/module/pstore/parameters/backend`,
never inferred from whether the pstore filesystem merely *mounted* -
Alpine's kernel has `CONFIG_PSTORE=y` built in regardless of whether
any actual backend is registered, so mount success alone proves
nothing. EFI pstore needs no physical memory address of any kind:
NVRAM is addressed by name, not by a memory offset that would
otherwise have to be hand-picked per machine to be safe. Legacy BIOS
boots have no EFI variables
- kernel-crash evidence is honestly `UNSUPPORTED` there, not silently
unavailable in a way indistinguishable from a clean boot. The
confirm service that resets `K` to `0` on a confirmed-good boot also
erases any pstore record still sitting there, so stale evidence from
days or reboots ago is never shown as if it were current.

All raw text shown on this screen (rc.log content, pstore record
content) has ANSI/control sequences stripped and is capped in size
before being displayed - presentation hardening against a terminal
being corrupted by an escape sequence embedded in untrusted,
target-sourced text, not a change to what the evidence means.

**What this does not cover**: an initramfs, root-mount, or `fsck`
failure that happens before the target ever reaches a real root
filesystem has no dedicated capture mechanism under this project's own
constraints (stock kernel, no magic addresses) - the boot-attempt
metadata above is the only evidence for that category, by design, not
a gap expected to close with a future patch.

## Recovery operations

### Diagnostics

`zpool status`, every other importable pool, `/proc/partitions`, and
the last 40 lines of `dmesg` on one screen - for troubleshooting a
boot failure without hand-typing each command separately in the
recovery shell. Nothing here needs a new package: every command is
already ZFS-native or a kernel pseudo-file/busybox applet this project
bundles anyway.

### Chroot into a boot environment

Mounts a chosen boot environment (read-write by default, read-only on
request), bind-mounts this rescue environment's own `/proc /sys /dev
/tmp` into it, and `chroot`s in - trying `bash`, then `sh`, then
`busybox sh` inside the target, since a boot environment is a real
installed Alpine system that may not have `bash` itself installed.
`exit` unmounts everything (lazily - something inside may still hold a
reference) and returns to the menu. For fixing
`/etc/fstab`, a broken package, or anything else directly, without a
manual mount dance every time.

### Editing the boot cmdline for one boot only

A kernel cmdline addition for this boot alone - never written back to
the boot environment's own persisted cmdline. For troubleshooting a
single boot (a debug flag, `single`, ...) without permanently changing
how that boot environment boots every other time.

The *persisted* addition (set via "Manage boot environments") comes
from an `org.alpinezfsboot:commandline` ZFS property if one is set -
readable straight after `zpool import`, no mount needed - or else a
plain `/etc/alpine-zfsboot-cmdline` file inside the boot environment,
for anyone who'd rather edit a file on a mounted root than run
`zfs set`. The property is checked first.

### Recovery shell

`bash`, not a bare `ash`/`sh` - deliberately a bit heavier than a
minimal rescue environment strictly needs, in exchange for
tab-completion, history, and line editing actually being pleasant to
use during an incident. Available from the local console or over
rescue SSH identically.

### Menu (always shown, FreeBSD-loader-style)

```
alpine-zfsboot
0) Boot default (<bootfs>)
1) Unlock encrypted root
2) Lock encrypted root
3) Select boot environment
4) Select kernel (on <bootfs>)
5) Manage boot environments (snapshot/clone/rollback/delete)
6) Chroot into a boot environment
7) Diagnostics (pool status / disks / dmesg)
8) Edit cmdline & boot
9) Deploy new machine (zfs recv)
10) Switch console
11) Network
12) Recovery shell (bash)
13) Boot log
14) Previous boot diagnostics
```

`Unlock encrypted root`/`Lock encrypted root` always show current state
right in their own labels (`LOCKED`, `unlocked, handoff ready`,
`unlocked, handoff NOT ready`, or `not encrypted`) - see
[Encryption](#encryption) for the full three-state model.
`Previous boot diagnostics` does the same for bootcheck's own
`FAILED`/`OK`/`UNKNOWN` state - see
[Failed-boot handling and diagnostics](#failed-boot-handling-and-diagnostics).

The real interactive menu is visible from the very first frame - no
plain-text placeholder screen beforehand. Its own countdown is built
from repeated `dialog --menu` calls (each a few seconds long) rather
than one call with dialog's `--timeout` set to the full duration, so
navigating with arrow keys and confirming a choice (Enter) dispatches
immediately at any point during the countdown, never delayed by the
countdown itself. Left completely alone for the full
`alpine-zfsboot.timeout=` window, it
boots the default; pressing Cancel (labeled `Boot default`) does the
same immediately; pressing ESC stops the countdown outright and drops
into a fully unhurried menu with no time pressure at all.

Every build (both `console=tty0` and `console=ttyS0`/`ttyAMA0` on the
cmdline, always) remembers which console you last used via a
real UEFI NVRAM variable (the same mechanism systemd-boot's
`LoaderEntryDefault`/GRUB's `grubenv` use to persist a choice across
reboots), defaulting on a first boot to whichever console the kernel
itself already treats as primary. `Switch console` flips between
vga/serial live, within the same boot, not just "takes effect next
time".

## Building

Only needed if you're changing something under `init/` or `build.sh` -
see [Quick start / installation](#quick-start--installation) for
pre-built images.

```sh
just build x86_64    # -> out/alpine-zfsboot-x86_64.EFI (+ .iso, + loose components, + BIOS boot code)
just build aarch64   # -> out/alpine-zfsboot-aarch64.EFI
just build-all        # both arches
```

Docker only - `build.sh` runs inside a real `alpine:3.24` container
(see the Justfile). Inside that container, nothing here compiles
anything except the tiny `bios/` boot code (x86_64 only) and `efi/`'s
own loader: `zfs-lts` ships prebuilt kernel modules, `mkinitfs`/
`objcopy` just assemble existing files, so even the non-native arch
runs tolerably under Docker's own `--platform` QEMU emulation for a
local build. CI uses a real native `aarch64` runner regardless,
removing the QEMU question entirely rather than assuming it would be
fine. `just build` also depends on `build-tool` (plain host-side Go
cross-compile, no Docker involved) - `cmd/tool`'s own CLI binary is
baked into the bundled initramfs at `/boot/alpine-zfsboot` straight
from this checkout's own source, so the rescue shell's CLI always
matches the image it's running on rather than lagging behind whatever
`unidoc-aports` last happened to package (see build.sh's
`alpine-zfsboot.files` manifest entry for the full reasoning).
Deliberately `/boot`, not `/usr/bin` - the rescue shell's own
`apk add alpine-zfsboot` (a real thing an operator might type out of
habit) must never be able to clobber this exact-match build, so it
lives off `$PATH` at `/boot/alpine-zfsboot` instead. A target OS
that's already installed still gets the CLI via
`apk add alpine-zfsboot` at `/usr/bin/alpine-zfsboot` instead - see
`alpine-installer`'s own README.

### How this is built

- **`init/init`** - PID 1 inside the initramfs. Mounts
  `/proc`/`/sys`/`/dev`/`efivarfs`, parses `alpine-zfsboot.*=` cmdline
  options and any ESP-persisted config, stages rescue-SSH material from
  the ESP if present, starts `dropbear` if it's usable (see
  [Rescue SSH](#rescue-ssh)), imports the
  named pool, reads `bootfs`, checks pool health, best-
  effort-imports every other reachable pool, then always runs
  `menu.py` (as a plain child, in a loop - `menu.py`'s
  `switch_console()` exits with a specific code asking to be relaunched
  attached to a different tty, which is what makes a live console
  switch possible). Any exit other than that falls through to
  `exec boot-dataset.sh` directly, the same safety net a `menu.py`
  crash also uses. Plain POSIX `sh`.
- **`init/boot-dataset.sh`** - `boot-dataset.sh DATASET POOL
  [KERNEL_SUFFIX] [CMDLINE_OVERRIDE]`: unlocks DATASET's encryption key
  if needed (see [Encryption](#encryption)), mounts it read-only,
  finds its newest kernel/initramfs pair (or an explicit one, if
  given), records boot-attempt metadata (see
  [Last Boot Diagnostics](#last-boot-diagnostics-1)), `kexec`s. The one
  authoritative implementation of "how we actually boot something" -
  called both from `/init`'s own automatic path and from every
  `menu.py` choice that boots anything, so there is exactly one place
  this logic lives and is tested. Kernel and initramfs are always
  derived from the *same* filename suffix (never two independent
  `sort -V` picks that could silently cross-pair a kernel with the
  wrong initramfs). Every failure path falls back to `/bin/bash`
  rather than exiting, since this script can be pid 1 itself by the
  time it runs.
- **`init/alpine-zfsboot-shell`** - root's login shell over `dropbear`
  (see [Rescue SSH](#rescue-ssh) for why this exists instead of a
  forced `command=`).
- **`init/menu.py`** - the TUI (boot environment / kernel selection, BE
  lifecycle management, chroot, cmdline editing, deploy, recovery
  shell, Last Boot Diagnostics), rendered through the real `dialog`
  binary (menu/inputbox/yesno/textbox widgets - the same tool behind
  Debian-installer and countless other rescue/installer TUIs) rather
  than Python's own curses module. Python's stdlib is included
  surgically, not the whole `python3` package: the traced import
  closure of `import os, subprocess` under the real interpreter, plus
  the couple of `lib-dynload` extensions that closure needs as
  separate `.so` files. Rendered plain black-and-white
  (`init/dialogrc`, `use_colors = OFF`) - color renders as corruption
  over a real serial connection, so this stays plain everywhere.
- **`build.sh`** - assembles the initramfs via Alpine's own `mkinitfs
  -i init/init` (real, documented flag - swaps in a custom `/init`
  without needing to fight or reimplement mkinitfs's own file/module
  collection), using its native `zfs` feature (kernel modules +
  userspace `zfs`/`zpool` binaries), storage-controller features
  (`ata`/`nvme`/`scsi`/`usb`/`virtio`), `dhcp` (not the broader
  `network` feature), plus a custom file list for the menu/shell/
  rescue pieces above. Deliberately **not** using mkinitfs's `kms`
  feature, despite the obvious-looking need for a VGA console: it
  pulls in real per-vendor GPU drivers and Alpine's ~130-subpackage
  `linux-firmware` dependency chain to go with them, bloating the
  image well past what a text console needs. `base`'s own `simpledrm`
  module (included unconditionally, no `kms` needed) drives a UEFI GOP
  framebuffer directly, which real hardware and OVMF/KVM VMs both
  expose - no vendor driver or firmware needed for a text console.
  Then bundles that initramfs with a kernel into one `.EFI` via
  `objcopy --add-section` at fixed VMAs, using this project's own EFI
  loader (`efi/` - built from source each run via `gnu-efi-dev`, no
  third-party stub of any kind). Finally calls `iso.sh` to wrap that
  same `.EFI` into a bootable ISO, and writes the loose
  kernel/initramfs/cmdline files for GRUB chainloading.
- **`iso.sh`** - wraps an already-built `.EFI` into a bootable ISO
  (El Torito, UEFI-only boot catalog entry backed by a small FAT image
  containing `EFI/BOOT/BOOT{X64,AA64}.EFI`, appended as a real GPT
  partition so the same `.iso` is also directly `dd`-able to a USB
  stick). The exact `xorrisofs` recipe mirrors archiso's own
  `_add_common_xorrisofs_options_uefi()` - the real code every actual
  Arch Linux install ISO ships with - rather than being assembled from
  documentation alone. Pure packaging - no boot-logic involvement at
  all.
- **`.github/workflows/release.yml`** - triggered by pushing a version
  tag (`v0.1.0`, `v0.2.0`, ...): no schedule, no PR trigger, no
  dependency on any other repo or service - these images get baked
  directly into installed systems' boot process with no review gate
  in between, so a release has to be a deliberate `git push --tags`
  decision. Native arch runners, GitHub Release + auto-generated notes
  + `SHA256SUMS`, no GitHub Pages/apk repo involved (these are boot
  binaries, not Alpine packages).

#### `cmd/tool`: `alpine-zfsboot`, the boot-management CLI

`alpine-zfsboot` (`cmd/tool`) is the authoritative management interface
for an installed alpine-zfsboot system, on both firmwares - not part of
the boot process itself, but everything an admin (or `alpine-installer`)
needs after it. Firmware (BIOS vs UEFI) is detected automatically, never
a flag: internally it dispatches between the UEFI backend (a single
self-contained `.EFI`) and the BIOS backend (stage1/stage2 plus the
separate FAT payload/config files), but the four commands below are the
same either way.

```
alpine-zfsboot status              # what's installed, on THIS machine
alpine-zfsboot verify              # the same facts, but fail (exit 1) on any gap
alpine-zfsboot update              # fetch+install the latest release over it
alpine-zfsboot install <disk> \    # write alpine-zfsboot's own artifacts onto an
  --root <path> --firmware <bios|uefi>  # already-partitioned, already-formatted disk
alpine-zfsboot version <path>      # inspect one .EFI file directly (unchanged, standalone)
```

`status`/`verify`/`update` auto-discover the canonical FAT/ESP partition
by volume label + marker files (the same algorithm `/init` itself uses
at boot - refuses to guess if more than one candidate qualifies) - no
path argument needed. `install` is the one exception: it writes onto a
disk that has no bootable identity yet, so both the target disk and
`--firmware` are explicit, required arguments - see that command's own
`--help` for why firmware is never auto-detected there.

`status`/`verify` also report the *boot environment*'s own identity -
the installed kernel's embedded version string, and the OpenZFS version
baked into the initrd's own `zfs.ko` (extracted directly from its ELF
`.modinfo` section, NOT inferred from the kernel version - independent
facts) - alongside a live pool's own active feature flags, for real
`zpool upgrade` decision-making. `Boot compatible:` deliberately stays
`UNKNOWN` rather than computing a false yes/no: there's no sound,
verified OpenZFS-version-to-pool-feature-flag mapping yet, so this
reports the facts it can actually establish and stops there.

Every write (`install`/`update`) goes through the same low-level
primitives regardless of caller - `internal/biosboot`'s `WriteStage1`/
`WriteStage2` (zero the whole reserved extent first, then write, then
read back and verify byte-for-byte - the proven `update-stage2-only.sh`
algorithm, now the tool's own code), `internal/espconfig`'s `WriteFile`
(temp file, fsync, rename, fsync the directory - real atomicity, not a
truncate-in-place), `internal/uefiboot`'s `WriteLoader` (same atomic
rename, with a `.previous` rollback copy). `WriteStage2` refuses to run
if anything on the disk's own partition table - GPT or MBR - actually
overlaps the fixed stage2 extent (LBA 34-97): a real partition, or
(GPT) the partition array's own on-disk footprint. This is a SAFETY
check, deliberately not an ownership one - identity ("is this
alpine-zfsboot's disk") comes from finding the canonical FAT/ESP
partition and deriving its parent disk, the same way `update`/`verify`/
`status` already establish it, not from any specially-named/typed
partition at the stage2 extent itself: an earlier version of this
check required exactly that (a GPT partition named
`alpine-zfsboot-stage2`), and was wrong to - that was never part of
the actual on-disk ABI (stage1 itself never looks stage2 up via
GPT/MBR at all, see `bios/mbr.h`'s own comment: fixed LBA, full stop),
and a real, already-migrated production system has no such
partition-table entry at all despite being a completely valid
installation - see `internal/bootenv.CheckStage2ExtentFree`'s own
comment for the full reasoning, including why an exact-match cosmetic
entry (what `partition_disk()` still creates on a fresh install) is
tolerated rather than treated as a conflict.

`alpine-installer` no longer implements any of this itself: it fetches
this one binary and calls `alpine-zfsboot install`/`verify` - one
authoritative implementation of the on-disk boot ABI, not two (see that
repo's own README).

## Testing

```sh
./tests/run-tests.sh
```

Runs directly on any Linux dev machine, no Alpine container or real
hardware needed - `init/init`, `boot-dataset.sh`, and `alpine-zfsboot-shell`
only ever call a small, fixed set of external commands (`mount`,
`zpool`, `zfs`, `kexec`, `dropbear`, ...), so the suite puts fake stub
commands in their place (`tests/stubs/`, logging their own invocation)
and runs the real, unmodified scripts against them, asserting on
control flow: does it call `kexec -l` with the *matching*
kernel/initramfs pair, does it refuse to start `dropbear` on
garbage input, does `alpine-zfsboot-shell -c "cmd"` run an allowed
`zfs send`/`zfs recv` command rather than being forced through the menu
(and refuse everything else, including anything containing a shell
metacharacter, rather than passing it through unrestricted), does the multi-pool
scan run only when the menu is actually shown, does the
`org.alpinezfsboot:commandline` ZFS property win over the persisted
file when set, does a one-shot cmdline override replace (not append
to) either, does `deploy()` refuse to `zpool create` over an
already-imported pool, does an unencrypted dataset skip the encryption
path entirely while a locked one gets `zfs load-key` on its
`encryptionroot` (never the leaf dataset), does a `keylocation=prompt`
failure retry 3x while a file/URL location is tried exactly once, does
a boot-attempt record land under the exact property names the
diagnostics reader expects, does the "previous boot" headline come
from bootcheck alone even when crash evidence is sitting there at
`K=0`, and so on.

`menu.py`'s non-dialog helpers (`zfs_list`, `list_kernels`,
`list_pools`, `parse_source`, `boot`'s pool-derivation,
`ensure_key_loaded`, the Last Boot Diagnostics evidence functions) are
exercised too; `dialog` itself needs a real terminal, which a CI
runner doesn't have, so the TUI rendering itself isn't covered here
(dialog-driven flows stub the `dialog_*()` functions directly instead
of running the real binary). Needs `bash` specifically (not just any
POSIX `sh`) - the "key held" tests exercise `read -t N -n 1`, which
plain `dash` doesn't support at all.

An empty `ROOTFS`/`STUB_ROOT` env var is the only thing standing
between these scripts and real boot behaviour - it's unset (so every
`"$ROOTFS/etc/passwd"`-style path is byte-identical to the bare path)
unless the test harness sets it to a scratch directory, which is what
keeps a test run from ever touching this dev machine's real
`/etc/passwd` or `/mnt`.

## License

Apache 2.0 — see [LICENSE.md](LICENSE.md).
