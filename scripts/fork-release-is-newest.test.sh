#!/usr/bin/env bash
# Fork-only test (not present upstream) for fork-release-is-newest.sh. A fake
# `gh` on PATH serves the tag list, or fails to simulate an API error.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SCRIPT="$SCRIPT_DIR/fork-release-is-newest.sh"
TEST_DIR=$(mktemp -d "${TMPDIR:-/tmp}/fork-release-is-newest.XXXXXX")
trap 'rm -rf "$TEST_DIR"' EXIT

cat >"$TEST_DIR/gh" <<'FAKE'
#!/usr/bin/env bash
if [ "$1 $2" != "api repos/acme/multica/tags" ]; then
  echo "unexpected gh call: $*" >&2
  exit 2
fi
if [ -n "${FAKE_GH_FAIL:-}" ]; then
  echo "HTTP 502: Bad Gateway" >&2
  exit 1
fi
printf '%s' "$FAKE_GH_TAGS"
FAKE
chmod +x "$TEST_DIR/gh"
export PATH="$TEST_DIR:$PATH" GITHUB_REPOSITORY=acme/multica
failures=0

check() {
  local want="$1" tag="$2" tags="$3" got
  got="$(FAKE_GH_TAGS="$tags" bash "$SCRIPT" "$tag")"
  if [ "$got" != "$want" ]; then
    echo "FAIL: $tag with tags [$(echo $tags)] -> $got, want $want" >&2
    failures=$((failures + 1))
  fi
}

# check_fails TAG: the script must exit non-zero and print nothing.
check_fails() {
  local tag="$1" out status=0
  out="$(bash "$SCRIPT" "$tag" 2>/dev/null)" || status=$?
  if [ "$status" -eq 0 ] || [ -n "$out" ]; then
    echo "FAIL: $tag -> exit $status, output '$out'; want a failure with no decision" >&2
    failures=$((failures + 1))
  fi
}

all=$'v0.8.1\nv0.8.2\nv0.8.2-mouse.1\nv0.8.2-mouse.9\nv0.8.2-mouse.10\nv0.8.2-rc.1\nv0.8.1-fork.3'

check true  v0.8.2-mouse.10 "$all"
check false v0.8.2-mouse.9  "$all"
check false v0.8.2-mouse.1  "$all"
check true  v0.8.3-mouse.1  "$all"$'\nv0.8.3-mouse.1'
check false v0.8.2-mouse.10 "$all"$'\nv0.8.3-mouse.1'
check true  v0.8.2-mouse.1  ""            # first release; tag list not yet updated
check true  v0.10.0-mouse.1 $'v0.9.9-mouse.20'

# A failed tag listing must not promote anything, old tag or new.
FAKE_GH_FAIL=1 check_fails v0.8.2-mouse.1
FAKE_GH_FAIL=1 check_fails v0.9.0-mouse.1

# The same failure through the workflows' call shape (bash -e, assignment).
if FAKE_GH_FAIL=1 bash -e -c 'newest="$(bash "$1" v0.8.2-mouse.1)"; echo "reached: $newest"' _ "$SCRIPT" 2>/dev/null | grep -q reached; then
  echo "FAIL: workflow step continued after a failed tag listing" >&2
  failures=$((failures + 1))
fi

if FAKE_GH_TAGS="" bash "$SCRIPT" v0.8.2 >/dev/null 2>&1; then
  echo "FAIL: accepted a tag without -mouse.N" >&2
  failures=$((failures + 1))
fi

if [ "$failures" -ne 0 ]; then
  exit 1
fi
echo "fork-release-is-newest: ok"
