# Developing cubby

Go 1.26+, no C compiler (SQLite is pure Go), Node 22+ for the JS checks.

```sh
go test -race ./... && go vet ./...
sh tools/jstest/run.sh      # kv.js, live, pulses, rooms, presence, inbox, devices, QR, against a fresh server
./build.sh                  # dist/cubby-macos, -linux-amd64, -linux-arm64
```

Browser checks run with [playwright-cli](https://github.com/microsoft/playwright-cli) against a server on port 28991, in Chromium and WebKit: `tools/jstest/live.browser.js`, `presence.browser.js`, `share.browser.js`. Each file's first lines say how. They're the only tests of kv.js keeping its key in IndexedDB.

`tools/interop/gen.sh` regenerates `internal/auth/testdata/interop.jsonl`, tokens from jose, PyJWT and openssl that `go test` checks.

## Layout

```
main.go             hands the command line to internal/cli
internal/auth/      JWT parsing and verification, device keys, the signer the CLI uses
internal/access/    levels, settings, the access rule
internal/cli/       serve, admin, and the client commands
internal/server/    HTTP routes, sign-in, live connections, watches, rate limits
internal/store/     SQLite: spaces, keys, series, visitors, devices, pairing, API keys, invites, members
internal/config/    config file, defaults, data directory
internal/names/     names and IDs, validation, words.txt
internal/apierr/    API errors
web/kv.js           the JS client, served at /kv.js
web/share.js        the share panel and QR encoder, served at /share.js
tools/              word list builder, JS and browser checks, interop token generator
```

`cli` uses `server`, `server` uses `store`, everything may use `auth`, `access`, `config`, `names` and `apierr`. `store` never imports `server`.

Docs never name the current release; use `vX.Y.Z`.
