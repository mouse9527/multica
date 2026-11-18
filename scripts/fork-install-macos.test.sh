#!/usr/bin/env bash
# Fork-only test (not present upstream) for fork-install-macos.sh. Releases
# are served from file:// URLs; fake `codesign`, `gh` and `multica` on PATH
# stand in for the signature, the GitHub API and the downloaded binary.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SCRIPT="$SCRIPT_DIR/fork-install-macos.sh"
TEST_DIR=$(mktemp -d "${TMPDIR:-/tmp}/fork-install-macos.XXXXXX")
trap 'rm -rf "$TEST_DIR"' EXIT
failures=0

fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

TAG=v1.2.3-mouse.4
COMMIT=0123456789abcdef0123456789abcdef01234567
GOOD_DR="$(bash "$SCRIPT_DIR/fork-macos-sign.sh" expected-requirement)"
case "$(uname -m)" in arm64 | aarch64) ARCH=arm64 ;; *) ARCH=amd64 ;; esac
ASSET="multica-cli-1.2.3-mouse.4-darwin-$ARCH.tar.gz"

mkdir -p "$TEST_DIR/bin" "$TEST_DIR/install"
cat >"$TEST_DIR/bin/codesign" <<'FAKE'
#!/usr/bin/env bash
case "$1" in
  --verify) exit "${FAKE_VERIFY_RC:-0}" ;;
  -d) echo "Executable=$3" >&2; echo "$FAKE_DR" >&2 ;;
  *) echo "unexpected codesign call: $*" >&2; exit 2 ;;
esac
FAKE
cat >"$TEST_DIR/bin/gh" <<'FAKE'
#!/usr/bin/env bash
case "$1 $2" in
  "api repos/mouse9527/multica/commits/v1.2.3-mouse.4") echo "$FAKE_TAG_COMMIT" ;;
  "run list") printf '%s\n' "$FAKE_RUN_URL" ;;
  *) echo "unexpected gh call: $*" >&2; exit 2 ;;
esac
FAKE
chmod +x "$TEST_DIR/bin/codesign" "$TEST_DIR/bin/gh"
export PATH="$TEST_DIR/bin:$PATH"

# publish VERSION_OUTPUT: builds the release directory with a fake binary.
publish() {
  rm -rf "$TEST_DIR/release" "$TEST_DIR/pack"
  mkdir -p "$TEST_DIR/release/$TAG" "$TEST_DIR/pack"
  printf '#!/bin/sh\necho "%s"\n' "$1" >"$TEST_DIR/pack/multica"
  chmod +x "$TEST_DIR/pack/multica"
  tar -czf "$TEST_DIR/release/$TAG/$ASSET" -C "$TEST_DIR/pack" multica
  (cd "$TEST_DIR/release/$TAG" && shasum -a 256 "$ASSET" >checksums.txt)
}

# run_install: installs over a fresh "old" target; prints nothing.
run_install() {
  printf 'old' >"$TEST_DIR/install/multica"
  rm -f "$TEST_DIR/install/multica.prev"
  MULTICA_RELEASE_DOWNLOAD_BASE_URL="file://$TEST_DIR/release" \
    bash "$SCRIPT" "$TAG" "$TEST_DIR/install/multica" >/dev/null 2>&1
}

# expect_kept NAME: install must fail and leave the old binary in place.
expect_kept() {
  if run_install; then
    fail "$1: install succeeded"
  fi
  [ "$(cat "$TEST_DIR/install/multica")" = "old" ] || fail "$1: target was replaced"
}

export FAKE_DR="$GOOD_DR" FAKE_TAG_COMMIT="$COMMIT" FAKE_RUN_URL="https://example.test/runs/1"
GOOD_VERSION="multica 1.2.3-mouse.4 (commit: 0123456, built: 2026-11-16T00:00:00Z)"

publish "$GOOD_VERSION"
run_install || fail "install of a matching release failed"
[ "$("$TEST_DIR/install/multica")" = "$GOOD_VERSION" ] || fail "target is not the downloaded binary"
[ "$(cat "$TEST_DIR/install/multica.prev")" = "old" ] || fail "previous binary not kept as .prev"

FAKE_DR='designated => cdhash H"6a1a87a4f42e56bbd2891efe7ff3d3da5a440453"' expect_kept "ad-hoc signature"
FAKE_DR="${GOOD_DR/H\"/H\"f}" expect_kept "another certificate"
FAKE_DR="${GOOD_DR/ai.multica.cli/com.example.other}" expect_kept "another identifier"
FAKE_VERIFY_RC=1 expect_kept "signature does not verify"
FAKE_TAG_COMMIT=fedcba9876543210fedcba9876543210fedcba98 expect_kept "tag points at another commit"
FAKE_RUN_URL="" expect_kept "no successful release run"

publish "multica 1.2.3-mouse.3 (commit: 0123456, built: 2026-11-16T00:00:00Z)"
expect_kept "binary reports another version"

publish "$GOOD_VERSION"
echo "0000000000000000000000000000000000000000000000000000000000000000  $ASSET" \
  >"$TEST_DIR/release/$TAG/checksums.txt"
expect_kept "checksum mismatch"

if bash "$SCRIPT" v1.2.3 "$TEST_DIR/install/multica" >/dev/null 2>&1; then
  fail "accepted a tag without -mouse.N"
fi

if [ "$failures" -ne 0 ]; then
  echo "$failures failure(s)" >&2
  exit 1
fi
echo "fork-install-macos tests passed"
