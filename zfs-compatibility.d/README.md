`zpool -o compatibility=<name>` feature-flag sets. Verbatim copies of
`cmd/zpool/compatibility.d/*` from upstream openzfs/zfs - unmodified
upstream content, not hand-written. Alpine's `zfs` package doesn't
ship this directory - confirmed empty on pkgs.alpinelinux.org - so
build.sh copies these in instead.

Only the Linux-relevant, non-FreeBSD `openzfs-*` files in this
directory (openzfs-2.0-linux through openzfs-2.4, as of this writing)
are kept up to date automatically, by
.github/scripts/check-zfs-compat.py - currently vendored from tag
`zfs-2.4.4` (Alpine 3.24's own `zfs` package version).

The REST of this directory (compat-*/freebsd-*/freenas-*/grub2-*/
openzfsonosx-*/zol-*) was vendored once, by hand, when this project
started, and is never re-fetched or re-verified by that script - its
provenance is whatever it was at that time, not tracked against any
tag here.
