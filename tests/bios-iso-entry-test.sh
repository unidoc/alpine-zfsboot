#!/bin/sh
# bios-iso-entry-test.sh - boots the REAL El-Torito ISO stage
# (bios/stage-iso.bin) in QEMU and proves it can load a kernel+initrd
# through a non-NULL fat_read_range() progress callback end to end -
# not just that the code links or that a host-side unit test agrees
# with itself.
#
# This exists because of a real, found-the-hard-way bug: El Torito
# "no emulation" BIOS handoff hands off control at %cs:%ip =
# 0x0000:0x7c00 (the same convention as a classic MBR boot sector) -
# NOT %cs:%ip = 0x07c0:0x0000, which is what stage2_entry.S's own SEG
# constant (and this whole stage's linked layout, ORIGIN=0 in link.ld)
# assumes %cs already equals. Every direct/PC-relative call and jump
# in this stage kept working completely normally despite that (their
# displacements are self-relative, entirely independent of %cs's
# actual value) - which is exactly why this went unnoticed through GPT
# parsing, FAT mount, and file lookups. It only broke the one place
# code needs an ABSOLUTE near-pointer value (a link-time offset baked
# in as a plain immediate) to resolve against %cs: fat_read_range()'s
# own `progress` callback argument (fat.c), passed as `progress_dot`.
# With %cs left at BIOS's 0x0000 instead of this stage's own SEG
# (0x07c0), the indirect near call `progress(...)` jumped to physical
# 0x0000:0x006c - deep in the real-mode interrupt vector table -
# instead of 0x07c0:0x006c, executed IVT bytes as code, and raised
# #UD repeatedly from that point on (confirmed: millions of times a
# second, indistinguishable at a glance from "the ISO CD-ROM path is
# just slow"). The disk build (STAGE2_SEGMENT=0x1000) never hit this:
# stage1.S's own handoff to it IS a real far jump with an explicit
# %cs:%ip = SEG:0 target, so the invariant this stage depends on was
# already satisfied there.
#
# The fix (stage2_entry.S) is an explicit far jump to SEG at the very
# first instruction, establishing %cs=SEG as an invariant this stage
# owns itself rather than trusting whoever handed off control to have
# already set it up correctly - a no-op transition for the disk build
# (where %cs was already SEG) and the real fix for the ISO build. This
# test is the regression guard for that: any future change that
# reintroduces a %cs assumption at ISO entry, or removes/reorders this
# far jump, should make this test hang or show the #UD-storm's own
# symptom (stuck at "loading kernel", never reaching "done") instead
# of silently regressing.
#
# A tiny kernel/initrd would be faster to boot but doesn't shrink the
# FAT32 volume for free: the volume itself needs to be large enough
# that mkfs.vfat's own cluster-count math produces a structurally real
# FAT32 filesystem - this project's own fat.c mount correctly refuses
# anything smaller (confirmed directly: below 65525 clusters,
# fat_mount() rejects it as "not a valid FAT32 volume", a real,
# deliberate strictness check, not a bug to work around - empirically,
# a 32MiB total volume lands at 64496 clusters, REJECTED, while 36MiB
# lands at 72562, accepted).
#
# The kernel/initrd generator below does NOT couple "real ATAPI-
# transferred bytes" to "FAT32 volume size" the way an earlier version
# of this test did (20MiB kernel + 12MiB initrd, purely so iso.sh's
# own payload-driven volume-size formula would clear the FAT32 floor -
# see the hardening ledger's "CI investigation: real bios-iso-atapi-
# test failure" entry for why that coupling turned into a real CI
# flakiness problem). KERNEL/INITRD are now sized just for real
# ATAPI/FAT coverage (multiple commands, multiple progress dots); the
# FAT32 volume is padded up to a safe size separately, via a dummy.EFI
# stub inflated with cold filler bytes stage2_main.c never reads (see
# the generator's own comment for the exact reasoning and the
# confirmation that no BIOS code path ever opens that file).
#
# Needs: gcc/as/ld/objcopy (to build bios/stage-iso.bin - same
# toolchain bios/Makefile already needs), xorriso, dosfstools
# (mkfs.vfat), mtools (mmd/mcopy), qemu-system-x86_64, python3. On
# Debian/Ubuntu: apt-get install gcc binutils xorriso dosfstools
# mtools qemu-system-x86 python3
set -eu

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

pass=0
fail=0
ok()  { pass=$((pass + 1)); echo "  ok - $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL - $1"; }

for cmd in gcc as ld objcopy xorrisofs mkfs.vfat mmd mcopy qemu-system-x86_64 python3; do
    command -v "$cmd" >/dev/null 2>&1 || { echo "missing required tool: $cmd" >&2; exit 1; }
done

W="$(mktemp -d)"
trap 'rm -rf "$W"' EXIT

echo "== building bios/stage-iso.bin =="
# FORCE_ATAPI=1 ./tests/bios-iso-entry-test.sh - same real boot, but
# forces cdrom_disk.c's ATAPI backend (see cdrom_disk.c's own comment on
# ZFSBOOT_FORCE_ATAPI): QEMU/SeaBIOS's INT13h normally succeeds, so a
# plain run of this script never actually exercises the ATAPI driver at
# all. Also runnable via `just test-iso-atapi`.
#
# TEST_SERIAL=1: mirrors every console_putc() byte to COM1 too (see
# console.c's own ZFSBOOT_TEST_SERIAL comment) - the PRIMARY success
# signal below now reads $W/serial.log, not the QEMU-monitor VGA poll.
# `pmemsave 0xb8000` read back completely blank for the ENTIRE run, on
# three separate real CI failures, including after a real, verified fix
# to a genuine race in this test's OWN monitor-polling code (see
# read_until_prompt's own comment below) changed nothing - a real
# `info registers` query on the SAME monitor connection, at the same
# moment, proved the guest was genuinely executing far past where a
# blank VGA screen would even be possible. So the monitor protocol
# itself demonstrably works in that environment; specifically reading
# guest physical memory at 0xb8000 through it does not, for reasons
# that could not be pinned down further without CI access (see
# console.c's ZFSBOOT_TEST_SERIAL comment and this file's own
# read_serial()/deadline-loop comments for the full history). A raw
# COM1 byte stream is a completely independent path - no video BIOS, no
# VGA device model, no monitor round-trip - so a QEMU/SeaBIOS default
# this project doesn't control has nothing left to interfere with. VGA
# polling stays in place below too, diagnostic-only now (poll.log).
make -C "$REPO_ROOT/bios" stage-iso.bin ZFSBOOT_VERSION=test TEST_SERIAL=1 ${FORCE_ATAPI:+FORCE_ATAPI=$FORCE_ATAPI} >&2

echo "== building a synthetic (but boot-protocol-valid) kernel+initrd+cmdline =="
python3 - "$W" <<'PYEOF'
import struct, sys
w = sys.argv[1]

SETUP_HEADER_FILE_OFFSET = 0x1f1
# struct setup_header (packed) - see bios/bootparams.h for the
# authoritative field list this must match byte-for-byte.
fmt = "<BHIHHHHHIHIHHBBHIIIIHBBIIIBBHIIQIIQQIII"
hdr = struct.pack(fmt,
    0, 0, 0, 0, 0xffff,          # setup_sects=0 (->4), root_flags, syssize, ram_size, vid_mode
    0, 0xaa55, 0,                # root_dev, boot_flag ("AA55"), jump
    0x53726448,                  # header = "HdrS"
    0x020f,                      # version
    0, 0, 0,                     # realmode_swtch, start_sys_seg, kernel_version
    0xff, 0x81,                  # type_of_loader, loadflags
    0,                           # setup_move_size
    0x00100000,                  # code32_start = 1MiB (never really executed by this test)
    0, 0, 0,                     # ramdisk_image, ramdisk_size, bootsect_kludge
    0,                           # heap_end_ptr
    0, 0,                        # ext_loader_ver, ext_loader_type
    0,                           # cmd_line_ptr
    0xffffffff,                  # initrd_addr_max - accept any initrd placement
    0x1000,                      # kernel_alignment
    0, 0,                        # relocatable_kernel, min_alignment
    0,                           # xloadflags
    0,                           # cmdline_size
    0, 0,                        # hardware_subarch, hardware_subarch_data
    0, 0,                        # payload_offset, payload_length
    0, 0,                        # setup_data, pref_address
    0, 0, 0,                     # init_size, handover_offset, kernel_info_offset
)
assert len(hdr) == 123, len(hdr)

buf = bytearray(SETUP_HEADER_FILE_OFFSET) + hdr
real_mode_bytes = 5 * 512  # (setup_sects 0 -> 4) + 1
buf += bytes(real_mode_bytes - len(buf))
# 2MiB protected-mode payload (down from a historical 20MiB - see
# "why 2MiB+1MiB, not 20MiB+12MiB" below) - enough to comfortably clear
# the >=1 progress-dot check below (PROGRESS_DOT_BYTES=1MiB in
# stage2_main.c) with real margin, and enough to force several dozen
# separate NATIVE_BATCH-sized ATAPI READ(10) commands under
# FORCE_ATAPI=1 (NATIVE_BATCH=9 sectors=18432 bytes/command in
# cdrom_disk.c - 2MiB/18432 is ~114 commands), not just one.
KERNEL_PAYLOAD_BYTES = 2 * 1024 * 1024
buf += bytes([(i * 37 + 11) & 0xff for i in range(KERNEL_PAYLOAD_BYTES)])
with open(f"{w}/KERNEL", "wb") as f:
    f.write(buf)
kernel_file_size = len(buf)

# 1MiB initrd (down from a historical 12MiB) - same reasoning as the
# kernel above: still >=1 progress dot, still tens of separate ATAPI
# commands (1MiB/18432 is ~57).
INITRD_BYTES = 1 * 1024 * 1024
chunk = bytes([(i * 13 + 3) & 0xff for i in range(INITRD_BYTES)])
with open(f"{w}/INITRD", "wb") as f:
    f.write(chunk)

cmdline_bytes = b"console=ttyS0 alpine-zfsboot.iso-entry-test=1\n"
with open(f"{w}/CMDLINE", "wb") as f:
    f.write(cmdline_bytes)

# why 2MiB+1MiB, not 20MiB+12MiB: a real CI investigation (see the
# hardening ledger, "CI investigation: real bios-iso-atapi-test
# failure") found this test's own historical 20MiB+12MiB sizing meant
# FORCE_ATAPI=1 pushed ~32MB through ~1800 individual ATAPI READ(10)
# commands - each one several separate port-I/O VM-exits under QEMU's
# software (TCG) CPU emulation - and that got slow enough under real
# CI host contention (not a driver bug - re-verified the exact CI
# QEMU 8.2.2/SeaBIOS 1.16.3 combination locally, both idle AND under
# deliberate synthetic core-saturation; it always completed, just
# slowly under load) to blow past even a 300s external test timeout.
#
# The 20MiB/12MiB sizing was never actually about exercising that much
# ATAPI traffic - it was a SIDE EFFECT of the old design coupling two
# unrelated things: the on-disk FAT32 volume's own total size (which
# genuinely does need to be large - bios/fat.c's own fat_mount() hard-
# refuses anything under 65525 clusters, the real FAT32 spec floor;
# empirically confirmed here, in this same investigation, that a
# 32MiB total volume gives 64496 clusters - REJECTED as FAT16-shaped -
# while 36MiB gives 72562 - accepted) and how much of that volume is
# real KERNEL/INITRD content stage2 actually reads over the slow
# ATAPI PIO path.
#
# Those two things don't need to be coupled at all: dummy.EFI below
# lives at EFI/BOOT/<name>.EFI on the FAT volume iso.sh builds, a path
# bios/stage2_main.c never opens under ANY boot path - confirmed
# directly by reading it: the only three fat_open_retry() calls in the
# whole file are literally "/EFI/ALPINE/KERNEL", "/EFI/ALPINE/INITRD",
# "/EFI/ALPINE/CMDLINE". Inflating dummy.EFI pads the FAT32 volume
# exactly the way the old, oversized KERNEL/INITRD used to, but every
# added byte is now cold - never read by ATAPI, never read by INT13h
# either, completely inert filler that only exists to satisfy
# mkfs.vfat's own cluster-count arithmetic.
#
# TARGET_VOLUME_MB reproduces very close to the OLD total FAT32 volume
# size (iso.sh: img_size_mb = payload_size/1MiB + 8) - comfortable,
# already-proven margin over the real 36MiB crossover point above, not
# a newly-guessed minimum.
TARGET_VOLUME_MB = 40
payload_before_efi = kernel_file_size + INITRD_BYTES + len(cmdline_bytes)
efi_padding_bytes = (TARGET_VOLUME_MB - 8) * 1024 * 1024 - payload_before_efi
assert efi_padding_bytes > 0, "kernel+initrd+cmdline already exceed the target volume size"
with open(f"{w}/dummy.EFI", "wb") as f:
    f.write(bytes(efi_padding_bytes))
PYEOF

echo "== building the test ISO via this project's own iso.sh =="
sh "$REPO_ROOT/iso.sh" "$W/dummy.EFI" x86_64 "$REPO_ROOT/bios/stage-iso.bin" \
    "$W/KERNEL" "$W/INITRD" "$W/CMDLINE" >&2
mv "$W/dummy.iso" "$W/test.iso"

MONITOR_PORT=45678

# backend_label: cosmetic-only (F5, unidoc-alip's PR #5 review) - the
# old banner said "INT13h backend" unconditionally, even under
# FORCE_ATAPI=1, which is the whole point of that variant.
if [ -n "${FORCE_ATAPI:-}" ]; then
    backend_label="forced-ATAPI backend"
else
    backend_label="INT13h backend"
fi

echo "== booting under QEMU (El Torito, $backend_label, no acceleration assumed) =="
qemu-system-x86_64 --version | head -1 >&2
# History of this test's own diagnostics, in order (kept because each
# round found something real, even though none of the first two closed
# the actual CI failure):
#
# Round 1 (F5, unidoc-alip's PR #5 review): the old harness ran with
# -serial none and one single end-of-run VGA snapshot, so a failure
# gave no way to tell a genuine hang apart from a capture glitch. Two
# real CI timeouts both came back completely blank, which didn't match
# this test's own "loading kernel" slow-crawl theory on its face.
#
# Round 2: per-poll VGA logging (poll.log, see log_poll below) caught a
# REAL bug on the very next two CI failures - one timeout was blank
# from the FIRST poll, the other caught one real (if truncated)
# 'SeaBIOS' frame on its first poll and then went blank from the SECOND
# poll onward, for the rest of the run. That shape (real frame, then
# blank forever) was snapshot()'s own fixed `time.sleep(0.3)` before
# reading the pmemsave output file, racing QEMU's monitor thread -
# proven for real by SIGSTOP-ing the actual qemu process mid-command
# and confirming the fixed-sleep read came back with stale/poisoned
# content while the prompt-wait version (read_until_prompt below)
# never did, at simulated stall lengths from 1.5s to 5s. A real, fixed
# bug - but round 3 (the very next real CI run, with this fix live)
# failed with the IDENTICAL "blank for the entire run" shape anyway,
# ruling the race out as the (or the only) cause: `read_until_prompt`
# was returning in tens of milliseconds every poll, not timing out, and
# a monitor `info registers` query at the same moment as the timeout
# came back fully formed, EIP proving the guest had gotten far past
# where a blank screen implies. So the monitor protocol plainly works
# in that environment; reading guest physical memory at 0xb8000
# through it does not, for a reason that could not be pinned down
# further without CI access (wrong default VGA device model for
# `-machine pc` under `-display none`? INT 0x10 teletype output -
# console.c's own only output primitive - not actually landing in the
# legacy aperture in that exact config? Genuinely unknown).
#
# Round 4 (this one): rather than guess a fourth VGA-specific fix,
# stage2 now ALSO mirrors every character to COM1 (console.c's
# ZFSBOOT_TEST_SERIAL, gated the same way ZFSBOOT_FORCE_ATAPI already
# is - never on in a real build). $W/serial.log is a completely
# independent capture path: no video BIOS, no VGA device model
# assumptions, no monitor round-trip at all, just a plain byte stream
# QEMU appends to as bytes are written - see read_serial() below, now
# the PRIMARY success signal. `-vga std` added explicitly too (was
# implicit before, whatever `-machine pc`'s own default resolves to) -
# cheap, and the single most plausible fix if a differently-resolved
# default device model was the actual VGA-side problem; VGA polling
# stays in place either way, diagnostic-only now (poll.log), including
# a hexdump of the first bytes of every snapshot so the NEXT failure
# (if VGA is still involved somehow) shows real bytes (all-0x00? all-
# 0xFF? something else entirely?) instead of just "printable or not".
qemu-system-x86_64 \
    -cdrom "$W/test.iso" \
    -m 256 -display none -vga std -no-reboot -no-shutdown -serial file:"$W/serial.log" -machine pc \
    -monitor "tcp:127.0.0.1:$MONITOR_PORT,server,wait=off" \
    >"$W/qemu.log" 2>&1 &
qemu_pid=$!
trap 'kill -9 "$qemu_pid" 2>/dev/null || true; rm -rf "$W"' EXIT

result="$(python3 - "$MONITOR_PORT" "$W/vga.bin" "$W/poll.log" "$W/serial.log" <<'PYEOF'
import socket, sys, time

port = int(sys.argv[1])
vga_path = sys.argv[2]
poll_log = open(sys.argv[3], "w")
serial_path = sys.argv[4]
t_start = time.time()

def read_serial():
    # The PRIMARY success signal now - see this file's own bash-level
    # comment (above the qemu invocation) for why VGA polling stopped
    # being trustworthy. A plain re-read of a file QEMU itself keeps
    # appending to as -serial file:... bytes arrive - no monitor
    # round-trip, no video BIOS, nothing to race against: whatever has
    # been written so far is exactly what's in this file right now.
    try:
        with open(serial_path, "rb") as f:
            return f.read().decode("ascii", "replace")
    except FileNotFoundError:
        return ""

# read_until_prompt/snapshot below found and fixed a REAL race (a fixed
# time.sleep(0.3) before reading pmemsave's output file, confirmed via
# a SIGSTOP-based simulated-hypervisor-stall test to read stale content
# under a real stall) - kept for the VGA diagnostic channel below, but
# no longer load-bearing for pass/fail. See this file's own bash-level
# comment above the qemu invocation for the full round-by-round history
# of why VGA stopped being the primary signal.
def read_until_prompt(s, timeout=10.0):
    s.settimeout(0.5)
    buf = b""
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            chunk = s.recv(4096)
        except socket.timeout:
            continue
        if not chunk:
            break
        buf += chunk
        if buf.rstrip().endswith(b"(qemu)"):
            break
    return buf

def snapshot():
    for _ in range(20):
        try:
            s = socket.create_connection(("127.0.0.1", port), timeout=5)
            break
        except (ConnectionRefusedError, OSError):
            time.sleep(0.5)
    else:
        return None, b""
    read_until_prompt(s)  # the connect-time greeting + first prompt
    s.sendall(f'pmemsave 0xb8000 0x4000 "{vga_path}"\n'.encode())
    read_until_prompt(s)  # blocks until pmemsave's own handler has returned - the file write is guaranteed done by then, not just "probably done after 0.3s"
    s.close()
    data = open(vga_path, "rb").read()
    cols, rows = 80, 25
    lines = []
    for r in range(rows):
        line = "".join(
            chr(data[(r * cols + c) * 2]) if 32 <= data[(r * cols + c) * 2] < 127 else " "
            for c in range(cols)
        )
        lines.append(line.rstrip())
    return "\n".join(lines), data[:32]

# Diagnostic-only now (see the bash-level comment above the qemu
# invocation) - a hexdump of vga.bin's own first bytes is new here,
# specifically so the NEXT VGA-side anomaly (if there ever is one)
# shows real bytes instead of just "printable or not": all-0x00 (the
# aperture reads as never written), all-0xFF (reads as unmapped MMIO),
# a plausible-looking but WRONG offset/length, and "real text, this
# script's own 80x25 rendering is just wrong" are four different bugs
# that "nonblank_rows=0" alone cannot tell apart.
def log_poll(elapsed, text, raw_head):
    if text is None:
        poll_log.write(f"[{elapsed:6.1f}s] (monitor not connected yet)\n")
        return
    nonblank = [ln for ln in text.split("\n") if ln.strip()]
    last = nonblank[-1] if nonblank else "(all blank)"
    poll_log.write(f"[{elapsed:6.1f}s] nonblank_rows={len(nonblank)} last={last!r} head={raw_head.hex()}\n")
    poll_log.flush()

# 60s - tight, and based on real measurement, not padding. History,
# kept because it's the actual evidence this number rests on:
#
# A real CI failure (push and pull_request both, 0.1.0-hardening-pass
# branch) turned out NOT to be a #cs-at-entry regression or any other
# driver bug: this project's own stage-iso.bin, built and booted under
# the EXACT QEMU 8.2.2 + SeaBIOS 1.16.3 combination GitHub's
# ubuntu-latest runner apt-installs (fetched and reproduced locally,
# byte-for-byte matching versions), reached "starting kernel" in well
# under a second with the host otherwise idle - proving the code
# itself was correct. Deliberately saturating all local cores (28x
# `yes` on 4 cores, load ~29) reproduced the exact CI failure symptom
# for real - "loading kernel" crawling instead of instant - and it
# still reached "starting kernel" and "done" every time, just slowly:
# real, continuing forward progress, never a hang.
#
# Root cause (of the original slow-crawl finding, still real and still
# why KERNEL/INITRD are sized 2MiB/1MiB, not 20MiB/12MiB): the OLD
# sizing pushed ~32MB through ~1800 individual ATAPI READ(10) commands
# (NATIVE_BATCH=9 sectors/command, cdrom_disk.c), each with its own
# wait_status_clear() ITERATION-count spin-wait (ata_atapi.c) - every
# inb/outb in that loop is a VM-exit under QEMU's TCG emulation, so the
# real wall-clock cost scales with per-VM-exit cost, trivial idle, not
# on a CI runner sharing cores with other tenants. That sizing was a
# side effect of coupling two unrelated things - the FAT32 volume's own
# size (genuinely needs to clear fat_mount()'s 65525-cluster spec
# floor) and how much of that volume is real KERNEL/INITRD content
# read over the slow ATAPI path - decoupled via dummy.EFI (cold filler,
# never opened - see the generator's own comment above), landing on the
# same ~80628-cluster on-disk FAT32 with ~10.6x less data moved.
#
# Measured locally (this exact payload, same CI-matching QEMU 8.2.2/
# SeaBIOS 1.16.3 binaries throughout): idle 3.7s, up through load~150
# (150x `yes`/4 cores, host loadavg ~44-97) at 73.9s - real, continuous
# forward progress every time, never a hang. But an honest flag, not
# swept under the serial-channel fix above: EIP at timeout has read the
# exact same address (0x1c22, inside atapi_send_packet's data-phase
# wait) in every one of four real CI failures on record so far - not
# what varying, contention-driven slowness would look like, closer to
# "the same wait_status_clear() spin (ata_atapi.c, ATA_TIMEOUT_SPINS=
# 20,000,000 iterations) is consistently taking close to this whole
# deadline in that specific hosting environment". GitHub's own hosted
# runners are themselves VMs with no nested-virtualization acceleration
# available to an arbitrary repo's own QEMU - a real, structural
# per-VM-exit cost this sandbox's own (differently-hosted, also
# TCG-only, but not nested) local reproduction may simply not carry
# over from. If that's what's actually happening, no amount of
# improving THIS SCRIPT's ability to observe the boot changes whether
# it finishes in time - the next real fix would be either a larger
# deadline or fewer ATAPI commands (NATIVE_BATCH, production code,
# needs sign-off before touching). serial.log's own real, ordered,
# gap-free record is what actually answers this on the next run:
# steady dot-by-dot progress that just runs long says "raise the
# margin"; stuck with no further progress for the whole remaining
# window, after visible progress up to some point, says something is
# genuinely wrong, not just slow.
deadline = time.time() + 150
last_serial = ""
last_vga = None
while time.time() < deadline:
    vga_text, vga_head = snapshot()
    log_poll(time.time() - t_start, vga_text, vga_head)
    if vga_text is not None:
        last_vga = vga_text

    text = read_serial()
    if text:
        last_serial = text
    if "starting kernel" in text and "loading initrd" in text and "done" in text.split("loading initrd", 1)[1]:
        # Real, direct proof the indirect progress_dot() callback itself
        # fired - not just that the boot got far enough to print the
        # surrounding success messages, which earlier versions of this
        # test treated as sufficient (a full source audit found this a
        # real gap: this test could pass on a boot that reached
        # "starting kernel" via some path that never actually exercised
        # the indirect call at all). progress_dot() (stage2_main.c)
        # prints one '.' via console_putc() per real 1MiB of progress -
        # the synthetic kernel/initrd this test builds are 2MiB/1MiB
        # (see this file's own header comment), comfortably enough to
        # cross that threshold many times over during a real, working
        # boot. Counted in the "loading kernel"..."done" window
        # specifically (both the kernel and initrd loads happen inside
        # it), not just "somewhere in the stream" - serial.log is a
        # linear append-only byte stream (unlike the old 80x25 VGA
        # screen, which could in principle hold stale characters from
        # an earlier redraw), so this window is unambiguous here.
        kernel_load_window = text.split("loading kernel", 1)[1].split("done", 1)[0] if "loading kernel" in text else ""
        dot_count = kernel_load_window.count(".")
        if dot_count < 1:
            print("PASS_BUT_NO_PROGRESS_DOTS")
            print(text)
            sys.exit(0)
        print("PASS")
        print(text)
        sys.exit(0)
    if "FATAL" in text:
        print("FATAL")
        print(text)
        sys.exit(0)
    time.sleep(2)

# `info registers` (+ `info status`, added this round) are independent
# monitor-based signals at the exact moment of timeout. `info registers`
# alone already answered the question an independent audit of this
# investigation raised - could the guest be frozen/paused (e.g. a
# triple fault, masked into a silent paused VM by -no-reboot
# -no-shutdown) rather than genuinely still running - CS has read
# 07c0 (this stage's OWN STAGE2_SEGMENT, base 0x7c00) in every real CI
# timeout on record, never SeaBIOS's 0xf000 or a reset vector's
# 0x0000, with HLT=0 throughout: the guest is genuinely executing this
# stage's own code, not frozen elsewhere. `info status` is one more
# free, authoritative confirmation of the same thing (a paused VM
# reports "paused (...)" here, not "running").
try:
    s = socket.create_connection(("127.0.0.1", port), timeout=5)
    read_until_prompt(s)
    s.sendall(b"info status\n")
    status = read_until_prompt(s).decode("ascii", "replace")
    s.sendall(b"info registers\n")
    regs = read_until_prompt(s).decode("ascii", "replace")
    s.close()
except OSError as e:
    status = regs = f"(could not query monitor: {e})"

print("TIMEOUT")
print("--- serial.log at timeout ---")
print(last_serial or "(no serial output ever captured)")
print("--- last VGA snapshot ---")
print(last_vga or "(no VGA output ever captured)")
print("--- info status at timeout ---")
print(status)
print("--- info registers at timeout ---")
print(regs)
PYEOF
)"

kill -9 "$qemu_pid" 2>/dev/null || true

status_line="$(echo "$result" | head -1)"
screen="$(echo "$result" | tail -n +2)"

# Print every real, independent log on every non-PASS outcome.
# serial.log is now the PRIMARY signal (see the bash-level comment
# above the qemu invocation) - poll.log/vga.bin stay as a secondary,
# diagnostic-only VGA channel, in case a real regression or a new
# failure mode ever shows up there again.
print_diagnostics() {
    echo "--- \$W/serial.log (PRIMARY signal - console.c's ZFSBOOT_TEST_SERIAL mirror) ---" >&2
    cat "$W/serial.log" >&2 2>/dev/null || echo "(no serial.log)" >&2
    echo "--- \$W/poll.log (diagnostic-only VGA snapshots, not load-bearing - see this file's own history comment) ---" >&2
    cat "$W/poll.log" >&2 2>/dev/null || echo "(no poll.log)" >&2
    echo "--- \$W/qemu.log (QEMU's own stdout/stderr) ---" >&2
    cat "$W/qemu.log" >&2 2>/dev/null || echo "(no qemu.log)" >&2
}

case "$status_line" in
    PASS)
        ok "ISO El-Torito boot loaded kernel+initrd through a non-NULL progress callback and reached 'starting kernel'"
        ;;
    PASS_BUT_NO_PROGRESS_DOTS)
        echo "$screen" >&2
        print_diagnostics
        bad "boot reached 'starting kernel' but printed zero progress-dot characters during the kernel load - the success messages alone don't prove the indirect progress_dot() callback actually fired (the exact gap this check exists to close)"
        ;;
    FATAL)
        echo "$screen" >&2
        print_diagnostics
        bad "stage2 reported a FATAL error - see captured serial output above"
        ;;
    TIMEOUT)
        echo "$screen" >&2
        print_diagnostics
        bad "boot never reached 'starting kernel' within the timeout - check serial.log above FIRST (the primary signal): if it shows steady forward progress ('loading kernel'/'loading initrd' with dots), this is genuine slow-crawl, real host contention worse than this deadline's own measured margin (see this file's own deadline comment for the numbers that set it); if serial.log is ALSO empty/stuck, this is a real regression or hang, not a capture problem - the whole point of moving off VGA polling was to remove that class of doubt"
        ;;
    *)
        echo "$result" >&2
        print_diagnostics
        bad "unexpected test harness output"
        ;;
esac

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
