# Building with cubby: a guide for coding agents

You're adding storage, sharing or live updates to a page or script for someone, using a cubby server. They may not know what a space, a token or an invite is. This guide is how to make the choices for them safely. The full contract is [spec.md](spec.md).

## The one rule

**Spaces are private unless you say otherwise.** Only the space's owner and the people they invite can read or write it. Everyone else gets 404 `no_space`, exactly as if the space didn't exist.

Leave it private unless the user has said that strangers should see or change the data. "Share with my family", "for my team", "me and my friends" all mean private plus invites. "Anyone can vote", "a public status page", "people I don't know can sign up" mean giving `anyone` a level, below.

## Levels

Everyone in a space has one level, and every choice below is a level:

| Level | May |
|---|---|
| `read` | read the space, and their own inbox |
| `post` | also write their own keys (`db.mine`) and send (`db.to()`) |
| `all` | also write common keys (`db.shared`) |
| `owner` | also moderate, change settings, manage members |

`anyone` on the space is the level strangers get (`none` by default). `access` on an invite is the level the person who opens it gets (`all` by default). `me.access` from `db.open()` is your own.

## Pick the setting

| The user wants | `anyone` | Sharing |
|---|---|---|
| something for themselves, on one or more devices | `none` (leave it out) | none. They link the second device, see below |
| something for people they know: family, friends, a team, a club | `none` (leave it out) | an invite link per person |
| a group chat or comment thread among people they know | `none` (leave it out) | invites with `access: "post"` if members shouldn't edit shared things |
| to publish something anyone may read, but only they or their scripts write | `read` | invite links, or a `post` invite per machine |
| a poll, quiz, RSVP or sign-up that strangers answer | `post` | none for answers. Invite co-editors |
| a public room anyone with the link can join and write in | `all` | share the page URL |

If you can't tell, ask the user: "Should people you haven't invited be able to see this?" If you still can't tell, leave it private. A private space that should have been public fails visibly and gets fixed. A public one that should have been private leaks without anyone noticing.

## Where data goes

This is the choice pages get wrong most often.

| What | Where | Why |
|---|---|---|
| something the group edits together: a list, a board, a document, a room title, a game's state | `db.shared` | anyone at `all` may change it |
| something one person posts: a message, a comment, a vote, an answer anyone may see, a display name, a score | `db.mine`, read with `db.by.list(...)` and `db.by.watch(...)` | only its author can change or delete it; owners can delete it as moderation |
| something for one reader: a direct message, a hidden answer, a dealt card | `db.to(id)`, read with `db.inbox` | nobody else can read it |
| private state of this visitor | `db.to("me")` | private to them, on all their devices |

A chat is `db.mine.set("msg." + Date.now() + "." + rand, { text })` and `db.by.watch("msg.*", ...)`, with names in `db.mine.set("name", ...)`. Items carry `by`, the author, so "mine" is `item.mine` and a name is a lookup by `item.by`. Never put messages in `db.shared`: any member could then rewrite or delete anyone's message.

## Page skeleton

```html
<script type="module">
import { kv, createSpace } from "https://SERVER/kv.js"

// 1. Which space. Fixed in the page, or per room from the URL, or created on first use.
let space = new URLSearchParams(location.hash.slice(1)).get("list")
let ownerToken = null
if (!space) {
  ({ space, ownerToken } = await createSpace({ label: "list" }))   // private; add anyone: "..." only if asked
  history.replaceState(null, "", `#list=${space}`)
  // show ownerToken to the user once: "Keep this recovery key"
}

// 2. Always open first. It redeems an invite link and says this visitor's level.
const db = kv(space)
const me = await db.open()
if (!me.can("read")) {
  // Not invited, or no such space. You can't tell which, so say both:
  showMessage(me.inviteFailed
    ? "This invite has expired or was already used. Ask for a new link."
    : "Ask the owner for an invite link, or check the address.")
}

// 3. Use it. Show edit controls only when me.can("write", "shared").
db.shared.watch("item.*", change => { /* update state */ }, { onSync: render })
await db.shared.set("item.123", { text: "milk" })
</script>
```

## Sharing

The easiest way is the ready-made panel. It gives owners invite links, a QR code, a member list with levels and Remove, the space's visibility and its recovery key. Members at `post` or above get the invite part, giving at most their own level.

```html
<script type="module" src="https://SERVER/share.js"></script>
<cubby-share space="SPACE-NAME" members></cubby-share>
```

After `createSpace`, pass the owner token so the panel asks the user to save it once: `document.querySelector("cubby-share").ownerToken = ownerToken`.

With your own UI:

```js
const { link } = await db.share({ label: "Anna" })                 // access all, 10 uses, 7 days
const { link } = await db.share({ label: "Grandma", uses: 1 })     // one person
const { link } = await db.share({ access: "post" })                // posts their own, can't change shared
const { link } = await db.share({ access: "read", uses: 1 })       // can only look
```

For a page where each room is its own space and people join by passing the link around (a board, a game, a party playlist), use `openRoom` instead of writing the setup yourself: `const room = await openRoom("board", { live: true })`, then `room.db` for data, `room.me.can("read")` to check, and `await room.link()` for a reusable invite link any member can show as a QR code.

The link is the current page with `#t=at_...` added. The page that opens it calls `db.open()`, which handles everything. Members at `post` or above can call `db.share()` too (check `me.canInvite`); their link gives at most their own level. To keep invites to owners, create the space with `invite: "owner"`.

Owners list people with `db.members()` and change someone's level with `db.owner.members.set(id, "read")`, or remove them with `db.owner.members.remove(id)`. Anyone leaves with `db.leave()`.

Defaults are deliberately short-lived: 10 uses, 7 days. Don't make invites unlimited (`uses: null, until: null`) unless the user asked for a link that works forever. For a page meant to be open to anyone, use `anyone` instead of an unlimited invite.

## Don't

- **Don't put an invite (`at_...`) or owner token (`ot_...`) in page source, a repo or a config file served to browsers.** Anyone who can load the page can use it. Invite links are for sending to people, and the owner token is for the user's password manager.
- **Don't give `anyone` a level to "fix" a `no_space` error.** The error means the name is wrong or this visitor wasn't invited. Check the name, then the invite.
- **Don't call `join` without the stored visitor token.** `db.open()` and `db.join()` send it for you. Raw HTTP code must send `Authorization: Bearer vt_...` if it has one, or the person gets split into two identities.
- **Don't put posts in `db.shared`.** Messages, comments and votes go in `db.mine`. In `db.shared`, every member can overwrite or delete them.
- **Don't store secrets in a space** expecting `anyone: none` to make them safe like a password vault. Private means invited people only, and the server operator can read everything.

## Scripts, devices and the CLI

```sh
cubby new homelab --anyone read                    # prints the space name, then the owner token
cubby token new homelab-... --access post --uses 1 --until 1h --label nas
# on the device or in CI, redeem once without an identity and keep the visitor token:
curl -s -X POST -d '{"token": "at_..."}' https://SERVER/v1/homelab-.../join     # → {"token": "vt_...", ...}
```

Store the returned `vt_...` as the device's or CI's secret (`CUBBY_TOKEN` for the CLI). A machine at `post` writes its own keys, such as `homelab-.../by/me/status`, and readers find them with `by/*/status` without knowing its ID. Give each machine its own invite, so removing one doesn't affect the others.

## The same person on another device

That's linking, not sharing. Don't make an invite for it: an invite makes a second visitor with a grant in one space, so `db.to("me")`, `db.mine` and every other space stay behind.

```js
import { devices } from "https://SERVER/kv.js"
const me = devices()
// on the device that's already them: a "Use on another device" button
const link = await me.link()             // show link.url as a QR code and link.display (K7M2X-9Q4PA) as text
await link.done                          // then say "linked"
// at the start of every page: never accept without asking
if (me.pending() && confirm("Use this device as your account from the other one? Anything stored here so far goes away.")) await me.accept()
```

A text field for the typed code calls `me.accept(code)`. Offer a device list with `me.list()` and "sign out" buttons with `me.remove(id)` where it fits.

## Hidden answers, private messages, moderation

- **Something only one person should see** (a quiz answer to the host, a dealt card, a role, a direct message) goes under `db.to(recipient)`, never `db.mine`, which everyone can read. The recipient reads `db.inbox`. `db.to("owners")` reaches the owners, who read `db.inbox.owners`.
- **A visitor's private state** is `db.to("me")`.
- **Moderation** is `db.owner.members.set(id, "banned")`, then deleting their posts with `db.by(id).delete(key)`. A ban removes their grants but keeps their data.

## Errors you'll see

| Code | Meaning | Do |
|---|---|---|
| `no_space` | no such space, or private and this visitor isn't in it | check the name; if it's right, the user needs an invite link |
| `invite_invalid` | the invite link expired, was used up or revoked | ask the owner for a new link |
| `no_access` | the visitor is in, but their level is too low for this write; `need` says which level it takes | check `me.can()` before showing the control; the owner can raise their level |
| `owner_only` | an owner action by a non-owner | only the owner can share or change settings |
| `not_yours` | writing another visitor's `by/{id}/` | write your own `db.mine` |
| `invalid_token` | the stored visitor is gone, for example after a year unused | kv.js gets a new one by itself; the person needs a new invite |
| `banned` | an owner banned this visitor from the space; `until` says how long | show it; `db.open()` returns `access: "banned"` and `until` instead of throwing |
| `no_visitor` | `db.to(id)` with an ID nobody has | check where the ID came from |

## Limits, presence, leases and deadlines

Ask `await db.limits()` for `{limits, usage}` rather than budgeting against defaults. The latest data-write response is available through `db.writeBudget`, with visitor, IP and bytes buckets. It is only a snapshot. Handle 429 using `err.retryAfter` in seconds. kv.js does not queue or coalesce writes, coordinate tabs, or retry failed writes automatically.

Presence stored as a key belongs in `db.mine.set("here", value, {ttl: 30})`, renewed every 15 to 20 seconds. Watch it through `db.by.watch("here", ...)`. `db.to("me")` is private. Expiry sends a delete with `expired: true` and preserves the last writer as `by`. Identical-value renewals do not wake watchers; watch entries may hold an old `expires` value. A set without TTL clears expiry.

Leases use `ifVersion: 0` to acquire, the acquired version to renew the same holder value with TTL, and that version to delete. A failed renewal means stop. No automatic lock helper is provided. This cannot prevent an actor from continuing external work after a long stall. See the [lease contract](spec.md#expiring-keys-and-leases).

For pages with a lot of live interaction, such as a shared board, a quiz or a game state that changes every second or so, use `kv(SPACE, { live: true })`. Everything works the same, but data goes over one WebSocket, watches are pushed, and each change costs no request. It falls back to HTTP by itself. Don't use it for a page that reads once and writes rarely. Live mode doesn't raise write limits: still 120 writes per minute per visitor.

For "who's here", use `const here = db.presence("here", { value: { name } })` and render from `here.peers` in `here.onChange`. Don't build presence from stored keys or your own heartbeat map. It leaves at once on a closed tab with `live: true` and after a few seconds over HTTP.

For cursors, drags, reactions and game inputs, use pulses: `db.mine.pulse("cursor", pos)` and `db.by.onPulse("cursor", p => ...)`, where `p.by` is the sender. They're never stored, so anything a late joiner needs (the final position, the score) still goes in a stored key. kv.js sends at most 20 pulses a second per key and keeps the newest value, so pulsing on every `pointermove` is fine. For events that must all arrive (taps, reactions), pass `{ maxRate: 0 }` and stay under the server's 60 per second per visitor.

For a synchronized reveal, call `db.clock.sync()` and schedule with `db.clock.at(serverMilliseconds, callback)`. Set an opening time a little ahead to allow HTTP watches to deliver it. Enforce closing times with `{ifBefore: closesAt}` on set, delete or add. A late write returns `too_late`, status 412. Plain keys have a server-generated `t`; a series point's time may be caller-supplied. Clock sync does not guarantee precise browser execution.

The share panel preserves the recovery view regardless of whether `ownerToken` or the `open` attribute is assigned first. Setting `ownerToken` alone opens it. Dismissing the recovery dialog clears that token from the component.
