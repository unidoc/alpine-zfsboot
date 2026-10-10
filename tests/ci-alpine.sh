#!/bin/sh
# ci-alpine.sh CMD... - runs CMD at the repo root inside
# alpine:<alpine-version.txt>, the same toolchain build.sh and the release
# build use (Alpine gcc/binutils on musl), with what the BIOS build and its
# QEMU/host tests need. CI runs every BIOS job through this: the stage2
# size limit (bios/Makefile, check-bss-bounds) depends on the compiler, and
# a Debian/Ubuntu gcc once passed where Alpine's failed.
set -eu
cd "$(dirname "$0")/.."
exec docker run --rm -v "$PWD:/work" -w /work \
    -e FORCE_ATAPI -e HDD_TEST_SCENARIOS -e OLD_STAGE_REF \
    "alpine:$(cat alpine-version.txt)" sh -c '
        set -e
        apk add --no-cache gcc musl-dev make binutils python3 bash coreutils \
            dosfstools mtools xorriso qemu-system-x86_64 git >/dev/null
        git config --global --add safe.directory /work
        exec "$@"' sh "$@"
