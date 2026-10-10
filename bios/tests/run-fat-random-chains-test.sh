#!/usr/bin/env bash
# run-fat-random-chains-test.sh - fat.c against many random valid FAT32 cluster chains (see
# fat_random_chains_test.py). Builds the driver twice (FAT_IO_BATCH_SECTORS=64 and 36).
set -euo pipefail

cd "$(dirname "$0")/../.."
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

for n in 64 36; do
    gcc -O1 -Wall -Wextra -DFAT_IO_BATCH_SECTORS=$n -o "$workdir/driver$n" bios/tests/fat_random_chains_driver.c bios/fat.c -I bios
done
python3 bios/tests/fat_random_chains_test.py "$workdir" "${SEED:-1}" "${CASES:-30}" "$workdir/driver64" "$workdir/driver36"
