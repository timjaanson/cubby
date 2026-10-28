# Using cubby

How to use a cubby server from a page, a shell or anything that speaks HTTP. Installing and running one is in [deploy.md](deploy.md). The full contract is [spec.md](spec.md).

- [How data is organised](#how-data-is-organised)
- [JS client](#js-client)
- [Live updates](#live-updates)
- [Pulses](#pulses)
- [Expiry, limits and timing](#expiry-limits-and-timing)
- [CLI](#cli)
- [Plain HTTP](#plain-http)
- [Series](#series)

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

- Owners invite people with access tokens (`at_...`), usually sent as a link with `#t=at_...`. Redeeming one with `join` gives that visitor a grant. By default an invite makes a full member, works for 10 people and expires after 7 days. Members can invite too, with at most the rights they hold, unless the space says `invite: "owners"`. The owner token (`ot_...`) is the owner's recovery key.
- A private space looks exactly like a missing one to anyone without a grant: 404 `no_space`.
- A key is up to 128 of `A-Z a-z 0-9 . _ : -`, and can't be `.`, `..`, `watch`, `series` or `pulse`. Group with dots (`msg.1791449600123`), because listing and watching select by prefix (`msg.*`).
- A value is UTF-8 text up to 64 KB. Valid JSON comes back as `application/json`, anything else as `text/plain`, always byte for byte as written.
- A series key keeps every value added to it, each with a time, for sensor readings and the like. Its value is the newest point. See [Series](#series).
- Every write gets a version number. Pass it back with `ifVersion` to write only if nobody changed the key meanwhile.
- Private means invited people only. The server's operator can still read everything, so it's not a password vault.

## JS client

The server hosts a small client at `/kv.js`:

```html
<script type="module">
import { kv, createSpace } from "https://cubby.example.com/kv.js"

const db = kv("poll-orbit-lilac-tango-slate")
const me = await db.open()                               // redeems #t=... invites; me.inside, me.owner, me.can()
if (!me.inside) showAskForInvite()
const { link } = await db.share({ label: "Anna" })       // owners and members: an invite link to send
await db.shared.set("question", "Pizza or tacos?")
await db.user().set("vote", "tacos")

db.users.watch("vote", change => console.log(change.user, change.deleted ? "removed" : change.value),
  { onSync: () => render() })                             // after each batch of changes
await db.shared.update("count", n => (n ?? 0) + 1)       // safe with many writers

await db.shared.add("env.*", { temp: 21.4, rh: 45 })     // a point each in env.temp and env.rh
const { points } = await db.shared.series("env.temp", { since: Date.now() - 864e5 })
</script>
```

A page that is a room, shared by passing its link around, can skip the setup with `openRoom`:

```js
import { openRoom } from "https://cubby.example.com/kv.js"

const room = await openRoom("board", { live: true })   // finds #s=... or the last room, or creates one
if (!room.me.inside) showRoomGone()                     // room.forget() before starting a new one
const db = room.db
shareButton.onclick = async () => showQr(await room.link())   // a reusable invite, from owners and members alike
```

It keeps the visitor token in `localStorage`, sends it as a header on writes and as `?token=` on reads (so reads skip CORS preflights), and finds the API from the URL it was loaded from. The module exports `kv`, `createSpace`, `takeInvite`, `inviteLink`, `newName`, `validSpace` and `KvError`. Every entry has `by`, the visitor who wrote it, and `mine`. Its opening comment is a short guide for coding agents; [docs/agents.md](agents.md) is the long one.

For sharing without writing any UI, the server also serves a panel: invite links, QR codes, the member list, who can see the space, and the recovery key.

```html
<script type="module" src="https://cubby.example.com/share.js"></script>
<cubby-share space="poll-orbit-lilac-tango-slate" members></cubby-share>
```

## Live updates

Every watch delivers the current state first, then each change. Over plain HTTP that's a long poll per watch. With `live: true` the client opens one WebSocket per space and changes are pushed:

```js
const db = kv(space, { live: true })
db.shared.watch("card.*", change => move(change.key, change.value), { onSync: render })
await db.shared.set("card.7", { x: 120, y: 40 })
```

- Same API and same results as without it. Requests use the socket while it's open and HTTP otherwise, so nothing waits for it.
- Watches resume from their last version after a dropped connection, a sleeping phone or a server restart, so no change is lost. A watch delivers state, not every event: several quick writes to one key can arrive as the latest value.
- Where WebSockets don't get through (a strict proxy, an older server), the client stays on HTTP by itself. `db.live.status` says which is in use.
- Use it for pages where several people change things often: boards, quizzes, games, presence. A page that saves now and then doesn't need it.
- Write limits stay the same, 120 writes per minute per visitor by default. For cursor positions and other fast traffic, use [pulses](#pulses).

Without the JS client, any WebSocket client can connect to `/v1/{space}/live` and send requests as JSON. [The live contract](spec.md#live-connection).

## Pulses

A pulse is a value that is never stored. It goes to whoever is listening at that moment and is gone. Use pulses for things that move or happen, and stored keys for things to keep:

| Stored key | Pulse |
|---|---|
| a card's position after the drop | the card while it's being dragged |
| who's in the room (`ttl` presence) | everyone's cursor |
| the vote | the confetti when someone votes |
| the game state | the inputs, many per second |

```js
const db = kv(space, { live: true })

addEventListener("pointermove", e => db.user().pulse("cursor", { x: e.clientX, y: e.clientY }))
const stop = db.users.onPulse("cursor", p => { if (!p.mine) drawCursor(p.user, p.value) })

db.shared.pulse("confetti", { by: "Anna" })  // to everyone listening on shared/confetti
db.me.pulse("sync", { tab: "settings" })     // to your own other tabs and devices
```

- **Nothing is kept.** `get`, `list` and `watch` never see a pulse, and it doesn't touch a stored key of the same name. Someone who opens the page a second later doesn't get it.
- **Throttled in the client.** kv.js sends at most 20 pulses a second per key and replaces a waiting value with a newer one, so the latest always arrives. `pulse(key, value, { maxRate })` or `kv(space, { pulseRate })` changes it, `0` turns it off for events that must all arrive. When it drops anything, it says so in one console warning per 10 seconds.
- **Fast and cheap.** Pulses have their own budget, 60 a second per visitor with bursts of 120, apart from the 120 writes a minute. On a live connection they arrive within one round trip, in the order they were sent. A pulse is at most 4 KB.
- **Same rights as the key.** Sending needs write access to the key, listening needs read access. Under `users/{id}/` only that visitor sends, so a cursor can't be faked.
- **Best effort, honestly.** The server keeps the last 10 seconds in memory, so a listener between two HTTP requests or reconnecting a socket catches up. If a listener falls further behind, it gets `onMissed` instead of the old pulses. A restart drops them all.
- **Without the JS client:** `POST /v1/{space}/shared/confetti/pulse` sends, `GET .../confetti/pulse?after=SEQ` long-polls (pass `seq` back as `after`), and on a WebSocket the same requests stream. From a shell: `cubby pulse space/shared/doorbell ding` and `cubby pulses 'space/shared/*'`. Details in the [spec](spec.md#12-pulses).

## Expiry, limits and timing

```sh
cubby limits                         # effective server limits, as JSON
cubby limits notes-cedar-pilot-mango-frost  # this space's limits and your usage
cubby set notes-cedar-pilot-mango-frost/users/me/here online --ttl 30s
cubby time                           # server clock offset and round trip
```

Plain keys can expire after 5 seconds to 30 days. Renewing the same value extends TTL without a new version or waking watchers. A PUT without TTL makes the key permanent. Expiry produces a delete marked `expired: true`. Combine TTL with `--create` and `--if-version` for a cooperative lease. See the [lease recipe and expiry rules](spec.md#expiring-keys-and-leases).

```js
const { limits, usage } = await db.limits()
await db.user().set("here", "online", { ttl: 30 })
await db.clock.sync()
const opensAt = Math.ceil(db.clock.now()) + 2000
const cancel = db.clock.at(opensAt, () => reveal())
await db.user().set("answer", "B", { ifBefore: opensAt + 20000 })
```

Every key returns its value time as `t` in list/watch and `KV-Time` on reads. `/v1/time` supplies the server clock. `ifBefore` enforces a write deadline. Clock estimates and browser timers are subject to network asymmetry and browser scheduling.

Defaults are 120 writes per visitor per minute, burst 30, and 50 open watches per IP. Reads remain 600/IP/minute. Every rate is configurable. `GET /v1/limits` reports the server's actual settings; `/access` includes effective space limits and usage. Data-write responses carry remaining visitor and IP budgets in `RateLimit` headers, exposed by kv.js as `db.writeBudget`. These are snapshots, so clients must still handle 429 and `err.retryAfter`. Writes are never automatically retried after 429 or an uncertain network failure. [Full contract](spec.md#limits-and-server-time).

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
| live connection | WebSocket to `/v1/{space}/live`: the same requests as JSON messages, watches pushed. [Contract](spec.md#live-connection) |
| add a point to a series | `POST /v1/{space}/shared/temp/series`, body is the value |
| add to several series | `POST /v1/{space}/shared/env.*/series`, body `{"temp": 21.4, "rh": 45}` |
| read a series | `GET /v1/{space}/shared/temp/series` (newest 1000) or `?since=MS` (after that time) |
| invite | `POST /v1/{space}/tokens` (owners) → `{"token": "at_...", "uses": 10, "until": ...}` |
| join | `POST /v1/{space}/join` with `{"token": "at_..."}`; without a visitor token one is created and returned |
| what may I do | `GET /v1/{space}/access` |
| owners | `PATCH`/`DELETE /v1/{space}`, `/tokens[/{id}]`, `/grants[/{id}]`, `POST /owner-token` |

Unknown or repeated query parameters, and unknown JSON fields, are rejected with 400, so a typo like `ifversion=0` fails instead of overwriting. Errors are JSON with a stable `error` code and a `message`. CORS is open, so pages on any origin can call it.

Every data URL is `/v1/{space}/{area}/{key or pattern}`, optionally followed by `/watch`, `/series` or `/pulse`, and nothing after that.

The full contract is in [docs/spec.md](spec.md), with worked examples in [docs/scenarios.md](scenarios.md).

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
