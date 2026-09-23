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
# A tiny kernel/initrd would be faster to boot but doesn't work here:
# the FAT32 volume itself needs to be large enough that mkfs.vfat's
# own cluster-count math produces a structurally real FAT32 filesystem
# - this project's own fat.c mount correctly refuses anything smaller
# (confirmed directly: an 8MB volume mkfs.vfat itself calls FAT32 gets
# rejected by fat_mount() as "not a valid FAT32 volume", a real,
# deliberate strictness check, not a bug to work around). 20MB
# kernel + 12MB initrd (iso.sh's own payload-driven sizing then lands
# comfortably past that threshold, confirmed empirically) is the
# smallest combination confirmed to produce a real FAT32 volume in
# this project's own testing - keep it at least this large if you ever
# need to change these sizes.
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
# 20MiB protected-mode payload - see this script's own header comment
# for why this size (not something smaller/faster): the FAT32 volume
# it forces iso.sh to build needs to clear fat_mount()'s own real
# structural FAT32 threshold.
buf += bytes([(i * 37 + 11) & 0xff for i in range(20 * 1024 * 1024)])
with open(f"{w}/KERNEL", "wb") as f:
    f.write(buf)

chunk = bytes([(i * 13 + 3) & 0xff for i in range(1024 * 1024)])
with open(f"{w}/INITRD", "wb") as f:
    for _ in range(12):
        f.write(chunk)

with open(f"{w}/CMDLINE", "wb") as f:
    f.write(b"console=ttyS0 alpine-zfsboot.iso-entry-test=1\n")

with open(f"{w}/dummy.EFI", "wb") as f:
    f.write(b"\x00" * 64)
PYEOF

echo "== building the test ISO via this project's own iso.sh =="
sh "$REPO_ROOT/iso.sh" "$W/dummy.EFI" x86_64 "$REPO_ROOT/bios/stage-iso.bin" \
    "$W/KERNEL" "$W/INITRD" "$W/CMDLINE" >&2
mv "$W/dummy.iso" "$W/test.iso"

MONITOR_PORT=45678

echo "== booting under QEMU (El Torito, INT13h backend, no acceleration assumed) =="
qemu-system-x86_64 \
    -cdrom "$W/test.iso" \
    -m 256 -display none -no-reboot -no-shutdown -serial none -machine pc \
    -monitor "tcp:127.0.0.1:$MONITOR_PORT,server,wait=off" \
    >"$W/qemu.log" 2>&1 &
qemu_pid=$!
trap 'kill -9 "$qemu_pid" 2>/dev/null || true; rm -rf "$W"' EXIT

result="$(python3 - "$MONITOR_PORT" "$W/vga.bin" <<'PYEOF'
import socket, sys, time

port = int(sys.argv[1])
vga_path = sys.argv[2]

def snapshot():
    for _ in range(20):
        try:
            s = socket.create_connection(("127.0.0.1", port), timeout=5)
            break
        except (ConnectionRefusedError, OSError):
            time.sleep(0.5)
    else:
        return None
    time.sleep(0.2)
    s.recv(4096)
    s.sendall(f'pmemsave 0xb8000 0x4000 "{vga_path}"\n'.encode())
    time.sleep(0.3)
    s.recv(8192)
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

# 300s, not 90s - raised after a real CI failure (both the push and
# pull_request runs on the 0.1.0-hardening-pass branch timed out here,
# identically) that turned out NOT to be a #cs-at-entry regression or
# any other driver bug: this exact commit, built and booted under the
# EXACT QEMU 8.2.2 + SeaBIOS 1.16.3 combination GitHub's ubuntu-latest
# runner apt-installs (fetched and reproduced locally, byte-for-byte
# matching versions), reaches "starting kernel" in well under a second
# with the host otherwise idle - proving the code itself is correct.
# Deliberately saturating all cores first (28 `yes` processes on 4
# cores, load average ~29) reproduced the exact failure symptom -
# "loading kernel" advancing one dot roughly every 5s instead of
# instantly - and it still reached "starting kernel" and "done" every
# time, just slowly: real, continuing forward progress, not a hang.
# Root cause: FORCE_ATAPI's PIO transfer issues thousands of individual
# inb/outb port operations (~1800 READ(10) commands for a 32MB
# kernel+initrd at NATIVE_BATCH=9, each with its own wait_status_clear
# spin-wait) - every one is a VM-exit under software emulation, and
# wait_status_clear's own 20-million-iteration ceiling (ata_atapi.c) is
# deliberately an ITERATION count, not a wall-clock one (see that
# constant's own comment - a real-time budget would need interrupts
# enabled for the WHOLE wait, which this driver can't assume). That's
# the right call for the driver's own real hang-detection purpose, but
# it means the REAL wall-clock cost of a full transfer scales with
# whatever this host's own per-VM-exit cost happens to be at the
# moment - fine on an idle dedicated machine, not fine on a CI runner
# sharing physical cores with other tenants at the hypervisor level
# (real, well-documented CPU steal-time noise, orthogonal to GitHub
# Actions' own per-job VM isolation). 300s leaves over 100x this
# project's own real, repeatedly-measured ~2.3s idle-host baseline
# (see the hardening ledger) - generous enough to absorb realistic CI
# noise without masking an actual hang (a genuinely wedged device would
# still exceed it, just as before).
deadline = time.time() + 300
last = None
while time.time() < deadline:
    text = snapshot()
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
        # the synthetic kernel/initrd this test builds are 20MB/12MB
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

print("TIMEOUT")
print(last or "(no VGA output ever captured)")
PYEOF
)"

kill -9 "$qemu_pid" 2>/dev/null || true

status_line="$(echo "$result" | head -1)"
screen="$(echo "$result" | tail -n +2)"

case "$status_line" in
    PASS)
        ok "ISO El-Torito boot loaded kernel+initrd through a non-NULL progress callback and reached 'starting kernel'"
        ;;
    PASS_BUT_NO_PROGRESS_DOTS)
        echo "$screen" >&2
        bad "boot reached 'starting kernel' but printed zero progress-dot characters during the kernel load - the success messages alone don't prove the indirect progress_dot() callback actually fired (the exact gap this check exists to close)"
        ;;
    FATAL)
        echo "$screen" >&2
        bad "stage2 reported a FATAL error - see captured screen above"
        ;;
    TIMEOUT)
        echo "$screen" >&2
        bad "boot never reached 'starting kernel' within the timeout - if the screen above is stuck at 'loading kernel' with no progress, this is the #cs=SEG-at-entry regression this test exists to catch"
        ;;
    *)
        echo "$result" >&2
        bad "unexpected test harness output"
        ;;
esac

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
