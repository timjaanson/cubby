#!/bin/sh
# Prints an EdDSA token minted with only openssl (3.x), base64 and printf:
# the shell recipe from the docs. Usage: openssl.sh AUD IAT
set -eu
OPENSSL=${OPENSSL:-openssl}
aud=$1 iat=$2
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
b64url() { $OPENSSL base64 -A | tr '+/' '-_' | tr -d '='; }
$OPENSSL genpkey -algorithm ed25519 -out "$tmp/key.pem"
x=$($OPENSSL pkey -in "$tmp/key.pem" -pubout -outform DER | tail -c 32 | b64url)
jwk=$(printf '{"crv":"Ed25519","kty":"OKP","x":"%s"}' "$x")
kid=$(printf '%s' "$jwk" | $OPENSSL dgst -sha256 -binary | b64url)
h=$(printf '{"alg":"EdDSA","typ":"JWT","kid":"%s"}' "$kid" | b64url)
p=$(printf '{"aud":"%s","iat":%s,"exp":%s}' "$aud" "$iat" $((iat + 3600)) | b64url)
printf '%s.%s' "$h" "$p" > "$tmp/in"
s=$($OPENSSL pkeyutl -sign -inkey "$tmp/key.pem" -rawin -in "$tmp/in" | b64url)
printf '{"tool":"openssl","alg":"EdDSA","jwk":%s,"token":"%s.%s.%s"}\n' "$jwk" "$h" "$p" "$s"
