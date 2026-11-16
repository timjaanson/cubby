# Plans

Nothing is agreed and waiting to be built. This file keeps deferred work, open candidates, what was decided against and why, and a short record of what was built from earlier plans. Built features have their contracts in [spec.md](spec.md).

## Deferred lease helpers

TTL plus `ifVersion` supports acquire, renew and release over raw HTTP, the CLI and kv.js. The [spec recipe](spec.md#expiring-keys-and-leases) covers this. A `cubby lock ... -- command` runner and a JS lease helper remain deferred. They need explicit behavior for renewal failures, process termination, cancellation and release. The underlying lease cannot fence an external resource.

## Candidates, not agreed

Written down so they aren't lost. Each needs a decision first.

- **Atomic multi-key writes in one space.** One request, one version, all or nothing, counts as one write, and a watch never splits that version across responses. Plain set and delete with optional per-key `ifVersion`, nothing query-like. Cheap with one SQLite file per space, and stops other clients seeing half-created items. Conflicts with the "transactions over several keys" non-goal in design.md, which would have to be revised.

## Decided against

- **A `mine=false` filter on watches and pulses**, in the server or kv.js. Rejected 2026-11-14. Doing it right needs exceptions nobody would guess: expiry markers carry the original writer, snapshots must stay whole, and two tabs of one visitor would hide each other's edits. Pages write `if (!c.mine)` where they mean it.
- **`to/all`.** With a slot per sender it is `by/{id}/` again, not `shared/`.
- **Groups or teams inside a space.** A second permission system beside levels. Small groups are one `to/` write per member; larger ones are a space each, since visitor IDs are the same in every space and owners set levels directly.
- **`lock=owner` on shared keys**, or a `write.shared: owners` setting. Rejected 2026-11-15. `by/` already gives creator-only keys, and `access: "post"` invites keep members out of `shared/`.
- **Server-ordered keys** (an append verb where the server picks `msg.<time>`). Rejected 2026-11-15. Clock skew is small in practice, and a `PUT` key never changes.
- **Listing a visitor's spaces** (`GET /v1/spaces`). Held 2026-11-15: it needs an index of grants in `server.db` kept in step with every space file. Maybe later, with a cross-space live connection.
- **A server-side block list per recipient.** The server would read stored data to decide access, which nothing else does. Pages keep a list in `to/me/` and skip those senders.
- **Owners deleting in other visitors' inboxes.** There's no path that names both a sender and a recipient besides the two ends, and owners can't read the content anyway. A ban plus the recipient's own delete covers it.

- **kv.js versioned mirror** (`db.shared.mirror(...)`). Rejected 2026-10-27. It is a large primitive for a narrow race. Pages handle it with specific checks instead: keep the version per key from the watch, including deletes, and ignore a write acknowledgement whose version is older than what the watch already delivered.

## Done: levels and by/

Agreed and built 2026-11-15, after a small model built a chat on cubby and put every message in `shared/`, where any member could rewrite them. The plan is in the project notes. Contracts in [spec.md](spec.md#4-spaces-and-access).

- **One level per visitor and space**: `banned < none < read < post < all < owner`. `post` writes your own keys, `all` also common keys. A visitor's level is the highest of their grants, or the space's `anyone` level, and a ban beats both.
- **Settings are two levels**: `anyone` (strangers, `none` by default) and `invite` (the lowest member level that may invite, `post` by default). Scopes, `read`/`write` per area, the `public` shorthand, `member`/`viewer` presets and `custom` are gone.
- **Invites give a level**: `access` on `POST /tokens`, `read`, `post` or `all`, default `all` or the minter's own level. A member gives at most their own.
- **`/members`** replaces `/grants` and `/bans`: members list who's in; owners `PUT` a level (replacing every grant) or `banned`, and `DELETE`; anyone leaves with `DELETE /members/me`. A ban now removes the visitor's grants.
- **`users/` is `by/`.** Under `by/`, an item's `by` is its author, and the `user` field is gone. kv.js `db.mine`, `db.by`, `db.by(id)`, `db.presence`, `db.members()`, `db.leave()`, `db.owner.members`, `me.access` and `me.can()`. CLI `--anyone`, `--access`, `cubby member`. The owner token field is `ownerToken` everywhere.
- **Space migration** (user_version 4) maps every grant, invite and setting to the closest level and renames `u:` rows to `b:`. A production scan before building found only owner grants and `*:rw`, so nothing lost precision.

## Done: device links

One visitor on several devices. A device makes a one-use code (`POST /v1/links`, 10 minutes, 50 bits, typed as `K7M2X-9Q4PA`), another redeems it (`POST /v1/links/redeem`, the code as the body) and gets its own token for the same visitor. Redeeming replaces the device's previous identity, deleting it if nothing else uses it. `/v1/devices` lists, labels and signs out devices. kv.js `devices()`, CLI `cubby device`. Differences from the plan: signed-out devices' live connections lose their identity instead of closing, so the page that just linked re-auths in place; there's no `has_identity` check or `replace` flag; the reverse direction, a new device showing a QR for an existing one to approve, is in spec.md Later. The contract is [spec.md](spec.md#devices-and-links).

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

## Done: throttle, delegated invites, rooms, last will, presence

Agreed and built 2026-10-28, after a marble maze demo on live connections and pulses showed the glue every live page was writing for itself. Contracts in [spec.md](spec.md).

- **Pulse throttle** in kv.js: 20 per second per key by default, newest value wins, one console warning per 10 seconds for all keys together. `maxRate` per call, `pulseRate` per client, `0` to turn it off.
- **Delegated invites**: the `invite` setting, `members` by default, `owners` to restrict. A member's token gives at most what they hold, defaults to exactly that, and works only while they could still mint it. Invitees stay when their inviter is removed; the inviter's open tokens end as `revoked`. `by` on tokens, `invitedBy` on grants. kv.js moved tokens from `db.owner.tokens` to `db.tokens`. Differences from the plan: the default is `members`, and a member's token isn't capped at their grant's end. Instead redeeming checks the minter's rights, which also covers expiry.
- **`openRoom`** in kv.js: find or create the room, open it, keep `#s=` in the address, and `link()` a reusable invite per visitor, checked before reuse.
- **Last will** on live connections: `op: "will"`, up to 8 per connection, checked and charged like a pulse at registration, sent on any close except a server shutdown, with access checked again. kv.js `area.will()` registers again after reconnects and is a no-op over HTTP.
- **Presence** in kv.js: `db.users.presence(key, { value })` with `peers`, `onJoin`, `onLeave`, `onChange`. Heartbeats every 2 s, a will and a pagehide pulse for leaving, and a 6 s timeout, which is how leaving shows over HTTP.

## Done: to/, inbox/ and bans

Agreed and built 2026-11-14. Contracts in [spec.md](spec.md#to-and-inbox).

- **`to/{recipient}/` and `inbox/{sender}/`**: one slot per recipient, sender and key, seen from both ends. Recipients are a visitor ID, `me` or `owners`; `to/*/` lists what you sent, `inbox/*/` what you got, `inbox/owners/` the owners' group mail. Items carry `from` and `to`. Pulses and wills work there, so two pages can signal each other directly.
- **`me/` removed.** `to/me/` is the private slot, and a space migration (user_version 3) moved rows and `me` scopes there. `me` stands for your own ID in `users/me/`, `to/me/` and `inbox/me/`, on the server.
- **Scopes** `to[/RECIPIENT[/TARGET]]:rw`, settings `write.to`. `respond` lets anyone send.
- **Bans**: per visitor and space, owners only, explicit 403 `banned` on every request including reads in private spaces, live sockets closed with 4403, wills dropped. A ban deletes and revokes nothing.

## Done: roomier defaults, paged by bytes

Agreed and built 2026-11-15, so chat and shared documents work on a default server. Values 512 KB (cap 4 MB, pinned too), points 4 KB like pulses with 20,000 per series so a full series stays under a space, 25 MB per visitor, 100 MB and 100,000 keys per space, 5 GB per server, 1200 reads per IP a minute, 100 new visitors per IP an hour, 20 new spaces per visitor and 50 per IP a day, TTL up to 90 days. The per-IP byte limit now counts growth of stored data (1 GB a day, `stored_limit`) instead of request bodies, so rewriting a document in place is free. List pages, watch responses and series reads stop at 4 MB of keys and values and continue through their usual cursors. Live connections accept messages sized from the largest value instead of a fixed 256 KB.
