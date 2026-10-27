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

## Done: a live connection per space

Built 2026-10-27. One WebSocket per space at `/v1/{space}/live`, carrying data requests as HTTP routes in an envelope and pushing watches. Auth comes in the first message or an `Authorization` header, never in the URL, and may change mid-connection. Access is checked per message. kv.js `live: true` uses it while open and HTTP otherwise, with identical results, and falls back to HTTP for good if it can't connect twice before ever opening. Decided against: clock sync over the socket (HTTP clock stays), tokens in the socket URL. The contract is [spec.md](spec.md#live-connection).

## Next: messages that aren't stored

Agreed as the next build after the live connection. Check with TJ before starting. The notes below are the starting point.

Working names were `send` and `listen`; better names wanted. Messages that live only in the server's memory and go to whoever is connected at that moment. Nothing is stored, nothing is versioned, nothing is replayed, and nobody would miss one that got lost. Same keys, patterns and rights as stored data (`r` to receive, `w` to send), so sending to `users/{me}/cursor` and receiving `users/*/cursor` gives one cursor per person that nobody else can fake.

Stored data with a socket watch already covers presence (with `ttl`), typing indicators, board edits, quiz rounds and device states. Unstored messages are only for the three cases it can't handle:

1. **High rates.** A cursor or a dragged card moves 10 to 30 times a second. Every stored write hits disk, makes a version and counts against 120 writes a minute.
2. **Every message matters.** Watch merges quick changes into the latest state, so the points of a freehand stroke or a burst of game inputs get lost.
3. **Repeats.** Writing identical bytes wakes nobody, so a doorbell or reaction pressed twice registers once.

Build it when an app needs one of these. Notes for then: a message rate per connection and a small size limit (for example 30 per second, 4 KB), dropping a slow receiver's oldest messages and telling it how many, whether a sender gets its own messages back, and whether HTTP clients can send them with a plain `POST`.

