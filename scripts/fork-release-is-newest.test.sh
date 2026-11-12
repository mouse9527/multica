#!/usr/bin/env bash
# Fork-only test (not present upstream) for fork-release-is-newest.sh.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SCRIPT="$SCRIPT_DIR/fork-release-is-newest.sh"
failures=0

check() {
  local want="$1" tag="$2" tags="$3" got
  got="$(printf '%s' "$tags" | bash "$SCRIPT" "$tag")"
  if [ "$got" != "$want" ]; then
    echo "FAIL: $tag with tags [$(echo $tags)] -> $got, want $want" >&2
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

if printf '' | bash "$SCRIPT" v0.8.2 >/dev/null 2>&1; then
  echo "FAIL: accepted a tag without -mouse.N" >&2
  failures=$((failures + 1))
fi

if [ "$failures" -ne 0 ]; then
  exit 1
fi
echo "fork-release-is-newest: ok"
