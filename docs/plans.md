# Plans

The agreed HTTP additions are implemented: adaptive watch pause, queryable limits and usage, write-budget headers, configurable rates, 120 visitor writes/minute, 50 watches/IP, TTL keys, value timestamps, clock helpers, write deadlines and the Fieldnotes client fixes. Their contracts now live in [spec.md](spec.md). Reads remain 600/IP/minute, and continuing HTTP watches still consume the read budget.

No live transport was introduced. The section below preserves that open design question.

## Deferred lease helpers

TTL plus `ifVersion` supports acquire, renew and release over raw HTTP, the CLI and kv.js. The [spec recipe](spec.md#expiring-keys-and-leases) covers this. A `cubby lock ... -- command` runner and a JS lease helper remain deferred. They need explicit behavior for renewal failures, process termination, cancellation and release. The underlying lease cannot fence an external resource.

## Candidates, not agreed

Written down so they aren't lost. Each needs a decision first.

- **Atomic multi-key writes in one space.** One request, one version, all or nothing, counts as one write, and a watch never splits that version across responses. Plain set and delete with optional per-key `ifVersion`, nothing query-like. Cheap with one SQLite file per space, and stops other clients seeing half-created items. Conflicts with the "transactions over several keys" non-goal in design.md, which would have to be revised.

## Decided against

- **kv.js versioned mirror** (`db.shared.mirror(...)`). Rejected 2026-10-27. It is a large primitive for a narrow race. Pages handle it with specific checks instead: keep the version per key from the watch, including deletes, and ignore a write acknowledgement whose version is older than what the watch already delivered.

## Rules for all of the above

So that nothing changes stored data in a way that's hard to see coming:

- Stored data changes only because someone wrote or deleted it. The one exception is `ttl` expiry, and that is always visible: the key carried `expires`, and the delete marker says `"expired": true` and names the writer.
- A write means the same over HTTP and over a live connection. Same handlers, same limits, same errors.
- Conditions (`ifVersion`, `ifBefore`) only ever refuse a write. They never change what a write does.
- Clients never retry a write on their own. kv.js reports a retryable error and the page decides. Resuming a watch from its last version is fine, since it only reads.
- Anything not stored never touches stored data, and stored data never arrives through a channel meant for unstored messages.

## Next: a live connection per space

Agreed as the next build on 2026-10-27, then unstored messages after it, one at a time. The design below is the starting point, not final. Direction as of 2026-10-22, so the thinking isn't redone.

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

- Whether the socket should also do clock sync (the HTTP clock helpers), since it's already a warm connection with known round-trip times.
