# cubby

A key-value store you run yourself, in one binary. Pages, scripts and anything else that can make an HTTP request use it to read and write small values. One request creates a space, and nobody has to sign up or log in.

Spaces are private by default: only their owner and people they invite get in. Opening one up is one word, `public`.

The same binary is the server and a command-line client.

## Run it

```sh
./cubby serve
```

That's the whole setup. It listens on port 28229 (CUBBY on a phone keypad) and keeps its data in the usual place for your OS (see [Where data lives](#where-data-lives)), creating the directory on first start. No config file is needed. For another port:

```sh
cubby serve -p 9000                          # or: CUBBY_LISTEN=9000 cubby serve
cubby serve --listen 127.0.0.1:9000          # this machine only
```

Try it from another terminal:

```sh
$ cubby new notes
notes-cedar-pilot-mango-frost
ot_8Hq3vT...                      # the owner token: your recovery key, shown once

$ cubby set notes-cedar-pilot-mango-frost/shared/hello "Hello there"
1792005858928

$ cubby get notes-cedar-pilot-mango-frost/shared/hello
Hello there

$ curl http://localhost:28229/v1/notes-cedar-pilot-mango-frost/shared/hello
{"error":"no_space","message":"No such space, or it's private and this visitor hasn't joined it. ..."}

$ curl "http://localhost:28229/v1/notes-cedar-pilot-mango-frost/shared/hello?token=$(cubby whoami --show-token)"
Hello there
```

`notes-cedar-pilot-mango-frost` is a space: an optional readable label plus four random words. `cubby new` created it and made you its owner. It's private, so `curl` without your visitor token can't see it. `cubby new notes --public read` would have let anyone read it.

## Install

Pick the binary for your machine from `dist/` (or build it, see below), put it on your `PATH` and name it `cubby`:

| File | For |
|---|---|
| `cubby-macos` | any Mac, Apple Silicon or Intel |
| `cubby-linux-amd64` | most Linux servers and PCs |
| `cubby-linux-arm64` | Raspberry Pi 4 or 5 with a 64-bit OS, ARM servers |

```sh
install -m 755 cubby-linux-amd64 /usr/local/bin/cubby
```

On a Mac, a binary downloaded through a browser is quarantined. Clear that once with `xattr -d com.apple.quarantine cubby`.

## How data is organised

```
space                 notes-cedar-pilot-mango-frost    owners, settings, invites
├── shared/{key}      the space's common data
├── users/{id}/{key}  each visitor's public part; only that visitor writes it
└── me/{key}          each visitor's private data
```

- A visitor is an anonymous identity with a visitor token (`vt_...`), the only credential a request ever carries. The CLI and the JS client get one by themselves when they need it.
- A space has an owner and two settings. `public` picks them in one word:

| `public` | Who reads | Who writes |
|---|---|---|
| `none`, the default | owners and invited people | owners and invited people |
| `read` | anyone with the name | owners and invited people |
| `respond` | anyone with the name | invited people in `shared/`; everyone their own `users/` and `me/` |
| `all` | anyone with the name | anyone with the name |

- Owners invite people with access tokens (`at_...`), usually sent as a link with `#t=at_...`. Redeeming one with `join` gives that visitor a grant. By default an invite makes a full member, works for 10 people and expires after 7 days. The owner token (`ot_...`) is the owner's recovery key.
- A private space looks exactly like a missing one to anyone without a grant: 404 `no_space`.
- A key is up to 128 of `A-Z a-z 0-9 . _ : -`, and can't be `.`, `..`, `watch` or `series`. Group with dots (`msg.1791449600123`), because listing and watching select by prefix (`msg.*`).
- A value is UTF-8 text up to 64 KB. Valid JSON comes back as `application/json`, anything else as `text/plain`, always byte for byte as written.
- A series key keeps every value added to it, each with a time, for sensor readings and the like. Its value is the newest point. See [Series](#series).
- Every write gets a version number. Pass it back with `ifVersion` to write only if nobody changed the key meanwhile.
- Private means invited people only. The server's operator can still read everything, so it's not a password vault.

## CLI

Paths mirror the URL after `/v1/`. Quote paths with `*`.

```sh
cubby get   notes-cedar-pilot-mango-frost/shared/hello
cubby set   notes-cedar-pilot-mango-frost/shared/hello "new text"       # or pipe the value on stdin
cubby del   notes-cedar-pilot-mango-frost/shared/hello
cubby ls    'notes-cedar-pilot-mango-frost/shared/msg.*'                # key<TAB>value per line
cubby ls    'poll-orbit-lilac-tango-slate/users/*/vote'                 # user<TAB>key<TAB>value
cubby set   poll-orbit-lilac-tango-slate/users/me/vote tacos            # users/me is your own ID
cubby watch notes-cedar-pilot-mango-frost/shared/hello                  # current value, then each change
cubby update homelab-maple-quilt-tundra-cobalt/shared/count --exec 'jq "(. // 0) + 1"'
cubby new status --public read                          # status-<four words>, then the owner token
cubby new status-cedar-pilot-mango-frost                # exactly this name, private; fails if it's taken
cubby name poll                                         # a fresh valid name, offline
cubby whoami [--show-token] | url <path>
```

Sharing:

```sh
cubby token new family-cedar-pilot-mango-frost --label anna --link https://example.com/list/
                                                        # https://example.com/list/#t=at_...  member, 10 uses, 7 days
cubby token new homelab-... --allow 'shared/status.*:w' --uses 1 --until 1h   # a narrow one for a machine
cubby join family-cedar-pilot-mango-frost 'https://example.com/list/#t=at_...'  # on the other machine
cubby access <space>                                    # owner, member or visitor, and your grants
cubby settings <space> [--public none|read|respond|all]
cubby token ls|set|rm <space> ...    cubby grant ls|add|rm <space> ...
cubby owner-token <space>            cubby drop <space>
```

Series:

```sh
cubby add    home-cedar-pilot-mango-frost/shared/temp 21.4                      # one point; prints its time
cubby add    'home-cedar-pilot-mango-frost/shared/env.*' temp=21.4 rh=45 door=open   # env.temp, env.rh, env.door
echo '{"temp": 21.4}' | cubby add 'home-cedar-pilot-mango-frost/shared/env.*'      # the same from stdin
cubby series home-cedar-pilot-mango-frost/shared/env.temp --since 24h           # t<TAB>value, oldest first
```

Run a command for every change, and resume where it stopped after a restart:

```sh
cubby watch homelab-maple-quilt-tundra-cobalt/shared/deploy --from now \
  --state /var/lib/deploy-watcher/version \
  --exec 'ref=$(cat); [ "$KV_DELETED" = 1 ] || /opt/app/deploy.sh "$ref"'
```

The command gets the value on stdin and `KV_KEY`, `KV_VERSION`, `KV_DELETED` and `KV_USER` in its environment, plus `KV_TIME` for a series key.

Exit codes: 0 ok, 1 not found, 2 version conflict, 3 not allowed, 4 rate limit or quota, 5 bad input, 6 server unreachable, 64 bad usage. `cubby help <command>` lists each command's flags.

### Pointing the CLI at a server

It talks to `http://localhost:28229` unless told otherwise. `--server` and `CUBBY_SERVER` take a URL, a host, or just a port for this machine:

```sh
export CUBBY_SERVER=https://kv.example.com
cubby get notes-cedar-pilot-mango-frost/shared/hello --server 9000      # http://localhost:9000
CUBBY_SERVER=9000 cubby ls 'notes-cedar-pilot-mango-frost/shared/*'
```

The CLI saves its visitor token in `~/.config/cubby/credentials.toml` (mode 0600) and sends it on every request. For a fixed identity in a script or CI, set `CUBBY_TOKEN`: redeem an invite once without one (`curl -X POST -d '{"token":"at_..."}' $S/v1/<space>/join`) and keep the `vt_` it returns.

## Plain HTTP

Everything the CLI does is a single request.

| Do | Request |
|---|---|
| create a space | `POST /v1/spaces` with `{"label": "poll", "public": "respond"}` (all optional) → `{"space", "owner": "ot_...", "id", "settings"}`, plus `"token"` if you sent none |
| get a visitor token | `POST /v1/anon` → `{"token": "vt_...", "id": "u_..."}` |
| read | `GET /v1/{space}/shared/{key}`, with `?token=vt_...` unless the space is public |
| write | `PUT /v1/{space}/shared/{key}` with `Authorization: Bearer vt_...`, body is the value |
| write only if unchanged | `PUT ...?ifVersion=N` (`0` means "only if it doesn't exist") |
| delete | `DELETE /v1/{space}/shared/{key}` |
| list: matching keys with their values | `GET /v1/{space}/shared/msg.*` or `GET /v1/{space}/users/*/vote` |
| wait for changes | `GET /v1/{space}/shared/msg.*/watch?version=N` (long poll, 25 s) |
| add a point to a series | `POST /v1/{space}/shared/temp/series`, body is the value |
| add to several series | `POST /v1/{space}/shared/env.*/series`, body `{"temp": 21.4, "rh": 45}` |
| read a series | `GET /v1/{space}/shared/temp/series` (newest 1000) or `?since=MS` (after that time) |
| invite | `POST /v1/{space}/tokens` (owners) → `{"token": "at_...", "uses": 10, "until": ...}` |
| join | `POST /v1/{space}/join` with `{"token": "at_..."}`; without a visitor token one is created and returned |
| what may I do | `GET /v1/{space}/access` |
| owners | `PATCH`/`DELETE /v1/{space}`, `/tokens[/{id}]`, `/grants[/{id}]`, `POST /owner-token` |

Unknown or repeated query parameters, and unknown JSON fields, are rejected with 400, so a typo like `ifversion=0` fails instead of overwriting. Errors are JSON with a stable `error` code and a `message`. CORS is open, so pages on any origin can call it.

Every data URL is `/v1/{space}/{area}/{key or pattern}`, optionally followed by `/watch` or `/series`, and nothing after that.

The full contract is in [docs/spec.md](docs/spec.md), with worked examples in [docs/scenarios.md](docs/scenarios.md).

## JS client

The server hosts a small client at `/kv.js`:

```html
<script type="module">
import { kv, createSpace } from "http://localhost:28229/kv.js"

const db = kv("poll-orbit-lilac-tango-slate")
const me = await db.open()                               // redeems #t=... invites; me.inside, me.owner, me.can()
if (!me.inside) showAskForInvite()
const { link } = await db.share({ label: "Anna" })       // owners: an invite link to send
await db.shared.set("question", "Pizza or tacos?")
await db.user().set("vote", "tacos")

db.users.watch("vote", change => console.log(change.user, change.deleted ? "removed" : change.value),
  { onSync: () => render() })                             // after each batch of changes
await db.shared.update("count", n => (n ?? 0) + 1)       // safe with many writers

await db.shared.add("env.*", { temp: 21.4, rh: 45 })     // a point each in env.temp and env.rh
const { points } = await db.shared.series("env.temp", { since: Date.now() - 864e5 })
</script>
```

It keeps the visitor token in `localStorage`, sends it as a header on writes and as `?token=` on reads (so reads skip CORS preflights), and finds the API from the URL it was loaded from. The module exports `kv`, `createSpace`, `takeInvite`, `inviteLink`, `newName`, `validSpace` and `KvError`. Every entry has `by`, the visitor who wrote it, and `mine`. Its opening comment is a short guide for coding agents; [docs/agents.md](docs/agents.md) is the long one.

For sharing without writing any UI, the server also serves a panel: invite links, QR codes, the member list, who can see the space, and the recovery key.

```html
<script type="module" src="http://localhost:28229/share.js"></script>
<cubby-share space="poll-orbit-lilac-tango-slate" members></cubby-share>
```

## Series

A key becomes a series with its first `POST {key}/series`. From then on:

```
POST /v1/home-cedar-pilot-mango-frost/shared/temp/series            21.4           → 201 {"version": …, "t": 1792056454796}
POST /v1/home-cedar-pilot-mango-frost/shared/temp/series?t=MS       21.2           a reading sent late, with its own time
POST /v1/home-cedar-pilot-mango-frost/shared/env.*/series           {"temp": 21.5, "rh": 45}
                                                     → 200 {"version": …, "t": …, "keys": ["env.temp", "env.rh"]}
GET  /v1/home-cedar-pilot-mango-frost/shared/temp                   → 21.4, the newest point, with header KV-Time: 1792056454796
GET  /v1/home-cedar-pilot-mango-frost/shared/temp/series            → {"key": "temp", "points": [{"t": …, "value": 21.4}, …], "version": …, "next": null}
GET  /v1/home-cedar-pilot-mango-frost/shared/temp/series?since=MS   → the points after MS, oldest first; pass "next" back as since for more
```

- With a key in the URL, the body is one point, whatever its shape: a number, text, an object. With `prefix*`, the body is an object, and each member becomes a point in series `prefix` + name, all at the same time. Up to 100 members, all or nothing.
- Points are stored as sent, up to 1 KB each. The server never calculates anything from them.
- `PUT` on a series returns 409 `is_series`, and `/series` on a plain key returns 409 `not_series`. `DELETE` removes a series with all its points.
- Each series keeps its newest 50,000 points and drops the oldest, so a device that sends forever never starts failing.
- Lists and watches include `t` for series keys. Watching gives the newest point, and `series?since=` catches up on every point.
- A `POST` counts as one write for rate limits, however many series it adds to. Points count towards the space's storage, not the visitor's.

## Configuration

Nothing is required. These cover what most setups change. A flag beats its environment variable, which beats the config file:

| Flag | Environment | Default |
|---|---|---|
| `-p`, `--port`, `--listen` | `CUBBY_LISTEN` | `:28229`, every interface |
| `--data` | `CUBBY_DATA` | see [Where data lives](#where-data-lives) |
| `--config` | `CUBBY_CONFIG` | none |
| `--quiet` | | logs each request to stderr |

For limits and retention, pass a TOML file with `--config`. The binary writes one for you, with a comment on every setting:

```sh
cubby config --defaults > cubby.toml     # every setting at its default
cubby serve --config cubby.toml
cubby config --config cubby.toml -p 9000 # check what serve would use; changed values say "# default: ..."
```

`cubby config` resolves settings exactly as `serve` does, so it also answers "which port and data directory will this use?" without starting anything. Every setting is optional, and the values below are the defaults:

```toml
[server]
listen = ":28229"
data_dir = "/var/lib/cubby"
trusted_proxies = ["127.0.0.1/32", "::1/128"]   # whose X-Forwarded-For to believe
rate_limits = true

[retention]
space_idle_ttl = "180d"           # spaces nobody read or wrote
visitor_idle_ttl = "365d"
empty_visitor_idle_ttl = "30d"    # visitors who never stored anything
tombstone_ttl = "7d"

[limits]
max_value_size = "64KB"
max_point_size = "1KB"            # one point in a series
max_series_points = 50000         # per series; the oldest are dropped
max_visitor_bytes = "1MB"
max_space_bytes = "5MB"
max_space_keys = 10000
max_total_bytes = "1GB"
max_spaces = 1000

[spaces.status-cedar-pilot-mango-frost]          # pinned: never cleaned up, own limits
max_bytes = "100MB"
max_value_size = "256KB"
max_series_points = 500000
```

## Where data lives

If you name a directory, with `--data`, `CUBBY_DATA`, `data_dir` in the config file, or systemd's `StateDirectory=`, the server uses it. If it can't write there, it stops and says why, with no silent fallback.

Otherwise it works out a place on its own, so any user can just run the binary:

| Order | Directory | When |
|---|---|---|
| 1 | `/var/lib/cubby` (Linux), `/Library/Application Support/cubby` (macOS) | running as root |
| 2 | `~/Library/Application Support/cubby` (macOS), `~/.local/share/cubby` (Linux), or `$XDG_DATA_HOME/cubby` | a normal user |
| 3 | `./cubby-data` in the current directory | no writable home, as for service accounts and containers with a random UID |

- A directory that already has data wins, so a later run never starts empty next to existing data.
- Otherwise the first directory that can actually be created and written is used. The directory is private to the user (mode 0700).
- When it falls back, the server logs which places it skipped and why.
- If nothing is writable, it says so and asks for `--data`. It never uses a temporary directory, where data would quietly disappear.
- `cubby admin` looks in the same places, so it finds the server's data without flags when run as the same user.

`cubby help serve` prints the one it would use. Inside it:

```
server.db               visitors and their tokens
spaces/{name}.db        one SQLite file per space, holding everything about it
admin.sock              how `cubby admin` reaches the running server
```

A space file is self-contained: its keys, version counter, settings, invites and grants. So spaces are easy to handle one by one:

- **Delete one** with `cubby admin delete <space>`, which also tells watchers it's gone. Removing the file by hand works too, but stop the server first.
- **Move or copy one** to another server by copying `spaces/{name}.db`, with the source server stopped or after `sqlite3 {name}.db "PRAGMA wal_checkpoint(TRUNCATE)"`. The other server picks it up on the next request for that space. Versions continue where they were, and settings and invites come along. Visitor tokens don't travel, since they live in `server.db`: grants and `users/` data stay, but nobody holds those visitors' tokens on the new server. The owner token still works, so the owner redeems it there to get back in.
- **Back up** by copying the whole directory with the server stopped, or with `sqlite3 file.db ".backup copy.db"` per file while it runs.

## Running it for real

Put it behind a TLS proxy. Caddy is two lines and also gives browsers HTTP/2, which matters because each open watch holds a connection:

```
kv.example.com {
    reverse_proxy localhost:28229
}
```

A systemd unit:

```ini
[Unit]
Description=cubby
After=network.target

[Service]
ExecStart=/usr/local/bin/cubby serve --listen 127.0.0.1:28229
DynamicUser=yes
StateDirectory=cubby
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

`StateDirectory=cubby` puts the data in `/var/lib/cubby` with no flag needed. That's also where root looks by default, so `sudo cubby admin spaces` works as is.

## Admin

`cubby admin` is a client of the running server. It sends each command through `admin.sock` in the data directory, and the server carries it out like any other request, so watchers learn about deleted spaces at once. Only the server's OS user can open the socket, which is the only access check. Admin has no HTTP route, so it can't be reached over the network.

```sh
cubby admin spaces              # name, read, write, size, keys, last activity
cubby admin stats
cubby admin export <space>      # JSON lines: every key, then every series point
cubby admin delete <space>
cubby admin owner-token <space> # a new owner token, for an owner who lost theirs
cubby admin cleanup             # run the hourly cleanup now
```

Run it on the server machine, as the server's user or root. Pass `--data DIR` if the server uses a non-default data directory. `--json` prints the raw response. If no server is running, admin says so and exits with 6.

## Build

Needs Go 1.26 or newer. No C compiler: SQLite is pure Go, so every binary is static.

```sh
go test ./...
./build.sh          # writes dist/cubby-macos, dist/cubby-linux-amd64, dist/cubby-linux-arm64
go build .           # just for this machine, writes ./cubby
```

## Code layout

```
main.go             entry point: hands the command line to internal/cli
internal/access/    who may do what: settings, the public shorthand, scopes, the access rule
internal/cli/       the cubby command: serve, admin, and the client commands
internal/server/    HTTP API, rate limits, watches, admin socket handler
internal/store/     SQLite storage: spaces, keys, series, visitors, tokens, grants, cleanup
internal/config/    config file, defaults, data directory lookup
internal/names/     name and key validation, random names and tokens, words.txt
internal/apierr/    the API error type and shared errors
internal/version/   build version, set by build.sh
web/kv.js           the JS client, served at /kv.js with the word list filled in
web/share.js        the share panel and QR encoder, served at /share.js
tools/wordlist/     builds internal/names/words.txt from public word lists (not in the binary)
tools/jstest/       kv.js and QR checks against a local server: ./tools/jstest/run.sh (needs Node)
docs/               design.md (the model), spec.md (the contract), scenarios.md (worked examples), agents.md (guide for coding agents), plans.md (agreed next changes)
```

Dependencies point one way: `cli` uses `server`, `server` uses `store`, and everything can use `access`, `config`, `names` and `apierr`. `store` never imports `server`.

## Differences from the design docs

- **Default data directory** follows the OS convention (see [Where data lives](#where-data-lives)) instead of `./data`, so `serve` and `admin` find the same data from any directory.
- **`visitors.db` is called `server.db`.** Besides visitors, it remembers which spaces each visitor wrote to, and the last counter of deleted spaces so a recreated space never reuses versions.
- **Admin needs the running server.** It doesn't fall back to opening files directly. That keeps one path for every change.
- **When `watch --from now` hits an expired version** it continues from now instead of replaying the current state, so an `--exec` deploy hook never reruns old requests.
- **Visitor cleanup turns their `users/` keys into tombstones**, so watchers see them go, instead of deleting rows silently.
- **List, watch and series responses compact JSON values**, so `{"a": 1}` comes back as `{"a":1}`. A plain `GET` returns the bytes exactly as written.
- **Space size counts the newest point twice** for a series: once as the key's value and once as a point. It's a few bytes per series and keeps the accounting simple.
- Not built yet: everything in [docs/plans.md](docs/plans.md), proof of work on `/v1/anon`, and everything else under "Later" in the spec, including reading several series at once and `?format=text`.
