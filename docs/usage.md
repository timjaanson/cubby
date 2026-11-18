# Using cubby

From a page, a shell, or anything that speaks HTTP. The HTTP details are in [api.md](api.md), running a server in [deploy.md](deploy.md).

## The model

```
space                 notes-cedar-pilot-mango-frost    owners, settings, invites
├── shared/{key}      common keys: what the group edits together
├── by/{id}/{key}     each visitor's own keys: posts, votes, names. Only that visitor writes them
└── to/{id}/{key}     for one reader, who reads it as inbox/{sender}/{key}
```

- A **visitor** is an anonymous identity (`u_...`). A visitor has one or more **devices**, each a key pair. The private key never leaves the device; requests carry a token signed with it that lasts at most an hour.
- Everyone in a space has one **level**:

| Level | May |
|---|---|
| `read` | read the space, and their own inbox |
| `post` | also write their own keys under `by/me/` and send under `to/` |
| `all` | also write common keys under `shared/` |
| `owner` | also delete others' posts, change settings, manage members |

- A space has two settings. `anyone` is the level strangers get: `none` (default, private), `read`, `post` or `all`. `inviters` is the lowest member level that may invite, `post` unless changed.
- A private space looks exactly like a missing one to anyone not in it: 404 `no_space`.
- `me` stands for your own ID: `by/me/`, `to/me/` (private to you). `to/owners/` reaches every owner.
- Keys are up to 128 of `A-Z a-z 0-9 . _ : -`. Group with dots (`msg.1791449600123`) and select with a trailing `*` (`msg.*`).
- Values are UTF-8 text up to 512 KB. Valid JSON comes back as JSON, anything else as text, byte for byte.
- Every write gets a version. `ifVersion: N` writes only if the key is still at N, `0` only if it doesn't exist.
- A time is ms since 1970. A duration (`ttl`) is seconds or `"90s"`, `"10m"`, `"12h"`, `"14d"`. Anything that ends takes `ttl` and reports `expires`.
- The server operator can read everything. Private means invited people only, not a password vault.

## Choosing for the user

Leave a space private unless the user said strangers should see or change it. "For my family", "my team", "me and my friends" mean private plus invites. "Anyone can vote", "a public status page" mean giving `anyone` a level. A private space that should have been public fails visibly and gets fixed. A public one that should have been private leaks quietly. If unsure, ask.

| The user wants | `anyone` | Sharing |
|---|---|---|
| something for themselves, on several devices | `none` | none: pair the devices |
| something for people they know | `none` | an invite link per person or group |
| a public page only they write | `read` | none |
| a poll, quiz or sign-up strangers answer | `post` | invites for co-editors |
| a public room anyone with the link writes in | `all` | the page URL |

Where data goes is the other choice pages get wrong:

| What | Where |
|---|---|
| what the group edits together: a list, a board, a game's state | `db.shared` |
| what one person posts: a message, a vote, a name, a score | `db.by("me")`, read with `db.by("*").list()` / `.watch()` |
| for one reader: a direct message, a hidden answer, a dealt card | `db.to(id)`, read with `db.inbox("*")` |
| this visitor's private state | `db.to("me")` |

Never put messages or votes in `db.shared`: any member could rewrite them.

## JS client

The server hosts it at `/kv.js`. It has two handles: `kv(space)` is one space, `visitor()` is you. It makes the device's key on first use (ECDSA P-256, non-extractable, kept in IndexedDB), registers it, and signs tokens in memory.

```html
<script type="module">
import { kv, visitor } from "https://cubby.example.com/kv.js"

const db = kv("poll-orbit-lilac-tango-slate")
const me = await db.open()                     // redeems #invite=... from the URL; me.access, me.can()
if (!me.can("read")) showAskForInvite()        // no_space: wrong name, or not invited

await db.shared.set("question", "Pizza or tacos?")       // needs all
await db.by("me").set("vote", "tacos")                   // your own key, needs post
db.by("*").watch("vote", c => console.log(c.by, c.deleted ? "removed" : c.value), { onSync: render })
await db.shared.update("count", n => (n ?? 0) + 1)       // read, change, write with ifVersion, retry
const { items, next } = await db.by("*").list("vote")    // items have key, value, version, by, mine
</script>
```

Every area (`db.shared`, `db.by(id|"me")`, `db.to(id|"me"|"owners")`) has the same methods:

| Method | Resolves |
|---|---|
| `get(key)` | the entry `{ key, value, version, by, mine, t?, expires? }`, or `null` |
| `set(key, value, { ifVersion, ttl, ifBefore })` | `{ version }` |
| `delete(key, { ifVersion, ifBefore })` | `{ version }`, or `null` if there was nothing |
| `update(key, fn, { ttl, ifBefore, retries })` | the entry after `fn(value)` was written, retrying on conflicts |
| `add(key, value, { t, ttl, ifBefore })` | `{ version, t }`: a point in the series at key |
| `series(key or pattern, { since, before, order, limit, after })` | a page, newest first: `{ items: [{ key?, t, value, expires? }], version, next }`, or `null` |
| `list(pattern, { limit, order, after })` | a page `{ items, version, next }` in key order |
| `watch(pattern, fn, { after, onSync, onError })` | a function that stops it |
| `pulse(key, value, { maxRate })` / `pulses(pattern, fn, { onError, onMissed })` | not stored, below |
| `will(key, value)` | a function that clears it, below |

- Pages: pass `next` back as `after` for the following page; `null` means that was the last. What's sorted by time comes newest first (series, `spaces()`), what's sorted by name A to Z (`list`); `order` turns either around.
- `db.by("*")`, `db.to("*")` (what you sent) and `db.inbox("*")` span several areas: they `list`, `watch` and listen to `pulses`. Their items carry `by`, `to` or `from`.
- `get` returns the same entry shape that `list` items and watch changes have, so one render function takes all three.
- Exports: `kv`, `visitor`, `KvError`. Errors are `KvError` with `code`, `status`, `details`, `retryAfter` and `retryable`. Unknown options throw `invalid_option`.
- Without IndexedDB (Node, a private window in some browsers) the key lives in memory: a new visitor per run. In Node pass `credentials` (a backup key, below) for a fixed identity.

### You

```js
const you = visitor()
const { space } = await you.createSpace({ prefix: "poll", anyone: "post" })   // you own it
const { items, next } = await you.spaces()       // every space you belong to, most recently active first
await you.id()                                   // your visitor ID, made if this device has none yet
```

- `createSpace({ prefix, name, anyone, inviters })`: `prefix` goes before four random words; `name` is a whole name of your own (16+ characters, fails if taken).
- `spaces({ limit, after })` items: `space`, `access`, `via` (`created`, `invite`, `added`), `since`, `expires`, `members`, `lastActive`, `bytes`, `keys`, `settings`. The same on every paired device. Open spaces you only wrote to aren't listed. Space names are random words, so keep a title inside the space (`shared/title`) if the page needs one.

### A page that is a room

A page whose link is the room: the space travels in the address as `#space=...`, which invite links carry too.

```js
let space = new URLSearchParams(location.hash.slice(1)).get("space")
if (!space) {
  space = (await visitor().createSpace({ prefix: "board" })).space
  history.replaceState(null, "", "#space=" + space)
}
const db = kv(space, { live: true })
const me = await db.open()                                 // takes #invite=..., keeps #space=...
const { link } = await db.invites.create({ ttl: "30d" })  // this page + #space=...&invite=...
```

To come back to the last room without a link, `visitor().spaces()` lists it first.

### Sharing

```js
const { link } = await db.invites.create()                      // all, any number of people, 7 days
const { link } = await db.invites.create({ access: "post", uses: 1, ttl: "1d", label: "Anna" })
const { link } = await db.invites.create({ access: "owner" })   // owners only: one use, at most 7 days
await db.invites.list(); await db.invites.update(id, { uses: 0 }); await db.invites.revoke(id)
await db.members.list()                                         // [{ id, access, since }], owners see more
await db.members.set(id, "read")                                // owners: read, post, all, owner or banned
await db.members.set(id, "banned", { ttl: "1d" })
await db.members.remove(id)                                     // a stranger again; lifts a ban
await db.members.remove("me")                                   // leave
await db.settings({ anyone: "read" })                           // owners
```

- An invite gives at most your own level. `accessTtl` makes the access it gives end that long after it's redeemed: a guest pass.
- The link is `base` (default: this page without its fragment) plus `#space=...&invite=inv_...`. `db.open()` redeems it and says how in `me.invite`: `"redeemed"`, `"invalid"` or `null`.
- `me` from `db.open()`: `{ space, id, access, expires, canInvite, settings, usage, limits, invite, can(op, area) }`. `me.can("write", "shared")` checks before showing a control. `id` is `null` until this device has written or joined something.
- A ban answers 403 `banned` everywhere in the space and removes grants, not data; delete their posts with `db.by(id).delete(key)`.

The server also serves a ready-made panel with invite links, QR codes, members, visibility and a backup key button:

```html
<script type="module" src="https://cubby.example.com/share.js"></script>
<cubby-share space="poll-orbit-lilac-tango-slate" members></cubby-share>
```

After `createSpace()`, call `el.showBackup()` to open it at the backup key step.

### Devices and backup keys

Pairing makes another device *you*: the same visitor, every grant, every space. It isn't sharing; don't use an invite for it.

```js
const you = visitor()

const p = await you.pair()           // { code, display: "K7M2X-9Q4PA", url: "<this page>#pair=...", done, cancel() }
showQr(p.url); showText(p.display)
await p.done                         // { device }; rejects pairing_code_expired or _cancelled

// On the other device. Never automatic: it replaces this device's identity.
if (you.pending() && confirm("Use this device as your account from the other one?")) await you.accept()

await you.devices.list()             // [{ id, label, created, lastSeen, pairedBy, current }]
await you.devices.rename("Work laptop"); await you.devices.remove(deviceId); await you.devices.removeOthers()

const text = await you.backup()      // a backup key: one more device, as JSON text. Show once, offer download
await you.restore(text)              // on a new device: become that visitor
```

Accepting replaces the device's old identity; if no other device used it, it's deleted with its data. A signed-out device's tokens stop at once, and kv.js on it comes back as a new visitor. The last device can't be signed out.

### API keys

For webhooks and tools that can't sign: a static `cubby_...` secret for one space, at most your level, never `owner`, 30 days unless `ttl` says otherwise.

```js
const { key } = await visitor().apiKeys.create(space, { label: "zapier", access: "post", ttl: "90d" })
await visitor().apiKeys.list(); await visitor().apiKeys.revoke(id)
const hook = kv(space, { apiKey: key })     // only when explicitly given
```

An API key reads and writes in its space. It can't invite, pair, manage members or create spaces. Signing out the device that made it ends it. `ttl: null` never expires, which is the weakest choice.

Never put an invite, API key or backup key in page source. Anyone who loads the page could use it.

## Live updates and pulses

```js
const db = kv(space, { live: true })
db.shared.watch("card.*", c => move(c.key, c.value), { onSync: render })
```

`live: true` sends everything over one WebSocket per space while it's open and HTTP otherwise, with the same results. Watches deliver what exists first (`after: 0`, the default), then each change, and resume from their last version after a drop. `after: "now"` skips to new changes. Several fast writes to a key can arrive as the last value. `db.live.status` says which transport is in use. Use it for boards, quizzes and games; a page that saves now and then doesn't need it.

A **pulse** is never stored. It goes to whoever listens right now. Use pulses for what moves, and keys for what to keep:

```js
addEventListener("pointermove", e => db.by("me").pulse("cursor", { x: e.clientX, y: e.clientY }))
db.by("*").pulses("cursor", p => { if (!p.mine) drawCursor(p.by, p.value) })
db.to(peerId).pulse("rtc.offer", offer)            // only peerId hears it, on db.inbox("*").pulses()

const here = db.presence("here", { value: { name } })   // here.peers, onJoin, onLeave, onChange
```

- kv.js sends at most 20 pulses a second per key and keeps the newest. `{ maxRate: 0 }` turns that off for events that must all arrive.
- The server allows 60 pulses a second per visitor, 4 KB each, apart from the write budget. It keeps 10 seconds of them so a reconnecting listener catches up; beyond that `onMissed` fires.
- `will(key, value)` is a pulse the server sends for you when your live connection ends, such as a closed tab. Presence uses it.
- Presence (`{ value, interval, timeout }`, durations, 2 s and 3 × interval by default) leaves within a round trip when a live tab closes, or after the timeout over HTTP.

## Values for one reader

```js
await db.to("owners").set("answer.3", "B")       // only owners read it, as db.inbox.owners("*")
await db.to(playerId).set("hand", ["7H", "QS"])
await db.to("me").set("draft", text)

const { items } = await db.inbox("*").list("answer.*")   // items have from and to
await db.inbox(senderId).delete("hand")
await db.to("*").list("*")                               // what you sent
```

Each sender has their own slot: two people sending `answer.3` to the owners fill two slots.

## Expiry, deadlines, limits

```js
await db.by("me").set("here", "online", { ttl: 30 })           // 5 s to 90 days; set again to renew
await db.shared.set("lock", me.id, { ttl: "1m", ifVersion: 0 }) // a lease: renew with its version
await db.clock.sync()
db.clock.at(opensAt, reveal)                                     // server time
await db.by("me").set("answer", "B", { ifBefore: closesAt })     // 412 too_late after
const { limits, usage } = await db.open()
```

Expiry produces a delete with `expired: true`. Renewing with the same value doesn't wake watchers. A set without `ttl` clears it.

Defaults: 120 writes per visitor per minute (burst 30), 50 open watches per IP, 1200 reads per IP per minute, 25 MB per visitor, 100 MB and 100,000 keys per space. On 429 wait `err.retryAfter` seconds; kv.js never retries writes by itself.

## Several writes at once

```js
await db.batch([
  { check: "shared/state", ifVersion: 40 },     // writes nothing: the batch needs state still at 40
  { set: "shared/title", value: "Trip" },
  { set: "by/me/cursor", value: { x: 3 }, ttl: 60 },
  { delete: "shared/draft", ifVersion: 41 },
  { add: "shared/temp", value: 21.4, t: 1794927600000, ttl: "14d" },
  { add: "shared/rh", value: 45, t: 1794927600000, ttl: "14d" },
])   // { version, items: [{ path, version, created?, t? }] }
```

All or nothing: up to 100 writes in one request and one transaction, each checked as if sent alone, with the options of the single call. A `check` makes the batch depend on a key it doesn't write: it fails with `version_conflict` unless the key still has that version (`0`: doesn't exist). If one fails, nothing is written and the error has `details.index`. Changed keys take consecutive versions in order, and a watcher gets them in the same answer. Paths are as in the HTTP API (`shared/k`, `by/me/k`, `to/{id}/k`). One entry per key, except points to one series that each give their own `t`. The same array works in kv.js, the CLI (`cubby batch`) and HTTP. A batch counts as one write for rate limits, which makes it the way to send a device's readings or a backlog of points.

## Series

```js
await db.shared.add("temp", 21.4)
const latest = await db.shared.series("events", { limit: 200 })                  // newest first
const more = await db.shared.series("events", { limit: 200, after: latest.next })
const chart = await db.shared.series("temp", { since: Date.now() - 864e5, order: "asc" })
const env = await db.shared.series("env.*", { since: Date.now() - 864e5 })        // items carry key
```

A key becomes a series with its first point; there's no separate create. Its value is the newest point. Points are up to 4 KB, and a series keeps the newest 1,000,000 (the operator can change that), within the space's byte quota.

```js
await db.shared.add("m1", { temp: 21.4 }, { t: reading.time, ttl: "14d" })   // kept 14 days
```

- `t` gives a reading's own time. A point at an existing `t` replaces it, so resending is safe.
- `ttl` removes the point that long after its `t`. A point without one stays. Reads show `expires`. When the newest point goes, the key shows the next newest as a new version; when the last goes, the key is gone.
- Reads come newest first; `order: "asc"` for oldest first. `since` (inclusive) and `before` (exclusive) bound a read; a page holds `limit` points (1,000 by default, up to 10,000).
- Several series at once (a reading of temperature and humidity): a batch with one `add` each, and a pattern to read them back merged by time. One pattern spans at most 100 series.
- A plain key and a series don't mix: delete one to make the other.

## Errors you'll see

| Code | Meaning | Do |
|---|---|---|
| `no_space` | no such space, or private and you're not in it | check the name; if right, the user needs an invite |
| `invite_invalid` | invite expired, used up or revoked | ask for a new link |
| `no_access` | your level is too low for this write; `details.need` says which | check `me.can()` before showing the control |
| `owner_only` | an owner action by a non-owner | |
| `not_yours` | writing another visitor's `by/{id}/` | write `db.by("me")` |
| `read_only` | a write on `by("*")`, `to("*")` or `inbox("*")` | write one area |
| `banned` | an owner banned you here; `expires` says until when | `db.open()` returns `access: "banned"` |
| `no_visitor` | `db.to(id)` with an ID nobody has | |
| `version_conflict` | `ifVersion` didn't match | read again; `update()` does this for you |
| `bad_batch` | a batch write that isn't one set, delete or add; `details.index` names it | |

kv.js handles token errors itself: a wrong clock is corrected from the server's time, and a signed-out key registers again as a new visitor.

## CLI

The same binary. Paths mirror the URL after `/v1/`; quote paths with `*`. Flags are the HTTP names in kebab case (`--if-version`, `--ttl`, `--order desc`). Reads that page (`ls`, `series`, `spaces`) follow `next` for you until `--limit`.

```sh
export CUBBY_SERVER=https://cubby.example.com   # or --server; a bare port means localhost
cubby new notes [--anyone read]                  # prints the new space name
cubby spaces                                     # the spaces you belong to: name, access, last active, members
cubby get   notes-cedar-pilot-mango-frost/shared/hello
cubby set   notes-cedar-pilot-mango-frost/shared/hello "new text"     # or the value on stdin
cubby set   poll-orbit-lilac-tango-slate/by/me/vote tacos
cubby ls    'poll-orbit-lilac-tango-slate/by/*/vote'                  # author<TAB>key<TAB>value
cubby watch notes-cedar-pilot-mango-frost/shared/hello                # what's there, then changes
cubby update homelab-maple-quilt-tundra-cobalt/shared/count --exec 'jq "(. // 0) + 1"'
cubby add   home-cedar-pilot-mango-frost/shared/temp 21.4 --ttl 14d
cubby add   home-cedar-pilot-mango-frost/shared/temp 21.1 --t 10m     # a reading from 10 minutes ago
cubby series home-cedar-pilot-mango-frost/shared/temp --since 24h       # newest first
cubby series 'home-cedar-pilot-mango-frost/shared/env.*' --limit 20 --json   # several series, NDJSON with key
cubby batch notes-cedar-pilot-mango-frost writes.json   # the same JSON array as db.batch()
cubby pulse space/shared/doorbell ding;  cubby pulses 'space/shared/*'
cubby info notes-cedar-pilot-mango-frost        # your level, settings, usage and limits, as JSON
```

`--since`, `--before` and `--t` take ms since 1970 or a duration meaning that long ago. `--ttl` takes a duration; `never` where no end is allowed.

Signing in, sharing and devices:

```sh
cubby token [--ttl 10m]                          # a JWT for curl, 1 hour by default
cubby invite new <space> [--access post] [--uses 1] [--ttl 1d] [--link URL]
cubby invite new <space> --access owner          # one use, at most 7 days
cubby join <link>                                # or: cubby join <space> <invite>
cubby settings <space> --anyone read | member ls|set|rm | invite ls|set|rm | drop <space>
cubby device pair                                # prints a code and waits
cubby device accept K7M2X-9Q4PA                  # on the other machine
cubby device ls | name <label> | rm <id> | rm --others
cubby device backup > backup.json                # keep it in a password manager
cubby login backup.json                          # use it on this machine
cubby api-key new <space> [--access read] [--ttl 30d|never] | ls | rm <id>
```

The first command that needs an identity makes this machine's key in `~/.config/cubby/credentials.json` (0600). For CI, put a backup key's JSON in `CUBBY_CREDENTIALS`, or an API key in `CUBBY_API_KEY`.

Run a command per change, resuming after a restart:

```sh
cubby watch homelab-maple-quilt-tundra-cobalt/shared/deploy --after now \
  --state /var/lib/deploy-watcher/version \
  --exec 'ref=$(cat); [ "$KV_DELETED" = 1 ] || /opt/app/deploy.sh "$ref"'
```

The command gets the value on stdin and `KV_KEY`, `KV_VERSION`, `KV_DELETED`, `KV_BY` (or `KV_FROM` and `KV_TO`, and `KV_TIME` for a series). Exit codes: 0 ok, 1 not found, 2 version conflict, 3 not allowed, 4 rate limit or quota, 5 bad input, 6 server unreachable, 64 bad usage. `cubby help <command>` lists flags.

## curl

```sh
T=$(cubby token)
curl -H "Authorization: Bearer $T" $S/v1/$SPACE/shared/hello
curl -X PUT -H "Authorization: Bearer $T" -d '"hi"' "$S/v1/$SPACE/shared/hello?ifVersion=0"
curl "$S/v1/$SPACE/shared/msg.*/watch?after=0&token=$T"
curl -H "Authorization: Bearer cubby_..." -d 21.4 $S/v1/$SPACE/shared/temp/series   # an API key
```

Without the CLI, any JWT library can sign: see [api.md](api.md#signing-in). Working examples for jose (JS), PyJWT and plain openssl are in `tools/interop/`. A minimal ESP32 (Arduino) client, with its own key in flash, is in `examples/esp32/`.
