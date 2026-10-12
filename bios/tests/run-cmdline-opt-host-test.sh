#!/usr/bin/env bash
# run-cmdline-opt-host-test.sh - builds and runs cmdline_opt_host_test.c: stage2's
# real cmdline key lookup and int13chunk= parsing (bios/cmdline_opt.c) on the host.
set -euo pipefail

cd "$(dirname "$0")/../.."
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

gcc -Wall -Wextra -o "$workdir/cmdline_opt_host_test" bios/tests/cmdline_opt_host_test.c bios/cmdline_opt.c -I bios
"$workdir/cmdline_opt_host_test"
