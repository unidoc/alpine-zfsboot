#!/usr/bin/env python3
"""Usage: fix-kexec-dtb.py OUTPUT_PATH

Real-hardware-found bug (a QEMU/KVM aarch64 "virt" board, Netcup,
2026-09-25): QEMU's virt machine injects a random, nonzero
/chosen/kaslr-seed property into the device tree it hands the kernel.
A kernel built WITH CONFIG_RANDOMIZE_BASE reads that value once at boot
and zeroes it back out - but Alpine's own aarch64 kernel builds do not
set CONFIG_RANDOMIZE_BASE (confirmed against the real kernel config
fragments), so the seed is never consumed. kexec-tools' arm64 backend
(kexec/arch/arm64/kexec-arm64.c, setup_2nd_dtb()) treats a nonzero
kaslr-seed still sitting in the CURRENT kernel's own live device tree
(/sys/firmware/fdt, what kexec -l reads by default) as a sign that the
device tree is stale/unsafe to reuse and refuses to proceed - silently:
that failure path is gated behind kexec's own -d/--debug output, so a
plain `kexec -l` just prints "kexec: setup_2nd_dtb failed." with no
indication of why.

This is unrelated to /proc/kcore - the VA_BITS-from-kcore warnings a
plain `kexec -l` also prints on this same kernel are a real, but
entirely non-fatal, fallback path (kexec-tools falls back to reading
_stext from /proc/kallsyms instead, which works fine here) - a red
herring that took real investigation to rule out. See this project's
own PR/issue tracker for the full writeup.

This script reads the CURRENTLY RUNNING kernel's own live device tree
(/sys/firmware/fdt - the exact same one kexec's own read_1st_dtb()
would otherwise auto-discover and use unmodified), and if /chosen/
kaslr-seed is present and nonzero, writes a byte-identical copy with
ONLY that property's bytes zeroed (same total file size - no need to
rebuild any offsets, since the fix never changes the property's length)
to OUTPUT_PATH, then prints OUTPUT_PATH on stdout and exits 0. The
caller (boot-dataset.sh) then passes that path to kexec's own --dtb=.

Deliberately conservative about when it does something:
  - No /sys/firmware/fdt at all (e.g. a BIOS/ACPI x86_64 boot, or an
    aarch64 platform that doesn't expose one): prints nothing, exits 1.
    The caller must treat this as "nothing to do here", not a fatal
    error - kexec's own default DTB auto-discovery is completely fine
    on every platform this doesn't apply to.
  - /chosen/kaslr-seed absent, or already all zero bytes: prints
    nothing, exits 0. Nothing needs fixing; passing an unmodified copy
    through --dtb= would be pointless - the original auto-discovery
    path already does the exact same thing kexec would do on its own.
  - Any parse error (a genuinely malformed FDT - should never happen in
    practice, since this is the kernel's OWN live tree, but this only
    ever gates an optional workaround, never boot itself): prints
    nothing to stdout, a diagnostic to stderr, exits 1. The caller must
    fall back to no --dtb= override, exactly as if this script did not
    exist at all - a bug in this workaround must never be able to turn
    a kexec that would otherwise have worked (kernels WITH
    CONFIG_RANDOMIZE_BASE, or a kaslr-seed that's already zero) into
    one that doesn't.

Pure stdlib (struct only) - no dtc/pylibfdt dependency, since this
needs to run inside alpine-zfsboot's own minimal rescue initramfs,
which carries neither. Implements exactly the one FDT operation this
needs (find one property under one top-level node, by name, and
overwrite its value bytes in place) rather than a general FDT
read/write library - see the Flattened Devicetree spec, s5.3 and
s5.4.4, for the structure-block token format this walks.
"""
import os
import struct
import sys

FDT_MAGIC = 0xD00DFEED
FDT_BEGIN_NODE = 1
FDT_END_NODE = 2
FDT_PROP = 3
FDT_NOP = 4
FDT_END = 9

# Overridable only for tests (tests/run-tests.sh feeds real dtc-compiled
# fixtures through this) - production callers never set this, so the
# real /sys/firmware/fdt is always what actually runs at boot.
LIVE_FDT_PATH = os.environ.get("FIX_KEXEC_DTB_TEST_INPUT", "/sys/firmware/fdt")


def find_and_zero_kaslr_seed(data):
    """data: a bytearray of a full FDT blob (mutated in place on
    success). Returns True if a nonzero /chosen/kaslr-seed was found
    and zeroed, False if there was nothing to do (property absent, or
    already all zero). Raises on a genuinely malformed FDT - the
    caller decides what "malformed" means for exit-status purposes.
    """
    (magic, _totalsize, off_dt_struct, off_dt_strings, _off_mem_rsvmap,
     _version, _last_comp_version, _boot_cpuid_phys, _size_dt_strings,
     size_dt_struct) = struct.unpack(">10I", data[0:40])
    if magic != FDT_MAGIC:
        raise ValueError(f"not an FDT blob (bad magic {magic:#x})")

    pos = off_dt_struct
    end = off_dt_struct + size_dt_struct
    path_stack = []
    found_offset = None
    found_len = None

    while pos < end:
        tag = struct.unpack_from(">I", data, pos)[0]
        pos += 4
        if tag == FDT_BEGIN_NODE:
            nul = data.index(b"\0", pos)
            path_stack.append(data[pos:nul].decode())
            pos = (nul + 1 + 3) & ~3
        elif tag == FDT_END_NODE:
            path_stack.pop()
        elif tag == FDT_PROP:
            plen, nameoff = struct.unpack_from(">II", data, pos)
            pos += 8
            strp = off_dt_strings + nameoff
            nul = data.index(b"\0", strp)
            propname = data[strp:nul].decode()
            if (path_stack and path_stack[-1] == "chosen"
                    and propname == "kaslr-seed"):
                found_offset, found_len = pos, plen
            pos = (pos + plen + 3) & ~3
        elif tag == FDT_NOP:
            pass
        elif tag == FDT_END:
            break
        else:
            raise ValueError(f"unrecognized FDT structure token {tag} at offset {pos - 4}")

    if found_offset is None:
        return False
    if all(b == 0 for b in data[found_offset:found_offset + found_len]):
        return False

    for i in range(found_len):
        data[found_offset + i] = 0
    return True


def main():
    if len(sys.argv) != 2:
        print("Usage: fix-kexec-dtb.py OUTPUT_PATH", file=sys.stderr)
        return 1

    output_path = sys.argv[1]
    try:
        with open(LIVE_FDT_PATH, "rb") as f:
            data = bytearray(f.read())
    except OSError as e:
        print(f"fix-kexec-dtb.py: no {LIVE_FDT_PATH} ({e}) - nothing to do", file=sys.stderr)
        return 1

    try:
        changed = find_and_zero_kaslr_seed(data)
    except (ValueError, IndexError) as e:
        print(f"fix-kexec-dtb.py: could not parse {LIVE_FDT_PATH} ({e}) - leaving kexec's own DTB auto-discovery alone", file=sys.stderr)
        return 1

    if not changed:
        # Nothing to fix - the original /sys/firmware/fdt kexec would
        # auto-discover anyway is already fine as-is.
        return 0

    with open(output_path, "wb") as f:
        f.write(data)
    print(output_path)
    return 0


if __name__ == "__main__":
    sys.exit(main())
