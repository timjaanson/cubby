#!/bin/sh
# Regenerates internal/auth/testdata/interop.jsonl: tokens from other JWT
# libraries that the server must accept. Needs Node, Python with
# pyjwt[crypto] (PYTHON=...), and OpenSSL 3 (OPENSSL=...; macOS's LibreSSL
# has no Ed25519). The Go test checks them at a fixed time, so this only
# needs running when a tool is added.
set -eu
cd "$(dirname "$0")"
aud=https://cubby.example.com iat=1794787200
[ -d node_modules/jose ] || npm install --silent --no-audit --no-fund
out=../../internal/auth/testdata/interop.jsonl
{
  node jose.mjs $aud $iat
  ${PYTHON:-python3} pyjwt.py $aud $iat
  sh openssl.sh $aud $iat
} > $out
wc -l $out
