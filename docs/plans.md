# Plans

Agreed changes that aren't built yet, plus one open question. [spec.md](spec.md) §19 "Later" keeps the wider pool of parked ideas. When an item here is built, its contract moves into spec.md and it's removed from this file.

Most of these came from building Fieldnotes (2026-10-22), a live kanban board with autosave, drag and drop and several people editing at once, using only the public docs, kv.js and share.js. Cubby held up: no contract mismatch was found. The cost was in client code around rate limits, pending writes and collisions, and in not knowing the limits it was running under.

Every item is an addition. Nothing changes the meaning of an existing route.

Order: 1 and 2 first, they're small and remove guesswork. Then 3, 4 and 5. 6 and 7 can go along with any release.

## 1. Adaptive watch pause

Today a watch that a write wakes up always sleeps 50 ms before answering, so a burst of writes goes out in one response (`internal/server/handlers.go`). That pause is about half of what a watcher waits for a single change.

- Answer at once when the change comes after a quiet period. Only pause when writes keep arriving, for example when another change came in within the last 50 ms.
- Bursts still go out together. A single change, like a light switch or a "start" signal, arrives one pause sooner.
- No API change. The spec's "Pause before answering a watch after a change" row becomes "up to 50 ms, only during bursts".

## 2. Limits a client can ask for

Fieldnotes budgeted its autosave against the documented defaults without knowing the server's real values or how much of its budget was left. An app should be able to ask, and slow down before it gets a 429.

**Server-wide values.** `GET /v1/limits` returns the effective limits as JSON: value and point size, storage quotas, rates, watch limits, ttl bounds once those exist. No token needed. Same names as the config keys.

**This space and this caller.** The access response (`GET /v1/{space}/access`) gains:

```json
"usage": {"bytes": 183204, "keys": 412, "visitorBytes": 20480},
"limits": {"spaceBytes": 5242880, "spaceKeys": 10000, "visitorBytes": 1048576, "writesPerMinute": 240, "writeBurst": 60}
```

Pinned spaces and future per-space or per-grant limits show up here with their own values, so a page never has to know about them.

**On every write.** Response headers with the caller's remaining write budget, following the IETF RateLimit header fields draft (`RateLimit-Policy`, `RateLimit`). Added to `Access-Control-Expose-Headers`, readable with `curl -i`.

**Clients.**

- kv.js: `db.limits()` (the access response's `limits` and `usage`), and the last seen remaining budget after each write.
- kv.js: `KvError.retryAfter` as a top-level property. Today it's `err.details.retryAfter`, which Fieldnotes read wrong at first.
- CLI: `cubby limits [space]`.

## 3. Higher default limits for live use, and configurable rates

60 writes a minute per visitor shapes everything in a live page. Autosave, moves, colour changes and (with item 4) presence heartbeats all spend the same budget. The rate limits are also fixed in code today. Only `rate_limits = false` turns them off.

Proposed defaults (numbers to confirm before building):

| Limit | Now | Proposed | Why |
|---|---|---|---|
| Writes per visitor | 60/min, burst 30 | 240/min, burst 60 | Autosave about once a second plus everything else, without a client-side queue |
| Writes per IP | 300/min | 1200/min | Keeps the same 5 visitors of headroom per household or office |
| Reads per IP | 600/min | 3000/min | Every watch answer is followed by a new watch request, which costs a read. Several busy pages behind one router reach 10 per second quickly |
| Open watches per IP | 20 | 100 | A few people with a phone and a laptop and a few tabs each |
| Write bytes per IP | 10 MB/day | 50 MB/day | Whole values are rewritten on every save |

Left as they are: new visitors per IP, new spaces, the miss limit and the storage quotas. Those are what keep anonymous visitors and name guessing in check.

- Every rate becomes a `[limits]` config key, as storage limits already are.
- Open question: whether a watch request that continues from a version should cost a read at all, or count against a separate, larger bucket. Charging every re-poll means busier spaces cost more reads per minute even when nothing else is read.
- Later, building on this: owners raising a space's write rate up to a cap the operator sets (extends "Per-grant quotas" in spec §19).

## 4. Keys that expire: `ttl`

From spec §19 "Key options", now planned. A key that deletes itself after N seconds unless it's written again.

```sh
curl -X PUT "$S/users/$ME/here?ttl=30" -d 'board-1'
cubby set users/$ME/here board-1 --ttl 30s
db.me.set("here", "board-1", { ttl: 30 })   # and on users/, shared
```

- The server stores an expiry time with the key. From that moment reads, lists and watches treat the key as missing.
- A sweep deletes expired keys and leaves the usual delete marker, so watchers get `"deleted": true`, marked `"expired": true`, within about a second. The sweep only visits spaces that have expiring keys.
- Writing the same bytes again with a `ttl` pushes the expiry forward without a new version, so heartbeats don't wake watchers. This extends the existing rule that an identical `PUT` changes nothing.
- A write without `ttl` makes the key permanent again.
- List and watch entries carry `expires` (server ms). A single read gets a `KV-Expires` header.
- Bounds: 5 seconds to 30 days, configurable. Not on series keys.

What it's for:

- **Presence.** Each visitor keeps `users/{id}/here` alive with a heartbeat every 15 to 20 s. Watching `users/*/here` is a live roster, and someone leaving (tab closed, phone asleep, signal lost) arrives as a delete. Today that needs timestamps, client-side filtering and timers, and leaving produces no event at all.
- **Edit indicators.** `users/{id}/editing = note.42` with a short ttl lets others see who's typing where, so pages can avoid collisions instead of resolving them.
- **Leases.** `PUT shared/lock.x?ifVersion=0&ttl=20`: the first writer gets it, everyone else gets 412, and a holder that dies releases it on its own. Works for scripts too, such as one of several machines running a job.
- **Device online status.** A board refreshes `status` with ttl 180 every minute. When it stops, pages see it go offline.
- **Short-lived content.** Typing indicators, doorbell-style signals, an announcement for tonight.

Not a precise timer, and not for anything that must survive a missed heartbeat.

## 5. Write time and a shared clock

For pages where several people must act at the same moment or be timed fairly: a Kahoot-style quiz, a countdown, a reveal, synced playback, a reaction game. These need agreement on time more than a faster connection.

**Write time on every key.** Every entry gets `t`, the server's clock in ms when the write arrived, on reads (`KV-Time`), lists and watches. Series already use `t` for the newest point, so the meaning is the same: the time of the current value. Lets a quiz score answers by when the server got them, and pages show "edited 2 minutes ago".

**Server time.** `GET /v1/time` returns `{"t": 1791449600042}`, nothing else. Cheap, no token, works from `curl`.

**kv.js clock.**

```js
await db.clock.sync()          // several /v1/time samples, keeps the one with the shortest round trip
db.clock.now()                 // server time estimated from the local clock and the offset
db.clock.at(t, fn)             // run fn at server time t
```

- Offset estimate: `serverT - (sent + received) / 2` from the fastest sample. Error is bounded by half the round trip and is usually 10 to 20 ms, about one frame.
- Re-syncs when the page becomes visible or comes back online, since phones drift and sleep.
- CLI: `cubby time` prints the offset and round trip, for scripts that schedule against the server clock.

**How a quiz uses it.** The host writes `round = {"q": 3, "opensAt": T, "closesAt": T + 20000}` with T a few seconds ahead. Every phone shows the question at `opensAt` by `db.clock.at`, so watch latency hides inside the lead time. Players write `users/{id}/a.3`, the server's `t` gives each answer's time, and `t - opensAt` scores speed. A player far from the server loses roughly half their round trip, which is fine among friends.

Open questions for this item:

- **Closing on time.** A late write can't be stopped today. A write condition `ifBefore=T` (server time) would make the server reject answers after `closesAt`. Small and generic, but it's a new condition next to `ifVersion`.
- **Hidden answers.** `users/*/` is readable by everyone in the space, so a player could watch others' answers before answering. The `sealed` area in spec §19 (writes hidden until a reveal) is what fixes that. A quiz is the first concrete case for it.

## 6. Client helper fixes from Fieldnotes

- share.js: assigning `ownerToken` opens the recovery view, and setting `open` right after hid the key in Fieldnotes' integration. Make the two orderable, or document that `ownerToken` alone is enough.
- share.js: "One person", "A group link" and "View only" create a link immediately, but read like a mode choice. Labels that say they create something, like "Create group link".
- kv.js: abort a waiting watch on the window's `offline` event (spec §19, Smaller additions), so a watch-only page shows offline at once.

## 7. Candidates, not agreed

Written down so they aren't lost. Each needs a decision first.

- **Atomic multi-key writes in one space.** One request, one version, all or nothing, counts as one write, and a watch never splits that version across responses. Plain set and delete with optional per-key `ifVersion`, nothing query-like. Cheap with one SQLite file per space, and stops other clients seeing half-created items. Conflicts with the "transactions over several keys" non-goal in design.md, which would have to be revised.
- **kv.js versioned mirror.** `db.shared.mirror("board.*")`: a Map kept in sync from a watch, tracking per-key versions including deletes, that ignores write acknowledgements older than what the watch already delivered. Fieldnotes hit that race (a late acknowledgement brought back a deleted note). Optional sugar over watch, raw HTTP unchanged.

## Open: a live channel next to watch ("listen")

Not decided. Notes so the thinking isn't redone.

**What it would be.** Watch is state: stored, versioned, merged to the latest value, caught up after a gap. A live channel would be events: not stored, no version, no replay, every message delivered while connected, best effort. Same keys and patterns, same access rights (`r` to listen, `w` to send), its own message-rate and size limits, memory only. One key could have watchers and listeners at once. Uses: cursors, drag previews, live drawing, game positions, a slider driving a lamp, reactions.

**Why not just a faster watch.** Any relay through a server costs roughly one round trip from sender to receiver, whatever the transport. One informal measurement (2026-10-22, a single client, keep-alive): 42 ms round trip, write-to-watch about 100 ms, half of it the fixed pause item 1 removes. A stream can't beat a tuned watch by much for a single change. What it adds is rate: every event delivered, no new request per event, no disk write, no per-visitor write budget spent at 30 Hz.

**Transport, SSE plus POST or WebSocket.**

- Receiving costs the same with either: the server pushes on an open connection.
- SSE plus POST works with `curl -N`, needs no new dependency in Go, and reconnects on its own in the browser. The design's reasons for rejecting SSE for watch mostly don't apply: tokens already go as `?token=` on reads, and there's nothing to replay. Proxy buffering still has to be switched off.
- WebSocket gives one connection for both directions. That matters for sending: no HTTP request per message, auth once per connection, messages stay in order, and the sending side can't go cold separately from the receiving side. Over HTTP/1.1, SSE and the POSTs use different connections, and an idle POST connection that a mobile network or NAT has dropped costs a fresh TCP and TLS setup, several round trips, on the next send. Over HTTP/2 both share one connection and most of that difference goes away, but a self-hosted server can't count on HTTP/2 everywhere.
- On mobile or far from the server, both have the same floor of about one round trip. WebSocket's gain is mostly in the slow outliers and in server cost per message, less in the typical delay.
- Costs of WebSocket: a Go dependency (the standard library has none), shell use needs `websocat` or the CLI, its own reconnect logic in kv.js, proxy upgrade config.

**Same room.** For people on the same network, browsers can connect directly with WebRTC data channels (few ms instead of a round trip to the server, and late packets can be dropped instead of waited for). Cubby would only carry the handshake, which ttl keys already cover. No server change, possibly a small optional module.

**Below about 20 ms** (music together, rhythm games): out of reach for anything relayed through a server over the internet.
