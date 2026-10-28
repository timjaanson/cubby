# Plans

This file keeps agreed work that isn't built yet, deferred work, open candidates, what was decided against and why, and a short record of what was built from earlier plans. Built features have their contracts in [spec.md](spec.md).

## Agreed, not built

Agreed 2026-10-28, after building a marble maze demo on live connections and pulses. Each page there needed the same glue: a room link, a hand-made presence map, and its own pulse throttle. The server parts stay generic. Nothing on the server knows about rooms or presence.

Suggested order: pulse throttle, then delegated invites with the room helper, then last will with the presence helper.

### Pulse throttle in kv.js (client only)

Pulses for a key are rate limited in the client by default, so a page that pulses on every `pointermove` doesn't run into the server's 60/s limit and 429s.

- Per area and key, at most `maxRate` sends per second. Default 20. Set per call with `pulse(key, value, { maxRate })`, or for the client with `kv(space, { pulseRate })`. `maxRate: 0` turns the throttle off for that call.
- Latest value wins. A pulse that arrives while the key is waiting for its next slot replaces the waiting one. The newest value is always sent once the slot opens, so the last cursor position or tilt is never lost, only the ones in between.
- `pulse()` resolves `true` when its value was sent, and `false` when a newer value replaced it before sending.
- Discrete events that must all arrive (reactions, button presses) either stay under the rate or use `maxRate: 0` and take the server limit. The docs say so.
- Warnings: one `console.warn` per 10 seconds at most, for all keys together. The first throttling in a quiet period warns at once, then the client collects counts and prints one summary per window, such as `cubby: throttled pulses in the last 10 s: users/u_.../cursor 412 (limit 20/s), shared/confetti 3. Pass { maxRate } to change.` Server 429s on pulses are counted and reported in the same line. Nothing is logged while nothing is throttled.

### Delegated invites (server and client)

Members can invite other people, never with more rights than they hold themselves.

- A new space setting, `invite`: `"owners"` (default, as today) or `"members"`. Owners change it like the other settings. `createSpace({ invite: "members" })` sets it at creation.
- With `"members"`, any visitor holding a grant may `POST /v1/{space}/tokens`. The token's `allow` must be covered by the minter's own grants. Rights from `public` settings don't count, since everyone has them anyway. A member's token can't outlast the minter's grant: `until` and `grantFor` are capped at the grant's `until`. Otherwise it's an ordinary token, with the usual defaults (10 uses, 7 days).
- Members list and revoke only the tokens they minted. Owners see and revoke all of them, each with `by`, the minter.
- Chains are visible. A grant's `source` names the token, and the token names its minter, so an owner can follow who invited whom. `GET /grants` gains `invitedBy` for each grant, from that link.
- Revoking a member keeps everyone they invited. Their grants stand on their own, as grants already outlive their tokens. The revoked member's unredeemed tokens are revoked with them, since they no longer hold the rights those tokens hand out. Owners revoke invitees separately if they want to.
- Switching `invite` back to `"owners"` stops members minting and revokes their unredeemed tokens. Existing grants stay.
- Members' tokens count towards the existing 1000 tokens per space.
- kv.js: `db.share()` works for members when the space allows it. `Access` gains `canInvite`. The share panel shows the invite part to members.

### Room helper in kv.js (client only, uses delegated invites)

One call for the usual "a page is a room, its link lets others in" setup:

```js
const room = await openRoom("marble", { live: true })
// room.db       the kv() client for the room's space
// room.me       the Access from db.open()
// room.created  true if this call created the space
// await room.link()  a reusable invite link for this room, minted once per device and remembered
```

- Reads the space from the URL fragment (`#s=...`, parameter configurable) or the space remembered in localStorage for that label, and creates one with `invite: "members"` when there's neither and `create` isn't `false`.
- Calls `db.open()`, which redeems `#t=...`, then rewrites the fragment to `#s=<space>` so a reload stays in the room.
- `room.link()` mints a token with `uses: null, until: null` on first use and keeps it in localStorage per device and space. Any member's device can do this, so nobody keeps someone else's token.
- `room.me.inside === false` means the room is gone or the link is bad. The page decides what to show. `room.forget()` clears the remembered space, for a "start a new room" button.

### Last-will pulses (server, live connections only)

A pulse the server sends for a client when its live connection ends without the client clearing it. Borrowed from MQTT's last will. The server doesn't know what it means.

- On a live connection: `{"op":"will","path":"users/u_.../here/pulse","body":{...}}` registers or replaces the will for that path. The same message without `body` clears it. At most 8 wills per connection.
- The path and body follow the rules for sending a pulse, and are checked at registration: write access to the key, 4 KB. Registering costs one pulse from the pulse budget. Sending it costs nothing.
- It fires when the connection closes for any reason other than a server shutdown: tab closed, network gone, ping timeout, the client closing after five minutes hidden. Access is checked again before sending, and a will the visitor may no longer send is dropped.
- A server shutdown doesn't fire wills. The window is lost on restart anyway, and clients reconnect.
- Over HTTP there is no connection to lose, so there is no will.
- It's a plain pulse. It never touches stored data, which keeps the rules below intact.
- kv.js: `area.will(key, value)` returns a function that clears it. The client keeps its wills and registers them again whenever its socket opens. On HTTP it keeps them and does nothing, so page code is the same either way.

### Presence helper in kv.js (client, uses last will when available)

```js
const here = db.users.presence("here", { value: { name: "Mac" } })
here.peers                 // Map: visitor id -> { value, since, seen }, without me
here.onJoin = (id, value) => {}
here.onLeave = (id) => {}
here.onChange = (peers) => {}   // after any join, leave or value change
here.set({ name: "Mac", busy: true })   // sent at once, through the throttle
here.stop()                // sends a leave pulse, clears the will, stops listening
```

- Built on pulses to `users/{me}/{key}`. A heartbeat every `interval` (default 2 s) with the current value, a value change sent at once, and a leave pulse on `stop()` and on `pagehide`.
- Registers a will with the leave pulse. On a live connection a closed tab leaves within about one round trip.
- A peer silent for `timeout` (default three intervals, 6 s) counts as gone. Over HTTP that's the only way to notice a leave, and it's fine, so page code doesn't change between transports.
- The helper owns the pulse format, `{"v": value}` for presence and `{"left": true}` for leaving. Other pulses on the same key are ignored, so pages use their own keys for fast data.
- Peers appear on their next heartbeat, so a new page sees everyone within one interval. Starting listeners from the beginning of the pulse window was considered and isn't needed for this.

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
