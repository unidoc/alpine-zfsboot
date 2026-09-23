#!/usr/bin/env python3
"""check-zfs-compat.py - vendors zfs-compatibility.d/ data for whatever
OpenZFS release line Alpine's own zfs package currently ships (per
alpine-version.txt), copied verbatim from openzfs/zfs's own
cmd/zpool/compatibility.d/ directory at the matching git tag. Never
invents or regenerates feature data - every byte written here is a
straight copy from a real upstream tag, the same provenance guarantee
zfs-compatibility.d/README.md already documents (see that file, and
zfscompat.go's own package comment on why this data is trustworthy at
all).

Only ever ADDS a new release line's own map entry/go:embed line to
zfscompat.go when the entry doesn't already exist - never removes or
reorders anything else in that file, and never touches the semantics
zfscompat.go's own package comment documents (active-only comparison,
etc.) - those were verified once, by hand, against OpenZFS source, not
something this script re-derives per run.

Prints "CHANGED_FILES=..." (space-separated, repo-relative) to stdout
and exits 0 if anything changed; prints a one-line "up to date" message
and exits 0 with no CHANGED_FILES line if not. Never touches git -
the caller (check-zfs-compat.yml) commits whatever's left in the
working tree.
"""
import base64
import json
import os
import re
import subprocess
import sys
import tarfile
import io
import urllib.request

GH_API = "https://api.github.com"
UPSTREAM_REPO = "openzfs/zfs"


def repo_root():
    return subprocess.check_output(
        ["git", "rev-parse", "--show-toplevel"], text=True
    ).strip()


def gh_get(url):
    headers = {
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
        "User-Agent": "alpine-zfsboot-check-zfs-compat",
    }
    token = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN")
    if token:
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(url, headers=headers)
    with urllib.request.urlopen(req) as r:
        return json.load(r)


def alpine_zfs_pkgver(alpine_branch):
    url = (
        f"https://dl-cdn.alpinelinux.org/alpine/v{alpine_branch}"
        "/main/x86_64/APKINDEX.tar.gz"
    )
    with urllib.request.urlopen(url) as r:
        data = r.read()
    tf = tarfile.open(fileobj=io.BytesIO(data))
    idx = tf.extractfile("APKINDEX").read().decode()
    # Each blank-line-delimited record starts with a C: checksum line, THEN
    # P:<pkgname> - not P: first, so match on any line in the block being
    # exactly "P:zfs" rather than assuming block position.
    for block in idx.split("\n\n"):
        if re.search(r"^P:zfs$", block, re.M):
            m = re.search(r"^V:(\S+)$", block, re.M)
            if not m:
                sys.exit("check-zfs-compat: zfs package entry has no V: line")
            return m.group(1)
    sys.exit(f"check-zfs-compat: no 'zfs' package in Alpine v{alpine_branch}'s APKINDEX")


def parse_known_release_lines(path):
    src = open(path).read()
    m = re.search(
        r"var zfsCompatReleaseLineFile = map\[string\]string\{(.*?)\n\}", src, re.S
    )
    if not m:
        sys.exit(
            "check-zfs-compat: could not find zfsCompatReleaseLineFile map in "
            "zfscompat.go - its format changed, needs a human look"
        )
    return set(re.findall(r'"([\d.]+)":\s*"zfs-compatibility\.d/', m.group(1)))


def add_release_lines_to_go(path, new_lines):
    """Returns the new file content - does not write anything. Callers
    write only after every computed edit (this and
    compute_readme_provenance below) has succeeded, so a sys.exit partway
    through never leaves the working tree half-updated (see this script's
    own module docstring)."""
    src = open(path).read()

    for line, name in sorted(new_lines):
        directive = f"//go:embed zfs-compatibility.d/{name}\n"
        if directive not in src:
            embeds = list(
                re.finditer(r"^//go:embed zfs-compatibility\.d/.*\n", src, re.M)
            )
            if not embeds:
                sys.exit(
                    "check-zfs-compat: no existing //go:embed lines in "
                    "zfscompat.go - format changed, needs a human look"
                )
            insert_at = embeds[-1].end()
            for e in embeds:
                if e.group() > directive:
                    insert_at = e.start()
                    break
            src = src[:insert_at] + directive + src[insert_at:]

        m = re.search(
            r"var zfsCompatReleaseLineFile = map\[string\]string\{\n(.*?)\n\}",
            src,
            re.S,
        )
        body = m.group(1)
        if f'"{line}":' in body:
            continue
        entries = [e for e in body.split("\n") if e.strip()]
        entries.append(f'\t"{line}": "zfs-compatibility.d/{name}",')
        entries.sort(
            key=lambda e: [int(x) for x in re.search(r'"([\d.]+)"', e).group(1).split(".")]
        )
        src = src[: m.start(1)] + "\n".join(entries) + src[m.end(1) :]

    return src


def compute_readme_provenance(path, tag, alpine_branch):
    """Returns the new file content - does not write anything, see
    add_release_lines_to_go's own docstring for why.

    Only rewrites the ONE sentence that describes the openzfs-*
    (Linux, non-FreeBSD) subset this script itself vendors - the
    subset `relevant` in main() actually re-fetches and compares
    byte-for-byte every run (openzfs-2.0-linux through openzfs-2.4, as
    of this writing - see this script's own module docstring). A full
    source audit found the previous version of this README sentence,
    and this function, treated "tag `zfs-X.Y.Z`" as a whole-directory
    claim - but zfs-compatibility.d/ also holds ~19 other files
    (compat-2018..2021, freebsd-*, freenas-*, grub2-*, openzfsonosx-*,
    zol-*) that this script has NEVER touched, filtered out entirely
    by main()'s own `relevant` list (name must start with "openzfs-"
    and not end with "-freebsd"). Every run that changed even ONE of
    the openzfs-* files rewrote that one global sentence to claim the
    WHOLE directory - including all ~19 untouched files - now matched
    the new tag, which was never true: those files' real provenance is
    whatever it was when they were first vendored, not re-verified
    here. The regexes below are scoped to match only the sentence
    naming the openzfs-* subset specifically (see the matching
    zfs-compatibility.d/README.md wording), not a bare "tag" anywhere
    in the file, and use \\s+ rather than a literal newline so a future
    reflow/rewrap of this paragraph doesn't silently stop this from
    matching (a real, if narrower, form of the same "auto-edit becomes
    silently wrong" risk this function's own sys.exit below already
    guards against for a full rewording).
    """
    text = open(path).read()
    new_text, n1 = re.subn(
        r"vendored from tag\s*\n?\s*`zfs-[\d.]+`", f"vendored from tag\n`{tag}`", text
    )
    new_text, n2 = re.subn(
        r"Alpine \d+\.\d+'s own `zfs` package version",
        f"Alpine {alpine_branch}'s own `zfs` package version",
        new_text,
    )
    if n1 != 1 or n2 != 1:
        sys.exit(
            "check-zfs-compat: zfs-compatibility.d/README.md's openzfs-* "
            "provenance sentence didn't match the expected shape (found "
            f"tag refs: {n1}, Alpine-version refs: {n2}) - it may have been "
            "reworded; needs a human look rather than a silently-wrong "
            "auto-edit (and rather than falsely re-dating it, or any of "
            "the ~19 other, unrelated vendored files in this directory "
            "this script never touches)"
        )
    return new_text


def main():
    root = repo_root()
    compat_dir = os.path.join(root, "zfs-compatibility.d")
    zfscompat_go = os.path.join(root, "zfscompat.go")
    readme = os.path.join(compat_dir, "README.md")

    alpine_branch = open(os.path.join(root, "alpine-version.txt")).read().strip()
    raw_pkgver = alpine_zfs_pkgver(alpine_branch)  # e.g. "2.4.4-r0"
    pkgver = raw_pkgver.split("-r")[0]  # -> "2.4.4"
    tag = f"zfs-{pkgver}"

    # Confirm the tag actually exists before trusting anything fetched "at"
    # it - Alpine occasionally carries a patched/backported pkgver upstream
    # never tagged verbatim, and silently falling back to a nearby tag
    # would be exactly the kind of unproven substitution this project has
    # avoided everywhere else in this data (see zfscompat.go's own package
    # comment).
    try:
        gh_get(f"{GH_API}/repos/{UPSTREAM_REPO}/git/refs/tags/{tag}")
    except Exception as e:
        sys.exit(
            f"check-zfs-compat: openzfs/zfs has no tag {tag!r} matching "
            f"Alpine's zfs {raw_pkgver} - needs a human look: {e}"
        )

    listing = gh_get(
        f"{GH_API}/repos/{UPSTREAM_REPO}/contents/cmd/zpool/compatibility.d?ref={tag}"
    )
    # Linux-relevant only, matching zfscompat.go's own doc comment on what
    # this project has any use for - derived from the REAL directory
    # listing at this tag, not a guessed "openzfs-X.Y[-linux]" pattern:
    # upstream's own naming isn't uniform (2.0/2.1 carry an explicit
    # -linux suffix, 2.2+ don't).
    relevant = [
        e
        for e in listing
        if e["name"].startswith("openzfs-") and not e["name"].endswith("-freebsd")
    ]
    if not relevant:
        sys.exit(
            "check-zfs-compat: found zero openzfs-*-non-freebsd entries at "
            f"{tag} - upstream's compatibility.d layout likely changed, "
            "needs a human look"
        )

    known_lines = parse_known_release_lines(zfscompat_go)
    new_lines = []  # (release_line, filename) not yet in zfscompat.go
    # (path, bytes) pairs, computed in full before anything is written -
    # see add_release_lines_to_go's own docstring for why nothing here
    # touches disk until every edit below has succeeded.
    pending_writes = []
    changed_files = []

    for entry in relevant:
        name = entry["name"]
        content = base64.b64decode(
            gh_get(
                f"{GH_API}/repos/{UPSTREAM_REPO}/contents/cmd/zpool/compatibility.d/{name}?ref={tag}"
            )["content"]
        )
        local_path = os.path.join(compat_dir, name)
        local_content = (
            open(local_path, "rb").read() if os.path.exists(local_path) else None
        )
        if local_content == content:
            continue

        pending_writes.append((local_path, content))
        changed_files.append(os.path.relpath(local_path, root))

        line = name[len("openzfs-") :]
        if line.endswith("-linux"):
            line = line[: -len("-linux")]
        if line not in known_lines:
            new_lines.append((line, name))

    if not changed_files:
        print(f"check-zfs-compat: up to date (Alpine zfs {raw_pkgver} / {tag})")
        return

    if new_lines:
        new_go_src = add_release_lines_to_go(zfscompat_go, new_lines)
        pending_writes.append((zfscompat_go, new_go_src.encode()))
        changed_files.append(os.path.relpath(zfscompat_go, root))

    new_readme = compute_readme_provenance(readme, tag, alpine_branch)
    pending_writes.append((readme, new_readme.encode()))
    changed_files.append(os.path.relpath(readme, root))

    # Every edit above succeeded - only now does anything touch disk, all
    # or nothing.
    for path, data in pending_writes:
        with open(path, "wb") as f:
            f.write(data)

    print("CHANGED_FILES=" + " ".join(changed_files))
    print("TAG=" + tag)
    print("ALPINE_ZFS_VERSION=" + raw_pkgver)
    print("NEW_RELEASE_LINES=" + ",".join(l for l, _ in new_lines))


if __name__ == "__main__":
    main()
