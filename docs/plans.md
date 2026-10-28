# Plans

Nothing is agreed and waiting to be built. This file keeps deferred work, open candidates, what was decided against and why, and a short record of what was built from earlier plans. Built features have their contracts in [spec.md](spec.md).

## Deferred lease helpers

TTL plus `ifVersion` supports acquire, renew and release over raw HTTP, the CLI and kv.js. The [spec recipe](spec.md#expiring-keys-and-leases) covers this. A `cubby lock ... -- command` runner and a JS lease helper remain deferred. They need explicit behavior for renewal failures, process termination, cancellation and release. The underlying lease cannot fence an external resource.

## Candidates, not agreed

Written down so they aren't lost. Each needs a decision first.

- **Atomic multi-key writes in one space.** One request, one version, all or nothing, counts as one write, and a watch never splits that version across responses. Plain set and delete with optional per-key `ifVersion`, nothing query-like. Cheap with one SQLite file per space, and stops other clients seeing half-created items. Conflicts with the "transactions over several keys" non-goal in design.md, which would have to be revised.

## Decided against

- **kv.js versioned mirror** (`db.shared.mirror(...)`). Rejected 2026-10-27. It is a large primitive for a narrow race. Pages handle it with specific checks instead: keep the version per key from the watch, including deletes, and ignore a write acknowledgement whose version is older than what the watch already delivered.

## Rules for any change

So that nothing changes stored data in a way that's hard to see coming:

- Stored data changes only because someone wrote or deleted it. The one exception is `ttl` expiry, and that is always visible: the key carried `expires`, and the delete marker says `"expired": true` and names the writer.
- A write means the same over HTTP and over a live connection. Same handlers, same limits, same errors.
- Conditions (`ifVersion`, `ifBefore`) only ever refuse a write. They never change what a write does.
- Clients never retry a write on their own. kv.js reports a retryable error and the page decides. Resuming a watch from its last version is fine, since it only reads.
- Anything not stored never touches stored data, and stored data never arrives through a channel meant for unstored messages.

## Done: HTTP additions

Released in v0.11.0. Adaptive watch pause, queryable limits and usage, write-budget headers, configurable rates, 120 visitor writes/minute, 50 watches/IP, TTL keys, value timestamps, clock helpers, write deadlines and the Fieldnotes client fixes. Reads stay at 600/IP/minute, and continuing HTTP watches still use the read budget.

## Done: a live connection per space

Released in v0.12.0. One WebSocket per space at `/v1/{space}/live`, carrying data requests as HTTP routes in an envelope and pushing watches. Auth comes in the first message or an `Authorization` header, never in the URL, and may change mid-connection. Access is checked per message. kv.js `live: true` uses it while open and HTTP otherwise, with identical results, and falls back to HTTP for good if it can't connect twice before ever opening. Decided against: clock sync over the socket (HTTP clock stays), tokens in the socket URL. The contract is [spec.md](spec.md#live-connection).

## Done: pulses

Released in v0.13.0. The messages that aren't stored are called pulses: `POST {key}/pulse` sends, `GET {key or pattern}/pulse` listens (long poll over HTTP, a stream on a live connection). Same keys, areas and rights as stored data. Own rate limits (60/s per visitor, burst 120; 300/s per IP), 4 KB each. A per-space in-memory window (10 s, 1024 pulses, 256 KB) with a server-wide sequence lets HTTP listeners and reconnecting sockets catch up, and reports `missed` instead of blocking. kv.js `pulse()` and `onPulse()`, CLI `pulse` and `pulses`. Senders get their own pulses back, marked `mine`. Contract: [spec.md](spec.md#12-pulses).
