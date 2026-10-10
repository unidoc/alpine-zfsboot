#!/bin/sh
# bios-mirror-boot-test.sh - a mirrored boot (issue #24) for real: TWO
# virtio disks, each with stage1/stage2 and its own ESP, booted in QEMU
# through the REAL chain - SeaBIOS -> stage1 -> stage2 -> the real Alpine
# kernel and initramfs from a build (out/) -> /init - with BIOS boot
# order set to the SECOND disk only. It checks, from /init's own serial
# log, which ESP /init took its config, authorized_keys and host key from.
#
# The initramfs is the built one with this tree's init scripts appended
# as an extra, uncompressed cpio segment (the kernel unpacks segments in
# order, later files replace earlier ones - the same mechanism
# kernel-integration-test.sh proves), so init/init, init/esp-select.sh,
# init/net-config.sh, init/rescue-ssh.sh and init/menu.py under test are
# this tree's, without rebuilding the image (no Docker needed).
#
# Scenarios (MIRROR_TEST_SCENARIOS, all by default):
#   absent     only disk 2 attached (disk 1 is dead/removed): boots, init
#              uses disk 2's ESP (the booted one) and warns that ESP 1 of
#              the set is missing
#   corrupt    disk 1 attached but wrecked (stage1 and FAT boot sector
#              zeroed); boot order disk 2 only: boots, init uses disk 2
#   listed     disks 1+2 healthy, plus a third disk with a FOREIGN ESP
#              (another installation's marker, a malicious authorized_keys);
#              alpine-zfsboot.esp-uuids=<1>,<2> on the cmdline: init uses the
#              FIRST LISTED ESP (disk 1), names the foreign one as not listed
#   foreign    no list, no esp-self (a pre-0.5.0 or UEFI-style cmdline):
#              ESPs of two installations present -> init refuses, applies
#              no ESP config, and the pool boot goes on ("importing pool")
#   stale      no list, no esp-self, disk 1 generation 4, disk 2 generation
#              5: init takes disk 2 (newest) and names disk 1 as STALE
#   booted     no list, a foreign ESP with a higher generation present:
#              init uses the ESP stage2 booted from (alpine-zfsboot.esp-self=)
#   netmac     (issue #22) two NICs, alpine-zfsboot.net.mac= names the
#              second: the pool import fails, rescue SSH comes up on eth1
#   netmac-missing  net.mac names no card: configuration error, no eth0
#              fallback, rescue SSH not started
#   legacy     two unmarked ESPs (installs before 0.5.0), no list: refused
#              exactly as before, the message now names both UUIDs and
#              the ways out (negative control for the marker logic)
#
# Needs: qemu-system-x86_64 (>= 8.1, or the TCG single-thread note in the
# README applies - this script passes -accel tcg,thread=single anyway),
# mkfs.vfat + mtools, cpio (GNU, -R), python3, and a build:
# ARTIFACT_DIR (default ./out) with alpine-zfsboot-x86_64-vmlinuz,
# -initramfs.img, -bios-stage1.bin, -bios-stage2.bin, -cmdline.txt.
set -eu

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
A="${ARTIFACT_DIR:-$REPO_ROOT/out}"
SCENARIOS="${MIRROR_TEST_SCENARIOS:-absent corrupt listed booted foreign stale legacy netmac netmac-missing}"
BOOT_TIMEOUT="${MIRROR_TEST_TIMEOUT:-240}"

pass=0
fail=0
ok()  { pass=$((pass + 1)); echo "  ok - $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL - $1"; }

for cmd in qemu-system-x86_64 mkfs.vfat mmd mcopy cpio gzip python3; do
    command -v "$cmd" >/dev/null 2>&1 || { echo "missing required tool: $cmd" >&2; exit 1; }
done
for f in vmlinuz initramfs.img bios-stage1.bin bios-stage2.bin cmdline.txt; do
    [ -r "$A/alpine-zfsboot-x86_64-$f" ] || { echo "missing build artifact $A/alpine-zfsboot-x86_64-$f (just build x86_64, or ARTIFACT_DIR=...)" >&2; exit 1; }
done

W="$(mktemp -d)"
qemu_pid=""
trap 'if [ -n "$qemu_pid" ]; then kill -9 "$qemu_pid" 2>/dev/null || true; fi; rm -rf "$W"' EXIT

ID_A="0f6c1e9a-4b1d-4c7e-9a55-1d2e3f405162"
ID_B="9a8b7c6d-1111-4222-8333-444455556666"
U1="6AC5-0B94"; U2="6AC5-2367"; U3="C0FE-0003"

echo "== initramfs: the build's, with this tree's init scripts appended =="
mkdir -p "$W/overlay"
for f in init esp-select.sh net-config.sh rescue-ssh.sh menu.py; do
    cp "$REPO_ROOT/init/$f" "$W/overlay/$f"
    chmod 755 "$W/overlay/$f"
done
cp "$A/alpine-zfsboot-x86_64-initramfs.img" "$W/INITRD"
pad=$(( (4 - $(wc -c < "$W/INITRD") % 4) % 4 ))
[ "$pad" -gt 0 ] && dd if=/dev/zero bs=1 count="$pad" >> "$W/INITRD" 2>/dev/null
( cd "$W/overlay" && find . | cpio -o -H newc -R 0:0 2>/dev/null ) >> "$W/INITRD"
cp "$A/alpine-zfsboot-x86_64-vmlinuz" "$W/KERNEL"
# The build's cmdline, with `quiet` dropped so init's messages reach the
# serial console in full; console=ttyS0 is already in every build.
BASE_CMDLINE="$(head -n 1 "$A/alpine-zfsboot-x86_64-cmdline.txt" | sed 's/ quiet / /')"
case "$BASE_CMDLINE" in *console=ttyS0*) ;; *) BASE_CMDLINE="$BASE_CMDLINE console=ttyS0,115200" ;; esac

# A real dropbear ed25519 host key (the format internal/espconfig writes:
# string "ssh-ed25519", then seed||public key), the public key derived
# with RFC 8032's reference arithmetic - dropbearkey -y in the initramfs
# validates it before rescue SSH starts, so a fake one would stop the
# net.mac scenarios before the network is even touched.
python3 - "$W/hostkey.real" <<'PYEOF2'
import hashlib, os, struct, sys
p = 2**255 - 19
d = -121665 * pow(121666, p - 2, p) % p
def inv(x): return pow(x, p - 2, p)
def add(P, Q):
    x1, y1, z1, t1 = P; x2, y2, z2, t2 = Q
    a = (y1 - x1) * (y2 - x2) % p; b = (y1 + x1) * (y2 + x2) % p
    c = 2 * t1 * t2 * d % p; dd = 2 * z1 * z2 % p
    e, f, g, h = b - a, dd - c, dd + c, b + a
    return (e * f % p, g * h % p, f * g % p, e * h % p)
def mul(s, P):
    Q = (0, 1, 1, 0)
    while s:
        if s & 1: Q = add(Q, P)
        P = add(P, P); s >>= 1
    return Q
gy = 4 * inv(5) % p
gx2 = (gy * gy - 1) * inv(d * gy * gy + 1) % p
gx = pow(gx2, (p + 3) // 8, p)
if (gx * gx - gx2) % p: gx = gx * pow(2, (p - 1) // 4, p) % p
if gx & 1: gx = p - gx
G = (gx, gy, 1, gx * gy % p)
seed = os.urandom(32)
h = hashlib.sha512(seed).digest()
a = int.from_bytes(h[:32], "little")
a &= (1 << 254) - 8; a |= 1 << 254
X, Y, Z, _ = mul(a, G)
x, y = X * inv(Z) % p, Y * inv(Z) % p
pub = (y | ((x & 1) << 255)).to_bytes(32, "little")
name = b"ssh-ed25519"
open(sys.argv[1], "wb").write(struct.pack(">I", len(name)) + name + struct.pack(">I", 64) + seed + pub)
PYEOF2

# build_disk NAME UUID CMDLINE_EXTRA MEMBER(id:gen:members|-) KEYNAME [BOOTABLE=1] [CONFIG_EXTRA]
# BOOTABLE=0: no stage1 boot code and no stage2 (the partition stays).
build_disk() {
    name="$1" uuid="$2" extra="$3" member="$4" key="$5" bootable="${6:-1}" cfgextra="${7:-}"
    img_mb=$(( ($(wc -c < "$W/KERNEL") + $(wc -c < "$W/INITRD")) / 1024 / 1024 + 12 ))
    rm -f "$W/fat.img"
    dd if=/dev/zero of="$W/fat.img" bs=1M count="$img_mb" status=none
    mkfs.vfat -F32 -n EFI -i "$(echo "$uuid" | tr -d '-')" "$W/fat.img" >/dev/null
    mmd -i "$W/fat.img" ::EFI ::EFI/ALPINE
    mcopy -i "$W/fat.img" "$W/KERNEL" ::EFI/ALPINE/KERNEL
    mcopy -i "$W/fat.img" "$W/INITRD" ::EFI/ALPINE/INITRD
    printf '%s%s\n' "$BASE_CMDLINE" "$extra" > "$W/CMDLINE"
    [ "$(wc -c < "$W/CMDLINE")" -le 511 ] || { echo "CMDLINE too long for stage2" >&2; exit 1; }
    mcopy -i "$W/fat.img" "$W/CMDLINE" ::EFI/ALPINE/CMDLINE
    printf 'alpine-zfsboot.ssh.port=22\n%s' "$cfgextra" > "$W/config"
    mcopy -i "$W/fat.img" "$W/config" ::EFI/ALPINE/config
    printf 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI%s %s\n' "$key" "$key" > "$W/authorized_keys"
    mcopy -i "$W/fat.img" "$W/authorized_keys" ::EFI/ALPINE/authorized_keys
    mcopy -i "$W/fat.img" "$W/hostkey.real" ::EFI/ALPINE/ssh_host_ed25519_key
    if [ "$member" != "-" ]; then
        id="${member%%:*}"; rest="${member#*:}"; gen="${rest%%:*}"; members="${rest#*:}"
        printf 'alpine-zfsboot esp-member 1\nINSTALL_ID=%s\nGENERATION=%s\nESP_UUID=%s\nMEMBERS=%s\n' "$id" "$gen" "$uuid" "$members" > "$W/MEMBER"
        mcopy -i "$W/fat.img" "$W/MEMBER" ::EFI/ALPINE/MEMBER
    fi
    python3 - "$A" "$W" "$name" "$bootable" <<'PYEOF'
import os, struct, sys
a, w, name, bootable = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4] == "1"
SECTOR, STAGE2_LBA, FAT_LBA = 512, 34, 2048
stage1 = bytearray(open(f"{a}/alpine-zfsboot-x86_64-bios-stage1.bin", "rb").read())
assert len(stage1) == 512 and stage1[446:462] == bytes(16)
fat = open(f"{w}/fat.img", "rb").read()
stage1[446:462] = struct.pack("<B3sB3sII", 0, b"\0\0\0", 0x2E, b"\0\0\0", FAT_LBA, len(fat) // SECTOR)
stage2 = open(f"{a}/alpine-zfsboot-x86_64-bios-stage2.bin", "rb").read()
with open(f"{w}/{name}.img", "wb") as f:
    if not bootable:
        # no boot code (BIOS cannot boot this disk), but the partition table
        # and its signature stay - Linux still sees the partition.
        stage1[0:446] = bytes(446)
    f.write(stage1)
    f.write(bytes(STAGE2_LBA * SECTOR - SECTOR))
    f.write(stage2 if bootable else bytes(len(stage2)))
    f.write(bytes(FAT_LBA * SECTOR - STAGE2_LBA * SECTOR - len(stage2)))
    f.write(fat)
PYEOF
    rm -f "$W/fat.img"
}

# wreck NAME - zero the FAT boot sector (blkid sees no filesystem any more)
wreck() {
    dd if=/dev/zero of="$W/$1.img" bs=512 seek=2048 count=1 conv=notrunc status=none
}

# boot SCENARIO DISK... - disks in attach order; ONLY the second one (or
# the only one) gets a bootindex, with strict boot order.
boot() {
    scen="$1"; shift
    args=""; n=0
    for img in "$@"; do
        n=$((n + 1))
        bi=""
        if [ "$#" -eq 1 ] || [ "$n" -eq 2 ]; then bi=",bootindex=1"; fi
        args="$args -drive file=$W/$img.img,format=raw,if=none,id=d$n -device virtio-blk-pci,drive=d$n$bi"
    done
    log="$W/serial-$scen.log"
    : > "$log"
    # shellcheck disable=SC2086
    qemu-system-x86_64 -machine pc -accel tcg,thread=single -m 1536 \
        -display none -no-reboot -monitor none -serial "file:$log" \
        -boot strict=on ${NICS:--nic none} $args >"$W/qemu-$scen.log" 2>&1 &
    qemu_pid=$!
    waited=0
    while [ "$waited" -lt "$BOOT_TIMEOUT" ]; do
        if grep -qE "${WAIT_FOR:-importing pool}" "$log" 2>/dev/null; then break; fi
        if ! kill -0 "$qemu_pid" 2>/dev/null; then break; fi
        sleep 2
        waited=$((waited + 2))
    done
    kill -9 "$qemu_pid" 2>/dev/null || true
    wait "$qemu_pid" 2>/dev/null || true
    qemu_pid=""
    tr -d '\r' < "$log" > "$log.txt"
    echo "   (scenario $scen: ${waited}s to '${WAIT_FOR:-importing pool}')"
}

# expect SCENARIO TEXT DESCRIPTION / expect_not ...
expect() {
    if grep -qF -- "$2" "$W/serial-$1.log.txt"; then ok "$1: $3"; else
        echo "---- serial log of $1 (tail) ----"; grep -i "esp\|alpine-zfsboot\|stage2\|error\|panic" "$W/serial-$1.log.txt" | tail -40
        bad "$1: $3 (missing: $2)"; fi
}
expect_not() {
    if ! grep -qF -- "$2" "$W/serial-$1.log.txt"; then ok "$1: $3"; else bad "$1: $3 (found: $2)"; fi
}

for scen in $SCENARIOS; do
    echo "== scenario: $scen =="
    case "$scen" in
        absent)
            build_disk d2 "$U2" " alpine-zfsboot.esp-self=$U2" "$ID_A:5:$U1,$U2" KEYD2
            boot absent d2
            expect absent "alpine-zfsboot ESP found on /dev/vda1 (UUID $U2: the ESP this boot came from)" "booted from disk 2 alone, its ESP supplied config/keys"
            expect absent "ESP $U1 of this host's set is not present" "the missing member is named"
            expect absent "authorized_keys: 1 usable key(s) found" "rescue SSH key staged"
            ;;
        corrupt)
            build_disk d1 "$U1" " alpine-zfsboot.esp-self=$U1" "$ID_A:5:$U1,$U2" KEYD1 0
            wreck d1
            build_disk d2 "$U2" " alpine-zfsboot.esp-self=$U2" "$ID_A:5:$U1,$U2" KEYD2
            boot corrupt d1 d2
            expect corrupt "alpine-zfsboot ESP found on /dev/vdb1 (UUID $U2: the ESP this boot came from)" "disk 1 wrecked: booted from disk 2, its ESP used"
            expect corrupt "ESP $U1 of this host's set is not present" "the wrecked member is named as missing"
            ;;
        listed)
            build_disk d1 "$U1" " alpine-zfsboot.esp-uuids=$U1,$U2 alpine-zfsboot.esp-self=$U1" "$ID_A:5:$U1,$U2" KEYD1
            build_disk d2 "$U2" " alpine-zfsboot.esp-uuids=$U1,$U2 alpine-zfsboot.esp-self=$U2" "$ID_A:5:$U1,$U2" KEYD2
            build_disk d3 "$U3" "" "$ID_B:99:$U3" EVILKEY 0
            boot listed d1 d2 d3
            expect listed "alpine-zfsboot ESP found on /dev/vda1 (UUID $U1: listed in alpine-zfsboot.esp-uuids from the kernel command line)" "the first listed ESP supplied config/keys"
            expect listed "ignoring ESP $U3 (/dev/vdc1): not in alpine-zfsboot.esp-uuids" "the foreign ESP is named and not used"
            expect_not listed "UUID $U3:" "the foreign ESP never supplied config"
            ;;
        foreign)
            build_disk d1 "$U1" "" "$ID_A:5:$U1,$U2" KEYD1
            build_disk d2 "$U2" "" "$ID_A:5:$U1,$U2" KEYD2
            build_disk d3 "$U3" "" "$ID_B:99:$U3" EVILKEY 0
            boot foreign d1 d2 d3
            expect foreign "ESPs of more than one alpine-zfsboot installation are present" "two installations, nothing to choose: refused"
            expect_not foreign "alpine-zfsboot ESP found on" "no ESP config applied"
            expect foreign "importing pool" "the pool boot goes on regardless"
            ;;
        stale)
            build_disk d1 "$U1" "" "$ID_A:4:$U1,$U2" KEYD1
            build_disk d2 "$U2" "" "$ID_A:5:$U1,$U2" KEYD2
            boot stale d1 d2
            expect stale "alpine-zfsboot ESP found on /dev/vdb1 (UUID $U2: member of installation $ID_A, generation 5)" "the newest member supplied config/keys"
            expect stale "ESP $U1 (/dev/vda1) is STALE (generation 4 < 5)" "the stale member is named"
            ;;
        booted)
            build_disk d1 "$U1" " alpine-zfsboot.esp-self=$U1" "$ID_A:5:$U1,$U2" KEYD1
            build_disk d2 "$U2" " alpine-zfsboot.esp-self=$U2" "$ID_A:5:$U1,$U2" KEYD2
            build_disk d3 "$U3" " alpine-zfsboot.esp-self=$U3" "$ID_B:99:$U3" EVILKEY 0
            boot booted d1 d2 d3
            expect booted "alpine-zfsboot ESP found on /dev/vdb1 (UUID $U2: the ESP this boot came from)" "no list: the ESP stage2 booted from (disk 2) supplied config/keys"
            expect_not booted "UUID $U3:" "the foreign ESP (higher generation, malicious key) never supplied config"
            ;;
        netmac)
            # Two NICs; the rescue network must come up on the SECOND
            # (eth1), named by MAC. The pool import fails (no pool), which
            # starts rescue SSH proactively - the real trigger path.
            build_disk d2 "$U2" " alpine-zfsboot.esp-self=$U2" "$ID_A:5:$U2" KEYD2 1 \
                "alpine-zfsboot.net.mac=52:54:00:AB:CD:02
alpine-zfsboot.ipv4=static
alpine-zfsboot.ipv4.address=10.0.2.15/24
alpine-zfsboot.ipv6=off
"
            NICS="-netdev user,id=n0 -device virtio-net-pci,netdev=n0,mac=52:54:00:ab:cd:01 -netdev user,id=n1 -device virtio-net-pci,netdev=n1,mac=52:54:00:ab:cd:02" \
            WAIT_FOR="dropbear sshd started|not starting dropbear|rescue SSH NOT reachable" boot netmac d2
            expect netmac "rescue network: eth1 is the card with MAC 52:54:00:ab:cd:02" "alpine-zfsboot.net.mac resolved to the second NIC"
            expect netmac "IPv4: static 10.0.2.15/24 on eth1" "the rescue network was configured on eth1"
            expect_not netmac "IPv4: static 10.0.2.15/24 on eth0" "eth0 was not used"
            expect netmac "dropbear sshd started" "rescue SSH started"
            ;;
        netmac-missing)
            build_disk d2 "$U2" " alpine-zfsboot.esp-self=$U2" "$ID_A:5:$U2" KEYD2 1 \
                "alpine-zfsboot.net.mac=52:54:00:99:99:99
alpine-zfsboot.ipv4=static
alpine-zfsboot.ipv4.address=10.0.2.15/24
alpine-zfsboot.ipv6=off
"
            NICS="-netdev user,id=n0 -device virtio-net-pci,netdev=n0,mac=52:54:00:ab:cd:01" \
            WAIT_FOR="dropbear sshd started|not starting dropbear|rescue SSH NOT reachable" boot netmac-missing d2
            expect netmac-missing "NOT falling back to eth0" "a MAC no card has is a configuration error"
            expect netmac-missing "eth0=52:54:00:ab:cd:01" "the message names the cards that are there"
            expect netmac-missing "not starting dropbear" "rescue SSH is not started"
            expect_not netmac-missing "on eth0" "eth0 was never configured"
            ;;
        legacy)
            build_disk d1 "$U1" "" - KEYD1
            build_disk d2 "$U2" "" - KEYD2
            boot legacy d1 d2
            expect legacy "ambiguous, applying NONE of it" "two unmarked ESPs: refused as before"
            expect legacy "alpine-zfsboot esp adopt $U1 $U2 --yes" "the refusal names the UUIDs and the way out"
            expect legacy "importing pool" "the pool boot goes on regardless"
            ;;
        *) echo "unknown scenario $scen" >&2; exit 2 ;;
    esac
    rm -f "$W"/d1.img "$W"/d2.img "$W"/d3.img
done

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
