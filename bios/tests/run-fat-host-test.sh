#!/usr/bin/env bash
# run-fat-host-test.sh - builds and runs fat_host_test.c against fresh
# fixtures from build_fat_fixtures.py.
#
# This didn't exist until a full source audit found it: fat_host_test.c
# and build_fat_fixtures.py existed in the tree (bios/tests/, added
# earlier this session as the regression guard for a real
# Floyd's-tortoise-and-hare FAT-table-cache thrashing bug - see fat.c's
# own comment) but were wired into NOTHING - no bios/Makefile target, no
# Justfile recipe, no CI job. The tightened "<= 20 disk_read_lba() calls"
# assertion that actually catches that regression only ever ran when a
# human happened to type a `gcc` command by hand. This script, plus the
# `test-fat-host` Justfile recipe and CI job that call it, close that gap.
set -euo pipefail

cd "$(dirname "$0")/../.."
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

python3 bios/tests/build_fat_fixtures.py "$workdir"
gcc -Wall -Wextra -o "$workdir/fat_host_test" bios/tests/fat_host_test.c bios/fat.c -I bios
"$workdir/fat_host_test" "$workdir"
