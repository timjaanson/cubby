# Developing cubby

Building, testing, how the code is laid out, and where the build differs from the design docs.

## Build

Needs Go 1.26 or newer. No C compiler: SQLite is pure Go, so every binary is static.

```sh
go test -race ./...
go vet ./...
sh tools/jstest/run.sh   # kv.js, live mode, pulses, rooms, presence and the QR encoder against a fresh local server (needs Node 22+)
./build.sh          # writes dist/cubby-macos, dist/cubby-linux-amd64, dist/cubby-linux-arm64
go build .           # just for this machine, writes ./cubby
```

Browser checks run in real engines with [playwright-cli](https://github.com/microsoft/playwright-cli), against a server on port 28991. `tools/jstest/live.browser.js` covers live mode in Chromium and WebKit, `tools/jstest/presence.browser.js` a closed tab leaving presence, `tools/jstest/share.browser.js` the share panel for owners and members. Each file's first lines say how to run it.

## Code layout

```
main.go             entry point: hands the command line to internal/cli
internal/access/    who may do what: settings, the public shorthand, scopes, the access rule
internal/cli/       the cubby command: serve, admin, and the client commands
internal/server/    HTTP API, live connections, rate limits, watches, admin socket handler
internal/store/     SQLite storage: spaces, keys, series, visitors, tokens, grants, cleanup
internal/config/    config file, defaults, data directory lookup
internal/names/     name and key validation, random names and tokens, words.txt
internal/apierr/    the API error type and shared errors
internal/version/   build version, set by build.sh
web/kv.js           the JS client, served at /kv.js with the word list filled in
web/share.js        the share panel and QR encoder, served at /share.js
tools/wordlist/     builds internal/names/words.txt from public word lists (not in the binary)
tools/jstest/       kv.js, live mode, QR and browser checks against a local server
docs/               usage.md, deploy.md, spec.md (the contract), design.md (the model), scenarios.md, agents.md, plans.md
```

Dependencies point one way: `cli` uses `server`, `server` uses `store`, and everything can use `access`, `config`, `names` and `apierr`. `store` never imports `server`.

## Differences from the design docs

- **Default data directory** follows the OS convention (see [Where data lives](deploy.md#where-data-lives)) instead of `./data`, so `serve` and `admin` find the same data from any directory.
- **`visitors.db` is called `server.db`.** Besides visitors, it remembers which spaces each visitor wrote to, and the last counter of deleted spaces so a recreated space never reuses versions.
- **Admin needs the running server.** It doesn't fall back to opening files directly. That keeps one path for every change.
- **When `watch --from now` hits an expired version** it continues from now instead of replaying the current state, so an `--exec` deploy hook never reruns old requests.
- **Visitor cleanup turns their `users/` keys into tombstones**, so watchers see them go, instead of deleting rows silently.
- **List, watch and series responses compact JSON values**, so `{"a": 1}` comes back as `{"a":1}`. A plain `GET` returns the bytes exactly as written.
- **Space size counts the newest point twice** for a series: once as the key's value and once as a point. It's a few bytes per series and keeps the accounting simple.
- Not built yet: the deferred helpers and candidates in [plans.md](plans.md), proof of work on `/v1/anon`, and everything else under "Later" in the spec, including reading several series at once and `?format=text`.
