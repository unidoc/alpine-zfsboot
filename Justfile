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

alpine_version := `cat alpine-version.txt`
out_dir := env_var_or_default("OUT_DIR", "./out")
# Extra words appended to the built-in kernel cmdline (see build.sh) -
# for debugging boot problems (e.g. `nokaslr`, `efi=debug`) without
# editing build.sh itself. Empty by default.
extra_cmdline := env_var_or_default("EXTRA_CMDLINE", "")

default: (build "x86_64")

# just build ARCH
# EXTRA_CMDLINE=... just build ARCH  adds words to the kernel cmdline
# for this build only.
#
# Depends on build-tool: cmd/tool's own CLI binary gets baked straight
# into the rescue initramfs now (see build.sh's alpine-zfsboot.files
# entry for /boot/alpine-zfsboot and the cp just above it) rather
# than relying on `apk add alpine-zfsboot` at rescue-shell runtime,
# which would pull whatever version unidoc-aports last happened to
# package - this way the CLI baked into a given image is always built
# from the exact same commit as the image itself, no version-lag risk.
build ARCH: build-tool
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
#
# CGO_ENABLED=0 on both - amd64 is this host's own likely native arch
# on most dev machines, so without this it can silently cgo-link
# against the HOST's glibc instead of coming out static (confirmed via
# `file` on a real build: a plain `go build GOOS=linux GOARCH=amd64`
# here produced a dynamically-linked ELF needing
# /lib64/ld-linux-x86-64.so.2). Every real place this binary runs -
# the rescue initramfs `build ARCH` now bakes it into (see build.sh),
# any musl-based Alpine target OS - is musl, not glibc, so a
# dynamically-linked build would just fail to execute there outright.
# -X main.version=$(cat version.txt) - the same repo version.txt
# build.sh already threads into every OTHER artifact (/etc/alpine-
# zfsboot-version, cmdline.txt's alpine-zfsboot.version=, bios/
# Makefile's own ZFSBOOT_VERSION) - this recipe was the one place that
# never did, so `alpine-zfsboot --version` on a build produced this
# way (including the copy build.sh now bakes into the rescue image
# itself - see build.sh's own /boot/alpine-zfsboot comment) printed
# "dev" instead of anything real. A real git tag (release.yml's own
# build-tool job, `-X main.version=$GITHUB_REF_NAME`) still wins for
# an actual tagged release - this is specifically for everything else
# that isn't one, same as version.txt already is for the C/EFI side.
build-tool:
    mkdir -p "{{out_dir}}"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-X main.version=$(cat version.txt)" -o "{{out_dir}}/alpine-zfsboot-x86_64" ./cmd/tool
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "-X main.version=$(cat version.txt)" -o "{{out_dir}}/alpine-zfsboot-aarch64" ./cmd/tool

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

# The BIOS ISO-entry QEMU integration test (see that script's own
# header comment for the full why) - boots the real El-Torito
# bios/stage-iso.bin in QEMU and proves it can load a kernel+initrd
# through a non-NULL fat_read_range() progress callback, the regression
# guard for the %cs=SEG-at-entry bug found and fixed this same session
# (stage2_entry.S's own far jump). Needs xorriso, dosfstools
# (mkfs.vfat), mtools (mmd/mcopy), qemu-system-x86_64, python3, on top
# of what bios/Makefile already needs (gcc/binutils) - on Debian/Ubuntu:
#   sudo apt-get install -y xorriso dosfstools mtools qemu-system-x86 python3
test-iso-entry:
    ./tests/bios-iso-entry-test.sh

# just test-iso-atapi  - same real QEMU boot as test-iso-entry, but forces
# cdrom_disk.c's ATAPI backend (see cdrom_disk.c's own comment on
# ZFSBOOT_FORCE_ATAPI) - QEMU/SeaBIOS's INT13h normally succeeds, so a
# plain test-iso-entry run never actually exercises the ATAPI driver.
test-iso-atapi:
    FORCE_ATAPI=1 ./tests/bios-iso-entry-test.sh

# just test-hdd-entry  - real QEMU boot of the raw-disk path
# (bios/stage1.bin + bios/stage2.bin as a classic MBR hard disk, NOT
# the El-Torito ISO test-iso-entry exercises) - the only automated
# coverage of disk.c's own disk_read_lba() (distinct from
# cdrom_disk.c's ISO-only int13_read_native()) and of stage1.S's own
# STAGE2_MAGIC/retry logic (which has no ISO equivalent at all - the
# ISO build enters directly at stage2_entry.S, no stage1 of its own).
# Also exercises stage2_main.c's GPT-then-MBR fallback for real (this
# disk has no GPT header at all). Needs dosfstools (mkfs.vfat), mtools
# (mmd/mcopy), qemu-system-x86_64, python3, on top of what
# bios/Makefile already needs.
test-hdd-entry:
    ./tests/bios-hdd-entry-test.sh

# just test-ata-atapi-host  - host-native regression test for
# ata_atapi.c's own multi-phase transfer bookkeeping (see
# ata_atapi_host_test.c's own header comment).
test-ata-atapi-host:
    ./bios/tests/run-ata-atapi-host-test.sh

# just test-fat-host  - host-native (no -m16/-ffreestanding) build+run of
# fat.c's own regression suite (bios/tests/fat_host_test.c) - needs only
# gcc + python3, no Docker/Alpine container.
test-fat-host:
    ./bios/tests/run-fat-host-test.sh

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
