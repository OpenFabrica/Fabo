#!/bin/sh
# Release gate (docs/release.md): stop unless this checkout is exactly the
# given annotated tag, the tag is on the project's main branch, and tracked
# files are clean. RELEASE_REMOTE / RELEASE_BRANCH name that branch.
set -eu

remote=${RELEASE_REMOTE:-upstream}
branch=${RELEASE_BRANCH:-master}

tag=${1:?usage: scripts/release-check.sh vX.Y.Z}
fail() { echo "release-check: $*" >&2; exit 1; }

# Only plain vX.Y.Z or vX.Y.Z-pre.N tags are releases.
printf '%s' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-(alpha|beta|rc)\.[0-9]+)?$' \
  || fail "$tag is not vX.Y.Z or vX.Y.Z-(alpha|beta|rc).N"
[ "$(git cat-file -t "$tag" 2>/dev/null)" = tag ] || fail "$tag is not an annotated tag"

# A lightweight upstream tag or a commit after the tag would change the version.
described=$(git describe --tags --dirty)
[ "$described" = "$tag" ] || fail "git describe gives $described, not $tag"

[ -z "$(git status --porcelain --untracked-files=no)" ] || fail "tracked files have uncommitted changes"

git fetch --quiet --no-tags "$remote" "$branch"
git merge-base --is-ancestor "$tag" "$remote/$branch" || fail "$tag is not on $remote/$branch"

grep -q "^## ${tag#v} · " CHANGELOG.md 2>/dev/null || fail "CHANGELOG.md has no '## ${tag#v} · …' entry"

# NOTICE lists what the binaries link; a dependency change must reach it.
python3 scripts/notice.py --check || fail "NOTICE is out of date: run python3 scripts/notice.py and commit it"

echo "release-check: $tag ok"
