# tiny-kv

A key-value store you run yourself, in one binary. Pages, scripts and anything else that can make an HTTP request use it to read and write small values. There's no setup per use and no login.

The same binary is the server and a command-line client.

## Run it

```sh
./tiny-kv serve
```

That's the whole setup. It listens on port 8080 and keeps its data in the usual place for your OS (see [Where data lives](#where-data-lives)), creating the directory on first start. No config file is needed.

Try it from another terminal:

```sh
$ tiny-kv set notes-k7m2x9q4/shared/hello "Hello there"
1792005858928

$ tiny-kv get notes-k7m2x9q4/shared/hello
Hello there

$ curl http://localhost:8080/v1/notes-k7m2x9q4/shared/hello
Hello there
```

`notes-k7m2x9q4` is a space: a readable label plus 8 random characters. Make one up with `tiny-kv name notes`. Writing to a space creates it.

## Install

Pick the binary for your machine from `dist/` (or build it, see below), put it on your `PATH` and name it `tiny-kv`:

| File | For |
|---|---|
| `tiny-kv-macos` | any Mac, Apple Silicon or Intel |
| `tiny-kv-linux-amd64` | most Linux servers and PCs |
| `tiny-kv-linux-arm64` | Raspberry Pi 4 or 5 with a 64-bit OS, ARM servers |

```sh
install -m 755 tiny-kv-linux-amd64 /usr/local/bin/tiny-kv
```

On a Mac, a binary downloaded through a browser is quarantined. Clear that once with `xattr -d com.apple.quarantine tiny-kv`.

## How data is organised

```
space                 notes-k7m2x9q4
├── shared/{key}      everyone reads; any visitor writes (or only the key holder, in a locked space)
├── users/{id}/{key}  everyone reads; only that visitor writes
└── me/{key}          only that visitor reads and writes
```

- A visitor is an anonymous identity. `POST /v1/anon` gives a token. The CLI and the JS client get one by themselves when they need it.
- A key is up to 128 of `A-Z a-z 0-9 . _ : -`, and can't be `.`, `..`, `watch` or `series`. Group with dots (`msg.1791449600123`), because listing and watching select by prefix (`msg.*`).
- A value is UTF-8 text up to 64 KB. Valid JSON comes back as `application/json`, anything else as `text/plain`, always byte for byte as written.
- A series key keeps every value added to it, each with a time, for sensor readings and the like. Its value is the newest point. See [Series](#series).
- Every write gets a version number. Pass it back with `ifVersion` to write only if nobody changed the key meanwhile.
- Data isn't secret. Anyone who knows a space name can read its `shared/` and `users/` data.

## CLI

Paths mirror the URL after `/v1/`. Quote paths with `*`.

```sh
tiny-kv get   notes-k7m2x9q4/shared/hello
tiny-kv set   notes-k7m2x9q4/shared/hello "new text"       # or pipe the value on stdin
tiny-kv del   notes-k7m2x9q4/shared/hello
tiny-kv ls    'notes-k7m2x9q4/shared/msg.*'                # key<TAB>value per line
tiny-kv ls    'poll-9d24vw6e/users/*/vote'                 # user<TAB>key<TAB>value
tiny-kv set   poll-9d24vw6e/users/me/vote tacos            # users/me is your own ID
tiny-kv watch notes-k7m2x9q4/shared/hello                  # current value, then each change
tiny-kv update homelab-4m8q2xk7/shared/count --exec 'jq "(. // 0) + 1"'
tiny-kv new --label status --locked                        # prints the name and a space key
tiny-kv name poll                                          # a fresh valid name, offline
tiny-kv whoami | token | url <path>
```

Series:

```sh
tiny-kv add    home-k7m2x9q4/shared/temp 21.4                      # one point; prints its time
tiny-kv add    'home-k7m2x9q4/shared/env.*' temp=21.4 rh=45 door=open   # env.temp, env.rh, env.door
echo '{"temp": 21.4}' | tiny-kv add 'home-k7m2x9q4/shared/env.*'      # the same from stdin
tiny-kv series home-k7m2x9q4/shared/env.temp --since 24h           # t<TAB>value, oldest first
```

Run a command for every change, and resume where it stopped after a restart:

```sh
tiny-kv watch homelab-4m8q2xk7/shared/deploy --from now \
  --state /var/lib/deploy-watcher/version \
  --exec 'ref=$(cat); [ "$KV_DELETED" = 1 ] || /opt/app/deploy.sh "$ref"'
```

The command gets the value on stdin and `KV_KEY`, `KV_VERSION`, `KV_DELETED` and `KV_USER` in its environment, plus `KV_TIME` for a series key.

Exit codes: 0 ok, 1 not found, 2 version conflict, 3 not allowed, 4 rate limit or quota, 5 bad input, 6 server unreachable, 64 bad usage. `tiny-kv help <command>` lists each command's flags.

### Pointing the CLI at a server

It talks to `http://localhost:8080` unless told otherwise:

```sh
export TINY_KV_SERVER=https://kv.example.com
```

The CLI saves its token, and keys of spaces it created, in `~/.config/tiny-kv/credentials.toml` (mode 0600). For a fixed identity in a script, set `TINY_KV_TOKEN`. To write a locked space from another machine, set `TINY_KV_SPACE_KEY`.

## Plain HTTP

Everything the CLI does is a single request.

| Do | Request |
|---|---|
| get a token | `POST /v1/anon` → `{"token": "kv_...", "id": "u_..."}` |
| read | `GET /v1/{space}/shared/{key}` |
| write | `PUT /v1/{space}/shared/{key}` with `Authorization: Bearer kv_...`, body is the value |
| write only if unchanged | `PUT ...?ifVersion=N` (`0` means "only if it doesn't exist") |
| delete | `DELETE /v1/{space}/shared/{key}` |
| list: matching keys with their values | `GET /v1/{space}/shared/msg.*` or `GET /v1/{space}/users/*/vote` |
| wait for changes | `GET /v1/{space}/shared/msg.*/watch?version=N` (long poll, 25 s) |
| add a point to a series | `POST /v1/{space}/shared/temp/series`, body is the value |
| add to several series | `POST /v1/{space}/shared/env.*/series`, body `{"temp": 21.4, "rh": 45}` |
| read a series | `GET /v1/{space}/shared/temp/series` (newest 1000) or `?since=MS` (after that time) |
| create a locked space | `POST /v1/spaces?label=poll&locked` → `{"space": ..., "key": "sk_..."}` |

Writes to `shared/` of a locked space also need `Space-Key: sk_...`. Unknown or repeated query parameters are rejected with 400, so a typo like `ifversion=0` fails instead of overwriting. Errors are JSON with a stable `error` code and a `message`. CORS is open, so pages on any origin can call it.

Every data URL is `/v1/{space}/{area}/{key or pattern}`, optionally followed by `/watch` or `/series`, and nothing after that.

The full contract is in [../SPEC.md](../SPEC.md), with worked examples in [../SCENARIOS.md](../SCENARIOS.md).

## JS client

The server hosts a small client at `/kv.js`:

```html
<script type="module">
import { kv } from "http://localhost:8080/kv.js"

const db = kv("poll-9d24vw6e")
await db.shared.set("question", "Pizza or tacos?")
await db.user().set("vote", "tacos")

db.users.watch("vote", change => console.log(change.user, change.deleted ? "removed" : change.value))
await db.shared.update("count", n => (n ?? 0) + 1)       // safe with many writers

await db.shared.add("env.*", { temp: 21.4, rh: 45 })     // a point each in env.temp and env.rh
const { points } = await db.shared.series("env.temp", { since: Date.now() - 864e5 })
</script>
```

It keeps the visitor token in `localStorage` and finds the API from the URL it was loaded from.

## Series

A key becomes a series with its first `POST {key}/series`. From then on:

```
POST /v1/home-k7m2x9q4/shared/temp/series            21.4           → 201 {"version": …, "t": 1792056454796}
POST /v1/home-k7m2x9q4/shared/temp/series?t=MS       21.2           a reading sent late, with its own time
POST /v1/home-k7m2x9q4/shared/env.*/series           {"temp": 21.5, "rh": 45}
                                                     → 200 {"version": …, "t": …, "keys": ["env.temp", "env.rh"]}
GET  /v1/home-k7m2x9q4/shared/temp                   → 21.4, the newest point, with header KV-Time: 1792056454796
GET  /v1/home-k7m2x9q4/shared/temp/series            → {"key": "temp", "points": [{"t": …, "value": 21.4}, …], "version": …, "next": null}
GET  /v1/home-k7m2x9q4/shared/temp/series?since=MS   → the points after MS, oldest first; pass "next" back as since for more
```

- With a key in the URL, the body is one point, whatever its shape: a number, text, an object. With `prefix*`, the body is an object, and each member becomes a point in series `prefix` + name, all at the same time. Up to 100 members, all or nothing.
- Points are stored as sent, up to 1 KB each. The server never calculates anything from them.
- `PUT` on a series returns 409 `is_series`, and `/series` on a plain key returns 409 `not_series`. `DELETE` removes a series with all its points.
- Each series keeps its newest 50,000 points and drops the oldest, so a device that sends forever never starts failing.
- Lists and watches include `t` for series keys. Watching gives the newest point, and `series?since=` catches up on every point.
- A `POST` counts as one write for rate limits, however many series it adds to. Points count towards the space's storage, not the visitor's.

## Configuration

Nothing is required. These cover what most setups change:

| Flag | Environment | Default |
|---|---|---|
| `--listen` | `TINY_KV_LISTEN` | `:8080` |
| `--data` | `TINY_KV_DATA` | see [Where data lives](#where-data-lives) |
| `--config` | `TINY_KV_CONFIG` | none |
| `--quiet` | | logs each request to stderr |

For limits and retention, pass a TOML file with `--config`. Every setting is optional, and the values below are the defaults:

```toml
[server]
listen = ":8080"
data_dir = "/var/lib/tiny-kv"
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

[spaces.status-k7m2x9q4]          # pinned: never cleaned up, own limits
max_bytes = "100MB"
max_value_size = "256KB"
max_series_points = 500000
```

## Where data lives

If you name a directory, with `--data`, `TINY_KV_DATA`, `data_dir` in the config file, or systemd's `StateDirectory=`, the server uses it. If it can't write there, it stops and says why, with no silent fallback.

Otherwise it works out a place on its own, so any user can just run the binary:

| Order | Directory | When |
|---|---|---|
| 1 | `/var/lib/tiny-kv` (Linux), `/Library/Application Support/tiny-kv` (macOS) | running as root |
| 2 | `~/Library/Application Support/tiny-kv` (macOS), `~/.local/share/tiny-kv` (Linux), or `$XDG_DATA_HOME/tiny-kv` | a normal user |
| 3 | `./tiny-kv-data` in the current directory | no writable home, as for service accounts and containers with a random UID |

- A directory that already has data wins, so a later run never starts empty next to existing data.
- Otherwise the first directory that can actually be created and written is used. The directory is private to the user (mode 0700).
- When it falls back, the server logs which places it skipped and why.
- If nothing is writable, it says so and asks for `--data`. It never uses a temporary directory, where data would quietly disappear.
- `tiny-kv admin` looks in the same places, so it finds the server's data without flags when run as the same user.

`tiny-kv help serve` prints the one it would use. Inside it:

```
server.db               visitors and their tokens
spaces/{name}.db        one SQLite file per space, holding everything about it
admin.sock              how `tiny-kv admin` reaches the running server
```

A space file is self-contained: its keys, version counter and lock key. So spaces are easy to handle one by one:

- **Delete one** with `tiny-kv admin delete <space>`, which also tells watchers it's gone. Removing the file by hand works too, but stop the server first.
- **Move or copy one** to another server by copying `spaces/{name}.db`, with the source server stopped or after `sqlite3 {name}.db "PRAGMA wal_checkpoint(TRUNCATE)"`. The other server picks it up on the next request for that space. Versions continue where they were, and a locked space keeps its key. Visitor IDs don't travel, since tokens live in `server.db`: the `users/` data stays readable, but its owners can't write it on the new server.
- **Back up** by copying the whole directory with the server stopped, or with `sqlite3 file.db ".backup copy.db"` per file while it runs.

## Running it for real

Put it behind a TLS proxy. Caddy is two lines and also gives browsers HTTP/2, which matters because each open watch holds a connection:

```
kv.example.com {
    reverse_proxy localhost:8080
}
```

A systemd unit:

```ini
[Unit]
Description=tiny-kv
After=network.target

[Service]
ExecStart=/usr/local/bin/tiny-kv serve --listen 127.0.0.1:8080
DynamicUser=yes
StateDirectory=tiny-kv
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

`StateDirectory=tiny-kv` puts the data in `/var/lib/tiny-kv` with no flag needed. That's also where root looks by default, so `sudo tiny-kv admin spaces` works as is.

## Admin

`tiny-kv admin` is a client of the running server. It sends each command through `admin.sock` in the data directory, and the server carries it out like any other request, so watchers learn about deleted spaces at once. Only the server's OS user can open the socket, which is the only access check. Admin has no HTTP route, so it can't be reached over the network.

```sh
tiny-kv admin spaces            # name, kind, size, keys, last activity
tiny-kv admin stats
tiny-kv admin export <space>    # JSON lines: every key, then every series point
tiny-kv admin delete <space>
tiny-kv admin rekey <space>     # lock a space or replace its key
tiny-kv admin cleanup           # run the hourly cleanup now
```

Run it on the server machine, as the server's user or root. Pass `--data DIR` if the server uses a non-default data directory. `--json` prints the raw response. If no server is running, admin says so and exits with 6.

## Build

Needs Go 1.26 or newer. No C compiler: SQLite is pure Go, so every binary is static.

```sh
go test ./...
./build.sh          # writes dist/tiny-kv-macos, dist/tiny-kv-linux-amd64, dist/tiny-kv-linux-arm64
go build .          # just for this machine
```

## Differences from the design docs

- **Default data directory** follows the OS convention (see [Where data lives](#where-data-lives)) instead of `./data`, so `serve` and `admin` find the same data from any directory.
- **`visitors.db` is called `server.db`.** Besides visitors, it remembers which spaces each visitor wrote to, and the last counter of deleted spaces so a recreated space never reuses versions.
- **Admin needs the running server.** It doesn't fall back to opening files directly. That keeps one path for every change.
- **When `watch --from now` hits an expired version** it continues from now instead of replaying the current state, so an `--exec` deploy hook never reruns old requests.
- **Visitor cleanup turns their `users/` keys into tombstones**, so watchers see them go, instead of deleting rows silently.
- **List, watch and series responses compact JSON values**, so `{"a": 1}` comes back as `{"a":1}`. A plain `GET` returns the bytes exactly as written.
- **Space size counts the newest point twice** for a series: once as the key's value and once as a point. It's a few bytes per series and keeps the accounting simple.
- Not built yet: proof of work on `/v1/anon`, and everything else under "Later" in the spec, including reading several series at once and `?format=text`.
