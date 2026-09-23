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
make -C "$REPO_ROOT/bios" stage-iso.bin ZFSBOOT_VERSION=test ${FORCE_ATAPI:+FORCE_ATAPI=$FORCE_ATAPI} >&2

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
# -serial file:$W/serial.log, not -serial none: kept as a second,
# independent capture channel (a plain byte stream to a file, no
# polling/timing race), but its own claimed justification turned out
# to be wrong - checked directly, this exact SeaBIOS 1.16.3/QEMU 8.2.2/
# `-machine pc` combination writes NOTHING to the serial port on a
# clean, fully successful local boot either (0 bytes, every time). So
# an empty serial.log proves nothing either way; it's cheap insurance
# against a future SeaBIOS/QEMU version that DOES use it, not
# evidence today. stage2 itself never writes to the serial port
# either (bios/console.c is VGA-text-only, confirmed by reading it).
#
# The real diagnostic gap this review flagged (F5), now actually
# closed: two real CI timeouts both captured a completely BLANK VGA
# snapshot at the moment of failure, which didn't match this test's
# own "loading kernel" slow-crawl theory on its face - a local
# reproduction of that theory (same pinned QEMU/SeaBIOS build,
# synthetic host contention up to load ~97 on 4 cores) never produced
# a blank final screen, always real forward progress. Per-poll logging
# (added for exactly this reason, see log_poll below) caught the real
# cause on the very next two CI runs: one timeout was blank from the
# FIRST poll, the other caught one real (if truncated) 'SeaBIOS' frame
# on its first poll and then went blank from the SECOND poll onward,
# for the entire rest of the run. Blank-forever-after-one-real-frame is
# not "the capture is unreliable" - it is snapshot()'s own fixed
# `time.sleep(0.3)` before reading the pmemsave output file racing
# QEMU's monitor thread, which real host contention can starve for
# longer than that sleep actually waits in wall-clock terms. Fixed by
# waiting for the monitor's own "(qemu) " prompt to reappear (see
# read_until_prompt below) instead of guessing a fixed delay is enough -
# pmemsave's file write is synchronous inside the command handler, so
# the prompt reappearing is an unconditional guarantee the write already
# landed, on any host at any speed. poll.log (still logged every poll,
# printed on every non-PASS outcome) stays in place regardless, in case
# a real regression or a genuinely different failure mode shows up next.
qemu-system-x86_64 \
    -cdrom "$W/test.iso" \
    -m 256 -display none -no-reboot -no-shutdown -serial file:"$W/serial.log" -machine pc \
    -monitor "tcp:127.0.0.1:$MONITOR_PORT,server,wait=off" \
    >"$W/qemu.log" 2>&1 &
qemu_pid=$!
trap 'kill -9 "$qemu_pid" 2>/dev/null || true; rm -rf "$W"' EXIT

result="$(python3 - "$MONITOR_PORT" "$W/vga.bin" "$W/poll.log" <<'PYEOF'
import socket, sys, time

port = int(sys.argv[1])
vga_path = sys.argv[2]
poll_log = open(sys.argv[3], "w")
t_start = time.time()

# The actual bug behind "blank from the very first/second poll onward,
# forever" (found by reading two real CI failure logs side by side: one
# blank from t=0.5s, the other caught one real, partial 'SeaBIOS' frame
# at t=0.5s and then went blank from t=3.0s to the end) - not a VGA/
# pmemsave capture mystery after all. The old code sent the pmemsave
# command, slept a FIXED 0.3s, then read the file regardless of
# whether QEMU's monitor had actually finished executing it yet.
# pmemsave's own file write happens synchronously inside the monitor
# command handler - QEMU does not print its "(qemu) " prompt again
# until that handler returns - so waiting for that prompt to reappear
# (instead of guessing a fixed sleep is enough) is a real, unconditional
# guarantee the write already landed, on any host at any speed. A fixed
# sleep is exactly the kind of thing that "usually" works locally and
# quietly stops working under the same real host-contention/steal-time
# effect this whole investigation keeps running into: 0.3 real seconds
# of wall-clock elapsing is not the same as 0.3 seconds of actual
# scheduled CPU time for QEMU's own monitor thread on a badly-scheduled
# host, so the read can race ahead of a write that hasn't started yet -
# reading a file that's stale from a previous poll (this project's own
# vga_path is reused every poll, never truncated between polls) instead
# of that poll's real content. Reproduced directly, not just reasoned
# about: SIGSTOP-ing the real qemu process itself right after sending
# the pmemsave command (simulating a hypervisor-level scheduling stall
# - a real noisy-neighbor CI host, not just a busy vCPU) and resuming it
# after a controlled delay. The OLD fixed-sleep read consistently came
# back with the STALE poisoned marker written into vga_path beforehand,
# at stall lengths from 1.5s to 5s. The prompt-wait version below
# never did - it blocked for exactly as long as the stall actually
# lasted, then read the real, fresh content every time.
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
        return None
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
    return "\n".join(lines)

# unidoc-alip's PR #5 review (F5) found the old single end-of-run VGA
# snapshot could not tell a genuine hang apart from a capture glitch:
# a real local reproduction of the slow-crawl theory (this same pinned
# QEMU/SeaBIOS build, host contention up to load ~97 on 4 cores) always
# showed real forward progress on every poll, never a blank final
# screen - the opposite of both real CI timeouts on record, which both
# came back completely blank despite `info registers` proving execution
# had already gotten well past FAT mount and into the ATAPI transfer
# loop. Logging every poll (not just the last one) turns "was this
# actually stuck, or did one snapshot just come back wrong" from a
# guess into something the next failure's own log answers directly.
def log_poll(elapsed, text):
    if text is None:
        poll_log.write(f"[{elapsed:6.1f}s] (monitor not connected yet)\n")
        return
    nonblank = [ln for ln in text.split("\n") if ln.strip()]
    last = nonblank[-1] if nonblank else "(all blank)"
    poll_log.write(f"[{elapsed:6.1f}s] nonblank_rows={len(nonblank)} last={last!r}\n")
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
# Root cause: this test's OLD 20MiB kernel + 12MiB initrd meant
# FORCE_ATAPI=1 pushed ~32MB through ~1800 individual ATAPI READ(10)
# commands (NATIVE_BATCH=9 sectors/command in cdrom_disk.c), each with
# its own wait_status_clear() spin-wait (ata_atapi.c) - a deliberate
# ITERATION-count timeout, not wall-clock (a real-time budget would
# need interrupts enabled for the whole wait, which this driver can't
# assume this early in boot - see that constant's own comment). Every
# inb/outb in that spin loop is a VM-exit under QEMU's software (TCG)
# CPU emulation, so the REAL wall-clock cost of ~1800 commands' worth
# of these scales with whatever this host's own per-VM-exit cost
# happens to be at the moment - trivial on an idle dedicated machine,
# not on a CI runner sharing physical cores with other tenants at the
# hypervisor level (real, well-documented CPU steal-time noise,
# orthogonal to GitHub Actions' own per-job VM isolation). Raising
# this deadline alone (90s, then 300s, then 900s - each one still not
# comfortably clearing a real subsequent CI run) never fixed anything:
# it only bought the same ~1800-command cost more time to finish in,
# which also means a REAL ATAPI regression could take up to however
# long this deadline is to even get flagged.
#
# The actual fix: this test's OLD 20MiB/12MiB sizing was never really
# about exercising that much ATAPI traffic - it was a side effect of
# coupling two unrelated things (see the kernel/initrd generator above
# for the full explanation and the real fat_mount()/mkfs.vfat cluster-
# count numbers): the on-disk FAT32 volume's own total size, which
# genuinely needs to be large, and how much of that volume is real
# KERNEL/INITRD content this test actually reads over the slow ATAPI
# PIO path, which does not need to be anywhere near that large.
# dummy.EFI now carries ALL of the volume-padding weight (cold filler
# bytes stage2_main.c never reads - confirmed directly: it only ever
# opens /EFI/ALPINE/{KERNEL,INITRD,CMDLINE}), while KERNEL/INITRD
# shrank to 2MiB/1MiB - still comfortably multiple ATAPI commands
# (~171, not 1) and multiple progress dots, still the real native
# ATAPI path, real FAT32 traversal, real kernel/initrd loading -
# proven identical on-disk FAT32 (80628 clusters, same as before) and
# a real successful boot, just ~10.6x less data moved.
#
# Measured (this exact 2MiB/1MiB payload, same CI-matching QEMU
# 8.2.2/SeaBIOS 1.16.3 binaries used throughout this investigation) -
# first round:
#   idle host:                                          3.7s
#   load ~26 (28x `yes`/4 cores)                         5.1s
#   load ~48 (56x `yes`/4 cores)                         9.7s
# 60s (>6x that worst case) still wasn't enough: it failed again in
# real CI. A second, much harder round pushed contention far past what
# the first round tried, on the same pinned QEMU/SeaBIOS build:
#   load ~28 (28x `yes`/4 cores)                        13.1s
#   load ~56 (56x `yes`/4 cores)                        26.8s
#   load ~100 (100x `yes`/4 cores)                       53.7s
#   load ~150 (150x `yes`/4 cores, host loadavg ~44-97)  73.9s
# every one of these completed with real, continuous forward progress
# (per-poll VGA text logged the whole way - see log_poll above) -
# never a hang, never a blank final screen. Real CI still failed at
# 60s twice more after this measurement, both times blank - but
# poll.log from those two runs (not available until it was added)
# showed why: not a genuine hang, and not the host actually running
# THAT much slower than 73.9s worth - a fixed-sleep race in this
# script's own monitor handling (see read_until_prompt above) that
# read the pmemsave output file before QEMU had necessarily finished
# writing it, which real host contention made far more likely to lose.
# That race is now fixed at its source, not worked around here - this
# deadline bump is for the separate, still-real slow-crawl case the
# measurements above establish margin for.
deadline = time.time() + 150
last = None
while time.time() < deadline:
    text = snapshot()
    log_poll(time.time() - t_start, text)
    if text is None:
        time.sleep(1)
        continue
    last = text
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
        # it), not just "somewhere on screen" - a stray '.' anywhere
        # else on a VGA text-mode screen (a period in some other
        # message, cursor/border artifacts) would make a bare substring
        # check meaningless.
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

# `info registers` here is a second, independent signal at the exact
# moment of timeout: it comes straight from the monitor, not from
# polling text-mode video memory. Two real CI timeouts both had this
# show EIP already deep inside atapi_send_packet while poll.log's own
# VGA reads had gone (or started) completely blank - not a real hang,
# not a genuinely unreliable capture channel either: read_until_prompt
# above is the actual fix, a fixed-sleep race in THIS SCRIPT's own
# monitor handling that only broke down under real CI host contention.
# Kept using the same wait-for-prompt read here too, for the same
# reason, though this query only runs once so it was never the
# repeating failure mode poll.log's fix targets.
try:
    s = socket.create_connection(("127.0.0.1", port), timeout=5)
    read_until_prompt(s)
    s.sendall(b"info registers\n")
    regs = read_until_prompt(s).decode("ascii", "replace")
    s.close()
except OSError as e:
    regs = f"(could not query monitor for info registers: {e})"

print("TIMEOUT")
print(last or "(no VGA output ever captured)")
print("--- info registers at timeout ---")
print(regs)
PYEOF
)"

kill -9 "$qemu_pid" 2>/dev/null || true

status_line="$(echo "$result" | head -1)"
screen="$(echo "$result" | tail -n +2)"

# unidoc-alip's PR #5 review (F5): the old harness ran QEMU with
# -serial none and never printed $W/qemu.log, so a failure gave no
# diagnostics beyond one VGA text snapshot - and that snapshot came
# back completely blank on both real CI timeouts this review found,
# which this test's own FAIL message couldn't explain. Print all
# three real, independent logs on every non-PASS outcome now -
# poll.log in particular is the one that actually answers "was this
# genuinely stuck, or did one snapshot just come back wrong".
print_diagnostics() {
    echo "--- \$W/poll.log (every VGA snapshot taken during this run, not just the last one) ---" >&2
    cat "$W/poll.log" >&2 2>/dev/null || echo "(no poll.log)" >&2
    echo "--- \$W/qemu.log (QEMU's own stdout/stderr) ---" >&2
    cat "$W/qemu.log" >&2 2>/dev/null || echo "(no qemu.log)" >&2
    echo "--- \$W/serial.log (SeaBIOS + guest serial output, if any - see this run's own banner comment: empirically always empty with this exact build, so absence here proves nothing) ---" >&2
    cat "$W/serial.log" >&2 2>/dev/null || echo "(no serial.log)" >&2
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
        bad "stage2 reported a FATAL error - see captured screen above"
        ;;
    TIMEOUT)
        echo "$screen" >&2
        print_diagnostics
        bad "boot never reached 'starting kernel' within the timeout - check poll.log above FIRST: if it shows steady forward progress (nonblank_rows climbing, 'loading kernel'/'loading initrd' with dots), this is genuine slow-crawl, real host contention worse than this deadline's own measured margin (see this file's own deadline comment for the numbers that set it) - if it goes blank after one or more real frames, or is blank from the very first poll, and this happens again AFTER read_until_prompt's own fix (see that function's comment - this exact pattern is what it was written to close), that's a new, not-yet-understood capture problem worth a fresh investigation, not another timeout bump"
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
