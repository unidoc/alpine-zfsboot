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
# It boots the same disk in several scenarios (HDD_TEST_SCENARIOS; all of
# them by default), each a separate QEMU run. "boots as before" means the
# change must not stop a machine that booted with the previous loader:
#   boot              CHECKSUM present (mode warn, the default): the loaded
#                     kernel+initrd are verified in RAM ("... ok") and
#                     'starting kernel' is reached
#   nosum             no CHECKSUM (older install): notice, boots as before
#   stale-warn        one KERNEL byte flipped after CHECKSUM was written,
#                     mode warn: both digests printed, WARNING, boots
#   stale-enforce     the same with MODE enforce: boot refused
#   cmdline-enforce   the same, CHECKSUM mode warn but
#                     alpine-zfsboot.integrity=enforce on the cmdline: refused
#   cmdline-off       CHECKSUM MODE enforce, alpine-zfsboot.integrity=off on
#                     the cmdline: not hashed, boots
#   stage2-corrupt    one byte flipped in stage2's tail on disk (marker
#                     present): stage1 halts with '!' instead of jumping in
#   small-ram         -m 64: the initrd (at 64 MiB) lies above the end of RAM,
#                     refused before anything is loaded (the only E820 case
#                     that refuses: no memory is there at all)
#   runtime-overlap   a kernel whose init_size area reaches the initrd:
#                     WARNING, boots as before
#   diag              alpine-zfsboot.diag=1: the E820 map and load ranges are
#                     printed, the boot still succeeds
#   old1-old2 old1-new2 new1-old2   (only when an older release is available:
#                     OLD_STAGE_DIR=<dir with stage1.bin stage2.bin>, or the
#                     git tag OLD_STAGE_REF, default v0.4.1, which is built):
#                     every pairing of an older and this stage1/stage2 boots;
#                     new1-old2 and old1-old2 run with a CHECKSUM MODE enforce
#                     describing a DIFFERENT kernel on the disk, proving an
#                     older stage2 ignores the file
#   cap127 .. cap1    alpine-zfsboot.int13chunk=127/64/32/16/8/1: boots, the
#                     diag counters show no transfer above the cap, and the
#                     payload still matches CHECKSUM
#   cap-invalid       int13chunk=0: a notice, the build default is used
#   int13-limit       (only via bios-hdd-int13-limit-test.sh, which builds with
#                     BIOS_EXTRA_CFLAGS=-DDISK_FAULT_INJECT_MAX_SECTORS=16):
#                     the shrink notice, every failed BIOS call printed
#                     (diag=1), and a payload that still verifies
#
# BIOS_EXTRA_CFLAGS, when set, builds bios/ with those EXTRA_CFLAGS in a
# temporary COPY of bios/, so no test-only object ever lands in the tree.
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

SCENARIOS="${HDD_TEST_SCENARIOS:-boot noblk staleblk nosum stale-warn stale-enforce cmdline-enforce cmdline-off stage2-corrupt small-ram runtime-overlap diag cap127 cap64 cap32 cap16 cap8 cap1 cap-invalid old1-old2 old1-new2 new1-old2}"
BIOS_EXTRA_CFLAGS="${BIOS_EXTRA_CFLAGS:-}"

echo "== building bios/stage1.bin + bios/stage2.bin (real disk variant, not ISO) =="
if [ -n "$BIOS_EXTRA_CFLAGS" ]; then
    mkdir -p "$W/repo"
    cp -r "$REPO_ROOT/bios" "$W/repo/bios"
    rm -f "$W"/repo/bios/*.o "$W"/repo/bios/*.elf "$W"/repo/bios/*.bin
    make -C "$W/repo/bios" stage1.bin stage2.bin ZFSBOOT_VERSION=test EXTRA_CFLAGS="$BIOS_EXTRA_CFLAGS" >&2
    BIOS_REPO="$W/repo"
else
    make -C "$REPO_ROOT/bios" stage1.bin stage2.bin ZFSBOOT_VERSION=test >&2
    BIOS_REPO="$REPO_ROOT"
fi

echo "== building a synthetic (but boot-protocol-valid) kernel+initrd+cmdline =="
# Same generator as bios-iso-entry-test.sh's own (see that script's
# header comment for why these exact sizes: the FAT32 volume needs to
# clear fat_mount()'s own real structural-FAT32 threshold), except that
# the relocation fields carry the values of the Alpine linux-lts kernel
# this project ships (relocatable, 16 MiB alignment/pref_address,
# init_size 0x2c30000), so the loader's E820/init_size checks run as they
# do for the real kernel. Also writes CHECKSUM (computed here with
# Python's hashlib, independently of the Go writer and the C reader),
# KERNEL.BAD (one byte flipped in the loaded part) and KERNEL.BIG (the
# same kernel with an init_size that reaches the initrd at 64 MiB).
gen_payload() {
python3 - "$W" <<'PYEOF'
import hashlib, struct, sys
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
    0x1000000,
    1, 0,
    0,
    0,
    0, 0,
    0, 0,
    0, 0x1000000,
    0x2c30000, 0, 0,
)
assert len(hdr) == 123, len(hdr)

buf = bytearray(SETUP_HEADER_FILE_OFFSET) + hdr
real_mode_bytes = 5 * 512
buf += bytes(real_mode_bytes - len(buf))
# Not periodic per sector (the sector number is mixed in): stale data,
# another LBA's data or a short read must look different from the right
# data, or the fault-injection boots could not see them.
buf += bytes([(i * 37 + 11 + (i >> 9) * 101) & 0xff for i in range(20 * 1024 * 1024)])
with open(f"{w}/KERNEL", "wb") as f:
    f.write(buf)
bad = bytearray(buf)
bad[real_mode_bytes + 10 * 1024 * 1024 + 12345] ^= 0x01
with open(f"{w}/KERNEL.BAD", "wb") as f:
    f.write(bad)
big = bytearray(buf)
struct.pack_into("<I", big, 0x260, 0x3100000)  # init_size: 16 MiB + this reaches 64 MiB
with open(f"{w}/KERNEL.BIG", "wb") as f:
    f.write(big)

chunk = bytes([(i * 13 + 3) & 0xff for i in range(1024 * 1024)])
with open(f"{w}/INITRD", "wb") as f:
    for _ in range(12):
        f.write(chunk)

with open(f"{w}/CMDLINE", "wb") as f:
    f.write(b"console=ttyS0 alpine-zfsboot.hdd-entry-test=1\n")
with open(f"{w}/CMDLINE.DIAG", "wb") as f:
    f.write(b"console=ttyS0 alpine-zfsboot.hdd-entry-test=1 alpine-zfsboot.diag=1\n")
with open(f"{w}/CMDLINE.INT13", "wb") as f:
    f.write(b"console=ttyS0 alpine-zfsboot.hdd-entry-test=1 alpine-zfsboot.diag=1 alpine-zfsboot.int13chunk=64\n")
for cap in ("127", "64", "32", "16", "8", "1", "0", "abc"):
    with open(f"{w}/CMDLINE.CAP{cap}", "wb") as f:
        f.write(b"console=ttyS0 alpine-zfsboot.hdd-entry-test=1 alpine-zfsboot.diag=1 alpine-zfsboot.int13chunk=" + cap.encode() + b"\n")
for mode in ("enforce", "off"):
    with open(f"{w}/CMDLINE.{mode.upper()}", "wb") as f:
        f.write(b"console=ttyS0 alpine-zfsboot.hdd-entry-test=1 alpine-zfsboot.integrity=" + mode.encode() + b"\n")

initrd = open(f"{w}/INITRD", "rb").read()
def line(name, data, off):
    return f"{name} {len(data)} {off} {hashlib.sha256(data[off:]).hexdigest()}\n"
for name, mode in (("CHECKSUM", "warn"), ("CHECKSUM.ENFORCE", "enforce")):
    with open(f"{w}/{name}", "w") as f:
        f.write("alpine-zfsboot payload-sum 1\nMODE " + mode + "\n" + line("KERNEL", buf, real_mode_bytes) + line("INITRD", initrd, 0))
with open(f"{w}/CHECKSUM.BIG", "w") as f:
    f.write("alpine-zfsboot payload-sum 1\n" + line("KERNEL", big, real_mode_bytes) + line("INITRD", initrd, 0))
PYEOF
}
gen_payload

# EFI/ALPINE/BLKSUM (and a second CHECKSUM, to cross-check the Python one
# above) from bios/tests/mkmanifest, the C twin of `alpine-zfsboot
# payload-manifest` (both checked against internal/payloadsum/testdata).
gcc -O2 -I "$REPO_ROOT/bios" -o "$W/mkmanifest" "$REPO_ROOT/bios/tests/mkmanifest.c" \
    "$REPO_ROOT/bios/payload_sum.c" "$REPO_ROOT/bios/blkverify.c"
mkdir -p "$W/m" "$W/mbig" "$W/mbad"
"$W/mkmanifest" --kernel "$W/KERNEL" --initrd "$W/INITRD" --out "$W/m"
"$W/mkmanifest" --kernel "$W/KERNEL.BIG" --initrd "$W/INITRD" --out "$W/mbig"
"$W/mkmanifest" --kernel "$W/KERNEL.BAD" --initrd "$W/INITRD" --out "$W/mbad"
cmp "$W/m/CHECKSUM" "$W/CHECKSUM" || { echo "mkmanifest's CHECKSUM differs from the independent Python one" >&2; exit 1; }
cp "$W/m/BLKSUM" "$W/BLKSUM"
cp "$W/mbig/BLKSUM" "$W/BLKSUM.BIG"
cp "$W/mbad/BLKSUM" "$W/BLKSUM.OTHER" # describes a different kernel

# build_disk KERNEL_FILE CMDLINE_FILE SUM_FILE|- STAGE2_CORRUPT(0/1) [BLKSUM_FILE|-] -> $W/disk.img
# A BLKSUM goes with every CHECKSUM unless the 5th argument says otherwise
# (BLKSUM.BIG with CHECKSUM.BIG).
# The stage1/stage2 used are $S1_DIR/stage1.bin and $S2_DIR/stage2.bin.
S1_DIR="$BIOS_REPO/bios"
S2_DIR="$BIOS_REPO/bios"
build_disk() {
echo "== building the FAT32 payload partition (mkfs.vfat + mcopy, real tools) =="
kernel_size=$(wc -c < "$W/KERNEL")
initrd_size=$(wc -c < "$W/INITRD")
img_size_mb=$(( ((kernel_size + initrd_size) / 1024 / 1024) + 8 ))
rm -f "$W/fat.img"
dd if=/dev/zero of="$W/fat.img" bs=1M count="$img_size_mb" status=none
mkfs.vfat -F32 -n ZFSBOOT "$W/fat.img" >/dev/null
mmd -i "$W/fat.img" ::EFI ::EFI/ALPINE
mcopy -i "$W/fat.img" "$1" ::EFI/ALPINE/KERNEL
mcopy -i "$W/fat.img" "$W/INITRD" ::EFI/ALPINE/INITRD
mcopy -i "$W/fat.img" "$2" ::EFI/ALPINE/CMDLINE
if [ "$3" != "-" ]; then
    mcopy -i "$W/fat.img" "$3" ::EFI/ALPINE/CHECKSUM
fi
blk="${5:-auto}"
if [ "$blk" = auto ]; then
    case "$3" in -) blk=- ;; *BIG) blk="$W/BLKSUM.BIG" ;; *) blk="$W/BLKSUM" ;; esac
fi
if [ "$blk" != "-" ]; then
    mcopy -i "$W/fat.img" "$blk" ::EFI/ALPINE/BLKSUM
fi

echo "== assembling the raw MBR disk image (stage1 @ LBA0, stage2 @ LBA34, FAT partition @ LBA2048) =="
python3 - "$S1_DIR" "$S2_DIR" "$W" "$4" <<'PYEOF'
import struct, sys, os

s1dir, s2dir, w, stage2_corrupt = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4] == "1"
SECTOR = 512
STAGE2_LBA = 34
STAGE2_SECTORS = 64
FAT_PART_LBA = 2048
FAT_MBR_TYPE = 0x2E  # ZFSBOOT_FAT_MBR_TYPE, see bios/mbr.h

stage1 = bytearray(open(f"{s1dir}/stage1.bin", "rb").read())
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

stage2 = bytearray(open(f"{s2dir}/stage2.bin", "rb").read())
assert len(stage2) <= STAGE2_SECTORS * SECTOR, "stage2.bin exceeds its own STAGE2_SECTORS budget"
if stage2_corrupt:
    # Past the first sector (magic word intact): only stage1's checksum
    # over the whole image can notice this.
    stage2[len(stage2) - 100] ^= 0x01

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
}

# fi_build FLAGS - builds stage1/stage2 with fault injection (disk.c's
# DISK_FI_*) in a temporary copy of bios/ and makes them the ones
# put_stages writes; fi_reset goes back to the normal build.
fi_build() {
    rm -rf "$W/fi"
    mkdir -p "$W/fi"
    cp -r "$BIOS_REPO/bios" "$W/fi/bios"
    rm -f "$W"/fi/bios/*.o "$W"/fi/bios/*.elf "$W"/fi/bios/*.bin
    make -C "$W/fi/bios" stage1.bin stage2.bin ZFSBOOT_VERSION=test EXTRA_CFLAGS="$1" >/dev/null 2>&1 ||
        { echo "fault-injection build failed: $1" >&2; exit 1; }
    S1_DIR="$W/fi/bios"
    S2_DIR="$W/fi/bios"
}
fi_reset() {
    S1_DIR="$BIOS_REPO/bios"
    S2_DIR="$BIOS_REPO/bios"
}
# put_stages - rewrites stage2 (and stage1's code bytes) in $W/disk.img
put_stages() {
python3 - "$S1_DIR" "$S2_DIR" "$W/disk.img" <<'PYEOF'
import sys
s1, s2, disk = sys.argv[1:]
with open(disk, "r+b") as f:
    f.write(open(f"{s1}/stage1.bin", "rb").read()[:440])
    f.seek(34 * 512)
    st2 = open(f"{s2}/stage2.bin", "rb").read()
    f.write(st2 + bytes(64 * 512 - len(st2)))
PYEOF
}
# lba_of FILE OFFSET - the disk LBA of byte OFFSET of EFI/ALPINE/FILE
lba_of() {
    python3 "$REPO_ROOT/tests/fat_lba.py" "$W/disk.img" "EFI/ALPINE/$1" "$2"
}
# The kernel byte the targeted faults hit: block 3 of its RAM range.
FI_KOFF=$((5 * 512 + 3 * 65536 + 5000))

MONITOR_PORT=45679
qemu_pid=
trap 'if [ -n "$qemu_pid" ]; then kill -9 "$qemu_pid" 2>/dev/null || true; fi; rm -rf "$W"' EXIT

# boot MEM_MB -> sets $status_line, $screen (last screen), $seen (every line
# any snapshot showed, in order - early lines scroll off an 80x25 screen)
boot() {
echo "== booting under QEMU (raw MBR disk, real INT13h path, no acceleration assumed, -m $1) =="
qemu-system-x86_64 \
    -drive file="$W/disk.img",format=raw ${BOOT_SECOND_DISK:+-drive file="$W/disk2.img",format=raw} \
    -m "$1" -display none -no-reboot -no-shutdown -serial none -machine pc \
    -monitor "tcp:127.0.0.1:$MONITOR_PORT,server,wait=off" \
    >"$W/qemu.log" 2>&1 &
qemu_pid=$!

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
seen = []
def done(status, text):
    print(status)
    print(text)
    print("--- every line seen ---")
    print("\n".join(seen))
    sys.exit(0)
while time.time() < deadline:
    text = snapshot()
    if text is None:
        time.sleep(1)
        continue
    last = text
    for l in text.split("\n"):
        if l.strip() and l not in seen[-60:]:
            seen.append(l)
    if "starting kernel" in text and "loading initrd" in text and "done" in text.split("loading initrd", 1)[1]:
        kernel_load_window = text.split("loading kernel", 1)[1].split("done", 1)[0] if "loading kernel" in text else ""
        dot_count = kernel_load_window.count(".")
        if dot_count < 1:
            done("PASS_BUT_NO_PROGRESS_DOTS", text)
        done("PASS", text)
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
        done("FATAL", text)
    time.sleep(2)

done("TIMEOUT", last or "(no VGA output ever captured)")
PYEOF
)"

kill -9 "$qemu_pid" 2>/dev/null || true
qemu_pid=

status_line="$(echo "$result" | head -1)"
screen="$(echo "$result" | tail -n +2 | sed '/^--- every line seen ---$/,$d')"
seen="$(echo "$result" | sed '1,/^--- every line seen ---$/d')"
# HDD_TEST_SHOW=1 prints what each boot showed, for a human reader.
if [ -n "${HDD_TEST_SHOW:-}" ]; then printf '%s\n' "$seen"; fi
}

# expect_boot DESCRIPTION - the boot must reach 'starting kernel' with progress dots.
expect_boot() {
case "$status_line" in
    PASS)
        ok "$1"
        return 0
        ;;
    PASS_BUT_NO_PROGRESS_DOTS)
        # ALLOW_NO_DOTS: a scenario whose own reports push the "loading
        # kernel" line off the screen before the snapshot.
        if [ -n "${ALLOW_NO_DOTS:-}" ]; then
            ok "$1"
            return 0
        fi
        echo "$screen" >&2
        bad "boot reached 'starting kernel' but printed zero progress-dot characters during the kernel load"
        ;;
    FATAL)
        echo "$seen" >&2
        bad "stage1/stage2 reported a FATAL error (or the stage1 magic-check '!' halt) - see captured screen above"
        ;;
    TIMEOUT)
        echo "$seen" >&2
        bad "boot never reached 'starting kernel' within the timeout"
        ;;
    *)
        echo "$result" >&2
        bad "unexpected test harness output"
        ;;
esac
return 1
}

# expect_halt DESCRIPTION - the boot must stop (FATAL or stage1's '!') and never start the kernel.
expect_halt() {
if [ "$status_line" = FATAL ] && ! printf '%s\n' "$seen" | grep -q 'starting kernel'; then
    ok "$1"
    return 0
fi
echo "$seen" >&2
bad "$1 (status $status_line)"
return 1
}

# saw TEXT DESCRIPTION - some captured line contains TEXT.
saw() {
if printf '%s\n' "$seen" | grep -qF -- "$1"; then
    ok "$2"
else
    echo "$seen" >&2
    bad "$2 (never saw '$1')"
fi
}

# old_stages -> sets OLD_DIR to a directory with an older release's
# stage1.bin/stage2.bin, or returns 1 when none is available.
OLD_DIR=
old_stages() {
    [ -n "$OLD_DIR" ] && return 0
    if [ -n "${OLD_STAGE_DIR:-}" ]; then
        OLD_DIR="$OLD_STAGE_DIR"
        return 0
    fi
    ref="${OLD_STAGE_REF:-v0.4.1}"
    git -C "$REPO_ROOT" rev-parse -q --verify "$ref^{commit}" >/dev/null 2>&1 || return 1
    mkdir -p "$W/old"
    git -C "$REPO_ROOT" archive "$ref" bios | tar -x -C "$W/old"
    make -C "$W/old/bios" stage1.bin stage2.bin ZFSBOOT_VERSION="old-$ref" >/dev/null 2>&1 || return 1
    OLD_DIR="$W/old/bios"
}

for scenario in $SCENARIOS; do
echo
echo "=== scenario: $scenario ==="
case "$scenario" in
    boot)
        build_disk "$W/KERNEL" "$W/CMDLINE" "$W/CHECKSUM" 0
        boot 256
        expect_boot "raw MBR HDD boot (stage1 -> stage2, real INT13h disk.c path, MBR fallback) loaded kernel+initrd through a non-NULL progress callback and reached 'starting kernel'" &&
            saw "verifying kernel+initrd" "the loaded kernel+initrd were checked against CHECKSUM" &&
            case "$seen" in *"verifying kernel+initrd"*" ok"*) ok "...and matched" ;; *) echo "$seen" >&2; bad "the integrity check did not print ok" ;; esac
        ;;
    nosum)
        build_disk "$W/KERNEL" "$W/CMDLINE" - 0
        boot 256
        expect_boot "without CHECKSUM the boot still reaches 'starting kernel' (backward compatible)" &&
            saw "no EFI/ALPINE/CHECKSUM" "...and says the payload was not verified"
        ;;
    stale-warn)
        build_disk "$W/KERNEL.BAD" "$W/CMDLINE" "$W/CHECKSUM" 0
        boot 256
        expect_boot "a stale CHECKSUM (mode warn, the default) does not stop the boot (boots as before)" &&
            saw "loaded KERNEL does not match CHECKSUM" "...but names the kernel" &&
            saw "  want " "...prints the expected digest" &&
            saw "  got  " "...and the digest of what is in RAM" &&
            saw "WARNING: differs from CHECKSUM - booting anyway" "...and says it boots anyway"
        ;;
    stale-enforce)
        build_disk "$W/KERNEL.BAD" "$W/CMDLINE" "$W/CHECKSUM.ENFORCE" 0
        boot 256
        expect_halt "CHECKSUM MODE enforce: a kernel with one flipped byte is refused before the jump" &&
            saw "integrity: enforce" "...announcing the mode" &&
            saw "integrity: KERNEL block 00a0 stayed wrong after 08 re-reads" "...naming the block (BLKSUM) after re-reading it" &&
            saw "integrity enforce: a block stayed wrong" "...with the refusal message" &&
            saw "  want " "...printing the expected digest" &&
            saw "  got  " "...and the digest of what is in RAM"
        ;;
    cmdline-enforce)
        build_disk "$W/KERNEL.BAD" "$W/CMDLINE.ENFORCE" "$W/CHECKSUM" 0
        boot 256
        expect_halt "alpine-zfsboot.integrity=enforce on the cmdline overrides CHECKSUM's mode warn: refused" &&
            saw "integrity enforce: a block stayed wrong" "...with the refusal message"
        ;;
    cmdline-off)
        build_disk "$W/KERNEL.BAD" "$W/CMDLINE.OFF" "$W/CHECKSUM.ENFORCE" 0
        boot 256
        expect_boot "alpine-zfsboot.integrity=off on the cmdline overrides MODE enforce: not hashed, boots" &&
            saw "integrity: off, not verified" "...and says so" &&
            if printf '%s\n' "$seen" | grep -q 'verifying kernel'; then bad "...but it hashed anyway"; else ok "...without hashing"; fi
        ;;
    stage2-corrupt)
        build_disk "$W/KERNEL" "$W/CMDLINE" "$W/CHECKSUM" 1
        boot 256
        if [ "$status_line" = FATAL ] && printf '%s\n' "$screen" | grep -qx ' *! *' && ! printf '%s\n' "$seen" | grep -q 'alpine-zfsboot-bios'; then
            ok "stage1 refuses a stage2 whose tail is corrupt (magic word intact): '!' and stage2 never runs"
        else
            echo "$seen" >&2
            bad "stage1 did not stop on a corrupt stage2 tail (status $status_line)"
        fi
        ;;
    small-ram)
        build_disk "$W/KERNEL" "$W/CMDLINE" "$W/CHECKSUM" 0
        boot 64
        expect_halt "with 64 MiB of RAM the initrd destination (64 MiB, above the end of RAM) is refused" &&
            saw "e820: initrd 04000000-" "...naming the initrd range" &&
            saw "above the end of RAM" "...with the E820 refusal" &&
            if printf '%s\n' "$seen" | grep -q 'loading kernel'; then bad "...but it loaded the kernel first"; else ok "...before loading anything"; fi
        ;;
    runtime-overlap)
        build_disk "$W/KERNEL.BIG" "$W/CMDLINE" "$W/CHECKSUM.BIG" 0
        boot 256
        expect_boot "a kernel whose init_size area reaches the initrd still boots (as before)" &&
            saw "overlaps initrd" "...after naming both ranges" &&
            saw "WARNING: booting anyway" "...and a warning"
        ;;
    diag)
        build_disk "$W/KERNEL" "$W/CMDLINE.DIAG" "$W/CHECKSUM" 0
        boot 256
        expect_boot "alpine-zfsboot.diag=1: the boot still reaches 'starting kernel'" &&
            saw "diag: e820" "...after printing the E820 map" &&
            saw "diag: kernel 00100000-" "...the kernel range" &&
            saw " run 01000000-03c30000" "...the kernel's runtime (init_size) range" &&
            saw "diag: code32 00100000 align 01000000 reloc 01 pref 01000000 init_size 02c30000" "...and its relocation fields"
        ;;
    old1-old2 | old1-new2 | new1-old2)
        if ! old_stages; then
            echo "  SKIP - no older release to test against (set OLD_STAGE_DIR, or fetch the ${OLD_STAGE_REF:-v0.4.1} tag)"
            continue
        fi
        case "$scenario" in old1-*) S1_DIR="$OLD_DIR" ;; esac
        case "$scenario" in *-old2) S2_DIR="$OLD_DIR" ;; esac
        if [ "$S2_DIR" = "$OLD_DIR" ]; then
            # A CHECKSUM MODE enforce describing a different kernel: an old
            # stage2 must not even look at it.
            build_disk "$W/KERNEL.BAD" "$W/CMDLINE" "$W/CHECKSUM.ENFORCE" 0
        else
            build_disk "$W/KERNEL" "$W/CMDLINE" "$W/CHECKSUM" 0
        fi
        S1_DIR="$BIOS_REPO/bios"
        S2_DIR="$BIOS_REPO/bios"
        boot 256
        expect_boot "$scenario: stage1 jumps into stage2 and it reaches 'starting kernel'"
        if [ "$scenario" != old1-new2 ]; then
            if printf '%s\n' "$seen" | grep -q 'CHECKSUM'; then bad "...an older stage2 mentioned CHECKSUM"; else ok "...the older stage2 ignored the CHECKSUM file"; fi
        fi
        ;;
    cap127 | cap64 | cap32 | cap16 | cap8 | cap1)
        n=${scenario#cap}
        build_disk "$W/KERNEL" "$W/CMDLINE.CAP$n" "$W/CHECKSUM" 0
        boot 256
        hex=$(printf '%04x' "$n")
        # The FAT layer asks for at most 32 sectors (FAT_IO_BATCH_SECTORS),
        # so above 32 the largest transfer is 32.
        big=$n; [ "$n" -gt 32 ] && big=32
        expect_boot "alpine-zfsboot.int13chunk=$n: boots to 'starting kernel'" &&
            saw "int13chunk: transfers of at most 0x$(printf '%08x' "$n") sectors" "...says which cap it uses" &&
            saw " cap $hex now $hex" "...the summary shows that cap in force" &&
            saw "largest $(printf '%04x' "$big")" "...and no transfer was larger than $n sectors" &&
            case "$seen" in *"verifying kernel+initrd"*" ok"*) ok "...and the payload in RAM matches CHECKSUM" ;; *) echo "$seen" >&2; bad "...CHECKSUM did not match after reads capped at $n" ;; esac
        ;;
    cap-invalid)
        build_disk "$W/KERNEL" "$W/CMDLINE.CAP0" "$W/CHECKSUM" 0
        boot 256
        expect_boot "alpine-zfsboot.int13chunk=0 (invalid) still boots" &&
            saw "alpine-zfsboot.int13chunk= ignored (not 1..127)" "...with a notice" &&
            saw " cap 0010 now 0010" "...using the build default (16)"
        ;;
    noblk)
        build_disk "$W/KERNEL" "$W/CMDLINE" "$W/CHECKSUM" 0 -
        boot 256
        expect_boot "CHECKSUM without BLKSUM (the previous release's install) boots as before" &&
            saw "no EFI/ALPINE/BLKSUM" "...with a notice" &&
            case "$seen" in *"verifying kernel+initrd"*" ok"*) ok "...and the whole-file check still runs" ;; *) echo "$seen" >&2; bad "the whole-file check did not run" ;; esac
        ;;
    staleblk)
        build_disk "$W/KERNEL" "$W/CMDLINE" "$W/CHECKSUM" 0 "$W/BLKSUM.OTHER"
        boot 256
        expect_boot "a BLKSUM from another payload is ignored and the boot goes on" &&
            saw "BLKSUM does not match CHECKSUM" "...with a notice"
        ;;
    fi-transient-flip | fi-transient-zero | fi-transient-stale | fi-transient-wronglba | fi-transient-short)
        k=${scenario#fi-transient-}
        case "$k" in flip) m=1 ;; zero) m=2 ;; stale) m=3 ;; wronglba) m=4 ;; short) m=5 ;; esac
        build_disk "$W/KERNEL" "$W/CMDLINE" "$W/CHECKSUM" 0
        fi_build "-DDISK_FI_MODE=$m -DDISK_FI_LBA=$(lba_of KERNEL $FI_KOFF) -DDISK_FI_TIMES=1"
        put_stages
        fi_reset
        boot 256
        expect_boot "CF=0 with wrong data once ($k) in kernel block 3: boots" &&
            saw "summary: blocks mism 0001 reread 0001 healed 0001 2nd 0000 bad 0000" "...the block was re-read once and healed (summary printed without diag)" &&
            case "$seen" in *"verifying kernel+initrd"*" ok"*) ok "...and the kernel in RAM matches the file (end-to-end SHA-256)" ;; *) echo "$seen" >&2; bad "end-to-end check failed after healing" ;; esac
        ;;
    fi-above-flip | fi-above-zero | fi-above-stale | fi-above-wronglba | fi-above-short)
        k=${scenario#fi-above-}
        case "$k" in flip) m=1 ;; zero) m=2 ;; stale) m=3 ;; wronglba) m=4 ;; short) m=5 ;; esac
        build_disk "$W/KERNEL" "$W/CMDLINE" "$W/CHECKSUM" 0
        fi_build "-DDISK_FI_MODE=$m -DDISK_FI_ABOVE=8"
        put_stages
        fi_reset
        boot 256
        expect_boot "CF=0 with wrong data ($k) on every transfer above 8 sectors: boots" &&
            saw " cap 0010 now 0008" "...the transfer size went 16 -> 8 and stayed there" &&
            saw "healed 0001 2nd 0000 bad 0000" "...one block healed, then no more trouble at the smaller size" &&
            case "$seen" in *"verifying kernel+initrd"*" ok"*) ok "...and kernel+initrd in RAM match the files" ;; *) echo "$seen" >&2; bad "end-to-end check failed" ;; esac
        ;;
    fi-persistent)
        build_disk "$W/KERNEL" "$W/CMDLINE" "$W/CHECKSUM" 0
        L=$(lba_of KERNEL $FI_KOFF)
        fi_build "-DDISK_FI_MODE=1 -DDISK_FI_LBA=$L"
        put_stages
        fi_reset
        boot 256
        expect_boot "the same wrong data on every read (mode warn, the default): reported, then boots" &&
            saw "integrity: KERNEL block 0003 stayed wrong after 08 re-reads" "...naming the block, after 8 re-reads" &&
            saw "file offset $(printf '%08x' $((5 * 512 + 3 * 65536))) lba $(printf '%010x' $((L - 5000 / 512)))" "...its file offset and the LBA where it starts" &&
            saw "bad 0001" "...counted in the summary" &&
            saw "summary: bad KERNEL @$(printf '%08x' $((5 * 512 + 3 * 65536))) lba $(printf '%08x' $((L - 5000 / 512))) exp " "...whose last line names the block, its LBA and both states" &&
            saw "WARNING: differs from CHECKSUM - booting anyway" "...and the end-to-end check warns too"
        ;;
    fi-persistent-enforce)
        build_disk "$W/KERNEL" "$W/CMDLINE" "$W/CHECKSUM.ENFORCE" 0
        fi_build "-DDISK_FI_MODE=1 -DDISK_FI_LBA=$(lba_of KERNEL $FI_KOFF)"
        put_stages
        fi_reset
        boot 256
        expect_halt "the same wrong data on every read, integrity enforce: refused" &&
            saw "stayed wrong after 08 re-reads" "...after reporting the block" &&
            saw "integrity enforce: a block stayed wrong" "...with the refusal"
        ;;
    fi-second)
        build_disk "$W/KERNEL" "$W/CMDLINE.DIAG" "$W/CHECKSUM" 0
        cp "$W/disk.img" "$W/disk2.img"
        fi_build "-DDISK_FI_MODE=2 -DDISK_FI_LBA=$(lba_of KERNEL $FI_KOFF) -DDISK_FI_DRIVE=0x80"
        put_stages
        fi_reset
        BOOT_SECOND_DISK=1 boot 256
        expect_boot "persistent wrong data on drive 80, a mirror on drive 81: boots" &&
            saw "diag: second source drive 81 KERNEL block 0003" "...block 3 read from drive 81" &&
            saw "healed 0000 2nd 0001 bad 0000" "...and counted as served by the second source" &&
            case "$seen" in *"verifying kernel+initrd"*" ok"*) ok "...and the kernel in RAM matches the file" ;; *) echo "$seen" >&2; bad "end-to-end check failed" ;; esac
        rm -f "$W/disk2.img"
        ;;
    fi-noext)
        build_disk "$W/KERNEL" "$W/CMDLINE.DIAG" "$W/CHECKSUM" 0
        fi_build "-DDISK_FI_NOEXT"
        put_stages
        fi_reset
        boot 256
        expect_boot "a BIOS without INT 13h extensions: stage2 reads with AH=02h (CHS) and boots" &&
            saw "summary: drive 80 chs bps 0200" "...using CHS" &&
            saw "summary: edd 00" "...as the BIOS reports no extensions" &&
            case "$seen" in *"verifying kernel+initrd"*" ok"*) ok "...and every byte arrived right" ;; *) echo "$seen" >&2; bad "end-to-end check failed over CHS" ;; esac
        ;;
    fi-4k)
        # AH=48h claims 4096-byte sectors, but stage1 just read stage2 in
        # 512-byte LBAs from this BIOS: a warning, and the boot goes on as before.
        build_disk "$W/KERNEL" "$W/CMDLINE" "$W/CHECKSUM" 0
        fi_build "-DDISK_FI_SECTOR_SIZE=4096"
        put_stages
        fi_reset
        boot 256
        expect_boot "a BIOS whose AH=48h reports 4096-byte sectors: warned, boots as before" &&
            saw "AH=48h sector size 1000h, reading 512 anyway" "...with the warning" &&
            saw "summary: drive 80 lba bps 1000" "...and the reported size in the summary"
        ;;
    fi-allbad)
        # Every kernel block from block 1 on comes back wrong, at every size:
        # 3 blocks reported after re-reads, the rest only counted - the
        # boot must not crawl through 8 re-reads per block.
        build_disk "$W/KERNEL" "$W/CMDLINE" "$W/CHECKSUM" 0
        ks=$(wc -c < "$W/KERNEL")
        fi_build "-DDISK_FI_MODE=1 -DDISK_FI_FROM=$(lba_of KERNEL $((5 * 512 + 65536))) -DDISK_FI_TO=$(lba_of KERNEL $((ks - 1)))"
        put_stages
        fi_reset
        t0=$(date +%s)
        boot 256
        t1=$(date +%s)
        nb=$(( (ks - 5 * 512 + 65535) / 65536 ))
        ALLOW_NO_DOTS=1 expect_boot "every kernel block wrong at every size (mode warn): boots in $((t1 - t0)) s" &&
            saw "integrity: $(printf '%04x' $((nb - 1 - 3))) more KERNEL blocks wrong, not re-read" "...3 blocks reported, the other $((nb - 4)) only counted" &&
            saw "reread 0018 healed 0000 2nd 0000 bad $(printf '%04x' $((nb - 1)))" "...24 re-reads in all, $((nb - 1)) bad blocks in the summary" &&
            saw "WARNING: differs from CHECKSUM - booting anyway" "...and the end-to-end check warns"
        ;;
    int13-limit)
        # With alpine-zfsboot.diag=1, so every failed BIOS call is printed
        # too, and int13chunk=64, so the reads start large enough for the
        # injected 16-sector limit to bite (the default cap is 16 itself).
        build_disk "$W/KERNEL" "$W/CMDLINE.INT13" "$W/CHECKSUM" 0
        boot 256
        expect_boot "a BIOS that rejects transfers above 16 sectors still boots: kernel+initrd loaded and 'starting kernel' reached" &&
            saw "BIOS refused a read" "the loader said which read the BIOS refused and what it does now" &&
            saw "diag: int13 fail lba " "diagnostics print each failed BIOS call" &&
            saw " n 0020 try 01 status 0009" "...with its sector count, attempt number and BIOS status" &&
            case "$seen" in *"verifying kernel+initrd"*" ok"*) ok "the split reads delivered byte-correct kernel+initrd (CHECKSUM matched in RAM)" ;; *) echo "$seen" >&2; bad "the integrity check did not print ok after the split reads" ;; esac
        ;;
    *)
        bad "unknown scenario $scenario"
        ;;
esac
done

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
