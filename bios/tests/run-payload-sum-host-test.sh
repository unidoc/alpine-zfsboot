#!/usr/bin/env bash
# run-payload-sum-host-test.sh - builds and runs payload_sum_host_test.c: stage2's
# real CHECKSUM parser and SHA-256 (bios/payload_sum.c) on the host, against
# sha256sum and against the Go writer's golden file.
set -euo pipefail

cd "$(dirname "$0")/../.."
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

gcc -Wall -Wextra -o "$workdir/payload_sum_host_test" bios/tests/payload_sum_host_test.c bios/payload_sum.c -I bios
head -c 3000001 /dev/urandom > "$workdir/big"
sum=$(sha256sum "$workdir/big" | cut -d' ' -f1)
"$workdir/payload_sum_host_test" "$workdir/big" "$sum" internal/payloadsum/testdata/golden.sum
