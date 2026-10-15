# cubby

A self-hosted key-value store in a single binary, for shared and per-visitor state in web pages, scripts and anything else that can make an HTTP request. No setup per use and no visible login.

Status: v0.5.0 is built, series included. Differences between the build and these docs are listed at the end of the [README](../README.md#differences-from-the-design-docs).

- This file explains the model and how a client uses it.
- [spec.md](spec.md) is the detailed contract, including the CLI, plus ideas kept for later.
- [scenarios.md](scenarios.md) has five complete examples: a page using raw HTTP, two pages using the JS client, shell scripts using the CLI and `curl`, and a sensor board with a chart page.

## What it is for

Two things set it apart from hosted services like Firebase:

- **Self-hosted.** One binary on your own machine. No account, no vendor, no console, and the data stays where you put it.
- **Language-agnostic down to a shell script.** Reading is a plain `GET` on a URL. Writing is a `PUT` with one header. The same binary is also a command-line client, so a cron job can publish data with `cubby set` and another script can react to changes with `cubby watch --exec`.

Typical uses: a page made of HTML and JS, hosted anywhere, that needs to remember something, like a shared note, a poll, a chat room or a visitor's settings. Or a script that publishes a status for a page to show, or waits for a signal to act on. Or a sensor board that sends readings every minute for a page to chart. The page or script picks a space name and reads and writes keys. Nothing is configured on the server and nobody clicks "sign in".

Hosted services do realtime, offline support and scale better. This trades those for owning the whole thing and needing nothing but HTTP.

Data is not secret. Anyone who knows a space's name can read its shared data and every visitor's public data. Only `me/` is private.

## Non-goals

- Real accounts, passwords, email.
- Queries, indexes, transactions over several keys.
- File or binary storage.
- Partial updates. A value is always read and written whole.
- Calculations on stored data. Series points come back exactly as stored, with no averages or downsampling.
- High-frequency realtime. Watching is long polling, made for changes every few seconds.
- Anything specific to one kind of host or page.

## Quickstart

Every feature with plain `curl` against a local server, using only the path and the defaults. The sections below explain the model.

```sh
cubby serve &                                       # port 8080, no config

S=http://localhost:8080/v1/demo-k7m2x9q4              # a space: any label plus 8 random characters
T=$(curl -s -X POST localhost:8080/v1/anon | jq -r .token)
A="Authorization: Bearer $T"                          # needed for writes and for me/, never for other reads
ID=$(curl -s -H "$A" localhost:8080/v1/whoami | jq -r .id)
```

Values:

```sh
curl -X PUT -H "$A" --data-binary 'Hello' $S/shared/greeting     # create or replace; the first write creates the space
curl $S/shared/greeting                                          # Hello, byte for byte
curl -X PUT -H "$A" --data-binary '{"theme":"dark"}' $S/me/settings
curl -H "$A" $S/me/settings                                      # private: the token is needed to read too
curl -X DELETE -H "$A" $S/shared/greeting
```

Lists and other visitors:

```sh
curl -X PUT -H "$A" --data-binary 'tacos' $S/users/$ID/vote      # only you can write under your ID
curl "$S/users/*/vote"                                           # every visitor's vote
curl "$S/shared/*"                                               # every key in shared/
curl "$S/shared/msg.*"                                           # keys starting with msg.
```

Watching:

```sh
curl "$S/shared/*/watch"                                         # waits for the next change in shared/, up to 25 s
curl "$S/shared/*/watch?version=0"                               # everything now, plus the version to watch from
```

Series:

```sh
curl -X POST -H "$A" --data-binary '21.4' $S/shared/temp/series                 # one point, server time
curl -X POST -H "$A" --data-binary '{"temp":21.6,"rh":45}' "$S/shared/env.*/series"   # env.temp and env.rh
curl -i $S/shared/temp                                           # 21.4, the newest point, with KV-Time
curl $S/shared/temp/series                                       # the newest 1000 points, oldest first
curl "$S/shared/temp/series?since=$(( $(date +%s) - 3600 ))000"  # the last hour
```

A locked space, where only the key holder writes `shared/`:

```sh
curl -X POST -H "$A" "localhost:8080/v1/spaces?label=home&locked"   # {"space":"home-…","key":"sk_…"}
# then add -H "Space-Key: sk_…" to writes in that space's shared/
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
space              "poll-k7m2x9q4"
├── shared/        everyone reads; who writes depends on the kind of space
├── users/{id}/    everyone reads, only that visitor writes
└── me/            only the visitor reads and writes
```

- **Space** is a name like `poll-k7m2x9q4`: an optional label plus 8 random-looking characters. The server rejects names without such a suffix, so nobody lands in someone else's `chat` by accident. A space is created by its first write. It's the unit of storage, quotas and cleanup.
- **Visitor** is an anonymous identity, one per browser. `POST /v1/anon` returns a token and the visitor's ID. The token goes in localStorage and is sent on writes.
- **shared/** is the space's common data. In an open space any visitor writes it, in a locked space only whoever holds the space key. See [Two kinds of spaces](#two-kinds-of-spaces).
- **users/{id}/** is each visitor's public data: a vote, a name, a score, a message. Anyone can read it, nobody else can change it. `users/*/vote` lists every visitor's `vote` at once.
- **me/** is the visitor's private data in this space.
- **Key** is a short name like `question` or `msg.1791449600123`. No slashes. Group with dots, because listing and watching work by prefix.
- **Value** is text up to 64 KB. If it parses as JSON it comes back as JSON, otherwise as plain text. You get back exactly the bytes you wrote.
- **Version** is a number that goes up with every write in the space. Each key remembers the version of its last write.
- **Series** is a key that keeps every value written to it, each with a time. Its value is the newest point, and `{key}/series` has the rest. See [Series](#series).

## Two kinds of spaces

The only difference is who may write `shared/`. Everything else, `users/` and `me/` included, works the same in both.

| | Open space | Locked space |
|---|---|---|
| For | a group working on the same things | one author publishing to many readers |
| Who writes `shared/` | every visitor | whoever holds the space key |
| Created | by the first write, no setup | once, with `POST /v1/spaces?locked`, which returns the key |
| Access works like | "anyone with the link can edit" | "anyone can view, I edit" |
| Typical pages | shopping or packing list, kanban board, meeting notes, a chat room, a turn-based game, a sign-up sheet | poll question, status page, schedule, announcements, quiz questions, data a script publishes |

Neither is the main mode. Pick by asking whether visitors should change the common data, or only add their own part to it.

In both kinds, visitors add their own part in `users/{id}/`, which nobody else can change, and keep private things in `me/`.

## Where data goes

| Situation | Put it in | Kind of space |
|---|---|---|
| Something everyone may change: the list, the board, the document | `shared/` | open |
| Something only the author may change: the question, the schedule, the rules | `shared/` | locked |
| Each visitor's own contribution: a vote, an answer, a message, a name, a score | `users/{id}/` | either |
| Each visitor's private state: settings, drafts, what they've already seen | `me/` | either |
| Many people adding to one collection at once | a key per item in `users/{id}/`, read with `users/*/` | either |
| One shared object several people edit | one key in `shared/`, written with `ifVersion` or `update()` | open |
| Readings over time from a sensor or a script | a series key per measurement, added to with `POST {key}/series` | locked |

Mixing is normal. The poll in [scenario 2](scenarios.md#2-lunch-poll-js-client) has its question in a locked `shared/`, votes in `users/` and a display setting in `me/`. The chat in [scenario 3](scenarios.md#3-chat-rooms-js-client) has the room title in an open `shared/` and messages in `users/`.

## Access

| | Read | Write |
|---|---|---|
| `shared/` | anyone, no token | any visitor; in a locked space also `Space-Key` |
| `users/{id}/` | anyone, no token | only visitor `{id}` |
| `users/*/` | anyone, no token | nobody, it's for reading across visitors |
| `me/` | the visitor, with token | the visitor, with token |

Reads need no token because a token would protect nothing. Anyone can get one. Without it, a plain URL works in a browser tab, `curl`, or a spreadsheet.

### Closed servers

For data that shouldn't be public, the operator sets `access = "closed"`. Then `/v1/anon` is off and every request needs a token, reads included. The operator issues tokens on the server machine, one per device or person, each limited to some spaces, areas and keys, read or write, and optionally to a date:

```
cubby admin token new dashboard --allow 'home-k7m2x9q4:r'
cubby admin token new sensor --allow 'home-k7m2x9q4/shared/env.*:w' --until 90d
cubby admin token revoke sensor
```

An issued token looks and works like any other, so clients don't change. Details in [SPEC section 6](spec.md#6-closed-servers-and-issued-tokens).

## Lifecycle

1. **Deploy once.** Run the binary. A config file for limits is optional.
2. **Pick the kind of space.** For an open space, generate a name locally (`kv.newName("list")`) and put it in the page. It's created by the first write. For a locked space, call `POST /v1/spaces?label=poll&locked` once, put the name in the page and keep the key yourself.
3. **Page loads and reads.** Plain `GET`s, no token needed.
4. **First write.** The page calls `POST /v1/anon`, stores the token and ID, and sends `Authorization: Bearer <token>` on writes and on anything in `me/`.
5. **Watch.** Ask for changes since a version. The server holds the request until something changes or 25 seconds pass, then the page asks again.
6. **Token expires** after a year without use, or after 30 days if the visitor never stored anything. That visitor's `users/` and `me/` data go with it. Shared data stays.
7. **Space expires** after 180 days with no reads or writes, unless the server config pins it. The long defaults suit pages used now and then, like a poll before each team lunch.

## The whole API

```
POST   /v1/anon                         → {"token": "kv_...", "id": "u_k7m2x9q4p1r8t3w6"}
GET    /v1/whoami                       → {"id": ...}                                 token
POST   /v1/spaces?label=poll[&locked]   → {"space": "poll-k7m2x9q4", "key": "sk_..."}  token, optional

GET    /v1/{space}/shared/{key}         read
PUT    /v1/{space}/shared/{key}         write                   token (+ Space-Key if locked)
DELETE /v1/{space}/shared/{key}         remove                  token (+ Space-Key if locked)

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
```

Every data URL is `/v1/{space}/{area}/{key or pattern}`, optionally followed by one action, `watch` or `series`. There's always a key or pattern, and nothing comes after the action.

Examples shorten space names to `poll` for readability. Real names need the suffix.

## Reading and writing

```
PUT /v1/poll/shared/question
Authorization: Bearer kv_...

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
| Writes per visitor | 60 per minute |
| New tokens per IP | 10 per hour |
| New spaces per visitor | 5 per day |
| Open watches per IP | 20 |

Hitting a limit makes the write fail with a clear error. Reads keep working and nothing stored is lost.

## CLI at a glance

The server binary is also the client. Paths mirror the URLs.

```sh
export CUBBY_SERVER=https://kv.example.com

cubby set  poll-k7m2x9q4/shared/question "Pizza or tacos?"
cubby get  poll-k7m2x9q4/shared/question               # prints the raw value
cubby set  poll-k7m2x9q4/users/me/vote tacos           # users/me = your own ID
cubby ls   'poll-k7m2x9q4/users/*/vote'                 # user, key, value per line
cubby watch status-4m8q2xk7/shared/deploy --exec ./deploy.sh
cubby update status-4m8q2xk7/shared/count --exec 'jq "(. // 0) + 1"'
cubby add  home-k7m2x9q4/shared/temp 21.4                 # a point in a series
cubby add  'home-k7m2x9q4/shared/env.*' temp=21.4 rh=45   # several series at once
cubby series home-k7m2x9q4/shared/temp --since 24h        # t<TAB>value per point
cubby new --label status --locked                       # prints name and key, saves the key
curl "$(cubby url poll-k7m2x9q4/shared/question)"       # same thing over raw HTTP
```

Quote paths containing `*`, or the shell tries to expand them. Tokens are fetched and saved automatically. Exit codes tell scripts what happened without parsing output: 1 not found, 2 version conflict, 3 not allowed, 4 limit hit. Server administration lives under `cubby admin`.

## JS client at a glance

Optional. Everything it does is plain HTTP underneath.

```js
import { kv } from "https://kv.example.com/kv.js"

const db = kv("poll-k7m2x9q4")

await db.shared.set("question", "Pizza or tacos?")
const q = await db.shared.get("question")               // null if missing

await db.user().set("vote", "tacos")                    // my public data
const { items } = await db.users.list("vote")           // everyone's vote

await db.me.set("showPercent", true)                    // private

await db.shared.update("settings", s => ({ ...s, open: false }))   // safe shared edit

const stop = db.shared.watch("msg.*", change => console.log(change.key, change.value))

await db.shared.add("temp", 21.4)                       // a point in a series
await db.shared.add("env.*", { temp: 21.4, rh: 45 })    // one point each in env.temp and env.rh
const { points } = await db.shared.series("temp", { since: Date.now() - 864e5 })

const admin = kv("poll-k7m2x9q4", { key: "sk_..." })   // can write shared/ in a locked space
```

## Decisions and rejected alternatives

| Decision | Rejected | Why |
|---|---|---|
| Go | Rust | A static binary without cgo, cross-compiled with one environment variable per target, and simple concurrency for watches. Rust would be smaller and faster, but SQLite makes cross-compiling harder. |
| The server binary is also the CLI client | A separate client, or only `curl` | One file to install anywhere. The CLI handles tokens, JSON and retries, so scripts stay short. Raw HTTP still works where the binary can't run. |
| Admin commands under `cubby admin`, through a local socket | Admin HTTP API, or admin commands mixed with client ones | Nothing to secure over the network, and no confusion between deleting a key and deleting a space. |
| Keep it usable without a client library | Features that need client-side hashing, URL fragment rules or remembered keys | The pages using this are short-lived. If raw HTTP takes 150 lines, people won't use it without the library, and the library becomes the real API. |
| "Space" as the one container | A project holding spaces | One concept is enough. A space works for an app, a poll, or a single chat room. |
| `shared/`, `users/{id}/`, `me/` | Only shared + private, or ownership flags on keys | "Everyone reads, only I write" covers votes, names, scores and messages without conflicts or tampering. Private data is still needed for settings and drafts. |
| Locking a whole space at creation | Locks per key, hashed read and write keys per space | One optional key per space gives read-only shared data with a plain header. Locking later would let anyone lock someone else's space. |
| Reads need no token, except `me/` | Token on every request | Anyone can get a token, so requiring one protects nothing. `me/` needs it only to know who "me" is. |
| One visitor ID across all spaces, returned by `/anon` | A different ID per space | Simpler for raw clients, no extra request. Linking a visitor across spaces reveals little when nothing stored is secret. |
| Space names need a random-looking suffix | Any name, or names derived from the page URL | Short names collide by accident. URL-derived names tie the store to how pages are hosted. |
| Token in localStorage, sent as a header | Cookie | A cookie from another domain counts as third-party. Safari and Brave block it, Firefox partitions it. |
| Closed mode with operator-issued tokens, one per device | One static server password | A leaked token is revoked alone, without changing every device. Each has its own scopes and end, and writes show who made them. |
| Scopes per token as `space/area/target:rights` | Roles, or permissions stored on spaces | Uses the same prefix patterns as the API, and the operator sees all of a token's access in one line. |
| Opaque random token | JWT | One process with a database lookup doesn't need stateless verification. Opaque tokens are revoked by deleting a row. |
| Value is text, JSON detected by parsing | Arbitrary bytes with client content type | Arbitrary content types make the server free hosting for phishing pages. `fetch` sends `text/plain` for JSON strings anyway. |
| Raw value as the body | `{ value, version }` wrapper | People would PUT the wrapper back and end up with nested wrappers. |
| Whole-value writes only | JSON field paths, `PATCH`, increment | Too much server logic. Separate keys and `ifVersion` cover the real needs. Series add whole points, they don't edit values. |
| Long polling with a version cursor | SSE | `EventSource` can't send headers, proxies buffer SSE, and SSE needs its own replay logic. |
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
| Proof of work, `ttl`, key locks, device linking: later | Including them now | Each is useful, none is needed for the first version. All can be added without changing the API. |
