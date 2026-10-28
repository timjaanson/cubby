# Building with cubby: a guide for coding agents

You're adding storage, sharing or live updates to a page or script for someone, using a cubby server. They may not know what a space, a token or an invite is. This guide is how to make the choices for them safely. The full contract is [spec.md](spec.md).

## The one rule

**Spaces are private unless you say otherwise.** Only the space's owner and the people they invite can read or write it. Everyone else gets 404 `no_space`, exactly as if the space didn't exist.

Leave it private unless the user has said that strangers should see or change the data. "Share with my family", "for my team", "me and my friends" all mean private plus invites. "Anyone can vote", "a public status page", "people I don't know can sign up" mean one of the public settings below.

## Pick the setting

| The user wants | `public` | Sharing |
|---|---|---|
| something for themselves, on one or more devices | `none` (leave it out) | none. They use the owner token on a second device |
| something for people they know: family, friends, a team, a club | `none` (leave it out) | an invite link per person |
| to publish something anyone may read, but only they or their scripts write | `read` | invite links or machine tokens for the writers |
| a poll, quiz, RSVP or sign-up that strangers answer | `respond` | none for answers. Invite co-editors |
| a public room anyone with the link can join and write in | `all` | share the page URL |

If you can't tell, ask the user: "Should people you haven't invited be able to see this?" If you still can't tell, leave it private. A private space that should have been public fails visibly and gets fixed. A public one that should have been private leaks without anyone noticing.

## Page skeleton

```html
<script type="module">
import { kv, createSpace } from "https://SERVER/kv.js"

// 1. Which space. Fixed in the page, or per room from the URL, or created on first use.
let space = new URLSearchParams(location.hash.slice(1)).get("list")
let ownerToken = null
if (!space) {
  ({ space, ownerToken } = await createSpace({ label: "list" }))   // private; add public: "..." only if asked
  history.replaceState(null, "", `#list=${space}`)
  // show ownerToken to the user once: "Keep this recovery key"
}

// 2. Always open first. It redeems an invite link and says whether this visitor is in.
const db = kv(space)
const me = await db.open()
if (!me.inside) {
  // Not invited, or no such space. You can't tell which, so say both:
  showMessage(me.inviteFailed
    ? "This invite has expired or was already used. Ask for a new link."
    : "Ask the owner for an invite link, or check the address.")
}

// 3. Use it.
db.shared.watch("item.*", change => { /* update state */ }, { onSync: render })
await db.shared.set("item.123", { text: "milk" })
</script>
```

## Sharing

The easiest way is the ready-made panel. It gives owners invite links, a QR code, a member list with Remove, the space's visibility and its recovery key. Members get the invite part: anyone in a space can pass it on, with at most their own rights.

```html
<script type="module" src="https://SERVER/share.js"></script>
<cubby-share space="SPACE-NAME" members></cubby-share>
```

After `createSpace`, pass the owner token so the panel asks the user to save it once: `document.querySelector("cubby-share").ownerToken = ownerToken`.

With your own UI:

```js
const { link } = await db.share({ label: "Anna" })              // full member, 10 uses, 7 days
const { link } = await db.share({ label: "Grandma", uses: 1 })  // one person
const { link } = await db.share({ allow: "viewer", uses: 1 })   // can only look
```

For a page where each room is its own space and people join by passing the link around (a board, a game, a party playlist), use `openRoom` instead of writing the setup yourself: `const room = await openRoom("board", { live: true })`, then `room.db` for data, `room.me.inside` to check, and `await room.link()` for a reusable invite link any member can show as a QR code.

The link is the current page with `#t=at_...` added. The page that opens it calls `db.open()`, which handles everything. Members can call `db.share()` too (check `me.canInvite`); their link gives what they have. To keep invites to owners, create the space with `invite: "owners"`.

Defaults are deliberately short-lived: 10 uses, 7 days. Don't make invites unlimited (`uses: null, until: null`) unless the user asked for a link that works forever. For a page meant to be open to anyone, use `public` instead of an unlimited invite.

## Don't

- **Don't put an access token (`at_...`) or owner token (`ot_...`) in page source, a repo or a config file served to browsers.** Anyone who can load the page can use it. Invite links are for sending to people, and the owner token is for the user's password manager.
- **Don't make a space public to "fix" a `no_space` error.** The error means the name is wrong or this visitor wasn't invited. Check the name, then the invite.
- **Don't call `join` without the stored visitor token.** `db.open()` and `db.join()` send it for you. Raw HTTP code must send `Authorization: Bearer vt_...` if it has one, or the person gets split into two identities.
- **Don't store secrets in a space** expecting `public: none` to make them safe like a password vault. Private means invited people only, and the server operator can read everything.

## Scripts, devices and the CLI

```sh
cubby new homelab --public read                    # prints the space name, then the owner token
cubby token new homelab-... --allow 'shared/status.*:w' --uses 1 --until 1h --label nas
# on the device or in CI, redeem once without an identity and keep the visitor token:
curl -s -X POST -d '{"token": "at_..."}' https://SERVER/v1/homelab-.../join     # → {"token": "vt_...", ...}
```

Store the returned `vt_...` as the device's or CI's secret (`CUBBY_TOKEN` for the CLI). Give each machine its own narrow token, so revoking one doesn't affect the others.

## Errors you'll see

| Code | Meaning | Do |
|---|---|---|
| `no_space` | no such space, or private and this visitor isn't in it | check the name; if it's right, the user needs an invite link |
| `invite_invalid` | the invite link expired, was used up or revoked | ask the owner for a new link |
| `no_access` | the visitor is in, but this write isn't allowed for them | the owner invites them with more rights, or changes `public` |
| `owner_only` | an owner action by a non-owner | only the owner can share or change settings |
| `not_yours` | writing another visitor's `users/{id}/` | write your own `db.user()` |
| `invalid_token` | the stored visitor is gone, for example after a year unused | kv.js gets a new one by itself; the person needs a new invite |

## Limits, presence, leases and deadlines

Ask `await db.limits()` for `{limits, usage}` rather than budgeting against defaults. The latest data-write response is available through `db.writeBudget`, with visitor, IP and bytes buckets. It is only a snapshot. Handle 429 using `err.retryAfter` in seconds. kv.js does not queue or coalesce writes, coordinate tabs, or retry failed writes automatically.

Presence belongs in `db.user().set("here", value, {ttl: 30})`, renewed every 15 to 20 seconds. Watch it through `db.users.watch("here", ...)`. `db.me` is private. Expiry sends a delete with `expired: true` and preserves the last writer as `by`. Identical-value renewals do not wake watchers; watch entries may hold an old `expires` value. A set without TTL clears expiry.

Leases use `ifVersion: 0` to acquire, the acquired version to renew the same holder value with TTL, and that version to delete. A failed renewal means stop. No automatic lock helper is provided. This cannot prevent an actor from continuing external work after a long stall. See the [lease contract](spec.md#expiring-keys-and-leases).

For pages with a lot of live interaction, such as a shared board, a quiz or a game state that changes every second or so, use `kv(SPACE, { live: true })`. Everything works the same, but data goes over one WebSocket, watches are pushed, and each change costs no request. It falls back to HTTP by itself. Don't use it for a page that reads once and writes rarely. Live mode doesn't raise write limits: still 120 writes per minute per visitor.

For "who's here", use `const here = db.users.presence("here", { value: { name } })` and render from `here.peers` in `here.onChange`. Don't build presence from stored keys or your own heartbeat map. It leaves at once on a closed tab with `live: true` and after a few seconds over HTTP.

For cursors, drags, reactions and game inputs, use pulses: `db.user().pulse("cursor", pos)` and `db.users.onPulse("cursor", p => ...)`. They're never stored, so anything a late joiner needs (the final position, the score) still goes in a stored key. kv.js sends at most 20 pulses a second per key and keeps the newest value, so pulsing on every `pointermove` is fine. For events that must all arrive (taps, reactions), pass `{ maxRate: 0 }` and stay under the server's 60 per second per visitor.

For a synchronized reveal, call `db.clock.sync()` and schedule with `db.clock.at(serverMilliseconds, callback)`. Set an opening time a little ahead to allow HTTP watches to deliver it. Enforce closing times with `{ifBefore: closesAt}` on set, delete or add. A late write returns `too_late`, status 412. Plain keys have a server-generated `t`; a series point's time may be caller-supplied. Clock sync does not guarantee precise browser execution.

The share panel preserves the recovery view regardless of whether `ownerToken` or the `open` attribute is assigned first. Setting `ownerToken` alone opens it. Dismissing the recovery dialog clears that token from the component.
