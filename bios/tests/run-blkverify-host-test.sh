#!/usr/bin/env bash
# run-blkverify-host-test.sh - builds and runs blkverify_host_test.c: stage2's real
# per-block verify-and-heal (bios/blkverify.c) against a simulated disk that returns
# wrong data, and the BLKSUM format against the Go writer's golden files.
set -euo pipefail

cd "$(dirname "$0")/../.."
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

gcc -O1 -Wall -Wextra -o "$workdir/blkverify_host_test" bios/tests/blkverify_host_test.c bios/blkverify.c bios/payload_sum.c -I bios
"$workdir/blkverify_host_test" internal/payloadsum/testdata/golden.blksum.hex internal/payloadsum/testdata/golden.sum
