# cubby

A self-hosted key-value store in a single binary, for shared and per-visitor state in web pages, scripts and anything else that can make an HTTP request. One request to create a space, and no visible login.

Status: built as described. Where the build differs from these docs, [development.md](development.md#differences-from-the-design-docs) lists it. v0.10.0 replaced open and locked spaces without any migration, so data from before v0.10 doesn't carry over. Later upgrades keep existing data.

- This file explains the model and how a client uses it.
- [spec.md](spec.md) is the detailed contract, including the CLI, plus ideas kept for later.
- [scenarios.md](scenarios.md) has six complete examples: a page using raw HTTP, two pages using the JS client, shell scripts using the CLI and `curl`, a sensor board with a chart page, and a private family list shared through invite links.

## What it is for

Two things set it apart from hosted services like Firebase:

- **Self-hosted.** One binary on your own machine. No account, no vendor, no console, and the data stays where you put it.
- **Language-agnostic down to a shell script.** Reading is a plain `GET` on a URL. Writing is a `PUT` with one header. The same binary is also a command-line client, so a cron job can publish data with `cubby set` and another script can react to changes with `cubby watch --exec`.

Typical uses: a page made of HTML and JS, hosted anywhere, that needs to remember something, like a shared note, a poll, a chat room or a visitor's settings. Or a script that publishes a status for a page to show, or waits for a signal to act on. Or a sensor board that sends readings every minute for a page to chart. The author creates a space with one request, or the page creates one per room, and then reads and writes keys. Nothing is configured on the server and nobody clicks "sign in".

Hosted services scale further and do offline support better. This trades those for owning the whole thing and needing nothing but HTTP. A WebSocket per space is optional and only makes live pages faster.

**Spaces are private by default.** Only the creator and the people they invite can read or write, and to everyone else the space looks like it doesn't exist. Most pages are made for people who know each other: a family, friends, a team. Many are written by coding agents for people who don't know what a token is, so the safe choice is the one that needs no thought. Opening a space to anyone who has its name takes one explicit word, `public`, which the creator or the agent has to write down.

## Non-goals

- Real accounts, passwords, email. Identity is an anonymous visitor token, and rights come from invites.
- Queries, indexes, transactions over several keys.
- File or binary storage.
- Partial updates. A value is always read and written whole.
- Calculations on stored data. Series points come back exactly as stored, with no averages or downsampling.
- Storing high-frequency events. Watching delivers state; pulses carry what isn't state, like cursors at 30 per second, and are never stored or replayed beyond a few seconds.
- Anything specific to one kind of host or page.

## Quickstart

Every feature with plain `curl` against a local server, using only the path and the defaults. The sections below explain the model.

```sh
cubby serve &                                       # port 28229, no config
API=http://localhost:28229/v1

R=$(curl -s -X POST $API/spaces -d '{"label":"demo"}')   # create a private space; you're its owner
T=$(echo "$R" | jq -r .token)                        # your new visitor token, sent on every request
ID=$(echo "$R" | jq -r .id)
S=$API/$(echo "$R" | jq -r .space)                   # demo- plus four random words
A="Authorization: Bearer $T"
```

The response also has `owner`, the owner token. Keep it to get back in from another device. The space is private, so every request below sends `$A`. Without it, the space answers 404 `no_space`, as if it didn't exist.

Values:

```sh
curl -X PUT -H "$A" --data-binary 'Hello' $S/shared/greeting     # create or replace
curl -H "$A" $S/shared/greeting                                          # Hello, byte for byte
curl -X PUT -H "$A" --data-binary '{"theme":"dark"}' $S/me/settings
curl -H "$A" $S/me/settings                                      # me/: only you, in any kind of space
curl -X DELETE -H "$A" $S/shared/greeting
```

Lists and other visitors:

```sh
curl -X PUT -H "$A" --data-binary 'tacos' $S/users/$ID/vote      # only you can write under your ID
curl -H "$A" "$S/users/*/vote"                                           # every visitor's vote
curl -H "$A" "$S/shared/*"                                               # every key in shared/
curl -H "$A" "$S/shared/msg.*"                                           # keys starting with msg.
```

Watching:

```sh
curl -H "$A" "$S/shared/*/watch"                                         # waits for the next change in shared/, up to 25 s
curl -H "$A" "$S/shared/*/watch?version=0"                               # everything now, plus the version to watch from
```

Series:

```sh
curl -X POST -H "$A" --data-binary '21.4' $S/shared/temp/series                 # one point, server time
curl -X POST -H "$A" --data-binary '{"temp":21.6,"rh":45}' "$S/shared/env.*/series"   # env.temp and env.rh
curl -i -H "$A" $S/shared/temp                                           # 21.4, the newest point, with KV-Time
curl -H "$A" $S/shared/temp/series                                       # the newest 1000 points, oldest first
curl -H "$A" "$S/shared/temp/series?since=$(( $(date +%s) - 3600 ))000"  # the last hour
```

Sharing, and opening up:

```sh
curl -X POST -H "$A" $S/tokens -d '{"label":"anna"}'              # {"token":"at_…","uses":10,"until":…}: full member, 10 uses, 7 days
curl -X POST $S/join -d '{"token":"at_…"}'                        # anna, with no identity yet: {"token":"vt_…","id":…,"allow":["*:rw"]}
curl -H "$A" $S/access                                           # what you may do here
curl -X PATCH -H "$A" $S -d '{"public":"read"}'                   # anyone may read now, still only invited people write
curl $S/shared/temp                                              # works without a token now
```

### What the path alone does

| Request | With no parameters |
|---|---|
| `GET {key}` | the value as stored, with its version in `ETag` |
| `GET {prefix}*` | every matching key with its value and version, sorted by key, up to 1000, with a `next` cursor if there are more |
| `GET {key}/watch` | waits up to 25 s for the next change to that key, then returns its key, value and version, or an empty list on timeout |
| `GET {prefix}*/watch` | the same for every matching key: each one that changed, with its current value |
| `GET {key}/series` | the newest 1000 points, oldest first |
| `PUT {key}` | creates or replaces, whatever is there |
| `POST {key}/series` | adds one point at the server's time; creates the series if needed |
| `POST {prefix}*/series` | body `{"name": value, …}`; adds `value` to series `{prefix}name` for each member, all at the same time, and returns the full key names written |
| `GET {prefix}*/series` | not supported yet; read series one key at a time |
| `DELETE {key}` | deletes the key, and its points if it's a series |

Parameters only narrow or guard that default:

| Parameter | On | Does |
|---|---|---|
| `version=0` | watch | return the current state at once instead of waiting |
| `version=N` | watch | changes after N, the `version` from the last response |
| `ifVersion=N` | `PUT`, `DELETE` | only if nobody changed the key since version N; `0` means only if missing |
| `limit`, `order=desc`, `after` | list | page size, direction, and the cursor from `next` |
| `since=T`, `limit` | series read | points after time T, in milliseconds |
| `t=T` | series add | the point's time, for readings sent late |
| `timeout=S` | watch | wait 1 to 55 s instead of 25 |

## The model

```
space              "poll-cedar-pilot-mango-frost"     settings: read, write per area; one or more owners
├── shared/        the space's common data
├── users/{id}/    each visitor's public data, only that visitor writes
└── me/            each visitor's private data
```

- **Space** is a name like `poll-cedar-pilot-mango-frost`: an optional label plus four random words from a 4096-word list. The server only requires 16 to 63 characters, so nobody lands in someone else's `chat` by accident. Generated names are hard to guess, and the server slows down anyone trying names that don't exist. A space is created with `POST /v1/spaces`, and its creator owns it. It's the unit of storage, quotas, access and cleanup.
- **Visitor** is an anonymous identity, one per browser, script or device. Its visitor token (`vt_…`) goes in localStorage or a config file and is the only credential ever sent on a request.
- **Settings** say what a visitor may do without a grant: `read` is `anyone` or `granted`, and `write` is `anyone` or `granted` for each area. The default is `granted` everywhere, a private space. The `public` shorthand names the useful combinations: `none`, `read`, `respond`, `all`. See [Who may do what](#who-may-do-what).
- **Access token** (`at_…`) is an invite an owner mints. Redeeming it with `join` gives the visitor a **grant**, which is what later requests are checked against.
- **Owner** is a visitor who may do everything in the space, including settings, every invite and deleting. Members may mint invites too, within their own rights. The **owner token** (`ot_…`) is the recovery key that makes another identity an owner.
- **shared/** is the space's common data.
- **users/{id}/** is each visitor's public data: a vote, a name, a score, a message. Everyone who can read the space can read it, nobody else can change it. `users/*/vote` lists every visitor's `vote` at once.
- **me/** is the visitor's private data in this space.
- **Key** is a short name like `question` or `msg.1791449600123`. No slashes. Group with dots, because listing and watching work by prefix.
- **Value** is text up to 64 KB. If it parses as JSON it comes back as JSON, otherwise as plain text. You get back exactly the bytes you wrote.
- **Version** is a number that goes up with every write in the space. Each key remembers the version of its last write, and `by`, the visitor who made it.
- **Series** is a key that keeps every value written to it, each with a time. Its value is the newest point, and `{key}/series` has the rest. See [Series](#series).

## Who may do what

Settings decide what anyone may do. Grants add rights for specific visitors. Owners may do everything. One word picks the settings:

| `public` | Who reads | Who writes | For | Then |
|---|---|---|---|---|
| `none`, the default | owners and invited people | owners and invited people | people who know each other: a family list, a team board, a journal, a game among friends | send each person an invite link |
| `read` | anyone with the name | owners and invited people | publishing: status pages, schedules, sensor readings | give each script or device a grant |
| `respond` | anyone with the name | `shared/` invited only; everyone their own `users/` and `me/` | polls, quizzes, RSVPs, sign-up sheets | invite co-editors, if any |
| `all` | anyone with the name | anyone with the name | a public chat room, a party playlist, a demo | share the name |

The full `read` and `write` settings are there for other combinations. The owner can change settings at any time without moving data, for example lock down a public space that got spammed, or open up a private one.

Visitors always add their own part in `users/{id}/`, which nobody else can change, and keep private things in `me/`, where the space's settings allow them to write at all.

### Sharing

Sharing is the step every private space needs, so it's made short:

- **In a page:** `db.share({ label: "Anna" })` returns a link with the invite in its fragment, `page#t=at_…`. The page that opens it starts with `db.open()`, which redeems the invite and says whether the visitor is in.
- **Ready-made:** `<cubby-share space="…">` from `/share.js` is a panel for owners and members: invite one person or a group, copy or share the link, show a QR code, see and remove members, change who can see the space, and keep the recovery key.
- **From a shell:** `cubby token new <space> --label anna --link https://…/page/`.
- **For coding agents:** kv.js opens with a short guide comment, and [agents.md](agents.md) has the longer version: what to ask the user, which `public` value fits, and how to wire invites.

## Where data goes

| Situation | Put it in | Settings |
|---|---|---|
| Something everyone in the space may change: the list, the board, the document | `shared/` | any; who "everyone" is comes from `public` |
| Something only some people may change: the question, the schedule, the rules | `shared/` | `respond` or `read`, editors invited |
| Each visitor's own contribution: a vote, an answer, a message, a name, a score | `users/{id}/` | any; `respond` lets strangers add theirs |
| Each visitor's private state: settings, drafts, what they've already seen | `me/` | any |
| Many people adding to one collection at once | a key per item in `users/{id}/`, read with `users/*/` | any |
| One shared object several people edit | one key in `shared/`, written with `ifVersion` or `update()` | any |
| Readings over time from a sensor or a script | a series key per measurement, added to with `POST {key}/series` | `read` or `none`, the device holds a grant |

Mixing is normal. The poll in [scenario 2](scenarios.md#2-lunch-poll-js-client) has its question in `shared/` that only the author writes, votes in `users/` and a display setting in `me/`. The chat in [scenario 3](scenarios.md#3-chat-rooms-js-client) has the room title in an open `shared/` and messages in `users/`.

## Access

One credential, the visitor token, says who you are. What you may do follows from the space:

1. If the space's setting allows it for anyone, you may.
2. If you hold a grant whose scopes cover it, you may.
3. If you own the space, you may.

Fixed on top: writes always need a visitor token, `users/{id}/` is written only by `{id}` (owners may delete there), and `me/` is always your own.

| | Read | Write |
|---|---|---|
| `shared/` | `read` setting | `write.shared` setting |
| `users/{id}/` | `read` setting | only visitor `{id}`, if `write.users` allows them |
| `users/*/` | `read` setting | nobody, it's for reading across visitors |
| `me/` | the visitor, if `read` allows them | the visitor, if `write.me` allows them |

In a space with `public` set to `read`, `respond` or `all`, reads need no token, so a plain URL works in a browser tab, `curl` or a spreadsheet. In a private space every request carries the visitor token.

### Invites and grants

An owner, or any member, mints an access token, hands it over, usually as a link with `#t=at_…`, and the recipient redeems it once:

```
POST /v1/{space}/tokens  {"label": "anna"}      → {"token": "at_…", "uses": 10, "until": …}    owner or member
POST /v1/{space}/join    {"token": "at_…"}      → the grant, plus a new visitor token if you had none
```

- **Defaults clean up after themselves.** A token with no options is a full member (`*:rw`), redeemable by 10 visitors within 7 days. Whichever runs out first ends it. Unlimited uses or no end are possible but have to be asked for.
- **The token is the invite, the grant is the membership.** Token limits only decide who can still join. A grant lasts until revoked, or for `grantFor` after joining if the token says so.
- **Members invite too.** By default anyone in the space can pass it on, with at most the rights they hold. Their invitees stay if the inviter is later removed, and owners see who invited whom. `invite: "owners"` keeps invites to owners.
- **Grants stack.** A visitor can hold several, from several tokens or direct grants, and rights are their union. Revoking one leaves the others.
- **Scopes** are `AREA[/TARGET]:r|w|rw`, like `shared/status.*:w` for a script that writes status keys and nothing else.
- **Owners manage everything** with plain requests: list and revoke tokens, list, add and revoke grants, change settings, rotate the owner token, delete the space. A direct grant to a visitor ID they can already see, such as a voter in `users/*/`, needs no token.
- **Private spaces don't exist to strangers.** Without a readable grant, every request, including a failed `join`, gets the same 404 as a missing space, and counts towards the limit on guessing names.

Details in [SPEC sections 4 to 6](spec.md#4-spaces-and-access).

## Lifecycle

1. **Deploy once.** Run the binary. A config file for limits is optional.
2. **Create a space.** One `POST /v1/spaces`. It's private unless it says `public`. The author does it once from a shell (`cubby new poll --public respond`), or the page does it per room with `createSpace()`. The creator is the owner and gets the owner token once.
3. **Invite.** The owner sends invite links, `page#t=at_…`, from `db.share()`, the share panel or `cubby token new`. A script or device redeems one once and keeps the visitor token it gets.
4. **Page loads.** It calls `db.open()`, which redeems an invite in the URL, sending the visitor token it has if any, removes the invite from the address bar, and reports whether the visitor is in.
5. **Writes.** The page sends its visitor token. kv.js gets one the first time it's needed.
6. **Watch.** Ask for changes since a version. The server holds the request until something changes or 25 seconds pass, then the page asks again.
7. **Invites run out** after 10 uses or 7 days by default. People who joined stay until an owner revokes them, or until their grant's own end if the token had one.
8. **Visitors expire** after a year unseen, or after 30 days if they never stored anything and hold no grant. Their `users/` and `me/` data and grants go with them. Shared data stays. An owner who expired gets back in with the owner token.
9. **Space ends** when an owner deletes it, or after 180 days with no reads or writes unless the server config pins it. The long defaults suit pages used now and then, like a poll before each team lunch.

## The whole API

```
POST   /v1/anon                         → {"token": "vt_...", "id": "u_k7m2x9q4p1r8t3w6"}
GET    /v1/whoami                       → {"id": ...}                                       token
POST   /v1/spaces   {"label", "public"}    → {"space", "owner": "ot_...", "id"}   private by default; token, or a new one is returned

GET    /v1/{space}/shared/{key}         read                     token, unless the space is public
PUT    /v1/{space}/shared/{key}         write                    token
DELETE /v1/{space}/shared/{key}         remove                   token

       /v1/{space}/users/{id}/{key}     same methods, only visitor {id} writes
GET    /v1/{space}/users/*/{key}        that key from every visitor
       /v1/{space}/me/{key}             same methods, token for everything

GET    .../{prefix}*                    list: keys starting with prefix, with their values
GET    .../{key}/watch?version=N        wait for changes to a key
GET    .../{prefix}*/watch?version=N    wait for changes under a prefix

POST   .../{key}/series                 add a point to a series
POST   .../{prefix}*/series             add a point to each of several series, body {"name": value}
GET    .../{key}/series?since=T         read points, oldest first

?ifVersion=N on PUT and DELETE          only if the key is still at version N

POST   /v1/{space}/join     {"token": "at_... or ot_..."}   redeem an invite        token, or a new one is returned
GET    /v1/{space}/access                                   what you may do here

PATCH  /v1/{space}          {"public"} or {"read", "write"}   change settings       owner
DELETE /v1/{space}                                          delete the space        owner
POST   /v1/{space}/tokens   {"label", "allow", "uses", "until", "grantFor"}           owner
GET    /v1/{space}/tokens                                                           owner
PATCH  /v1/{space}/tokens/{id}   {"uses", "until"}                                  owner
DELETE /v1/{space}/tokens/{id}   revokes the token and its grants                  owner
GET    /v1/{space}/grants[?user=ID]                                                 owner
POST   /v1/{space}/grants   {"user", "allow" or "owner", "until"}                   owner
DELETE /v1/{space}/grants/{id}, /v1/{space}/grants?user=ID                         owner
POST   /v1/{space}/owner-token                              rotate                  owner
```

"token" means the visitor token in `Authorization: Bearer`, or `?token=`. It's the only credential any request carries.

Every data URL is `/v1/{space}/{area}/{key or pattern}`, optionally followed by one action, `watch`, `series` or `pulse`. There's always a key or pattern, and nothing comes after the action.

Examples shorten space names to `poll` for readability. Real names are at least 16 characters.

## Reading and writing

```
PUT /v1/poll/shared/question
Authorization: Bearer vt_...

Pizza or tacos?
```

```
200  {"version": 41}
```

```
GET /v1/poll/shared/question
```

```
200  Content-Type: text/plain
     ETag: "41"

Pizza or tacos?
```

## Not losing other people's writes

Two people read version 41 of a shared object, both change it, both write it back. Without a guard, the second write silently removes the first change. The guard is `?ifVersion=41`. If someone wrote in between, the server answers 412 with the current version, and the client reads again and retries. The JS client's `update(key, fn)` does this loop. `?ifVersion=0` means "only if it doesn't exist yet".

Often the better fix is not sharing a key at all. Votes go in each visitor's own area, and the page lists them across everyone:

```
PUT /v1/poll/users/u_k7m2x9q4p1r8t3w6/vote   "tacos"     only that visitor can write here
GET /v1/poll/users/*/vote                    → every visitor's vote, the page counts them
```

No conflicts, nobody can change anyone else's vote, and voting again replaces your own.

## Watching for changes

The client says which version it has, and the server answers with what changed after that.

```
GET /v1/poll/users/*/vote/watch?version=0
→ immediately: every current vote, "version": 57

GET /v1/poll/users/*/vote/watch?version=57
→ waits...
→ someone votes: {"changes": [{"user": "u_p1r8...", "key": "vote", "value": "pizza", "version": 58}], "version": 58}
→ or after 25 s: {"changes": [], "version": 57}
```

Then ask again with the `version` from the last response.

- `version=0` means "everything", so the first call loads the current state.
- Leaving `version` out means "from now on".
- Nothing is replayed. A key that changed five times while you weren't asking comes back once, with its current value.
- A tab that slept for an hour catches up correctly on its next request.

## Series

A series key keeps every value written to it, each with a time. It's made for a device or script that sends readings, one or many sensors at a time.

```
POST /v1/home/shared/temp/series          21.4                       one point
POST /v1/home/shared/temp/series?t=...    21.2                       a reading sent late, with its own time
POST /v1/home/shared/env.*/series         {"temp": 21.4, "rh": 45}   env.temp and env.rh, one request
POST /v1/home/shared/gps/series           {"lat": 59.43, "lon": 24.75}   one point that is an object
```

The URL decides what the body means. With a key, the body is one value, whatever its shape. With a `*`, the body's names are added to the prefix and each value goes to its own series. Leaving out the key name gets an error, and an object is never split into series by accident.

Reading:

```
GET /v1/home/shared/temp                          → 21.4, the newest point, with KV-Time
GET /v1/home/shared/temp/series                   → the newest 1000 points
GET /v1/home/shared/temp/series?since=T           → the points after T, then follow "next"
```

```json
{"key": "temp", "points": [{"t": 1791440000500, "value": 21.3}, {"t": 1791440060500, "value": 21.4}], "version": 1791449600058, "next": null}
```

- The first `POST {key}/series` makes the key a series. `PUT` on it then returns 409 `is_series`, so a stray write can't make the latest value disagree with the history.
- Points accept anything a `PUT` does: numbers, strings, objects, text. They're stored and returned as sent, up to 1 KB each.
- Points are always returned oldest first. A chart of the last day is `since=<now - 24h>`.
- Watching a series key gives the newest point, like any key. Reading `series?since=` catches up on every point.
- Each series keeps its newest 50,000 points and drops the oldest, so a device sending forever never starts failing.
- A `POST` counts as one write however many series it adds to.

## Limits, briefly

| Thing | Default |
|---|---|
| Value size | 64 KB |
| Point size in a series | 1 KB |
| Points kept per series | 50,000, oldest dropped |
| Key length | 128 characters |
| Data per visitor | 1 MB |
| Data per space | 5 MB |
| Total data | 1 GB |
| Writes per visitor | 120 per minute, burst 30 |
| New visitors per IP | 30 per hour |
| New spaces per visitor | 5 per day |
| Open watches per IP | 50 |

Hitting a limit makes the write fail with a clear error. Reads keep working and nothing stored is lost.

Limits and storage usage are queryable through plain HTTP, `db.limits()` and `cubby limits`. Rates are configurable. TTL, value timestamps, server clock sync and `ifBefore` deadlines work over the same HTTP routes. See [the spec](spec.md#expiring-keys-and-leases) for the contracts.

## CLI at a glance

The server binary is also the client. Paths mirror the URLs.

```sh
export CUBBY_SERVER=https://kv.example.com

cubby set  poll-cedar-pilot-mango-frost/shared/question "Pizza or tacos?"
cubby get  poll-cedar-pilot-mango-frost/shared/question               # prints the raw value
cubby set  poll-cedar-pilot-mango-frost/users/me/vote tacos           # users/me = your own ID
cubby ls   'poll-cedar-pilot-mango-frost/users/*/vote'                 # user, key, value per line
cubby watch status-maple-quilt-tundra-cobalt/shared/deploy --exec ./deploy.sh
cubby update status-maple-quilt-tundra-cobalt/shared/count --exec 'jq "(. // 0) + 1"'
cubby add  home-cedar-pilot-mango-frost/shared/temp 21.4                 # a point in a series
cubby add  'home-cedar-pilot-mango-frost/shared/env.*' temp=21.4 rh=45   # several series at once
cubby series home-cedar-pilot-mango-frost/shared/temp --since 24h        # t<TAB>value per point
cubby new status --write granted                        # prints the name, then the owner token
cubby token new status-maple-quilt-tundra-cobalt --allow 'shared/*:w' --label ci   # an invite: 10 uses, 7 days
cubby join poll-cedar-pilot-mango-frost 'https://example.com/poll/#t=at_…'        # redeem an invite
curl "$(cubby url poll-cedar-pilot-mango-frost/shared/question)"       # same thing over raw HTTP
```

Quote paths containing `*`, or the shell tries to expand them. The visitor token is fetched and saved automatically. Exit codes tell scripts what happened without parsing output: 1 not found, 2 version conflict, 3 not allowed, 4 limit hit. Server administration lives under `cubby admin`.

## JS client at a glance

Optional. Everything it does is plain HTTP underneath.

```js
import { kv, createSpace, takeInvite, inviteLink, newName, validSpace, KvError } from "https://kv.example.com/kv.js"

const db = kv("poll-cedar-pilot-mango-frost")
const me = await db.open()                              // redeems #t=at_… from the URL; me.inside, me.owner, me.can(...)
if (!me.inside) showAskForInvite()                      // no such space, or private and not invited

await db.shared.set("question", "Pizza or tacos?")
const q = await db.shared.get("question")               // null if missing

await db.user().set("vote", "tacos")                    // my public data
const { items } = await db.users.list("vote")           // everyone's vote

await db.me.set("showPercent", true)                    // private

await db.shared.update("settings", s => ({ ...s, open: false }))   // safe shared edit

const stop = db.shared.watch("msg.*", change => {
  console.log(change.key, change.value, change.by, change.mine)   // mine: this visitor wrote it
}, {
  onSync: ({ initial }) => render(),     // after each batch; initial: every existing key has arrived
  onError: err => showOffline(err),      // err.retryable: the watch keeps trying
})

await db.shared.add("temp", 21.4)                       // a point in a series
await db.shared.add("env.*", { temp: 21.4, rh: 45 })    // one point each in env.temp and env.rh
const { points } = await db.shared.series("temp", { since: Date.now() - 864e5 })

const { space, ownerToken } = await createSpace({ label: "room" })   // private, you own it; show ownerToken once
createSpace({ label: "poll", public: "respond" })       // only when the user wants strangers in
const { link } = await db.share({ label: "Anna" })      // owners: invite link, member, 10 uses, 7 days

newName("poll")                                         // "poll-slate-hinge-cocoa-pilot", no request
validSpace(new URLSearchParams(location.hash.slice(1)).get("s"))   // check a name from a link
```

### Keeping a page in sync

A page that mirrors a pattern, like every game in a scorecard app, needs one watch and no `list()`:

- Watch from `0`, the default. Keep a map of key to value in `onChange`, deleting on `change.deleted`.
- Render in `onSync`. The first one has `initial: true`, which means loading is done.
- Show offline on `onError` and online on the next `onSync`. A working watch syncs at least every 25 seconds, even when nothing changes.
- Queue writes made offline and send them with `update()`. On an error, keep the write if `err.retryable`, otherwise drop it and tell the user.
- Your own writes come back through the watch with `mine: true`.

The watch handles the rest. A phone that slept or lost signal asks again as soon as the page is visible or online. A tab that slept for over a week gets a fresh snapshot, with deleted keys reported as deleted.

To show a list first and follow it after, pass the list itself: `watch("game.*", fn, { from: await db.shared.list("game.*") })`.

## Decisions and rejected alternatives

| Decision | Rejected | Why |
|---|---|---|
| Go | Rust | A static binary without cgo, cross-compiled with one environment variable per target, and simple concurrency for watches. Rust would be smaller and faster, but SQLite makes cross-compiling harder. |
| The server binary is also the CLI client | A separate client, or only `curl` | One file to install anywhere. The CLI handles tokens, JSON and retries, so scripts stay short. Raw HTTP still works where the binary can't run. |
| Admin commands under `cubby admin`, through a local socket | Admin HTTP API, or admin commands mixed with client ones | Nothing to secure over the network, and no confusion between deleting a key and deleting a space. |
| Keep it usable without a client library | Features that need client-side hashing, URL fragment rules or remembered keys | The pages using this are short-lived. If raw HTTP takes 150 lines, people won't use it without the library, and the library becomes the real API. |
| "Space" as the one container | A project holding spaces | One concept is enough. A space works for an app, a poll, or a single chat room. |
| `shared/`, `users/{id}/`, `me/` | Only shared + private, or ownership flags on keys | "Everyone reads, only I write" covers votes, names, scores and messages without conflicts or tampering. Private data is still needed for settings and drafts. |
| Every space is created explicitly and has an owner | Spaces created by their first write | Every space has someone who can moderate, change settings and invite. Typos don't create spaces, and there's no implicit-creation rule to reason about. It costs one request, which `createSpace()` and `cubby new` make. |
| Settings per space, `read` plus `write` per area, changeable by the owner | Open and locked kinds fixed at creation, a separate closed-server mode | One model covers group editing, author plus responses, publishing and private spaces. A spammed open space can be locked down without moving data. A private space replaces a closed server. |
| One credential per request, the visitor token; access tokens are redeemed into grants | Sending a visitor token and an access token side by side | "Do I need one header or two" was the confusing part. Redeeming keeps identity and rights separate on the server while clients send one thing. Grants can be listed, revoked and timed per person. |
| Access tokens default to 10 uses and 7 days; unlimited must be explicit | No limits by default | Invite links get forgotten in chats and mail. The default makes the least-effort token expire on its own, and still covers inviting a family or a team. |
| Token limits apply to redeeming, grants last until revoked unless `grantFor` | Grants ending with their token | A week-long invite shouldn't throw everyone out after a week. Joining and staying are separate questions. |
| Grants stack as a union, no deny rules, never edited | Editable grants, deny scopes, one grant per visitor | Nothing cancels anything, order never matters, and every grant matches the token it came from. Changing rights is revoke plus add. |
| Owners can grant directly to a visitor ID | Tokens only | Promoting someone visible in `users/*/` needs no link round trip. |
| The owner token is a recovery key, redeemed like an invite | The owner token sent on admin requests | Owners act with their visitor identity, so there's still only one credential. The owner token gets a new device or a cleared browser back in. |
| A private space answers exactly like a missing one | 403 for spaces without access | A different answer would tell someone trying names which ones exist. |
| No `add` right in scopes | Create-only writes | `users/{id}/` already gives "add your own, nobody else can touch it". The real gap, "only the owner reads", is the `inbox` area in Later. |
| Private by default, `public` to open up | Public by default, or public reads with invite-only writes | A private default fails visibly: "my friend sees nothing" gets fixed in minutes. A public default fails silently: a name written into a page's source exposes everything, and nobody notices. Public-read-only would need the whole invite flow anyway, for half the protection. |
| `public: none, read, respond, all` | Only the per-area `read`/`write` object | Opening a space should be one readable word in the code an agent writes, so a reviewer sees the decision. The object stays for other combinations. |
| `no_space` says "no such space, or private and not joined" | Separate messages, or a vague one | The answer must be identical either way, so the message names both causes. Agents and people then check the invite, not just the spelling. |
| `db.open()` as the one way a page starts | Each page wiring `takeInvite`, `join` and `access` itself | The invite step is easy to get subtly wrong: forgetting the stored token splits a person into two visitors. One call does it right and returns `inside`. |
| Share UI as a separate module, `/share.js` | Built into kv.js, or left to every page | kv.js stays DOM-free for Node, workers and scripts. Pages that share get invite links, QR codes and member management without writing them, and the safe choices are the easy buttons. |
| Public reads need no token | Token on every request | Anyone can get a token, so requiring one for public data protects nothing. A plain URL keeps working in a browser tab or spreadsheet. |
| Every key records `by`, the visitor who last wrote or deleted it | Writer hidden, or only on `users/` | Pages want "P2 moved to hole 4" and to skip their own writes coming back. IDs are already public under `users/*/`. |
| The JS client sends the token as a header only on writes, `me/` and owner calls, and as `?token=` on reads | A header on every request once there is one | Any custom header makes the browser preflight, preflights are cached per URL, and each watch request has a new URL. |
| Watch reports `onSync` after each answer, beside `onChange` per key | A status enum, batched change arrays, or `list()` before every watch | One callback says "loaded", "working again" and "render now". `onChange` stays one key at a time. |
| One visitor ID across all spaces, returned by `/anon` | A different ID per space | Simpler for raw clients, no extra request. Linking a visitor across spaces reveals little when nothing stored is secret. |
| Generated names are four random words; any name of 16+ characters is accepted | A random alphanumeric suffix, any name, or names derived from the page URL | Words are easy to read out and type, and 4 of 4096 give 48 bits. Short names collide by accident. URL-derived names tie the store to how pages are hosted. A per-address limit on distinct missing spaces makes guessing slow. |
| Token in localStorage, sent as a header | Cookie | A cookie from another domain counts as third-party. Safari and Brave block it, Firefox partitions it. |
| Scopes as `area/target:rights`, within one space | Roles | Uses the same prefix patterns as the API, and an owner sees all of a grant's access in one line. |
| Members may invite by default, within their own rights | Only owners invite; or members invite with any rights | Passing a link on is what people do anyway. Without it, pages kept an owner's unlimited token on every device so anyone could show a QR code. Capping at the minter's rights makes it delegation, not escalation. |
| Invitees stay when their inviter is removed | Revoking a member's whole invite tree | Grants already outlive their tokens, and nobody expects a friend-of-a-friend to vanish. The removed member's unused invites end, since they no longer hold what those hand out. |
| Opaque random tokens, server state | JWT | Tokens are redeemed once and turned into a grant row, so stateless verification buys nothing. JWT would add 250 to 400 bytes per link or QR code, readable contents, frozen claims and known pitfalls. Revoking is deleting a row. Signed owner keys are in Later if a server should never hold owner power. |
| Value is text, JSON detected by parsing | Arbitrary bytes with client content type | Arbitrary content types make the server free hosting for phishing pages. `fetch` sends `text/plain` for JSON strings anyway. |
| Raw value as the body | `{ value, version }` wrapper | People would PUT the wrapper back and end up with nested wrappers. |
| Whole-value writes only | JSON field paths, `PATCH`, increment | Too much server logic. Separate keys and `ifVersion` cover the real needs. Series add whole points, they don't edit values. |
| Long polling with a version cursor | SSE | `EventSource` can't send headers, proxies buffer SSE, and SSE needs its own replay logic. |
| One optional WebSocket per space, carrying HTTP requests in an envelope | SSE beside HTTP, or a separate realtime protocol | Pages with tight live use need pushes without a request each and a warm connection both ways, which matters most on phones. Running each message through the same router keeps rules, limits and errors identical to HTTP. With a socket available, SSE would be a third way that adds nothing. |
| Socket auth in the first message, or a header | Token in the socket URL, a ticket route, a token in `Sec-WebSocket-Protocol` | URLs end up in proxy logs. Browsers can't set headers on a WebSocket. A ticket costs a request and server state for what a message does. `auth` can also switch visitor mid-connection, so a reader who starts writing keeps the socket. |
| Pulses: unstored values on the same keys and rights | A separate pub/sub channel namespace, or storing events with a short TTL | The access model already says who may write `users/{id}/cursor`, so a cursor can't be faked with no new rules. Storing would cost a disk write and a version per mouse move, and a watch would merge the moves away. |
| A 10-second in-memory window per space with a cursor | Pure fan-out to connected sockets | HTTP listeners are between requests most of the time, and sockets reconnect. A short window with a cursor lets both catch up, and a listener that falls behind is told instead of slowing the sender. |
| Access checked per socket message | A check at connect | A page may connect before it joins, and a grant can start or end while connected. Per message, a socket answers exactly like the HTTP request would. |
| Watch returns current state since a version | Replaying history, or only changes since connecting | "Since connecting" loses writes between long-poll requests. A version cursor doesn't, and needs no history. |
| `/watch` path suffix | `?watch` query flag | Keys are one path segment, so a trailing `/watch` is unambiguous. |
| Every URL names a key or pattern, then at most one action | `/shared/watch` for a whole area, actions after actions | One shape to learn. A whole area is `*`, which already means "several keys". |
| Action words aren't valid keys | Telling them apart by position only | The parser could, but `/shared/series/series` is hard to read. The list stays at two or three words. |
| Series as a separate key kind, set by its first `POST` | Key per point, one JSON array value, a `series/` area, series declared in config | A key per point hits the key limit and never rolls over. An array means read-modify-write. Areas are about who may write. Config goes against nothing being set up per use. |
| `POST {key}/series` | Plain `POST {key}` to append | Someone reading a `curl` line sees what's being added to, without knowing a REST convention. |
| The word "series" | "history", "log", "points" | It names the thing, so the URL, the errors and the docs use one word. "history" describes reading it, "log" suggests debug output. |
| Batch through a pattern, `env.*/series` | An object posted to the area | `*` already means several keys. An object posted to a key is always one point, so a forgotten key can't split it. |
| Points take any value, stored as sent | Numbers only | The server never calculates, so it doesn't need to know types. Numbers lose nothing, and door states and GPS fit too. |
| Keep the newest N points per series | Quota error, time-based expiry | A device sending forever must not start failing. A count gives a hard bound per series. |
| Series reads oldest first, `since` and `limit` | List's `after` and `order` | The path alone gives the newest points ready to chart. For times, "after" with `order=desc` would mean "older than", which reads wrong. |
| Watching a series returns the newest point | `/series/watch` with every point | Watching stays "current state". Every point is one `since` read away. |
| One SQLite file per space | One shared database | Deleting a space frees disk at once, damage stays contained, backups are a file copy. |
| No slashes in keys | Percent-encoded slashes | Proxies and routers handle `%2F` inconsistently. |
| No migration or compatibility code for the access model | Converting locked spaces and keeping old routes | Nothing stored on the live server is worth it. One clean code path is easier to get right than two. |
| TTL is a PUT option; leases combine it with ifVersion | A separate lock service | Expiry is visible as a marked deletion. Ordinary conditional writes handle acquisition and renewal. Automatic lease helpers, proof of work and device linking remain deferred. |
