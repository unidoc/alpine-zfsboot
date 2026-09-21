#!/bin/sh
# kernel-integration-test.sh - boots a REAL Linux kernel in QEMU against
# a REAL, hand-built concatenated initramfs (gzip-compressed segment +
# NUL padding + uncompressed newc segment), to prove the encrypted-root
# kexec handoff's core mechanism (init/boot-dataset.sh's own "Encrypted-
# root kexec handoff" block) actually works the way Linux's own kernel
# unpacks it - not just that boot-dataset.sh's own shell-level round-
# trip check (re-extracting its own just-built archive) agrees with
# itself.
#
# This exists because that shell-level check alone was NOT enough: a
# real hardware boot showed a "verified" archive having NO effect once
# handed to a real target kernel. The actual root cause, found by a
# from-scratch adversarial code review and confirmed against the
# kernel's own init/initramfs.c: the unpack loop only recognizes the
# START of a new uncompressed cpio segment at a 4-byte-aligned offset -
# a plain `cat compressed-segment uncompressed-segment` (what boot-
# dataset.sh used to do) silently fails that check whenever the first
# segment's own byte length isn't already a multiple of 4 (true roughly
# 3 times out of 4), with NO error anywhere - the kernel just keeps
# whatever the first segment already unpacked. Confirmed for real in
# this exact test, in QEMU, against a real (Debian) kernel: the
# unpadded case prints the kernel's own "Initramfs unpacking failed:
# invalid magic at start of compressed archive" and the overwrite never
# takes effect; the padded case succeeds.
#
# Deliberately generic (a plain Debian kernel + busybox, not Alpine's
# own kernel/mkinitfs/zfs) - the bug being tested is in Linux's own
# init/initramfs.c, identical on every kernel regardless of
# distribution, so a minimal, fast-to-obtain kernel proves exactly as
# much here as Alpine's own would. Alpine-SPECIFIC assumptions (the
# real usr/sbin/zfs path, prepare_zfs_root()'s own calling convention,
# -L file:// still being accepted) are a separate concern, already
# covered by build.sh's own "verify the encrypted-root kexec handoff's
# own assumptions" block (grep build.sh for that phrase) - this test's
# only job is the concatenation/overwrite mechanism itself.
#
# Needs: qemu-system-x86_64, a real `cpio` (not a busybox applet - see
# this project's own standing note elsewhere on why that distinction
# matters), a static busybox binary, and a bootable x86_64 bzImage.
# BUSYBOX_BIN and KERNEL_IMAGE may be set explicitly (CI does this,
# after `apt-get install busybox-static` and extracting a kernel
# package with `dpkg -x` rather than a full `apt-get install` of the
# kernel - see .github/workflows/ci.yml for why: a real `apt-get
# install` of a kernel package on a CI runner triggers an unnecessary,
# slow initramfs/bootloader update for a kernel that will never
# actually be booted on that machine).
set -eu

pass=0
fail=0
ok()  { pass=$((pass + 1)); echo "  ok - $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL - $1"; }

for cmd in qemu-system-x86_64 cpio dd; do
    command -v "$cmd" >/dev/null 2>&1 || { echo "missing required tool: $cmd" >&2; exit 1; }
done

BUSYBOX_BIN="${BUSYBOX_BIN:-$(command -v busybox 2>/dev/null || true)}"
if [ -z "$BUSYBOX_BIN" ] || [ ! -x "$BUSYBOX_BIN" ]; then
    echo "no busybox binary found - set BUSYBOX_BIN=/path/to/busybox (a static build - 'apt-get install busybox-static' on Debian/Ubuntu)" >&2
    exit 1
fi

KERNEL_IMAGE="${KERNEL_IMAGE:-}"
if [ -z "$KERNEL_IMAGE" ]; then
    KERNEL_IMAGE="$(find /boot -maxdepth 1 -name 'vmlinuz-*' 2>/dev/null | sort -V | tail -1)"
fi
if [ -z "$KERNEL_IMAGE" ] || [ ! -r "$KERNEL_IMAGE" ]; then
    echo "no kernel image found - set KERNEL_IMAGE=/path/to/vmlinuz" >&2
    exit 1
fi

echo "using kernel: $KERNEL_IMAGE"
echo "using busybox: $BUSYBOX_BIN"

W="$(mktemp -d)"
trap 'rm -rf "$W"' EXIT

mkdir -p "$W/orig/bin" "$W/orig/usr/sbin" "$W/handoff/usr/sbin"
cp "$BUSYBOX_BIN" "$W/orig/bin/busybox"
chmod 755 "$W/orig/bin/busybox"

# The "real target initrd" fixture - its own /init is the ONLY thing
# that ever runs; usr/sbin/zfs is a plain marker file (this test
# doesn't need it to be a real ELF binary - that specific check is
# boot-dataset.sh's own round-trip verification's job, already covered
# by tests/run-tests.sh - this test's only job is proving the kernel
# unpacks and overwrites correctly).
printf 'ORIGINAL-ZFS-BINARY-UNTOUCHED' > "$W/orig/usr/sbin/zfs"
chmod 755 "$W/orig/usr/sbin/zfs"
cat > "$W/orig/init" <<'EOF'
#!/bin/busybox sh
content="$(/bin/busybox cat /usr/sbin/zfs)"
if [ "$content" = "REPLACED-BY-HANDOFF-CPIO" ]; then
    echo "ALPINE-ZFSBOOT-QEMU-TEST: PASS - handoff segment overwrote usr/sbin/zfs as expected"
else
    echo "ALPINE-ZFSBOOT-QEMU-TEST: FAIL - usr/sbin/zfs content was: [$content]"
fi
EOF
chmod 755 "$W/orig/init"

# The handoff segment - overwrites ONLY usr/sbin/zfs, exactly matching
# boot-dataset.sh's own real handoff cpio's scope (it never touches
# /init at all - see that block's own design notes on why /init itself
# is the wrong interposition point).
printf 'REPLACED-BY-HANDOFF-CPIO' > "$W/handoff/usr/sbin/zfs"
chmod 755 "$W/handoff/usr/sbin/zfs"

( cd "$W/orig" && find . | cpio -o -H newc 2>/dev/null | gzip -9 > "$W/orig.cpio.gz" )
( cd "$W/handoff" && find . | cpio -o -H newc 2>/dev/null > "$W/handoff.cpio" )

# Positive case: correctly padded, matching boot-dataset.sh's own fix.
cat "$W/orig.cpio.gz" > "$W/combined-padded.img"
orig_size="$(wc -c < "$W/combined-padded.img")"
pad=$(( (4 - orig_size % 4) % 4 ))
[ "$pad" -gt 0 ] && dd if=/dev/zero bs=1 count="$pad" >> "$W/combined-padded.img" 2>/dev/null
cat "$W/handoff.cpio" >> "$W/combined-padded.img"

# Negative control: the ORIGINAL, broken behavior (plain concatenation,
# no alignment padding) - this MUST fail to overwrite usr/sbin/zfs,
# proving this test suite can actually tell a good archive from a bad
# one, not just always agree with whatever it's given. Deliberately
# forced to be non-4-byte-aligned rather than just "whatever orig_size
# naturally is": orig.cpio.gz's own compressed size depends on the
# exact gzip implementation/version, and has roughly a 1-in-4 chance of
# already landing on a 4-byte boundary by pure coincidence, regardless
# of environment - if that happens, plain concatenation is silently
# indistinguishable from the correctly-padded case, and this negative
# control stops proving anything at all. Real, not hypothetical: this
# is exactly what happened switching CI from a Debian to an Ubuntu
# kernel/toolchain - a different gzip produced a coincidentally-aligned
# orig.cpio.gz, and the unpadded case started passing right along with
# the padded one.
bad_pad=0
[ "$(($(wc -c < "$W/orig.cpio.gz") % 4))" -eq 0 ] && bad_pad=1
cat "$W/orig.cpio.gz" > "$W/combined-unpadded.img"
[ "$bad_pad" -gt 0 ] && dd if=/dev/zero bs=1 count="$bad_pad" >> "$W/combined-unpadded.img" 2>/dev/null
cat "$W/handoff.cpio" >> "$W/combined-unpadded.img"

kvm_args=""
if [ -w /dev/kvm ] 2>/dev/null; then
    kvm_args="-enable-kvm -cpu host"
fi

boot_and_capture() {
    # shellcheck disable=SC2086
    timeout 60 qemu-system-x86_64 \
        -kernel "$KERNEL_IMAGE" \
        -initrd "$1" \
        -append "console=ttyS0 panic=-1 quiet" \
        -m 256 -display none -serial stdio -monitor none -no-reboot \
        $kvm_args \
        2>&1 || true
}

echo "== booting with the correctly-padded combined initrd (expect PASS) =="
padded_out="$(boot_and_capture "$W/combined-padded.img")"
if echo "$padded_out" | grep -q "ALPINE-ZFSBOOT-QEMU-TEST: PASS"; then
    ok "real kernel correctly unpacked both segments and the handoff overwrote usr/sbin/zfs"
else
    echo "$padded_out" | tail -20
    bad "real kernel did NOT show the expected PASS sentinel with correct padding"
fi

echo "== booting with the deliberately UNPADDED combined initrd (expect FAIL/no overwrite - this is the bug that broke real hardware) =="
unpadded_out="$(boot_and_capture "$W/combined-unpadded.img")"
if echo "$unpadded_out" | grep -q "ALPINE-ZFSBOOT-QEMU-TEST: PASS"; then
    echo "$unpadded_out" | tail -20
    bad "unpadded archive unexpectedly still overwrote usr/sbin/zfs - this test can no longer distinguish good from bad padding, investigate before trusting the positive case above"
else
    ok "unpadded archive correctly failed to overwrite usr/sbin/zfs (matches the real hardware failure this was built to reproduce)"
fi

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
