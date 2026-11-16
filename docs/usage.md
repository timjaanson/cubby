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
├── shared/{key}      common keys: what the group edits together
├── by/{id}/{key}     each visitor's own keys: posts, votes, names. Only that visitor writes them
└── to/{id}/{key}     sent to one visitor; they read it as inbox/{sender}/{key}

me stands for your own ID anywhere one goes: by/me/, to/me/ (private to you), inbox/me/.
to/owners/{key} reaches every owner, who read it under inbox/owners/{sender}/{key}.
```

- A visitor is an anonymous identity with a visitor token (`vt_...`), the only credential a request ever carries. The CLI and the JS client get one by themselves when they need it.
- Everyone in a space has one level. Lowest first:

| Level | May |
|---|---|
| `read` | read the space, and their own inbox |
| `post` | also write their own keys under `by/me/` and send under `to/` |
| `all` | also write common keys under `shared/` |
| `owner` | also delete others' posts, change settings, manage members |

- A space has two settings. `anyone` is the level strangers get: `none` (the default, private), `read`, `post` or `all`. `invite` is the lowest member level that may invite others, `post` unless changed.
- Invites (`at_...`) are usually sent as a link with `#t=at_...`. Redeeming one with `join` gives that visitor the invite's level. By default an invite gives `all` (or the inviter's own level if lower), works for 10 people and expires after 7 days. The owner token (`ot_...`) is the owner's recovery key.
- A private space looks exactly like a missing one to anyone without a grant: 404 `no_space`.
- A key is up to 128 of `A-Z a-z 0-9 . _ : -`, and can't be `.`, `..`, `watch`, `series` or `pulse`. Group with dots (`msg.1791449600123`), because listing and watching select by prefix (`msg.*`).
- A value is UTF-8 text up to 512 KB. Valid JSON comes back as `application/json`, anything else as `text/plain`, always byte for byte as written.
- A series key keeps every value added to it, each with a time, for sensor readings and the like. Its value is the newest point. See [Series](#series).
- Every write gets a version number. Pass it back with `ifVersion` to write only if nobody changed the key meanwhile.
- Private means invited people only. The server's operator can still read everything, so it's not a password vault.

## JS client

The server hosts a small client at `/kv.js`:

```html
<script type="module">
import { kv, createSpace } from "https://cubby.example.com/kv.js"

const db = kv("poll-orbit-lilac-tango-slate")
const me = await db.open()                               // redeems #t=... invites; me.access, me.can()
if (!me.can("read")) showAskForInvite()
const { link } = await db.share({ label: "Anna" })       // owners and members: an invite link to send
await db.shared.set("question", "Pizza or tacos?")      // needs level all
await db.mine.set("vote", "tacos")                       // your own key, needs post

db.by.watch("vote", change => console.log(change.by, change.deleted ? "removed" : change.value),
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
if (!room.me.can("read")) showRoomGone()               // room.forget() before starting a new one
const db = room.db
shareButton.onclick = async () => showQr(await room.link())   // a reusable invite, from owners and members alike
```

It keeps the visitor token in `localStorage`, sends it as a header on writes and as `?token=` on reads (so reads skip CORS preflights), and finds the API from the URL it was loaded from. The module exports `kv`, `createSpace`, `openRoom`, `devices`, `takeInvite`, `inviteLink`, `newName`, `validSpace` and `KvError`. Every entry has `by` (under `by/` its author, elsewhere whoever last wrote it) and `mine`. Its opening comment is a short guide for coding agents; [docs/agents.md](agents.md) is the long one.

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

addEventListener("pointermove", e => db.mine.pulse("cursor", { x: e.clientX, y: e.clientY }))
const stop = db.by.onPulse("cursor", p => { if (!p.mine) drawCursor(p.by, p.value) })

db.shared.pulse("confetti", { by: "Anna" })  // to everyone listening on shared/confetti
db.to("me").pulse("sync", { tab: "settings" })   // to your own other tabs and devices
db.to(peerId).pulse("rtc.offer", offer)          // only peerId hears it, on db.inbox.onPulse("rtc.*", ...)
```

- **Nothing is kept.** `get`, `list` and `watch` never see a pulse, and it doesn't touch a stored key of the same name. Someone who opens the page a second later doesn't get it.
- **Who's here.** `db.presence("here", { value: { name } })` keeps `peers` with everyone else on the page, with `onJoin`, `onLeave` and `onChange`. On a live connection a closed tab leaves within a round trip, through a last will the server sends for it. Over HTTP a peer leaves after 6 seconds of silence. The same code works either way.
- **Throttled in the client.** kv.js sends at most 20 pulses a second per key and replaces a waiting value with a newer one, so the latest always arrives. `pulse(key, value, { maxRate })` or `kv(space, { pulseRate })` changes it, `0` turns it off for events that must all arrive. When it drops anything, it says so in one console warning per 10 seconds.
- **Fast and cheap.** Pulses have their own budget, 60 a second per visitor with bursts of 120, apart from the 120 writes a minute. On a live connection they arrive within one round trip, in the order they were sent. A pulse is at most 4 KB.
- **Same rights as the key.** Sending needs the level that writes the key, listening needs `read`. Under `by/{id}/` only that visitor sends, so a cursor can't be faked.
- **Best effort, honestly.** The server keeps the last 10 seconds in memory, so a listener between two HTTP requests or reconnecting a socket catches up. If a listener falls further behind, it gets `onMissed` instead of the old pulses. A restart drops them all.
- **Without the JS client:** `POST /v1/{space}/shared/confetti/pulse` sends, `GET .../confetti/pulse?after=SEQ` long-polls (pass `seq` back as `after`), and on a WebSocket the same requests stream. From a shell: `cubby pulse space/shared/doorbell ding` and `cubby pulses 'space/shared/*'`. Details in the [spec](spec.md#12-pulses).

## Values for one reader

`by/{id}/` is public to everyone in the space. For something one person should see, send it under `to/`:

```js
await db.to("owners").set("answer.3", "B")          // quiz answer, only the owners read it
await db.to(playerId).set("hand", ["7H", "QS"])     // a dealt card, only that player reads it
await db.to("me").set("draft", text)                // private to you

const { items } = await db.inbox.list("answer.*")   // what others sent you: items have from and to
db.inbox.watch("hand", c => show(c.value))           // a card arrives
await db.inbox.from(senderId).delete("hand")         // the recipient can delete it

const answers = await db.inbox.owners.list("answer.*")   // owners: what was sent to the owners
const sent = await db.sent.list("*")                     // what you sent, items have to
```

- **One slot per sender.** Bob and Cara both sending `answer.3` to the owners fill two slots. Bob sending it again replaces his own.
- **Your own sends echo back**, like every write: `to("me")` entries appear in your inbox, from you, and `c.mine` is true for anything you wrote.
- **Pulses work too.** `db.to(peerId).pulse("rtc.offer", offer)` reaches only `peerId`'s `db.inbox.onPulse(...)`. That's enough for WebRTC signaling.
- **Sending takes `post`.** In an open space with `anyone: "post"`, anyone can send, which is a contact form: `db.to("owners")`.
- Plain HTTP: `PUT /v1/{space}/to/{id|me|owners}/{key}` sends, `GET /v1/{space}/inbox/*/{key}` reads. [The contract](spec.md#to-and-inbox).

## Members and bans

```js
const people = await db.members()                         // [{ id, access, since }], owners see more
await db.owner.members.set(visitorId, "read")             // exactly one level: read, post, all or owner
await db.owner.members.set(visitorId, "banned", { until: "1d" })   // or no until for no end
await db.owner.members.remove(visitorId)                  // a stranger again; lifts a ban
await db.leave()                                          // anyone: leave the space
```

A banned visitor gets 403 `banned` for every request in the space and their live connections close. `db.open()` returns `{ access: "banned", until }`. A ban removes their grants, not their data. Delete their messages separately, with `db.by(id).delete(key)` as an owner. From a shell: `cubby member set <space> <id> banned --until 1d`.

To let one person mute another, keep a list in `db.to("me")` and skip those senders in the page. The server has no per-person block.

## Your devices

Sharing gives *another person* a grant in one space. Linking makes *another device you*: the same visitor ID, with every grant and all data, in every space. A device makes a one-use code that lives 10 minutes; the other device redeems it.

```js
import { devices } from "https://cubby.example.com/kv.js"
const me = devices()

// The device that's already you: show the link or the code, then wait.
const link = await me.link()                    // { code, display: "K7M2X-9Q4PA", url: "<this page>#link=lk_...", done, cancel() }
showQr(link.url); showText(link.display)
const { device } = await link.done              // { id, label: "Safari on iOS" }; rejects link_expired, link_cancelled

// The new device: never automatic, ask first.
if (me.pending() && confirm("Use this device as your account from the other one? Anything stored here so far goes away."))
  await me.accept()                             // or me.accept(typedCode); { id, previous, deleted }

await me.list()                                 // [{ id, label, created, lastSeen, linkedBy, current }]
await me.rename("Work laptop"); await me.remove(id); await me.signOutOthers()
```

Redeeming always replaces the identity the device had. If nothing else uses that old visitor, it is deleted with its data, so a page should ask before calling `accept()`. A signed-out device's token stops working at once. The last device can't be signed out.

From a shell or another language it's two requests:

```sh
curl -s -X POST -H "Authorization: Bearer $VT" $S/v1/links            # {"id": "l_...", "code": "lk_...", "display": "K7M2X-9Q4PA", ...}
curl -s -H "Authorization: Bearer $VT" "$S/v1/links/l_...?wait=60"   # waits until used: "state": "used", "device": {...}
curl -s -d K7M2X-9Q4PA $S/v1/links/redeem                             # on the new device: {"token": "vt_...", "id": "u_...", ...}
curl -s -H "Authorization: Bearer $VT" $S/v1/devices
curl -s -X DELETE -H "Authorization: Bearer $VT" "$S/v1/devices?others=1"
```

```python
import json, urllib.request
code = input("Code from your other device: ")
r = urllib.request.urlopen(urllib.request.Request(S + "/v1/links/redeem", data=code.encode()))
token = json.load(r)["token"]                   # save it before anything else
```

The CLI: `cubby device link` prints the code and waits, `cubby device accept K7M2X-9Q4PA` on the other machine, and `cubby device ls|name|rm`.

## Expiry, limits and timing

```sh
cubby limits                         # effective server limits, as JSON
cubby limits notes-cedar-pilot-mango-frost  # this space's limits and your usage
cubby set notes-cedar-pilot-mango-frost/by/me/here online --ttl 30s
cubby time                           # server clock offset and round trip
```

Plain keys can expire after 5 seconds to 90 days. Renewing the same value extends TTL without a new version or waking watchers. A PUT without TTL makes the key permanent. Expiry produces a delete marked `expired: true`. Combine TTL with `--create` and `--if-version` for a cooperative lease. See the [lease recipe and expiry rules](spec.md#expiring-keys-and-leases).

```js
const { limits, usage } = await db.limits()
await db.mine.set("here", "online", { ttl: 30 })
await db.clock.sync()
const opensAt = Math.ceil(db.clock.now()) + 2000
const cancel = db.clock.at(opensAt, () => reveal())
await db.mine.set("answer", "B", { ifBefore: opensAt + 20000 })
```

Every key returns its value time as `t` in list/watch and `KV-Time` on reads. `/v1/time` supplies the server clock. `ifBefore` enforces a write deadline. Clock estimates and browser timers are subject to network asymmetry and browser scheduling.

Defaults are 120 writes per visitor per minute, burst 30, and 50 open watches per IP. Reads are 1200/IP/minute, and stored data may grow by 1 GB per IP per day; rewrites in place don't count. Every rate is configurable. `GET /v1/limits` reports the server's actual settings; `/access` includes effective space limits and usage. Data-write responses carry remaining visitor and IP budgets in `RateLimit` headers, exposed by kv.js as `db.writeBudget`. These are snapshots, so clients must still handle 429 and `err.retryAfter`. Writes are never automatically retried after 429 or an uncertain network failure. [Full contract](spec.md#limits-and-server-time).

## CLI

Paths mirror the URL after `/v1/`. Quote paths with `*`.

```sh
cubby get   notes-cedar-pilot-mango-frost/shared/hello
cubby set   notes-cedar-pilot-mango-frost/shared/hello "new text"       # or pipe the value on stdin
cubby del   notes-cedar-pilot-mango-frost/shared/hello
cubby ls    'notes-cedar-pilot-mango-frost/shared/msg.*'                # key<TAB>value per line
cubby ls    'poll-orbit-lilac-tango-slate/by/*/vote'                    # author<TAB>key<TAB>value
cubby set   poll-orbit-lilac-tango-slate/by/me/vote tacos               # by/me is your own ID
cubby watch notes-cedar-pilot-mango-frost/shared/hello                  # current value, then each change
cubby update homelab-maple-quilt-tundra-cobalt/shared/count --exec 'jq "(. // 0) + 1"'
cubby new status --anyone read                          # status-<four words>, then the owner token
cubby new status-cedar-pilot-mango-frost                # exactly this name, private; fails if it's taken
cubby name poll                                         # a fresh valid name, offline
cubby whoami [--show-token] | url <path>
```

Sharing:

```sh
cubby token new family-cedar-pilot-mango-frost --label anna --link https://example.com/list/
                                                        # https://example.com/list/#t=at_...  all, 10 uses, 7 days
cubby token new homelab-... --access post --uses 1 --until 1h   # a machine that writes its own by/ keys
cubby join family-cedar-pilot-mango-frost 'https://example.com/list/#t=at_...'  # on the other machine
cubby access <space>                                    # your level: none, read, post, all or owner
cubby settings <space> [--anyone none|read|post|all] [--invite read|post|all|owner]
cubby token ls|set|rm <space> ...    cubby member ls|set|rm <space> ...
cubby owner-token <space>            cubby drop <space>
```

Your devices (this visitor on another machine, not sharing):

```sh
cubby device link [--ttl 10m] [--link URL]              # prints K7M2X-9Q4PA and waits until it's used
cubby device accept K7M2X-9Q4PA                          # on the other machine: become that visitor
cubby device ls | name <label> | rm <id> | rm --others
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
| create a space | `POST /v1/spaces` with `{"label": "poll", "anyone": "post"}` (all optional) → `{"space", "ownerToken": "ot_...", "id", "settings"}`, plus `"token"` if you sent none |
| get a visitor token | `POST /v1/anon` → `{"token": "vt_...", "id": "u_..."}` |
| read | `GET /v1/{space}/shared/{key}`, with `?token=vt_...` unless the space is open to anyone |
| write | `PUT /v1/{space}/shared/{key}` with `Authorization: Bearer vt_...`, body is the value |
| write only if unchanged | `PUT ...?ifVersion=N` (`0` means "only if it doesn't exist") |
| delete | `DELETE /v1/{space}/shared/{key}` |
| list: matching keys with their values | `GET /v1/{space}/shared/msg.*` or `GET /v1/{space}/by/*/vote` |
| wait for changes | `GET /v1/{space}/shared/msg.*/watch?version=N` (long poll, 25 s) |
| live connection | WebSocket to `/v1/{space}/live`: the same requests as JSON messages, watches pushed. [Contract](spec.md#live-connection) |
| add a point to a series | `POST /v1/{space}/shared/temp/series`, body is the value |
| add to several series | `POST /v1/{space}/shared/env.*/series`, body `{"temp": 21.4, "rh": 45}` |
| read a series | `GET /v1/{space}/shared/temp/series` (newest 1000) or `?since=MS` (after that time) |
| invite | `POST /v1/{space}/tokens` with optional `{"access": "post"}` → `{"token": "at_...", "access", "uses": 10, "until": ...}` |
| join | `POST /v1/{space}/join` with `{"token": "at_..."}`; without a visitor token one is created and returned |
| what may I do | `GET /v1/{space}/access` → `{"access": "post", ...}` |
| members | `GET /v1/{space}/members`; owners `PUT /members/{id}` with `{"access": "read"}` or `"banned"`; `DELETE /members/{id or me}` |
| link another device | `POST /v1/links` → `{"id", "code", "display"}`; the other device `POST /v1/links/redeem` with the code as the body → `{"token", "id"}` |
| your devices | `GET /v1/devices`, `PATCH`/`DELETE /v1/devices/{id or me}`, `DELETE /v1/devices?others=1` |
| owners | `PATCH /v1/{space}` with `{"anyone", "invite"}`, `DELETE /v1/{space}`, `/tokens[/{id}]`, `POST /owner-token` |

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
- Points are stored as sent, up to 4 KB each. The server never calculates anything from them.
- `PUT` on a series returns 409 `is_series`, and `/series` on a plain key returns 409 `not_series`. `DELETE` removes a series with all its points.
- Each series keeps its newest 20,000 points and drops the oldest, so a device that sends forever never starts failing.
- Lists and watches include `t` for series keys. Watching gives the newest point, and `series?since=` catches up on every point.
- A `POST` counts as one write for rate limits, however many series it adds to. Points count towards the space's storage, not the visitor's.
