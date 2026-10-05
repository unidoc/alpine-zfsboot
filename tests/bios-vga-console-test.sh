#!/bin/sh
# bios-vga-console-test.sh - boots the REAL built ISO (BIOS El Torito path,
# no serial port at all, -vga std) and requires the kernel+initramfs to
# show "press TAB to interrupt" on the VGA text screen (read straight out
# of guest memory at 0xb8000 through the QEMU monitor).
#
# Regression guard for a real bug: the BIOS loader enters the kernel's
# 32-bit entry point, so the kernel's own real-mode setup never ran and
# boot_params.screen_info stayed zero -> no vgacon -> tty0 invisible on
# VGA ("starting kernel" and then nothing). Every other BIOS test here
# uses serial, so none could see it. UEFI is unaffected (GOP fills
# screen_info). Boots stage2 via the ISO because stage2's bootparams
# code is shared by the ISO and raw-disk builds.
#
# Needs: qemu-system-x86_64, python3, and a built ISO (just build x86_64).
# ISO=path overrides the default out/alpine-zfsboot-x86_64.iso.
set -eu

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ISO="${ISO:-$REPO_ROOT/out/alpine-zfsboot-x86_64.iso}"
PORT="${MONITOR_PORT:-45691}"
NEEDLE="press TAB to interrupt"

for cmd in qemu-system-x86_64 python3; do
    command -v "$cmd" >/dev/null 2>&1 || { echo "missing required tool: $cmd" >&2; exit 1; }
done
[ -f "$ISO" ] || { echo "no ISO at $ISO (run: just build x86_64)" >&2; exit 1; }

W="$(mktemp -d)"
qemu_pid=""
trap '[ -n "$qemu_pid" ] && kill -9 "$qemu_pid" 2>/dev/null; rm -rf "$W"' EXIT

qemu-system-x86_64 -cdrom "$ISO" -boot d -m 1024 -machine pc \
    -display none -vga std -serial none -no-reboot \
    -monitor "tcp:127.0.0.1:$PORT,server,wait=off" >"$W/qemu.log" 2>&1 &
qemu_pid=$!

result="$(python3 - "$PORT" "$W/vga.bin" "$NEEDLE" <<'PYEOF'
import socket, sys, time
port, path, needle = int(sys.argv[1]), sys.argv[2], sys.argv[3]

def read_until_prompt(s, timeout=10.0):
    # Same pattern as tests/bios-iso-entry-test.sh: wait for the monitor's
    # "(qemu)" prompt instead of a fixed sleep (a fixed sleep read stale data
    # under a host stall).
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

def snap():
    s = socket.create_connection(("127.0.0.1", port), timeout=5)
    read_until_prompt(s)
    # vgacon hardware-scrolls through its whole 32 KiB window once the screen
    # fills, so the prompt is not necessarily in the first 25 rows: save the
    # whole window and search every row.
    s.sendall(f'pmemsave 0xb8000 0x8000 "{path}"\n'.encode())
    read_until_prompt(s); s.close()
    d = open(path, "rb").read()
    return "\n".join("".join(chr(d[(r*80+c)*2]) if 32 <= d[(r*80+c)*2] < 127 else " "
                             for c in range(80)).rstrip() for r in range(len(d) // 160))

time.sleep(2)
deadline, last = time.time() + 150, "(no screen captured)"
while time.time() < deadline:
    try:
        last = snap()
    except OSError:
        time.sleep(1); continue
    if needle in last:
        print("PASS"); print(last); sys.exit(0)
    time.sleep(3)
print("FAIL"); print(last)
PYEOF
)"

pass=0; fail=0
if [ "$(echo "$result" | head -1)" = PASS ]; then
    pass=1; echo "  ok - BIOS boot with VGA only (no serial) shows '$NEEDLE' on the VGA text screen"
else
    fail=1; echo "$result" | tail -n +2 >&2
    echo "  FAIL - '$NEEDLE' never appeared on VGA (screen above)"
fi
echo; echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
