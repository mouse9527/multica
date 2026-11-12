#!/usr/bin/env bash
# Fork-only script (not present upstream), used by build-image.yml and
# release-fork.yml. Prints "true" when TAG is the newest vX.Y.Z-mouse.N tag of
# $GITHUB_REPOSITORY, "false" otherwise. Only the newest release moves the
# `latest` image tag and the "latest" GitHub Release, so re-running an older
# tag's workflow never rolls them back.
#
# The tag list comes from `gh api`. If that request fails the script exits
# non-zero and prints nothing: an unknown answer must never promote a tag.
#
# Usage: GITHUB_REPOSITORY=owner/repo GH_TOKEN=... fork-release-is-newest.sh TAG
set -euo pipefail

tag="${1:?usage: fork-release-is-newest.sh TAG}"
repo="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must be set}"
pattern='^v[0-9]+\.[0-9]+\.[0-9]+-mouse\.[0-9]+$'

if [[ ! "$tag" =~ $pattern ]]; then
  echo "not a fork release tag: $tag" >&2
  exit 2
fi

if ! tags="$(gh api "repos/$repo/tags" --paginate --jq '.[].name')"; then
  echo "could not list tags of $repo; refusing to decide" >&2
  exit 1
fi

# The pushed tag is normally in the list already; adding it covers a list
# that lags behind the push. Sort on the numeric fields (X, Y, Z, N) so
# mouse.10 sorts after mouse.9.
newest="$(
  printf '%s\n%s\n' "$tags" "$tag" |
    { grep -E "$pattern" || true; } |
    sed -E 's/^v([0-9]+)\.([0-9]+)\.([0-9]+)-mouse\.([0-9]+)$/\1 \2 \3 \4 &/' |
    sort -k1,1n -k2,2n -k3,3n -k4,4n |
    tail -n 1 |
    cut -d' ' -f5
)"

if [ "$newest" = "$tag" ]; then
  echo true
else
  echo false
fi
