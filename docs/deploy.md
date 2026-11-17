# Running cubby

## Install and run

```sh
go install github.com/timjaanson/cubby@vX.Y.Z      # Go 1.26+
./build.sh                                          # or static binaries in dist/: macOS, linux-amd64, linux-arm64
cubby serve                                         # port 28229, every interface
cubby serve --listen 127.0.0.1:28229                # behind a proxy
```

It needs no config file, creates its data directory on first start and logs one line per request (`--quiet` stops that). `cubby help serve` shows the data directory it would use.

## Configuration

Flags beat environment variables, which beat the config file: `--listen`/`-p` (`CUBBY_LISTEN`), `--data` (`CUBBY_DATA`), `--config` (`CUBBY_CONFIG`).

```sh
cubby config --defaults > cubby.toml     # every setting, commented, at its default
cubby serve --config cubby.toml
cubby config --config cubby.toml         # what serve would use, without starting
```

Settings worth knowing: `public_url` (the origin tokens are signed for; set it behind a proxy), `trusted_proxies` (whose `X-Forwarded-*` to believe), the `[limits]` and `[retention]` sections, and `[spaces.NAME]` to pin a space with its own limits.

## Behind a reverse proxy

On the internet, run cubby on 127.0.0.1 behind a TLS proxy. The proxy must:

- pass WebSocket upgrades on `/v1/{space}/live` (without them kv.js falls back to HTTP polling);
- allow 75 s per request (watches wait up to 55 s, pings come every 30 s);
- send `X-Forwarded-For`, `X-Forwarded-Host`, `X-Forwarded-Proto` from an address in `trusted_proxies`, or set `public_url`;
- accept 1 MB bodies;
- not log query strings: reads may carry `?token=`, and API keys used by webhooks live there for good.

Caddy does all of that by default:

```
cubby.example.com {
    reverse_proxy 127.0.0.1:28229
    log { format filter { request>uri query { delete token } } }
}
```

nginx:

```nginx
# http context, e.g. /etc/nginx/conf.d/cubby.conf
map $http_upgrade $connection_upgrade { default upgrade; "" ""; }
log_format path_only '$remote_addr - [$time_local] "$request_method $uri $server_protocol" $status $body_bytes_sent "$http_user_agent"';

server {
    server_name cubby.example.com;
    listen 443 ssl;
    http2 on;
    ssl_certificate     /etc/letsencrypt/live/cubby.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/cubby.example.com/privkey.pem;
    access_log /var/log/nginx/access.log path_only;
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

## As a service

```ini
# /etc/systemd/system/cubby.service
[Unit]
Description=cubby
After=network.target

[Service]
ExecStart=/usr/local/bin/cubby serve --listen 127.0.0.1:28229 --data /var/lib/cubby
User=cubby
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

## Data

```
server.db             visitors, devices and their public keys, pairing codes, API keys, who belongs where
spaces/{name}.db      one SQLite file per space: keys, settings, invites, members
admin.sock            how cubby admin reaches the running server
```

Back up by copying the directory with the server stopped, or `sqlite3 file.db ".backup copy.db"` per file while it runs. A space file can move to another server, but visitors live in `server.db`, so its members become unknown IDs there.

## Admin

Run on the server machine as the server's user. It talks to the running server through `admin.sock`; there's no HTTP route.

```sh
cubby admin spaces                     # name, size, keys, last activity
cubby admin stats
cubby admin export <space>             # JSON lines
cubby admin delete <space>
cubby admin owner <space> <visitor>    # make a visitor owner, for an owner who lost every device
cubby admin cleanup                    # run the hourly cleanup now
```

Pass `--data DIR` for a non-default data directory.

## Upgrading

Install the new binary, restart, check `/health`. Live connections close with 1001 and clients reconnect. Name the version (`@vX.Y.Z`): the Go module proxy can lag on `@latest`. A release that can't read older data says so at startup instead of starting empty.
