#!/usr/bin/env bash
# run-ata-atapi-host-test.sh - builds and runs ata_atapi_host_test.c,
# the regression guard for a real multi-phase-transfer buffer
# double-advance bug found in a full source audit (see
# ata_atapi_host_test.c's own header comment).
set -euo pipefail

cd "$(dirname "$0")/../.."
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

gcc -Wall -Wextra -o "$workdir/ata_atapi_host_test" bios/tests/ata_atapi_host_test.c bios/ata_atapi.c
"$workdir/ata_atapi_host_test"
