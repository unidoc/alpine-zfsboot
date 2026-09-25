#!/usr/bin/env bash
# run-tests.sh - functional tests for init/init, boot-dataset.sh and
# alpine-zfsboot-shell, run directly on the dev host (no Alpine container, no
# real hardware needed).
#
# Uses #!/usr/bin/env bash - init/init used to have a real reason this
# harness specifically needed bash (a `read -t N -n 1` keypress race,
# unsupported by dash's `read`), which is gone now that select_console()
# no longer races a keypress at all (menu.py's own dialog --timeout is
# the countdown now - see /init's own header comment for why). Left as
# bash anyway rather than re-verified against dash - no longer a hard
# requirement as far as this file's own contents go, but not worth
# re-litigating without a real reason to.
#
# init/init and boot-dataset.sh both set PATH=/sbin:/bin:/usr/sbin:/usr/bin
# themselves - deliberately, for a deterministic PATH at real boot,
# not inherited from whatever the caller's environment happened to be.
# That also means a test harness can't inject stub commands by
# prepending PATH from the outside - the script's own assignment wins.
# Instead, run_stubbed() below dot-sources the script under test into
# a subshell where mount/zpool/zfs/kexec/etc are shell FUNCTIONS
# pointing at tests/stubs/<name> - function resolution takes
# precedence over PATH lookup no matter what PATH is set to, so this
# works regardless. alpine-zfsboot-shell doesn't set its own PATH (it inherits
# whatever /init exported), so its tests use plain PATH-prepend instead.
#
# STUB_ROOT (-> $ROOTFS inside init/init and boot-dataset.sh) redirects
# every absolute path those scripts would otherwise touch (/etc/passwd,
# /root/.ssh, /mnt/root) into a scratch directory - see each script's
# own ROOTFS comment. Without this, running them directly would write
# into THIS dev machine's real /etc/passwd. /proc/cmdline is likewise
# read from "$ROOTFS/proc/cmdline", not the real (host) one.
#
# menu.py is exercised too, but only its non-dialog helpers (imported
# as a module, or pure functions like parse_source) - `dialog` itself
# needs a real terminal, which a CI runner / this dev host don't have.
# Tests that exercise a dialog-driven flow (see deploy() below) stub
# the dialog_*() functions directly instead of running the real thing.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$HERE/.." && pwd)"
STUBS="$HERE/stubs"
STUBS_PY="$HERE/stubs-py-only"

pass=0
fail=0

ok()  { pass=$((pass + 1)); echo "  ok - $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL - $1"; }

# fresh_env - prints a scratch dir path with:
#   $d/root      - ROOTFS/STUB_ROOT: fake /etc, /proc, /root, /mnt
#   $d/pooldata  - STUB_POOL_DATA: what the stub `mount -t zfs` copies
#                  in as the mounted dataset's content (e.g. boot/*)
#   $d/log       - STUB_LOG
fresh_env() {
    d="$(mktemp -d)"
    mkdir -p "$d/root/etc" "$d/root/proc" "$d/root/root" "$d/root/dev" "$d/root/tmp" "$d/pooldata"
    printf 'root:x:0:0:root:/root:/bin/ash\n' > "$d/root/etc/passwd"
    printf '' > "$d/root/proc/cmdline"
    # A plain openable regular file to stand in for /dev/tty - this
    # harness's own dot-sourcing subshells never have a controlling
    # terminal at all (see the NOTE above the recovery-shell tests
    # further down), so a bare /dev/tty never resolves here. Tests that
    # exercise a passphrase prompt export STUB_TTY="$d/tty" to point
    # zfs-unlock.sh's own $ZFS_TTY at this instead - exists
    # unconditionally so it's there whether or not a given test uses it.
    : > "$d/tty"
    echo "$d"
}

# fake_target_initrd DEST - writes a fake "target initramfs" to DEST
# that the encrypted-root kexec handoff's own extraction step
# (decompress_initrd | cpio -i --to-stdout usr/sbin/zfs) can actually
# extract something real from, via tests/stubs/cpio's own tar-backed
# round-trip. A plain empty placeholder file (this suite's usual
# `: > .../initramfs-lts`, fine for every OTHER test here) is NOT
# enough for the handoff tests specifically - decompress_initrd falls
# through to a bare `cat` on non-gzip/xz content, so an empty source
# means the extraction step has nothing to extract, silently taking the
# SAME "could not extract" fallback branch regardless of whether the
# surrounding verification logic being tested is actually correct -
# confirmed the hard way once already this session (a real regression
# introduced deliberately to check a test still failed, passed anyway,
# because this exact fixture gap made the test vacuous).
fake_target_initrd() {
    dest="$1"
    src="$(mktemp -d)"
    mkdir -p "$src/usr/sbin"
    # \x7fELF - real ELF magic, since the handoff's own round-trip
    # check requires the extracted "real binary" to start with it.
    printf '\177ELF-fake-target-zfs-binary-content' > "$src/usr/sbin/zfs"
    chmod 755 "$src/usr/sbin/zfs"
    ( cd "$src" && find . | "$STUBS/cpio" -o -H newc > "$dest" 2>/dev/null )
    rm -rf "$src"
}

# run_stubbed SCRIPT ARG... - see the header comment above.
run_stubbed() {
    script="$1"; shift
    (
        # Neither init nor boot-dataset.sh use `set -e` themselves
        # (deliberately - a failed step must explain itself and fall
        # through to a shell, not abort - see their own comments).
        # Dot-sourcing shares process state with the caller, so
        # run-tests.sh's own top-level `set -eu` would otherwise leak
        # in and abort the very first redirect failure (e.g. this dev
        # host can't write /dev/kmsg without real root) - undoing that
        # here restores the scripts' own real behaviour under test.
        set +e
        mount()    { "$STUBS/mount" "$@"; }
        umount()   { "$STUBS/umount" "$@"; }
        zpool()    { "$STUBS/zpool" "$@"; }
        zfs()      { "$STUBS/zfs" "$@"; }
        kexec()    { "$STUBS/kexec" "$@"; }
        modprobe() { "$STUBS/modprobe" "$@"; }
        mdev()     { "$STUBS/mdev" "$@"; }
        ifconfig() { "$STUBS/ifconfig" "$@"; }
        udhcpc()   { "$STUBS/udhcpc" "$@"; }
        udhcpc6()  { "$STUBS/udhcpc6" "$@"; }
        ip()       { "$STUBS/ip" "$@"; }
        nft()      { "$STUBS/nft" "$@"; }
        dropbear() { "$STUBS/dropbear" "$@"; }
        dropbearkey() { "$STUBS/dropbearkey" "$@"; }
        cpio()     { "$STUBS/cpio" "$@"; }
        # setsid, not a plain `. "$script"` in this same subshell -
        # found the hard way, running this suite interactively on real
        # hardware (bureau): a bare fd-redirected subshell (`>"$d/out"
        # 2>&1`) does NOT detach from a controlling terminal - /dev/tty
        # resolution is a SESSION property, entirely independent of
        # what fds 0/1/2 point at, so a script run from a real
        # interactive terminal/SSH session inherits that session's own
        # ctty all the way down through this dot-sourced subshell. Every
        # test that stages a real die()/fail() -> _recovery_shell() call
        # (see that function's own `( : < /dev/tty ) 2>/dev/null`
        # branch check) and asserts the "no controlling terminal yet"
        # branch was silently relying on whoever invoked run-tests.sh
        # itself having no ctty - true for CI and any other detached
        # invocation, false the moment a human runs this suite from
        # their own real terminal, where it started failing FOUR real,
        # otherwise-passing tests with no code regression behind them at
        # all. `setsid` (without -c/--ctty, which does the opposite -
        # attaches the CURRENT terminal instead) creates a genuinely
        # fresh session with no controlling terminal at all, regardless
        # of the caller's own - making run_stubbed()'s own long-
        # standing "never has a controlling terminal" comment (see this
        # function's own callers, and the block comment further down
        # this file explaining exactly this) actually true unconditionally,
        # not just true by accident of how it happened to be invoked so
        # far. The stub functions above are `export -f`'d first since
        # setsid execs a genuinely NEW bash process - unlike a plain `(
        # ... )` subshell, it does not inherit function definitions any
        # other way.
        export -f mount umount zpool zfs kexec modprobe mdev ifconfig udhcpc udhcpc6 ip nft dropbear dropbearkey cpio
        # export -f only carries each function's own body text - $STUBS
        # itself (every stub function's own closed-over reference to
        # where the real tests/stubs/* scripts live) is a PLAIN variable,
        # resolved fresh by the new bash process's own environment at
        # call time, not baked into the exported function text. Without
        # this, every stub call below resolved $STUBS as empty in the
        # new process (unset, not the caller's own value) - "$STUBS/mount"
        # became the literal path "/mount", and every single stubbed
        # command failed. Found immediately while testing this exact
        # setsid fix, not shipped broken.
        export STUBS
        # "$@" here is run_stubbed()'s own remaining args after its
        # `shift` above (e.g. the DATASET/POOL args boot-dataset.sh
        # itself takes) - forwarded through as bash -c's own extra
        # arguments ($2 onward, since $1 is $script), then re-shifted
        # inside the new process so `. "$_s" "$@"` passes them on to
        # the sourced script as ITS $1 $2..., exactly as the original
        # plain `. "$script"` did by sharing positional params directly
        # with its own enclosing subshell - a new bash -c process has
        # no such sharing, so this has to be done explicitly.
        exec setsid bash -c '_s="$1"; shift; . "$_s" "$@"' -- "$script" "$@"
    )
}

# stage_rescue_ssh ROOTDIR [KEY_LINE] - drops a usable authorized_keys +
# persistent host key directly under ROOTDIR/tmp/alpine-zfsboot/, the
# exact fixed tmpfs paths /init's own ESP-discovery pass stages them
# into on a real boot (see /init's own header comment on that block).
# That discovery pass is real-boot-only ($ROOTFS-prefixed test runs
# have no real block devices to scan, same as the config file it reads
# alongside these two) - so simulating "rescue SSH is configured for
# this boot" means placing these two files directly, the same way
# fresh_env() pre-populates /etc/passwd rather than simulating whatever
# upstream step would normally write it.
stage_rescue_ssh() {
    root_dir="$1"
    key_line="${2:-ssh-ed25519 AAAAFAKEKEY testuser@laptop}"
    mkdir -p "$root_dir/tmp/alpine-zfsboot"
    printf '%s\n' "$key_line" > "$root_dir/tmp/alpine-zfsboot/authorized_keys"
    printf 'FAKE-DROPBEAR-ED25519-HOST-KEY-BYTES\n' > "$root_dir/tmp/alpine-zfsboot/ssh_host_ed25519_key"
}

# =============================================================================
echo "== syntax check =="
for f in init/init init/boot-dataset.sh init/alpine-zfsboot-shell init/net-config.sh init/rescue-ssh.sh init/pid-alive.sh init/zfs-unlock.sh init/zfs-unlock build.sh iso.sh; do
    if sh -n "$REPO_ROOT/$f"; then ok "sh -n $f"; else bad "sh -n $f"; fi
done
if python3 -m py_compile "$REPO_ROOT/init/menu.py" 2>/tmp/pyc.log; then
    ok "py_compile menu.py"
else
    cat /tmp/pyc.log; bad "py_compile menu.py"
fi

# =============================================================================
echo "== menu.py: a malformed persisted alpine-zfsboot.timeout= must not abort import =="
# Regression test for a real bug found in a full source audit:
# MENU_TIMEOUT used to be a bare int(os.environ.get(...)) at MODULE
# SCOPE, which raises ValueError at IMPORT time on any non-numeric
# value - before main()'s own try/except (further down in the file)
# exists to catch anything. /init sets this from a persisted
# alpine-zfsboot.timeout= line in EFI/ALPINE/config with zero validation
# of its own (init:426) - so one bad character there killed the entire
# rescue menu, permanently, on every console, on every boot: no recovery
# shell, no console switch, no unlock screen, nothing. py_compile above
# only proves the file PARSES - it says nothing about whether IMPORTING
# it (running its module-level code, including this exact line) crashes
# on real persisted input.
d="$(fresh_env)"
ALPINE_ZFSBOOT_MENU_TIMEOUT="10s" python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>"$d/err" || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
print("imported ok, MENU_TIMEOUT =", menu.MENU_TIMEOUT)
PYEOF
if grep -q "imported ok, MENU_TIMEOUT = 10$" "$d/out"; then
    ok "menu.py imports successfully with a malformed timeout, falling back to the documented default (10)"
else
    cat "$d/out" "$d/err"; bad "menu.py import crashed (or fell back to the wrong value) on a malformed alpine-zfsboot.timeout="
fi
if grep -q "not a whole number" "$d/err"; then
    ok "a diagnostic is printed for the malformed value, not a silent fallback"
else
    cat "$d/err"; bad "no diagnostic printed for the malformed alpine-zfsboot.timeout="
fi
rm -rf "$d"

d="$(fresh_env)"
ALPINE_ZFSBOOT_MENU_TIMEOUT="" python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
print("imported ok, MENU_TIMEOUT =", menu.MENU_TIMEOUT)
PYEOF
if grep -q "imported ok, MENU_TIMEOUT = 10$" "$d/out"; then
    ok "an empty (but present) alpine-zfsboot.timeout= also falls back to the documented default (10), same as unset"
else
    cat "$d/out"; bad "an empty alpine-zfsboot.timeout= did not fall back correctly"
fi
rm -rf "$d"

d="$(fresh_env)"
ALPINE_ZFSBOOT_MENU_TIMEOUT="30" python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
print("imported ok, MENU_TIMEOUT =", menu.MENU_TIMEOUT)
PYEOF
if grep -q "imported ok, MENU_TIMEOUT = 30$" "$d/out"; then
    ok "a genuinely valid alpine-zfsboot.timeout=30 is still honored exactly (the fallback path doesn't shadow real values)"
else
    cat "$d/out"; bad "a valid alpine-zfsboot.timeout= was not honored"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: single kernel present -> that one, kexec'd =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"
: > "$d/pooldata/boot/initramfs-lts"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" >"$d/out" 2>&1 || true
if grep -q -- "-l .*vmlinuz-lts --initrd=.*initramfs-lts" "$d/log"; then
    ok "kexec -l called with the vmlinuz-lts/initramfs-lts pair"
else
    cat "$d/log" 2>/dev/null; bad "kexec -l not called with expected kernel/initrd"
fi
if grep -q "^kexec -e$" "$d/log"; then ok "kexec -e called (handoff attempted)"; else bad "kexec -e not called"; fi
rm -rf "$d"

# =============================================================================
echo "== fix-kexec-dtb.py: finds and zeros a real nonzero /chosen/kaslr-seed, touches nothing else =="
# Real-hardware-found (QEMU/KVM aarch64 "virt" board, 2026-09-25): QEMU
# injects a random, nonzero /chosen/kaslr-seed into the live device
# tree; Alpine's aarch64 kernels don't build CONFIG_RANDOMIZE_BASE, so
# it's never consumed - kexec-tools' arm64 backend then silently
# refuses to load ("kexec: setup_2nd_dtb failed.", no detail without
# -d). This is the regression test for fix-kexec-dtb.py's own core
# logic: a hand-built, real, spec-valid FDT (encoder below is
# deliberately independent from the module's own decoder - same
# format understanding, not the same code) with a genuinely nonzero
# kaslr-seed goes in, and the fixed copy must have ONLY that property's
# bytes zeroed - every other byte in the file identical.
d="$(fresh_env)"
python3 - "$REPO_ROOT/init/fix-kexec-dtb.py" "$d" > "$d/out" 2>&1 <<'PYEOF'
import importlib.util, os, struct, sys

fix_kexec_dtb_path, workdir = sys.argv[1], sys.argv[2]

# --- minimal, real, spec-valid FDT encoder (independent of the module
# under test - see the Flattened Devicetree spec s5.3/s5.4) ---
FDT_BEGIN_NODE, FDT_END_NODE, FDT_PROP, FDT_END = 1, 2, 3, 9

def build_fdt(kaslr_seed=None):
    strings, offs, struct_block = bytearray(), {}, bytearray()

    def strid(name):
        if name not in offs:
            offs[name] = len(strings)
            strings.extend(name.encode() + b"\0")
        return offs[name]

    def align4(b):
        while len(b) % 4:
            b.append(0)

    def begin(name):
        struct_block.extend(struct.pack(">I", FDT_BEGIN_NODE))
        struct_block.extend(name.encode() + b"\0")
        align4(struct_block)

    def end():
        struct_block.extend(struct.pack(">I", FDT_END_NODE))

    def prop(name, data):
        struct_block.extend(struct.pack(">III", FDT_PROP, len(data), strid(name)))
        struct_block.extend(data)
        align4(struct_block)

    begin("")
    prop("model", b"unit-test,fake-board\0")
    begin("chosen")
    prop("bootargs", b"console=ttyS0\0")
    if kaslr_seed is not None:
        prop("kaslr-seed", kaslr_seed)
    end()
    begin("memory@40000000")
    prop("device_type", b"memory\0")
    prop("reg", struct.pack(">4I", 0, 0x40000000, 0, 0x40000000))
    end()
    end()
    struct_block.extend(struct.pack(">I", FDT_END))
    align4(struct_block)

    mem_rsvmap = struct.pack(">QQ", 0, 0)
    off_mem_rsvmap = 40
    off_dt_struct = off_mem_rsvmap + len(mem_rsvmap)
    off_dt_strings = off_dt_struct + len(struct_block)
    totalsize = off_dt_strings + len(strings)
    header = struct.pack(">10I", 0xD00DFEED, totalsize, off_dt_struct,
                          off_dt_strings, off_mem_rsvmap, 17, 16, 0,
                          len(strings), len(struct_block))
    return bytes(header) + mem_rsvmap + bytes(struct_block) + bytes(strings)

spec = importlib.util.spec_from_file_location("fix_kexec_dtb", fix_kexec_dtb_path)
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)

seed = bytes([0x47, 0x16, 0xCE, 0x7F, 0xD8, 0x1E, 0x1C, 0x78])
original = build_fdt(kaslr_seed=seed)
in_path = os.path.join(workdir, "in.dtb")
out_path = os.path.join(workdir, "out.dtb")
open(in_path, "wb").write(original)

mod.LIVE_FDT_PATH = in_path
sys.argv = ["fix-kexec-dtb.py", out_path]
result = mod.main()

fixed = open(out_path, "rb").read()
diff_positions = [i for i, (a, b) in enumerate(zip(original, fixed)) if a != b]

print("exit code:", result)
print("output file exists:", os.path.exists(out_path))
print("same length:", len(original) == len(fixed))
print("bytes changed:", len(diff_positions))
print("changed region matches seed exactly:",
      diff_positions != [] and
      original[diff_positions[0]:diff_positions[-1] + 1] == seed and
      fixed[diff_positions[0]:diff_positions[-1] + 1] == bytes(8))
PYEOF
if grep -qx "exit code: 0" "$d/out" 2>/dev/null \
   && grep -qx "output file exists: True" "$d/out" \
   && grep -qx "same length: True" "$d/out" \
   && grep -qx "bytes changed: 8" "$d/out" \
   && grep -qx "changed region matches seed exactly: True" "$d/out"; then
    ok "fix-kexec-dtb.py finds and zeros a real nonzero kaslr-seed, exactly 8 bytes changed, nothing else touched"
else
    cat "$d/out" 2>/dev/null; bad "fix-kexec-dtb.py did not correctly zero the kaslr-seed"
fi
rm -rf "$d"

# =============================================================================
echo "== fix-kexec-dtb.py: kaslr-seed absent, already zero, missing fdt, and malformed input - all no-ops, never a false positive or a crash =="
d="$(fresh_env)"
python3 - "$REPO_ROOT/init/fix-kexec-dtb.py" "$d" > "$d/out" 2>&1 <<'PYEOF'
import importlib.util, os, struct, sys

fix_kexec_dtb_path, workdir = sys.argv[1], sys.argv[2]
FDT_BEGIN_NODE, FDT_END_NODE, FDT_PROP, FDT_END = 1, 2, 3, 9

def build_fdt(kaslr_seed=None):
    strings, offs, struct_block = bytearray(), {}, bytearray()

    def strid(name):
        if name not in offs:
            offs[name] = len(strings)
            strings.extend(name.encode() + b"\0")
        return offs[name]

    def align4(b):
        while len(b) % 4:
            b.append(0)

    def begin(name):
        struct_block.extend(struct.pack(">I", FDT_BEGIN_NODE))
        struct_block.extend(name.encode() + b"\0")
        align4(struct_block)

    def end():
        struct_block.extend(struct.pack(">I", FDT_END_NODE))

    def prop(name, data):
        struct_block.extend(struct.pack(">III", FDT_PROP, len(data), strid(name)))
        struct_block.extend(data)
        align4(struct_block)

    begin("")
    begin("chosen")
    prop("bootargs", b"console=ttyS0\0")
    if kaslr_seed is not None:
        prop("kaslr-seed", kaslr_seed)
    end()
    end()
    struct_block.extend(struct.pack(">I", FDT_END))
    align4(struct_block)

    mem_rsvmap = struct.pack(">QQ", 0, 0)
    off_mem_rsvmap = 40
    off_dt_struct = off_mem_rsvmap + len(mem_rsvmap)
    off_dt_strings = off_dt_struct + len(struct_block)
    totalsize = off_dt_strings + len(strings)
    header = struct.pack(">10I", 0xD00DFEED, totalsize, off_dt_struct,
                          off_dt_strings, off_mem_rsvmap, 17, 16, 0,
                          len(strings), len(struct_block))
    return bytes(header) + mem_rsvmap + bytes(struct_block) + bytes(strings)

spec = importlib.util.spec_from_file_location("fix_kexec_dtb", fix_kexec_dtb_path)
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)

def run(label, live_path, out_name):
    mod.LIVE_FDT_PATH = live_path
    out_path = os.path.join(workdir, out_name)
    sys.argv = ["fix-kexec-dtb.py", out_path]
    rc = mod.main()
    print(f"{label}: rc={rc} wrote_output={os.path.exists(out_path)}")

absent_path = os.path.join(workdir, "absent-seed.dtb")
open(absent_path, "wb").write(build_fdt(kaslr_seed=None))
run("absent", absent_path, "absent-out.dtb")

zero_path = os.path.join(workdir, "zero-seed.dtb")
open(zero_path, "wb").write(build_fdt(kaslr_seed=bytes(8)))
run("already-zero", zero_path, "zero-out.dtb")

run("no-fdt-file", os.path.join(workdir, "does-not-exist.dtb"), "missing-out.dtb")

garbage_path = os.path.join(workdir, "garbage.dtb")
open(garbage_path, "wb").write(b"not an fdt blob, just garbage bytes 1234567890")
run("garbage", garbage_path, "garbage-out.dtb")
PYEOF
if grep -qx "absent: rc=0 wrote_output=False" "$d/out" \
   && grep -qx "already-zero: rc=0 wrote_output=False" "$d/out" \
   && grep -qx "no-fdt-file: rc=1 wrote_output=False" "$d/out" \
   && grep -qx "garbage: rc=1 wrote_output=False" "$d/out"; then
    ok "fix-kexec-dtb.py is a clean no-op on every case that isn't the real bug - never writes a file, never a nonzero rc on the two genuinely-fine cases"
else
    cat "$d/out" 2>/dev/null; bad "fix-kexec-dtb.py did not behave as a safe no-op on one of the non-bug cases"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: fix-kexec-dtb.py finding nothing to fix -> kexec called with no --dtb= at all (identical to today) =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"
: > "$d/pooldata/boot/initramfs-lts"
cat > "$d/fake-fix-kexec-dtb.py" <<'EOF'
import sys
sys.exit(1)  # "nothing to do" - the real script's own contract on every non-bug platform
EOF
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    STUB_FIX_KEXEC_DTB_SCRIPT="$d/fake-fix-kexec-dtb.py" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" >"$d/out" 2>&1 || true
if grep -q -- "-l .*vmlinuz-lts --initrd=.*initramfs-lts --command-line=" "$d/log" \
   && ! grep -q -- "--dtb=" "$d/log"; then
    ok "no --dtb= passed when there's nothing to fix - identical kexec invocation to before this change"
else
    cat "$d/log" 2>/dev/null; bad "kexec -l invocation changed even though fix-kexec-dtb.py found nothing to fix"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: fix-kexec-dtb.py finding a real fix -> --dtb= passed to kexec with the fixed path =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"
: > "$d/pooldata/boot/initramfs-lts"
cat > "$d/fake-fix-kexec-dtb.py" <<EOF
import sys
out = sys.argv[1]
open(out, "wb").write(b"fake fixed dtb bytes")
print(out)
EOF
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    STUB_FIX_KEXEC_DTB_SCRIPT="$d/fake-fix-kexec-dtb.py" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" >"$d/out" 2>&1 || true
if grep -q -- "--dtb=/tmp/kexec-fixed.dtb" "$d/log"; then
    ok "--dtb=<fixed path> passed to kexec when fix-kexec-dtb.py found and fixed a nonzero kaslr-seed"
else
    cat "$d/log" 2>/dev/null; bad "--dtb= was not passed to kexec even though fix-kexec-dtb.py reported a fix"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: zpool export failing before the jump stops the boot via fail(), never a silent kexec -e =="
# Regression test for a real gap a full source audit found: `zpool
# export "$POOL"` right before the kexec jump was called with its exit
# status entirely unchecked (`2>/dev/null`, no `if`) - a real, non-force
# export refuses outright if any dataset under the pool is still
# mounted/busy (another boot environment, a rescue-SSH chroot_be()
# session left open), and this used to jump into the new kernel anyway
# regardless. kexec -l has already succeeded by this point (the new
# kernel is staged, not yet triggered), so refusing via fail() here is
# fully safe - nothing is lost, the operator just gets a chance to find
# out why before retrying.
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"
: > "$d/pooldata/boot/initramfs-lts"
STUB_ZPOOL_EXPORT_FAIL=1
export STUB_ZPOOL_EXPORT_FAIL
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" >"$d/out" 2>&1 || true
unset STUB_ZPOOL_EXPORT_FAIL
if grep -qi "failed to export zroot before the kexec jump" "$d/out" \
   && ! grep -q "^kexec -e$" "$d/log" 2>/dev/null \
   && grep -q "TEST STUB: would exec setsid -c /bin/bash (no controlling terminal yet)" "$d/out"; then
    ok "a failed zpool export stops the boot via fail() before kexec -e - never an unexplained silent jump"
else
    cat "$d/out" 2>/dev/null; cat "$d/log" 2>/dev/null
    bad "a failed zpool export did not stop the boot as expected (kexec -e may have still run)"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: F21 busy pre-check refuses BEFORE the attempt record/bootcheck counter are ever touched, not just before the real zpool export =="
# Regression test for F21 (unidoc-alip's PR #5 review): the real zpool
# export hard-fail (test just above) already stops the boot safely,
# but it used to run strictly AFTER the attempt record and bootcheck
# counter were already written - so a refused export still counted as
# a real boot attempt for Last Boot Diagnostics, even though kexec -e
# never ran at all. This test proves the NEW read-only /proc/mounts
# pre-check refuses first, before either of those writes ever happens
# - STUB_PROC_MOUNTS points at a fixture claiming a descendant dataset
# is already mounted elsewhere (a stand-in for another boot environment
# or a left-open rescue-SSH chroot_be() session).
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"
: > "$d/pooldata/boot/initramfs-lts"
printf 'zroot/ROOT/other /mnt/other zfs rw 0 0\n' > "$d/fake-proc-mounts"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_PROC_MOUNTS="$d/fake-proc-mounts" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" >"$d/out" 2>&1 || true
if grep -qi "still has something else mounted" "$d/out" \
   && grep -qi "zroot/ROOT/other at /mnt/other" "$d/out" \
   && grep -q "TEST STUB: would exec setsid -c /bin/bash (no controlling terminal yet)" "$d/out" \
   && ! grep -q "^kexec -e$" "$d/log" 2>/dev/null \
   && ! grep -q "^zpool export" "$d/log" 2>/dev/null \
   && ! grep -q "^zfs set.*attempt_" "$d/log" 2>/dev/null \
   && ! grep -q "^zfs set.*bootcheck" "$d/log" 2>/dev/null; then
    ok "the busy pre-check refuses (drops to a rescue shell via fail()) before the real zpool export, the attempt record, AND the bootcheck counter are ever reached"
else
    cat "$d/out" 2>/dev/null; cat "$d/log" 2>/dev/null
    bad "the busy pre-check either did not fire, or something downstream of it still ran (zpool export / attempt record / bootcheck write)"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: N2 - the bootcheck confirm-service check reads \$ROOTFS/mnt/root BEFORE it gets unmounted, not after =="
# Regression test for N2 (unidoc-alip's PR #5 follow-up review): F21
# (test just above) moved `umount "$ROOTFS"/mnt/root` earlier in this
# file to make its own pre-check possible, but the bootcheck
# confirm-service file tests further down still read paths UNDER that
# same mount - so once the umount ran first, both tests always read
# "absent", and every armed BE silently disarmed itself
# (org.alpinezfsboot:bootcheck reset to armed:0) instead of ever
# incrementing, on every single real boot. Not reproducible against
# this suite's own default umount stub (a no-op that never actually
# removes anything) - STUB_UMOUNT_REALLY_UNMOUNT makes it really hide
# the target directory's content, the same real-world behavior a
# genuine umount has, so this test fails against the pre-fix ordering
# and passes against the fix.
#
# Bypasses run_stubbed (which always wires the plain, not-bootcheck-
# aware `zfs` stub) in favor of the same fully-manual inline-subshell
# shape the /init bootcheck tests already use elsewhere in this suite,
# so zfs() can be zfs.bootcheck here instead.
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
# Under $STUB_POOL_DATA, NOT directly under $d/root/mnt/root - the
# mount stub is what actually populates $ROOTFS/mnt/root (by copying
# $STUB_POOL_DATA's own content into it), and this file lives BEFORE
# that mount stub's own call in this script's own flow, AFTER an EARLY
# pre-mount cleanup `umount` (line ~584, unrelated to N2 - this one
# runs before ANY mount has happened, clearing any stale leftover from
# a previous crashed attempt) that STUB_UMOUNT_REALLY_UNMOUNT would
# otherwise also hide away if this fixture were placed directly under
# $d/root/mnt/root ahead of time.
mkdir -p "$d/pooldata/etc/runlevels/default" "$d/pooldata/etc/init.d"
ln -s /etc/init.d/alpine-zfsboot-bootcheck "$d/pooldata/etc/runlevels/default/alpine-zfsboot-bootcheck"
: > "$d/pooldata/etc/init.d/alpine-zfsboot-bootcheck"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.bootcheck" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    modprobe() { "$STUBS/modprobe" "$@"; }
    mdev()     { "$STUBS/mdev" "$@"; }
    ifconfig() { "$STUBS/ifconfig" "$@"; }
    udhcpc()   { "$STUBS/udhcpc" "$@"; }
    udhcpc6()  { "$STUBS/udhcpc6" "$@"; }
    ip()       { "$STUBS/ip" "$@"; }
    nft()      { "$STUBS/nft" "$@"; }
    dropbear() { "$STUBS/dropbear" "$@"; }
    dropbearkey() { "$STUBS/dropbearkey" "$@"; }
    cpio()     { "$STUBS/cpio" "$@"; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
        STUB_BOOTCHECK_VALUE="armed:2" STUB_BOOTCHECK_SOURCE="local" \
        STUB_UMOUNT_REALLY_UNMOUNT=1
    export STUB_LOG STUB_ROOT STUB_POOL_DATA STUB_BOOTCHECK_VALUE STUB_BOOTCHECK_SOURCE STUB_UMOUNT_REALLY_UNMOUNT
    . "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot"
) >"$d/out" 2>&1 || true
if grep -q "^zfs set.*bootcheck=armed:3.*zroot/ROOT/alpine" "$d/log" 2>/dev/null \
   && ! grep -qi "disarming" "$d/out" 2>/dev/null; then
    ok "a real confirm service present before the umount is still seen as present - armed:2 -> armed:3, not disarmed"
else
    cat "$d/out" 2>/dev/null; cat "$d/log" 2>/dev/null
    bad "the confirm-service check read \$ROOTFS/mnt/root AFTER it was unmounted - a real, armed BE with a real confirm service was incorrectly disarmed"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: unencrypted dataset -> no load-key call at all, log unchanged =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" >"$d/out" 2>&1 || true
if ! grep -q "load-key" "$d/log" 2>/dev/null && grep -q -- "-l .*vmlinuz-lts" "$d/log"; then
    ok "encryptionroot=- -> no load-key call, boots exactly as before encryption existed"
else
    cat "$d/log" 2>/dev/null; bad "unencrypted dataset unexpectedly triggered a load-key call"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: encrypted + locked dataset -> load-key on the encryptionroot, not \$DATASET =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
cat > "$STUBS/zfs.encrypted" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    get)
        # args: get -H -o value PROPERTY DATASET
        prop="$5"; ds="$6"
        case "$prop" in
            encryptionroot) echo "zroot/ROOT" ;;   # a PARENT of $DATASET, not $DATASET itself
            keystatus) [ "$ds" = "zroot/ROOT" ] && echo "unavailable" || echo "-" ;;
            keylocation) echo "prompt" ;;
            org.alpinezfsboot:commandline) echo "-" ;;
        esac
        ;;
    load-key)
        # succeeds only when targeting the encryptionroot itself
        [ "$2" = "zroot/ROOT" ]
        ;;
esac
exit 0
EOF
chmod +x "$STUBS/zfs.encrypted"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.encrypted" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    # No real terminal/dialog binary under the test harness - the
    # passphrase content doesn't matter to zfs.encrypted's own stub
    # above (it decides success/failure by target dataset name, not by
    # what was typed), so a fixed value on stderr (real dialog's own
    # --passwordbox convention - see boot-dataset.sh's own 2>"$file"
    # capture) is enough to exercise the real code path.
    dialog()   { echo "test-passphrase" >&2; return 0; }
    stty()     { :; }
    clear()    { :; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_TTY="$d/tty"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA STUB_TTY
    . "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot"
) >"$d/out" 2>&1 || true
if grep -q "^zfs load-key zroot/ROOT$" "$d/log" 2>/dev/null \
   && ! grep -q "^zfs load-key zroot/ROOT/alpine$" "$d/log" \
   && grep -q -- "-l .*vmlinuz-lts" "$d/log"; then
    ok "load-key targets the encryptionroot (zroot/ROOT), not the leaf dataset - and boot proceeds once unlocked"
else
    cat "$d/log" 2>/dev/null; bad "load-key was not called on the correct encryptionroot as expected"
fi
rm -f "$STUBS/zfs.encrypted"
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: rescue SSH running -> a clear 'shutting down, continuing via kexec' notice is logged BEFORE dropbear is stopped =="
# Confirmed on real hardware: to an operator connected over the exact
# SSH session about to be torn down by kexec (which replaces this
# whole rescue kernel - there is no way to keep that connection alive
# across it), dropbear just stopping with no explanation looks
# identical to the connection dying for no reason. The notice must
# appear, and appear BEFORE "stopped", not after (after which nothing
# reaches that terminal at all).
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
stage_rescue_ssh "$d/root"
STUB_DROPBEAR_SLEEP=10
export STUB_DROPBEAR_SLEEP
cat > "$STUBS/zfs.encrypted3" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    get)
        prop="$5"; ds="$6"
        case "$prop" in
            encryptionroot) echo "zroot/ROOT" ;;
            keystatus) [ "$ds" = "zroot/ROOT" ] && echo "unavailable" || echo "-" ;;
            keylocation) echo "prompt" ;;
            org.alpinezfsboot:commandline) echo "-" ;;
        esac
        ;;
    load-key) [ "$2" = "zroot/ROOT" ] ;;
esac
exit 0
EOF
chmod +x "$STUBS/zfs.encrypted3"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.encrypted3" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    ifconfig() { "$STUBS/ifconfig" "$@"; }
    ip()       { "$STUBS/ip" "$@"; }
    udhcpc()   { "$STUBS/udhcpc" "$@"; }
    udhcpc6()  { "$STUBS/udhcpc6" "$@"; }
    nft()      { "$STUBS/nft" "$@"; }
    dropbear() { "$STUBS/dropbear" "$@"; }
    dropbearkey() { "$STUBS/dropbearkey" "$@"; }
    dialog()   { echo "test-passphrase" >&2; return 0; }
    stty()     { :; }
    clear()    { :; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_TTY="$d/tty"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA STUB_TTY
    . "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot"
) >"$d/out" 2>&1 || true
notice_line="$(grep -n "rescue SSH is shutting down" "$d/out" | head -1 | cut -d: -f1)"
stopped_line="$(grep -n "dropbear sshd (pid=.*) stopped" "$d/out" | head -1 | cut -d: -f1)"
if [ -n "$notice_line" ] && [ -n "$stopped_line" ] && [ "$notice_line" -lt "$stopped_line" ]; then
    ok "the 'shutting down, continuing via kexec' notice is logged, and appears BEFORE dropbear is actually stopped"
else
    cat "$d/out"; echo "notice_line=$notice_line stopped_line=$stopped_line"
    bad "the pre-shutdown notice did not appear (or appeared too late) before rescue SSH was stopped"
fi
rm -f "$STUBS/zfs.encrypted3"
rm -rf "$d"
unset STUB_DROPBEAR_SLEEP

# =============================================================================
echo "== boot-dataset.sh: rescue SSH never started -> no 'shutting down' notice on a purely local unlock =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
cat > "$STUBS/zfs.encrypted4" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    get)
        prop="$5"; ds="$6"
        case "$prop" in
            encryptionroot) echo "zroot/ROOT" ;;
            keystatus) [ "$ds" = "zroot/ROOT" ] && echo "unavailable" || echo "-" ;;
            keylocation) echo "prompt" ;;
            org.alpinezfsboot:commandline) echo "-" ;;
        esac
        ;;
    load-key) [ "$2" = "zroot/ROOT" ] ;;
esac
exit 0
EOF
chmod +x "$STUBS/zfs.encrypted4"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.encrypted4" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    dialog()   { echo "test-passphrase" >&2; return 0; }
    stty()     { :; }
    clear()    { :; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_TTY="$d/tty"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA STUB_TTY
    . "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot"
) >"$d/out" 2>&1 || true
if ! grep -q "rescue SSH is shutting down" "$d/out" 2>/dev/null; then
    ok "no confusing 'rescue SSH is shutting down' notice on a boot where rescue SSH was never started at all"
else
    cat "$d/out"; bad "the shutdown notice appeared even though rescue SSH was never running"
fi
rm -f "$STUBS/zfs.encrypted4"
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: kexec handoff - success path leaves no staging/scratch files behind =="
# The secret-lifecycle question this project's own author's advisor
# raised: not just "does the wrapper route correctly" (see the test
# below) but "is every intermediate copy of the plaintext actually
# gone afterward" - zfs-key.*, zfs-handoff.*, and zfs-handoff.*.cpio
# should all be unlinked well before kexec -l even runs (see boot-
# dataset.sh's own handoff block), leaving nothing in $ROOTFS/tmp for
# this test to find once the run completes.
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; fake_target_initrd "$d/pooldata/boot/initramfs-lts"
cat > "$STUBS/zfs.encrypted" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    get)
        prop="$5"; ds="$6"
        case "$prop" in
            encryptionroot) echo "zroot/ROOT" ;;
            keystatus) [ "$ds" = "zroot/ROOT" ] && echo "unavailable" || echo "-" ;;
            keylocation) echo "prompt" ;;
            org.alpinezfsboot:commandline) echo "-" ;;
        esac
        ;;
    load-key)
        [ "$2" = "zroot/ROOT" ]
        ;;
esac
exit 0
EOF
chmod +x "$STUBS/zfs.encrypted"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.encrypted" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    cpio()     { "$STUBS/cpio" "$@"; }
    dialog()   { echo "test-passphrase" >&2; return 0; }
    stty()     { :; }
    clear()    { :; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_TTY="$d/tty"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA STUB_TTY
    . "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot"
) >"$d/out" 2>&1 || true
leftover="$(find "$d/root/tmp" -maxdepth 1 \( -name 'zfs-key.*' -o -name 'zfs-handoff.*' -o -name 'zfs-combined-initrd.*' \) 2>/dev/null)"
if [ -z "$leftover" ] && grep -q -- "-l .*vmlinuz-lts" "$d/log" 2>/dev/null; then
    ok "successful handoff+boot leaves no zfs-key/handoff/combined-initrd scratch files behind"
else
    echo "leftover: $leftover"; cat "$d/log" 2>/dev/null; bad "scratch files were left behind after a successful boot (leftover: $leftover)"
fi
rm -f "$STUBS/zfs.encrypted"
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: kexec handoff - a truncated staged key is detected and rejected, never appended half-written =="
# Regression test for a real gap a full source audit found: the
# round-trip verification block only checked the extracted key file
# EXISTS (-f), not that its content actually matches what was staged -
# a cp interrupted partway (signal, tmpfs I/O error) would leave a
# real, non-empty, truncated file that check couldn't tell apart from
# a correct one. cp() is overridden here to simulate exactly that one
# failure (truncating ONLY the key file's own copy, nothing else) -
# the fixed code must refuse to build the combined initrd at all and
# fall back to the plain, unmodified target initrd instead.
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; fake_target_initrd "$d/pooldata/boot/initramfs-lts"
cat > "$STUBS/zfs.encrypted-truncated" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    get)
        prop="$5"; ds="$6"
        case "$prop" in
            encryptionroot) echo "zroot/ROOT" ;;
            keystatus) [ "$ds" = "zroot/ROOT" ] && echo "unavailable" || echo "-" ;;
            keylocation) echo "prompt" ;;
            org.alpinezfsboot:commandline) echo "-" ;;
        esac
        ;;
    load-key)
        [ "$2" = "zroot/ROOT" ]
        ;;
esac
exit 0
EOF
chmod +x "$STUBS/zfs.encrypted-truncated"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.encrypted-truncated" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    cpio()     { "$STUBS/cpio" "$@"; }
    dialog()   { echo "test-passphrase" >&2; return 0; }
    stty()     { :; }
    clear()    { :; }
    # Only the ONE real cp() call in boot-dataset.sh's own handoff
    # block (staging the key into $handoff_root) - the exact,
    # deterministic source path zfs_key_stage_path() computes for this
    # test's own encryptionroot ("zroot/ROOT"), matched independently
    # of $$ so this doesn't depend on which subshell PID happens to be
    # running.
    cp() {
        if [ "$1" = "$d/root/tmp/zfs-key.zroot_ROOT" ]; then
            dd if="$1" of="$2" bs=1 count=4 2>/dev/null
        else
            command cp "$1" "$2"
        fi
    }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_TTY="$d/tty"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA STUB_TTY
    . "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot"
) >"$d/out" 2>&1 || true
leftover="$(find "$d/root/tmp" -maxdepth 1 \( -name 'zfs-key.*' -o -name 'zfs-handoff.*' -o -name 'zfs-combined-initrd.*' \) 2>/dev/null)"
if grep -qi "handoff verification failed" "$d/out" \
   && grep -q -- "-l .*vmlinuz-lts --initrd=.*mnt/root/boot/initramfs-lts" "$d/log" 2>/dev/null \
   && ! grep -q -- "zfs-combined-initrd" "$d/log" 2>/dev/null \
   && [ -z "$leftover" ]; then
    ok "a truncated staged key is caught by the round-trip check - kexec falls back to the plain target initrd, no scratch files left behind"
else
    echo "leftover: $leftover"; cat "$d/out"; echo "log:"; cat "$d/log" 2>/dev/null
    bad "a truncated staged key was NOT detected - the handoff may have appended a corrupt/incomplete secret instead of failing safe"
fi
rm -f "$STUBS/zfs.encrypted-truncated"
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: kexec handoff - a failed write while BUILDING the combined initrd falls back safely, never kexecs a truncated image =="
# Regression test for a real gap the full 8-way source audit found: the
# round-trip verification above only proves the small handoff cpio's
# own SOURCE material is correct - it says nothing about whether
# actually COMBINING it with the real (potentially large) target initrd
# succeeds. `cat "$initrd" > "$kexec_initrd"`, the alignment `dd`, and
# the final `cat "$handoff_cpio" >> ...` append were all unchecked - a
# real, plausible ENOSPC case (this creates a full second copy of the
# target initrd in the SAME tmpfs that already holds the original)
# would silently truncate $kexec_initrd and proceed straight to
# kexec -l/-e regardless, handing the target kernel a truncated gzip
# stream to panic on instead of the documented graceful "just
# re-prompts" degradation. cat() is overridden here to fail ONLY the
# initial initrd-copy step (matched on $initrd's own real path, so
# every OTHER `cat` call in this file - log dumps, /tmp/*.log - is
# unaffected).
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; fake_target_initrd "$d/pooldata/boot/initramfs-lts"
cat > "$STUBS/zfs.encrypted-buildfail" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    get)
        prop="$5"; ds="$6"
        case "$prop" in
            encryptionroot) echo "zroot/ROOT" ;;
            keystatus) [ "$ds" = "zroot/ROOT" ] && echo "unavailable" || echo "-" ;;
            keylocation) echo "prompt" ;;
            org.alpinezfsboot:commandline) echo "-" ;;
        esac
        ;;
    load-key)
        [ "$2" = "zroot/ROOT" ]
        ;;
esac
exit 0
EOF
chmod +x "$STUBS/zfs.encrypted-buildfail"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.encrypted-buildfail" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    cpio()     { "$STUBS/cpio" "$@"; }
    dialog()   { echo "test-passphrase" >&2; return 0; }
    stty()     { :; }
    clear()    { :; }
    # $initrd is `cat`'d TWICE in a real run before this point: once
    # inside decompress_initrd() (extracting usr/sbin/zfs for the
    # wrapper - must succeed, or a completely different, earlier
    # failure path fires instead of the one this test targets), and
    # again here, actually building the combined initrd - the SECOND
    # invocation is the one this test needs to fail. A plain counter
    # file (not a shell variable - the first call runs inside a
    # `decompress_initrd | cpio` pipeline, a separate subshell) tells
    # them apart.
    cat() {
        if [ "${1:-}" = "$d/root/mnt/root/boot/initramfs-lts" ]; then
            n=$(( $(command cat "$d/cat_initrd_count" 2>/dev/null || echo 0) + 1 ))
            echo "$n" > "$d/cat_initrd_count"
            if [ "$n" -ge 2 ]; then
                echo "cat: simulated ENOSPC" >&2
                return 1
            fi
        fi
        command cat "$@"
    }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_TTY="$d/tty"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA STUB_TTY
    . "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot"
) >"$d/out" 2>&1 || true
leftover="$(find "$d/root/tmp" -maxdepth 1 \( -name 'zfs-key.*' -o -name 'zfs-handoff.*' -o -name 'zfs-combined-initrd.*' \) 2>/dev/null)"
if grep -qi "failed to write the combined handoff initrd" "$d/out" \
   && grep -q -- "-l .*vmlinuz-lts --initrd=.*mnt/root/boot/initramfs-lts" "$d/log" 2>/dev/null \
   && ! grep -q -- "zfs-combined-initrd" "$d/log" 2>/dev/null \
   && [ -z "$leftover" ]; then
    ok "a failed write while building the combined initrd falls back to the plain target initrd, never kexecs a truncated image"
else
    echo "leftover: $leftover"; cat "$d/out"; echo "log:"; cat "$d/log" 2>/dev/null
    bad "a failed combined-initrd write was NOT caught - kexec may have proceeded with a truncated image"
fi
rm -f "$STUBS/zfs.encrypted-buildfail"
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: kexec handoff - combined initrd pads the handoff segment to a 4-byte-aligned offset =="
# The actual confirmed root cause of a real hardware failure: `cat
# original-initrd handoff.cpio` with no padding lands the second
# (uncompressed newc) segment at whatever byte offset the first
# segment happened to end at - the kernel's own init/initramfs.c only
# recognizes the START of a new uncompressed cpio segment when that
# offset is a multiple of 4 (confirmed against its own source), so an
# unlucky (non-4-aligned) original initrd size makes the kernel keep
# ONLY the first segment's content, no error anywhere - exactly what a
# real boot showed (archive round-trip-verified as correct, target
# still unwrapped). This tests the actual byte layout handed to `kexec
# -l`, not just file presence - deliberately forces the original
# initrd's own size off a 4-byte boundary (real gzip-compressed sizes
# have no reason to land on one by chance either) rather than trusting
# the test fixture to happen to already be misaligned.
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"
fake_target_initrd "$d/pooldata/boot/initramfs-lts"
printf 'xyz' >> "$d/pooldata/boot/initramfs-lts"
orig_size="$(wc -c < "$d/pooldata/boot/initramfs-lts")"
cat > "$STUBS/zfs.encrypted" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    get)
        prop="$5"; ds="$6"
        case "$prop" in
            encryptionroot) echo "zroot/ROOT" ;;
            keystatus) [ "$ds" = "zroot/ROOT" ] && echo "unavailable" || echo "-" ;;
            keylocation) echo "prompt" ;;
            org.alpinezfsboot:commandline) echo "-" ;;
        esac
        ;;
    load-key)
        [ "$2" = "zroot/ROOT" ]
        ;;
esac
exit 0
EOF
chmod +x "$STUBS/zfs.encrypted"
cat > "$STUBS/kexec.captureinitrd" <<EOF
#!/bin/sh
echo "kexec \$*" >> "\${STUB_LOG:-/dev/null}"
for a in "\$@"; do
    case "\$a" in
        --initrd=*) cp "\${a#--initrd=}" "$d/captured-initrd" ;;
    esac
done
exit 0
EOF
chmod +x "$STUBS/kexec.captureinitrd"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.encrypted" "$@"; }
    kexec()    { "$STUBS/kexec.captureinitrd" "$@"; }
    cpio()     { "$STUBS/cpio" "$@"; }
    dialog()   { echo "test-passphrase" >&2; return 0; }
    stty()     { :; }
    clear()    { :; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_TTY="$d/tty"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA STUB_TTY
    . "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot"
) >"$d/out" 2>&1 || true
if [ -f "$d/captured-initrd" ]; then
    total_size="$(wc -c < "$d/captured-initrd")"
    pad=$(( (4 - orig_size % 4) % 4 ))
    handoff_start=$((orig_size + pad))
    pad_ok=1
    if [ "$pad" -gt 0 ]; then
        padbytes="$(dd if="$d/captured-initrd" bs=1 skip="$orig_size" count="$pad" 2>/dev/null | od -An -tx1 | tr -d ' \n')"
        want="$(i=0; while [ "$i" -lt "$pad" ]; do printf '00'; i=$((i+1)); done)"
        [ "$padbytes" = "$want" ] || pad_ok=0
    fi
    if [ "$pad_ok" = 1 ] && [ $((handoff_start % 4)) -eq 0 ] && [ "$total_size" -gt "$handoff_start" ] && [ "$pad" -gt 0 ]; then
        ok "combined initrd inserts $pad zero pad byte(s) so the handoff segment starts 4-byte-aligned (orig=$orig_size bytes)"
    else
        cat "$d/log" 2>/dev/null
        bad "combined initrd padding/alignment incorrect (orig=$orig_size pad=$pad handoff_start=$handoff_start total=$total_size pad_ok=$pad_ok)"
    fi
else
    cat "$d/log" 2>/dev/null
    bad "kexec was never called with an --initrd= argument to capture (or handoff verification did not build a combined initrd at all)"
fi
rm -f "$STUBS/zfs.encrypted" "$STUBS/kexec.captureinitrd"
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: kexec handoff - kexec -l failing still cleans up the combined-initrd handoff file =="
# The one real gap a review of this lifecycle found: kexec -l itself
# failing (as opposed to kexec -e failing AFTER a successful -l, which
# the plain rm right after that call already covered) used to leave
# the plaintext-embedding combined-initrd file sitting in $ROOTFS/tmp
# indefinitely - fixed by fail() itself cleaning it up too (guarded
# with ${var:-} for the many OTHER failure paths that call fail()
# before $kexec_initrd is ever assigned at all).
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; fake_target_initrd "$d/pooldata/boot/initramfs-lts"
cat > "$STUBS/zfs.encrypted2" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    get)
        prop="$5"; ds="$6"
        case "$prop" in
            encryptionroot) echo "zroot/ROOT" ;;
            keystatus) [ "$ds" = "zroot/ROOT" ] && echo "unavailable" || echo "-" ;;
            keylocation) echo "prompt" ;;
            org.alpinezfsboot:commandline) echo "-" ;;
        esac
        ;;
    load-key)
        [ "$2" = "zroot/ROOT" ]
        ;;
esac
exit 0
EOF
chmod +x "$STUBS/zfs.encrypted2"
cat > "$STUBS/kexec.failboot" <<'EOF'
#!/bin/sh
echo "kexec $*" >> "${STUB_LOG:-/dev/null}"
exit 1
EOF
chmod +x "$STUBS/kexec.failboot"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.encrypted2" "$@"; }
    kexec()    { "$STUBS/kexec.failboot" "$@"; }
    cpio()     { "$STUBS/cpio" "$@"; }
    dialog()   { echo "test-passphrase" >&2; return 0; }
    stty()     { :; }
    clear()    { :; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_TTY="$d/tty"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA STUB_TTY
    . "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot"
) >"$d/out" 2>&1 || true
leftover="$(find "$d/root/tmp" -maxdepth 1 -name 'zfs-combined-initrd.*' 2>/dev/null)"
if [ -z "$leftover" ] && grep -qi "kexec -l failed" "$d/out" 2>/dev/null; then
    ok "kexec -l failing still cleans up the combined-initrd handoff file via fail()"
else
    echo "leftover: $leftover"; cat "$d/out" 2>/dev/null; bad "combined-initrd handoff file was left behind after kexec -l failed (leftover: $leftover)"
fi
rm -f "$STUBS/zfs.encrypted2" "$STUBS/kexec.failboot"
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: kexec handoff zfs wrapper - load-key with a staged key goes through -L file://, anything else passes through unchanged =="
# Extracted directly out of boot-dataset.sh's own heredoc (not hand-
# duplicated here) so this test can never silently drift from what a
# real boot actually embeds in the handoff cpio - see that heredoc's
# own header comment (grep boot-dataset.sh for "Encrypted-root kexec
# handoff") for the full mechanism this is testing.
d="$(fresh_env)"
wrapper="$d/zfs-wrapper"
start=$(grep -n "cat > \"\$handoff_root/usr/sbin/zfs\" <<'ZFSWRAP'" "$REPO_ROOT/init/boot-dataset.sh" | head -1 | cut -d: -f1)
end=$(awk -v s="$start" 'NR>s && $0=="ZFSWRAP"{print NR; exit}' "$REPO_ROOT/init/boot-dataset.sh")
if [ -z "$start" ] || [ -z "$end" ]; then
    bad "could not locate the ZFSWRAP heredoc in boot-dataset.sh to extract for testing"
else
    sed -n "$((start+1)),$((end-1))p" "$REPO_ROOT/init/boot-dataset.sh" > "$wrapper"
    chmod +x "$wrapper"
    mkdir -p "$d/run/alpine-zfsboot"
    cat > "$d/zfs.alpine-zfsboot-real" <<EOF
#!/bin/sh
echo "real-zfs \$*" >> "$d/log"
exit 0
EOF
    chmod +x "$d/zfs.alpine-zfsboot-real"
    sed -i \
        -e "s#/usr/sbin/zfs.alpine-zfsboot-real#$d/zfs.alpine-zfsboot-real#g" \
        -e "s#/run/alpine-zfsboot/zfs-key#$d/run/alpine-zfsboot/zfs-key#g" \
        "$wrapper"
    echo secretpassphrase > "$d/run/alpine-zfsboot/zfs-key"
    "$wrapper" load-key zroot/ROOT >"$d/out" 2>&1
    if grep -q "^real-zfs load-key -L file://$d/run/alpine-zfsboot/zfs-key zroot/ROOT$" "$d/log" 2>/dev/null \
       && [ ! -e "$d/run/alpine-zfsboot/zfs-key" ]; then
        ok "load-key with a staged key present routes through -L file:// and removes the key file"
    else
        cat "$d/log" 2>/dev/null; bad "wrapper did not route load-key through -L file:// as expected, or left the key file behind"
    fi
    : > "$d/log"
    "$wrapper" snapshot zroot/ROOT@test >"$d/out" 2>&1
    if grep -q "^real-zfs snapshot zroot/ROOT@test$" "$d/log" 2>/dev/null; then
        ok "any other zfs subcommand passes through to the real binary unchanged"
    else
        cat "$d/log" 2>/dev/null; bad "wrapper did not pass through a non-load-key call correctly"
    fi
    : > "$d/log"
    "$wrapper" load-key zroot/ROOT >"$d/out" 2>&1
    if grep -q "^real-zfs load-key zroot/ROOT$" "$d/log" 2>/dev/null; then
        ok "load-key with no staged key present falls through unchanged - a normal interactive prompt still happens"
    else
        cat "$d/log" 2>/dev/null; bad "wrapper did not fall through correctly when no key was staged"
    fi
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: wrong passphrase 3x -> falls back to a shell, no kexec =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
cat > "$STUBS/zfs.wrongkey" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    get)
        prop="$5"
        case "$prop" in
            encryptionroot) echo "zroot/ROOT/alpine" ;;
            keystatus) echo "unavailable" ;;
            keylocation) echo "prompt" ;;
            org.alpinezfsboot:commandline) echo "-" ;;
        esac
        ;;
    load-key) exit 1 ;;  # always wrong
esac
exit 0
EOF
chmod +x "$STUBS/zfs.wrongkey"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.wrongkey" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    # Same reasoning as the encryptionroot test above - dialog is a
    # normal killable process under the test harness too, and this
    # test's whole point is that zfs.wrongkey rejects the key content-
    # independently, all 3 times.
    dialog()   { echo "test-passphrase" >&2; return 0; }
    stty()     { :; }
    clear()    { :; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_TTY="$d/tty"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA STUB_TTY
    . "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot"
) >"$d/out" 2>&1 || true
attempts=$(grep -c "^zfs load-key zroot/ROOT/alpine$" "$d/log" 2>/dev/null || true)
if [ "$attempts" = 3 ] && ! grep -q "^mount" "$d/log" 2>/dev/null && grep -qi "failed to load encryption key" "$d/out"; then
    ok "retries exactly 3 times on keylocation=prompt, then falls back to a shell without ever mounting"
else
    cat "$d/out"; cat "$d/log" 2>/dev/null; bad "wrong-passphrase handling did not behave as expected (attempts=$attempts)"
fi
rm -f "$STUBS/zfs.wrongkey"
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: non-prompt keylocation -> tried exactly once, no retry loop =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
cat > "$STUBS/zfs.filekey" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    get)
        prop="$5"
        case "$prop" in
            encryptionroot) echo "zroot/ROOT/alpine" ;;
            keystatus) echo "unavailable" ;;
            keylocation) echo "file:///etc/zfs/key" ;;
            org.alpinezfsboot:commandline) echo "-" ;;
        esac
        ;;
    load-key) exit 1 ;;  # the file is unreachable/wrong either way
esac
exit 0
EOF
chmod +x "$STUBS/zfs.filekey"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.filekey" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA
    . "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot"
) >"$d/out" 2>&1 || true
attempts=$(grep -c "^zfs load-key zroot/ROOT/alpine$" "$d/log" 2>/dev/null || true)
if [ "$attempts" = 1 ]; then
    ok "keylocation=file:// tried exactly once, not retried 3x with no new input"
else
    cat "$d/log" 2>/dev/null; bad "non-prompt keylocation was retried when it should only be tried once (attempts=$attempts)"
fi
rm -f "$STUBS/zfs.filekey"
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: two kernels present -> kernel+initrd always come from the SAME suffix, never cross-paired =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
# lts-old has no matching initramfs at all - regression case for the
# bug where kernel/initrd used to be picked by two INDEPENDENT
# `sort -V` calls and could silently cross-pair (kexec into a kernel
# with someone else's initramfs, a confusing panic - not just a "wrong
# kernel" mistake).
: > "$d/pooldata/boot/vmlinuz-lts-old"
: > "$d/pooldata/boot/vmlinuz-lts"
: > "$d/pooldata/boot/initramfs-lts"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" >"$d/out" 2>&1 || true
if grep -q "vmlinuz-lts-old" "$d/out"; then
    # picked the suffix with no matching initramfs - must exit 1, NOT kexec
    if grep -q "no matching kernel/initramfs" "$d/out" && ! grep -q "^kexec -l" "$d/log" 2>/dev/null; then
        ok "suffix with no matching initramfs correctly rejected, no kexec attempted"
    else
        cat "$d/out"; bad "kexec -l was called despite no matching initramfs for the picked suffix"
    fi
elif grep -q -- "-l .*vmlinuz-lts --initrd=.*initramfs-lts" "$d/log" 2>/dev/null && ! grep -q "vmlinuz-lts-old" "$d/log"; then
    ok "picked vmlinuz-lts, correctly paired with initramfs-lts (never cross-paired with lts-old)"
else
    cat "$d/out"; cat "$d/log" 2>/dev/null; bad "kernel/initrd pairing broken - got a mismatched or unexpected pair"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: explicit KERNEL_SUFFIX overrides newest-pick =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"
: > "$d/pooldata/boot/initramfs-lts"
: > "$d/pooldata/boot/vmlinuz-5.15.999"
: > "$d/pooldata/boot/initramfs-5.15.999"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" "lts" >"$d/out" 2>&1 || true
if grep -q -- "-l .*vmlinuz-lts --initrd=.*initramfs-lts" "$d/log" 2>/dev/null && ! grep -q "5.15.999" "$d/log"; then
    ok "explicit suffix 'lts' wins over the numerically-newer 5.15.999"
else
    cat "$d/out"; cat "$d/log" 2>/dev/null; bad "explicit KERNEL_SUFFIX not honoured"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: missing kernel -> falls back to a shell, does not kexec or panic pid 1 =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/empty" "zroot" >"$d/out" 2>&1 || true
# Never a bare `exit 1` any more - this script is always reached via
# exec, so exiting outright would panic a real kernel ("Attempted to
# kill init!") instead of falling back to a shell. The exit CODE is
# now whatever /bin/bash itself returns (0, on a closed/EOF stdin in
# this test) - the message and "no kexec attempted" are what matter.
if grep -q "no matching kernel/initramfs" "$d/out" && ! grep -q "^kexec" "$d/log" 2>/dev/null; then
    ok "no kernel found -> falls back to a shell, no kexec attempted"
else
    cat "$d/out"; bad "expected a clear no-kernel message with no kexec call"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: missing kernel + authorized_keys/host key staged -> fail() starts rescue ssh =="
# The second of this project's three rescue-ssh trigger points (see
# rescue-ssh.sh's own header comment) - fail() is boot-dataset.sh's own
# equivalent of /init's die(), reached here via the exact same missing-
# kernel condition as the test above, just with rescue SSH material
# staged first (boot-dataset.sh never does ESP discovery itself - at a
# real boot it's already staged by /init before the exec; standalone
# here, stage_rescue_ssh() drops the files directly).
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
stage_rescue_ssh "$d/root"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/empty" "zroot" >"$d/out" 2>&1 || true
if grep -q -- "^dropbear -r $d/root/etc/dropbear/dropbear_ed25519_host_key -F -s -g -p 22$" "$d/log" 2>/dev/null; then
    ok "fail() (missing kernel) starts rescue ssh when authorized_keys/host key are staged"
else
    cat "$d/log"; bad "fail() did not start rescue ssh"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: normal run claims the single-owner boot lock and proceeds =="
# The positive case: with no pre-existing lock, boot-dataset.sh must
# still mkdir it and proceed to a normal kexec exactly as before this
# lock existed at all. Checking kexec -l AND kexec -e both appear is
# sufficient proof the lock was actually acquired - lose_boot_race()
# returns/execs away immediately on a failed mkdir, long before either
# of those calls could ever happen, so their presence in the log is
# conclusive either way. (Not asserting the lock directory itself
# still exists afterward: this test harness's stub kexec "returns"
# instead of genuinely replacing the process the way real kexec does,
# so execution continues past `kexec -e` into this file's own trailing
# `fail "FATAL: kexec -e did not take over..."` - which correctly
# releases the lock via fail()'s own cleanup, exactly as intended for
# a real failure. That's a property of the stub, not a real double-
# free or a sign the lock logic is wrong.)
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" >"$d/out" 2>&1 || true
if grep -q "^kexec -l" "$d/log" 2>/dev/null && grep -q "^kexec -e" "$d/log" 2>/dev/null; then
    ok "boot lock claimed (mkdir succeeded) and normal boot proceeded to kexec"
else
    cat "$d/log"; bad "lock not claimed or kexec did not proceed"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: lock already held by another session -> this one backs off cleanly, no kexec =="
# Simulates the real race directly requested: two processes (the
# original local one from the encryption-wait path, and a remote
# operator's own fresh menu.py/boot-dataset.sh session) both reaching
# the "proceed to boot" decision point for the SAME dataset. `mkdir`'s
# atomicity means the code path exercised here (a pre-existing lock
# directory causing THIS process's own mkdir to fail with EEXIST) is
# byte-for-byte the same code path a genuine concurrent race would
# hit - there is no separate "was this really concurrent" branch to
# test differently. Pre-creating the lock directory before this run
# stands in for "someone else's mkdir already won".
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot" "$d/root/tmp/boot-lock.zroot_ROOT_alpine"
# A real, currently-alive pid ($$, this very test process) - as of F11
# (unidoc-alip's PR #5 follow-up review), a pid-less lock directory is
# correctly reclaimed after a brief bounded poll rather than treated as
# "someone else holds it"; this fixture wants the latter (a genuine
# lost-race backoff), so it must record a live pid, the same as a real
# concurrent mkdir winner's own zfs_op_lock()/boot_lock_acquire() would
# have done immediately after its own mkdir.
echo "$$" > "$d/root/tmp/boot-lock.zroot_ROOT_alpine/pid"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" >"$d/out" 2>&1 || true
if ! grep -q "^kexec" "$d/log" 2>/dev/null && ! grep -q "^mount" "$d/log" 2>/dev/null; then
    ok "lost the lock race -> no mount or kexec attempted at all"
else
    cat "$d/log"; bad "a losing process still attempted mount/kexec"
fi
if grep -qi "another session is already proceeding" "$d/out" && ! grep -qi "FATAL" "$d/out"; then
    ok "calm backoff message shown, not routed through fail()'s FATAL/error language"
else
    cat "$d/out"; bad "backoff message missing or wrongly alarmed"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: a session that staged the handoff secret itself, then loses the boot race, must NOT delete its own stage =="
# F2 (unidoc-alip's PR #5 follow-up review, 'smaller leftovers'): N3's
# staged_by_me gate only ever protected a DIFFERENT process's stage
# from a losing session that never staged anything itself. It does
# nothing for the case alip's own follow-up flagged as still open: THIS
# session unlocks and stages the secret (encryptionroot is checked well
# before boot_lock_acquire - see this file's own call ordering), then
# loses the boot lock race to a session that got there first and is
# already proceeding with the SAME dataset - which may well be relying
# on the exact stage this session just published, since the stage path
# is keyed by encryptionroot, not by process. Before lose_boot_race()
# cleared staged_by_me itself, its own unconditional `exit 0` ran
# _cleanup_secrets() with staged_by_me=1 still set from this session's
# own successful staging, deleting a secret a still-booting session may
# depend on - the same double-ZFS-prompt failure mode this project's
# kexec handoff exists to prevent, just reached via a different pairing
# of processes than the original bug.
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot" "$d/root/tmp/boot-lock.zroot_ROOT_alpine"
# Same fixture convention as the plain lost-race test above: a live pid
# recorded up front so F11's own empty-pid reclaim poll does not treat
# this as abandoned and steal it back before boot_lock_acquire ever
# gets a chance to genuinely lose the race.
echo "$$" > "$d/root/tmp/boot-lock.zroot_ROOT_alpine/pid"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
cat > "$STUBS/zfs.f2-lose-race" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    get)
        prop="$5"; ds="$6"
        case "$prop" in
            encryptionroot) echo "zroot/ROOT" ;;
            keystatus) [ "$ds" = "zroot/ROOT" ] && echo "unavailable" || echo "-" ;;
            keylocation) echo "prompt" ;;
            org.alpinezfsboot:commandline) echo "-" ;;
        esac
        ;;
    load-key)
        [ "$2" = "zroot/ROOT" ]
        ;;
esac
exit 0
EOF
chmod +x "$STUBS/zfs.f2-lose-race"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.f2-lose-race" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    dialog()   { echo "test-passphrase" >&2; return 0; }
    stty()     { :; }
    clear()    { :; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_TTY="$d/tty"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA STUB_TTY
    . "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot"
) >"$d/out" 2>&1 || true
key_stage="$d/root/tmp/zfs-key.zroot_ROOT"
if grep -qi "another session is already proceeding" "$d/out" && [ -s "$key_stage" ]; then
    ok "lost the boot race after staging the secret itself - the stage it created survives, not deleted out from under the winner"
else
    cat "$d/out"; ls "$d/root/tmp" 2>/dev/null
    bad "a session that staged its own secret then lost the race deleted it on exit (key_stage present=$([ -s "$key_stage" ] && echo yes || echo no))"
fi
rm -f "$STUBS/zfs.f2-lose-race"
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: fail() releases the boot lock, so a manual retry is never falsely blocked =="
# Without this, a human who reaches fail()'s own recovery shell and
# manually re-runs boot-dataset.sh for the same DATASET would be told
# "another session is already proceeding" by their own already-
# abandoned previous attempt's stale lock - a real usability trap this
# test exists specifically to catch.
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/empty" "zroot" >"$d/out" 2>&1 || true
if [ ! -d "$d/root/tmp/boot-lock.zroot_ROOT_empty" ]; then
    ok "fail() (missing kernel) released the boot lock - a manual retry would not be falsely blocked"
else
    ls "$d/root/tmp"; bad "boot lock was left behind after fail()"
fi
rm -rf "$d"

# =============================================================================
# Regression tests for a real bug found in a full source audit:
# $boot_lock_dir had NEITHER quality zfs-unlock.sh's own zfs_op_lock()
# already has (see that pair's own extensive comment, and its matching
# reclaim tests above this point in the file, which these three deliberately
# mirror) - no owner (PID) recorded at all, and no trap, so a Ctrl-C or a
# dropped SSH session mid-boot (both ordinary, explicitly-supported operator
# actions, not edge cases) bypassed fail()'s own existing cleanup entirely
# and permanently wedged that dataset's boot for the rest of the rescue
# session - recoverable only by a human finding and manually removing the
# stale directory.
#
# Real, not simulated, same reasoning as the zfs_op_lock reclaim tests: a
# genuinely SEPARATE process (a real `sh holder.sh &`, not a `(...) &`
# subshell of this script - $$ inside a subshell always reports the
# INVOKING shell's own pid, never the subshell's own real one) takes the
# real boot lock and is actually SIGKILLed. boot-dataset.sh resets PATH
# itself (`PATH=/sbin:/bin:/usr/sbin:/usr/bin`), so the holder script
# defines the same STUBS-backed shell FUNCTIONS run_stubbed uses (function
# lookup wins over PATH search regardless of PATH's own value) rather than
# relying on PATH to find the stubs. STUB_MOUNT_SLEEP holds the holder
# inside the (stubbed) `mount` call - well after boot_lock_acquire has
# already run and recorded the real pid, and before anything else in the
# script could complete first.
_boot_lock_holder_script() {
    # #!/usr/bin/env bash, NOT #!/bin/sh - boot-dataset.sh resolves its
    # own directory (RESCUE_LIB_DIR, to then source rescue-ssh.sh/
    # zfs-unlock.sh relative to itself) via \${BASH_SOURCE:-\$0}. That
    # only resolves correctly through a dot-sourcing chain under bash -
    # BASH_SOURCE tracks the actual innermost sourced file regardless of
    # nesting, while a plain \$0 (dash/ash's only option) stays fixed at
    # the outermost script's own path. run_stubbed's own dot-sourcing
    # already depends on exactly this (run-tests.sh itself is bash - see
    # its own shebang) - this holder script needs the same interpreter
    # for the same reason, or RESCUE_LIB_DIR would resolve to this
    # holder script's own directory instead of init/'s.
    cat > "$1" <<EOF
#!/usr/bin/env bash
STUB_ROOT="$2"
STUB_LOG="$3"
STUB_MOUNT_SLEEP="$4"
export STUB_ROOT STUB_LOG STUB_MOUNT_SLEEP
mount()    { "$STUBS/mount" "\$@"; }
umount()   { "$STUBS/umount" "\$@"; }
zpool()    { "$STUBS/zpool" "\$@"; }
zfs()      { "$STUBS/zfs" "\$@"; }
kexec()    { "$STUBS/kexec" "\$@"; }
modprobe() { "$STUBS/modprobe" "\$@"; }
mdev()     { "$STUBS/mdev" "\$@"; }
ifconfig() { "$STUBS/ifconfig" "\$@"; }
udhcpc()   { "$STUBS/udhcpc" "\$@"; }
udhcpc6()  { "$STUBS/udhcpc6" "\$@"; }
ip()       { "$STUBS/ip" "\$@"; }
nft()      { "$STUBS/nft" "\$@"; }
dropbear() { "$STUBS/dropbear" "\$@"; }
dropbearkey() { "$STUBS/dropbearkey" "\$@"; }
. "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot"
EOF
    chmod +x "$1"
}

echo "== boot-dataset.sh: boot_lock_acquire reclaims a lock whose holder was SIGKILLed (real, separate process, real signal) =="
d="$(fresh_env)"
_boot_lock_holder_script "$d/holder.sh" "$d/root" "$d/log" 300
"$d/holder.sh" &
holder_pid=$!
lock_dir="$d/root/tmp/boot-lock.zroot_ROOT_alpine"
i=0
while [ ! -s "$lock_dir/pid" ] && [ "$i" -lt 50 ]; do
    sleep 0.1
    i=$((i + 1))
done
recorded_pid="$(cat "$lock_dir/pid" 2>/dev/null)"
kill -KILL "$holder_pid" 2>/dev/null || true
i=0
while [ -d "/proc/$holder_pid" ] && [ "$i" -lt 50 ]; do
    sleep 0.1
    i=$((i + 1))
done
STUB_LOG="$d/log2" STUB_ROOT="$d/root" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" >"$d/out" 2>&1 || true
if grep -q "^mount" "$d/log2" 2>/dev/null \
   && [ "$recorded_pid" = "$holder_pid" ] \
   && grep -q "reclaimed a stale boot lock for zroot/ROOT/alpine (holder pid $holder_pid is gone)" "$d/out"; then
    ok "boot_lock_acquire reclaims a lock left behind by a real, SIGKILLed holder - a manual retry is no longer stuck until the next reboot"
else
    cat "$d/out" 2>/dev/null; echo "recorded_pid=$recorded_pid holder_pid=$holder_pid"; ls "$d/root/tmp" 2>/dev/null
    bad "boot_lock_acquire did not reclaim a real dead holder's lock as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: F11 - a boot lock directory with no pid file at all is reclaimed, not permanently wedged =="
# Same real gap as zfs-unlock.sh's own identical F11 fix (see that
# file's own regression test for the full reasoning): mkdir and the pid
# write below it are not atomic together, so a holder killed in that
# exact gap leaves $boot_lock_dir existing with no pid file - lock_pid
# then reads empty and the dead-pid liveness check never runs at all,
# permanently wedging every future boot of this dataset until a human
# removes the directory by hand or the machine reboots. Simulates the
# exact end state directly (a real mkdir, deliberately no pid file)
# rather than timing a real kill - boot_lock_acquire's own fix only
# ever examines the end state, not how it arose.
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
lock_dir="$d/root/tmp/boot-lock.zroot_ROOT_alpine"
mkdir -p "$lock_dir" # deliberately no pid file inside
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" >"$d/out" 2>&1 || true
if grep -qi "reclaimed an abandoned boot lock" "$d/out" \
   && [ -s "$lock_dir/pid" -o ! -e "$lock_dir" ]; then
    ok "a pid-less boot lock directory (holder killed between mkdir and recording ownership) is reclaimed after the bounded poll, not left wedged forever"
else
    cat "$d/out" 2>/dev/null; ls -la "$lock_dir" 2>/dev/null
    bad "a pid-less boot lock directory was not reclaimed - this would wedge every future boot of this dataset until a human intervenes or the machine reboots"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: boot_lock_acquire does NOT reclaim a lock whose holder is still genuinely alive =="
d="$(fresh_env)"
_boot_lock_holder_script "$d/holder.sh" "$d/root" "$d/log" 5
"$d/holder.sh" &
holder_pid=$!
lock_dir="$d/root/tmp/boot-lock.zroot_ROOT_alpine"
i=0
while [ ! -s "$lock_dir/pid" ] && [ "$i" -lt 50 ]; do
    sleep 0.1
    i=$((i + 1))
done
STUB_LOG="$d/log2" STUB_ROOT="$d/root" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" >"$d/out" 2>&1 || true
kill -KILL "$holder_pid" 2>/dev/null || true
if grep -qi "another session is already proceeding" "$d/out" && ! grep -q "^mount" "$d/log2" 2>/dev/null; then
    ok "a lock held by a genuinely live holder is correctly NOT reclaimed - the fix reclaims dead holders only, never steals a live lock"
else
    cat "$d/out" 2>/dev/null
    bad "boot_lock_acquire incorrectly reclaimed a lock from a still-alive holder"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: SIGTERM/SIGINT mid-boot releases the lock via the trap, not just fail()/normal exit =="
# The other half of the same bug: even with owner tracking, a Ctrl-C or a
# dropped SSH session bypasses fail()'s own cleanup entirely (a killed
# process runs no cleanup code without a trap) - release_boot_lock is
# registered as this script's own EXIT/INT/TERM/HUP trap specifically so
# this path is covered too, not just the ones that happen to call fail().
d="$(fresh_env)"
_boot_lock_holder_script "$d/holder.sh" "$d/root" "$d/log" 300
"$d/holder.sh" &
holder_pid=$!
lock_dir="$d/root/tmp/boot-lock.zroot_ROOT_alpine"
i=0
while [ ! -s "$lock_dir/pid" ] && [ "$i" -lt 50 ]; do
    sleep 0.1
    i=$((i + 1))
done
# SIGTERM, not SIGKILL - this test specifically proves the TRAP path
# (which only a catchable signal can exercise at all; SIGKILL cannot be
# trapped by any process, which is exactly why the separate PID-based
# reclaim tests above exist as the OTHER, independent layer).
#
# Also signals the holder's own direct child (the mount stub, itself
# blocked in `sleep $STUB_MOUNT_SLEEP`), not just $holder_pid - a real,
# confirmed bash behavior found writing this exact test: bash defers
# processing a trapped signal until its CURRENT FOREGROUND CHILD
# finishes, so a bare `kill -TERM "$holder_pid"` here would sit pending,
# unnoticed, for the full 300s STUB_MOUNT_SLEEP before the trap ever
# ran - not because the fix is wrong, but because nothing yet unblocked
# bash's own wait() on that child. A real Ctrl-C avoids this because the
# kernel delivers SIGINT to the whole foreground PROCESS GROUP at
# once (parent and child together) - but $holder_pid does NOT get its
# own process group here (job control is off in this non-interactive
# script, confirmed directly: a backgrounded job's pgid equals the
# SCRIPT's own pgid, not a fresh one - so signaling "-$holder_pid" as a
# process group would incorrectly hit run-tests.sh itself, not a
# separate group). Signaling both PIDs explicitly reproduces the same
# net effect (parent AND its blocking child interrupted together)
# without relying on process-group semantics that don't hold here.
kill -TERM "$holder_pid" 2>/dev/null || true
pkill -TERM -P "$holder_pid" 2>/dev/null || true
i=0
while [ -d "/proc/$holder_pid" ] && [ "$i" -lt 50 ]; do
    sleep 0.1
    i=$((i + 1))
done
if [ ! -d "$lock_dir" ]; then
    ok "SIGTERM mid-boot released the lock via the trap - no stale-reclaim needed, no manual cleanup needed"
else
    ls "$lock_dir" 2>/dev/null
    bad "SIGTERM mid-boot left the lock directory behind - the trap did not release it"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: SIGTERM after a passphrase is staged removes the plaintext secret too, not just the boot lock =="
# Regression test for a real gap a full source audit found: the
# EXIT/INT/TERM/HUP trap only ever released the boot lock
# (release_boot_lock) - a signal landing between zfs_unlock() staging
# the passphrase (which happens BEFORE mount - see this file's own
# call ordering) and the script's own later, normal end-of-handoff
# cleanup left the plaintext secret sitting in tmpfs indefinitely,
# something only fail()'s own explicit paths used to clean up. Same
# real-separate-process-plus-real-SIGTERM technique as the test above,
# but this holder is a genuinely ENCRYPTED dataset (keylocation=prompt,
# a scripted correct passphrase via a stubbed dialog) blocked in the
# stubbed `mount` call - well after zfs_unlock() has already staged
# the real secret - so there is a genuine plaintext file on disk for
# the trap to either clean up or leak.
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
cat > "$STUBS/zfs.sigterm-secret" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    get)
        prop="$5"; ds="$6"
        case "$prop" in
            encryptionroot) echo "zroot/ROOT" ;;
            keystatus) [ "$ds" = "zroot/ROOT" ] && echo "unavailable" || echo "-" ;;
            keylocation) echo "prompt" ;;
            org.alpinezfsboot:commandline) echo "-" ;;
        esac
        ;;
    load-key)
        [ "$2" = "zroot/ROOT" ]
        ;;
esac
exit 0
EOF
chmod +x "$STUBS/zfs.sigterm-secret"
cat > "$d/holder.sh" <<EOF
#!/usr/bin/env bash
STUB_ROOT="$d/root"
STUB_LOG="$d/log"
STUB_MOUNT_SLEEP="300"
STUB_TTY="$d/tty"
export STUB_ROOT STUB_LOG STUB_MOUNT_SLEEP STUB_TTY
mount()    { "$STUBS/mount" "\$@"; }
umount()   { "$STUBS/umount" "\$@"; }
zpool()    { "$STUBS/zpool" "\$@"; }
zfs()      { "$STUBS/zfs.sigterm-secret" "\$@"; }
kexec()    { "$STUBS/kexec" "\$@"; }
modprobe() { "$STUBS/modprobe" "\$@"; }
mdev()     { "$STUBS/mdev" "\$@"; }
ifconfig() { "$STUBS/ifconfig" "\$@"; }
udhcpc()   { "$STUBS/udhcpc" "\$@"; }
udhcpc6()  { "$STUBS/udhcpc6" "\$@"; }
ip()       { "$STUBS/ip" "\$@"; }
nft()      { "$STUBS/nft" "\$@"; }
dropbear() { "$STUBS/dropbear" "\$@"; }
dropbearkey() { "$STUBS/dropbearkey" "\$@"; }
dialog()   { echo "test-passphrase" >&2; return 0; }
stty()     { :; }
clear()    { :; }
. "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot"
EOF
chmod +x "$d/holder.sh"
"$d/holder.sh" &
holder_pid=$!
key_stage="$d/root/tmp/zfs-key.zroot_ROOT"
i=0
while [ ! -s "$key_stage" ] && [ "$i" -lt 50 ]; do
    sleep 0.1
    i=$((i + 1))
done
staged_before="$([ -s "$key_stage" ] && echo yes || echo no)"
# Same reasoning as the plain SIGTERM test above: signal both the
# holder and its direct blocking child (the mount stub's own sleep),
# not just $holder_pid.
kill -TERM "$holder_pid" 2>/dev/null || true
pkill -TERM -P "$holder_pid" 2>/dev/null || true
i=0
while [ -d "/proc/$holder_pid" ] && [ "$i" -lt 50 ]; do
    sleep 0.1
    i=$((i + 1))
done
if [ "$staged_before" = yes ] && [ ! -e "$key_stage" ]; then
    ok "SIGTERM after staging removed the plaintext passphrase too - the trap covers secrets, not just the boot lock"
else
    echo "staged_before=$staged_before"; ls "$d/root/tmp" 2>/dev/null
    bad "SIGTERM after staging left the plaintext passphrase behind in tmpfs (staged_before=$staged_before)"
fi
rm -f "$STUBS/zfs.sigterm-secret"
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: SIGTERM during zfs load-key releases the per-encryptionroot zfs_op_lock too, not just the boot lock or a staged secret =="
# F3 (unidoc-alip's PR #5 follow-up review), the boot-dataset.sh half:
# this file calls zfs_unlock() in-process (see this file's own header
# comment), which holds zfs_op_lock across its own `zfs load-key` call -
# a DIFFERENT lock from $boot_lock_dir (covered by the test above) and
# a window that closes BEFORE the plaintext secret ever gets staged
# (covered by the test above THAT one) - neither existing test actually
# signals inside this specific window. `load-key` blocks via a short-
# poll loop here, not a single long sleep - see the standalone
# zfs-unlock wrapper's own equivalent test for why (a bash foreground
# wait() on one long sleep does not get interrupted promptly by a
# trapped signal; this project's existing SIGTERM-mid-boot test above
# documents the same lesson for pkill's own -P usage).
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
cat > "$STUBS/zfs.sigterm-oplock" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    get)
        prop="$5"; ds="$6"
        case "$prop" in
            encryptionroot) echo "zroot/ROOT" ;;
            keystatus) [ "$ds" = "zroot/ROOT" ] && echo "unavailable" || echo "-" ;;
            keylocation) echo "prompt" ;;
            org.alpinezfsboot:commandline) echo "-" ;;
        esac
        ;;
    load-key)
        while :; do sleep 1; done
        ;;
esac
exit 0
EOF
chmod +x "$STUBS/zfs.sigterm-oplock"
cat > "$d/holder.sh" <<EOF
#!/usr/bin/env bash
STUB_ROOT="$d/root"
STUB_LOG="$d/log"
STUB_TTY="$d/tty"
export STUB_ROOT STUB_LOG STUB_TTY
mount()    { "$STUBS/mount" "\$@"; }
umount()   { "$STUBS/umount" "\$@"; }
zpool()    { "$STUBS/zpool" "\$@"; }
zfs()      { "$STUBS/zfs.sigterm-oplock" "\$@"; }
kexec()    { "$STUBS/kexec" "\$@"; }
modprobe() { "$STUBS/modprobe" "\$@"; }
mdev()     { "$STUBS/mdev" "\$@"; }
ifconfig() { "$STUBS/ifconfig" "\$@"; }
udhcpc()   { "$STUBS/udhcpc" "\$@"; }
udhcpc6()  { "$STUBS/udhcpc6" "\$@"; }
ip()       { "$STUBS/ip" "\$@"; }
nft()      { "$STUBS/nft" "\$@"; }
dropbear() { "$STUBS/dropbear" "\$@"; }
dropbearkey() { "$STUBS/dropbearkey" "\$@"; }
dialog()   { echo "test-passphrase" >&2; return 0; }
stty()     { :; }
clear()    { :; }
. "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot"
EOF
chmod +x "$d/holder.sh"
"$d/holder.sh" &
holder_pid=$!
op_lock_dir="$d/root/tmp/zfs-key-lock.zroot_ROOT"
i=0
while [ ! -s "$op_lock_dir/pid" ] && [ "$i" -lt 50 ]; do
    sleep 0.1
    i=$((i + 1))
done
lock_seen="$([ -s "$op_lock_dir/pid" ] && echo yes || echo no)"
kill -TERM "$holder_pid" 2>/dev/null || true
pkill -TERM -P "$holder_pid" 2>/dev/null || true
i=0
while [ -d "/proc/$holder_pid" ] && [ "$i" -lt 50 ]; do
    sleep 0.1
    i=$((i + 1))
done
if [ "$lock_seen" = yes ] && [ ! -e "$op_lock_dir" ]; then
    ok "SIGTERM during zfs load-key released the per-encryptionroot zfs_op_lock via boot-dataset.sh's own trap"
else
    echo "lock_seen=$lock_seen"; ls "$op_lock_dir" 2>/dev/null
    bad "SIGTERM during zfs load-key left the per-encryptionroot zfs_op_lock directory behind (lock_seen=$lock_seen)"
fi
rm -f "$STUBS/zfs.sigterm-oplock"
rm -rf "$d"

# =============================================================================
echo "== init: no authorized_keys/host key staged -> dropbear never started =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0\n' > "$d/root/proc/cmdline"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="zroot/ROOT/alpine" \
    run_stubbed "$REPO_ROOT/init/init" >"$d/out" 2>&1 || true
if ! grep -q "^dropbear " "$d/log" 2>/dev/null; then
    ok "dropbear not invoked when no rescue SSH material is staged"
else
    cat "$d/log"; bad "dropbear was invoked despite no authorized_keys/host key"
fi
rm -rf "$d"

# =============================================================================
echo "== init: authorized_keys/host key staged but boot is healthy -> dropbear NEVER started =="
# The real behavior change this project's author explicitly asked for:
# rescue material being CONFIGURED for break-glass use is no longer the
# same thing as dropbear actually LISTENING - see rescue-ssh.sh's own
# header comment. A healthy boot (valid bootfs, matching kernel found)
# never calls die()/fail(), so start_rescue_ssh is never even invoked
# here, regardless of authorized_keys/host key being staged.
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
stage_rescue_ssh "$d/root"
printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0\n' > "$d/root/proc/cmdline"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="zroot/ROOT/alpine" \
    run_stubbed "$REPO_ROOT/init/init" >"$d/out" 2>&1 || true
if ! grep -q "^dropbear " "$d/log" 2>/dev/null; then
    ok "dropbear never started on a healthy boot, even with rescue SSH material staged"
else
    cat "$d/log"; bad "dropbear was invoked on a healthy boot"
fi
rm -rf "$d"

# =============================================================================
echo "== init: zpool import itself failing (not just a missing bootfs) starts rescue ssh proactively, before menu.py even runs =="
# The actual confirmed gap: `menu.py` itself renders the
# POOL_IMPORT_ERROR state (skips the countdown, opens on "Boot log")
# and, on a real headless machine, just sits there blocked on
# interactive input that can never come over a console nobody can
# reach - it never exits, never crashes, and NEVER calls
# start_rescue_ssh anywhere in its own code (confirmed by reading it -
# grep menu.py for start_rescue_ssh finds nothing). die()'s own
# fallback trigger is reached ONLY if menu.py ALSO fails to even run at
# all - on real hardware, the ordinary "pool import failed, waiting for
# an operator with no console" case never reaches that fallback either.
# Fixed by calling start_rescue_ssh proactively the moment
# POOL_IMPORT_ERROR is known, in /init itself, before menu.py is ever
# launched - this test's own python3 fails fast against a nonexistent
# /menu.py (same as every other test here, no real terminal), so it
# can't cleanly distinguish "the new proactive call fired" from "die()'s
# own fallback also would have" in THIS harness - but it does add real,
# previously-missing coverage for the "zpool import itself fails"
# sub-case specifically (only "no bootfs property" was ever tested
# before today).
d="$(fresh_env)"
cat > "$STUBS/zpool.importfail" <<'EOF'
#!/bin/sh
echo "zpool $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    import) exit 1 ;;
    get) echo "${STUB_BOOTFS:--}" ;;
    status) echo "${STUB_POOL_HEALTH:-pool is healthy}" ;;
esac
exit 0
EOF
chmod +x "$STUBS/zpool.importfail"
stage_rescue_ssh "$d/root"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool.importfail" "$@"; }
    zfs()      { "$STUBS/zfs" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    modprobe() { "$STUBS/modprobe" "$@"; }
    mdev()     { "$STUBS/mdev" "$@"; }
    ifconfig() { "$STUBS/ifconfig" "$@"; }
    udhcpc()   { "$STUBS/udhcpc" "$@"; }
    ip()       { "$STUBS/ip" "$@"; }
    dropbear() { "$STUBS/dropbear" "$@"; }
    dropbearkey() { "$STUBS/dropbearkey" "$@"; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA
    printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0\n' > "$d/root/proc/cmdline"
    . "$REPO_ROOT/init/init"
) >"$d/out" 2>&1 || true
if grep -q "^zpool import -f -N -d /dev zroot$" "$d/log" 2>/dev/null \
   && grep -q -- "^dropbear -r $d/root/etc/dropbear/dropbear_ed25519_host_key -F -s -g -p 22$" "$d/log"; then
    ok "zpool import itself failing still starts rescue ssh (dropbear), not just the 'no bootfs' sub-case"
else
    cat "$d/log" 2>/dev/null; cat "$d/out"; bad "a genuine zpool import failure did not start rescue ssh"
fi
rm -f "$STUBS/zpool.importfail"
rm -rf "$d"

# =============================================================================
echo "== init: bootcheck - armed:K below the threshold -> auto-boot proceeds, not forced rescue =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0\n' > "$d/root/proc/cmdline"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.bootcheck" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    modprobe() { "$STUBS/modprobe" "$@"; }
    mdev()     { "$STUBS/mdev" "$@"; }
    ifconfig() { "$STUBS/ifconfig" "$@"; }
    udhcpc()   { "$STUBS/udhcpc" "$@"; }
    ip()       { "$STUBS/ip" "$@"; }
    dropbear() { "$STUBS/dropbear" "$@"; }
    dropbearkey() { "$STUBS/dropbearkey" "$@"; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="zroot/ROOT/alpine" \
        STUB_BOOTCHECK_VALUE="armed:1" STUB_BOOTCHECK_SOURCE="local"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA STUB_BOOTFS STUB_BOOTCHECK_VALUE STUB_BOOTCHECK_SOURCE
    . "$REPO_ROOT/init/init"
) >"$d/out" 2>&1 || true
if grep -qi "1 consecutive.*auto-booting anyway" "$d/out" 2>/dev/null \
   && grep -q "/boot-dataset.sh: No such file or directory" "$d/out" \
   && ! grep -q "^dropbear " "$d/log" 2>/dev/null; then
    ok "armed:1 (below the default threshold of 3) does not trigger forced rescue - the auto-boot path is still attempted"
else
    cat "$d/out"; bad "a below-threshold attempt count incorrectly blocked auto-boot"
fi
rm -rf "$d"

# =============================================================================
echo "== init: bootcheck - armed:K at/above the threshold, rescue SSH actually starts -> forced rescue, no auto-boot =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
stage_rescue_ssh "$d/root"
printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0\n' > "$d/root/proc/cmdline"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.bootcheck" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    modprobe() { "$STUBS/modprobe" "$@"; }
    mdev()     { "$STUBS/mdev" "$@"; }
    ifconfig() { "$STUBS/ifconfig" "$@"; }
    udhcpc()   { "$STUBS/udhcpc" "$@"; }
    ip()       { "$STUBS/ip" "$@"; }
    dropbear() { "$STUBS/dropbear" "$@"; }
    dropbearkey() { "$STUBS/dropbearkey" "$@"; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="zroot/ROOT/alpine" \
        STUB_BOOTCHECK_VALUE="armed:3" STUB_BOOTCHECK_SOURCE="local"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA STUB_BOOTFS STUB_BOOTCHECK_VALUE STUB_BOOTCHECK_SOURCE
    . "$REPO_ROOT/init/init"
) >"$d/out" 2>&1 || true
if grep -q -- "^dropbear -r $d/root/etc/dropbear/dropbear_ed25519_host_key -F -s -g -p 22$" "$d/log" 2>/dev/null \
   && grep -qi "FATAL: zroot/ROOT/alpine has 3 consecutive" "$d/out"; then
    ok "armed:3 (at the default threshold) with reachable rescue SSH forces rescue, auto-boot suppressed"
else
    cat "$d/log" 2>/dev/null; cat "$d/out"; bad "at-threshold attempt count with working rescue SSH did not force rescue"
fi
rm -rf "$d"

# =============================================================================
echo "== init: bootcheck - threshold hit, rescue SSH fails to actually start, NO force -> auto-boot anyway =="
# The gate-on-actual-success blocker: deciding "authorized_keys/host key
# staged, therefore safe to stop auto-booting" would strand exactly the
# headless machine this feature exists to protect. Forces
# start_rescue_ssh to genuinely fail (a malformed ssh.allow= CIDR,
# default-deny) despite real rescue SSH material being staged.
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
stage_rescue_ssh "$d/root"
printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0 alpine-zfsboot.ssh.allow=not-a-cidr\n' > "$d/root/proc/cmdline"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.bootcheck" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    modprobe() { "$STUBS/modprobe" "$@"; }
    mdev()     { "$STUBS/mdev" "$@"; }
    ifconfig() { "$STUBS/ifconfig" "$@"; }
    udhcpc()   { "$STUBS/udhcpc" "$@"; }
    ip()       { "$STUBS/ip" "$@"; }
    dropbear() { "$STUBS/dropbear" "$@"; }
    dropbearkey() { "$STUBS/dropbearkey" "$@"; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="zroot/ROOT/alpine" \
        STUB_BOOTCHECK_VALUE="armed:3" STUB_BOOTCHECK_SOURCE="local"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA STUB_BOOTFS STUB_BOOTCHECK_VALUE STUB_BOOTCHECK_SOURCE
    . "$REPO_ROOT/init/init"
) >"$d/out" 2>&1 || true
if ! grep -q "^dropbear " "$d/log" 2>/dev/null && grep -q "/boot-dataset.sh: No such file or directory" "$d/out"; then
    ok "rescue SSH failing to start (no force) does NOT block auto-boot - never stranding a headless machine unreachably"
else
    cat "$d/log" 2>/dev/null; cat "$d/out"; bad "a failed rescue-SSH start incorrectly blocked auto-boot with no force override"
fi
rm -rf "$d"

# =============================================================================
echo "== init: bootcheck - threshold hit, rescue SSH fails, bootcheck=force -> rescue enforced anyway =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
stage_rescue_ssh "$d/root"
printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0 alpine-zfsboot.ssh.allow=not-a-cidr alpine-zfsboot.bootcheck=force\n' > "$d/root/proc/cmdline"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.bootcheck" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    modprobe() { "$STUBS/modprobe" "$@"; }
    mdev()     { "$STUBS/mdev" "$@"; }
    ifconfig() { "$STUBS/ifconfig" "$@"; }
    udhcpc()   { "$STUBS/udhcpc" "$@"; }
    ip()       { "$STUBS/ip" "$@"; }
    dropbear() { "$STUBS/dropbear" "$@"; }
    dropbearkey() { "$STUBS/dropbearkey" "$@"; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="zroot/ROOT/alpine" \
        STUB_BOOTCHECK_VALUE="armed:3" STUB_BOOTCHECK_SOURCE="local"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA STUB_BOOTFS STUB_BOOTCHECK_VALUE STUB_BOOTCHECK_SOURCE
    . "$REPO_ROOT/init/init"
) >"$d/out" 2>&1 || true
if ! grep -q "^dropbear " "$d/log" 2>/dev/null && grep -qi "FATAL:.*rescue SSH unreachable, forced anyway" "$d/out"; then
    ok "alpine-zfsboot.bootcheck=force keeps rescue enforced even though rescue SSH itself never actually started"
else
    cat "$d/log" 2>/dev/null; cat "$d/out"; bad "bootcheck=force did not override a failed rescue-SSH start as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== init: bootcheck.max= validation - out-of-range value falls back to the default (3), not silently coerced =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0 alpine-zfsboot.bootcheck.max=999\n' > "$d/root/proc/cmdline"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.bootcheck" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    modprobe() { "$STUBS/modprobe" "$@"; }
    mdev()     { "$STUBS/mdev" "$@"; }
    ifconfig() { "$STUBS/ifconfig" "$@"; }
    udhcpc()   { "$STUBS/udhcpc" "$@"; }
    ip()       { "$STUBS/ip" "$@"; }
    dropbear() { "$STUBS/dropbear" "$@"; }
    dropbearkey() { "$STUBS/dropbearkey" "$@"; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="zroot/ROOT/alpine" \
        STUB_BOOTCHECK_VALUE="armed:3" STUB_BOOTCHECK_SOURCE="local"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA STUB_BOOTFS STUB_BOOTCHECK_VALUE STUB_BOOTCHECK_SOURCE
    . "$REPO_ROOT/init/init"
) >"$d/out" 2>&1 || true
if grep -qi "out of range (1-99), using the default (3)" "$d/out" 2>/dev/null \
   && grep -q "/boot-dataset.sh: No such file or directory" "$d/out"; then
    ok "bootcheck.max=999 is rejected as out of range and falls back to the default (3) rather than being silently honored"
else
    cat "$d/out"; bad "out-of-range bootcheck.max= was not rejected/fallen-back as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== init: die() with authorized_keys/host key staged -> rescue ssh starts, hardened, key installed, passwd rewritten =="
# STUB_BOOTFS="-" is this suite's own existing way to force a real
# die() call (see the "pool with no bootfs set" test above/below) -
# menu.py exits/crashes under test (no real terminal), falls through
# to /init's own bottom-of-file fallback, which calls die() because
# ALPINE_ZFSBOOT_POOL_IMPORT_ERROR is set. This is one of the three
# real rescue-ssh trigger points, exercised for real here, not assumed.
d="$(fresh_env)"
stage_rescue_ssh "$d/root"
printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0\n' > "$d/root/proc/cmdline"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="-" \
    run_stubbed "$REPO_ROOT/init/init" >"$d/out" 2>&1 || true
if grep -q -- "^dropbear -r $d/root/etc/dropbear/dropbear_ed25519_host_key -F -s -g -p 22$" "$d/log" 2>/dev/null; then
    ok "dropbear -r <persistent host key> -F -s -g -p 22 invoked (-s -g: password auth disabled, not an accidental omission; -r, not -R: no ephemeral host key)"
else
    cat "$d/log"; bad "dropbear not invoked with expected hardened flags"
fi
if grep -q "no-port-forwarding,no-agent-forwarding,no-X11-forwarding ssh-ed25519 AAAAFAKEKEY testuser@laptop" "$d/root/root/.ssh/authorized_keys" 2>/dev/null; then
    ok "authorized_keys has the staged key with the hardened option set injected and NO command= (recv must stay usable)"
else
    cat "$d/root/root/.ssh/authorized_keys" 2>/dev/null || echo "(no authorized_keys written)"
    bad "authorized_keys missing/wrong"
fi
if grep -q "^root:x:0:0:root:/root:/alpine-zfsboot-shell$" "$d/root/etc/passwd"; then
    ok "root's shell rewritten to /alpine-zfsboot-shell in /etc/passwd"
else
    cat "$d/root/etc/passwd"; bad "root's shell not rewritten"
fi
if grep -q "^ip link set eth0 up" "$d/log" 2>/dev/null; then
    ok "network brought up (default auto/dhcp) before dropbear started"
else
    cat "$d/log"; bad "network was not brought up before dropbear"
fi
rm -rf "$d"

# =============================================================================
echo "== rescue-ssh.sh: multiple authorized_keys lines each get the hardened option set injected independently =="
# Ordinary OpenSSH authorized_keys semantics - the whole point of
# replacing the old single-base64-key mechanism - so more than one
# operator key must survive, each hardened the same mandatory way.
d="$(fresh_env)"
mkdir -p "$d/root/tmp/alpine-zfsboot"
printf 'ssh-ed25519 AAAAFIRSTKEY alice@laptop\nssh-ed25519 AAAASECONDKEY bob@desktop\n' > "$d/root/tmp/alpine-zfsboot/authorized_keys"
printf 'FAKE-DROPBEAR-ED25519-HOST-KEY-BYTES\n' > "$d/root/tmp/alpine-zfsboot/ssh_host_ed25519_key"
printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0\n' > "$d/root/proc/cmdline"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="-" \
    run_stubbed "$REPO_ROOT/init/init" >"$d/out" 2>&1 || true
if grep -q "^no-port-forwarding,no-agent-forwarding,no-X11-forwarding ssh-ed25519 AAAAFIRSTKEY alice@laptop$" "$d/root/root/.ssh/authorized_keys" 2>/dev/null \
   && grep -q "^no-port-forwarding,no-agent-forwarding,no-X11-forwarding ssh-ed25519 AAAASECONDKEY bob@desktop$" "$d/root/root/.ssh/authorized_keys" 2>/dev/null; then
    ok "both authorized_keys lines survive, each independently prefixed with the hardened option set"
else
    cat "$d/root/root/.ssh/authorized_keys" 2>/dev/null; bad "multiple authorized_keys lines were not both hardened and installed"
fi
rm -rf "$d"

# =============================================================================
echo "== rescue-ssh.sh: dropbear backgrounds successfully but exits immediately -> start_rescue_ssh reports failure, not success =="
# A backgrounded process starting (fork succeeding) says nothing about
# whether it then immediately exited on its own (bad bind_spec, port
# already in use, a malformed generated host key) - start_rescue_ssh
# must actually check, not just assume success because `&` didn't
# error. This matters for real: bootcheck's forced-rescue gate (/init)
# decides whether to keep auto-booting a headless machine specifically
# based on start_rescue_ssh's return value.
d="$(fresh_env)"
stage_rescue_ssh "$d/root"
printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0\n' > "$d/root/proc/cmdline"
STUB_DROPBEAR_DIES_IMMEDIATELY=1 STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="-" \
    run_stubbed "$REPO_ROOT/init/init" >"$d/out" 2>&1 || true
if grep -q -- "^dropbear -r $d/root/etc/dropbear/dropbear_ed25519_host_key -F -s -g -p 22$" "$d/log" 2>/dev/null \
   && grep -qi "dropbear sshd exited immediately after starting" "$d/out" 2>/dev/null; then
    ok "dropbear exiting immediately after backgrounding is detected and reported, not silently treated as success"
else
    cat "$d/log" 2>/dev/null; cat "$d/out"; bad "dropbear exiting immediately was not detected"
fi
rm -rf "$d"

# =============================================================================
echo "== rescue-ssh.sh: dropbear exits immediately with ssh.allow= set -> the nftables allowlist is torn back down =="
# Otherwise the drop rule for this port stays installed with nothing
# behind it to ever answer (silently unreachable even after a LATER
# successful start_rescue_ssh retry, whose own \`nft add rule\` would
# just stack a duplicate accept/drop pair onto the same table).
d="$(fresh_env)"
stage_rescue_ssh "$d/root"
printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0 alpine-zfsboot.ssh.allow=203.0.113.5/32\n' > "$d/root/proc/cmdline"
STUB_DROPBEAR_DIES_IMMEDIATELY=1 STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="-" \
    run_stubbed "$REPO_ROOT/init/init" >"$d/out" 2>&1 || true
if grep -q "^nft add rule inet alpinezfsboot_rescue input tcp dport 22 drop" "$d/log" 2>/dev/null \
   && grep -q "^nft delete table inet alpinezfsboot_rescue" "$d/log" 2>/dev/null; then
    ok "the nftables allowlist applied before dropbear died is torn back down, not left stranded"
else
    cat "$d/log" 2>/dev/null; bad "nftables allowlist was not cleaned up after dropbear died immediately"
fi
rm -rf "$d"

# =============================================================================
echo "== init: die() with authorized_keys staged but NO persistent host key -> rescue ssh refuses, no ephemeral fallback =="
# The whole point of the persistent-identity redesign: a machine with
# no ssh_host_ed25519_key on its ESP (never installed with the new
# installer, or the file was lost) must NOT silently fall back to a
# fresh in-memory host key the way the old -R behavior did - that
# defeats stable host-key verification exactly when it matters (a
# real, headless recovery, not a lab test).
d="$(fresh_env)"
mkdir -p "$d/root/tmp/alpine-zfsboot"
printf 'ssh-ed25519 AAAAFAKEKEY testuser@laptop\n' > "$d/root/tmp/alpine-zfsboot/authorized_keys"
printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0\n' > "$d/root/proc/cmdline"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="-" \
    run_stubbed "$REPO_ROOT/init/init" >"$d/out" 2>&1 || true
if ! grep -q "^dropbear " "$d/log" 2>/dev/null && grep -qi "no persistent ssh_host_ed25519_key was found" "$d/out"; then
    ok "authorized_keys without a persistent host key refuses to start rescue SSH rather than generating a throwaway identity"
else
    cat "$d/out"; bad "missing host key was not handled as an explicit refusal"
fi
rm -rf "$d"

# =============================================================================
echo "== init: die() with a persistent host key that fails to validate -> rescue ssh refuses, no ephemeral fallback =="
d="$(fresh_env)"
stage_rescue_ssh "$d/root"
printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0\n' > "$d/root/proc/cmdline"
STUB_DROPBEARKEY_FAIL=1 STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="-" \
    run_stubbed "$REPO_ROOT/init/init" >"$d/out" 2>&1 || true
if ! grep -q "^dropbear -r" "$d/log" 2>/dev/null && grep -qi "failed to validate" "$d/out"; then
    ok "a host key that fails dropbearkey -y validation refuses to start rescue SSH rather than falling back to an ephemeral key"
else
    cat "$d/out"; bad "an invalid persistent host key was not handled as an explicit refusal"
fi
rm -rf "$d"

# =============================================================================
echo "== init: die() with no authorized_keys staged -> rescue ssh not attempted at all =="
d="$(fresh_env)"
printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0\n' > "$d/root/proc/cmdline"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="-" \
    run_stubbed "$REPO_ROOT/init/init" >"$d/out" 2>&1 || true
if ! grep -q "^dropbear " "$d/log" 2>/dev/null && grep -qi "cannot start rescue SSH" "$d/out"; then
    ok "no authorized_keys staged -> die() still tries start_rescue_ssh, which no-ops cleanly"
else
    cat "$d/out"; bad "unexpected rescue-ssh behavior with no material staged"
fi
rm -rf "$d"

# =============================================================================
echo "== init: alpine-zfsboot.ssh.allow= with malformed CIDR -> refuses to start (default-deny) =="
d="$(fresh_env)"
stage_rescue_ssh "$d/root"
printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0 alpine-zfsboot.ssh.allow=not-a-cidr\n' > "$d/root/proc/cmdline"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="-" \
    run_stubbed "$REPO_ROOT/init/init" >"$d/out" 2>&1 || true
if ! grep -q "^dropbear " "$d/log" 2>/dev/null && grep -qi "malformed ALPINE_ZFSBOOT_SSH_ALLOW" "$d/out"; then
    ok "malformed ssh.allow= refuses to start rescue ssh rather than falling open"
else
    cat "$d/out"; bad "malformed ssh.allow= was not handled as default-deny"
fi
rm -rf "$d"

# =============================================================================
echo "== init: alpine-zfsboot.ssh.allow= with a valid CIDR -> nftables rule applied, dropbear starts =="
d="$(fresh_env)"
stage_rescue_ssh "$d/root"
printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0 alpine-zfsboot.ssh.allow=203.0.113.5/32\n' > "$d/root/proc/cmdline"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="-" \
    run_stubbed "$REPO_ROOT/init/init" >"$d/out" 2>&1 || true
if grep -q "^nft add rule inet alpinezfsboot_rescue input tcp dport 22 ip saddr 203.0.113.5/32 accept" "$d/log" 2>/dev/null &&
   grep -q -- "^dropbear -r $d/root/etc/dropbear/dropbear_ed25519_host_key -F -s -g -p 22$" "$d/log" 2>/dev/null; then
    ok "valid ssh.allow= applies a real nft rule before dropbear starts"
else
    cat "$d/log"; bad "ssh.allow= did not apply the expected nft rule"
fi
rm -rf "$d"

# =============================================================================
echo "== init: alpine-zfsboot.ssh.listen=ipv6 -> dropbear bound IPv6-only, not dual-stack =="
d="$(fresh_env)"
stage_rescue_ssh "$d/root"
printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0 alpine-zfsboot.ssh.listen=ipv6 alpine-zfsboot.ssh.port=2222\n' > "$d/root/proc/cmdline"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="-" \
    run_stubbed "$REPO_ROOT/init/init" >"$d/out" 2>&1 || true
if grep -q -- "^dropbear -r $d/root/etc/dropbear/dropbear_ed25519_host_key -F -s -g -p \[::\]:2222$" "$d/log" 2>/dev/null; then
    ok "ssh.listen=ipv6 + ssh.port=2222 bind dropbear to [::]:2222, not a dual-stack default"
else
    cat "$d/log"; bad "ssh.listen=ipv6/ssh.port= did not produce the expected bind spec"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: no CMDLINE_OVERRIDE -> persisted /etc/alpine-zfsboot-cmdline is used =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot" "$d/pooldata/etc"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
printf 'persisted=1\n' > "$d/pooldata/etc/alpine-zfsboot-cmdline"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" >"$d/out" 2>&1 || true
if grep -q -- "--command-line=root=ZFS=zroot/ROOT/alpine ro console=tty0 panic=10 persisted=1" "$d/log" 2>/dev/null; then
    ok "persisted /etc/alpine-zfsboot-cmdline used when no override given"
else
    cat "$d/log" 2>/dev/null; bad "persisted cmdline not applied"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: org.alpinezfsboot:commandline ZFS property wins over the persisted file =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot" "$d/pooldata/etc"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
printf 'from-file=1\n' > "$d/pooldata/etc/alpine-zfsboot-cmdline"
cat > "$STUBS/zfs.withprop" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    get) echo "from-property=1" ;;
esac
exit 0
EOF
chmod +x "$STUBS/zfs.withprop"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.withprop" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA
    . "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot"
) >"$d/out" 2>&1 || true
if grep -q -- "--command-line=root=ZFS=zroot/ROOT/alpine ro console=tty0 panic=10 from-property=1" "$d/log" 2>/dev/null \
   && ! grep -q "from-file=1" "$d/log"; then
    ok "ZFS property value used, persisted file ignored when the property is set"
else
    cat "$d/log" 2>/dev/null; bad "property-based cmdline did not take priority over the file as expected"
fi
rm -f "$STUBS/zfs.withprop"
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: explicit CMDLINE_OVERRIDE replaces the persisted file, not appended =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot" "$d/pooldata/etc"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
printf 'persisted=1\n' > "$d/pooldata/etc/alpine-zfsboot-cmdline"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" "" "oneshot=1" >"$d/out" 2>&1 || true
if grep -q -- "--command-line=root=ZFS=zroot/ROOT/alpine ro console=tty0 panic=10 oneshot=1" "$d/log" 2>/dev/null \
   && ! grep -q "persisted=1" "$d/log"; then
    ok "one-shot CMDLINE_OVERRIDE used instead of the persisted file, not appended to it"
else
    cat "$d/log" 2>/dev/null; bad "CMDLINE_OVERRIDE did not replace the persisted cmdline as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: passes through the active console, unless one is already persisted =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
# ALPINE_ZFSBOOT_ACTIVE_TTY=ttyS0 - confirmed missing the hard way on real
# hardware: with nothing here ever adding a console= at all, the
# target kernel fell back to its own default console, which wasn't
# necessarily the one the operator was actually watching (a real boot
# showed no output at all past the kexec message, right after the
# rescue environment's own console worked fine on that exact tty).
ALPINE_ZFSBOOT_ACTIVE_TTY=ttyS0 STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" >"$d/out" 2>&1 || true
if grep -q -- "--command-line=root=ZFS=zroot/ROOT/alpine ro console=ttyS0,115200n8" "$d/log" 2>/dev/null; then
    ok "passes through ALPINE_ZFSBOOT_ACTIVE_TTY as the target kernel's own console="
else
    cat "$d/log" 2>/dev/null; bad "did not pass through the active console as expected"
fi
rm -rf "$d"
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
ALPINE_ZFSBOOT_ACTIVE_TTY=ttyS0 STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" "" "console=tty0 explicit=1" >"$d/out" 2>&1 || true
if grep -q -- "--command-line=root=ZFS=zroot/ROOT/alpine ro panic=10 console=tty0 explicit=1" "$d/log" 2>/dev/null; then
    ok "an explicit console= in the cmdline wins over the active-tty passthrough"
else
    cat "$d/log" 2>/dev/null; bad "explicit console= did not take priority as expected"
fi
rm -rf "$d"
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
# ttyS1 - confirmed a real need on physical OVH hardware, whose remote-
# console redirection doesn't always land on ttyS0 (see /init's own
# select_console() comment).
ALPINE_ZFSBOOT_ACTIVE_TTY=ttyS1 STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" >"$d/out" 2>&1 || true
if grep -q -- "--command-line=root=ZFS=zroot/ROOT/alpine ro console=ttyS1,115200n8" "$d/log" 2>/dev/null; then
    ok "passes through ALPINE_ZFSBOOT_ACTIVE_TTY=ttyS1 the same as ttyS0"
else
    cat "$d/log" 2>/dev/null; bad "did not pass through ttyS1 as the active console as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: bootcheck - armed:K + confirm service present -> increments after a successful kexec -l =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot" "$d/pooldata/etc/runlevels/default" "$d/pooldata/etc/init.d"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
# The real OpenRC registration symlink the confirm service creates at
# install time - boot-dataset.sh checks for exactly this path on the
# BE's own mounted filesystem before ever incrementing (see that
# block's own "content-derived, not just property-derived" comment).
# A REAL symlink, not a plain file: `rc-update add` creates an ABSOLUTE
# symlink, and an earlier version of this exact test used `: > path`
# instead - a plain regular file - which let a real `[ -f ]`-dereferences-
# symlinks-against-the-wrong-root bug through undetected (the check was
# always false on real hardware, silently disabling the whole feature).
ln -s /etc/init.d/alpine-zfsboot-bootcheck "$d/pooldata/etc/runlevels/default/alpine-zfsboot-bootcheck"
# ... and the symlink's own real target must actually exist too (a
# dangling runlevel symlink is the same "will never actually run" case
# - see the next test below for that specific scenario).
: > "$d/pooldata/etc/init.d/alpine-zfsboot-bootcheck"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.bootcheck" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
        STUB_BOOTCHECK_VALUE="armed:2" STUB_BOOTCHECK_SOURCE="local" STUB_BOOTCHECK_STATE="$d/bc-state"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA STUB_BOOTCHECK_VALUE STUB_BOOTCHECK_SOURCE STUB_BOOTCHECK_STATE
    . "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot"
) >"$d/out" 2>&1 || true
if grep -q -- "^armed:3	local$" "$d/bc-state" 2>/dev/null \
   && grep -qi "bootcheck: zroot/ROOT/alpine attempt 3 recorded" "$d/out"; then
    ok "armed:2 with the confirm service present becomes armed:3 after a successful kexec -l"
else
    cat "$d/bc-state" 2>/dev/null; cat "$d/out"; bad "bootcheck did not increment as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: bootcheck - DANGLING confirm-service symlink (script removed) -> disarms, same as absent =="
# The runlevel symlink alone being present isn't enough - if its own
# target (the actual /etc/init.d script) was removed by hand or lost to
# an interrupted apk upgrade, OpenRC will never actually run it either,
# so this must be treated exactly like the confirm service being
# absent entirely (disarm, don't increment) - not like it being present
# just because `-L` on a dangling symlink is still true.
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot" "$d/pooldata/etc/runlevels/default"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
ln -s /etc/init.d/alpine-zfsboot-bootcheck "$d/pooldata/etc/runlevels/default/alpine-zfsboot-bootcheck"
# Deliberately NOT creating etc/init.d/alpine-zfsboot-bootcheck itself.
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.bootcheck" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
        STUB_BOOTCHECK_VALUE="armed:2" STUB_BOOTCHECK_SOURCE="local" STUB_BOOTCHECK_STATE="$d/bc-state"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA STUB_BOOTCHECK_VALUE STUB_BOOTCHECK_SOURCE STUB_BOOTCHECK_STATE
    . "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot"
) >"$d/out" 2>&1 || true
if grep -q -- "^armed:0	local$" "$d/bc-state" 2>/dev/null && grep -qi "disarming (armed:0)" "$d/out"; then
    ok "a dangling runlevel symlink (script itself missing) is treated as no confirm service, disarmed"
else
    cat "$d/bc-state" 2>/dev/null; cat "$d/out"; bad "dangling confirm-service symlink was not treated as absent"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: bootcheck - armed:K + confirm service ABSENT -> disarms (armed:0), warns instead of incrementing =="
# The one genuinely unsafe outcome design review found across every
# scenario traced: a BE that's armed but has no way to ever reset the
# counter (hand-cloned outside this project's own tooling, rolled back
# to a pre-service snapshot, or had the service removed later) must
# never accumulate a count it can't reset - this is the fix. Actively
# disarming (not just refusing to increment) also self-heals a BE that
# already crossed the threshold before losing its confirm service,
# rather than leaving it stuck in forced-rescue forever.
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
# Deliberately NOT creating etc/runlevels/default/alpine-zfsboot-bootcheck.
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.bootcheck" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
        STUB_BOOTCHECK_VALUE="armed:2" STUB_BOOTCHECK_SOURCE="local" STUB_BOOTCHECK_STATE="$d/bc-state"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA STUB_BOOTCHECK_VALUE STUB_BOOTCHECK_SOURCE STUB_BOOTCHECK_STATE
    . "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot"
) >"$d/out" 2>&1 || true
if grep -q -- "^armed:0	local$" "$d/bc-state" 2>/dev/null && grep -qi "disarming (armed:0)" "$d/out"; then
    ok "armed BE with no confirm service registered is disarmed (armed:0), with a clear warning"
else
    cat "$d/bc-state" 2>/dev/null; cat "$d/out"; bad "bootcheck was not disarmed as expected when no confirm service is registered"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: bootcheck - absent/not-participating -> no bootcheck zfs set ever appears =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" >"$d/out" 2>&1 || true
if ! grep -q "^zfs set org.alpinezfsboot:bootcheck=" "$d/log" 2>/dev/null; then
    ok "a BE not participating in bootcheck (default stub, no property) is read but never written"
else
    cat "$d/log" 2>/dev/null; bad "bootcheck was written for a dataset that never opted in"
fi
rm -rf "$d"

# =============================================================================
# Last Boot Diagnostics' writer side: menu.py's own _last_attempt() reads
# org.alpinezfsboot:attempt_time/attempt_kernel/attempt_initrd/attempt_cmdline
# - a typo in either this file's property names or that one's would
# silently degrade to "never recorded" with every test still green (the
# exact stub-shaped hole that let a real NameError bug ship earlier this
# session). Asserting on the real logged `zfs set` invocation is the
# only thing that actually catches a name mismatch between the two files.
echo "== boot-dataset.sh: a successful kexec -l records the attempt (time/kernel/initrd/cmdline) via the exact property names menu.py's _last_attempt() reads =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" >"$d/out" 2>&1 || true
if grep -q "^zfs set org.alpinezfsboot:attempt_time=[0-9]\{4\}-[0-9]\{2\}-[0-9]\{2\}T[0-9]\{2\}:[0-9]\{2\}:[0-9]\{2\}Z org.alpinezfsboot:attempt_kernel=/boot/vmlinuz-lts org.alpinezfsboot:attempt_initrd=/boot/initramfs-lts org.alpinezfsboot:attempt_cmdline=.* zroot/ROOT/alpine\$" "$d/log" 2>/dev/null; then
    ok "a successful kexec -l records time/kernel/initrd/cmdline under the exact property names _last_attempt() reads back"
else
    cat "$d/log" 2>/dev/null; bad "the attempt record was not written with the expected property names/values - menu.py's _last_attempt() would silently see 'never recorded'"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: bootcheck=off for this boot -> no read, no increment, no warning =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.bootcheck" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
        STUB_BOOTCHECK_VALUE="armed:2" STUB_BOOTCHECK_SOURCE="local" ALPINE_ZFSBOOT_BOOTCHECK="off"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA STUB_BOOTCHECK_VALUE STUB_BOOTCHECK_SOURCE ALPINE_ZFSBOOT_BOOTCHECK
    . "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot"
) >"$d/out" 2>&1 || true
if ! grep -q "org.alpinezfsboot:bootcheck" "$d/log" 2>/dev/null; then
    ok "alpine-zfsboot.bootcheck=off skips reading/writing the property entirely for this boot"
else
    cat "$d/log" 2>/dev/null; bad "bootcheck=off did not fully disable the gate"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: panic= injection - absent from persisted cmdline, default timeout used =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" >"$d/out" 2>&1 || true
if grep -q -- "panic=10" "$d/log" 2>/dev/null; then
    ok "panic=10 (the default) is injected when nothing else configures panic="
else
    cat "$d/log" 2>/dev/null; bad "default panic= was not injected"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: panic_timeout=0 is a legal, meaningful value - not treated as unset =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
ALPINE_ZFSBOOT_PANIC_TIMEOUT=0 STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" >"$d/out" 2>&1 || true
if grep -q -- "panic=0" "$d/log" 2>/dev/null; then
    ok "alpine-zfsboot.panic_timeout=0 injects panic=0, not the default"
else
    cat "$d/log" 2>/dev/null; bad "explicit panic_timeout=0 was not honored"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: panic= injection - already present in the persisted cmdline wins, not duplicated =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot" "$d/pooldata/etc"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
printf 'panic=30\n' > "$d/pooldata/etc/alpine-zfsboot-cmdline"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot" >"$d/out" 2>&1 || true
if grep -q -- "panic=30" "$d/log" 2>/dev/null && ! grep -q -- "panic=10" "$d/log" 2>/dev/null; then
    ok "an explicit persisted panic= wins, alpine-zfsboot's own default is not appended alongside it"
else
    cat "$d/log" 2>/dev/null; bad "explicit panic= did not take priority as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: boot() derives the pool from the dataset, not a stale global =="
d="$(fresh_env)"
# boot() runs boot-dataset.sh via subprocess.run(), not os.execv() - an
# earlier version used execv, which meant a real user's `exit` from
# boot-dataset.sh's own failure-path bash shell had nothing to return
# to (confirmed on real hardware: it unwound all the way back to
# /init, which read that as "menu.py crashed" and fell through to
# automatic boot instead of back to this menu). Patch subprocess.run
# itself here, same technique the old version used on os.execv, to
# capture the argv without actually running anything.
PATH="$STUBS:$PATH" python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out2" 2>&1 || true
import sys, os
sys.path.insert(0, sys.argv[1])
import menu
menu.POOL = "zroot"  # the "stale global" - a different pool than the dataset below
captured = {}
def fake_run(argv):
    captured["argv"] = argv
menu.subprocess.run = fake_run
menu.boot("backuppool/ROOT/alpine")
print("argv:", captured["argv"])
PYEOF
if grep -q "argv: \['/boot-dataset.sh', 'backuppool/ROOT/alpine', 'backuppool', ''\]" "$d/out2"; then
    ok "boot() derives 'backuppool' from the dataset path, ignoring the stale POOL global"
else
    cat "$d/out2"; bad "boot() did not derive the correct pool from the dataset"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: list_all_boot_environments() aggregates across every imported pool =="
d="$(fresh_env)"
cat > "$d/stub-zpool" <<'EOF'
#!/bin/sh
[ "$1" = "list" ] && printf 'zroot\nbackuppool\n'
exit 0
EOF
chmod +x "$d/stub-zpool"
mkdir -p "$d/multipool"
cp "$d/stub-zpool" "$d/multipool/zpool"
cat > "$d/multipool/zfs" <<'EOF'
#!/bin/sh
case "$2 $3 $4 $5" in
    "-H -o name") true ;;
esac
if [ "$6" = "zroot/ROOT" ]; then printf 'zroot/ROOT/alpine\n'; fi
if [ "$6" = "backuppool/ROOT" ]; then printf 'backuppool/ROOT/alpine\n'; fi
exit 0
EOF
chmod +x "$d/multipool/zfs"
PATH="$d/multipool:$PATH" python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
print("pools:", menu.list_pools())
print("bes:", menu.list_all_boot_environments())
PYEOF
if grep -q "pools: \['zroot', 'backuppool'\]" "$d/out" \
   && grep -q "bes: \['zroot/ROOT/alpine', 'backuppool/ROOT/alpine'\]" "$d/out"; then
    ok "list_all_boot_environments() sees boot environments on BOTH imported pools"
else
    cat "$d/out"; bad "multi-pool BE aggregation did not behave as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== init: always scans for and imports other pools before the menu =="
# menu.py always runs now (see init/init's own header comment - an
# earlier version of this project had a separate fast, no-menu path
# that skipped this scan; that whole split is gone by deliberate
# choice), so this happens unconditionally, not only when a key was
# held.
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
cat > "$STUBS/zpool.multipool" <<'EOF'
#!/bin/sh
echo "zpool $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    import)
        if [ "$2" = "-d" ] && [ "$3" = "/dev" ] && [ -z "$4" ]; then
            printf '   pool: zroot\n   pool: backuppool\n'
        fi
        ;;
    get) echo "${STUB_BOOTFS:--}" ;;
    status) echo "pool is healthy" ;;
esac
exit 0
EOF
chmod +x "$STUBS/zpool.multipool"
printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0\n' > "$d/root/proc/cmdline"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool.multipool" "$@"; }
    zfs()      { "$STUBS/zfs" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    modprobe() { "$STUBS/modprobe" "$@"; }
    mdev()     { "$STUBS/mdev" "$@"; }
    ifconfig() { "$STUBS/ifconfig" "$@"; }
    udhcpc()   { "$STUBS/udhcpc" "$@"; }
    dropbear() { "$STUBS/dropbear" "$@"; }
    dropbearkey() { "$STUBS/dropbearkey" "$@"; }
    # python3 is NOT stubbed here (unlike other commands) - not worth
    # it either way, since real python3 running and failing to open
    # /menu.py (which isn't copied into this scratch dir) is fine for
    # this test: the scan being asserted on below already happened and
    # got logged before /init even reaches its `python3 /menu.py` line.
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="zroot/ROOT/alpine"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA STUB_BOOTFS
    . "$REPO_ROOT/init/init"
) >"$d/out" 2>&1 || true
if grep -q "zpool import -d /dev$" "$d/log" 2>/dev/null && grep -q "zpool import -f -N -d /dev backuppool" "$d/log"; then
    ok "scans for and imports the extra pool (backuppool) before the menu"
else
    cat "$d/log" 2>/dev/null; bad "did not scan for/import other pools as expected"
fi
rm -f "$STUBS/zpool.multipool"
rm -rf "$d"

# =============================================================================
echo "== init: warns when the pool is unhealthy =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0\n' > "$d/root/proc/cmdline"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="zroot/ROOT/alpine" \
    STUB_POOL_HEALTH="DEGRADED: one or more devices are faulted" \
    run_stubbed "$REPO_ROOT/init/init" >"$d/out" 2>&1 || true
if grep -qi "WARNING.*not healthy" "$d/out"; then
    ok "unhealthy pool prints a clear warning before the menu"
else
    cat "$d/out"; bad "unhealthy pool did not print the expected warning"
fi
rm -rf "$d"

# =============================================================================
echo "== init: pool with no bootfs set -> dies to a recovery shell, not a crash =="
d="$(fresh_env)"
printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0\n' > "$d/root/proc/cmdline"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="-" \
    run_stubbed "$REPO_ROOT/init/init" >"$d/out" 2>&1 || true
if grep -qi "no bootfs property set" "$d/out"; then
    ok "missing bootfs reported clearly rather than silently booting nothing"
else
    cat "$d/out"; bad "missing-bootfs case not handled as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== init: apply_zfsboot_kv() strips a trailing CR and warns on an unrecognized alpine-zfsboot.* key =="
# A real risk, not hypothetical: the ESP is FAT-formatted, and config
# files there get edited from Windows/rescue tooling/anything, any of
# which can leave CRLF line endings - a gateway value with a stray CR
# stuck to the end fails in a way that looks like an unrelated network
# problem, on a machine nobody can see. The unrecognized-key case
# matters for the identical reason: a typo (ipv6.gw instead of
# ipv6.gateway) was previously silently dropped with zero signal.
# Exercised via the real /proc/cmdline path (the ESP-config path itself
# is real-boot-only by construction, see /init's own comment) - the
# function under test is identical either way, apply_zfsboot_kv()
# doesn't know or care which caller fed it a line.
d="$(fresh_env)"
# STUB_BOOTFS="-" forces die() (same technique the existing die()
# tests above already use) - the only path where /init actually calls
# net_config/dropbear at all; a healthy boot never touches the network
# regardless of what's configured (see the test above this one), so a
# CR/route problem would never be exercised by one.
stage_rescue_ssh "$d/root"
printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0 alpine-zfsboot.ipv4=off alpine-zfsboot.ipv6=static alpine-zfsboot.ipv6.address=2001:db8::5/64 alpine-zfsboot.ipv6.gateway=fe80::1\r alpine-zfsboot.ipv6.gw=typo\n' > "$d/root/proc/cmdline"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="-" \
    run_stubbed "$REPO_ROOT/init/init" >"$d/out" 2>&1 || true
if grep -q -- "-6 route add default via fe80::1 dev" "$d/log" 2>/dev/null \
   && ! grep -q $'fe80::1\r' "$d/log" \
   && grep -qi "unrecognized option alpine-zfsboot.ipv6.gw=typo" "$d/out"; then
    ok "trailing CR stripped from a config value, and an unrecognized alpine-zfsboot.* key is warned about, not silently dropped"
else
    cat "$d/log" 2>/dev/null; cat "$d/out"; bad "CR-stripping or the unrecognized-key warning did not behave as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== init: apply_zfsboot_kv() treats alpine-zfsboot.version=/buildstamp= as recognized no-ops, not unrecognized options =="
# Real CAX hardware bug: build.sh's own cmdline-assembly printf bakes
# BOTH of these onto EVERY built cmdline unconditionally (see build.sh's
# own comment) - they are cmd/tool-only metadata, read straight out of
# the .cmdline PE section offline, never meant as operator config and
# never read by anything inside the booted environment. Before the fix,
# apply_zfsboot_kv() had no case arm for either, so they fell into the
# generic "unrecognized option" catch-all and warned on 100% of real
# boots - confirmed on real hardware, not a hypothetical. The exact
# cmdline shape below mirrors build.sh's own printf ordering, and a
# genuinely bogus key is included alongside them to prove the catch-all
# itself isn't weakened - only these two specific keys are exempted.
d="$(fresh_env)"
printf 'console=tty0 root=ZFS=zroot/ROOT/alpine ro quiet kexec_load_disabled=0 alpine-zfsboot.pool=zroot alpine-zfsboot.timeout=0 alpine-zfsboot.version=0.1.0 alpine-zfsboot.buildstamp=20260917T220623Z alpine-zfsboot.ipv6.gw=typo\n' \
    > "$d/root/proc/cmdline"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="-" \
    run_stubbed "$REPO_ROOT/init/init" >"$d/out" 2>&1 || true
if ! grep -qi "unrecognized option alpine-zfsboot.version" "$d/out" 2>/dev/null \
   && ! grep -qi "unrecognized option alpine-zfsboot.buildstamp" "$d/out" 2>/dev/null \
   && grep -qi "unrecognized option alpine-zfsboot.ipv6.gw=typo" "$d/out" 2>/dev/null; then
    ok "version=/buildstamp= produce no warning, while a genuinely unrecognized key on the SAME cmdline still does"
else
    cat "$d/out"; bad "version=/buildstamp= warned as unrecognized, or the catch-all stopped catching real typos"
fi
rm -rf "$d"

# =============================================================================
echo "== alpine-zfsboot-shell: interactive (no args) -> execs the menu =="
d="$(fresh_env)"
STUB_LOG="$d/log" PATH="$STUBS_PY:$STUBS:$PATH" "$REPO_ROOT/init/alpine-zfsboot-shell" >"$d/out" 2>&1 || true
if grep -q "^python3 /menu.py$" "$d/log" 2>/dev/null; then
    ok "no args -> exec python3 /menu.py"
else
    cat "$d/log" 2>/dev/null || true; bad "interactive invocation did not run menu.py"
fi
rm -rf "$d"

# =============================================================================
echo "== alpine-zfsboot-shell: -c \"cmd\" -> runs that exact command, not the menu =="
d="$(fresh_env)"
STUB_LOG="$d/log" PATH="$STUBS_PY:$STUBS:$PATH" "$REPO_ROOT/init/alpine-zfsboot-shell" -c "zfs recv zroot/ROOT/newbe" >"$d/out" 2>&1 || true
if grep -q "^zfs recv zroot/ROOT/newbe$" "$d/log" 2>/dev/null; then
    ok "-c \"zfs recv ...\" runs verbatim through bash -c, NOT forced to menu.py"
else
    cat "$d/log" 2>/dev/null || true; bad "-c dispatch broken - this is what would break zfs send | ssh ... zfs recv"
fi
rm -rf "$d"

# =============================================================================
echo "== alpine-zfsboot-shell: -c \"zfs send ...\" (the reverse direction) is also allowed =="
d="$(fresh_env)"
STUB_LOG="$d/log" PATH="$STUBS_PY:$STUBS:$PATH" "$REPO_ROOT/init/alpine-zfsboot-shell" -c "zfs send zroot/ROOT/alpine@snap1" >"$d/out" 2>&1 || true
if grep -q "^zfs send zroot/ROOT/alpine@snap1$" "$d/log" 2>/dev/null; then
    ok "-c \"zfs send ...\" runs verbatim through bash -c too, not just recv"
else
    cat "$d/log" 2>/dev/null || true; bad "zfs send dispatch broken"
fi
rm -rf "$d"

# =============================================================================
echo "== alpine-zfsboot-shell: -c \"zfs receive ...\" (the full spelling, not just recv) is also allowed =="
# zfs's own recv/receive are genuinely interchangeable (recv is just
# the short alias) and real syncoid-style tooling/documentation uses
# the full spelling at least as often - refusing it would read like a
# deliberate policy choice rather than the spelling gap it'd actually be.
d="$(fresh_env)"
STUB_LOG="$d/log" PATH="$STUBS_PY:$STUBS:$PATH" "$REPO_ROOT/init/alpine-zfsboot-shell" -c "zfs receive zroot/ROOT/newbe" >"$d/out" 2>&1 || true
if grep -q "^zfs receive zroot/ROOT/newbe$" "$d/log" 2>/dev/null; then
    ok "-c \"zfs receive ...\" (full spelling) is allowed too, not just the recv abbreviation"
else
    cat "$d/log" 2>/dev/null || true; cat "$d/out"; bad "zfs receive (full spelling) was incorrectly refused"
fi
rm -rf "$d"

# =============================================================================
echo "== alpine-zfsboot-shell: -c with anything other than zfs send/recv is refused, not run =="
# The actual security-relevant fix: rescue-ssh.sh's own authorized_keys
# forces \`no-port-forwarding,no-agent-forwarding,no-X11-forwarding\` on
# every accepted key, but that only disables SSH FEATURES - it says
# nothing about which COMMAND runs. Without an allowlist here, every
# rescue key would still have unrestricted root command execution in
# this pre-boot environment, which would make that option set a false
# sense of hardening rather than a real one.
d="$(fresh_env)"
STUB_LOG="$d/log" PATH="$STUBS_PY:$STUBS:$PATH" "$REPO_ROOT/init/alpine-zfsboot-shell" -c "cat /etc/passwd" >"$d/out" 2>&1 || true
if [ ! -s "$d/log" ] && grep -qi "only 'zfs send" "$d/out" 2>/dev/null; then
    ok "an arbitrary non-zfs command is refused outright, never reaches a real shell"
else
    cat "$d/log" 2>/dev/null || true; cat "$d/out"; bad "an arbitrary command was NOT refused as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== alpine-zfsboot-shell: -c \"zfs recv ...; rm -rf /\" (prefix matches, payload doesn't) is refused =="
# A prefix check alone ("starts with zfs recv ") proves nothing once
# the string reaches a real shell - this is exactly the injection route
# a naive allowlist would miss. The full command must be free of shell
# metacharacters, not just correctly prefixed.
d="$(fresh_env)"
STUB_LOG="$d/log" PATH="$STUBS_PY:$STUBS:$PATH" "$REPO_ROOT/init/alpine-zfsboot-shell" -c "zfs recv zroot/ROOT/newbe; rm -rf /" >"$d/out" 2>&1 || true
if [ ! -s "$d/log" ] && grep -qi "outside the safe zfs send/recv set" "$d/out" 2>/dev/null; then
    ok "a zfs recv command with an injected second command is refused, not partially executed"
else
    cat "$d/log" 2>/dev/null || true; cat "$d/out"; bad "command injection via a disallowed character was NOT refused"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: zfs_list()/list_kernels()/parse_source() helpers, no curses =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"
PATH="$STUBS:$PATH" STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    STUB_ZFS_LIST_OUTPUT="zroot/ROOT/alpine
zroot/ROOT/alpine-backup" \
    python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
print("zfs_list:", menu.zfs_list("zroot/ROOT"))
print("list_kernels:", menu.list_kernels("zroot/ROOT/alpine"))
print("parse_source remote:", menu.parse_source("backup01:zroot/images/alpine@golden"))
print("parse_source local:", menu.parse_source("zroot/images/alpine@golden"))
PYEOF
if grep -q "zfs_list: \['zroot/ROOT/alpine', 'zroot/ROOT/alpine-backup'\]" "$d/out" \
   && grep -q "list_kernels: \['lts'\]" "$d/out"; then
    ok "menu.py's zfs_list/list_kernels parse stub output correctly"
else
    cat "$d/out"; bad "menu.py helper functions did not behave as expected"
fi
if grep -q "parse_source remote: ('backup01', 'zroot/images/alpine@golden')" "$d/out" \
   && grep -q "parse_source local: (None, 'zroot/images/alpine@golden')" "$d/out"; then
    ok "parse_source splits host:dataset@snap correctly, leaves a bare local source alone"
else
    cat "$d/out"; bad "parse_source did not behave as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: previous_boot_evidence() returns None when pstore was never mounted/configured =="
d="$(fresh_env)"
PATH="$STUBS:$PATH" python3 - "$REPO_ROOT/init" <<PYEOF >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
menu.PSTORE_ROOT = "$d/no-such-pstore-dir"
print("evidence:", menu.previous_boot_evidence())
PYEOF
if grep -qx "evidence: None" "$d/out"; then
    ok "previous_boot_evidence() returns None when /sys/fs/pstore doesn't exist at all"
else
    cat "$d/out"; bad "previous_boot_evidence() did not return None as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: previous_boot_evidence() returns None when pstore is mounted but empty =="
d="$(fresh_env)"
mkdir -p "$d/pstore"
PATH="$STUBS:$PATH" python3 - "$REPO_ROOT/init" <<PYEOF >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
menu.PSTORE_ROOT = "$d/pstore"
print("evidence:", menu.previous_boot_evidence())
PYEOF
if grep -qx "evidence: None" "$d/out"; then
    ok "previous_boot_evidence() returns None when pstore exists but has no records (last boot didn't panic)"
else
    cat "$d/out"; bad "previous_boot_evidence() did not return None for an empty pstore"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: previous_boot_evidence() detects a real kernel panic ONLY via the literal substring, never inferred =="
d="$(fresh_env)"
mkdir -p "$d/pstore"
printf '<0>[    3.914616] Kernel panic - not syncing: VFS: Unable to mount root fs\n' > "$d/pstore/dmesg-efi-0"
PATH="$STUBS:$PATH" python3 - "$REPO_ROOT/init" <<PYEOF >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
menu.PSTORE_ROOT = "$d/pstore"
e = menu.previous_boot_evidence()
print("panic:", e["panic"])
print("has_text:", "Unable to mount root fs" in e["text"])
PYEOF
if grep -qx "panic: True" "$d/out" && grep -qx "has_text: True" "$d/out"; then
    ok "previous_boot_evidence() correctly detects a real 'Kernel panic' record and preserves its full text"
else
    cat "$d/out"; bad "previous_boot_evidence() did not detect the panic record as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: previous_boot_evidence() never claims a panic that isn't literally in the text - shows evidence, not a guess =="
d="$(fresh_env)"
mkdir -p "$d/pstore"
printf '<6>[    4.812] Starting localmount ...\n' > "$d/pstore/dmesg-efi-0"
PATH="$STUBS:$PATH" python3 - "$REPO_ROOT/init" <<PYEOF >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
menu.PSTORE_ROOT = "$d/pstore"
e = menu.previous_boot_evidence()
print("panic:", e["panic"])
print("has_text:", "Starting localmount" in e["text"])
PYEOF
if grep -qx "panic: False" "$d/out" && grep -qx "has_text: True" "$d/out"; then
    ok "a record with no 'Kernel panic' substring is never labeled a panic - raw evidence is still preserved and shown"
else
    cat "$d/out"; bad "previous_boot_evidence() incorrectly claimed a cause not present in the text"
fi
rm -rf "$d"

# =============================================================================
# The architecture audit's biggest finding: the headline ("Previous boot
# diagnostics (FAILED/OK)", the cockpit's "Prev boot:" line) must be
# derived ONLY from bootcheck's own armed:K counter - NEVER from whether
# a crash-evidence backend happens to have anything to show. An earlier
# version violated this (evidence-empty was rendered as "clean boot"),
# which is a category-4 violation: a machine mid-FORCED_RESCUE (bootcheck
# already proved the last attempt failed) would have been told "clean
# boot" purely because pstore was empty. The tests below drive
# _bootcheck_k() via a zfs stub and deliberately ALSO plant real panic
# evidence in the K=0 case, to prove the headline truly ignores it.
echo "== menu.py: _items()'s 'Previous boot diagnostics' label comes from bootcheck ONLY, never from evidence presence =="
d="$(fresh_env)"
mkdir -p "$d/zfsstub" "$d/pstore-panic"
cp "$STUBS/zfs.bootcheck" "$d/zfsstub/zfs"
chmod +x "$d/zfsstub/zfs"
printf 'Kernel panic - not syncing: test\n' > "$d/pstore-panic/dmesg-efi-0"
PATH="$d/zfsstub:$STUBS:$PATH" \
    python3 - "$REPO_ROOT/init" <<PYEOF >"$d/out" 2>&1 || true
import sys, os
sys.path.insert(0, sys.argv[1])
import menu
menu.PSTORE_ROOT = "$d/pstore-panic"
# K=0 but real panic evidence physically present - headline must still say OK.
os.environ["STUB_BOOTCHECK_VALUE"] = "armed:0"
os.environ["STUB_BOOTCHECK_SOURCE"] = "local"
items = menu._items()
print("ok_label:", [i for i in items if i.startswith("Previous boot diagnostics")][0])
os.environ["STUB_BOOTCHECK_VALUE"] = "armed:3"
items = menu._items()
print("failed_label:", [i for i in items if i.startswith("Previous boot diagnostics")][0])
PYEOF
if grep -qx "ok_label: Previous boot diagnostics (OK)" "$d/out" \
   && grep -qx "failed_label: Previous boot diagnostics (FAILED)" "$d/out"; then
    ok "the menu label reflects bootcheck's armed:K state, even with real panic evidence sitting there at K=0"
else
    cat "$d/out"; bad "_items()'s Previous boot diagnostics label did not follow bootcheck K as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: _rescue_status_text()'s 'Prev boot:' line comes from bootcheck ONLY, never from evidence presence =="
d="$(fresh_env)"
mkdir -p "$d/zfsstub" "$d/pstore-panic"
cp "$STUBS/zfs.bootcheck" "$d/zfsstub/zfs"
chmod +x "$d/zfsstub/zfs"
printf 'Kernel panic - not syncing: test\n' > "$d/pstore-panic/dmesg-efi-0"
PATH="$d/zfsstub:$STUBS:$PATH" \
    STUB_BOOTCHECK_VALUE="armed:0" STUB_BOOTCHECK_SOURCE="local" \
    python3 - "$REPO_ROOT/init" <<PYEOF >"$d/out" 2>&1 || true
import sys, os
sys.path.insert(0, sys.argv[1])
import menu
menu.PSTORE_ROOT = "$d/pstore-panic"
os.environ["STUB_BOOTCHECK_VALUE"] = "armed:0"
text = menu._rescue_status_text()
print("ok_line:", [l for l in text.splitlines() if l.startswith("Prev boot:")][0])
os.environ["STUB_BOOTCHECK_VALUE"] = "armed:5"
text = menu._rescue_status_text()
print("failed_line:", [l for l in text.splitlines() if l.startswith("Prev boot:")][0])
PYEOF
if grep -qx "ok_line: Prev boot:       OK" "$d/out" \
   && grep -qx "failed_line: Prev boot:       FAILED - see Previous boot diagnostics" "$d/out"; then
    ok "the cockpit 'Prev boot:' line reflects bootcheck's armed:K state, even with real panic evidence sitting there at K=0"
else
    cat "$d/out"; bad "_rescue_status_text()'s Prev boot line did not follow bootcheck K as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: show_previous_boot_diagnostics()'s 'Kernel crash record:' line distinguishes unsupported/not-available/available on ITS OWN capability signal =="
d="$(fresh_env)"
mkdir -p "$d/root" "$d/pooldata" "$d/pstore-empty" "$d/pstore-panic"
printf 'Kernel panic - not syncing: test\n' > "$d/pstore-panic/dmesg-efi-0"
PATH="$STUBS:$PATH" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    python3 - "$REPO_ROOT/init" <<PYEOF >"$d/out" 2>&1 || true
import sys, os
sys.path.insert(0, sys.argv[1])
os.environ["ALPINE_ZFSBOOT_POOL_IMPORT_ERROR"] = "boom"  # skip bootcheck/attempt-record calls, not the focus here
import menu
calls = []
menu.dialog_textbox = lambda title, text: calls.append(text)

menu.PSTORE_BACKEND_PATH = "$d/no-such-backend-file"
menu.PSTORE_ROOT = "$d/no-such-dir"
menu.show_previous_boot_diagnostics()
print("unsupported:", [l for l in calls[-1].splitlines() if "UNSUPPORTED" in l])

menu.PSTORE_BACKEND_PATH = "$d/backend-file"
with open("$d/backend-file", "w") as f:
    f.write("efi_pstore\n")
menu.PSTORE_ROOT = "$d/pstore-empty"
menu.show_previous_boot_diagnostics()
print("not_available:", [l for l in calls[-1].splitlines() if "NOT AVAILABLE" in l])

menu.PSTORE_ROOT = "$d/pstore-panic"
menu.show_previous_boot_diagnostics()
print("available:", [l for l in calls[-1].splitlines() if l.startswith("  AVAILABLE")])
PYEOF
if grep -q "^unsupported:.*UNSUPPORTED" "$d/out" \
   && grep -q "^not_available:.*NOT AVAILABLE" "$d/out" \
   && grep -q "^available:.*AVAILABLE.*KERNEL PANIC" "$d/out"; then
    ok "the Kernel crash record line distinguishes 'unsupported platform' from 'no record' from 'record present', from its own capability signal"
else
    cat "$d/out"; bad "show_previous_boot_diagnostics()'s Kernel crash record line did not reflect all three states as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: _pstore_records() sorts numerically, not lexicographically (dmesg-efi-10 must not sort before dmesg-efi-2) =="
d="$(fresh_env)"
mkdir -p "$d/pstore"
for n in 0 1 2 10; do printf 'record %s\n' "$n" > "$d/pstore/dmesg-efi-$n"; done
PATH="$STUBS:$PATH" python3 - "$REPO_ROOT/init" <<PYEOF >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
menu.PSTORE_ROOT = "$d/pstore"
print("records:", menu._pstore_records())
PYEOF
if grep -Fqx "records: ['dmesg-efi-0', 'dmesg-efi-1', 'dmesg-efi-2', 'dmesg-efi-10']" "$d/out"; then
    ok "_pstore_records() sorts by the numeric record index, not string order"
else
    cat "$d/out"; bad "_pstore_records() did not sort numerically as expected - dmesg-efi-10 sorting before dmesg-efi-2 would silently misorder a multi-part crash dump"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: _last_attempt() returns None when never recorded, and the real fields when it was =="
d="$(fresh_env)"
mkdir -p "$d/attemptstub"
cat > "$d/attemptstub/zfs" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
if [ "$6" = "zroot/ROOT/alpine" ]; then
    case "$1 $2 $3 $4 $5" in
        "get -H -o value org.alpinezfsboot:attempt_time") echo "2026-09-20T10:00:00Z" ;;
        "get -H -o value org.alpinezfsboot:attempt_kernel") echo "/boot/vmlinuz-lts" ;;
        "get -H -o value org.alpinezfsboot:attempt_initrd") echo "/boot/initramfs-lts" ;;
        "get -H -o value org.alpinezfsboot:attempt_cmdline") echo "root=ZFS=zroot/ROOT/alpine ro" ;;
        *) echo "-" ;;
    esac
else
    echo "-"
fi
exit 0
EOF
chmod +x "$d/attemptstub/zfs"
PATH="$d/attemptstub:$STUBS:$PATH" python3 - "$REPO_ROOT/init" <<PYEOF >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
print("never_recorded:", menu._last_attempt("zroot/ROOT/never-booted"))
print("recorded:", menu._last_attempt("zroot/ROOT/alpine"))
PYEOF
if grep -qx "never_recorded: None" "$d/out"; then
    :
else
    cat "$d/out"; bad "_last_attempt() did not return None for a BE with no recorded attempt"
fi
if grep -q "^recorded: {'time': '2026-09-20T10:00:00Z', 'kernel': '/boot/vmlinuz-lts', 'initrd': '/boot/initramfs-lts', 'cmdline': 'root=ZFS=zroot/ROOT/alpine ro'}$" "$d/out"; then
    ok "_last_attempt() returns None when never recorded, and reads back the real attempt_* fields when it was"
else
    cat "$d/out"; bad "_last_attempt() did not return the recorded attempt fields as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: _rc_log_evidence() distinguishes unavailable/fresh/stale - stale means rc.log itself predates the recorded attempt, no cause claimed =="
d="$(fresh_env)"
mkdir -p "$d/root" "$d/pooldata/var/log"
printf '* Starting networking ...\n* ERROR: networking failed to start\n' > "$d/pooldata/var/log/rc.log"
attempt='{"time": "2026-09-20T10:00:00Z"}'
PATH="$STUBS:$PATH" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    python3 - "$REPO_ROOT/init" <<PYEOF >"$d/out" 2>&1 || true
import sys, os
sys.path.insert(0, sys.argv[1])
import menu

attempt_fresh = {"time": "2020-01-01T00:00:00Z"}   # safely BEFORE rc.log's real mtime (now) -> fresh
attempt_stale = {"time": "2099-01-01T00:00:00Z"}   # AFTER rc.log's real mtime -> stale

result_no_attempt = menu._rc_log_evidence("zroot/ROOT/alpine", None)
print("no_attempt_available:", result_no_attempt["available"])
print("no_attempt_stale:", result_no_attempt["stale"])

result_fresh = menu._rc_log_evidence("zroot/ROOT/alpine", attempt_fresh)
print("fresh_available:", result_fresh["available"])
print("fresh_stale:", result_fresh["stale"])
print("fresh_has_text:", "networking failed to start" in (result_fresh["text"] or ""))

result_stale = menu._rc_log_evidence("zroot/ROOT/alpine", attempt_stale)
print("stale_available:", result_stale["available"])
print("stale_stale:", result_stale["stale"])
PYEOF
if grep -qx "no_attempt_available: True" "$d/out" && grep -qx "no_attempt_stale: False" "$d/out" \
   && grep -qx "fresh_available: True" "$d/out" && grep -qx "fresh_stale: False" "$d/out" \
   && grep -qx "fresh_has_text: True" "$d/out" \
   && grep -qx "stale_available: True" "$d/out" && grep -qx "stale_stale: True" "$d/out"; then
    ok "_rc_log_evidence() reads real rc.log content and correctly flags it stale when it predates the recorded attempt"
else
    cat "$d/out"; bad "_rc_log_evidence() did not distinguish fresh/stale/no-attempt as expected"
fi
rm -rf "$d"

# =============================================================================
# A second, later adversarial audit ("rock solid / Rolls Royce" pass)
# found three more real issues in the first implementation, all fixed
# and regression-tested here:
echo "== menu.py: _pstore_backend() reads the kernel's own sysfs file live - survives a wiped environment (the real rescue-SSH case) =="
d="$(fresh_env)"
printf 'efi_pstore\n' > "$d/backend-file"
PATH="$STUBS:$PATH" python3 - "$REPO_ROOT/init" <<PYEOF >"$d/out" 2>&1 || true
import sys, os
sys.path.insert(0, sys.argv[1])
import menu
menu.PSTORE_BACKEND_PATH = "$d/backend-file"
# Simulate dropbear's own clearenv() - an earlier version of this read
# an ALPINE_ZFSBOOT_PSTORE_BACKEND env var /init exported, which real
# dropbear wipes for every rescue-SSH session (same already-documented
# problem as ACTIVE_TTY) - a remote operator would always have seen
# "unsupported" regardless of real hardware state. Backend registration
# is a kernel-global fact, not a per-process one, so clearing the
# environment entirely must have NO effect on the answer.
os.environ.clear()
print("backend:", menu._pstore_backend())
PYEOF
if grep -qx "backend: efi_pstore" "$d/out"; then
    ok "_pstore_backend() reads the real sysfs file directly - a fully wiped environment (the real dropbear/rescue-SSH case) does not change the answer"
else
    cat "$d/out"; bad "_pstore_backend() did not survive a wiped environment as expected - this is exactly the bug that would make crash evidence invisible over rescue SSH"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: _bootcheck_k() returns None (UNKNOWN), never a false OK, when bootcheck isn't really tracking this BE =="
d="$(fresh_env)"
mkdir -p "$d/zfsstub"
cp "$STUBS/zfs.bootcheck" "$d/zfsstub/zfs"
chmod +x "$d/zfsstub/zfs"
PATH="$d/zfsstub:$STUBS:$PATH" ALPINE_ZFSBOOT_POOL_IMPORT_ERROR="boom" \
    python3 - "$REPO_ROOT/init" <<PYEOF >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
print("pool_import_error:", menu._bootcheck_k("zroot/ROOT/alpine"))
PYEOF
PATH="$d/zfsstub:$STUBS:$PATH" STUB_BOOTCHECK_VALUE="-" STUB_BOOTCHECK_SOURCE="-" \
    python3 - "$REPO_ROOT/init" <<PYEOF >>"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
print("absent:", menu._bootcheck_k("zroot/ROOT/alpine"))
PYEOF
if grep -qx "pool_import_error: None" "$d/out" && grep -qx "absent: None" "$d/out"; then
    ok "_bootcheck_k() returns None (unknown) rather than a false 0/OK when the pool didn't import or bootcheck isn't tracking this BE"
else
    cat "$d/out"; bad "_bootcheck_k() did not return None as expected - a machine whose pool failed to import would be told its previous boot was OK"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: _rc_log_evidence() never prompts for a passphrase on a locked encrypted BE - reports locked=True instead =="
d="$(fresh_env)"
mkdir -p "$d/encfs"
cat > "$d/encfs/zfs" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1 $5 $6" in
    "get encryptionroot zroot/ROOT/alpine") echo "zroot/ROOT/alpine" ;;
    "get keystatus zroot/ROOT/alpine") echo "unavailable" ;;
    *) echo "-" ;;
esac
exit 0
EOF
chmod +x "$d/encfs/zfs"
cat > "$d/encfs/zfs-unlock" <<'EOF'
#!/bin/sh
echo "zfs-unlock $*" >> "${STUB_LOG:-/dev/null}"
exit 0
EOF
chmod +x "$d/encfs/zfs-unlock"
PATH="$d/encfs:$PATH" STUB_LOG="$d/log" STUB_ZFS_UNLOCK_SH="$d/encfs/zfs-unlock" \
    python3 - "$REPO_ROOT/init" <<PYEOF >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
result = menu._rc_log_evidence("zroot/ROOT/alpine", None)
print("locked:", result["locked"])
print("available:", result["available"])
PYEOF
if grep -qx "locked: True" "$d/out" && grep -qx "available: False" "$d/out" \
   && ! grep -q "^zfs-unlock " "$d/log" 2>/dev/null; then
    ok "_rc_log_evidence() reports locked=True on a locked encrypted BE without ever invoking zfs-unlock (no forced passphrase prompt just to open a status screen)"
else
    cat "$d/out"; cat "$d/log" 2>/dev/null; bad "_rc_log_evidence() either failed to detect the locked state or (worse) tried to unlock it"
fi
rm -rf "$d"

# =============================================================================
# rc.log and pstore records are raw text sourced from the TARGET's own
# boot - genuinely untrusted by the time they reach this rescue
# session's terminal. sanitize_diagnostic_text() strips ANSI/control
# sequences that could corrupt the display and caps size - presentation
# hardening requested explicitly after the architecture review, since
# errors="replace" at read time only handles invalid byte sequences,
# not a terminal-corrupting escape sequence or a pathologically large
# record.
echo "== menu.py: sanitize_diagnostic_text() strips ANSI escapes and other control bytes, keeps \\n/\\t, truncates oversized input =="
d="$(fresh_env)"
PATH="$STUBS:$PATH" python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu

colored = "\x1b[31m* ERROR: networking failed to start\x1b[0m\nline two"
print("ansi_stripped:", repr(menu.sanitize_diagnostic_text(colored)))

controlled = "before\x00\x01\x07after\r\nkeep\ttab\nkeep newline"
print("control_stripped:", repr(menu.sanitize_diagnostic_text(controlled)))

huge = "x" * (menu.DIAGNOSTIC_TEXT_MAX_BYTES + 5000)
result = menu.sanitize_diagnostic_text(huge)
print("truncated:", len(result.encode("utf-8")) <= menu.DIAGNOSTIC_TEXT_MAX_BYTES + 200)
print("truncated_marker:", "truncated" in result)
PYEOF
if grep -qx "ansi_stripped: '\* ERROR: networking failed to start\\\\nline two'" "$d/out" \
   && grep -qx "control_stripped: 'beforeafter\\\\nkeep\\\\ttab\\\\nkeep newline'" "$d/out" \
   && grep -qx "truncated: True" "$d/out" && grep -qx "truncated_marker: True" "$d/out"; then
    ok "sanitize_diagnostic_text() strips ANSI color codes and stray control bytes, preserves real newlines/tabs, and caps oversized output"
else
    cat "$d/out"; bad "sanitize_diagnostic_text() did not sanitize as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: show_previous_boot_diagnostics() actually sanitizes rc.log/pstore text before it reaches dialog, not just in isolation =="
d="$(fresh_env)"
mkdir -p "$d/root" "$d/pooldata/var/log" "$d/pstore-panic"
printf '\x1b[31mKernel panic - not syncing: test\x1b[0m\r\n' > "$d/pstore-panic/dmesg-efi-0"
printf '\x1b[32m* Starting networking ...\x1b[0m\r\n\x1b[31m* ERROR: networking failed to start\x1b[0m\r\n' > "$d/pooldata/var/log/rc.log"
PATH="$STUBS:$PATH" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    python3 - "$REPO_ROOT/init" <<PYEOF >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
menu.PSTORE_ROOT = "$d/pstore-panic"
menu.PSTORE_BACKEND_PATH = "$d/backend-file"
with open("$d/backend-file", "w") as f:
    f.write("efi_pstore\n")
calls = []
menu.dialog_textbox = lambda title, text: calls.append(text)
menu.show_previous_boot_diagnostics()
shown = calls[-1]
print("has_escape_byte:", "\x1b" in shown)
print("has_cr:", "\r" in shown)
print("has_panic_plain:", "Kernel panic - not syncing: test" in shown)
print("has_rc_plain:", "ERROR: networking failed to start" in shown)
PYEOF
if grep -qx "has_escape_byte: False" "$d/out" && grep -qx "has_cr: False" "$d/out" \
   && grep -qx "has_panic_plain: True" "$d/out" && grep -qx "has_rc_plain: True" "$d/out"; then
    ok "the real diagnostics screen sanitizes both rc.log and the pstore crash record before showing them, while preserving the real evidence text"
else
    cat "$d/out"; bad "show_previous_boot_diagnostics() did not sanitize the text it actually shows"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: _dispatch(14) calls show_previous_boot_diagnostics() =="
d="$(fresh_env)"
PATH="$STUBS:$PATH" python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
called = {}
menu.show_previous_boot_diagnostics = lambda: called.setdefault("yes", True)
menu._dispatch(14)
print("called:", called.get("yes", False))
PYEOF
if grep -qx "called: True" "$d/out"; then
    ok "_dispatch(14) routes to show_previous_boot_diagnostics()"
else
    cat "$d/out"; bad "_dispatch(14) did not call show_previous_boot_diagnostics()"
fi
rm -rf "$d"

# =============================================================================
# The three tests above (and the earlier previous_boot_evidence()/_items()/
# _rescue_status_text() ones) all either call previous_boot_evidence()
# directly or stub show_previous_boot_diagnostics()/show_diagnostics()
# themselves out before dispatch - NONE of them ever actually executed
# either function's real body end to end. That gap is exactly how a real
# bug shipped silently: show_diagnostics()'s own closing render call had
# been left stranded inside show_previous_boot_diagnostics() instead (a
# stray edit during this feature's own development), so the former did
# nothing at all (item 7, a real pre-existing feature, silently broken)
# and the latter raised NameError the moment real panic evidence existed
# - which would have propagated out of _dispatch() to main()'s own
# top-level handler and kexec'd straight back into the BE that just
# panicked. Caught only by an adversarial audit reading the file, not by
# any of the 143 tests passing - the two tests below close that gap by
# calling the REAL functions, not a stub standing in for them.
echo "== menu.py: show_previous_boot_diagnostics() with a real panic record renders exactly once, no crash (regression) =="
d="$(fresh_env)"
mkdir -p "$d/root" "$d/pooldata" "$d/pstore-panic"
printf 'Kernel panic - not syncing: test\n' > "$d/pstore-panic/dmesg-efi-0"
PATH="$STUBS:$PATH" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    python3 - "$REPO_ROOT/init" <<PYEOF >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
menu.PSTORE_ROOT = "$d/pstore-panic"
menu.PSTORE_BACKEND_PATH = "$d/backend-file"
with open("$d/backend-file", "w") as f:
    f.write("efi_pstore\n")
calls = []
menu.dialog_textbox = lambda title, text: calls.append(text)
menu.show_previous_boot_diagnostics()
print("call_count:", len(calls))
print("has_panic_text:", "Kernel panic" in calls[0] if calls else None)
PYEOF
if grep -qx "call_count: 1" "$d/out" && grep -qx "has_panic_text: True" "$d/out"; then
    ok "show_previous_boot_diagnostics() renders the real panic evidence exactly once, without raising"
else
    cat "$d/out"; bad "show_previous_boot_diagnostics() did not render cleanly - this is the exact NameError regression this test exists to catch"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: show_diagnostics() actually renders its sections (regression - its own closing call had gone missing) =="
d="$(fresh_env)"
PATH="$STUBS:$PATH" python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys, os
sys.path.insert(0, sys.argv[1])
os.environ["ALPINE_ZFSBOOT_POOL_IMPORT_ERROR"] = "boom"  # skip real zfs/network calls
import menu
calls = []
menu.dialog_textbox = lambda title, text: calls.append((title, text))
menu.show_diagnostics()
print("call_count:", len(calls))
print("has_zpool_section:", "=== zpool status ===" in calls[0][1] if calls else None)
PYEOF
if grep -qx "call_count: 1" "$d/out" && grep -qx "has_zpool_section: True" "$d/out"; then
    ok "show_diagnostics() actually renders its sections - was a silent no-op before this fix"
else
    cat "$d/out"; bad "show_diagnostics() did not render its sections - this is the exact regression this test exists to catch"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: ensure_key_loaded() - unencrypted passes through, locked shells out to the shared zfs-unlock primitive =="
# STUB_ZFS_UNLOCK_SH stands in for the real /zfs-unlock (only present
# inside a real built initramfs) - this test's real point is that
# ensure_key_loaded() calls THIS shared entry point at all (the actual
# fix for the reported gap - see ZFS_UNLOCK_SH's own comment), not that
# it reimplements the prompt loop itself (zfs-unlock.sh's own tests
# below cover that).
d="$(fresh_env)"
mkdir -p "$d/encfs"
cat > "$d/encfs/zfs" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    get)
        prop="$5"; ds="$6"
        case "$ds:$prop" in
            "zroot/ROOT/plain:encryptionroot") echo "-" ;;
            "zroot/ROOT/enc:encryptionroot") echo "zroot/ROOT/enc" ;;
            "zroot/ROOT/enc:keystatus") echo "unavailable" ;;
            "zroot/ROOT/enc:keylocation") echo "prompt" ;;
            *) echo "-" ;;
        esac
        ;;
esac
exit 0
EOF
chmod +x "$d/encfs/zfs"
cat > "$d/encfs/zfs-unlock" <<'EOF'
#!/bin/sh
echo "zfs-unlock $*" >> "${STUB_LOG:-/dev/null}"
exit 0
EOF
chmod +x "$d/encfs/zfs-unlock"
PATH="$d/encfs:$PATH" STUB_LOG="$d/log" STUB_ZFS_UNLOCK_SH="$d/encfs/zfs-unlock" \
    python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
print("unencrypted:", menu.ensure_key_loaded("zroot/ROOT/plain"))
print("encrypted-and-unlocked:", menu.ensure_key_loaded("zroot/ROOT/enc"))
PYEOF
if grep -q "^unencrypted: True$" "$d/out" && grep -q "^encrypted-and-unlocked: True$" "$d/out" \
   && grep -q "^zfs-unlock unlock zroot/ROOT/enc$" "$d/log" 2>/dev/null \
   && ! grep -q "^zfs-unlock unlock zroot/ROOT/plain$" "$d/log" 2>/dev/null; then
    ok "ensure_key_loaded() skips unencrypted datasets and shells out to zfs-unlock for encrypted ones"
else
    cat "$d/out"; cat "$d/log" 2>/dev/null; bad "ensure_key_loaded() did not behave as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: ensure_key_loaded() - already unlocked AND handoff ready never calls zfs-unlock again =="
d="$(fresh_env)"
mkdir -p "$d/encfs" "$d/root/tmp"
printf 'staged-secret' > "$d/root/tmp/zfs-key.zroot_ROOT_enc"
cat > "$d/encfs/zfs" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    get)
        prop="$5"; ds="$6"
        case "$ds:$prop" in
            "zroot/ROOT/enc:encryptionroot") echo "zroot/ROOT/enc" ;;
            "zroot/ROOT/enc:keystatus") echo "available" ;;
            *) echo "-" ;;
        esac
        ;;
esac
exit 0
EOF
chmod +x "$d/encfs/zfs"
cat > "$d/encfs/zfs-unlock" <<'EOF'
#!/bin/sh
echo "zfs-unlock $*" >> "${STUB_LOG:-/dev/null}"
exit 0
EOF
chmod +x "$d/encfs/zfs-unlock"
PATH="$d/encfs:$PATH" STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_ZFS_UNLOCK_SH="$d/encfs/zfs-unlock" \
    python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
print("already-ready:", menu.ensure_key_loaded("zroot/ROOT/enc"))
PYEOF
if grep -q "^already-ready: True$" "$d/out" && ! grep -q "^zfs-unlock" "$d/log" 2>/dev/null; then
    ok "ensure_key_loaded() is a no-op when already unlocked with a staged handoff secret - never reruns zfs-unlock"
else
    cat "$d/out"; cat "$d/log" 2>/dev/null; bad "ensure_key_loaded() re-ran zfs-unlock when it was already unlocked/ready"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: unlock_encrypted_root() offers to end the rescue-SSH session when already-ready, and Yes actually exits (SSH only) =="
# Issue #7, unidoc-alip's PR #11 review, F2: SSH_CONNECTION is now set
# explicitly - _offer_to_end_session() is gated on IS_SSH_SESSION (F1),
# so without this the Yes prompt would never even appear and this test
# would silently degenerate into testing the wrong thing depending on
# whether the runner happens to have SSH_* set.
d="$(fresh_env)"
mkdir -p "$d/encfs" "$d/root/tmp"
printf 'staged-secret' > "$d/root/tmp/zfs-key.zroot_ROOT_alpine"
cat > "$d/encfs/zfs" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    get)
        prop="$5"; ds="$6"
        case "$ds:$prop" in
            "zroot/ROOT/alpine:encryptionroot") echo "zroot/ROOT/alpine" ;;
            "zroot/ROOT/alpine:keystatus") echo "available" ;;
            *) echo "-" ;;
        esac
        ;;
esac
exit 0
EOF
chmod +x "$d/encfs/zfs"
env -u SSH_TTY SSH_CONNECTION="10.0.0.1 1 10.0.0.2 22" \
    PATH="$d/encfs:$PATH" STUB_LOG="$d/log" STUB_ROOT="$d/root" \
    python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu

yesno_calls = []
menu.dialog_yesno = lambda title, text, default_no=True: (yesno_calls.append((title, text, default_no)), True)[1]

try:
    menu.unlock_encrypted_root()
    print("returned normally - BUG, sys.exit() should have fired")
except SystemExit:
    print("SystemExit raised - correct")

title, text, default_no = yesno_calls[0]
print("dialog title:", title)
print("dialog default_no:", default_no)
print("dialog mentions READY:", "kexec handoff READY" in text)
print("dialog asks to end session:", "End this rescue SSH session now?" in text)
PYEOF
if grep -qx "SystemExit raised - correct" "$d/out" \
   && grep -qx "dialog default_no: True" "$d/out" \
   && grep -qx "dialog mentions READY: True" "$d/out" \
   && grep -qx "dialog asks to end session: True" "$d/out"; then
    ok "unlock_encrypted_root() offers Yes/No to end the session (default No) and Yes really exits (already-ready, SSH)"
else
    cat "$d/out"; bad "unlock_encrypted_root() did not offer to end the session as expected (already-ready, SSH)"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: unlock_encrypted_root() offers to end the session after UNLOCKING THIS RUN too, and Yes actually exits (SSH only) =="
# unidoc-alip's PR #11 review, F2: the test above only exercises the
# "already unlocked before this run" early return (lines ~1207-1209) -
# it never reaches the actual #7 scenario, the branch at ~1214-1216
# that runs AFTER a real subprocess.run([ZFS_UNLOCK_SH, ...]) call.
# keystatus starts "unavailable" and only flips to "available" once
# STUB_ZFS_UNLOCK_SH's own stub has actually run (mirroring how the
# real zfs-unlock changes keystatus) - proving this specific branch,
# not just the early-return one, offers the prompt and really exits.
# Asserting the stub was called is what makes this a different branch
# from the test above, not just the same assertions run twice.
d="$(fresh_env)"
mkdir -p "$d/encfs" "$d/root/tmp"
cat > "$d/encfs/zfs" <<EOF
#!/bin/sh
echo "zfs \$*" >> "\${STUB_LOG:-/dev/null}"
case "\$1" in
    get)
        prop="\$5"; ds="\$6"
        case "\$ds:\$prop" in
            "zroot/ROOT/alpine:encryptionroot") echo "zroot/ROOT/alpine" ;;
            "zroot/ROOT/alpine:keystatus")
                if [ -e "$d/marker" ]; then echo available; else echo unavailable; fi ;;
            *) echo "-" ;;
        esac
        ;;
esac
exit 0
EOF
chmod +x "$d/encfs/zfs"
cat > "$d/encfs/zfs-unlock" <<EOF
#!/bin/sh
echo "zfs-unlock \$*" >> "\${STUB_LOG:-/dev/null}"
: > "$d/marker"
printf 'staged-secret' > "$d/root/tmp/zfs-key.zroot_ROOT_alpine"
EOF
chmod +x "$d/encfs/zfs-unlock"
env -u SSH_TTY SSH_CONNECTION="10.0.0.1 1 10.0.0.2 22" \
    PATH="$d/encfs:$PATH" STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_ZFS_UNLOCK_SH="$d/encfs/zfs-unlock" \
    python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu

yesno_calls = []
menu.dialog_yesno = lambda title, text, default_no=True: (yesno_calls.append((title, text, default_no)), True)[1]

try:
    menu.unlock_encrypted_root()
    print("returned normally - BUG, sys.exit() should have fired")
except SystemExit:
    print("SystemExit raised - correct")

title, text, default_no = yesno_calls[0]
print("dialog title:", title)
print("dialog default_no:", default_no)
print("dialog mentions READY:", "kexec handoff READY" in text)
print("dialog asks to end session:", "End this rescue SSH session now?" in text)
PYEOF
if grep -qx "SystemExit raised - correct" "$d/out" \
   && grep -qx "dialog default_no: True" "$d/out" \
   && grep -qx "dialog mentions READY: True" "$d/out" \
   && grep -qx "dialog asks to end session: True" "$d/out" \
   && grep -q "^zfs-unlock unlock zroot/ROOT/alpine$" "$d/log" 2>/dev/null; then
    ok "unlock_encrypted_root() offers Yes/No and Yes really exits after unlocking THIS run (the actual #7 scenario)"
else
    cat "$d/out"; cat "$d/log" 2>/dev/null; bad "unlock_encrypted_root() did not offer to end the session after unlocking this run"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: unlock_encrypted_root() answering No returns to the main menu, exactly like the old plain dialog did (SSH) =="
d="$(fresh_env)"
mkdir -p "$d/encfs" "$d/root/tmp"
printf 'staged-secret' > "$d/root/tmp/zfs-key.zroot_ROOT_alpine"
cat > "$d/encfs/zfs" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    get)
        prop="$5"; ds="$6"
        case "$ds:$prop" in
            "zroot/ROOT/alpine:encryptionroot") echo "zroot/ROOT/alpine" ;;
            "zroot/ROOT/alpine:keystatus") echo "available" ;;
            *) echo "-" ;;
        esac
        ;;
esac
exit 0
EOF
chmod +x "$d/encfs/zfs"
env -u SSH_TTY SSH_CONNECTION="10.0.0.1 1 10.0.0.2 22" \
    PATH="$d/encfs:$PATH" STUB_LOG="$d/log" STUB_ROOT="$d/root" \
    python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
menu.dialog_yesno = lambda title, text, default_no=True: False
menu.unlock_encrypted_root()
print("returned normally - correct")
PYEOF
if grep -qx "returned normally - correct" "$d/out"; then
    ok "unlock_encrypted_root() answering No falls through and returns, same as before this feature"
else
    cat "$d/out"; bad "unlock_encrypted_root() did not return normally on No"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: unlock_encrypted_root() on the LOCAL CONSOLE never offers to end a session - old plain msgbox, no exit (F1 negative control) =="
# unidoc-alip's PR #11 review, F1 (must-fix): over SSH, menu.py IS the
# login shell (alpine-zfsboot-shell execs it), so exiting ends just
# that connection. On the local console menu.py is /init's own child -
# ANY exit other than the special 42 is /init's "menu.py missing or
# crashed" fallback, which kexecs straight into the default boot
# environment with no passphrase re-prompt (the handoff secret is
# already staged). A console operator must never be asked "end this
# rescue SSH session?" and must never have Yes actually exit the
# process - this is the direct regression test for that, with
# SSH_CONNECTION/SSH_TTY explicitly unset (not just "happens to be
# unset on this runner"). menu.dialog_yesno is stubbed to explode if
# called at all - it must never even be reached here.
d="$(fresh_env)"
mkdir -p "$d/encfs" "$d/root/tmp"
printf 'staged-secret' > "$d/root/tmp/zfs-key.zroot_ROOT_alpine"
cat > "$d/encfs/zfs" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    get)
        prop="$5"; ds="$6"
        case "$ds:$prop" in
            "zroot/ROOT/alpine:encryptionroot") echo "zroot/ROOT/alpine" ;;
            "zroot/ROOT/alpine:keystatus") echo "available" ;;
            *) echo "-" ;;
        esac
        ;;
esac
exit 0
EOF
chmod +x "$d/encfs/zfs"
env -u SSH_CONNECTION -u SSH_TTY \
    PATH="$d/encfs:$PATH" STUB_LOG="$d/log" STUB_ROOT="$d/root" \
    python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu

def _boom(*a, **kw):
    raise AssertionError("dialog_yesno must never be called on the local console")
menu.dialog_yesno = _boom

msgbox_calls = []
menu.dialog_msgbox = lambda title, text: msgbox_calls.append((title, text))

try:
    menu.unlock_encrypted_root()
    print("returned normally - correct")
except SystemExit as e:
    print(f"SystemExit({e.code!r}) raised - BUG, must never exit on the local console")
except AssertionError as e:
    print(f"AssertionError: {e}")

print("msgbox_call_count:", len(msgbox_calls))
if msgbox_calls:
    print("msgbox mentions READY:", "kexec handoff READY" in msgbox_calls[0][1])
    print("msgbox asks to end session:", "End this rescue SSH session now?" in msgbox_calls[0][1])
PYEOF
if grep -qx "returned normally - correct" "$d/out" \
   && grep -qx "msgbox_call_count: 1" "$d/out" \
   && grep -qx "msgbox mentions READY: True" "$d/out" \
   && grep -qx "msgbox asks to end session: False" "$d/out"; then
    ok "unlock_encrypted_root() on the local console shows the old plain msgbox and never offers/exits"
else
    cat "$d/out"; bad "unlock_encrypted_root() on the local console did not behave like the pre-#7 plain msgbox"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: deploy() refuses to zpool create over an already-imported pool =="
d="$(fresh_env)"
cat > "$d/stub-zpool" <<'EOF'
#!/bin/sh
echo "zpool $*" >> "${STUB_LOG:-/dev/null}"
[ "$1" = "list" ] && printf 'zroot\n'
exit 0
EOF
chmod +x "$d/stub-zpool"
mkdir -p "$d/collision"
cp "$d/stub-zpool" "$d/collision/zpool"
PATH="$d/collision:$PATH" STUB_LOG="$d/log" python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
# Stub dialog_inputbox/dialog_msgbox directly rather than driving the
# real `dialog` binary (not usable headless in a test harness anyway -
# it needs a real tty). Feed deploy()'s dialog_inputbox() calls in
# order: source, target pool (blank -> deploy()'s own `or "zroot"`
# fallback picks the already-imported "zroot"), vdev spec, target
# dataset (blank -> default), hostname (blank) - deploy() should
# refuse before ever reaching a confirmation/zpool create.
answers = iter([
    "backup01:zroot/images/alpine@golden",
    "",
    "/dev/sda",
    "",
    "",
])
menu.dialog_inputbox = lambda title, text, default="": next(answers)
menu.dialog_msgbox = lambda title, text: print(text)
menu.deploy()
PYEOF
if grep -q "already imported" "$d/out" && ! grep -q "^zpool create" "$d/log" 2>/dev/null; then
    ok "deploy() refuses the default pool name when it collides with an already-imported pool"
else
    cat "$d/out"; cat "$d/log" 2>/dev/null; bad "deploy() did not refuse the colliding pool name as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: deploy() always resets bootcheck state fresh on a newly received BE =="
# A received golden image's own attempt-count HISTORY must never carry
# over to this machine - real ZFS behavior confirmed this session:
# `zfs send -R` implies `-p` (properties), so the source's own
# org.alpinezfsboot:bootcheck value genuinely does arrive on the
# received dataset unless something explicitly overwrites it.
d="$(fresh_env)"
mkdir -p "$d/deploybin"
cat > "$d/deploybin/zpool" <<'EOF'
#!/bin/sh
echo "zpool $*" >> "${STUB_LOG:-/dev/null}"
[ "$1" = "list" ] && printf 'otherpool\n'
exit 0
EOF
cat > "$d/deploybin/zfs" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    send) exit 0 ;;
    recv) exit 0 ;;
    get) echo "-" ;;
esac
exit 0
EOF
cat > "$d/deploybin/mount" <<'EOF'
#!/bin/sh
echo "mount $*" >> "${STUB_LOG:-/dev/null}"
exit 1
EOF
cat > "$d/deploybin/zgenhostid" <<'EOF'
#!/bin/sh
exit 0
EOF
chmod +x "$d/deploybin/zpool" "$d/deploybin/zfs" "$d/deploybin/mount" "$d/deploybin/zgenhostid"
PATH="$d/deploybin:$PATH" STUB_LOG="$d/log" python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
# newpool doesn't collide with "otherpool" (the only already-imported
# pool the zpool stub reports) - deploy() proceeds all the way through
# zfs send/recv instead of refusing early, same as the real success path.
answers = iter([
    "zroot/images/alpine@golden",
    "newpool",
    "/dev/sdb",
    "",
    "",
])
menu.dialog_inputbox = lambda title, text, default="": next(answers)
menu.dialog_msgbox = lambda title, text: print(text)
menu.dialog_yesno = lambda *a, **k: True
menu.deploy()
PYEOF
if grep -q "^zfs set org.alpinezfsboot:bootcheck=armed:0 newpool/ROOT/alpine$" "$d/log" 2>/dev/null; then
    ok "deploy() explicitly resets bootcheck to armed:0 on the newly received BE"
else
    cat "$d/out"; cat "$d/log" 2>/dev/null; bad "deploy() did not reset bootcheck state on the received BE"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: deploy() always inherits (clears) any commandline override carried over from the golden image =="
# Regression test for a real gap a full source audit found, the same
# carryover class the bootcheck test above already covers: `zfs send -R`
# also carries org.alpinezfsboot:commandline if the golden image ever had
# one explicitly set (this project's own "edit cmdline & persist" menu
# action) - without an explicit reset here, a brand-new machine's very
# first real boot would silently inherit whatever cmdline override was
# set on some OTHER, unrelated machine's golden image, with nobody ever
# reviewing it.
d="$(fresh_env)"
mkdir -p "$d/deploybin"
cat > "$d/deploybin/zpool" <<'EOF'
#!/bin/sh
echo "zpool $*" >> "${STUB_LOG:-/dev/null}"
[ "$1" = "list" ] && printf 'otherpool\n'
exit 0
EOF
cat > "$d/deploybin/zfs" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    send) exit 0 ;;
    recv) exit 0 ;;
    get) echo "-" ;;
esac
exit 0
EOF
cat > "$d/deploybin/mount" <<'EOF'
#!/bin/sh
echo "mount $*" >> "${STUB_LOG:-/dev/null}"
exit 1
EOF
cat > "$d/deploybin/zgenhostid" <<'EOF'
#!/bin/sh
exit 0
EOF
chmod +x "$d/deploybin/zpool" "$d/deploybin/zfs" "$d/deploybin/mount" "$d/deploybin/zgenhostid"
PATH="$d/deploybin:$PATH" STUB_LOG="$d/log" python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
answers = iter([
    "zroot/images/alpine@golden",
    "newpool",
    "/dev/sdb",
    "",
    "",
])
menu.dialog_inputbox = lambda title, text, default="": next(answers)
menu.dialog_msgbox = lambda title, text: print(text)
menu.dialog_yesno = lambda *a, **k: True
menu.deploy()
PYEOF
if grep -q "^zfs inherit org.alpinezfsboot:commandline newpool/ROOT/alpine$" "$d/log" 2>/dev/null; then
    ok "deploy() explicitly inherits (clears) any commandline override carried over from the golden image"
else
    cat "$d/out"; cat "$d/log" 2>/dev/null; bad "deploy() did not clear a carried-over commandline override on the received BE"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: chroot_be() gives the chroot a fresh tmpfs, never bind-mounts this rescue session's own live /tmp =="
# Regression test for a real gap a full source audit found: this rescue
# session's own /tmp is exactly where zfs-unlock.sh stages a plaintext
# encryption passphrase, boot-dataset.sh's own boot/operation locks
# live, and rescue-ssh.sh's own pidfile lives - bind-mounting it
# straight into a chrooted BE meant anything running inside that
# chroot could read a currently-staged passphrase belonging to a
# completely different boot operation.
d="$(fresh_env)"
mkdir -p "$d/chrootbin"
cat > "$d/chrootbin/mkdir" <<'EOF'
#!/bin/sh
echo "mkdir $*" >> "${STUB_LOG:-/dev/null}"
exit 0
EOF
cat > "$d/chrootbin/mount" <<'EOF'
#!/bin/sh
echo "mount $*" >> "${STUB_LOG:-/dev/null}"
exit 0
EOF
cat > "$d/chrootbin/umount" <<'EOF'
#!/bin/sh
echo "umount $*" >> "${STUB_LOG:-/dev/null}"
exit 0
EOF
chmod +x "$d/chrootbin/mkdir" "$d/chrootbin/mount" "$d/chrootbin/umount"
PATH="$d/chrootbin:$PATH" STUB_LOG="$d/log" python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
menu.list_all_boot_environments = lambda: ["zroot/ROOT/alpine"]
menu.ensure_key_loaded = lambda dataset: True
menu.dialog_menu = lambda *a, **k: 0
menu.dialog_yesno = lambda *a, **k: False
menu.dialog_msgbox = lambda title, text: print(text)
menu._run_interactive = lambda argv: None
menu.chroot_be()
PYEOF
if grep -q "^mount -t tmpfs tmpfs /mnt/chroot/tmp$" "$d/log" 2>/dev/null \
   && ! grep -q -- "--bind /tmp /mnt/chroot/tmp" "$d/log" 2>/dev/null; then
    ok "chroot_be() mounts a fresh, empty tmpfs at the BE's own /tmp - never this rescue session's own live /tmp"
else
    cat "$d/out" 2>/dev/null; cat "$d/log" 2>/dev/null
    bad "chroot_be() did not isolate /tmp as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: cloning a boot environment copies bootcheck POLICY, never the origin's attempt HISTORY =="
d="$(fresh_env)"
mkdir -p "$d/clonebin"
cat > "$d/clonebin/zfs" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    clone) exit 0 ;;
    get)
        # get -H -o value,source org.alpinezfsboot:bootcheck DATASET -
        # only the ORIGIN (zroot/ROOT/alpine) is locally armed with a
        # real history; the new clone must never be asked about itself
        # here (this stub would return the same thing either way, but
        # the point under test is what the CODE does with the origin's
        # own value, not what a real clone would independently report).
        printf 'armed:5\tlocal\n'
        ;;
    set) exit 0 ;;
esac
exit 0
EOF
chmod +x "$d/clonebin/zfs"
PATH="$d/clonebin:$PATH" STUB_LOG="$d/log" python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
# _manage_one_be's own actions list, index 1 = Clone; then "Clone which
# snapshot?" (index 0); "New boot environment name" via dialog_inputbox.
menu_calls = iter([1, 0])
menu.dialog_menu = lambda *a, **k: next(menu_calls)
menu.dialog_inputbox = lambda title, text, default="": "newbe"
menu.dialog_msgbox = lambda title, text: print(text)
menu.zfs_list = lambda ds: [f"{ds}@snap1"]
menu._manage_one_be("zroot/ROOT/alpine")
PYEOF
if grep -q "^zfs set org.alpinezfsboot:bootcheck=armed:0 zroot/ROOT/newbe$" "$d/log" 2>/dev/null; then
    ok "clone copies the origin's armed POLICY as a fresh armed:0, never the origin's own armed:5 count"
else
    cat "$d/out"; cat "$d/log" 2>/dev/null; bad "clone did not reset bootcheck history as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: _network_status() parses a real busybox ifconfig eth0 address =="
d="$(fresh_env)"
mkdir -p "$d/net"
cat > "$d/net/ifconfig" <<'EOF'
#!/bin/sh
cat <<'OUT'
eth0      Link encap:Ethernet  HWaddr 52:54:00:12:34:56
          inet addr:10.0.2.15  Bcast:10.0.2.255  Mask:255.255.255.0
          UP BROADCAST RUNNING MULTICAST  MTU:1500  Metric:1
OUT
exit 0
EOF
chmod +x "$d/net/ifconfig"
PATH="$d/net:$PATH" python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
print("status:", menu._network_status())
PYEOF
if grep -q "status: 10.0.2.15" "$d/out"; then
    ok "_network_status() parses a real busybox 'inet addr:X.X.X.X' line"
else
    cat "$d/out"; bad "_network_status() did not parse the address as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: _network_status() returns None when eth0 has no address =="
d="$(fresh_env)"
mkdir -p "$d/net"
cat > "$d/net/ifconfig" <<'EOF'
#!/bin/sh
cat <<'OUT'
eth0      Link encap:Ethernet  HWaddr 52:54:00:12:34:56
          UP BROADCAST MULTICAST  MTU:1500  Metric:1
OUT
exit 0
EOF
chmod +x "$d/net/ifconfig"
PATH="$d/net:$PATH" python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
print("status:", menu._network_status())
PYEOF
if grep -q "status: None" "$d/out"; then
    ok "_network_status() returns None when eth0 has no inet address yet"
else
    cat "$d/out"; bad "_network_status() did not return None as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: _network_addresses() reports a global IPv6 address too, not just IPv4 =="
# Darius/Hetzner CAX is itself commonly an IPv6-only rescue case - the
# cockpit must not silently under-report a machine that is reachable.
d="$(fresh_env)"
mkdir -p "$d/net"
cat > "$d/net/ifconfig" <<'EOF'
#!/bin/sh
cat <<'OUT'
eth0      Link encap:Ethernet  HWaddr 52:54:00:12:34:56
          inet6 addr: fe80::5054:ff:fe12:3456/64 Scope:Link
          inet6 addr: 2001:db8::1234/64 Scope:Global
          UP BROADCAST RUNNING MULTICAST  MTU:1500  Metric:1
OUT
exit 0
EOF
chmod +x "$d/net/ifconfig"
PATH="$d/net:$PATH" python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
print("addrs:", menu._network_addresses())
PYEOF
if grep -q "addrs: \['2001:db8::1234'\]" "$d/out"; then
    ok "_network_addresses() reports the global IPv6 address, and skips the link-local one"
else
    cat "$d/out"; bad "_network_addresses() did not report the IPv6 address as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: _items()'s main-menu Network label shows an IPv6-only address too, not 'not connected' =="
# Regression test for a real gap a full source audit found: the main
# menu's own live "Network: ..." label (_items(), shown on every
# render, including the countdown) used _network_status() - the
# deliberately IPv4-only helper _network_addresses()'s own comment
# already documents as wrong for anything but the DHCP bring-up flow -
# so a machine reachable ONLY over IPv6 (the exact, documented,
# real-world Hetzner CAX rescue scenario) showed "Network: not
# connected" in the main menu the whole session, despite the operator
# being actively connected right now, over this exact interface, via
# the very SSH session showing them that label.
d="$(fresh_env)"
mkdir -p "$d/net"
cat > "$d/net/ifconfig" <<'EOF'
#!/bin/sh
cat <<'OUT'
eth0      Link encap:Ethernet  HWaddr 52:54:00:12:34:56
          inet6 addr: fe80::5054:ff:fe12:3456/64 Scope:Link
          inet6 addr: 2001:db8::1234/64 Scope:Global
          UP BROADCAST RUNNING MULTICAST  MTU:1500  Metric:1
OUT
exit 0
EOF
chmod +x "$d/net/ifconfig"
PATH="$d/net:$STUBS:$PATH" python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
items = menu._items()
print("network_label:", [i for i in items if i.startswith("Network:")][0])
PYEOF
if grep -qx "network_label: Network: 2001:db8::1234" "$d/out" 2>/dev/null; then
    ok "the main menu's Network label shows the real IPv6-only address, not 'not connected'"
else
    cat "$d/out"; bad "the main menu's Network label did not reflect IPv6-only connectivity as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: _probe_network_drivers() modprobes an unbound network-class PCI device by its modalias, skips everything else =="
d="$(fresh_env)"
pci="$d/root/sys/bus/pci/devices"
mkdir -p "$pci/0000:00:01.0" "$pci/0000:00:02.0" "$pci/0000:00:03.0"
# 0000:00:01.0 - a network-class device (0x020000, real Ethernet
# controller subclass) with no driver bound yet - the one case that
# should get modprobed.
printf '0x020000\n' > "$pci/0000:00:01.0/class"
printf 'r8169\n' > "$pci/0000:00:01.0/modalias"
# 0000:00:02.0 - also network-class, but ALREADY has a driver (the
# mere presence of this file/symlink is what matters, not its
# content) - must NOT be modprobed again.
printf '0x020000\n' > "$pci/0000:00:02.0/class"
printf 'e1000e\n' > "$pci/0000:00:02.0/modalias"
: > "$pci/0000:00:02.0/driver"
# 0000:00:03.0 - NOT network-class (e.g. a storage controller) - must
# be skipped regardless of its own driver/modalias state.
printf '0x010000\n' > "$pci/0000:00:03.0/class"
printf 'ahci\n' > "$pci/0000:00:03.0/modalias"
mkdir -p "$d/stub"
cat > "$d/stub/modprobe" <<'EOF'
#!/bin/sh
echo "modprobe $*" >> "${STUB_LOG:-/dev/null}"
exit 0
EOF
chmod +x "$d/stub/modprobe"
PATH="$d/stub:$PATH" STUB_ROOT="$d/root" STUB_LOG="$d/log" python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
menu._probe_network_drivers()
PYEOF
if grep -qx "modprobe r8169" "$d/log" 2>/dev/null \
   && ! grep -q "e1000e" "$d/log" \
   && ! grep -q "ahci" "$d/log"; then
    ok "_probe_network_drivers() modprobes only the unbound network-class device, by its real modalias"
else
    cat "$d/out"; cat "$d/log" 2>/dev/null
    bad "_probe_network_drivers() did not probe exactly the expected device"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: _sanitize_term() honors a resolvable \$TERM unchanged (dropbear's own value) =="
d="$(fresh_env)"
mkdir -p "$d/terminfo/x"
: > "$d/terminfo/x/xterm-256color"
STUB_TERMINFO_ROOT="$d/terminfo" TERM=xterm-256color python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
menu._sanitize_term()
print("TERM=" + menu.os.environ["TERM"])
PYEOF
if grep -qx "TERM=xterm-256color" "$d/out" && ! grep -q "falling back" "$d/out"; then
    ok "_sanitize_term() leaves a resolvable \$TERM untouched, no fallback message logged"
else
    cat "$d/out"; bad "_sanitize_term() did not honor a resolvable \$TERM"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: _sanitize_term() falls back to 'linux' for an unresolvable \$TERM on a non-SSH session =="
d="$(fresh_env)"
mkdir -p "$d/terminfo/x"
: > "$d/terminfo/x/xterm-256color"
env -u SSH_CONNECTION -u SSH_TTY STUB_TERMINFO_ROOT="$d/terminfo" TERM=bogus-alpine-test \
    python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
menu._sanitize_term()
print("TERM=" + menu.os.environ["TERM"])
PYEOF
if grep -q "TERM='bogus-alpine-test' has no bundled terminfo entry" "$d/out" && grep -qx "TERM=linux" "$d/out"; then
    ok "_sanitize_term() falls back to 'linux' (the local-console default) for an unresolvable \$TERM when not over SSH"
else
    cat "$d/out"; bad "_sanitize_term() did not fall back correctly for an unresolvable \$TERM"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: _sanitize_term() falls back to 'ansi' for an unresolvable \$TERM over SSH (IS_SSH_SESSION) =="
d="$(fresh_env)"
mkdir -p "$d/terminfo/x"
: > "$d/terminfo/x/xterm-256color"
STUB_TERMINFO_ROOT="$d/terminfo" TERM=bogus-alpine-test SSH_CONNECTION="10.0.0.1 1 10.0.0.2 22" \
    python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
menu._sanitize_term()
print("TERM=" + menu.os.environ["TERM"])
PYEOF
if grep -q "TERM='bogus-alpine-test' has no bundled terminfo entry" "$d/out" && grep -qx "TERM=ansi" "$d/out"; then
    ok "_sanitize_term() falls back to 'ansi' (never 'linux' - a Linux-vt-specific control sequence set) for an unresolvable \$TERM over SSH"
else
    cat "$d/out"; bad "_sanitize_term() did not fall back correctly over SSH"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: _dialog() distinguishes a genuine dialog init failure from a plain Cancel/ESC =="
d="$(fresh_env)"
mkdir -p "$d/stub"
# 255 is dialog's own real exit code for BOTH ESC and a genuine internal
# failure (see _dialog()'s own comment) - STUB_DIALOG_MODE picks which
# of the two this fake binary simulates.
cat > "$d/stub/dialog" <<'EOF'
#!/bin/sh
case "${STUB_DIALOG_MODE:-cancel}" in
    cancel) exit 255 ;;  # empty stderr - a real ESC
    crash)  echo "Error opening terminal: unknown." >&2; exit 255 ;;
esac
EOF
chmod +x "$d/stub/dialog"
PATH="$d/stub:$PATH" STUB_DIALOG_MODE=cancel python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out.cancel" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
print("result:", menu.dialog_msgbox("t", "text"))
print("no exception raised")
PYEOF
PATH="$d/stub:$PATH" STUB_DIALOG_MODE=crash python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out.crash" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu
try:
    menu.dialog_msgbox("t", "text")
    print("no exception raised")
except menu.DialogUnavailable as e:
    print("DialogUnavailable raised:", e)
PYEOF
if grep -q "no exception raised" "$d/out.cancel" && grep -q "^DialogUnavailable raised: Error opening terminal: unknown.$" "$d/out.crash"; then
    ok "_dialog() raises DialogUnavailable only on a genuine init failure, not on a plain ESC/Cancel"
else
    echo "--- cancel ---"; cat "$d/out.cancel"
    echo "--- crash ---"; cat "$d/out.crash"
    bad "_dialog() did not distinguish a genuine failure from Cancel/ESC correctly"
fi
rm -rf "$d"

# =============================================================================
echo "== menu.py: _cancellable() turns a KeyboardInterrupt into a clean return-to-menu, not a traceback =="
d="$(fresh_env)"
python3 - "$REPO_ROOT/init" <<'PYEOF' >"$d/out" 2>&1 || true
import sys
sys.path.insert(0, sys.argv[1])
import menu

def boom():
    raise KeyboardInterrupt

print("result:", menu._cancellable(boom))
print("still running after Ctrl-C")
PYEOF
if grep -q "cancelled (Ctrl-C)" "$d/out" && grep -qx "result: None" "$d/out" && grep -q "still running after Ctrl-C" "$d/out"; then
    ok "_cancellable() catches KeyboardInterrupt, prints a clean message, and control returns normally"
else
    cat "$d/out"; bad "_cancellable() did not handle KeyboardInterrupt as expected"
fi
rm -rf "$d"

# NOTE on what the next two tests can and cannot prove: run_stubbed()'s
# dot-sourcing subshell never has a controlling terminal of its own
# (stdin/stdout/stderr are all redirected to plain files, never a real
# pty), so /dev/tty never resolves there - both tests below therefore
# always land in _recovery_shell()'s "no controlling terminal yet"
# branch, never the "already has a ctty" one. That's the correct,
# expected outcome for THIS harness, and is exactly what's asserted -
# but it means neither branch's own real setsid()/TIOCSCTTY/fork+wait
# behavior is, or can be, exercised here at all. Only real hardware (a
# real kernel, a real PID 1, a real pty - see the CAX acceptance matrix)
# can prove those actually work; these tests only prove the DECISION
# LOGIC picks the right branch and that reaching it genuinely stops
# script execution, which is the part a mock CAN prove.

# =============================================================================
echo "== init: die() with authorized_keys/host key staged -> _recovery_shell() reached, correct branch logged =="
# STUB_BOOTFS="-" is this suite's own existing way to force a real
# die() call (see the earlier identical-technique test above).
d="$(fresh_env)"
stage_rescue_ssh "$d/root"
printf 'root=ZFS=zroot/ROOT/alpine ro alpine-zfsboot.timeout=0\n' > "$d/root/proc/cmdline"
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_BOOTFS="-" \
    run_stubbed "$REPO_ROOT/init/init" >"$d/out" 2>&1 || true
if grep -q "TEST STUB: would exec setsid -c /bin/bash (no controlling terminal yet)" "$d/out"; then
    ok "die() reaches _recovery_shell(), correctly picks the no-ctty branch under the non-interactive test harness"
else
    cat "$d/out"; bad "die() did not reach _recovery_shell()'s expected decision branch"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: fail() (missing kernel) reaches _recovery_shell(), correct branch logged, script stops there =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"  # deliberately no vmlinuz-* -> fail() via the missing-kernel path
STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" \
    run_stubbed "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/empty" "zroot" >"$d/out" 2>&1 || true
if grep -q "TEST STUB: would exec setsid -c /bin/bash (no controlling terminal yet)" "$d/out" \
   && ! grep -q "^kexec -l" "$d/log" 2>/dev/null; then
    ok "fail() reaches _recovery_shell() and the script genuinely stops there - no kexec ever attempted afterward"
else
    cat "$d/out"; cat "$d/log" 2>/dev/null
    bad "fail()/_recovery_shell() did not stop script execution as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== boot-dataset.sh: passphrase prompt - dialog init failure (bad \$TERM) stops immediately, does not burn all retries =="
d="$(fresh_env)"
mkdir -p "$d/pooldata/boot"
: > "$d/pooldata/boot/vmlinuz-lts"; : > "$d/pooldata/boot/initramfs-lts"
cat > "$STUBS/zfs.dialogcrash" <<'EOF'
#!/bin/sh
echo "zfs $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    get)
        prop="$5"
        case "$prop" in
            encryptionroot) echo "zroot/ROOT/alpine" ;;
            keystatus) echo "unavailable" ;;
            keylocation) echo "prompt" ;;
            org.alpinezfsboot:commandline) echo "-" ;;
        esac
        ;;
    load-key) exit 1 ;;
esac
exit 0
EOF
chmod +x "$STUBS/zfs.dialogcrash"
(
    set +e
    mount()    { "$STUBS/mount" "$@"; }
    umount()   { "$STUBS/umount" "$@"; }
    zpool()    { "$STUBS/zpool" "$@"; }
    zfs()      { "$STUBS/zfs.dialogcrash" "$@"; }
    kexec()    { "$STUBS/kexec" "$@"; }
    # Simulates dialog's own real behavior on an unresolvable $TERM -
    # exit 255 (the SAME code as a plain ESC - see _dialog()'s own
    # comment in menu.py) but with a genuine, non-empty error message on
    # stderr, which is the actual, confirmed discriminator.
    dialog()   { echo "Error opening terminal: unknown." >&2; return 255; }
    stty()     { :; }
    clear()    { :; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root" STUB_POOL_DATA="$d/pooldata" STUB_TTY="$d/tty"
    export STUB_LOG STUB_ROOT STUB_POOL_DATA STUB_TTY
    . "$REPO_ROOT/init/boot-dataset.sh" "zroot/ROOT/alpine" "zroot"
) >"$d/out" 2>&1 || true
attempts=$(grep -c "^zfs load-key zroot/ROOT/alpine$" "$d/log" 2>/dev/null || true)
if [ "$attempts" = 0 ] && grep -q "dialog failed to prompt for the zroot/ROOT/alpine passphrase: Error opening terminal: unknown." "$d/out"; then
    ok "a genuine dialog init failure stops immediately (0 load-key attempts), with a clear diagnostic - no wasted retries"
else
    cat "$d/out"; cat "$d/log" 2>/dev/null || true
    bad "dialog init failure during passphrase prompt was not handled as expected (attempts=$attempts)"
fi
rm -f "$STUBS/zfs.dialogcrash"
rm -rf "$d"

# zfs_unlock_env D - the shared boilerplate for the zfs-unlock.sh tests
# below: a fresh scratch root, keystatus state file (default LOCKED),
# and the stub $STUBS/zfs.unlock wired up via env vars (see that stub's
# own header comment). Callers still set STUB_ZFS_UNLOCK_CORRECT/BUSY
# themselves before sourcing zfs-unlock.sh, and reset the state file
# between phases of a single test as needed.
zfs_unlock_env() {
    d="$1"
    echo "unavailable" > "$d/state"
    STUB_ZFS_UNLOCK_STATE="$d/state"
    STUB_ZFS_UNLOCK_ROOT="zroot/ROOT/enc"
    STUB_TTY="$d/tty"
    export STUB_ZFS_UNLOCK_STATE STUB_ZFS_UNLOCK_ROOT STUB_TTY
}

# =============================================================================
echo "== zfs-unlock.sh: zfs_unlock() on a LOCKED dataset - correct passphrase unlocks and stages a handoff secret =="
d="$(fresh_env)"
zfs_unlock_env "$d"
STUB_ZFS_UNLOCK_CORRECT="hunter2"
export STUB_ZFS_UNLOCK_CORRECT
(
    set +e
    zfs()    { "$STUBS/zfs.unlock" "$@"; }
    dialog() { echo "hunter2" >&2; return 0; }
    stty()   { :; }
    clear()  { :; }
    STUB_ROOT="$d/root"
    STUB_LOG="$d/log"
    export STUB_ROOT STUB_LOG
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_unlock "zroot/ROOT/enc"
    echo "unlock_status=$?"
    zfs_handoff_ready "zroot/ROOT/enc" && echo "handoff=ready" || echo "handoff=not-ready"
) >"$d/out" 2>&1 || true
staged="$(cat "$d/root/tmp/zfs-key.zroot_ROOT_enc" 2>/dev/null)" || true
if grep -qx "unlock_status=0" "$d/out" && grep -qx "handoff=ready" "$d/out" \
   && [ "$staged" = "hunter2" ]; then
    ok "zfs_unlock() on a locked dataset: correct passphrase unlocks it AND stages the exact typed passphrase for kexec handoff"
else
    cat "$d/out"; echo "staged=[$staged]"
    bad "zfs_unlock() did not unlock+stage as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: load-key succeeds but staging fails - final state is UNLOCKED/NOT-READY, never a false 'staged' claim =="
# The Rolls-Royce audit's own remaining correctness finding: neither
# the initial unlock nor the reacquire path may claim the handoff was
# staged unless zfs_stage_secret() actually reported success. Stubbing
# `mv` to always fail makes the PUBLISH step of every zfs_stage_secret()
# call fail (the write itself still succeeds) while load-key/
# load-key -n keep succeeding via the zfs stub, and - importantly -
# while the per-encryptionroot operation lock's own `mkdir` (a
# different call, unaffected by this) still succeeds normally. A
# directory-permission-based approach was tried first and rejected:
# the op-lock directory lives in the same $ROOTFS/tmp as the staged
# secret, so making that whole directory read-only also blocks the
# lock's own mkdir, which isn't the failure this test is after.
d="$(fresh_env)"
zfs_unlock_env "$d"
STUB_ZFS_UNLOCK_CORRECT="hunter2"
export STUB_ZFS_UNLOCK_CORRECT
(
    set +e
    zfs()    { "$STUBS/zfs.unlock" "$@"; }
    dialog() { echo "hunter2" >&2; return 0; }
    stty()   { :; }
    clear()  { :; }
    mv()     { return 1; }
    STUB_ROOT="$d/root"
    STUB_LOG="$d/log"
    export STUB_ROOT STUB_LOG
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_unlock "zroot/ROOT/enc"
    echo "unlock_status=$?"
    zfs_handoff_ready "zroot/ROOT/enc" && echo "handoff=ready-BUG" || echo "handoff=not-ready-correct"
) >"$d/out" 2>&1 || true
if grep -qx "unlock_status=0" "$d/out" && grep -qx "handoff=not-ready-correct" "$d/out" \
   && grep -qi "staging the kexec handoff secret failed" "$d/out" \
   && ! grep -qi "reacquired and staged" "$d/out" \
   && [ ! -e "$d/root/tmp/zfs-key.zroot_ROOT_enc" ] \
   && [ "$(cat "$d/state" 2>/dev/null)" = "available" ]; then
    ok "a staging failure never claims 'staged' - key stays genuinely unlocked, handoff correctly reports NOT READY"
else
    cat "$d/out"; ls "$d/root/tmp" 2>/dev/null
    bad "a staging failure was not reported/handled as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: zfs_unlock() -> zfs_lock() -> LOCKED again, staged secret removed, keystatus verified unavailable =="
d="$(fresh_env)"
zfs_unlock_env "$d"
STUB_ZFS_UNLOCK_CORRECT="hunter2"
export STUB_ZFS_UNLOCK_CORRECT
(
    set +e
    zfs()    { "$STUBS/zfs.unlock" "$@"; }
    dialog() { echo "hunter2" >&2; return 0; }
    stty()   { :; }
    clear()  { :; }
    STUB_ROOT="$d/root"
    STUB_LOG="$d/log"
    export STUB_ROOT STUB_LOG
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_unlock "zroot/ROOT/enc"
    zfs_lock "zroot/ROOT/enc"
    echo "lock_status=$?"
    zfs_handoff_ready "zroot/ROOT/enc" && echo "handoff=ready" || echo "handoff=not-ready"
) >"$d/out" 2>&1 || true
if grep -qx "lock_status=0" "$d/out" && grep -qx "handoff=not-ready" "$d/out" \
   && [ "$(cat "$d/state")" = "unavailable" ] \
   && [ ! -e "$d/root/tmp/zfs-key.zroot_ROOT_enc" ]; then
    ok "zfs_lock() after an unlock: keystatus verified unavailable AND the staged secret is gone - never LOCKED with a leftover plaintext secret"
else
    cat "$d/out"; echo "state=$(cat "$d/state" 2>/dev/null)"
    ls "$d/root/tmp" 2>/dev/null
    bad "zfs_lock() did not fully re-lock as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: zfs_unlock() on an ALREADY-unlocked dataset with no staged secret reacquires via a dry-run verify =="
# Simulates the exact reported gap: something else (a manual 'zfs
# load-key' over the rescue shell, menu.py's OWN prior behavior before
# this fix) unlocked the dataset without ever staging a handoff secret.
d="$(fresh_env)"
zfs_unlock_env "$d"
echo "available" > "$d/state"
STUB_ZFS_UNLOCK_CORRECT="hunter2"
export STUB_ZFS_UNLOCK_CORRECT
(
    set +e
    zfs()    { "$STUBS/zfs.unlock" "$@"; }
    dialog() { echo "hunter2" >&2; return 0; }
    stty()   { :; }
    clear()  { :; }
    STUB_ROOT="$d/root"
    STUB_LOG="$d/log"
    export STUB_ROOT STUB_LOG
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_unlock "zroot/ROOT/enc"
    echo "unlock_status=$?"
    zfs_handoff_ready "zroot/ROOT/enc" && echo "handoff=ready" || echo "handoff=not-ready"
) >"$d/out" 2>&1 || true
staged="$(cat "$d/root/tmp/zfs-key.zroot_ROOT_enc" 2>/dev/null)" || true
if grep -qx "unlock_status=0" "$d/out" && grep -qx "handoff=ready" "$d/out" \
   && [ "$staged" = "hunter2" ] && grep -q "^zfs load-key -n zroot/ROOT/enc$" "$d/log" 2>/dev/null; then
    ok "zfs_unlock() reacquires a verified passphrase (via dry-run load-key -n, never disturbing the already-loaded key) and stages it"
else
    cat "$d/out"; echo "staged=[$staged]"; cat "$d/log" 2>/dev/null || true
    bad "zfs_unlock() did not correctly reacquire the handoff secret for an already-unlocked dataset"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: zfs_unlock() reacquire with the WRONG passphrase never stages anything, still reports unlocked =="
d="$(fresh_env)"
zfs_unlock_env "$d"
echo "available" > "$d/state"
STUB_ZFS_UNLOCK_CORRECT="hunter2"
export STUB_ZFS_UNLOCK_CORRECT
(
    set +e
    zfs()    { "$STUBS/zfs.unlock" "$@"; }
    dialog() { echo "wrong-guess" >&2; return 0; }
    stty()   { :; }
    clear()  { :; }
    STUB_ROOT="$d/root"
    STUB_LOG="$d/log"
    export STUB_ROOT STUB_LOG
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_unlock "zroot/ROOT/enc"
    echo "unlock_status=$?"
    zfs_handoff_ready "zroot/ROOT/enc" && echo "handoff=ready" || echo "handoff=not-ready"
) >"$d/out" 2>&1 || true
if grep -qx "unlock_status=0" "$d/out" && grep -qx "handoff=not-ready" "$d/out" \
   && [ ! -e "$d/root/tmp/zfs-key.zroot_ROOT_enc" ] && [ "$(cat "$d/state")" = "available" ]; then
    ok "a wrong reacquire-guess never stages a wrong secret, and does NOT disturb the already-unlocked key (still available)"
else
    cat "$d/out"
    bad "wrong-passphrase reacquire was not handled safely"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: zfs_unlock() on an already-unlocked NON-PROMPT (file/https) dataset never stages a bogus secret =="
# Regression test for a real gap the full 8-way source audit found (not
# caught by this project's own earlier hardening pass): the branch
# selector used to be `[ "$keystatus" != "available" ] && [ "$keylocation"
# != "prompt" ]` - requiring BOTH conditions to enter the "no human
# involved" branch. An already-UNLOCKED dataset (keystatus=available)
# with a non-prompt keylocation (file://, https://) fell through that
# exclusion into the INTERACTIVE path instead: verify_only became 1, a
# human got prompted for a "passphrase" that means nothing for this
# keylocation class, and `zfs load-key -n` with no `-L` override reads
# the dataset's OWN real keylocation, silently ignoring stdin entirely -
# so that check "succeeds" regardless of what was typed, and the
# OPERATOR'S TYPED TEXT then got staged as the "verified" kexec handoff
# secret. Simulated here via the zfs.unlock stub's own new
# STUB_ZFS_UNLOCK_KEYLOCATION toggle (models real `-n`-without-`-L`
# semantics: succeeds unconditionally, stdin genuinely unused) and a
# dialog stub that "types" a value that is NOT the dataset's real key,
# proving the fix never treats that as verified.
d="$(fresh_env)"
zfs_unlock_env "$d"
echo "available" > "$d/state"
STUB_ZFS_UNLOCK_KEYLOCATION="file:///etc/zfs/keys/enc.key"
export STUB_ZFS_UNLOCK_KEYLOCATION
(
    set +e
    zfs()    { "$STUBS/zfs.unlock" "$@"; }
    dialog() { echo "some-typed-garbage-not-the-real-key" >&2; return 0; }
    stty()   { :; }
    clear()  { :; }
    STUB_ROOT="$d/root"
    STUB_LOG="$d/log"
    export STUB_ROOT STUB_LOG
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_unlock "zroot/ROOT/enc"
    echo "unlock_status=$?"
    zfs_handoff_ready "zroot/ROOT/enc" && echo "handoff=ready" || echo "handoff=not-ready"
) >"$d/out" 2>&1 || true
unset STUB_ZFS_UNLOCK_KEYLOCATION
if grep -qx "unlock_status=0" "$d/out" && grep -qx "handoff=not-ready" "$d/out" \
   && [ ! -e "$d/root/tmp/zfs-key.zroot_ROOT_enc" ] \
   && grep -qi "no passphrase to reacquire" "$d/out" \
   && ! grep -q "^dialog " "$d/log" 2>/dev/null; then
    ok "an already-unlocked non-prompt (file/https) dataset never stages the operator's typed text as a bogus secret - gives up gracefully instead"
else
    cat "$d/out"; ls "$d/root/tmp" 2>/dev/null
    bad "an already-unlocked non-prompt dataset staged a bogus secret instead of giving up gracefully"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: zfs_lock() refuses cleanly when a descendant is mounted - no unmount, no unload-key, secret survives =="
d="$(fresh_env)"
zfs_unlock_env "$d"
echo "available" > "$d/state"
mkdir -p "$d/root/tmp"
printf 'hunter2' > "$d/root/tmp/zfs-key.zroot_ROOT_enc"
STUB_ZFS_UNLOCK_BUSY="zroot/ROOT/enc/child"
export STUB_ZFS_UNLOCK_BUSY
(
    set +e
    zfs() { "$STUBS/zfs.unlock" "$@"; }
    STUB_ROOT="$d/root"
    STUB_LOG="$d/log"
    export STUB_ROOT STUB_LOG
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_lock "zroot/ROOT/enc"
    echo "lock_status=$?"
) >"$d/out" 2>&1 || true
if grep -q "lock_status=1" "$d/out" && grep -qi "still mounted" "$d/out" \
   && ! grep -q "^umount" "$d/log" 2>/dev/null && ! grep -q "^zfs unload-key" "$d/log" 2>/dev/null \
   && [ "$(cat "$d/state")" = "available" ] \
   && [ "$(cat "$d/root/tmp/zfs-key.zroot_ROOT_enc" 2>/dev/null)" = "hunter2" ]; then
    ok "zfs_lock() refuses when busy - no unmount attempted, key stays loaded, staged secret survives untouched"
else
    cat "$d/out"; cat "$d/log" 2>/dev/null || true
    bad "zfs_lock() did not refuse the busy case safely"
fi
rm -rf "$d"
unset STUB_ZFS_UNLOCK_BUSY

# =============================================================================
echo "== zfs-unlock.sh: zfs_lock() with an unknown/failed keystatus read still attempts a REAL unload-key, never assumes already-locked =="
# Regression test for a real gap a full source audit found:
# _zfs_lock_locked() used to check `[ "$keystatus" != "available" ]` to
# decide "already locked, nothing to do" - which also silently matched
# an EMPTY string (exactly what a real `zfs get` failure/transient
# error produces via `$(... 2>/dev/null)`), skipping the actual `zfs
# unload-key` call entirely and reporting success even though the key
# could still be fully loaded. Simulated here by leaving the state
# file EMPTY (neither "available" nor "unavailable") rather than a
# real keystatus value - the fixed code must fall through to a real
# unload-key attempt rather than short-circuiting.
d="$(fresh_env)"
zfs_unlock_env "$d"
: > "$d/state"
(
    set +e
    zfs() { "$STUBS/zfs.unlock" "$@"; }
    STUB_ROOT="$d/root"
    STUB_LOG="$d/log"
    export STUB_ROOT STUB_LOG
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_lock "zroot/ROOT/enc"
    echo "lock_status=$?"
) >"$d/out" 2>&1 || true
if grep -qx "lock_status=0" "$d/out" \
   && grep -q "^zfs unload-key zroot/ROOT/enc$" "$d/log" 2>/dev/null \
   && [ "$(cat "$d/state")" = "unavailable" ]; then
    ok "an unknown/failed keystatus read is never treated as confirmed-already-locked - a real unload-key is attempted and actually locks the dataset"
else
    cat "$d/out"; echo "log:"; cat "$d/log" 2>/dev/null || true; echo "state=[$(cat "$d/state" 2>/dev/null)]"
    bad "zfs_lock() short-circuited on an unknown keystatus instead of attempting a real unload-key"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: zfs_lock() authoritatively checks a clone's OWN encryptionroot property, not just descendant-of-namespace =="
# The Rolls-Royce audit's own finding: OpenZFS explicitly allows a clone
# to live ANYWHERE in the pool, sharing its origin's encryption key
# without being a ZFS descendant of it. STUB_ZFS_UNLOCK_BUSY here names
# a dataset the stub reports as sharing $ROOT's own encryptionroot
# property (see tests/stubs/zfs.unlock's own comment) - this test's own
# point is simply that the busy check is driven by that property, not
# by name-prefix membership, so this is really exercising the same
# mechanism as the test above from a different angle: the enumeration
# walks the whole pool and asks ZFS itself, rather than assuming
# "shares the key" and "is a descendant" are the same thing.
d="$(fresh_env)"
zfs_unlock_env "$d"
echo "available" > "$d/state"
mkdir -p "$d/root/tmp"
printf 'hunter2' > "$d/root/tmp/zfs-key.zroot_ROOT_enc"
STUB_ZFS_UNLOCK_BUSY="zroot/elsewhere/clone"
export STUB_ZFS_UNLOCK_BUSY
(
    set +e
    zfs() { "$STUBS/zfs.unlock" "$@"; }
    STUB_ROOT="$d/root"
    STUB_LOG="$d/log"
    export STUB_ROOT STUB_LOG
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_lock "zroot/ROOT/enc"
    echo "lock_status=$?"
) >"$d/out" 2>&1 || true
if grep -q "lock_status=1" "$d/out" && grep -qi "still mounted" "$d/out" \
   && ! grep -q "^zfs unload-key" "$d/log" 2>/dev/null; then
    ok "zfs_lock() refuses for a clone sharing the key from OUTSIDE the encryption root's own namespace, not just a namespace descendant"
else
    cat "$d/out"; cat "$d/log" 2>/dev/null || true
    bad "zfs_lock() did not check the clone's actual encryptionroot property as expected"
fi
rm -rf "$d"
unset STUB_ZFS_UNLOCK_BUSY

# =============================================================================
echo "== zfs-unlock.sh: zfs_stage_secret() publishes atomically - an orphaned tmp file is never mistaken for READY =="
d="$(fresh_env)"
(
    set +e
    STUB_ROOT="$d/root"
    export STUB_ROOT
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    stage="$(zfs_key_stage_path "zroot/ROOT/enc")"
    # Simulates a writer killed/interrupted before ever renaming: an
    # orphaned temporary file exists, but nothing was ever published at
    # the canonical path. handoff_ready() must only ever look at the
    # canonical path, never a same-directory .tmp file.
    printf 'partial-corrupt-nu' > "$stage.tmp.99999"
    zfs_handoff_ready "zroot/ROOT/enc" && echo "handoff=ready-BUG" || echo "handoff=not-ready-correct"
) >"$d/out" 2>&1 || true
if grep -qx "handoff=not-ready-correct" "$d/out"; then
    ok "zfs_stage_secret()'s own orphaned temp file is never mistaken for a published secret"
else
    cat "$d/out"; bad "an orphaned temp file was incorrectly treated as READY"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: zfs_stage_secret() publishes atomically - a write that fails partway never destroys a previously GOOD staged secret =="
# The actual bug atomic publication fixes: writing straight to the
# canonical path truncates it (via '>') BEFORE the write itself can
# fail - so a write that fails partway through (disk full, a size
# limit, any I/O error after open()) would destroy an already-good,
# already-staged secret, not just fail to add a new one. `ulimit -f 0`
# reproduces exactly that shape of failure deterministically: any
# attempt to write a non-empty file is killed by SIGXFSZ - confirmed
# directly against both the old direct-write implementation (destroys
# the existing secret, leaves the canonical path empty) and the current
# temp-file+rename one (leaves the existing secret completely
# untouched, since the write happens on a fresh temp file and the
# rename that would replace the canonical path is never reached).
# `ulimit -c 0` first so the SIGXFSZ kill doesn't also leave a core
# file behind in the scratch dir.
d="$(fresh_env)"
(
    set +e
    STUB_ROOT="$d/root"
    export STUB_ROOT
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    stage="$(zfs_key_stage_path "zroot/ROOT/enc")"
    zfs_stage_secret "zroot/ROOT/enc" "old-good-secret"
    (
        ulimit -c 0
        ulimit -f 0
        zfs_stage_secret "zroot/ROOT/enc" "new-secret-that-should-never-land"
        echo "second_stage_status=$?"
    )
    echo "content-after=[$(cat "$stage" 2>/dev/null)]"
    zfs_handoff_ready "zroot/ROOT/enc" && echo "handoff=ready-correct" || echo "handoff=NOT-ready-BUG"
) >"$d/out" 2>&1 || true
if grep -Fqx "content-after=[old-good-secret]" "$d/out" && grep -qx "handoff=ready-correct" "$d/out"; then
    ok "a write that fails partway through never destroys a previously published, still-good staged secret"
else
    cat "$d/out"; bad "a failed re-publish corrupted or lost a previously good staged secret"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: N3 - zfs_stage_secret() sets staged_by_me BEFORE calling mv, not after the whole function returns =="
# Regression test for N3 (unidoc-alip's PR #5 follow-up review): `mv` is
# an external command - a signal landing while the calling shell merely
# WAITS on it can have the rename already genuinely complete with
# staged_by_me still 0, if that flag is only set by the CALLER after
# zfs_stage_secret's own return (the old shape). boot-dataset.sh's own
# EXIT/INT/TERM/HUP trap (_cleanup_secrets) then sees staged_by_me=0
# and leaves a real plaintext secret sitting in tmpfs for the rest of
# the rescue session - exactly the exposure window this flag exists to
# close in the first place (F2). Proven directly, not by racing a real
# signal against a real timing window (unreliable by nature) - a `mv`
# override captures staged_by_me's own value at the exact moment it is
# invoked, before doing the real rename itself, so this test is
# deterministic: it fails every time against the old "set it after the
# call returns" shape and passes every time against the fix.
d="$(fresh_env)"
mv_capture="$d/mv-capture"
(
    set +e
    STUB_ROOT="$d/root"
    export STUB_ROOT
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    mv() {
        echo "staged_by_me-at-mv-time=${staged_by_me:-unset}" > "$mv_capture"
        command mv "$@"
    }
    zfs_stage_secret "zroot/ROOT/enc" "a-real-secret"
    echo "staged_by_me-after-return=${staged_by_me:-unset}"
) >"$d/out" 2>&1 || true
if grep -qx "staged_by_me-at-mv-time=1" "$mv_capture" 2>/dev/null \
   && grep -qx "staged_by_me-after-return=1" "$d/out"; then
    ok "staged_by_me is already 1 at the exact moment mv is invoked, not only after zfs_stage_secret returns"
else
    cat "$mv_capture" 2>/dev/null; cat "$d/out"
    bad "staged_by_me was not set before the rename - a signal during a real mv could leave a staged secret with the cleanup flag still unset"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: N3 - a failed rename resets staged_by_me to 0, never claiming ownership of a stage this process didn't actually publish =="
# The other half of N3's fix: setting the flag optimistically before
# the rename would be unsafe on its own if a FAILED mv left it stuck at
# 1 - cleanup would then delete whatever is sitting at the canonical
# stage path even though this process never actually published
# anything there, which could be a DIFFERENT session's own real,
# in-use secret (the exact cross-session deletion F2 already closed
# once, for a different code path).
d="$(fresh_env)"
(
    set +e
    STUB_ROOT="$d/root"
    export STUB_ROOT
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    mv() { return 1; }
    zfs_stage_secret "zroot/ROOT/enc" "a-real-secret"
    echo "stage-status=$?"
    echo "staged_by_me-after-failed-rename=${staged_by_me:-unset}"
) >"$d/out" 2>&1 || true
if grep -qx "stage-status=1" "$d/out" && grep -qx "staged_by_me-after-failed-rename=0" "$d/out"; then
    ok "a failed rename resets staged_by_me to 0 - this process never claims ownership of a publish that didn't actually happen"
else
    cat "$d/out"; bad "staged_by_me was left at 1 after a failed rename - cleanup could delete another session's real stage"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: a passphrase prompt fails CLOSED when terminal echo suppression cannot be established =="
# The Rolls-Royce audit's own required invariant: never accept a
# passphrase without a confirmed-working echo-suppression/restore
# guarantee. STUB_TTY points at a path that does not exist at all here
# (unlike every other test's STUB_TTY, which points at a real openable
# file) - simulating the real ENXIO/ENOTTY a genuinely missing
# controlling terminal would produce. Zero load-key attempts is the
# proof this refuses BEFORE ever showing dialog or touching zfs, not
# just "eventually reports failure".
d="$(fresh_env)"
zfs_unlock_env "$d"
STUB_TTY="$d/no-such-tty"
export STUB_TTY
(
    set +e
    zfs()    { "$STUBS/zfs.unlock" "$@"; }
    dialog() { echo "hunter2" >&2; return 0; }
    clear()  { :; }
    STUB_ROOT="$d/root"
    STUB_LOG="$d/log"
    export STUB_ROOT STUB_LOG
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_unlock "zroot/ROOT/enc"
    echo "unlock_status=$?"
) >"$d/out" 2>&1 || true
attempts=$(grep -c "^zfs load-key" "$d/log" 2>/dev/null || true)
if [ "$attempts" = 0 ] && grep -qx "unlock_status=1" "$d/out" \
   && grep -qi "refusing to prompt (fail-closed" "$d/out"; then
    ok "a missing/broken terminal refuses the passphrase prompt entirely (fail-closed), never touching zfs at all"
else
    cat "$d/out"; echo "attempts=$attempts"
    bad "the passphrase prompt did not fail closed as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: a terminal-restore failure after a successful capture is loud, tries 'stty sane', and does not fail the unlock =="
# By the time restore runs, the passphrase was already captured with
# echo confirmed off - a restore failure is a real usability problem
# (the terminal could be left echo-off) but not a secret-exposure one,
# so this must NOT discard an already-good unlock. It must, however,
# say so loudly and attempt one further fallback rather than silently
# claiming the terminal was restored when it wasn't.
d="$(fresh_env)"
zfs_unlock_env "$d"
STUB_ZFS_UNLOCK_CORRECT="hunter2"
export STUB_ZFS_UNLOCK_CORRECT
(
    set +e
    zfs()    { "$STUBS/zfs.unlock" "$@"; }
    dialog() { echo "hunter2" >&2; return 0; }
    clear()  { :; }
    stty() {
        case "$1" in
            -g) echo "saved-state-xyz" ;;
            -echo) return 0 ;;
            sane) echo "stty sane called" >> "${STUB_LOG:-/dev/null}"; return 0 ;;
            *) return 1 ;;
        esac
    }
    STUB_ROOT="$d/root"
    STUB_LOG="$d/log"
    export STUB_ROOT STUB_LOG
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_unlock "zroot/ROOT/enc"
    echo "unlock_status=$?"
) >"$d/out" 2>&1 || true
if grep -qx "unlock_status=0" "$d/out" \
   && grep -qi "ERROR: could not restore terminal state" "$d/out" \
   && grep -qx "stty sane called" "$d/log" 2>/dev/null; then
    ok "a restore failure is reported loudly, falls back to 'stty sane', and never fails an otherwise-successful unlock"
else
    cat "$d/out"; cat "$d/log" 2>/dev/null || true
    bad "a terminal-restore failure was not handled as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: zfs_unlock() PROMPTS even when another operation already holds this encryptionroot's lock (real-CAX architecture fix) =="
# The real bug found on Hetzner CAX hardware: a local console sitting
# at a passwordbox (not typing, just present) used to hold this exact
# lock for the ENTIRE call, so a rescue operator connecting over SSH
# got an immediate, wrong "another encryption operation is already in
# progress" and could never unlock the machine remotely at all. The
# fix: the lock is acquired ONLY after a passphrase is already
# captured, never during the prompt itself - so dialog() must still run
# here despite the lock being held throughout this whole test.
# STUB_ZFS_OP_LOCK_RETRY_SLEEP=0 so the (real, exercised) retry loop
# doesn't burn actual wall-clock seconds in a test that deliberately
# never releases the lock.
d="$(fresh_env)"
zfs_unlock_env "$d"
mkdir -p "$d/root/tmp"
# Simulates a concurrent operation already in flight - a real second
# process would have created this exact directory via zfs_op_lock()'s
# own atomic mkdir AND immediately recorded its own pid inside it;
# pre-creating both here is the deterministic, single-process way to
# exercise "someone else already holds this encryptionroot's lock"
# without needing real concurrency. The pid file matters as of F11
# (unidoc-alip's PR #5 follow-up review): a lock directory that stays
# pid-less is now correctly treated as abandoned (reclaimed after a
# brief bounded poll) rather than "someone else holds it" - this
# fixture must record a REAL, currently-alive pid ($$, this very test
# process) so it still means what it always meant here: a live holder,
# not the F11 case this test isn't exercising.
mkdir -p "$d/root/tmp/zfs-key-lock.zroot_ROOT_enc"
echo "$$" > "$d/root/tmp/zfs-key-lock.zroot_ROOT_enc/pid"
(
    set +e
    zfs()    { "$STUBS/zfs.unlock" "$@"; }
    dialog() { echo "hunter2" >&2; echo "dialog-was-called" >> "${STUB_LOG:-/dev/null}"; return 0; }
    stty()   { :; }
    clear()  { :; }
    STUB_ROOT="$d/root"
    STUB_LOG="$d/log"
    STUB_ZFS_OP_LOCK_RETRY_SLEEP=0
    export STUB_ROOT STUB_LOG STUB_ZFS_OP_LOCK_RETRY_SLEEP
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_unlock "zroot/ROOT/enc"
    echo "unlock_status=$?"
) >"$d/out" 2>&1 || true
if grep -qx "unlock_status=1" "$d/out" && grep -qi "still in progress" "$d/out" \
   && grep -q "^dialog-was-called$" "$d/log" 2>/dev/null \
   && ! grep -q "^zfs load-key" "$d/log" 2>/dev/null \
   && [ ! -e "$d/root/tmp/zfs-key.zroot_ROOT_enc" ]; then
    ok "the passphrase prompt still runs despite a held lock; only the post-prompt lock acquisition discovers the contention and discards the passphrase - never calls the real zfs load-key"
else
    cat "$d/out"; echo "---log---"; cat "$d/log" 2>/dev/null || true
    bad "zfs_unlock() did not correctly separate prompting from the operation lock"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: a lock held by another terminal that releases mid-wait lets THIS unlock still complete (real concurrency, not simulation) =="
# The actual real-CAX scenario: local tty0 is mid-transition (holds the
# lock) when an SSH operator finishes typing. Uses a REAL background
# process to release the lock partway through this call's own retry
# window - not just before/after state setup - so this genuinely
# exercises zfs_op_lock_retry()'s own retry loop, not just its
# give-up path.
d="$(fresh_env)"
zfs_unlock_env "$d"
STUB_ZFS_UNLOCK_CORRECT="hunter2"
export STUB_ZFS_UNLOCK_CORRECT
mkdir -p "$d/root/tmp"
mkdir -p "$d/root/tmp/zfs-key-lock.zroot_ROOT_enc"
# A real, currently-alive pid, same F11 reasoning as the fixture just
# above - this test wants "genuinely held, then released", not F11's
# own "pid-less, reclaimed as abandoned" case.
echo "$$" > "$d/root/tmp/zfs-key-lock.zroot_ROOT_enc/pid"
# rm -f the pid file first, same order zfs_op_unlock() itself uses -
# a bare rmdir would fail on the now-non-empty directory otherwise.
( sleep 0.3; rm -f "$d/root/tmp/zfs-key-lock.zroot_ROOT_enc/pid"; rmdir "$d/root/tmp/zfs-key-lock.zroot_ROOT_enc" ) &
release_pid=$!
(
    set +e
    zfs()    { "$STUBS/zfs.unlock" "$@"; }
    dialog() { echo "hunter2" >&2; return 0; }
    stty()   { :; }
    clear()  { :; }
    STUB_ROOT="$d/root"
    STUB_LOG="$d/log"
    STUB_ZFS_OP_LOCK_RETRY_SLEEP=0.5
    export STUB_ROOT STUB_LOG STUB_ZFS_OP_LOCK_RETRY_SLEEP
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_unlock "zroot/ROOT/enc"
    echo "unlock_status=$?"
    zfs_handoff_ready "zroot/ROOT/enc" && echo "handoff=ready" || echo "handoff=not-ready"
) >"$d/out" 2>&1 || true
wait "$release_pid" 2>/dev/null
if grep -qx "unlock_status=0" "$d/out" && grep -qx "handoff=ready" "$d/out"; then
    ok "a lock released by another (real, backgrounded) process mid-retry lets this unlock complete - a local console sitting on the lock does not permanently block a remote unlock"
else
    cat "$d/out"
    bad "the unlock did not recover once the held lock was released mid-wait"
fi
rm -rf "$d"

# =============================================================================
# unidoc-alip's PR #1 review, F2: zfs_op_lock() had no owner tracking and
# nothing anywhere ever cleaned up a directory left behind by a holder
# that died mid-operation (SIGKILL, OOM, SIGHUP on a dropped rescue-SSH
# session) - every other frontend would then see a live-looking "another
# encryption operation is already in progress" refusal until the next
# real reboot wiped tmpfs. Real, not simulated: a genuine child process
# takes the real lock and is actually SIGKILLed (not just cleaned up
# with rmdir, which would prove nothing about the reclaim logic itself),
# and a later caller must reclaim it.
# These two tests need a genuinely SEPARATE process to hold the lock,
# not a `( ... ) &` backgrounded subshell of this same script - bash's
# `$$` is documented to always report the INVOKING shell's pid inside a
# subshell, never the subshell's own real pid ($BASHPID would, $$ never
# does), so a subshell here would make zfs_op_lock() record run-tests.sh's
# own pid as the "holder" - very much alive, since it's this very test
# script - instead of the actual holder process. A real, separate `sh`
# script file backgrounded with `&` does not have this problem: it's a
# genuine fork+exec, so its own $$ is its own real pid, exactly matching
# how zfs_op_lock() is ever really called in production (always a
# script's own top-level execution - boot-dataset.sh itself, or the
# standalone zfs-unlock executable - never a backgrounded subshell
# fragment of a larger script).
_lock_holder_script() {
    cat > "$1" <<EOF
#!/bin/sh
STUB_ROOT="$2"
export STUB_ROOT
. "$REPO_ROOT/init/pid-alive.sh"
. "$REPO_ROOT/init/zfs-unlock.sh"
zfs_op_lock "zroot/ROOT/enc"
sleep "$3"
EOF
    chmod +x "$1"
}

echo "== zfs-unlock.sh: zfs_op_lock() reclaims a lock whose holder was SIGKILLed (real, separate process, real signal - not simulated) =="
d="$(fresh_env)"
mkdir -p "$d/root/tmp"
_lock_holder_script "$d/holder.sh" "$d/root" 300
sh "$d/holder.sh" &
holder_pid=$!
# Wait for the holder to actually record itself as the lock owner,
# rather than a fixed sleep - avoids a flaky race against how fast the
# background process reaches zfs_op_lock().
lock_dir="$d/root/tmp/zfs-key-lock.zroot_ROOT_enc"
i=0
while [ ! -s "$lock_dir/pid" ] && [ "$i" -lt 50 ]; do
    sleep 0.1
    i=$((i + 1))
done
recorded_pid="$(cat "$lock_dir/pid" 2>/dev/null)"
kill -KILL "$holder_pid" 2>/dev/null || true
# Give the kernel a moment to actually reap/remove the process from
# /proc - _pid_alive checks /proc directly, and a SIGKILL isn't
# necessarily instantaneous from this shell's point of view. Polling
# /proc directly rather than `wait`ing on the pid - `wait` on a
# background job started this deep into a long-running script has shown
# real, reproducible hangs in this same test file (a genuine bash
# quirk around job-control tracking, not specific to this test) even
# after the process is already confirmed gone from /proc.
i=0
while [ -d "/proc/$holder_pid" ] && [ "$i" -lt 50 ]; do
    sleep 0.1
    i=$((i + 1))
done
(
    set +e
    STUB_ROOT="$d/root"
    export STUB_ROOT
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_op_lock "zroot/ROOT/enc"
    echo "reclaim_status=$?"
) >"$d/out" 2>&1 || true
if grep -qx "reclaim_status=0" "$d/out" \
   && [ "$recorded_pid" = "$holder_pid" ] \
   && grep -q "reclaimed a stale encryption operation lock for zroot/ROOT/enc (holder pid $holder_pid is gone)" "$d/out"; then
    ok "zfs_op_lock() reclaims a lock left behind by a real, SIGKILLed holder - every other frontend is no longer stuck until the next reboot"
else
    cat "$d/out"; echo "recorded_pid=$recorded_pid holder_pid=$holder_pid"
    bad "zfs_op_lock() did not reclaim a real dead holder's lock as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: zfs_op_lock() does NOT reclaim a lock whose holder is still genuinely alive (regression check for the F2 fix above) =="
d="$(fresh_env)"
mkdir -p "$d/root/tmp"
_lock_holder_script "$d/holder.sh" "$d/root" 5
sh "$d/holder.sh" &
holder_pid=$!
lock_dir="$d/root/tmp/zfs-key-lock.zroot_ROOT_enc"
i=0
while [ ! -s "$lock_dir/pid" ] && [ "$i" -lt 50 ]; do
    sleep 0.1
    i=$((i + 1))
done
(
    set +e
    STUB_ROOT="$d/root"
    export STUB_ROOT
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_op_lock "zroot/ROOT/enc"
    echo "second_lock_status=$?"
) >"$d/out" 2>&1 || true
kill -KILL "$holder_pid" 2>/dev/null || true
if grep -qx "second_lock_status=1" "$d/out"; then
    ok "a lock held by a genuinely live holder is correctly NOT reclaimed - the fix reclaims dead holders only, never steals a live lock"
else
    cat "$d/out"
    bad "zfs_op_lock() incorrectly reclaimed a lock from a still-alive holder - this would break the lock's whole purpose"
fi
rm -rf "$d"

# =============================================================================
# F11 (unidoc-alip's PR #5 review): the staleness check (_pid_alive
# "$lock_pid") and the reclaim mv are not atomic together - two real
# processes that both observe the SAME dead holder can race, and the
# loser's own mv (run AFTER the winner already completed its full
# mv+rm+mkdir+echo-pid reclaim) would move the WINNER's fresh, live
# lock aside instead of the dead one it actually judged, leaving both
# processes believing they hold it. Real timing, not simulated: two
# genuinely separate processes, one with `mv` itself overridden to
# sleep first (deterministically losing the race at the exact mv step
# the fix's own re-check runs at, not just "probably loses" from a
# head start) so the OTHER process's full reclaim genuinely completes
# first, every run.
_lock_racer_script() {
    cat > "$1" <<EOF
#!/bin/sh
STUB_ROOT="$2"
export STUB_ROOT
. "$REPO_ROOT/init/pid-alive.sh"
. "$REPO_ROOT/init/zfs-unlock.sh"
$3
zfs_op_lock "zroot/ROOT/enc"
echo "lock_status=\$? pid=\$\$" > "$4"
EOF
    chmod +x "$1"
}

echo "== zfs-unlock.sh: zfs_op_lock() reclaim does not steal a lock another process ALREADY reclaimed in the same race window =="
d="$(fresh_env)"
mkdir -p "$d/root/tmp"
_lock_holder_script "$d/holder.sh" "$d/root" 300
sh "$d/holder.sh" &
holder_pid=$!
lock_dir="$d/root/tmp/zfs-key-lock.zroot_ROOT_enc"
i=0
while [ ! -s "$lock_dir/pid" ] && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i + 1)); done
kill -KILL "$holder_pid" 2>/dev/null || true
i=0
while [ -d "/proc/$holder_pid" ] && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i + 1)); done

# racer_slow overrides mv to sleep 0.5s before the REAL mv (the exact
# operation this fix's own re-check happens around) - a shell function
# defined before zfs-unlock.sh is sourced takes precedence over the
# real mv(1) for every call inside it, same function-override
# technique this whole file already uses throughout.
_lock_racer_script "$d/racer_slow.sh" "$d/root" \
    'mv() { command sleep 0.5; command mv "$@"; }' "$d/slow.out"
_lock_racer_script "$d/racer_fast.sh" "$d/root" '' "$d/fast.out"
sh "$d/racer_slow.sh" &
slow_shell_pid=$!
sh "$d/racer_fast.sh" &
fast_shell_pid=$!
wait "$fast_shell_pid" 2>/dev/null
wait "$slow_shell_pid" 2>/dev/null

fast_status="$(sed -n 's/.*lock_status=\([0-9-]*\).*/\1/p' "$d/fast.out" 2>/dev/null)"
fast_pid="$(sed -n 's/.*pid=\([0-9]*\).*/\1/p' "$d/fast.out" 2>/dev/null)"
slow_status="$(sed -n 's/.*lock_status=\([0-9-]*\).*/\1/p' "$d/slow.out" 2>/dev/null)"
final_pid="$(cat "$lock_dir/pid" 2>/dev/null)"
if [ "$fast_status" = "0" ] && [ "$slow_status" = "1" ] && [ "$final_pid" = "$fast_pid" ]; then
    ok "the slower racer detected the pid mismatch after its own delayed mv and backed off - the faster racer's real reclaim survived untouched"
else
    echo "fast: status=$fast_status pid=$fast_pid / slow: status=$slow_status / final lock pid=$final_pid"
    cat "$d/fast.out" "$d/slow.out" 2>/dev/null
    bad "zfs_op_lock()'s reclaim let a slower racer steal a lock the faster racer had already genuinely reclaimed"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: N4 - a third process's fresh lock is never permanently wedged by another process's mv-back =="
# Regression test for N4 (unidoc-alip's PR #5 follow-up review), a
# THIRD real process added to the exact two-process race just above -
# their own PoC methodology, reproduced here with three real processes
# running the real functions, not simulated:
#
#   B judges the original holder dead and starts reclaiming - its own
#   mv-aside is delayed so A's full reclaim (mv+rm+mkdir+own pid)
#   genuinely completes first, same as the test above.
#   B's own (now real) mv-aside grabs A's fresh lock instead of the
#   dead one it judged - the pid mismatch this file's own F11 fix
#   already detects. Before this round's fix, B would then
#   unconditionally `mv` A's lock BACK onto $lock_dir.
#   C, a genuinely separate process, does a PLAIN zfs_op_lock() call in
#   the real gap this creates (B's mv-aside already ran, so $lock_dir
#   is genuinely missing) - C's own ordinary mkdir fast path succeeds,
#   and C believes it holds the lock.
#   B's delayed mv-back then runs. `mv src dst` onto a dst that already
#   exists as a directory does not fail or replace it - it nests src
#   INSIDE dst. Before this fix, that left C's own lock directory
#   containing an unexpected extra subdirectory (A's stale-named dir,
#   pid file and all) - C's own later zfs_op_unlock() rmdir then fails
#   on a non-empty directory forever, recoverable only by a reboot.
#
# B signals C via a marker file the instant its own mv-aside genuinely
# completes (not a fixed sleep guess) - C polls for that marker, then
# fires its own zfs_op_lock() immediately, landing deterministically in
# the real gap every run.
d="$(fresh_env)"
mkdir -p "$d/root/tmp"
_lock_holder_script "$d/holder.sh" "$d/root" 300
sh "$d/holder.sh" &
holder_pid=$!
lock_dir="$d/root/tmp/zfs-key-lock.zroot_ROOT_enc"
i=0
while [ ! -s "$lock_dir/pid" ] && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i + 1)); done
kill -KILL "$holder_pid" 2>/dev/null || true
i=0
while [ -d "/proc/$holder_pid" ] && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i + 1)); done

marker="$d/b-mv-aside-done"
_lock_racer_script "$d/racer_B.sh" "$d/root" \
    "mv() { _mv_n=\$((\${_mv_n:-0} + 1)); if [ \"\$_mv_n\" = 1 ]; then command sleep 0.3; command mv \"\$@\"; touch '$marker'; command sleep 0.5; else command mv \"\$@\"; fi; }" \
    "$d/B.out"
_lock_racer_script "$d/racer_A.sh" "$d/root" '' "$d/A.out"
cat > "$d/racer_C.sh" <<EOF
#!/bin/sh
STUB_ROOT="$d/root"
export STUB_ROOT
. "$REPO_ROOT/init/pid-alive.sh"
. "$REPO_ROOT/init/zfs-unlock.sh"
i=0
while [ ! -e "$marker" ] && [ "\$i" -lt 100 ]; do sleep 0.05; i=\$((i + 1)); done
zfs_op_lock "zroot/ROOT/enc"
echo "lock_status=\$? pid=\$\$" > "$d/C.out"
EOF
chmod +x "$d/racer_C.sh"
sh "$d/racer_B.sh" &
b_pid=$!
sh "$d/racer_A.sh" &
a_pid=$!
sh "$d/racer_C.sh" &
c_pid=$!
wait "$a_pid" 2>/dev/null
wait "$c_pid" 2>/dev/null
wait "$b_pid" 2>/dev/null

c_status="$(sed -n 's/.*lock_status=\([0-9-]*\).*/\1/p' "$d/C.out" 2>/dev/null)"
c_pid_recorded="$(sed -n 's/.*pid=\([0-9]*\).*/\1/p' "$d/C.out" 2>/dev/null)"
lock_dir_entries="$(ls -A "$lock_dir" 2>/dev/null)"
if [ "$c_status" = "0" ] && [ "$lock_dir_entries" = "pid" ]; then
    rm -f "$lock_dir/pid"
    if rmdir "$lock_dir" 2>/dev/null; then
        ok "C's own fresh lock is never nested/wedged by B's mv-back - contains only its own pid file, and releases cleanly"
    else
        bad "C's lock directory could not be removed even though it looked clean - something else is wrong"
    fi
else
    echo "C: status=$c_status pid=$c_pid_recorded / lock_dir entries: [$lock_dir_entries]"
    cat "$d/A.out" "$d/B.out" "$d/C.out" 2>/dev/null
    bad "C's fresh lock was corrupted by B's mv-back - either C never acquired cleanly, or \$lock_dir now contains more than just C's own pid file (the permanent-wedge shape N4 describes)"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: F11 - a lock directory with no pid file at all (holder killed between mkdir and recording ownership) is reclaimed, not permanently wedged =="
# Regression test for F11 (unidoc-alip's PR #5 follow-up review):
# mkdir and the pid write immediately after it are not atomic together
# - a holder genuinely killed (SIGKILL, an OOM kill) in that exact gap
# leaves a lock directory that EXISTS but has no pid file inside it at
# all. Before this fix, lock_pid then read empty, the dead-pid
# liveness check never even ran (it's gated on `[ -n "$lock_pid" ]`),
# and nothing in this function could ever tell that holder apart from
# one still legitimately running - permanently wedged, the same
# severity as N4's own finding, reached through a different gap.
# Simulates the exact end state a killed-mid-acquire holder leaves
# behind directly (a real `mkdir`, deliberately with no pid file
# written into it) rather than timing a real kill against a real
# process - the fix's own logic only ever examines the end state, not
# how it arose, so this is a faithful, deterministic reproduction.
d="$(fresh_env)"
mkdir -p "$d/root/tmp"
lock_dir="$d/root/tmp/zfs-key-lock.zroot_ROOT_enc"
mkdir -p "$lock_dir" # deliberately no pid file inside
(
    set +e
    STUB_ROOT="$d/root"
    export STUB_ROOT
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_op_lock "zroot/ROOT/enc"
    echo "lock_status=$?"
) >"$d/out" 2>&1 || true
if grep -qx "lock_status=0" "$d/out" \
   && grep -qi "reclaimed an abandoned encryption operation lock" "$d/out" \
   && [ -s "$lock_dir/pid" ]; then
    ok "a pid-less lock directory (holder killed between mkdir and recording ownership) is reclaimed after the bounded poll, not left wedged forever"
else
    cat "$d/out" 2>/dev/null; ls -la "$lock_dir" 2>/dev/null
    bad "a pid-less lock directory was not reclaimed - this would wedge every future zfs_op_lock() call on this encryptionroot until the next reboot"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: F11 - a lock directory whose pid write is merely in progress (not abandoned) is left alone =="
# The negative-facing control for F11 above: a genuinely live holder
# that has mkdir'd but not yet finished writing its own pid file (the
# real, narrow, legitimate window the bounded poll must not steal from)
# must NOT be reclaimed. A real background process holds the lock open
# (writes its own pid, then sleeps) - by the time this test's own
# zfs_op_lock() call runs, the pid IS already there, so this proves the
# common "someone else genuinely holds it, and has recorded that"
# case still correctly refuses, unaffected by the new poll.
d="$(fresh_env)"
mkdir -p "$d/root/tmp"
_lock_holder_script "$d/holder.sh" "$d/root" 5
sh "$d/holder.sh" &
holder_pid=$!
lock_dir="$d/root/tmp/zfs-key-lock.zroot_ROOT_enc"
i=0
while [ ! -s "$lock_dir/pid" ] && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i + 1)); done
(
    set +e
    STUB_ROOT="$d/root"
    export STUB_ROOT
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_op_lock "zroot/ROOT/enc"
    echo "lock_status=$?"
) >"$d/out" 2>&1 || true
kill -KILL "$holder_pid" 2>/dev/null || true
if grep -qx "lock_status=1" "$d/out"; then
    ok "a lock genuinely held by a live process (pid already recorded) is correctly refused, unaffected by the new pid-less-reclaim poll"
else
    cat "$d/out"
    bad "a lock held by a genuinely live holder was incorrectly reclaimed"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: zfs_op_lock_retry() itself is a real ~10-poll budget, not just barely wide enough for the other tests' own release timings =="
# A DIRECT unit test of zfs_op_lock_retry() alone, deliberately not
# routed through the full zfs_unlock() flow - that flow's own outer
# 3-attempt retry loop gives every attempt its OWN fresh
# zfs_op_lock_retry() budget, which means a release late enough to miss
# one attempt's budget can still be caught by the NEXT attempt's own
# fresh one, silently masking a too-narrow inner budget (confirmed:
# an earlier version of this test routed through zfs_unlock() and
# stayed green even with the inner budget cut back down to 3 tries).
# Calling zfs_op_lock_retry() directly removes that confound. Release
# timed (at whole-second, not sub-second, granularity - ordinary
# subshell/fork overhead can otherwise swallow a tighter margin) to
# land after a 3-try budget would already have given up (~3s) but
# comfortably inside the current ~10-try one.
d="$(fresh_env)"
mkdir -p "$d/root/tmp"
mkdir -p "$d/root/tmp/zfs-key-lock.zroot_ROOT_enc"
# A real, currently-alive pid (F11, unidoc-alip's PR #5 follow-up
# review) - without it, this pid-less lock directory would be
# reclaimed as abandoned after F11's own bounded poll (~250ms), long
# before this test's own 4-second release, and the background job's
# plain `rmdir` below would then fail on the reclaiming process's own
# non-empty (pid file present) directory.
echo "$$" > "$d/root/tmp/zfs-key-lock.zroot_ROOT_enc/pid"
( sleep 4; rm -f "$d/root/tmp/zfs-key-lock.zroot_ROOT_enc/pid"; rmdir "$d/root/tmp/zfs-key-lock.zroot_ROOT_enc" ) &
release_pid=$!
(
    set +e
    STUB_ROOT="$d/root"
    export STUB_ROOT
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_op_lock_retry "zroot/ROOT/enc"
    echo "retry_status=$?"
) >"$d/out" 2>&1 || true
wait "$release_pid" 2>/dev/null
if grep -qx "retry_status=0" "$d/out"; then
    ok "zfs_op_lock_retry() itself survives past a release that a narrower (e.g. 3-try) budget would already have given up on"
else
    cat "$d/out"
    bad "zfs_op_lock_retry()'s own contention window is not wide enough to survive this release timing"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: two terminals both captured the correct passphrase - the one that loses the race discards cleanly, no re-staging or corruption =="
d="$(fresh_env)"
zfs_unlock_env "$d"
STUB_ZFS_UNLOCK_CORRECT="hunter2"
export STUB_ZFS_UNLOCK_CORRECT
mkdir -p "$d/root/tmp"
mkdir -p "$d/root/tmp/zfs-key-lock.zroot_ROOT_enc"
# A real, currently-alive pid - see F11's own fixture comment above for
# why this matters now (unidoc-alip's PR #5 follow-up review).
echo "$$" > "$d/root/tmp/zfs-key-lock.zroot_ROOT_enc/pid"
# Background job simulates the OTHER terminal's own real load-key+stage
# completing first (same correct passphrase), then releasing the lock -
# standing in for a second, genuinely concurrent zfs_unlock() call.
(
    sleep 0.3
    echo "available" > "$d/state"
    printf 'hunter2' > "$d/root/tmp/zfs-key.zroot_ROOT_enc"
    rm -f "$d/root/tmp/zfs-key-lock.zroot_ROOT_enc/pid"
    rmdir "$d/root/tmp/zfs-key-lock.zroot_ROOT_enc"
) &
release_pid=$!
(
    set +e
    zfs()    { "$STUBS/zfs.unlock" "$@"; }
    dialog() { echo "hunter2" >&2; return 0; }
    stty()   { :; }
    clear()  { :; }
    STUB_ROOT="$d/root"
    STUB_LOG="$d/log"
    STUB_ZFS_OP_LOCK_RETRY_SLEEP=0.5
    export STUB_ROOT STUB_LOG STUB_ZFS_OP_LOCK_RETRY_SLEEP
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_unlock "zroot/ROOT/enc"
    echo "unlock_status=$?"
    zfs_handoff_ready "zroot/ROOT/enc" && echo "handoff=ready" || echo "handoff=not-ready"
) >"$d/out" 2>&1 || true
wait "$release_pid" 2>/dev/null
staged="$(cat "$d/root/tmp/zfs-key.zroot_ROOT_enc" 2>/dev/null)"
if grep -qx "unlock_status=0" "$d/out" && grep -qx "handoff=ready" "$d/out" \
   && [ "$staged" = "hunter2" ] \
   && ! grep -q "^zfs load-key -n zroot/ROOT/enc$" "$d/log" 2>/dev/null; then
    ok "the losing terminal recognizes the already-completed transition under the lock and discards its own passphrase, without re-verifying or re-staging"
else
    cat "$d/out"; echo "staged=[$staged]"; cat "$d/log" 2>/dev/null || true
    bad "the second terminal did not discard its own passphrase cleanly"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: a concurrent zfs_lock() wins the race while this terminal is mid-prompt - a fresh real unlock happens with what was just typed =="
# One of the audit's own explicit races: a reacquire prompt (dataset
# UNLOCKED/NOT-READY at peek time) is in flight when a concurrent
# zfs_lock() relocks the dataset before this call gets the operation
# lock. Re-checking state under the lock must notice it's LOCKED again
# and perform a REAL load-key with what was just typed, not a stale
# load-key -n verify against a key that no longer exists.
d="$(fresh_env)"
zfs_unlock_env "$d"
echo "available" > "$d/state"
STUB_ZFS_UNLOCK_CORRECT="hunter2"
export STUB_ZFS_UNLOCK_CORRECT
mkdir -p "$d/root/tmp"
mkdir -p "$d/root/tmp/zfs-key-lock.zroot_ROOT_enc"
# A real, currently-alive pid - see F11's own fixture comment above for
# why this matters now (unidoc-alip's PR #5 follow-up review).
echo "$$" > "$d/root/tmp/zfs-key-lock.zroot_ROOT_enc/pid"
( sleep 0.3; echo "unavailable" > "$d/state"; rm -f "$d/root/tmp/zfs-key-lock.zroot_ROOT_enc/pid"; rmdir "$d/root/tmp/zfs-key-lock.zroot_ROOT_enc" ) &
release_pid=$!
(
    set +e
    zfs()    { "$STUBS/zfs.unlock" "$@"; }
    dialog() { echo "hunter2" >&2; return 0; }
    stty()   { :; }
    clear()  { :; }
    STUB_ROOT="$d/root"
    STUB_LOG="$d/log"
    STUB_ZFS_OP_LOCK_RETRY_SLEEP=0.5
    export STUB_ROOT STUB_LOG STUB_ZFS_OP_LOCK_RETRY_SLEEP
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_unlock "zroot/ROOT/enc"
    echo "unlock_status=$?"
    zfs_handoff_ready "zroot/ROOT/enc" && echo "handoff=ready" || echo "handoff=not-ready"
) >"$d/out" 2>&1 || true
wait "$release_pid" 2>/dev/null
if grep -qx "unlock_status=0" "$d/out" && grep -qx "handoff=ready" "$d/out" \
   && grep -q "^zfs load-key zroot/ROOT/enc$" "$d/log" 2>/dev/null; then
    ok "a concurrent lock winning mid-prompt is detected under the lock, and a real (not dry-run) load-key with the just-typed passphrase completes a fresh unlock"
else
    cat "$d/out"; cat "$d/log" 2>/dev/null || true
    bad "a concurrent Lock-wins race was not handled with a fresh real unlock"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: cancelling every passphrase prompt never acquires the operation lock at all =="
d="$(fresh_env)"
zfs_unlock_env "$d"
(
    set +e
    zfs()    { "$STUBS/zfs.unlock" "$@"; }
    dialog() { return 1; }
    stty()   { :; }
    clear()  { :; }
    STUB_ROOT="$d/root"
    STUB_LOG="$d/log"
    STUB_ZFS_OP_LOCK_RETRY_SLEEP=0
    export STUB_ROOT STUB_LOG STUB_ZFS_OP_LOCK_RETRY_SLEEP
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_unlock "zroot/ROOT/enc"
    echo "unlock_status=$?"
    lock_dir="$(zfs_op_lock_path "zroot/ROOT/enc")"
    [ -d "$lock_dir" ] && echo "lock_dir=held-BUG" || echo "lock_dir=never-held-correct"
) >"$d/out" 2>&1 || true
if grep -qx "unlock_status=1" "$d/out" && grep -qx "lock_dir=never-held-correct" "$d/out" \
   && ! grep -q "^zfs load-key" "$d/log" 2>/dev/null; then
    ok "cancelling the passphrase prompt every time never acquires (or leaves held) the operation lock"
else
    cat "$d/out"; cat "$d/log" 2>/dev/null || true
    bad "cancelling the prompt did not behave as expected with respect to the operation lock"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: zfs_lock() refuses immediately (no zfs calls at all) when a concurrent unlock/reacquire already holds the lock =="
# Exactly the race the audit called out: a lock racing a reacquire must
# never interleave - one holder at a time, enforced by the SAME
# per-encryptionroot lock zfs_unlock() itself takes (see the test
# above) - this is the other side of that same mutual-exclusion.
d="$(fresh_env)"
zfs_unlock_env "$d"
echo "available" > "$d/state"
mkdir -p "$d/root/tmp"
mkdir -p "$d/root/tmp/zfs-key-lock.zroot_ROOT_enc"
# A real, currently-alive pid - see F11's own fixture comment further
# above for why this matters now (unidoc-alip's PR #5 follow-up
# review): this test wants an IMMEDIATE, permanent refusal with zero
# zfs calls, not F11's own bounded-poll-then-reclaim path.
echo "$$" > "$d/root/tmp/zfs-key-lock.zroot_ROOT_enc/pid"
(
    set +e
    zfs() { "$STUBS/zfs.unlock" "$@"; }
    STUB_ROOT="$d/root"
    STUB_LOG="$d/log"
    export STUB_ROOT STUB_LOG
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_lock "zroot/ROOT/enc"
    echo "lock_status=$?"
) >"$d/out" 2>&1 || true
if grep -qx "lock_status=1" "$d/out" && grep -qi "already in progress" "$d/out" \
   && [ ! -s "$d/log" ] && [ "$(cat "$d/state")" = "available" ]; then
    ok "zfs_lock() refuses immediately when a concurrent unlock/reacquire already holds this encryptionroot's lock - keystatus untouched"
else
    cat "$d/out"; echo "---log---"; cat "$d/log" 2>/dev/null || true
    bad "zfs_lock() did not respect a held operation lock as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: the operation lock is released after a call completes, so the NEXT call is never blocked =="
# Guards against the opposite bug from the two tests above: a lock that
# is acquired but never released would permanently wedge every future
# unlock/lock attempt for this encryptionroot, which would be just as
# real a production incident as no locking at all.
d="$(fresh_env)"
zfs_unlock_env "$d"
STUB_ZFS_UNLOCK_CORRECT="hunter2"
export STUB_ZFS_UNLOCK_CORRECT
(
    set +e
    zfs()    { "$STUBS/zfs.unlock" "$@"; }
    dialog() { echo "hunter2" >&2; return 0; }
    stty()   { :; }
    clear()  { :; }
    STUB_ROOT="$d/root"
    STUB_LOG="$d/log"
    export STUB_ROOT STUB_LOG
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    zfs_unlock "zroot/ROOT/enc"
    echo "unlock_status=$?"
    lock_dir="$(zfs_op_lock_path "zroot/ROOT/enc")"
    [ -d "$lock_dir" ] && echo "lock_dir=still-held-BUG" || echo "lock_dir=released-correct"
    zfs_lock "zroot/ROOT/enc"
    echo "lock_status=$?"
) >"$d/out" 2>&1 || true
if grep -qx "unlock_status=0" "$d/out" && grep -qx "lock_dir=released-correct" "$d/out" \
   && grep -qx "lock_status=0" "$d/out"; then
    ok "the per-encryptionroot operation lock is released after each call - a second, later operation is never blocked by it"
else
    cat "$d/out"; bad "the operation lock was not released as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock.sh: _zfs_unlock_cleanup_tempfiles removes both plaintext-passphrase mktemp files =="
# Regression test for a real gap the full 8-way source audit found:
# $dialog_err_file (dialog's own stderr/answer capture) and $prompt_out
# (zfs_unlock()'s own OUT_FILE) both hold the operator's plaintext
# passphrase for a real window between being written and being read
# back, with no signal protection at all before this fix. Direct test
# of the new cleanup function itself, not a signal-timing-dependent
# end-to-end scenario (see the SIGTERM test below for that, on the
# standalone wrapper where it's cleanly isolatable).
d="$(fresh_env)"
(
    set +e
    STUB_ROOT="$d/root"
    export STUB_ROOT
    . "$REPO_ROOT/init/pid-alive.sh"
    . "$REPO_ROOT/init/zfs-unlock.sh"
    dialog_err_file="$d/dialog_err_file"
    prompt_out="$d/prompt_out"
    printf 'plaintext-passphrase-1' > "$dialog_err_file"
    printf 'plaintext-passphrase-2' > "$prompt_out"
    _zfs_unlock_cleanup_tempfiles
    [ -e "$dialog_err_file" ] && echo "dialog_err_file=still-present-BUG" || echo "dialog_err_file=removed"
    [ -e "$prompt_out" ] && echo "prompt_out=still-present-BUG" || echo "prompt_out=removed"
    # Safe to call again (e.g. a real trap firing after normal cleanup
    # already ran) - rm -f on an already-gone path must not error.
    _zfs_unlock_cleanup_tempfiles
    echo "second_call_status=$?"
) >"$d/out" 2>&1 || true
if grep -qx "dialog_err_file=removed" "$d/out" && grep -qx "prompt_out=removed" "$d/out" \
   && grep -qx "second_call_status=0" "$d/out"; then
    ok "_zfs_unlock_cleanup_tempfiles removes both plaintext-passphrase files, and is safe to call again afterward"
else
    cat "$d/out"; bad "_zfs_unlock_cleanup_tempfiles did not clean up as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock: SIGTERM mid-prompt removes the plaintext passphrase temp files via the standalone wrapper's own trap =="
# Real, separate-process SIGTERM test of init/zfs-unlock (the standalone
# CLI menu.py's "Unlock encrypted root" action shells out to) - unlike
# boot-dataset.sh, this wrapper has no OTHER cleanup path (no fail(), no
# cascading mount-failure) that could confound a negative control, so
# this one IS a clean, mechanism-isolating regression test: the ONLY
# thing that can remove these temp files here is the trap this fix
# adds. A real holder SCRIPT (not a PATH override - init/zfs-unlock
# resets its own PATH internally, same as boot-dataset.sh, so stub
# commands must be shell FUNCTIONS in a dot-sourcing subshell, same
# technique this whole file already uses throughout - see run-tests.sh's
# own top-of-file comment) dot-sources init/zfs-unlock directly. dialog
# is stubbed to block indefinitely (simulating an operator sitting at
# the passwordbox, not typing) - dialog_err_file exists on disk
# (created by `dialog ... 2>"$dialog_err_file"`) but stays empty until
# dialog itself exits, so this test checks for the FILE being gone, not
# its content. TMPDIR is pointed at a private scratch dir so the poll
# below can find the exact file mktemp creates without guessing a name
# or scanning the real, shared /tmp.
d="$(fresh_env)"
mkdir -p "$d/scratch-tmp"
cat > "$d/holder.sh" <<EOF
#!/usr/bin/env bash
zfs() {
    if [ "\$1" = "get" ]; then
        case "\$5" in
            keystatus) echo "unavailable" ;;
            keylocation) echo "prompt" ;;
        esac
    fi
}
dialog() { while :; do sleep 300; done; }
stty() { [ "\$1" = "-g" ] && echo "stub-saved-state"; return 0; }
clear() { :; }
STUB_ROOT="$d/root"
TMPDIR="$d/scratch-tmp"
STUB_TTY="$d/tty"
export STUB_ROOT TMPDIR STUB_TTY
. "$REPO_ROOT/init/zfs-unlock" unlock "zroot/ROOT/enc"
EOF
chmod +x "$d/holder.sh"
"$d/holder.sh" >"$d/out" 2>"$d/err" &
wrapper_pid=$!
# dialog_err_file is created (via mktemp, honoring $TMPDIR above) right
# before dialog itself runs - poll for it directly rather than guessing
# a fixed sleep.
i=0
while [ -z "$(find "$d/scratch-tmp" -mindepth 1 2>/dev/null)" ] && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i + 1)); done
dialog_err_file="$(find "$d/scratch-tmp" -mindepth 1 2>/dev/null | head -1)"
kill -TERM "$wrapper_pid" 2>/dev/null
# Poll for the FILE being gone AND for the wrapper process itself
# exiting. The second check is real regression coverage for init/
# zfs-unlock's own F3-equivalent fix (unidoc-alip's PR #5 review found
# this exact bug in boot-dataset.sh's own trap; the standalone
# zfs-unlock wrapper had the identical shape and was fixed the same
# way): a single `trap CMD EXIT INT TERM HUP` does not terminate a
# POSIX sh script on its own - the handler runs and the script RESUMES
# right after it. Before that fix, this wrapper cleaned up the temp
# file and then kept running (confirmed directly, real trap, real
# signal) until explicitly killed a few lines below - this test used
# to accept that as fine, checking only the file's removal. After the
# fix, the handler also disarms the EXIT trap and calls `exit 143`
# (128+SIGTERM) for real, so the process is expected to actually be
# gone here, not just quiescent.
i=0
while [ -e "$dialog_err_file" ] && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i + 1)); done
if [ -n "$dialog_err_file" ] && [ ! -e "$dialog_err_file" ]; then
    ok "SIGTERM mid-prompt removed the plaintext-passphrase temp file via the standalone wrapper's own trap"
else
    echo "dialog_err_file=[$dialog_err_file]"; ls -la "$d/scratch-tmp" 2>/dev/null; cat "$d/out" "$d/err" 2>/dev/null
    bad "SIGTERM mid-prompt left the plaintext-passphrase temp file behind"
fi
i=0
while kill -0 "$wrapper_pid" 2>/dev/null && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i + 1)); done
if ! kill -0 "$wrapper_pid" 2>/dev/null; then
    ok "SIGTERM mid-prompt made the standalone wrapper actually terminate, not just clean up and resume"
else
    bad "SIGTERM mid-prompt cleaned up but the wrapper process is still running - the trap resumed instead of exiting"
fi
# Safety net only now (the wrapper is expected to already be gone by
# this point, per the real assertion just above) - || true on both
# since under this file's own `set -eu`, either one returning nonzero
# for the now-ordinary "already exited, nothing to kill" case would
# abort the ENTIRE test suite right here, not just this one cleanup
# step - confirmed the hard way, this exact gap silently truncated
# every run after this test until fixed.
kill -KILL "$wrapper_pid" 2>/dev/null || true
pkill -9 -P "$wrapper_pid" 2>/dev/null || true
rm -rf "$d"

# =============================================================================
echo "== zfs-unlock: SIGTERM after zfs_op_lock succeeds but before zfs_op_unlock releases the per-encryptionroot lock, not just the boot lock =="
# F3 (unidoc-alip's PR #5 follow-up review): distinct from the SIGTERM
# mid-prompt test above, which lands BEFORE zfs_op_lock() is ever
# acquired (this file's own header comment: the lock is deliberately
# never held during the human prompt itself). This test signals in the
# ONE window that actually exercises the bug - after zfs_op_lock()
# has succeeded but before that same call has reached its own
# zfs_op_unlock() - using the non-prompt keylocation path
# (zfs_unlock.sh lines ~712-734), the shortest real path from "lock
# acquired" to "a blocking external command runs next" (`zfs load-key`,
# stubbed here to block indefinitely). Before the F3 fix, nothing in
# this wrapper's trap chain ever called zfs_op_unlock() for a lock
# already held at signal time - the lock directory (with a live-looking
# pid file, since the process really was alive when it wrote it) was
# left behind, invisible to N4/F11's own dead-pid reclaim until this
# process's pid was ALSO gone, i.e. exactly the state a clean release
# reaches immediately anyway. Checking for the lock directory itself
# being gone (not just the wrapper process exiting, already proven by
# the test above) is what's new here.
d="$(fresh_env)"
cat > "$d/holder.sh" <<EOF
#!/usr/bin/env bash
zfs() {
    if [ "\$1" = "get" ]; then
        case "\$5" in
            keystatus) echo "unavailable" ;;
            keylocation) echo "file:///run/some-key" ;;
        esac
    elif [ "\$1" = "load-key" ]; then
        # A short-poll loop, not a single long sleep - a bash foreground
        # wait() on a single multi-hundred-second child does NOT get
        # interrupted promptly by delivery of a trapped signal (confirmed
        # directly: a bare 300s sleep left the trap unrun for minutes).
        # Real dialog in _zfs_prompt_once() has the same shape for the
        # same reason (background + a 1s poll loop, see its own code) -
        # matching that grain here is what lets SIGTERM actually land
        # inside this window within the test's own poll budget below.
        while :; do sleep 1; done
    fi
}
STUB_ROOT="$d/root"
export STUB_ROOT
. "$REPO_ROOT/init/zfs-unlock" unlock "zroot/ROOT/enc"
EOF
chmod +x "$d/holder.sh"
lock_dir="$d/root/tmp/zfs-key-lock.zroot_ROOT_enc"
"$d/holder.sh" >"$d/out" 2>"$d/err" &
wrapper_pid=$!
i=0
while [ ! -e "$lock_dir/pid" ] && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i + 1)); done
if [ ! -e "$lock_dir/pid" ]; then
    cat "$d/out" "$d/err" 2>/dev/null
    bad "SIGTERM-after-lock test setup failed - the lock was never even acquired (zfs load-key never reached)"
else
    kill -TERM "$wrapper_pid" 2>/dev/null
    i=0
    while [ -e "$lock_dir" ] && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i + 1)); done
    if [ ! -e "$lock_dir" ]; then
        ok "SIGTERM after zfs_op_lock succeeded released the per-encryptionroot lock via the trap, not just the wrapper process itself"
    else
        ls -la "$lock_dir" 2>/dev/null; cat "$d/out" "$d/err" 2>/dev/null
        bad "SIGTERM after zfs_op_lock succeeded left the per-encryptionroot lock directory behind"
    fi
fi
kill -KILL "$wrapper_pid" 2>/dev/null || true
pkill -9 -P "$wrapper_pid" 2>/dev/null || true
rm -rf "$d"

# net_config_test MODE_VARS... - runs net_config eth0 under the usual
# stubs with the given ALPINE_ZFSBOOT_* env assignments, logs to $d/log,
# fresh scratch dir each call (echoed as $d for the caller to inspect
# and clean up itself, matching every other test's own convention).
run_net_config() {
    d="$(fresh_env)"
    # NET_CONFIG_STATE, if the caller set it before calling, is reused
    # (a test that calls run_net_config twice to check idempotency wants
    # the SECOND call to see the FIRST call's own state, exactly like a
    # real interface would) - otherwise each call gets its own fresh
    # scratch file, isolated from every other test.
    ip_state="${NET_CONFIG_STATE:-$d/ip-state}"
    (
        set +e
        ip()       { "$STUBS/ip" "$@"; }
        ifconfig() { "$STUBS/ifconfig" "$@"; }
        udhcpc()   { "$STUBS/udhcpc" "$@"; }
        udhcpc6()  { "$STUBS/udhcpc6" "$@"; }
        STUB_LOG="$d/log"
        STUB_IP_STATE="$ip_state"
        export STUB_LOG STUB_IP_STATE
        # Fast (no real sleeping) by default for every test - same
        # override-for-speed precedent zfs-unlock.sh's own
        # STUB_ZFS_OP_LOCK_RETRY_SLEEP already established for its
        # analogous bounded-poll lock-retry loop. wait_for_global_ipv6's
        # real production default is a whole second per poll (busybox
        # `sleep` portability - see net-config.sh's own comment) and up
        # to 5 poll attempts - without this override, EVERY test that
        # exercises IPv6=auto (SLAAC) without pre-seeding a global
        # address (almost none of them do - SLAAC is real-kernel-driven,
        # not something the `ip` stub ever produces on its own) would
        # genuinely wait 5 real seconds. Confirmed the hard way: the
        # first version of this fix stalled the whole suite exactly
        # there. A caller can still override this explicitly (several
        # of this file's own IPv6 SLAAC tests do, redundantly but
        # harmlessly, to make the intent locally obvious) - `eval`'s own
        # later assignment below still wins over this default either way.
        : "${NET_CONFIG_POLL_INTERVAL:=0}"
        export NET_CONFIG_POLL_INTERVAL
        . "$REPO_ROOT/init/net-config.sh"
        # shellcheck disable=SC2068
        eval "$@" 'net_config eth0'
        # Written out explicitly rather than relying on this subshell's
        # own exit status: run-tests.sh runs under `set -eu`, and most
        # existing callers of run_net_config never look at a return
        # value at all (they only inspect $d/log/$d/out afterward) - if
        # this function returned net_config's real status directly, set
        # -e would abort the ENTIRE test suite the first time it's
        # called with a config that's SUPPOSED to fail (exactly the two
        # new failure-path tests this variable exists for). Stashed in
        # the global $net_config_exit instead, checked only by the
        # handful of tests that actually care about it.
        echo "NET_CONFIG_EXIT=$?" >> "$d/out.status"
    ) >"$d/out" 2>&1 || true
    net_config_exit="$(sed -n 's/^NET_CONFIG_EXIT=//p' "$d/out.status" 2>/dev/null)"
    net_config_exit="${net_config_exit:-1}"
}

# =============================================================================
echo "== net-config.sh: default (no env at all) -> IPv4 dhcp + IPv6 SLAAC, unchanged legacy behavior =="
# ALPINE_ZFSBOOT_NET defaults to auto, which defaults IPv4 to dhcp and
# IPv6 to auto(SLAAC) - the exact pre-existing behavior every current
# deployment already depends on, byte for byte (see net-config.sh's own
# header comment on why the original ifconfig+udhcpc path is untouched).
run_net_config
if grep -q "^ifconfig eth0 up$" "$d/log" 2>/dev/null \
   && grep -q "^udhcpc -i eth0 -n -q$" "$d/log" \
   && grep -qi "SLAAC" "$d/out" \
   && ! grep -q "udhcpc6\|ip -6 addr" "$d/log"; then
    ok "no env at all -> IPv4 dhcp (ifconfig+udhcpc) + IPv6 SLAAC, nothing else touched"
else
    cat "$d/log" 2>/dev/null; cat "$d/out"; bad "default (no env) net_config did not behave as the unchanged legacy default"
fi
rm -rf "$d"

# =============================================================================
echo "== net-config.sh: explicit IPv4=dhcp, IPv6=dhcp (udhcpc6) =="
run_net_config 'ALPINE_ZFSBOOT_IPV4=dhcp ALPINE_ZFSBOOT_IPV6=dhcp'
if grep -q "^udhcpc -i eth0 -n -q$" "$d/log" 2>/dev/null \
   && grep -q "^udhcpc6 -i eth0 -n -q$" "$d/log"; then
    ok "explicit dhcp on both families runs udhcpc AND udhcpc6"
else
    cat "$d/log" 2>/dev/null; bad "explicit IPv4=dhcp/IPv6=dhcp did not run both DHCP clients"
fi
rm -rf "$d"

# =============================================================================
echo "== net-config.sh: IPv4 dhcp that never obtains a real lease must not report ok =="
# Regression test for a real bug found in a full source audit: udhcpc
# used to be backgrounded (trailing &) with ipv4_ok=1 set unconditionally
# the instant the client merely STARTED - never checking whether a lease
# was actually obtained. STUB_UDHCPC_FAIL=1 makes the stub exit 1
# (matches busybox udhcpc's own real -n behavior: exit nonzero if no
# lease), simulating a DHCP server that never answers.
run_net_config 'ALPINE_ZFSBOOT_IPV4=dhcp ALPINE_ZFSBOOT_IPV6=off STUB_UDHCPC_FAIL=1'
if [ "$net_config_exit" != "0" ]; then
    ok "IPv4 dhcp with no real lease (and IPv6 off) makes net_config genuinely fail, not silently report ok"
else
    cat "$d/out" 2>/dev/null; bad "net_config reported success despite udhcpc never obtaining a lease"
fi
rm -rf "$d"

# =============================================================================
echo "== net-config.sh: IPv4 dhcp that DOES obtain a lease still reports ok (positive case, not just the failure path) =="
run_net_config 'ALPINE_ZFSBOOT_IPV4=dhcp ALPINE_ZFSBOOT_IPV6=off'
if [ "$net_config_exit" = "0" ]; then
    ok "IPv4 dhcp with a real (stub-simulated) lease still reports ok - the fix didn't just make everything fail"
else
    cat "$d/out" 2>/dev/null; bad "a genuinely successful IPv4 dhcp lease was not reported as ok"
fi
rm -rf "$d"

# =============================================================================
echo "== net-config.sh: IPv6 dhcp (udhcpc6) that never obtains a real lease must not report ok =="
run_net_config 'ALPINE_ZFSBOOT_IPV4=off ALPINE_ZFSBOOT_IPV6=dhcp STUB_UDHCPC6_FAIL=1'
if [ "$net_config_exit" != "0" ]; then
    ok "IPv6 dhcp (udhcpc6) with no real lease (and IPv4 off) makes net_config genuinely fail"
else
    cat "$d/out" 2>/dev/null; bad "net_config reported success despite udhcpc6 never obtaining a lease"
fi
rm -rf "$d"

# =============================================================================
echo "== net-config.sh: IPv6 SLAAC that never produces a global address must not report ok =="
# Regression test for the same class of bug as IPv4 dhcp above, but for
# SLAAC specifically - there's no client process to check an exit status
# on, so the fix is a bounded poll (wait_for_global_ipv6) instead of a
# synchronous command. NET_CONFIG_POLL_INTERVAL=0 keeps this test fast
# (no real sleeping) without changing the number of poll attempts. A
# fresh NET_CONFIG_STATE with nothing in it simulates an interface that
# never receives a Router Advertisement at all.
# seed dir is deliberately SEPARATE from run_net_config's own $d (which
# it reassigns internally via fresh_env, on every call) - NET_CONFIG_STATE
# is expanded to today's value of $seed BEFORE run_net_config's body
# reassigns $d, so this correctly points at our own pre-seeded file
# regardless of what run_net_config does with $d afterward.
seed="$(mktemp -d)"
NET_CONFIG_STATE="$seed/ip-state" run_net_config 'ALPINE_ZFSBOOT_IPV4=off ALPINE_ZFSBOOT_IPV6=auto NET_CONFIG_POLL_INTERVAL=0'
if [ "$net_config_exit" != "0" ]; then
    ok "IPv6 SLAAC that never produces any address (IPv4 off) makes net_config genuinely fail, not silently report ok"
else
    cat "$d/out" 2>/dev/null; bad "net_config reported success despite SLAAC never producing an address"
fi
rm -rf "$d" "$seed"

# =============================================================================
echo "== net-config.sh: IPv6 SLAAC with only a link-local address must not count as ready =="
# The specific false-positive the audit flagged by name: the kernel
# assigns a link-local (fe80::/64) address the instant the link comes
# up, regardless of whether SLAAC ever configures anything real -
# counting that would make this check exactly as vacuous as the bug it
# replaces.
seed="$(mktemp -d)"
echo "ADDR6 fe80::1/64 eth0" > "$seed/ip-state"
NET_CONFIG_STATE="$seed/ip-state" run_net_config 'ALPINE_ZFSBOOT_IPV4=off ALPINE_ZFSBOOT_IPV6=auto NET_CONFIG_POLL_INTERVAL=0'
if [ "$net_config_exit" != "0" ]; then
    ok "a link-local-only address does not count as SLAAC readiness"
else
    cat "$d/out" 2>/dev/null; bad "a link-local-only address was incorrectly treated as SLAAC success"
fi
rm -rf "$d" "$seed"

# =============================================================================
echo "== net-config.sh: IPv6 SLAAC with a real global address present reports ok =="
seed="$(mktemp -d)"
echo "ADDR6 2001:db8::5/64 eth0" > "$seed/ip-state"
NET_CONFIG_STATE="$seed/ip-state" run_net_config 'ALPINE_ZFSBOOT_IPV4=off ALPINE_ZFSBOOT_IPV6=auto NET_CONFIG_POLL_INTERVAL=0'
if [ "$net_config_exit" = "0" ]; then
    ok "a real global IPv6 address makes SLAAC readiness report ok - the fix didn't just make everything fail"
else
    cat "$d/out" 2>/dev/null; bad "a genuine global IPv6 address was not recognized as SLAAC success"
fi
rm -rf "$d" "$seed"

# =============================================================================
echo "== net-config.sh: IPv4 static (with gateway) + IPv6 SLAAC together =="
run_net_config 'ALPINE_ZFSBOOT_IPV4=static ALPINE_ZFSBOOT_IPV4_ADDRESS=203.0.113.5/24 ALPINE_ZFSBOOT_IPV4_GATEWAY=203.0.113.1 ALPINE_ZFSBOOT_IPV6=auto'
if grep -q "^ip addr add 203.0.113.5/24 dev eth0$" "$d/log" 2>/dev/null \
   && grep -q "^ip route add default via 203.0.113.1 dev eth0$" "$d/log" \
   && ! grep -q "udhcpc \|udhcpc6" "$d/log"; then
    ok "IPv4 static + IPv6 SLAAC together: correct address+route, no DHCP client run for either family"
else
    cat "$d/log" 2>/dev/null; bad "mixed IPv4=static/IPv6=auto did not behave as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== net-config.sh: IPv4 static with NO gateway - address still applied, no route command at all =="
run_net_config 'ALPINE_ZFSBOOT_IPV4=static ALPINE_ZFSBOOT_IPV4_ADDRESS=203.0.113.5/24 ALPINE_ZFSBOOT_IPV6=off'
if grep -q "^ip addr add 203.0.113.5/24 dev eth0$" "$d/log" 2>/dev/null \
   && ! grep -q "route add" "$d/log"; then
    ok "IPv4 static with no gateway configured - address applied, no route command attempted at all (gateway is optional even under static)"
else
    cat "$d/log" 2>/dev/null; bad "IPv4 static with no gateway did not behave as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== net-config.sh: IPv4=static but ALPINE_ZFSBOOT_IPV4_ADDRESS unset -> skipped, reported, not silently ignored =="
run_net_config 'ALPINE_ZFSBOOT_IPV4=static ALPINE_ZFSBOOT_IPV6=off'
if grep -qi "IPv4=static but ALPINE_ZFSBOOT_IPV4_ADDRESS is unset" "$d/out" 2>/dev/null \
   && ! grep -q "ip addr add" "$d/log" 2>/dev/null; then
    ok "IPv4=static with no address configured is reported clearly, not silently skipped or crashing"
else
    cat "$d/out"; bad "missing IPV4_ADDRESS under static was not handled as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== net-config.sh: IPv6=static but ALPINE_ZFSBOOT_IPV6_ADDRESS unset -> skipped, reported, not silently ignored =="
run_net_config 'ALPINE_ZFSBOOT_IPV4=off ALPINE_ZFSBOOT_IPV6=static'
if grep -qi "IPv6=static but ALPINE_ZFSBOOT_IPV6_ADDRESS is unset" "$d/out" 2>/dev/null \
   && ! grep -q "ip -6 addr add" "$d/log" 2>/dev/null; then
    ok "IPv6=static with no address configured is reported clearly, not silently skipped or crashing"
else
    cat "$d/out"; bad "missing IPV6_ADDRESS under static was not handled as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== net-config.sh: alpine-zfsboot.net=static sets BOTH families' default to static (per-family var still overrides) =="
run_net_config 'ALPINE_ZFSBOOT_NET=static ALPINE_ZFSBOOT_IPV4_ADDRESS=203.0.113.5/24 ALPINE_ZFSBOOT_IPV6_ADDRESS=2001:db8::5/64'
if grep -q "^ip addr add 203.0.113.5/24 dev eth0$" "$d/log" 2>/dev/null \
   && grep -q "^ip -6 addr add 2001:db8::5/64 dev eth0$" "$d/log"; then
    ok "net=static (no explicit per-family mode) defaults BOTH IPv4 and IPv6 to static"
else
    cat "$d/log" 2>/dev/null; bad "net=static did not correctly default both families to static"
fi
rm -rf "$d"

# =============================================================================
echo "== net-config.sh: alpine-zfsboot.net=off with nothing else -> interface never even brought up =="
run_net_config 'ALPINE_ZFSBOOT_NET=off'
if ! grep -q "^ip link set eth0 up$" "$d/log" 2>/dev/null && grep -qi "both families off" "$d/out"; then
    ok "net=off skips the interface entirely, both families, clearly reported"
else
    cat "$d/log" 2>/dev/null; cat "$d/out"; bad "net=off did not skip the interface as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== net-config.sh: ipv6=off actually disables IPv6 on the interface, not just skips configuring it =="
# A real, confirmed gap: the kernel auto-assigns a link-local address
# and processes Router Advertisements the moment an interface comes up,
# entirely independent of anything this script itself configures -
# "off" silently was NOT off before this fix. disable_ipv6 itself can't
# be observed from here (there's no real interface named eth0 in this
# test sandbox for /proc/sys/net/ipv6/conf/eth0/ to exist under), but
# the explicit log message it's paired with proves the code path that
# writes it was actually reached, not skipped.
run_net_config 'ALPINE_ZFSBOOT_IPV4=dhcp ALPINE_ZFSBOOT_IPV6=off'
if grep -qi "IPv6: off on eth0 (disabling IPv6 on the interface" "$d/out" 2>/dev/null; then
    ok "ipv6=off explicitly disables IPv6 on the interface (not just 'we configure nothing')"
else
    cat "$d/out"; bad "ipv6=off did not attempt to actually disable IPv6 on the interface"
fi
rm -rf "$d"

# =============================================================================
echo "== net-config.sh: the exact Hetzner headless-rescue config - ipv4=off, ipv6=static with a link-local gateway =="
# The specific real-world combination this project's own design notes
# now call out (see boot-dataset.sh) - an IPv6-only rescue SSH path,
# proven end-to-end on real Hetzner CAX hardware. ipv4=off must produce
# NO IPv4 activity at all (no DHCP client, no address, no route);
# ipv6=static must produce exactly the address+route and nothing about
# IPv6 being disabled (that's the ipv6=off case above, not this one).
d="$(fresh_env)"
run_net_config 'ALPINE_ZFSBOOT_IPV4=off ALPINE_ZFSBOOT_IPV6=static ALPINE_ZFSBOOT_IPV6_ADDRESS=2a01:4f8:1c1e:c1eb::2afa/64 ALPINE_ZFSBOOT_IPV6_GATEWAY=fe80::1'
if grep -q -- "^ip -6 addr add 2a01:4f8:1c1e:c1eb::2afa/64 dev eth0$" "$d/log" 2>/dev/null \
   && grep -q -- "^ip -6 route add default via fe80::1 dev eth0$" "$d/log" \
   && ! grep -q "^udhcpc \|^ip addr add\|^ip route add default via" "$d/log" \
   && ! grep -qi "disabling IPv6" "$d/out"; then
    ok "ipv4=off + ipv6=static (link-local gw) - IPv6-only, no IPv4 activity, IPv6 not disabled"
else
    cat "$d/log" 2>/dev/null; cat "$d/out"; bad "the IPv6-only Hetzner-style config did not behave exactly as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== net-config.sh: calling net_config a SECOND time on an already-configured interface is idempotent =="
# A real, confirmed gap: rescue-ssh.sh's own fail()-triggered retry can
# call net_config a second time on an interface THIS SAME PROCESS
# already configured (local unlock succeeds -> stop_rescue_ssh -> a
# LATER, unrelated failure -> fail() -> start_rescue_ssh again). A plain
# `ip addr add`/`ip route add` of something already present fails with
# "File exists" - this used to make the whole family (and therefore
# dropbear itself, under the old any-failure-fails scheme) look broken
# on exactly the boot that most needs it to work.
NET_CONFIG_STATE="$(mktemp)"
export NET_CONFIG_STATE
run_net_config 'ALPINE_ZFSBOOT_IPV4=off ALPINE_ZFSBOOT_IPV6=static ALPINE_ZFSBOOT_IPV6_ADDRESS=2001:db8::5/64 ALPINE_ZFSBOOT_IPV6_GATEWAY=2001:db8::1'
first_status=$net_config_exit
first_log="$(cat "$d/log" 2>/dev/null)"
rm -rf "$d"
run_net_config 'ALPINE_ZFSBOOT_IPV4=off ALPINE_ZFSBOOT_IPV6=static ALPINE_ZFSBOOT_IPV6_ADDRESS=2001:db8::5/64 ALPINE_ZFSBOOT_IPV6_GATEWAY=2001:db8::1'
second_status=$net_config_exit
if [ "$second_status" = 0 ] && grep -qi "already configured" "$d/out" 2>/dev/null \
   && grep -qi "already present" "$d/out"; then
    ok "second net_config call on the same interface succeeds cleanly (address+route already present, not re-added)"
else
    echo "first_status=$first_status second_status=$second_status"; echo "$first_log"; cat "$d/out" 2>/dev/null
    bad "second net_config call did not behave idempotently"
fi
rm -f "$NET_CONFIG_STATE"
unset NET_CONFIG_STATE
rm -rf "$d"

# =============================================================================
echo "== net-config.sh: one family fails, the other succeeds -> net_config still returns success =="
# The actual bug this whole rescue-ssh integration depends on: the old
# scheme failed the WHOLE call if ANY family had a problem, which meant
# rescue-ssh.sh's own \`if ! net_config eth0\` refused to start dropbear
# even when the working family was perfectly reachable. Forcing IPv6's
# address-add to fail here while IPv4 dhcp succeeds must still return 0.
run_net_config 'ALPINE_ZFSBOOT_IPV4=dhcp ALPINE_ZFSBOOT_IPV6=static ALPINE_ZFSBOOT_IPV6_ADDRESS=2001:db8::5/64 STUB_IP_FAIL_V6ADDR=1'
if [ "$net_config_exit" = 0 ] && grep -q "^udhcpc -i eth0 -n -q$" "$d/log" 2>/dev/null \
   && grep -qi "failed to add IPv6 address" "$d/out"; then
    ok "IPv6 address-add failing does not stop net_config reporting overall success when IPv4 (dhcp) worked"
else
    cat "$d/log" 2>/dev/null; cat "$d/out"; bad "a single-family failure incorrectly failed the whole net_config call"
fi
rm -rf "$d"

# =============================================================================
echo "== net-config.sh: EVERY requested family failing -> net_config genuinely returns failure =="
# The other half of the same fix - this must NOT become "never fails at
# all". Both families explicitly requested (not off), both forced to
# fail their address configuration.
run_net_config 'ALPINE_ZFSBOOT_IPV4=static ALPINE_ZFSBOOT_IPV4_ADDRESS=203.0.113.5/24 ALPINE_ZFSBOOT_IPV6=static ALPINE_ZFSBOOT_IPV6_ADDRESS=2001:db8::5/64 STUB_IP_FAIL_V4ADDR=1 STUB_IP_FAIL_V6ADDR=1'
if [ "$net_config_exit" != 0 ]; then
    ok "net_config genuinely fails when every requested family fails (not silently always-succeeding)"
else
    cat "$d/log" 2>/dev/null; cat "$d/out"; bad "net_config reported success even though every requested family failed"
fi
rm -rf "$d"

# =============================================================================
echo "== net-config.sh: rescue-ssh.sh refuses to start dropbear when net_config genuinely fails completely =="
d="$(fresh_env)"
(
    set +e
    ip()       { "$STUBS/ip" "$@"; }
    ifconfig() { "$STUBS/ifconfig" "$@"; }
    udhcpc()   { "$STUBS/udhcpc" "$@"; }
    udhcpc6()  { "$STUBS/udhcpc6" "$@"; }
    dropbear() { "$STUBS/dropbear" "$@"; }
    dropbearkey() { "$STUBS/dropbearkey" "$@"; }
    STUB_LOG="$d/log" STUB_ROOT="$d/root"
    export STUB_LOG STUB_ROOT
    stage_rescue_ssh "$d/root"
    . "$REPO_ROOT/init/rescue-ssh.sh"
    ALPINE_ZFSBOOT_IPV4=static ALPINE_ZFSBOOT_IPV4_ADDRESS=203.0.113.5/24 \
    ALPINE_ZFSBOOT_IPV6=static ALPINE_ZFSBOOT_IPV6_ADDRESS=2001:db8::5/64 \
    STUB_IP_FAIL_V4ADDR=1 STUB_IP_FAIL_V6ADDR=1 \
        start_rescue_ssh
) >"$d/out" 2>&1 || true
if ! grep -q "^dropbear " "$d/log" 2>/dev/null && grep -qi "network bring-up failed" "$d/out"; then
    ok "dropbear correctly never starts when net_config genuinely has no working family at all"
else
    cat "$d/log" 2>/dev/null; cat "$d/out"; bad "dropbear started (or the right message was missing) despite total network failure"
fi
rm -rf "$d"

# =============================================================================
echo "== net-config.sh: gateway outside the configured prefix falls back to an on-link host route =="
# A real, documented convention, not a hypothetical: Hetzner Cloud's own
# IPv4 setup is a /32 address with a gateway that's unreachable as a
# plain default route without an explicit on-link host route first -
# their own netplan config carries \`on-link: true\` for exactly this.
# STUB_IP_FAIL_V4ROUTE=once makes the FIRST \`route add default\` attempt
# fail (simulating the real "gateway unreachable" rejection), so the
# on-link fallback (host route, then retry) is what has to succeed.
run_net_config 'ALPINE_ZFSBOOT_IPV4=static ALPINE_ZFSBOOT_IPV4_ADDRESS=203.0.113.5/32 ALPINE_ZFSBOOT_IPV4_GATEWAY=198.51.100.1 STUB_IP_FAIL_V4ROUTE=once'
if grep -q -- "^ip route add 198.51.100.1 dev eth0$" "$d/log" 2>/dev/null \
   && grep -q -- "^ip route add default via 198.51.100.1 dev eth0$" "$d/log" \
   && grep -qi "on-link fallback" "$d/out"; then
    ok "an off-prefix IPv4 gateway falls back to an explicit on-link host route, then succeeds"
else
    cat "$d/log" 2>/dev/null; cat "$d/out"; bad "the on-link fallback for an off-prefix gateway did not behave as expected"
fi
rm -rf "$d"

# =============================================================================
echo "== net-config.sh: IPv6 static with a link-local gateway passes an explicit dev to the route =="
# A real, previously-untested gap: `ip -6 route add default via fe80::1`
# with NO dev - the kernel rejects a link-local (fe80::/10) next-hop
# with no interface/scope attached ("Invalid argument"), since the same
# fe80::-range address is valid, unrelated, on every link at once. This
# is not a rare edge case - it's the standard IPv6 gateway convention
# several real hosting providers use, confirmed hit for real setting up
# a machine's own ESP config this same session.
d="$(fresh_env)"
(
    set +e
    ip()       { "$STUBS/ip" "$@"; }
    ifconfig() { "$STUBS/ifconfig" "$@"; }
    udhcpc()   { "$STUBS/udhcpc" "$@"; }
    udhcpc6()  { "$STUBS/udhcpc6" "$@"; }
    STUB_LOG="$d/log"
    export STUB_LOG
    . "$REPO_ROOT/init/net-config.sh"
    ALPINE_ZFSBOOT_NET=static \
    ALPINE_ZFSBOOT_IPV4=off \
    ALPINE_ZFSBOOT_IPV6=static \
    ALPINE_ZFSBOOT_IPV6_ADDRESS="2001:db8::5/64" \
    ALPINE_ZFSBOOT_IPV6_GATEWAY="fe80::1" \
        net_config eth0
) >"$d/out" 2>&1 || true
if grep -q -- "^ip -6 route add default via fe80::1 dev eth0$" "$d/log" 2>/dev/null; then
    ok "IPv6 static + link-local gateway passes an explicit 'dev eth0', not just 'via fe80::1'"
else
    cat "$d/log" 2>/dev/null; bad "IPv6 default route was not given an explicit dev for the link-local gateway"
fi
rm -rf "$d"

# =============================================================================
echo "== net-config.sh: IPv4 static gateway also passes an explicit dev, for consistency =="
d="$(fresh_env)"
(
    set +e
    ip()       { "$STUBS/ip" "$@"; }
    ifconfig() { "$STUBS/ifconfig" "$@"; }
    udhcpc()   { "$STUBS/udhcpc" "$@"; }
    udhcpc6()  { "$STUBS/udhcpc6" "$@"; }
    STUB_LOG="$d/log"
    export STUB_LOG
    . "$REPO_ROOT/init/net-config.sh"
    ALPINE_ZFSBOOT_NET=static \
    ALPINE_ZFSBOOT_IPV4=static \
    ALPINE_ZFSBOOT_IPV4_ADDRESS="203.0.113.5/24" \
    ALPINE_ZFSBOOT_IPV4_GATEWAY="203.0.113.1" \
    ALPINE_ZFSBOOT_IPV6=off \
        net_config eth0
) >"$d/out" 2>&1 || true
if grep -q -- "^ip route add default via 203.0.113.1 dev eth0$" "$d/log" 2>/dev/null; then
    ok "IPv4 static gateway passes an explicit 'dev eth0' too"
else
    cat "$d/log" 2>/dev/null; bad "IPv4 default route was not given an explicit dev"
fi
rm -rf "$d"

# =============================================================================
echo
echo "$pass passed, $fail failed"
[ "$fail" = 0 ]
