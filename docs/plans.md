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
"limits": {"spaceBytes": 5242880, "spaceKeys": 10000, "visitorBytes": 1048576, "writesPerMinute": 120, "writeBurst": 30}
```

Pinned spaces and future per-space or per-grant limits show up here with their own values, so a page never has to know about them.

**On every write.** Response headers with the caller's remaining write budget, following the IETF RateLimit header fields draft (`RateLimit-Policy`, `RateLimit`). Added to `Access-Control-Expose-Headers`, readable with `curl -i`.

**Clients.**

- kv.js: `db.limits()` (the access response's `limits` and `usage`), and the last seen remaining budget after each write.
- kv.js: `KvError.retryAfter` as a top-level property. Today it's `err.details.retryAfter`, which Fieldnotes read wrong at first.
- CLI: `cubby limits [space]`.

## 3. Higher default limits for live use, and configurable rates

60 writes a minute per visitor shapes everything in a live page. Autosave, moves, colour changes and (with item 4) presence heartbeats all spend the same budget. The rate limits are also fixed in code today. Only `rate_limits = false` turns them off.

New defaults (agreed 2026-10-22):

| Limit | Now | New | Why |
|---|---|---|---|
| Writes per visitor | 60/min, burst 30 | 120/min, burst 30 | Autosave about once a second with room for moves, edits and presence heartbeats |
| Open watches per IP | 20 | 50 | A few people with a phone and a laptop and a few tabs each |

Everything else stays: writes per IP (300/min), reads per IP (600/min), write bytes per IP, new visitors, new spaces, the miss limit and the storage quotas. Writes per IP now covers 2.5 visitors writing at full rate instead of 5, which is fine since people rarely do that at the same time.

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
- Each write states its own lifetime. A write without `ttl` makes the key permanent, so a key never keeps a ttl someone else set.
- List and watch entries carry `expires` (server ms). A single read gets a `KV-Expires` header. Because renewals don't wake watchers, `expires` in a watch entry is only accurate as of the key's last change. Pages shouldn't build countdowns on it.
- An expired key's delete marker keeps the last writer as `by` and says `"expired": true`, so nobody mistakes it for someone deleting it.
- Bounds: 5 seconds to 30 days, configurable. Not on series keys.

What it's for:

- **Presence.** Each visitor keeps `users/{id}/here` alive with a heartbeat every 15 to 20 s. Watching `users/*/here` is a live roster, and someone leaving (tab closed, phone asleep, signal lost) arrives as a delete. Today that needs timestamps, client-side filtering and timers, and leaving produces no event at all.
- **Edit indicators.** `users/{id}/editing = note.42` with a short ttl lets others see who's typing where, so pages can avoid collisions instead of resolving them.
- **Locks (leases) between several actors.** Pages, parallel scripts, several machines running the same job. Expiry is on the server's clock, so the actors' clocks don't matter.
  - Take: `PUT shared/lock.backup?ifVersion=0&ttl=60` with the holder's own ID as the value. The first writer gets it, everyone else gets 412 and knows who holds it.
  - Keep: write the same value with `ttl=60` and `ifVersion` set to the version it got. Same bytes, so no new version and no watcher wakes. A 412 means the lease was lost (it expired and someone else took it) and the holder must stop.
  - Release: `DELETE ...?ifVersion=v`, so a holder can only release its own lease.
  - A holder that dies or loses its connection releases it on its own when the ttl runs out. Waiting actors watch the key and try again when it's deleted.
  - A holder that stalls longer than the ttl (a sleeping laptop) can come back thinking it still holds the lock. The failed renewal tells it otherwise. Work that must never overlap should check the renewal before each step.
  - CLI helper, like `flock`: `cubby lock <space> <key> --ttl 60s -- ./backup.sh` takes the lease, renews it while the command runs, releases it after, and exits with code 2 if someone else holds it. kv.js could get the same as `db.shared.lock(key, { ttl })` returning a release function.
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

**Closing on time: `ifBefore=T`.** A write condition next to `ifVersion`: the write succeeds only if the server's clock is still before `T` (ms) when it arrives. Otherwise 412 `too_late`, with the server time in the body. Works on `PUT`, `DELETE` and series `POST`, and combines with `ifVersion`. A quiz sets it to `closesAt`, so late answers are refused by the server, not by each page.

Not planned: hiding answers until a reveal (the `sealed` area in spec §19). `users/*/` stays readable by everyone in the space, so a quiz between friends trusts players not to peek.

## 6. Client helper fixes from Fieldnotes

- share.js: assigning `ownerToken` opens the recovery view, and setting `open` right after hid the key in Fieldnotes' integration. Make the two orderable, or document that `ownerToken` alone is enough.
- share.js: "One person", "A group link" and "View only" create a link immediately, but read like a mode choice. Labels that say they create something, like "Create group link".
- kv.js: abort a waiting watch on the window's `offline` event (spec §19, Smaller additions), so a watch-only page shows offline at once.

## 7. Candidates, not agreed

Written down so they aren't lost. Each needs a decision first.

- **Atomic multi-key writes in one space.** One request, one version, all or nothing, counts as one write, and a watch never splits that version across responses. Plain set and delete with optional per-key `ifVersion`, nothing query-like. Cheap with one SQLite file per space, and stops other clients seeing half-created items. Conflicts with the "transactions over several keys" non-goal in design.md, which would have to be revised.
- **kv.js versioned mirror.** `db.shared.mirror("board.*")`: a Map kept in sync from a watch, tracking per-key versions including deletes, that ignores write acknowledgements older than what the watch already delivered. Fieldnotes hit that race (a late acknowledgement brought back a deleted note). Optional sugar over watch, raw HTTP unchanged.

## Rules for all of the above

So that nothing changes stored data in a way that's hard to see coming:

- Stored data changes only because someone wrote or deleted it. The one exception is `ttl` expiry, and that is always visible: the key carried `expires`, and the delete marker says `"expired": true` and names the writer.
- A write means the same over HTTP and over a live connection. Same handlers, same limits, same errors.
- Conditions (`ifVersion`, `ifBefore`) only ever refuse a write. They never change what a write does.
- Clients never retry a write on their own. kv.js reports a retryable error and the page decides. Resuming a watch from its last version is fine, since it only reads.
- Anything not stored never touches stored data, and stored data never arrives through a channel meant for unstored messages.

## Open: a live connection per space

Not decided. Direction as of 2026-10-22, so the thinking isn't redone.

**Two ways to talk to a space, nothing in between.**

- **Plain HTTP**, as today. One request per read or write, long-poll watch per key or pattern. Works from `curl`, scripts, devices and any page. Stays the basic way and keeps every feature it has.
- **One WebSocket per space** for pages that need tight live interaction. Reads, writes and watches over one connection. Watched changes are pushed with no new request per change, so watching costs no per-IP reads, and the connection stays warm in both directions, which matters most on mobile networks and far from the server.

No SSE. With a socket available it would be a third way that adds nothing.

Both are the same store. A write over HTTP wakes watchers on sockets and the other way round, versions are shared, and a script using `curl` and a browser on a socket see each other's changes as they happen.

**State first.** The first version carries stored data only. Unstored messages come later, if an app needs them (see below).

### Signature sketch

```
GET /v1/{space}/live?token=vt_...        Upgrade: websocket
```

A sibling of `/access` and `/join`. Access is checked when the socket opens, and a space the caller can't read fails exactly like a missing one. Messages are JSON. A request is an HTTP route in an envelope: `op` is the method, `target` is the area and key, the rest are the route's query parameters.

```
→ {"id":1,"op":"get","target":"shared/note.42"}
→ {"id":2,"op":"set","target":"shared/note.42","value":"Buy milk","ifVersion":17}
→ {"id":3,"op":"watch","target":"shared/board.*","version":0}
→ {"op":"stop","id":3}

← {"id":1,"ok":{"value":"Milk","version":17,"by":"u_p1r8"}}
← {"id":2,"error":{"status":412,"code":"version_conflict","message":"...","current":18}}
← {"id":3,"ok":{"changes":[...],"version":1791449600061}}          pushed again on every change
```

- Replies and pushes use the same JSON bodies and error codes as the HTTP routes. Internally a socket message goes through the same handlers, so rules, limits and errors can't drift apart. Writes count against the same write limits as HTTP.
- Covers data in the space: get, set, delete, list, watch and series. Creating spaces, joining and managing access stay HTTP only. They happen before or beside live use and aren't time-sensitive.
- A slow watcher drops nothing: it falls behind and catches up to current state, as long-poll does.
- A grant ending while connected ends that watch with an error. Each message is checked against access at that moment, as an HTTP request would be.
- Open sockets count with open watches per IP. One socket per page per space, however many targets it follows. Per space rather than per server because rights are per space.

### kv.js

Switching to live changes one option, not the code:

```js
const db = kv(SPACE, { live: true })      // same API, over one socket
const me = await db.open()

db.shared.watch("board.*", onChange, { onSync })      // as today, now pushed
await db.shared.set("note.42", text, { ifVersion })   // as today, over the socket
db.live.status   // "connecting" | "open" | "offline"
```

- The socket opens on first use and reconnects with backoff. Watches resume from their last version, so nothing is lost across a reconnect.
- A request in flight when the socket drops fails with a retryable `KvError`. kv.js doesn't resend it (see the rules above).
- If the socket can't connect at all (a proxy that blocks it), kv.js falls back to HTTP.
- Frontend state stays the page's own.

### Cost

- Server: a WebSocket library (the Go standard library has none; `github.com/coder/websocket` is small and has no dependencies), a connection loop, routing envelopes into existing handlers (handlers need to accept a request that didn't come from `net/http`), a set of watches per connection, keepalive pings. The watch loop already exists. A few hundred lines plus tests.
- kv.js: the socket, request ids, reconnect and resubscribe, fallback. Roughly 250 lines, and the existing area API on top stays.
- CLI: nothing at first. HTTP covers scripts.
- Proxies need WebSocket upgrade headers. Documented in the deploy notes.

### Later: messages that aren't stored

Working names were `send` and `listen`; better names wanted. Messages that live only in the server's memory and go to whoever is connected at that moment. Nothing is stored, nothing is versioned, nothing is replayed, and nobody would miss one that got lost. Same keys, patterns and rights as stored data (`r` to receive, `w` to send), so sending to `users/{me}/cursor` and receiving `users/*/cursor` gives one cursor per person that nobody else can fake.

Stored data with a socket watch already covers presence (with `ttl`), typing indicators, board edits, quiz rounds and device states. Unstored messages are only for the three cases it can't handle:

1. **High rates.** A cursor or a dragged card moves 10 to 30 times a second. Every stored write hits disk, makes a version and counts against 120 writes a minute.
2. **Every message matters.** Watch merges quick changes into the latest state, so the points of a freehand stroke or a burst of game inputs get lost.
3. **Repeats.** Writing identical bytes wakes nobody, so a doorbell or reaction pressed twice registers once.

Build it when an app needs one of these. Notes for then: a message rate per connection and a small size limit (for example 30 per second, 4 KB), dropping a slow receiver's oldest messages and telling it how many, whether a sender gets its own messages back, and whether HTTP clients can send them with a plain `POST`.

### Still open

- Whether the socket should also do clock sync (item 5), since it's already a warm connection with known round-trip times.
