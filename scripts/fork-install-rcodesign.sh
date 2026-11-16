#!/usr/bin/env bash
# Fork-only script (not present upstream). Installs the pinned rcodesign
# (apple-codesign, https://github.com/indygreg/apple-platform-rs) into
# <dest-dir> on a Linux runner after checking its SHA-256, for
# scripts/fork-macos-sign.sh. Bump VERSION and both hashes together; take the
# hashes from the release's SHA256SUMS.
set -euo pipefail

VERSION="0.29.0"

dest="${1:?usage: $0 <dest-dir>}"
case "$(uname -s)/$(uname -m)" in
  Linux/x86_64)
    target="x86_64-unknown-linux-musl"
    sha256="dbe85cedd8ee4217b64e9a0e4c2aef92ab8bcaaa41f20bde99781ff02e600002"
    ;;
  Linux/aarch64)
    target="aarch64-unknown-linux-musl"
    sha256="4af92c87ddf52f5f2d1258a3b4e56c7dcb8f1b2468df744976c5f139e031961f"
    ;;
  *)
    echo "fork-install-rcodesign: unsupported platform $(uname -s)/$(uname -m)" >&2
    exit 1
    ;;
esac

name="apple-codesign-${VERSION}-${target}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
curl -fsSL --retry 3 -o "$work/$name.tar.gz" \
  "https://github.com/indygreg/apple-platform-rs/releases/download/apple-codesign%2F${VERSION}/${name}.tar.gz"
echo "$sha256  $work/$name.tar.gz" | sha256sum -c - >/dev/null
tar -xzf "$work/$name.tar.gz" -C "$work"
mkdir -p "$dest"
install -m 755 "$work/$name/rcodesign" "$dest/rcodesign"
"$dest/rcodesign" --version
