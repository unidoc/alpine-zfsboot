#!/usr/bin/env bash
# Builds and runs bootparams_host_test.c: checks that bootparams_build() fills
# the kernel's screen_info (VGA text console) so console=tty0 works on BIOS boots.
set -euo pipefail
cd "$(dirname "$0")/../.."
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT
gcc -Wall -Wextra -o "$workdir/bootparams_host_test" bios/tests/bootparams_host_test.c bios/bootparams.c -I bios
"$workdir/bootparams_host_test"
