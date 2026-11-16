#!/usr/bin/env bash
# Fork-only test (not present upstream) for fork-macos-sign.sh. Signs small
# darwin binaries with throwaway certificates and checks that verify accepts
# only the expected identity. Needs rcodesign (RCODESIGN or PATH), openssl
# and go. On macOS it also checks the result with Apple's codesign.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SCRIPT="$SCRIPT_DIR/fork-macos-sign.sh"
RCODESIGN="${RCODESIGN:-rcodesign}"
export RCODESIGN
TEST_DIR=$(mktemp -d "${TMPDIR:-/tmp}/fork-macos-sign.XXXXXX")
trap 'rm -rf "$TEST_DIR"' EXIT
failures=0

fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

# make_cert NAME: self-signed code-signing certificate and PKCS#12 bundle.
# rcodesign reads only the PBE-SHA1-3DES encryption of PKCS#12 files.
make_cert() {
  local name="$1"
  openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=$name" \
    -addext "extendedKeyUsage=critical,codeSigning" \
    -addext "keyUsage=critical,digitalSignature" \
    -addext "basicConstraints=critical,CA:false" \
    -keyout "$TEST_DIR/$name.key" -out "$TEST_DIR/$name.pem" 2>/dev/null
  printf 'pw-%s' "$name" >"$TEST_DIR/$name.pw"
  openssl pkcs12 -export -inkey "$TEST_DIR/$name.key" -in "$TEST_DIR/$name.pem" \
    -passout "file:$TEST_DIR/$name.pw" -name "$name" \
    -keypbe PBE-SHA1-3DES -certpbe PBE-SHA1-3DES -macalg sha1 \
    -out "$TEST_DIR/$name.p12"
  openssl x509 -in "$TEST_DIR/$name.pem" -noout -fingerprint -sha1 |
    cut -d= -f2 | tr -d ':' | tr 'A-F' 'a-f' >"$TEST_DIR/$name.sha1"
}

# sign_with CERT PINNED_CERT GOOS BINARY
sign_with() {
  MULTICA_MACOS_SIGN_P12="$TEST_DIR/$1.p12" \
    MULTICA_MACOS_SIGN_P12_PASSWORD_FILE="$TEST_DIR/$1.pw" \
    MULTICA_MACOS_SIGN_CERT_SHA1="$(cat "$TEST_DIR/$2.sha1")" \
    bash "$SCRIPT" sign "$3" "$4"
}

# verify_with PINNED_CERT BINARY
verify_with() {
  MULTICA_MACOS_SIGN_CERT_SHA1="$(cat "$TEST_DIR/$1.sha1")" bash "$SCRIPT" verify "$2"
}

# The compiled requirement must match what Apple's csreq produces for
#   identifier "ai.multica.cli" and certificate leaf = H"3a3c…9ae9"
want_req="fade0c00000000480000000100000006000000020000000e61692e6d756c746963612e636c69"
want_req+="0000000000040000000000000014"
want_req+="3a3ce662d8abf372eedd653eb1776d713eec9ae9"
bash "$SCRIPT" requirement 3A3CE662D8ABF372EEDD653EB1776D713EEC9AE9 "$TEST_DIR/req.bin"
got_req="$(od -An -tx1 "$TEST_DIR/req.bin" | tr -d ' \n')"
[ "$got_req" = "$want_req" ] || fail "requirement blob $got_req, want $want_req"

mkdir "$TEST_DIR/src"
printf 'package main\n\nfunc main() {}\n' >"$TEST_DIR/src/main.go"
printf 'module example.com/signtest\n\ngo 1.21\n' >"$TEST_DIR/src/go.mod"
for arch in arm64 amd64; do
  (cd "$TEST_DIR/src" && CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" go build -o "$TEST_DIR/darwin-$arch" .)
done
make_cert ci
make_cert other

# Non-darwin binaries are left alone and need no key.
printf 'not a mach-o' >"$TEST_DIR/linux-bin"
cp "$TEST_DIR/linux-bin" "$TEST_DIR/linux-bin.orig"
bash "$SCRIPT" sign linux "$TEST_DIR/linux-bin" || fail "sign linux should be a no-op"
cmp -s "$TEST_DIR/linux-bin" "$TEST_DIR/linux-bin.orig" || fail "sign linux changed the file"

for arch in arm64 amd64; do
  bin="$TEST_DIR/darwin-$arch"
  cp "$bin" "$bin.unsigned"
  if verify_with ci "$bin.unsigned" >/dev/null 2>&1; then
    fail "verify accepted the unsigned (ad-hoc) $arch binary"
  fi
  sign_with ci ci darwin "$bin" >/dev/null || fail "sign darwin/$arch failed"
  verify_with ci "$bin" >/dev/null || fail "verify rejected the signed $arch binary"
  if verify_with other "$bin" >/dev/null 2>&1; then
    fail "verify accepted $arch signed by another certificate"
  fi
  # Signing the same binary again keeps the same identity.
  sign_with ci ci darwin "$bin" >/dev/null || fail "re-sign darwin/$arch failed"
  verify_with ci "$bin" >/dev/null || fail "verify rejected the re-signed $arch binary"
  if command -v codesign >/dev/null 2>&1; then
    want="designated => identifier \"ai.multica.cli\" and certificate leaf = H\"$(cat "$TEST_DIR/ci.sha1")\""
    got="$(codesign -d -r- "$bin" 2>&1 | grep '^designated' || true)"
    [ "$got" = "$want" ] || fail "codesign reports '$got' for $arch, want '$want'"
    codesign --verify --strict "$bin" 2>/dev/null || fail "codesign --verify failed for $arch"
  fi
done

# verify-archives checks the binary inside both darwin archives.
mkdir "$TEST_DIR/dist"
for arch in arm64 amd64; do
  mkdir "$TEST_DIR/pack-$arch"
  cp "$TEST_DIR/darwin-$arch" "$TEST_DIR/pack-$arch/multica"
  tar -czf "$TEST_DIR/dist/multica-cli-1.2.3-mouse.1-darwin-$arch.tar.gz" -C "$TEST_DIR/pack-$arch" multica
done
MULTICA_MACOS_SIGN_CERT_SHA1="$(cat "$TEST_DIR/ci.sha1")" \
  bash "$SCRIPT" verify-archives "$TEST_DIR/dist" >/dev/null || fail "verify-archives rejected signed archives"
cp "$TEST_DIR/darwin-amd64.unsigned" "$TEST_DIR/pack-amd64/multica"
tar -czf "$TEST_DIR/dist/multica-cli-1.2.3-mouse.1-darwin-amd64.tar.gz" -C "$TEST_DIR/pack-amd64" multica
if MULTICA_MACOS_SIGN_CERT_SHA1="$(cat "$TEST_DIR/ci.sha1")" \
  bash "$SCRIPT" verify-archives "$TEST_DIR/dist" >/dev/null 2>&1; then
  fail "verify-archives accepted an unsigned darwin/amd64 binary"
fi
rm "$TEST_DIR/dist/multica-cli-1.2.3-mouse.1-darwin-amd64.tar.gz"
if MULTICA_MACOS_SIGN_CERT_SHA1="$(cat "$TEST_DIR/ci.sha1")" \
  bash "$SCRIPT" verify-archives "$TEST_DIR/dist" >/dev/null 2>&1; then
  fail "verify-archives passed without a darwin/amd64 archive"
fi

# A secret holding a different certificate than the pinned one fails signing.
cp "$TEST_DIR/darwin-arm64.unsigned" "$TEST_DIR/wrong-cert"
if sign_with other ci darwin "$TEST_DIR/wrong-cert" >/dev/null 2>&1; then
  fail "sign succeeded with a certificate other than the pinned one"
fi
# Without an override the pinned CI certificate is expected.
cp "$TEST_DIR/darwin-arm64.unsigned" "$TEST_DIR/unpinned"
if MULTICA_MACOS_SIGN_P12="$TEST_DIR/ci.p12" MULTICA_MACOS_SIGN_P12_PASSWORD_FILE="$TEST_DIR/ci.pw" \
  bash "$SCRIPT" sign darwin "$TEST_DIR/unpinned" >/dev/null 2>&1; then
  fail "sign accepted a test certificate instead of the pinned one"
fi
# Missing key material fails instead of shipping an unsigned binary.
cp "$TEST_DIR/darwin-arm64.unsigned" "$TEST_DIR/no-key"
if MULTICA_MACOS_SIGN_CERT_SHA1="$(cat "$TEST_DIR/ci.sha1")" \
  bash "$SCRIPT" sign darwin "$TEST_DIR/no-key" >/dev/null 2>&1; then
  fail "sign succeeded without MULTICA_MACOS_SIGN_P12"
fi
# The right certificate with another identifier is rejected.
cp "$TEST_DIR/darwin-arm64.unsigned" "$TEST_DIR/wrong-id"
"$RCODESIGN" sign --p12-file "$TEST_DIR/ci.p12" --p12-password-file "$TEST_DIR/ci.pw" \
  --binary-identifier com.example.other --timestamp-url none "$TEST_DIR/wrong-id" >/dev/null 2>&1
if verify_with ci "$TEST_DIR/wrong-id" >/dev/null 2>&1; then
  fail "verify accepted a binary with another identifier"
fi

if [ "$failures" -ne 0 ]; then
  echo "$failures failure(s)" >&2
  exit 1
fi
echo "fork-macos-sign tests passed"
