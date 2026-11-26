#!/usr/bin/env bash
# Fork-only script (not present upstream), run by the verify job of
# release-fork.yml before anything is built or published. It is the automatic
# gate for a fork release: the release job only runs when every check passes.
#
#   1. TAG looks like vX.Y.Z-mouse.N (N >= 1, no leading zero).
#   2. TAG is an annotated tag on the fork remote and points directly at SHA.
#   3. SHA is on the fork's main (git merge-base --is-ancestor).
#   4. X.Y.Z is the newest upstream vX.Y.Z tag merged into SHA, and N is
#      greater than every existing vX.Y.Z-mouse.* tag other than TAG.
#   5. The newest push-to-main run of CI, CI (fork) and Build Image for SHA
#      completed with success. A missing run, a run still going, or an API
#      error fails the gate; nothing is ever assumed green.
#
# Run from a full (non-shallow) clone of the fork. Main and the fork's tags
# are fetched fresh into refs/fork-release-verify/, so the result does not
# depend on how the checkout fetched tags. Every failure exits non-zero.
#
# Usage: GITHUB_REPOSITORY=owner/repo GH_TOKEN=... fork-release-verify.sh TAG SHA
# Env:   FORK_RELEASE_REMOTE    remote or URL with main and the fork's tags (default origin)
#        FORK_RELEASE_UPSTREAM  upstream repository URL (default https://github.com/multica-ai/multica.git)
set -euo pipefail

tag="${1:?usage: fork-release-verify.sh TAG SHA}"
sha="${2:?usage: fork-release-verify.sh TAG SHA}"
repo="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must be set}"
remote="${FORK_RELEASE_REMOTE:-origin}"
upstream="${FORK_RELEASE_UPSTREAM:-https://github.com/multica-ai/multica.git}"
# The workflows a release commit must have passed on main: CI, CI (fork) and
# Build Image. Matched by file, not by display name.
required_workflows=(ci.yml ci-fork.yml build-image.yml)
ns=refs/fork-release-verify

fail() {
  echo "::error::$*" >&2
  exit 1
}

# 1. Tag format.
if [[ ! "$tag" =~ ^v([0-9]+\.[0-9]+\.[0-9]+)-mouse\.([1-9][0-9]*)$ ]]; then
  fail "release tags must look like vX.Y.Z-mouse.N with N >= 1 and no leading zero; got '$tag'"
fi
version="v${BASH_REMATCH[1]}"
n="${BASH_REMATCH[2]}"
if [[ ! "$sha" =~ ^[0-9a-f]{40}$ ]]; then
  fail "expected a full commit SHA; got '$sha'"
fi

# A shallow clone hides history: ancestry checks would give wrong answers.
if [ "$(git rev-parse --is-shallow-repository)" != "false" ]; then
  fail "the checkout is shallow; fetch the full history (fetch-depth: 0)"
fi

git for-each-ref --format='delete %(refname)' "$ns/" | git update-ref --stdin
if ! git fetch --quiet --no-tags "$remote" "+refs/heads/main:$ns/main" "+refs/tags/*:$ns/tags/*"; then
  fail "could not fetch main and tags from $remote"
fi

# 2. Annotated tag pointing at SHA.
read -r obj_type target_type target < <(
  git for-each-ref --format='%(objecttype) %(*objecttype) %(*objectname)' "$ns/tags/$tag"
) || true
if [ -z "${obj_type:-}" ]; then
  fail "$tag does not exist on $remote"
fi
if [ "$obj_type" != "tag" ]; then
  fail "$tag is a lightweight tag; release tags must be annotated (git tag -a)"
fi
if [ "$target_type" != "commit" ]; then
  fail "$tag must point directly at a commit, not at a $target_type"
fi
if [ "$target" != "$sha" ]; then
  fail "$tag points at $target, but this run is for $sha; was the tag moved?"
fi
echo "ok: $tag is an annotated tag on $sha"

# 3. On main.
if ! git merge-base --is-ancestor "$sha" "$ns/main"; then
  fail "$tag points at $sha, which is not on main"
fi
echo "ok: $sha is on main"

# 4a. X.Y.Z is the newest upstream vX.Y.Z tag merged into SHA. ls-remote
# lists an annotated tag twice; the ^{} line carries the commit. An upstream
# commit missing from this full clone is not merged.
if ! upstream_refs="$(git ls-remote --tags "$upstream")"; then
  fail "could not list the tags of $upstream"
fi
base=""
while read -r name commit; do
  if git merge-base --is-ancestor "$commit" "$sha" 2>/dev/null; then
    base="$name"
    break
  fi
done < <(
  printf '%s\n' "$upstream_refs" |
    awk -F'\t' '{
      ref = $2; sub(/^refs\/tags\//, "", ref); peeled = sub(/\^[{][}]$/, "", ref)
      if (ref !~ /^v[0-9]+\.[0-9]+\.[0-9]+$/) next
      if (peeled || !(ref in c)) c[ref] = $1
    } END { for (r in c) print r, c[r] }' |
    sed -E 's/^v([0-9]+)\.([0-9]+)\.([0-9]+) /\1 \2 \3 &/' |
    sort -k1,1nr -k2,2nr -k3,3nr |
    cut -d' ' -f4-
)
if [ -z "$base" ]; then
  fail "no upstream vX.Y.Z tag is merged into $sha"
fi
if [ "$version" != "$base" ]; then
  fail "$tag must be based on $base, the newest upstream tag merged into $sha; use $base-mouse.N"
fi
echo "ok: $base is the newest upstream tag merged into $sha"

# 4b. N is greater than every other N on the same upstream version.
max=0
while read -r name; do
  [ "$name" = "$tag" ] && continue
  if [[ "$name" =~ ^v([0-9]+\.[0-9]+\.[0-9]+)-mouse\.([0-9]+)$ ]] && [ "v${BASH_REMATCH[1]}" = "$base" ]; then
    other=$((10#${BASH_REMATCH[2]}))
    if [ "$other" -gt "$max" ]; then
      max="$other"
    fi
  fi
done < <(git for-each-ref --format='%(refname:strip=3)' "$ns/tags/")
if [ "$n" -le "$max" ]; then
  fail "$tag reuses or goes back on N: $base-mouse.$max already exists; use $base-mouse.$((max + 1))"
fi
echo "ok: N=$n is greater than the existing maximum $max"

# 5. CI, CI (fork) and Build Image passed on main for SHA. The query already
# filters by commit, event and branch; the jq filter repeats it so a filter
# the API ignores cannot let another run through. The newest run decides.
for workflow in "${required_workflows[@]}"; do
  if ! result="$(gh api "repos/$repo/actions/workflows/$workflow/runs?head_sha=$sha&event=push&branch=main&per_page=100" \
    --jq "[.workflow_runs[] | select(.head_sha == \"$sha\" and .event == \"push\" and .head_branch == \"main\")]
          | sort_by(.id) | last
          | if . == null then \"missing\" else \"\(.status)/\(.conclusion)\" end")"; then
    fail "could not list $workflow runs for $sha"
  fi
  case "$result" in
    completed/success) echo "ok: $workflow passed on main for $sha" ;;
    missing) fail "no push-to-main run of $workflow for $sha" ;;
    *) fail "the newest push-to-main run of $workflow for $sha is $result, not completed/success" ;;
  esac
done

echo "verified: $tag may be released"
