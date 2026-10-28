# Running cubby

Installing it, putting it behind a reverse proxy, and looking after its data. Using it is in [usage.md](usage.md).

- [Install](#install)
- [Run](#run)
- [Behind a reverse proxy](#behind-a-reverse-proxy)
- [As a service](#as-a-service)
- [Configuration](#configuration)
- [Where data lives](#where-data-lives)
- [Admin](#admin)
- [Upgrading](#upgrading)

## Install

With Go 1.26 or newer, on any OS:

```sh
go install github.com/timjaanson/cubby@latest      # or @v0.13.0 for an exact version
```

Or build static binaries with `./build.sh` in a checkout, and copy the one for the machine onto its `PATH` as `cubby`:

| File in `dist/` | For |
|---|---|
| `cubby-macos` | any Mac, Apple Silicon or Intel |
| `cubby-linux-amd64` | most Linux servers and PCs |
| `cubby-linux-arm64` | Raspberry Pi 4 or 5 with a 64-bit OS, ARM servers |

```sh
install -m 755 dist/cubby-linux-amd64 /usr/local/bin/cubby
```

On a Mac, a binary downloaded through a browser is quarantined. Clear that once with `xattr -d com.apple.quarantine cubby`.

## Run

```sh
cubby serve                                  # port 28229 on every interface
cubby serve -p 9000                          # or: CUBBY_LISTEN=9000 cubby serve
cubby serve --listen 127.0.0.1:28229         # this machine only, for behind a proxy
```

It creates its data directory on first start, logs one line per request to stderr (`--quiet` turns that off) and needs no config file. `cubby config` prints what `serve` would use without starting anything.

## Behind a reverse proxy

On the internet, run cubby on `127.0.0.1` behind a proxy that does TLS. Pages on `https://` can only reach `https://` and `wss://`, and browsers keep the visitor token in the page's storage, so plain HTTP is for local use only.

Whatever the proxy, it needs to:

- **Pass WebSocket upgrades.** Live connections are a WebSocket at `/v1/{space}/live`. Without upgrade headers they fail, and the JS client quietly falls back to HTTP polling, which works but is slower.
- **Allow at least 60 seconds per request.** An HTTP watch waits up to 25 seconds for a change (55 if the client asks), and the server pings each WebSocket every 30 seconds. 75 seconds leaves room.
- **Send `X-Forwarded-For`,** and run where cubby trusts it. Rate limits are per client address. cubby believes `X-Forwarded-For` only from `trusted_proxies`, by default `127.0.0.1` and `::1`. A proxy on another host or in another container needs its address added, or every visitor shares the proxy's limits.
- **Accept request bodies up to 256 KB.** A value is at most 64 KB and a batch of series points about 120 KB.
- **Speak HTTP/2 to browsers,** ideally. Browsers open at most 6 HTTP/1.1 connections per host across all tabs, and each HTTP watch holds one. Pages using live connections don't need it.

To check a proxy, ask for a WebSocket on any valid space name. `101 Switching Protocols` means upgrades get through:

```sh
curl -si -o /dev/null -w "%{http_code}\n" --http1.1 -H "Connection: Upgrade" -H "Upgrade: websocket" \
  -H "Sec-WebSocket-Version: 13" -H "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==" \
  https://cubby.example.com/v1/check-cedar-pilot-mango-frost/live --max-time 2
```

### Caddy

Caddy gets a certificate, speaks HTTP/2, passes WebSockets and sets `X-Forwarded-For` with no extra settings:

```
cubby.example.com {
    reverse_proxy 127.0.0.1:28229
}
```

### nginx

The `map` goes in the `http` context, for example `/etc/nginx/conf.d/connection-upgrade.conf`. It turns `Connection` into `upgrade` for WebSockets and leaves it empty otherwise, which keeps connections to cubby alive between requests.

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    ""      "";
}
```

```nginx
server {
    server_name cubby.example.com;
    listen 443 ssl;
    http2 on;                            # nginx 1.25.1+; before that: listen 443 ssl http2;
    ssl_certificate     /etc/letsencrypt/live/cubby.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/cubby.example.com/privkey.pem;
    client_max_body_size 1m;

    location / {
        proxy_pass http://127.0.0.1:28229;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_buffering off;
        proxy_read_timeout 75s;
    }
}
```

### Apache

Apache 2.4.47 or newer, with `a2enmod proxy proxy_http ssl http2`. `upgrade=websocket` passes live connections; `mod_proxy` adds `X-Forwarded-For` by itself.

```apache
<VirtualHost *:443>
    ServerName cubby.example.com
    SSLEngine on
    SSLCertificateFile    /etc/letsencrypt/live/cubby.example.com/fullchain.pem
    SSLCertificateKeyFile /etc/letsencrypt/live/cubby.example.com/privkey.pem
    Protocols h2 http/1.1

    ProxyPreserveHost On
    ProxyTimeout 75
    ProxyPass        / http://127.0.0.1:28229/ upgrade=websocket
    ProxyPassReverse / http://127.0.0.1:28229/
</VirtualHost>
```

## As a service

A systemd unit, `/etc/systemd/system/cubby.service`:

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

## Upgrading

Install the new binary and restart the service. Stored data carries over unless the release notes say otherwise. Live connections close with code 1001 on shutdown, and the JS client reconnects at once and resumes its watches.

```sh
go install github.com/timjaanson/cubby@v0.13.0 && sudo install -m 755 ~/go/bin/cubby /usr/local/bin/cubby
sudo systemctl restart cubby
curl -s localhost:28229/health          # {"ok":true,"version":"v0.13.0"}
```

Name the version right after a release: the Go module proxy can take a while to notice a new `@latest`.
