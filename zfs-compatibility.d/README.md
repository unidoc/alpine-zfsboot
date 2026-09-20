`zpool -o compatibility=<name>` feature-flag sets. Verbatim copies of
`cmd/zpool/compatibility.d/*` from upstream openzfs/zfs, tag
`zfs-2.4.4` (Alpine 3.24's own `zfs` package version). Alpine's `zfs`
package doesn't ship this directory - confirmed empty on
pkgs.alpinelinux.org - so build.sh copies these in instead. Unmodified
upstream content, not hand-written.
