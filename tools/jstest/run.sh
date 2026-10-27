#!/bin/sh
# Runs the JS client checks against a fresh local server:
#   kv.test.mjs   kv.js end to end: private spaces, invites, owners, watch
#   live.test.mjs kv.js with live: true, over the WebSocket
#   qr.test.mjs   share.js's QR encoder, decoded back with jsQR
# Needs Go, Node 20+ and npm. Nothing here goes into the binary.
set -eu
cd "$(dirname "$0")"
[ -d node_modules/jsqr ] || npm install --silent --no-audit --no-fund
tmp=$(mktemp -d)
trap 'kill $pid 2>/dev/null; rm -rf "$tmp"' EXIT
(cd ../.. && go build -o "$tmp/cubby" .)
port=28990
"$tmp/cubby" serve -p $port --data "$tmp/data" --quiet &
pid=$!
sleep 1
S=http://localhost:$port
curl -fsS $S/kv.js > "$tmp/kv.mjs"
sed 's#from "./kv.js"#from "./kv.mjs"#' ../../web/share.js > "$tmp/share.mjs"
S=$S MOD="$tmp/kv.mjs" node kv.test.mjs
S=$S MOD="$tmp/kv.mjs" node live.test.mjs
MOD="$tmp/share.mjs" node qr.test.mjs
