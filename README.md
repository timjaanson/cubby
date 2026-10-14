# tiny-kv

A key-value store you run yourself, in one binary. Pages, scripts and anything else that can make an HTTP request use it to read and write small values. There's no setup per use and no login.

The same binary is the server and a command-line client.

## Run it

```sh
./tiny-kv serve
```

That's the whole setup. It listens on port 8080 and keeps its data in `~/.local/share/tiny-kv/`. No config file is needed.

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
- A key is up to 128 of `A-Z a-z 0-9 . _ : -`. Group with dots (`msg.1791449600123`), because listing and watching select by prefix (`msg.*`).
- A value is UTF-8 text up to 64 KB. Valid JSON comes back as `application/json`, anything else as `text/plain`, always byte for byte as written.
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

Run a command for every change, and resume where it stopped after a restart:

```sh
tiny-kv watch homelab-4m8q2xk7/shared/deploy --from now \
  --state /var/lib/deploy-watcher/version \
  --exec 'ref=$(cat); [ "$KV_DELETED" = 1 ] || /opt/app/deploy.sh "$ref"'
```

The command gets the value on stdin and `KV_KEY`, `KV_VERSION`, `KV_DELETED` and `KV_USER` in its environment.

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
| list | `GET /v1/{space}/shared/msg.*` or `GET /v1/{space}/users/*/vote` |
| wait for changes | `GET /v1/{space}/shared/msg.*/watch?version=N` (long poll, 25 s) |
| create a locked space | `POST /v1/spaces?label=poll&locked` → `{"space": ..., "key": "sk_..."}` |

Writes to `shared/` of a locked space also need `Space-Key: sk_...`. Errors are JSON with a stable `error` code and a `message`. CORS is open, so pages on any origin can call it.

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
</script>
```

It keeps the visitor token in `localStorage` and finds the API from the URL it was loaded from.

## Configuration

Nothing is required. These cover what most setups change:

| Flag | Environment | Default |
|---|---|---|
| `--listen` | `TINY_KV_LISTEN` | `:8080` |
| `--data` | `TINY_KV_DATA` | `~/.local/share/tiny-kv` |
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
max_visitor_bytes = "1MB"
max_space_bytes = "5MB"
max_space_keys = 10000
max_total_bytes = "1GB"
max_spaces = 1000

[spaces.status-k7m2x9q4]          # pinned: never cleaned up, own limits
max_bytes = "100MB"
max_value_size = "256KB"
```

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
ExecStart=/usr/local/bin/tiny-kv serve --listen 127.0.0.1:8080 --data /var/lib/tiny-kv
DynamicUser=yes
StateDirectory=tiny-kv
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

Back up by copying `tiny-kv.db` from the data directory, or with `sqlite3 tiny-kv.db ".backup copy.db"` while it runs.

## Admin

Runs on the server machine against the data directory. Safe while the server runs.

```sh
tiny-kv admin spaces            # name, kind, size, keys, last activity
tiny-kv admin stats
tiny-kv admin export <space>    # JSON lines
tiny-kv admin delete <space>
tiny-kv admin rekey <space>     # lock a space or replace its key
tiny-kv admin cleanup           # run the hourly cleanup now
```

Pass `--data DIR` if the server uses a non-default data directory.

## Build

Needs Go 1.26 or newer. No C compiler: SQLite is pure Go, so every binary is static.

```sh
go test ./...
./build.sh          # writes dist/tiny-kv-macos, dist/tiny-kv-linux-amd64, dist/tiny-kv-linux-arm64
go build .          # just for this machine
```

## Differences from the design docs

- **One SQLite file** (`tiny-kv.db`) instead of a file per space. Fewer moving parts, and fine at the default limits.
- **Admin opens the database directly**, with no Unix socket. SQLite's WAL mode makes this safe next to a running server. Watchers aren't woken by admin changes.
- **Default data directory** is `~/.local/share/tiny-kv` rather than `./data`, so `serve` and `admin` find the same data from any directory.
- **Activity times** are written straight to the database, at most once an hour per space or visitor, instead of being buffered.
- **When `watch --from now` hits an expired version** it continues from now instead of replaying the current state, so an `--exec` deploy hook never reruns old requests.
- Not built yet: proof of work on `/v1/anon`, and everything else under "Later" in the spec.
