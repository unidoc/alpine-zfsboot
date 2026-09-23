#!/bin/sh
# commit-verified.sh BRANCH MESSAGE FILE [FILE...] - moves BRANCH to one
# or more new commits on top of the current default branch's tip,
# containing the given files' current on-disk content, made through
# GitHub's own Contents API rather than a local `git commit` - so every
# commit comes back Verified. Ported from unidoc-aports'
# .github/scripts/commit-verified.sh (see that file's own comment for
# why the Contents API specifically, not `git commit` -S or the Git Data
# API's lower-level blobs/trees/commits) and generalized to more than
# one file: that version's single PUT call is one commit already
# Verified in production there; each additional file here is just
# another PUT chained onto the same branch, threading the previous
# call's own returned commit sha in as the next call's base instead of
# re-reading a fixed `git rev-parse HEAD` - the Contents API only ever
# takes one file per call, and this is the straightforward way to land
# several without the Git Data API's own lower-level, unverified-until-
# proven-otherwise commit path.
#
# Needs: GH_TOKEN (or GITHUB_TOKEN) and GITHUB_REPOSITORY in the
# environment - both already set automatically inside any GitHub
# Actions run.
set -eu

BRANCH="${1:?usage: commit-verified.sh BRANCH MESSAGE FILE [FILE...]}"
MESSAGE="${2:?usage: commit-verified.sh BRANCH MESSAGE FILE [FILE...]}"
shift 2
[ "$#" -ge 1 ] || { echo "commit-verified: need at least one FILE" >&2; exit 1; }

base_sha="$(git rev-parse HEAD)"

# A real, separate branch for the scratch commits, not BRANCH itself -
# see unidoc-aports' own commit-verified.sh for why (BRANCH must only
# ever move once, to the fully-committed result, or not at all; a
# failure partway through chaining N file commits here must not leave
# BRANCH pointing at a half-updated tree).
tmp_branch="$BRANCH-tmp.$$"
resp_file="$(mktemp)"

cleanup() {
    rm -f "$resp_file"
    gh api --method DELETE "repos/$GITHUB_REPOSITORY/git/refs/heads/$tmp_branch" \
        >/dev/null 2>&1 || true
}
trap cleanup EXIT

create_or_move_branch() {
    name="$1"
    sha="$2"
    if err="$(gh api --method POST "repos/$GITHUB_REPOSITORY/git/refs" \
            -f ref="refs/heads/$name" -f sha="$sha" 2>&1 >/dev/null)"; then
        return 0
    fi
    if ! printf '%s\n' "$err" | grep -q 'Reference already exists'; then
        printf 'commit-verified: creating refs/heads/%s: %s\n' "$name" "$err" >&2
        return 1
    fi
    gh api --method PATCH "repos/$GITHUB_REPOSITORY/git/refs/heads/$name" \
        -f sha="$sha" -F force=true >/dev/null
}

# refuse_if_branch_has_human_commits guards the one force-move
# (create_or_move_branch's own -F force=true PATCH path, below) that a
# real person could actually be affected by: BRANCH is a long-lived,
# reused name (e.g. "bump-alpine-3.22" - see check-alpine.yml, chosen
# by Alpine's own version number, not a fresh name per run), and this
# script re-runs on a schedule for as long as the resulting PR stays
# open and unmerged. Every run up to now force-moved BRANCH straight to
# a fresh commit chain with no check at all - if a human had pushed
# their OWN fixup commit onto that same branch in the meantime (a real,
# plausible action on an open PR: addressing review feedback, adjusting
# something before merge), the next scheduled run silently discarded
# it with no warning, no error, nothing in the job log to notice. Every
# commit this script itself makes goes through the Contents API with no
# explicit author/committer (see the `gh api --method PUT` call below),
# which GitHub attributes to the token's own bot identity
# ("github-actions[bot]" for the default GITHUB_TOKEN this project
# uses - see this file's own header comment) - so BRANCH's current tip,
# right before this run's own force-move would happen, should ALWAYS
# be one of this script's own commits unless something else (a human,
# some other automation) touched it since. A 404 (BRANCH doesn't exist
# yet) is the normal first-run case and always safe.
refuse_if_branch_has_human_commits() {
    name="$1"
    ref_json="$(gh api "repos/$GITHUB_REPOSITORY/git/refs/heads/$name" 2>/dev/null)" || return 0
    tip_sha="$(printf '%s' "$ref_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["object"]["sha"])')"
    author_login="$(gh api "repos/$GITHUB_REPOSITORY/commits/$tip_sha" --jq '.author.login // "(none - commit has no linked GitHub account)"' 2>/dev/null)" || {
        echo "commit-verified: refusing to move $name - could not read its current tip commit ($tip_sha) to check who authored it; failing closed rather than force-overwriting an unknown history" >&2
        exit 1
    }
    expected="${COMMIT_VERIFIED_BOT_LOGIN:-github-actions[bot]}"
    if [ "$author_login" != "$expected" ]; then
        echo "commit-verified: refusing to force-move $name - its current tip ($tip_sha) was authored by '$author_login', not this automation's own identity ('$expected'). That commit looks like it came from a person, not a previous run of this script, and force-moving the branch would silently discard it. Resolve this by hand: merge/close the existing PR on $name, or delete the branch, then re-run." >&2
        exit 1
    fi
}

create_or_move_branch "$tmp_branch" "$base_sha"

token="${GH_TOKEN:-${GITHUB_TOKEN:-}}"
cur_sha="$base_sha"
for file in "$@"; do
    # 404 (file doesn't exist yet at cur_sha, e.g. a brand-new vendored
    # compatibility.d entry for a release line this repo hasn't tracked
    # before) is a real, expected case, not an error - the Contents API
    # PUT below creates it when no -f sha= is given at all, and errors
    # if one IS given for a file that doesn't exist. Only fail loudly on
    # something other than "not found". Plain curl + HTTP status code
    # here, not `gh api`'s own text-shaped error output - that format
    # isn't a documented contract to parse against, where a raw status
    # code is.
    status="$(curl -s -o "$resp_file" -w '%{http_code}' \
        -H "Authorization: Bearer $token" \
        -H "Accept: application/vnd.github+json" \
        "https://api.github.com/repos/$GITHUB_REPOSITORY/contents/$file?ref=$cur_sha")"
    file_sha=""
    case "$status" in
        200) file_sha="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["sha"])' < "$resp_file")" ;;
        404) ;;
        *)
            printf 'commit-verified: reading %s at %s: HTTP %s\n' "$file" "$cur_sha" "$status" >&2
            cat "$resp_file" >&2
            exit 1
            ;;
    esac

    sha_args=""
    [ -n "$file_sha" ] && sha_args="-f sha=$file_sha"

    # shellcheck disable=SC2086 - sha_args is intentionally either empty
    # or a single well-formed -f flag, never attacker-controlled.
    cur_sha="$(gh api --method PUT "repos/$GITHUB_REPOSITORY/contents/$file" \
        -f message="$MESSAGE" \
        -f content="$(base64 "$file" | tr -d '\n')" \
        $sha_args \
        -f branch="$tmp_branch" \
        --jq .commit.sha)"
done

refuse_if_branch_has_human_commits "$BRANCH"
create_or_move_branch "$BRANCH" "$cur_sha"
