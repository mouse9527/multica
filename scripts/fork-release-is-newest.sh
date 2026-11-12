#!/usr/bin/env bash
# Fork-only script (not present upstream), used by build-image.yml and
# release-fork.yml. Prints "true" when TAG is the newest vX.Y.Z-mouse.N tag in
# the tag names read from stdin, "false" otherwise. Only the newest release
# moves the `latest` image tag and the "latest" GitHub Release, so re-running
# an older tag's workflow never rolls them back.
#
# Usage: gh api repos/OWNER/REPO/tags --paginate --jq '.[].name' | fork-release-is-newest.sh TAG
set -euo pipefail

tag="${1:?usage: fork-release-is-newest.sh TAG < tag-names}"
pattern='^v[0-9]+\.[0-9]+\.[0-9]+-mouse\.[0-9]+$'

if [[ ! "$tag" =~ $pattern ]]; then
  echo "not a fork release tag: $tag" >&2
  exit 2
fi

# Sort on the numeric fields (X, Y, Z, N) so mouse.10 sorts after mouse.9.
newest="$(
  { cat; echo; echo "$tag"; } |
    grep -E "$pattern" |
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
