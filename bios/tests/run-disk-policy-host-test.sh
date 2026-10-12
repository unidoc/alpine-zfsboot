#!/usr/bin/env bash
# run-disk-policy-host-test.sh - builds and runs disk_policy_host_test.c: the real
# disk_read_lba() (disk_policy.c) against a fake INT 13h that rejects large
# transfers, fails on one sector, or reports short transfers.
set -euo pipefail

cd "$(dirname "$0")/../.."
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

gcc -Wall -Wextra -DDISK_POLICY_HOST_TEST -o "$workdir/disk_policy_host_test" bios/tests/disk_policy_host_test.c bios/disk_policy.c -I bios
"$workdir/disk_policy_host_test"
