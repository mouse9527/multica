#!/usr/bin/env bash
# Fork-only script (not present upstream). Installs the signed darwin
# `multica` binary of a fork release on a macOS machine. <target> is replaced
# only after all of these hold:
#   - the archive matches the release's checksums.txt;
#   - `codesign --verify --strict` passes and `codesign -d -r-` shows the CI
#     certificate's designated requirement (fork-macos-sign.sh);
#   - the binary reports the release's version and the commit the tag points at;
#   - a successful Release (fork) run built that commit for that tag.
# See docs/fork-release.md, "本机过渡".
#
#   fork-install-macos.sh <tag> [<target>]
#
# <target> defaults to the multica on PATH (symlinks resolved); the replaced
# binary is kept as <target>.prev. MULTICA_RELEASE_REPO and
# MULTICA_RELEASE_DOWNLOAD_BASE_URL select the source like `multica update`.
# Needs curl, shasum, codesign and an authenticated gh.
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO="${MULTICA_RELEASE_REPO:-mouse9527/multica}"

die() {
  echo "fork-install-macos: $*; $target not replaced" >&2
  exit 1
}

tag="${1:-}"
target="${2:-$(command -v multica || true)}"
if [[ ! "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+-mouse\.[0-9]+$ ]] || [ -z "$target" ]; then
  echo "usage: $0 <vX.Y.Z-mouse.N> [<target>] (no multica on PATH: pass <target>)" >&2
  exit 2
fi
[ ! -e "$target" ] || target="$(readlink -f "$target")"

case "$(uname -m)" in
  arm64 | aarch64) arch=arm64 ;;
  x86_64) arch=amd64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac
version="${tag#v}"
asset="multica-cli-${version}-darwin-${arch}.tar.gz"
base="${MULTICA_RELEASE_DOWNLOAD_BASE_URL:-https://github.com/$REPO/releases/download}"
work="$(mktemp -d "${TMPDIR:-/tmp}/multica-install.XXXXXX")"
trap 'rm -rf "$work"' EXIT

curl -fsSL --retry 3 -o "$work/$asset" "$base/$tag/$asset" || die "cannot download $asset"
curl -fsSL --retry 3 -o "$work/checksums.txt" "$base/$tag/checksums.txt" || die "cannot download checksums.txt"
want="$(awk -v f="$asset" '$2 == f { print $1 }' "$work/checksums.txt")"
got="$(shasum -a 256 "$work/$asset" | cut -d' ' -f1)"
[ -n "$want" ] && [ "$got" = "$want" ] || die "$asset SHA-256 $got does not match checksums.txt (${want:-missing})"
tar -xzf "$work/$asset" -C "$work" multica || die "$asset has no multica binary"
bin="$work/multica"

# Identity before running anything from the download.
codesign --verify --strict "$bin" 2>/dev/null || die "the signature of the downloaded binary does not verify"
expected="$(bash "$SCRIPT_DIR/fork-macos-sign.sh" expected-requirement)"
actual="$(codesign -d -r- "$bin" 2>&1 | grep '^designated' || true)"
[ "$actual" = "$expected" ] || die "signature identity is '${actual:-none}', want '$expected'"

out="$("$bin" --version | head -n 1)"
[[ "$out" =~ ^multica\ v?([^ ]+)\ \(commit:\ ([0-9a-f]+), ]] || die "unexpected version output '$out'"
[ "${BASH_REMATCH[1]}" = "$version" ] || die "binary reports version ${BASH_REMATCH[1]}, want $version"
short="${BASH_REMATCH[2]}"

commit="$(gh api "repos/$REPO/commits/$tag" --jq .sha)" || die "cannot resolve $tag on $REPO"
[[ "$commit" == "$short"* ]] || die "binary was built from $short, but $tag points at $commit"
run="$(gh run list --repo "$REPO" --workflow release-fork.yml --branch "$tag" \
  --json url,headSha,conclusion \
  --jq "[.[] | select(.conclusion == \"success\" and .headSha == \"$commit\")][0].url // empty")" ||
  die "cannot list Release (fork) runs"
[ -n "$run" ] || die "no successful Release (fork) run for $tag at $commit"

dir="$(dirname "$target")"
tmp="$(mktemp "$dir/.multica-install.XXXXXX")"
cp "$bin" "$tmp"
chmod 755 "$tmp"
[ ! -e "$target" ] || cp -p "$target" "$target.prev"
mv -f "$tmp" "$target"
echo "fork-install-macos: installed $tag to $target"
echo "  commit:      $commit"
echo "  release run: $run"
echo "  identity:    $expected"
[ ! -e "$target.prev" ] || echo "  previous:    $target.prev"
