#!/usr/bin/env bash
# Fork-only test (not present upstream) for fork-release-verify.sh. Real git
# repositories stand in for upstream, the fork remote and the checkout; a fake
# `gh` on PATH serves the workflow runs (needs jq) or fails like an API error.
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SCRIPT="$SCRIPT_DIR/fork-release-verify.sh"
TEST_DIR=$(mktemp -d "${TMPDIR:-/tmp}/fork-release-verify.XXXXXX")
trap 'rm -rf "$TEST_DIR"' EXIT

# Keep the caller's git configuration (signing, hooks, default branch) out.
export HOME="$TEST_DIR" GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.com
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.com

UP="$TEST_DIR/upstream"
FORK="$TEST_DIR/fork.git"
WORK="$TEST_DIR/work"

commit() { # commit REPO MESSAGE -> prints the new commit
  git -C "$1" commit -q --allow-empty -m "$2"
  git -C "$1" rev-parse HEAD
}

# Upstream: v0.12.1 (lightweight, like upstream's real tags), v0.12.2
# (annotated), then v0.12.3-rc.1 and v0.12.3, which the fork has not merged.
git init -q -b main "$UP"
commit "$UP" u1 >/dev/null
git -C "$UP" tag v0.12.1
commit "$UP" u2 >/dev/null
git -C "$UP" tag -a v0.12.2 -m v0.12.2
commit "$UP" u3 >/dev/null
git -C "$UP" tag v0.12.3-rc.1
commit "$UP" u4 >/dev/null
git -C "$UP" tag v0.12.3

# Fork: main = upstream v0.12.2 + F1..F3. Releases v0.12.2-mouse.1 (F1) and
# v0.12.2-mouse.3 (F2) exist; the gap makes "N not above the maximum" testable.
# SIDE is a reviewed-looking commit that never reached main.
git init -q --bare "$FORK"
git init -q -b main "$WORK"
git -C "$WORK" fetch -q --no-tags "$UP" "+refs/tags/v0.12.2:refs/upstream/v0.12.2"
git -C "$WORK" reset -q --hard "refs/upstream/v0.12.2^{commit}"
git -C "$WORK" update-ref -d refs/upstream/v0.12.2
F1=$(commit "$WORK" f1)
F2=$(commit "$WORK" f2)
F3=$(commit "$WORK" f3)
git -C "$WORK" checkout -q -b side
SIDE=$(commit "$WORK" side)
git -C "$WORK" checkout -q main
git -C "$WORK" push -q "$FORK" main

tag_annotated() { # tag_annotated NAME COMMIT: create and push
  git -C "$WORK" tag -a "$1" "$2" -m "$1"
  git -C "$WORK" push -q "$FORK" "refs/tags/$1"
}
untag() {
  git -C "$WORK" push -q "$FORK" ":refs/tags/$1"
  git -C "$WORK" tag -d "$1" >/dev/null
}
tag_annotated v0.12.2-mouse.1 "$F1"
tag_annotated v0.12.2-mouse.3 "$F2"

# FAKE_RUNS_<workflow file, non-alphanumerics as _> holds one run per line:
# "id status conclusion branch". Unset or empty means no runs.
mkdir "$TEST_DIR/bin"
cat >"$TEST_DIR/bin/gh" <<'FAKE'
#!/usr/bin/env bash
set -eu
prefix="repos/acme/multica/actions/workflows/"
if [ "$1" != api ] || [ "${3:-}" != --jq ] || [[ "$2" != "$prefix"*"/runs?"* ]]; then
  echo "unexpected gh call: $*" >&2
  exit 2
fi
if [ -n "${FAKE_GH_FAIL:-}" ]; then
  echo "HTTP 502: Bad Gateway" >&2
  exit 1
fi
workflow="${2#"$prefix"}"
workflow="${workflow%%/*}"
var="FAKE_RUNS_$(printf '%s' "$workflow" | tr -c 'A-Za-z0-9' _)"
printf '%s\n' "${!var:-}" |
  jq -R -s --arg sha "$FAKE_SHA" '
    [split("\n")[] | select(length > 0) | split(" ")
     | {id: (.[0] | tonumber), status: .[1],
        conclusion: (if .[2] == "null" then null else .[2] end),
        head_branch: .[3], event: "push", head_sha: $sha}]
    | {total_count: length, workflow_runs: .}' |
  jq -r "$4"
FAKE
chmod +x "$TEST_DIR/bin/gh"

export PATH="$TEST_DIR/bin:$PATH" GITHUB_REPOSITORY=acme/multica
export FORK_RELEASE_REMOTE="$FORK" FORK_RELEASE_UPSTREAM="$UP"
GREEN="1 completed success main"
export FAKE_RUNS_ci_yml="$GREEN" FAKE_RUNS_ci_fork_yml="$GREEN" FAKE_RUNS_build_image_yml="$GREEN"
failures=0

verify() { # verify TAG SHA [VAR=value...]
  local tag="$1" sha="$2"
  shift 2
  (cd "$WORK" && env FAKE_SHA="$sha" "$@" bash "$SCRIPT" "$tag" "$sha")
}

pass() { # pass CASE TAG SHA [VAR=value...]
  local name="$1" out
  shift
  if ! out="$(verify "$@" 2>&1)"; then
    printf 'FAIL: %s: want success, got:\n%s\n' "$name" "$out" >&2
    failures=$((failures + 1))
  fi
}

# reject CASE REASON TAG SHA [VAR=value...]: must fail, and for REASON, so a
# case cannot pass because an earlier check happened to fail.
reject() {
  local name="$1" want="$2" out status=0
  shift 2
  out="$(verify "$@" 2>&1)" || status=$?
  if [ "$status" -eq 0 ]; then
    printf 'FAIL: %s: want a failure, got success:\n%s\n' "$name" "$out" >&2
    failures=$((failures + 1))
  elif ! grep -qF -- "$want" <<<"$out"; then
    printf 'FAIL: %s: want a failure mentioning "%s", got:\n%s\n' "$name" "$want" "$out" >&2
    failures=$((failures + 1))
  fi
}

# Normal release, and a re-run of the same tag (it does not count against itself).
tag_annotated v0.12.2-mouse.4 "$F3"
pass "normal release" v0.12.2-mouse.4 "$F3"
pass "re-run of the newest tag" v0.12.2-mouse.4 "$F3"
pass "a failed run that was re-run later is superseded" v0.12.2-mouse.4 "$F3" \
  FAKE_RUNS_ci_yml=$'1 completed failure main\n2 completed success main'
pass "the tag's own Build Image run is ignored" v0.12.2-mouse.4 "$F3" \
  FAKE_RUNS_build_image_yml=$'1 completed success main\n2 in_progress null v0.12.2-mouse.4'
reject "tag points elsewhere than the run's commit" "was the tag moved?" v0.12.2-mouse.4 "$F2"
untag v0.12.2-mouse.4

# Tag format.
for bad in v0.12.2 v0.12.2-mouse.0 v0.12.2-mouse.03 v0.12.2-mouse.1-rc.1 v0.12-mouse.1 0.12.2-mouse.5; do
  reject "format $bad" "must look like vX.Y.Z-mouse.N" "$bad" "$F3"
done

# Tag kind and location.
reject "tag not pushed" "does not exist on" v0.12.2-mouse.5 "$F3"
git -C "$WORK" tag v0.12.2-mouse.5 "$F3"
git -C "$WORK" push -q "$FORK" refs/tags/v0.12.2-mouse.5
reject "lightweight tag" "lightweight tag" v0.12.2-mouse.5 "$F3"
untag v0.12.2-mouse.5

tag_annotated v0.12.2-mouse.5 "$SIDE"
reject "commit not on main" "not on main" v0.12.2-mouse.5 "$SIDE"
untag v0.12.2-mouse.5

# Version prefix and N.
tag_annotated v0.12.1-mouse.1 "$F3"
reject "older upstream prefix" "must be based on v0.12.2" v0.12.1-mouse.1 "$F3"
untag v0.12.1-mouse.1
tag_annotated v0.12.3-mouse.1 "$F3"
reject "upstream prefix not merged yet" "must be based on v0.12.2" v0.12.3-mouse.1 "$F3"
untag v0.12.3-mouse.1
tag_annotated v0.12.2-mouse.2 "$F3"
reject "N below the existing maximum" "v0.12.2-mouse.3 already exists" v0.12.2-mouse.2 "$F3"
untag v0.12.2-mouse.2
# The same N under a different upstream version does not count.
tag_annotated v0.12.1-mouse.9 "$F1"
tag_annotated v0.12.2-mouse.5 "$F3"
pass "N jumps ahead; other versions' N are ignored" v0.12.2-mouse.5 "$F3"

# Required workflows on main.
for var in FAKE_RUNS_ci_yml FAKE_RUNS_ci_fork_yml FAKE_RUNS_build_image_yml; do
  reject "$var failed" "is completed/failure" v0.12.2-mouse.5 "$F3" "$var=1 completed failure main"
  reject "$var cancelled" "is completed/cancelled" v0.12.2-mouse.5 "$F3" "$var=1 completed cancelled main"
  reject "$var still running" "is in_progress/null" v0.12.2-mouse.5 "$F3" "$var=1 in_progress null main"
  reject "$var queued" "is queued/null" v0.12.2-mouse.5 "$F3" "$var=1 queued null main"
  reject "$var missing" "no push-to-main run" v0.12.2-mouse.5 "$F3" "$var="
  reject "$var only on another branch" "no push-to-main run" v0.12.2-mouse.5 "$F3" "$var=1 completed success feature"
  reject "$var newest run failed" "is completed/failure" v0.12.2-mouse.5 "$F3" \
    "$var=$(printf '7 completed failure main\n3 completed success main')"
done
reject "Actions API error" "could not list" v0.12.2-mouse.5 "$F3" FAKE_GH_FAIL=1

# Environment the gate cannot trust.
reject "upstream unreachable" "could not list the tags of" v0.12.2-mouse.5 "$F3" \
  FORK_RELEASE_UPSTREAM="$TEST_DIR/missing"
reject "fork remote unreachable" "could not fetch main and tags" v0.12.2-mouse.5 "$F3" \
  FORK_RELEASE_REMOTE="$TEST_DIR/missing.git"
git clone -q --depth 1 "file://$FORK" "$TEST_DIR/shallow" 2>/dev/null
if out="$(cd "$TEST_DIR/shallow" && FAKE_SHA="$F3" bash "$SCRIPT" v0.12.2-mouse.5 "$F3" 2>&1)" ||
  ! grep -qF "shallow" <<<"$out"; then
  printf 'FAIL: shallow checkout: want a failure mentioning "shallow", got:\n%s\n' "$out" >&2
  failures=$((failures + 1))
fi

# The workflow's call shape: bash -e stops at the first failing check.
if FAKE_SHA="$F3" FAKE_RUNS_ci_yml='' bash -e -c 'cd "$1" && bash "$2" v0.12.2-mouse.5 "$3"; echo reached' \
  _ "$WORK" "$SCRIPT" "$F3" 2>/dev/null | grep -q reached; then
  echo "FAIL: workflow step continued after a failed gate" >&2
  failures=$((failures + 1))
fi

if [ "$failures" -ne 0 ]; then
  echo "fork-release-verify: $failures failure(s)" >&2
  exit 1
fi
echo "fork-release-verify: ok"
