#!/usr/bin/env bash
# run-e820-range-host-test.sh - builds and runs e820_range_host_test.c: the real
# e820_range.c (stage2's "is this destination RAM" checks) on the host.
set -euo pipefail

cd "$(dirname "$0")/../.."
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

gcc -Wall -Wextra -o "$workdir/e820_range_host_test" bios/tests/e820_range_host_test.c bios/e820_range.c -I bios
"$workdir/e820_range_host_test"
