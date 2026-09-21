# =============================================================================
# alpine-zfsboot — builds the .EFI images (x86_64 + aarch64)
# =============================================================================
# just build x86_64            → alpine-zfsboot-x86_64.EFI
# just build aarch64           → alpine-zfsboot-aarch64.EFI
# just build-all                → both arches
#
# One build per arch, always both consoles active at boot - menu.py
# picks whichever was last used, or the first found, and can switch
# live between them, FreeBSD-loader-style (see init/init's
# select_console() and menu.py's switch_console()). An earlier version
# of this project also built separate single-console (vga-only/
# serial-only) variants, selectable via a CONSOLE build-time choice -
# dropped: those never bought anything the always-both-consoles build
# didn't already handle at runtime, just tripled the release matrix.
#
# Docker only - runs build.sh inside a real `alpine:{{alpine_version}}`
# container (no cross-compilation happening: mkinitfs/objcopy/kernel
# install are all prebuilt-.apk-package operations, so a plain `docker
# run --platform` with QEMU is fine locally even for the non-native
# arch - CI uses a real native aarch64 runner instead, see
# .github/workflows/release.yml, matching alpine-installer's own
# reasoning for why its aarch64 job needs one).
# =============================================================================

alpine_version := "3.24"
out_dir := env_var_or_default("OUT_DIR", "./out")
# Extra words appended to the built-in kernel cmdline (see build.sh) -
# for debugging boot problems (e.g. `nokaslr`, `efi=debug`) without
# editing build.sh itself. Empty by default.
extra_cmdline := env_var_or_default("EXTRA_CMDLINE", "")

default: (build "x86_64")

# just build ARCH
# EXTRA_CMDLINE=... just build ARCH  adds words to the kernel cmdline
# for this build only.
build ARCH:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p "{{out_dir}}"
    docker run --rm \
        --platform "linux/{{ARCH}}" \
        -v "$PWD:/work" -w /work \
        -e OUT_DIR=/work/{{out_dir}} \
        -e EXTRA_CMDLINE="{{extra_cmdline}}" \
        alpine:{{alpine_version}} sh -c '
            set -eu
            apk update
            apk add --no-cache ca-certificates-bundle mkinitfs zfs zfs-lts linux-lts kexec-tools ncurses-terminfo-base xz dialog gnu-efi gnu-efi-dev gcc musl-dev make binutils python3 bash dropbear dropbear-dbclient xorriso dosfstools mtools sgdisk
            ./build.sh
        '

# Both arches - two .EFI files in {{out_dir}}.
build-all:
    just build x86_64
    just build aarch64

# cmd/tool (the install-time version/update helper) - plain Go
# cross-compilation, no Docker/Alpine container needed at all, same
# as release.yml's own build-tool job.
build-tool:
    mkdir -p "{{out_dir}}"
    GOOS=linux GOARCH=amd64 go build -o "{{out_dir}}/alpine-zfsboot-x86_64" ./cmd/tool
    GOOS=linux GOARCH=arm64 go build -o "{{out_dir}}/alpine-zfsboot-aarch64" ./cmd/tool

clean:
    rm -rf "{{out_dir}}"
    make -C bios clean

# Shell-level unit tests (init/init, boot-dataset.sh, alpine-zfsboot-shell,
# menu.py's non-dialog helpers) - runs directly on this machine, no
# Docker/Alpine container, no real hardware. Same thing CI's own
# unit-tests job runs.
test:
    ./tests/run-tests.sh

# The kernel/QEMU integration test (see that script's own header
# comment for the full why) - boots a real Linux kernel against a real,
# hand-built concatenated initramfs to prove the encrypted-root kexec
# handoff's own overwrite mechanism actually works, not just that its
# own shell-level round-trip check agrees with itself. Needs
# qemu-system-x86_64, a real `cpio`, a static busybox, and a kernel
# image - on Debian/Ubuntu:
#   sudo apt-get install -y qemu-system-x86 cpio busybox-static
#   sudo apt-get install -y --download-only linux-image-amd64  # or:
#   apt-get download $(apt-cache depends linux-image-amd64 | awk '/Depends:/{print $2; exit}')
#   dpkg -x linux-image-*.deb /tmp/kernel-extract
# then: KERNEL_IMAGE=/tmp/kernel-extract/boot/vmlinuz-* just test-kernel
test-kernel:
    ./tests/kernel-integration-test.sh

# ── Release ──────────────────────────────────────────────────────────────────
#
# Same two-step, PR-based flow as isms/Justfile, same filename too
# (version.txt) - but unlike isms, this file is a real build input
# here, not just a review-gate artifact: build.sh reads it straight
# into /etc/alpine-zfsboot-version and cmdline.txt's own
# alpine-zfsboot.version= (see internal/cmdline's own comment). The
# pushed tag still versions the RELEASE itself (release.yml's `-X
# main.version=...` reads github.ref_name, independently) - version.txt
# is what ends up baked into the booted image and shown to an operator.
#
#   1. just release-pr 0.1.1   → branch + version.txt bump + PR (review, CI)
#   2. merge the PR
#   3. just release 0.1.1      → verifies master carries 0.1.1, signs the
#                                 tag, pushes - release.yml builds and
#                                 publishes everything.
#
# `release` never moves or replaces an existing tag - re-releasing the
# SAME version after a failed release BUILD (nothing in the repo
# changed, the tag just never got a working artifact) is not "just step
# 3": delete the tag first, on both sides, then step 3:
#
#   git push origin :refs/tags/v0.1.1 && git tag -d v0.1.1
#   just release 0.1.1
#
# The tag then points at whatever master is NOW, not at the commit the
# failed build actually ran against.

# Step 1: open the version-bump PR.
release-pr VERSION:
    #!/usr/bin/env bash
    set -euo pipefail
    [[ "{{VERSION}}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "✗ version must be X.Y.Z (no leading v), got '{{VERSION}}'"; exit 1; }
    [ -z "$(git status --porcelain --untracked-files=no)" ] || { echo "✗ uncommitted changes - commit or stash them first"; exit 1; }
    git fetch origin
    git checkout -b "release/v{{VERSION}}" origin/master
    echo "{{VERSION}}" > version.txt
    git add version.txt
    git commit -m "Release v{{VERSION}}"
    git push -u origin "release/v{{VERSION}}"
    gh pr create --title "Release v{{VERSION}}" \
        --body "Bumps version.txt to {{VERSION}}. After merge: \`just release {{VERSION}}\` tags master and CI publishes the release."
    git checkout -
    echo "✓ release PR opened — merge it, then run: just release {{VERSION}}"

# Step 2 (after the PR is merged): verify, tag master (signed), push.
# Refuses if the tag already exists - see the recovery sequence above
# if you're re-releasing the same version after a failed build.
release VERSION:
    #!/usr/bin/env bash
    set -euo pipefail
    [[ "{{VERSION}}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "✗ version must be X.Y.Z (no leading v), got '{{VERSION}}'"; exit 1; }
    [ -z "$(git status --porcelain --untracked-files=no)" ] || { echo "✗ uncommitted changes - commit or stash them first"; exit 1; }
    git checkout master
    git pull --ff-only
    git fetch --tags origin
    [ "$(tr -d '[:space:]' < version.txt)" = "{{VERSION}}" ] || \
        { echo "✗ version.txt is '$(cat version.txt)' — bump it first (just release-pr {{VERSION}})"; exit 1; }
    git rev-parse -q --verify "refs/tags/v{{VERSION}}" >/dev/null && { echo "✗ tag v{{VERSION}} already exists"; exit 1; }
    git tag -s "v{{VERSION}}" -m "v{{VERSION}}"
    git push origin "refs/tags/v{{VERSION}}"
    echo "✓ v{{VERSION}} tagged — CI builds the release: gh run watch"

help:
    @just --list
