#!/bin/sh
# bios-hdd-entry-test.sh - boots the REAL raw-disk stage1+stage2
# (bios/stage1.bin + bios/stage2.bin) in QEMU as a classic MBR hard
# disk (-drive format=raw), NOT the El-Torito ISO path
# bios-iso-entry-test.sh already covers.
#
# Written specifically to close a real gap this project's own
# hardening-pass ledger flagged: two fixes only ever run on this raw-
# disk path - disk.c's DAP transferred-count check (int13_read_native()
# in cdrom_disk.c has its own, ISO-only sibling, already exercised by
# bios-iso-entry-test.sh; disk.c's own disk_read_lba() is a completely
# separate code path, only ever called from stage1.S's own read and
# every stage2 disk read on a REAL DISK boot, never during an ISO
# boot) - and stage1.S's own STAGE2_MAGIC/STAGE2_READ_RETRIES logic
# (stage1.S has no El-Torito equivalent at all - the ISO build enters
# directly at stage2_entry.S, with no stage1 of its own in the loop) -
# had NO software-level test coverage anywhere in this repo before
# this script existed. A REQUIRES REAL HARDWARE disposition for either
# is only honest once emulation has genuinely been tried and found to
# still leave a real, unresolvable gap - this script is that attempt.
#
# Builds a real classic (msdos, not GPT) MBR disk by hand: stage1.bin
# at LBA0 with a real partition-table entry spliced into its own
# already-zero 446-509 byte region, stage2.bin at the fixed LBA34
# (bios/stage1.S's own STAGE2_LBA), a real mkfs.vfat FAT32 partition
# at LBA 2048 (a conventional, comfortably-clear-of-stage2's-own-64-
# sector-budget alignment) holding EFI/ALPINE/{KERNEL,INITRD,CMDLINE}.
# stage2_main.c's own GPT-then-MBR fallback (see that file's own
# comment) means a disk with no valid GPT header at LBA1 correctly
# falls through to mbr_find_partition() - this deliberately exercises
# that exact fallback, the same "BIOS + MBR (msdos)" layout this
# project's own installer supports as a real, distinct configuration
# from "BIOS + GPT".
#
# Needs: gcc/as/ld/objcopy (bios/Makefile's own toolchain), dosfstools
# (mkfs.vfat), mtools (mmd/mcopy), qemu-system-x86_64, python3.
set -eu

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

pass=0
fail=0
ok()  { pass=$((pass + 1)); echo "  ok - $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL - $1"; }

for cmd in gcc as ld objcopy mkfs.vfat mmd mcopy qemu-system-x86_64 python3; do
    command -v "$cmd" >/dev/null 2>&1 || { echo "missing required tool: $cmd" >&2; exit 1; }
done

W="$(mktemp -d)"
trap 'rm -rf "$W"' EXIT

echo "== building bios/stage1.bin + bios/stage2.bin (real disk variant, not ISO) =="
make -C "$REPO_ROOT/bios" stage1.bin stage2.bin ZFSBOOT_VERSION=test >&2

echo "== building a synthetic (but boot-protocol-valid) kernel+initrd+cmdline =="
# Identical generator to bios-iso-entry-test.sh's own (see that
# script's header comment for why these exact sizes: the FAT32 volume
# needs to clear fat_mount()'s own real structural-FAT32 threshold).
python3 - "$W" <<'PYEOF'
import struct, sys
w = sys.argv[1]

SETUP_HEADER_FILE_OFFSET = 0x1f1
fmt = "<BHIHHHHHIHIHHBBHIIIIHBBIIIBBHIIQIIQQIII"
hdr = struct.pack(fmt,
    0, 0, 0, 0, 0xffff,
    0, 0xaa55, 0,
    0x53726448,
    0x020f,
    0, 0, 0,
    0xff, 0x81,
    0,
    0x00100000,
    0, 0, 0,
    0,
    0, 0,
    0,
    0xffffffff,
    0x1000,
    0, 0,
    0,
    0,
    0, 0,
    0, 0,
    0, 0,
    0, 0, 0,
)
assert len(hdr) == 123, len(hdr)

buf = bytearray(SETUP_HEADER_FILE_OFFSET) + hdr
real_mode_bytes = 5 * 512
buf += bytes(real_mode_bytes - len(buf))
buf += bytes([(i * 37 + 11) & 0xff for i in range(20 * 1024 * 1024)])
with open(f"{w}/KERNEL", "wb") as f:
    f.write(buf)

chunk = bytes([(i * 13 + 3) & 0xff for i in range(1024 * 1024)])
with open(f"{w}/INITRD", "wb") as f:
    for _ in range(12):
        f.write(chunk)

with open(f"{w}/CMDLINE", "wb") as f:
    f.write(b"console=ttyS0 alpine-zfsboot.hdd-entry-test=1\n")
PYEOF

echo "== building the FAT32 payload partition (mkfs.vfat + mcopy, real tools) =="
kernel_size=$(wc -c < "$W/KERNEL")
initrd_size=$(wc -c < "$W/INITRD")
img_size_mb=$(( ((kernel_size + initrd_size) / 1024 / 1024) + 8 ))
dd if=/dev/zero of="$W/fat.img" bs=1M count="$img_size_mb" status=none
mkfs.vfat -F32 -n ZFSBOOT "$W/fat.img" >/dev/null
mmd -i "$W/fat.img" ::EFI ::EFI/ALPINE
mcopy -i "$W/fat.img" "$W/KERNEL" ::EFI/ALPINE/KERNEL
mcopy -i "$W/fat.img" "$W/INITRD" ::EFI/ALPINE/INITRD
mcopy -i "$W/fat.img" "$W/CMDLINE" ::EFI/ALPINE/CMDLINE

echo "== assembling the raw MBR disk image (stage1 @ LBA0, stage2 @ LBA34, FAT partition @ LBA2048) =="
python3 - "$REPO_ROOT" "$W" <<'PYEOF'
import struct, sys, os

repo, w = sys.argv[1], sys.argv[2]
SECTOR = 512
STAGE2_LBA = 34
STAGE2_SECTORS = 64
FAT_PART_LBA = 2048
FAT_MBR_TYPE = 0x2E  # ZFSBOOT_FAT_MBR_TYPE, see bios/mbr.h

stage1 = bytearray(open(f"{repo}/bios/stage1.bin", "rb").read())
assert len(stage1) == 512, len(stage1)
assert stage1[510] == 0x55 and stage1[511] == 0xAA, "stage1.bin missing 55AA signature"
# Real installers write this table at install time - stage1.bin's own
# build currently leaves bytes 446-509 all-zero, so this splice is
# always safe today, but that's a fact about stage1.S's own current
# code size (88 of 440 bytes used, per a real `objcopy`d stage1.bin -
# corrected, F22 in unidoc-alip's PR #5 review, from a stale 77 this
# comment previously said), not a guarantee - checked here for real rather
# than assumed, so a future stage1.S that genuinely grows into this
# region fails this script loudly instead of silently splicing a
# partition entry over live boot code in the fixture. One classic MBR
# partition entry, type 0x2E, the same constant bios/mbr.c's own
# mbr_find_partition() looks for.
assert stage1[446:462] == bytes(16), \
    "stage1.bin's own partition-table region (446-461) is no longer zero - stage1.S has grown into it; this splice would overwrite real boot code, not blank space"
fat_sectors = os.path.getsize(f"{w}/fat.img") // SECTOR
entry = struct.pack("<B3sB3sII", 0x00, b"\0\0\0", FAT_MBR_TYPE, b"\0\0\0", FAT_PART_LBA, fat_sectors)
assert len(entry) == 16
stage1[446:462] = entry

stage2 = open(f"{repo}/bios/stage2.bin", "rb").read()
assert len(stage2) <= STAGE2_SECTORS * SECTOR, "stage2.bin exceeds its own STAGE2_SECTORS budget"

disk_path = f"{w}/disk.img"
with open(disk_path, "wb") as f:
    f.write(stage1)
    f.write(bytes((STAGE2_LBA * SECTOR) - len(stage1)))          # LBA1..33: zero (no GPT header here - exercises the MBR fallback)
    f.write(stage2)
    f.write(bytes((STAGE2_SECTORS * SECTOR) - len(stage2)))       # pad stage2's own region to its full budget
    written = STAGE2_LBA * SECTOR + STAGE2_SECTORS * SECTOR
    f.write(bytes((FAT_PART_LBA * SECTOR) - written))             # zero-fill up to the FAT partition's own start
    f.write(open(f"{w}/fat.img", "rb").read())
print(f"disk image: {os.path.getsize(disk_path)} bytes, FAT partition {fat_sectors} sectors @ LBA {FAT_PART_LBA}")
PYEOF

MONITOR_PORT=45679

echo "== booting under QEMU (raw MBR disk, real INT13h path, no acceleration assumed) =="
qemu-system-x86_64 \
    -drive file="$W/disk.img",format=raw \
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

deadline = time.time() + 90
last = None
while time.time() < deadline:
    text = snapshot()
    if text is None:
        time.sleep(1)
        continue
    last = text
    if "starting kernel" in text and "loading initrd" in text and "done" in text.split("loading initrd", 1)[1]:
        kernel_load_window = text.split("loading kernel", 1)[1].split("done", 1)[0] if "loading kernel" in text else ""
        dot_count = kernel_load_window.count(".")
        if dot_count < 1:
            print("PASS_BUT_NO_PROGRESS_DOTS")
            print(text)
            sys.exit(0)
        print("PASS")
        print(text)
        sys.exit(0)
    # stage1.S's own read_failed: prints a single '!' via INT 10h/AH=0x0E
    # teletype output and nothing else (see that file's own comment on
    # why - no room in 440 bytes for a real message) - it lands alone on
    # whatever line the cursor is on at that point, immediately after any
    # firmware banner text already on screen. Matched as an EXACT,
    # trimmed line rather than a bare substring - a bare "!" in text
    # would also match should some future firmware banner ever contain
    # one, the same class of false-positive this project's own
    # bios-iso-entry-test.sh already guards against for '.' (see its own
    # comment on why a bare substring check on a VGA text-mode screen is
    # meaningless).
    if "FATAL" in text or any(line.strip() == "!" for line in text.split("\n")):
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
        ok "raw MBR HDD boot (stage1 -> stage2, real INT13h disk.c path, MBR fallback) loaded kernel+initrd through a non-NULL progress callback and reached 'starting kernel'"
        ;;
    PASS_BUT_NO_PROGRESS_DOTS)
        echo "$screen" >&2
        bad "boot reached 'starting kernel' but printed zero progress-dot characters during the kernel load"
        ;;
    FATAL)
        echo "$screen" >&2
        bad "stage1/stage2 reported a FATAL error (or the stage1 magic-check '!' halt) - see captured screen above"
        ;;
    TIMEOUT)
        echo "$screen" >&2
        bad "boot never reached 'starting kernel' within the timeout"
        ;;
    *)
        echo "$result" >&2
        bad "unexpected test harness output"
        ;;
esac

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
