#!/usr/bin/env bash
# Fork-only script (not present upstream). Signs and checks the darwin
# `multica` binaries of a fork release with the CI code-signing certificate,
# so every release has the same macOS identity and privacy (TCC) grants
# survive upgrades. See docs/fork-release.md, "macOS 签名".
#
#   fork-macos-sign.sh sign <goos> <binary>
#       GoReleaser post-build hook (.goreleaser.fork.yml). For darwin, signs
#       <binary> in place and then runs `verify`; for other systems it does
#       nothing. Reads MULTICA_MACOS_SIGN_P12 (PKCS#12 file with the key and
#       certificate) and MULTICA_MACOS_SIGN_P12_PASSWORD_FILE.
#   fork-macos-sign.sh verify <binary>
#       Fails unless <binary> carries a signature whose identifier, designated
#       requirement and signing certificate all match the values below.
#   fork-macos-sign.sh verify-archives <dist-dir>
#       Runs `verify` on the `multica` binary inside each darwin archive in
#       <dist-dir>; fails unless both darwin/amd64 and darwin/arm64 are there.
#   fork-macos-sign.sh requirement <cert-sha1> <out-file>
#       Writes the compiled designated requirement (used by the test).
#
# RCODESIGN selects the rcodesign binary (default: rcodesign on PATH);
# scripts/fork-install-rcodesign.sh installs the pinned version.
set -euo pipefail

IDENTIFIER="ai.multica.cli"
# SHA-1 of the CI signing certificate (CN=multica-ci): the hash macOS puts in
# the designated requirement. A secret holding any other certificate fails
# the release. MULTICA_MACOS_SIGN_CERT_SHA1 overrides it for tests only.
PINNED_CERT_SHA1="0000000000000000000000000000000000000000"
CERT_SHA1="$(printf '%s' "${MULTICA_MACOS_SIGN_CERT_SHA1:-$PINNED_CERT_SHA1}" | tr 'A-F' 'a-f')"
RCODESIGN="${RCODESIGN:-rcodesign}"

die() {
  echo "fork-macos-sign: $*" >&2
  exit 1
}

check_sha1() {
  [[ "$1" =~ ^[0-9a-f]{40}$ ]] || die "certificate SHA-1 must be 40 hex digits, got '$1'"
  [ "$1" != "0000000000000000000000000000000000000000" ] ||
    die "the CI certificate SHA-1 is not pinned yet (PINNED_CERT_SHA1)"
}

be32() {
  printf '%08x' "$1"
}

# write_requirement SHA1 OUT writes the compiled form of
#   identifier "ai.multica.cli" and certificate leaf = H"<SHA1>"
# — the requirement `codesign` itself generates for a self-signed
# certificate. rcodesign only accepts compiled requirements and Apple's
# `csreq` compiler exists only on macOS, so the blob is assembled here;
# fork-macos-sign.test.sh compares it with csreq's output.
write_requirement() {
  local sha1="$1" out="$2" id_hex pad expr
  check_sha1 "$sha1"
  id_hex="$(printf '%s' "$IDENTIFIER" | od -An -tx1 | tr -d ' \n')"
  pad=$(((4 - ${#IDENTIFIER} % 4) % 4))
  # opAnd(6) opIdent(2) <len><identifier, zero-padded to 4 bytes>
  # opAnchorHash(4) slot 0 (leaf) <len 20><sha1>
  expr="$(be32 6)$(be32 2)$(be32 ${#IDENTIFIER})${id_hex}$(printf '%*s' $((pad * 2)) '' | tr ' ' 0)"
  expr+="$(be32 4)$(be32 0)$(be32 20)${sha1}"
  # Requirement blob: magic 0xfade0c00, total length, kind 1 (expression).
  local blob
  blob="fade0c00$(be32 $((12 + ${#expr} / 2)))$(be32 1)${expr}"
  printf '%b' "$(printf '%s' "$blob" | sed 's/../\\x&/g')" >"$out"
}

verify() {
  local bin="$1" out leaf certs
  [ -f "$bin" ] || die "$bin does not exist"
  check_sha1 "$CERT_SHA1"

  # rcodesign verify checks that the code hashes match the binary.
  "$RCODESIGN" verify "$bin" >/dev/null 2>&1 || die "$bin: signature does not verify"

  out="$("$RCODESIGN" extract code-directory "$bin")" || die "$bin: cannot read the code directory"
  grep -qxF "    ident: \"$IDENTIFIER\"," <<<"$out" ||
    die "$bin: identifier is not $IDENTIFIER: $(grep -m1 'ident:' <<<"$out" || echo none)"

  out="$("$RCODESIGN" extract requirements "$bin")" || die "$bin: cannot read the requirements"
  local want="designated(3) => (identifier \"$IDENTIFIER\") and (certificate leaf = H\"$CERT_SHA1\")"
  [ "$out" = "$want" ] || die "$bin: designated requirement is '$out', want '$want'"

  # The CMS signature must carry exactly the pinned certificate.
  certs="$("$RCODESIGN" extract cms-pem "$bin" | openssl pkcs7 -print_certs 2>/dev/null |
    grep -c -- '-----BEGIN CERTIFICATE-----' || true)"
  [ "$certs" = "1" ] || die "$bin: expected 1 certificate in the signature, found ${certs:-0}"
  leaf="$("$RCODESIGN" extract cms-pem "$bin" | openssl pkcs7 -print_certs |
    openssl x509 -noout -fingerprint -sha1 | cut -d= -f2 | tr -d ':' | tr 'A-F' 'a-f')"
  [ "$leaf" = "$CERT_SHA1" ] || die "$bin: signed by certificate $leaf, want $CERT_SHA1"

  echo "fork-macos-sign: $bin: identifier $IDENTIFIER, certificate leaf $CERT_SHA1"
}

verify_archives() {
  local dist="$1" arch archive
  work="$(mktemp -d "${TMPDIR:-/tmp}/multica-archives.XXXXXX")"
  trap 'rm -rf "$work"' EXIT
  for arch in amd64 arm64; do
    archive=("$dist"/multica-cli-*-darwin-"$arch".tar.gz)
    [ ${#archive[@]} -eq 1 ] && [ -f "${archive[0]}" ] ||
      die "expected one darwin/$arch archive in $dist, found: ${archive[*]}"
    mkdir "$work/$arch"
    tar -xzf "${archive[0]}" -C "$work/$arch" multica || die "${archive[0]}: no multica binary"
    verify "$work/$arch/multica"
  done
}

sign() {
  local goos="$1" bin="$2"
  [ "$goos" = "darwin" ] || return 0
  [ -f "$bin" ] || die "$bin does not exist"
  check_sha1 "$CERT_SHA1"
  [ -s "${MULTICA_MACOS_SIGN_P12:-}" ] || die "MULTICA_MACOS_SIGN_P12 must name the PKCS#12 file"
  [ -s "${MULTICA_MACOS_SIGN_P12_PASSWORD_FILE:-}" ] ||
    die "MULTICA_MACOS_SIGN_P12_PASSWORD_FILE must name the password file"

  req="$(mktemp "${TMPDIR:-/tmp}/multica-requirement.XXXXXX")"
  trap 'rm -f "$req"' EXIT
  write_requirement "$CERT_SHA1" "$req"
  # No timestamp server: the certificate is self-signed, nothing is
  # notarized, and the signing time does not affect the identity.
  "$RCODESIGN" sign \
    --p12-file "$MULTICA_MACOS_SIGN_P12" \
    --p12-password-file "$MULTICA_MACOS_SIGN_P12_PASSWORD_FILE" \
    --binary-identifier "$IDENTIFIER" \
    --code-requirements-file "$req" \
    --timestamp-url none \
    "$bin" >/dev/null || die "$bin: rcodesign sign failed"
  verify "$bin"
}

case "${1:-}" in
  sign)
    [ $# -eq 3 ] || die "usage: $0 sign <goos> <binary>"
    sign "$2" "$3"
    ;;
  verify)
    [ $# -eq 2 ] || die "usage: $0 verify <binary>"
    verify "$2"
    ;;
  verify-archives)
    [ $# -eq 2 ] || die "usage: $0 verify-archives <dist-dir>"
    verify_archives "$2"
    ;;
  requirement)
    [ $# -eq 3 ] || die "usage: $0 requirement <cert-sha1> <out-file>"
    write_requirement "$(printf '%s' "$2" | tr 'A-F' 'a-f')" "$3"
    ;;
  *)
    die "usage: $0 sign <goos> <binary> | verify <binary> | verify-archives <dist-dir> | requirement <cert-sha1> <out-file>"
    ;;
esac
