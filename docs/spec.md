# cubby specification

The detailed contract for the server, its built-in CLI and the optional JS client. [design.md](design.md) has the overview. Where they disagree, this file wins.

"Must" means a client or server that doesn't do it is broken. "Should" means a strong default with room for a reason not to.

Contents:

1. [Conventions](#1-conventions)
2. [Names](#2-names)
3. [Visitors and tokens](#3-visitors-and-tokens)
4. [Areas and access](#4-areas-and-access)
5. [Locked spaces](#5-locked-spaces)
6. [Closed servers and issued tokens](#6-closed-servers-and-issued-tokens)
7. [Values](#7-values)
8. [Versions](#8-versions)
9. [Endpoints](#9-endpoints)
10. [Watching](#10-watching)
11. [Series](#11-series)
12. [Limits and quotas](#12-limits-and-quotas)
13. [Server config and lifecycle](#13-server-config-and-lifecycle)
14. [Errors](#14-errors)
15. [HTTP details](#15-http-details)
16. [JS client](#16-js-client)
17. [CLI](#17-cli)
18. [Implementation notes](#18-implementation-notes)
19. [Later](#19-later)

## 1. Conventions

- All API routes live under `/v1`. The client script is at `/kv.js`, a health check at `/health`.
- Every `/v1` response that isn't a stored value or a 304 has a JSON body.
- Every error is JSON with at least `error`, a stable snake_case code, and `message`, human text.
- `base64url` means RFC 4648 base64url without padding.
- Sizes in config are like `64KB`, meaning powers of 1024. Durations are like `30d`.
- Examples shorten space names to `poll` and leave out headers that aren't the point.
- "Area" means one of `shared/`, `users/{id}/` or `me/` within a space.

## 2. Names

### Space name

16 to 63 lowercase letters, digits and single hyphens, not starting or ending with a hyphen:

```
^[a-z0-9]+(-[a-z0-9]+)*$      16 to 63 characters
```

| Name | Valid | Why |
|---|---|---|
| `cedar-pilot-mango-frost` | yes | four generated words |
| `poll-cedar-pilot-mango-frost` | yes | a label plus four generated words |
| `team-lunch-dashboard` | yes | any name of 16+ characters |
| `poll` | no | shorter than 16 |
| `poll-k7m2x9q4` | no | shorter than 16 |
| `Poll-cedar-pilot-mango-frost` | no | uppercase |
| `poll--cedar-pilot` | no | double hyphen |

- Anyone using the same name uses the same space. The name is the only thing between a stranger and an open space's `shared/` and `users/` data, so how hard it is to guess matters.
- The server only checks length and characters. A hand-picked name like `team-lunch-dashboard` is valid and as easy to guess as it looks. For privacy, let cubby generate the name.
- The 16-character minimum applies to the API only. The server stores and manages any space whose name is file-safe (the same characters, 1 to 63 long), such as `poll-k7m2x9q4` from before v0.8.0. Such a space can't be reached over the API, but it still counts toward its writers' quotas, shows in `cubby admin spaces`, can be exported or deleted with `cubby admin`, and is cleaned up after 180 days idle.
- Words like `anon`, `spaces` or `whoami` can never be valid names, so system routes can't collide with spaces.
- An invalid name returns 400 `invalid_space` with a `suggestion`: a valid name built from it, such as `poll-slate-hinge-cocoa-pilot` for `poll`.

Getting a name:

- **Generate it.** Four words drawn from a fixed list of 4096 short English words (3 to 6 letters), joined by hyphens. That's 48 random bits, about 2.8 × 10^14 names. A label goes in front: `poll-cedar-pilot-mango-frost`. A label is at most 35 characters, so a labeled name always fits in 63. The JS client exports `newName("poll")`, the CLI has `cubby name poll`, the server uses the same list. The first write creates the space.
- **Ask the server** with `POST /v1/spaces`, see [Create a space](#create-a-space). Required for locked spaces.

### Visitor ID

`u_` followed by 16 random lowercase base32 characters, for example `u_k7m2x9q4p1r8t3w6`. The same ID in every space.

### Key

```
^[A-Za-z0-9._:-]{1,128}$
```

- No slashes, so a key is always one path segment. Slashes and `%2F` return 400 `invalid_key`.
- Case-sensitive.
- `watch` and `series` aren't keys. They're the action words that can follow a key (see [Paths](#paths)), and a key with the same name would make URLs like `/shared/series/series` hard to read. Any later action word is reserved the same way.
- `.` and `..` aren't keys. Browsers, `fetch` and `curl` collapse them as path segments, so they could never be reached and would send the request elsewhere. `...` and longer are fine.
- Percent-escapes are decoded once before checking, so `%2E%2E` is `..` and `%2F` is a slash. Both are rejected.
- Group related keys with dots, like `msg.1791449600123.a8k2`. Listing and watching select by prefix, so the grouping part goes first.
- Keys sort by byte order. Use fixed-width numbers when order matters, such as 13-digit millisecond timestamps.

### Pattern

A prefix followed by `*`, for listing, watching and adding to several series at once:

```
^[A-Za-z0-9._:-]{0,127}\*$
```

- `*` is the last character and appears once. `*` alone matches every key in the area.
- `msg.*` matches `msg.1`, `msg.` and `msg.a.b`, not `msg`.
- The prefix is plain text, not a path or a word. `env*` also matches `envelope`, which is why grouping with a trailing dot, `env.*`, is the habit.
- A path segment is a key if it has no `*` and a pattern if it ends with one.

What a pattern does on each route, with `shared/` holding `env.temp` = `21.4`, `env.rh` = `45` and `note` = `"hi"`:

| Request | Body | Returns |
|---|---|---|
| `GET env.*` | none | the matching keys with their values: `{"items": [{"key": "env.rh", "value": 45, "version": …}, {"key": "env.temp", "value": 21.4, "version": …}], "version": …, "next": null}` |
| `GET env.*/watch` | none | the matching keys that changed, with their current values: `{"changes": [{"key": "env.temp", "value": 21.5, "version": …}], "version": …, "more": false}` |
| `POST env.*/series` | `{"temp": 21.5, "rh": 46}` | the names of the keys written, not their values: `{"version": …, "t": …, "keys": ["env.temp", "env.rh"]}` |
| `GET env.*/series` | none | 400 `invalid_key`. Series are read one key at a time for now, see [Later](#19-later) |
| `PUT env.*`, `DELETE env.*` | | 405. Writing and deleting take one key |

- "List" in this spec means a `GET` with a `*` anywhere in the path, in the key or in `users/*/`. It isn't a separate action. A `GET` on a single key returns the raw value. A list returns JSON items, because it can match many keys.
- A pattern never returns names alone. List and watch always include the values. There's no keys-only listing.
- Full key names come back, prefix included: `env.temp`, not `temp`.
- Under `users/*/`, every item also has `user`. Series keys also have `t`, the time of their newest point, and `value` is that point.

### Paths

Every data URL has the same shape:

```
/v1/{space}/{area}/{target}[/{action}]
```

- `{area}` is `shared`, `users/{id}`, `users/*` or `me`.
- `{target}` is a key or a pattern. It's always there. There's no URL that acts on a whole area without one, so watching an area is `*/watch`.
- `{action}` is left out, `watch` or `series`. Without it, the request is about the target's current value.
- Nothing comes after the action.

The method and the action together say what happens:

| | no action | `/watch` | `/series` |
|---|---|---|---|
| `GET` | read a key, or list a pattern | wait for changes | read a series' points |
| `PUT` | replace a value | 405 | 405 |
| `POST` | 405 | 405 | add a point, or one point to each of several series with a pattern |
| `DELETE` | delete a key and its points | 405 | 405 |

A 405 names the methods that work and, where it helps, the route you probably meant. `POST /shared/temp` answers "to add a point use /temp/series, to replace the value use PUT".

## 3. Visitors and tokens

### Getting a token

```
POST /v1/anon
```

```
201
{"token": "kv_4Jx9...", "id": "u_k7m2x9q4p1r8t3w6"}
```

- A token is `kv_` plus 32 random bytes in base64url. It belongs to one visitor.
- The response includes the visitor's ID, so a client needs no second request to know who it is.
- Rate limited by IP: 10 per hour with a burst of 10. IPv6 addresses are grouped by /64.

### Sending the token

```
Authorization: Bearer kv_4Jx9...
```

or `?token=kv_4Jx9...` where a header can't be set, such as a browser tab. The header wins if both are given. The scheme name `Bearer` is case-insensitive. A token that doesn't have the shape `kv_` plus 43 base64url characters is treated as unknown without a lookup. The server must not log query strings containing `token`.

### When a token is needed

| Request | Token |
|---|---|
| any read of `shared/` or `users/` | no |
| any write or delete | yes |
| anything in `me/` | yes |
| `GET /v1/whoami` | yes |
| `POST /v1/spaces` | yes |

- This table is for an open server. In a closed server every request needs a token, see [Closed servers and issued tokens](#6-closed-servers-and-issued-tokens).
- A token sent with a read that doesn't need one is used only for rate limiting. If it's unknown or expired, it's ignored.
- Where a token is needed, a missing one returns 401 `missing_token` and an unknown or expired one returns 401 `invalid_token`.

### whoami

```
GET /v1/whoami
```

```
200
{"id": "u_k7m2x9q4p1r8t3w6"}
```

For clients that kept the token but lost the ID.

### Expiry

- A visitor counts as seen whenever a request carries their valid token, including reads where the token wasn't needed. The JS client always sends it once it has one.
- After 365 days unseen (`visitor_idle_ttl`), a cleanup job deletes the visitor, their token, and their `users/` and `me/` data in every space.
- Visitors who never stored anything are deleted after 30 days unseen (`empty_visitor_idle_ttl`). Tokens fetched by pages that never wrote would otherwise pile up for a year.
- `visitor_idle_ttl` should stay longer than `space_idle_ttl`. Otherwise a visitor's votes or messages could vanish from a space that's still in use.
- Shared keys they wrote stay. Their bytes stop counting against any visitor and keep counting against the space.
- A client that gets 401 `invalid_token` must drop the token, get a new one and retry once. The new token is a new visitor with a new ID.
- A client that gets 403 `server_closed` from `/v1/anon` must not retry. The server only accepts issued tokens.
- Issued tokens expire differently, see [Valid until](#valid-until).

## 4. Areas and access

| Area | Path | Read | Write |
|---|---|---|---|
| shared | `/v1/{space}/shared/{key}` | anyone | any visitor; in a locked space only with `Space-Key` |
| a visitor | `/v1/{space}/users/{id}/{key}` | anyone | only the visitor `{id}` |
| all visitors | `/v1/{space}/users/*/{key or pattern}` | anyone | not allowed |
| private | `/v1/{space}/me/{key}` | only the visitor | only the visitor |

All areas support read, write, delete, list, watch and series, except that `users/*/` is read-only and doesn't read series yet (see [Later](#19-later)).

### shared/

The space's common data. Spaces come in two kinds, and they differ only here:

- **Open:** every visitor can write any key. For groups working on the same data: lists, boards, documents, room settings, game state. Created by the first write.
- **Locked:** only requests with the space key can write. For one author publishing to many readers: poll questions, schedules, status pages, script output. Created with `POST /v1/spaces?locked`. See [Locked spaces](#5-locked-spaces).

Neither kind is the default use case. The kind is fixed when the space is created.

### users/{id}/

A visitor's public data. Good for things that belong to one visitor and everyone should see: a vote, a display name, a score, messages they wrote.

- Writing or deleting under another visitor's ID returns 403 `not_owner`.
- Locking a space doesn't affect `users/`. Every visitor can always write their own.
- Goes away with the visitor after `visitor_idle_ttl`.

### users/*/

Reads one key, or one pattern, across every visitor in the space:

- `users/*/vote` lists every visitor's `vote`.
- `users/*/msg.*` lists every visitor's keys starting with `msg.`.
- Each item includes `user`, the ID it belongs to.
- Only `GET`, `HEAD` and watch. Other methods return 405.

### me/

Private data of the visitor in this space.

- Every request needs a token, including reads. No visitor can address another's `me/`.
- Goes away with the visitor's token.
- Not affected by locking.

## 5. Locked spaces

A locked space lets only its creator, or whoever they give the key, write `shared/`. Everyone can still read it, and every visitor can still write their own `users/` and `me/`.

```
POST /v1/spaces?label=poll&locked
Authorization: Bearer kv_...
```

```
201
{"space": "poll-cedar-pilot-mango-frost", "key": "sk_8Hq3vT..."}
```

- The key is `sk_` plus 32 random bytes in base64url. The server stores only its SHA-256 hash and returns the key this once.
- A space can only be locked when it's created through this endpoint. A space created by its first write is open and stays open. Locking later would let anyone lock someone else's space.
- Writes and deletes in `shared/` must send `Space-Key: sk_...`. Missing or wrong: 403 `space_locked`.
- A `Space-Key` header on any other request is ignored.
- A lost or leaked key can be replaced only by the server operator: `cubby admin rekey <space>` prints a new key and invalidates the old one.
- The key must never be in a public page. A page that contains it might as well use an open space.

Typical uses: a poll whose question only the author can change, a status page updated by a script, a published schedule or announcement, quiz questions with answers collected in `users/`.

Open spaces need nothing comparable: no creation step and no key. The trade-off is that anyone who learns the name can change `shared/`, which is what a group editing together wants.

## 6. Closed servers and issued tokens

A server is open by default. Anyone can get a token with `POST /v1/anon`, and reading `shared/` and `users/` needs no token at all. A closed server turns both off. It accepts only tokens the operator issued, and every request needs one, reads included.

```toml
[server]
access = "closed"      # "open" by default
```

Switching is a config change and a restart. Nothing in the data files changes.

### What changes in a closed server

- `POST /v1/anon` returns 403 `server_closed`.
- Every `/v1` request needs a token: reads, lists, watches, series, `whoami` and `POST /v1/spaces`. A missing one returns 401 `missing_token`. An unknown, revoked or expired one returns 401 `invalid_token`.
- Only issued tokens count. Anonymous tokens from a time when the server was open return 401 `invalid_token`. Their visitors and data stay and age out under `visitor_idle_ttl` as usual, so reopening the server soon after makes them valid again.
- `/health` and `/kv.js` stay open. Neither shows any data.
- Everything else works as in an open server: areas, locked spaces, `Space-Key`, quotas and rate limits.
- A closed server starts with no issued tokens, so nothing works until the operator creates one. `cubby serve` warns at startup when the server is closed and no active token exists.

Closing a server decides who may use it. It doesn't protect anything in transit. Without TLS, tokens and values cross the network as plain text, see [Transport and logging](#transport-and-logging).

### Issued tokens

The operator creates them on the server machine, see [Managing tokens](#managing-tokens):

```
$ cubby admin token new sensor-livingroom --allow 'home-cedar-pilot-mango-frost/shared/env.*:w' --until 90d
kv_4Jx9...
u_k7m2x9q4p1r8t3w6
```

- An issued token belongs to a visitor like any other, with an ID, a `users/{id}/` and a `me/`. It has the same shape as an anonymous token, so clients can't tell them apart and need no changes to use one.
- Each has a name, unique on the server, for the operator only. Clients never see it. Names follow `^[a-z0-9]+(-[a-z0-9]+)*$`, at most 63 characters.
- The token is printed once. The server stores only its SHA-256 hash.
- Every issued token has at least one [scope](#scopes) and can have an [end](#valid-until).
- Issued tokens also work in an open server, for a token with a narrow scope or an end date, or a fixed identity for a script. There, scopes apply only to requests that need a token. A read that would work without a token works with any valid token, because dropping the token would get the same answer.
- `visitor_idle_ttl` and `empty_visitor_idle_ttl` don't apply to a visitor with an active issued token. A sensor that's silent for a year keeps its token. Tokens end by `revoke` or by their end date.
- The limit on new tokens per IP doesn't apply. Every other limit does.

### Scopes

Every request with an issued token is checked against the token's scopes. It's allowed only if at least one scope covers it.

```
SPACE[/AREA[/TARGET]]:RIGHTS
```

| Part | Values | If left out |
|---|---|---|
| `SPACE` | a space name, a prefix pattern like `home-*`, or `*` | required |
| `AREA` | `shared`, `users`, `me` or `*` | `*` |
| `TARGET` | a key or a pattern, see [Names](#2-names) | `*` |
| `RIGHTS` | `r`, `w` or `rw` | required |

`all` is short for `*:rw`.

Rules:

- A scope only narrows. It never allows what the normal rules forbid. `users/{id}/` stays writable only by its owner, `me/` is always the token's own, and a locked space still needs `Space-Key`.
- `r` covers `GET` and `HEAD`: reading, listing, watching and reading series.
- `w` covers `PUT`, `DELETE` and `POST`: writing, deleting and adding points. It doesn't include reading, but write responses still return versions and times, so conditional writes work.
- `users` covers both `users/{id}/` and `users/*/`.
- A key covers only itself. A pattern `p*` covers every key that starts with `p`, and every pattern `q*` where `q` starts with `p`. `env.*` covers `env.temp`, `env.*` and `env.t*`, but not `*` or `e*`.
- A request with a pattern needs one scope that covers the whole pattern. Scopes don't add up: `env.temp:r` and `env.rh:r` together don't allow `GET env.*`. A batch `POST env.*/series` needs `w` on a scope covering `env.*`.
- A space pattern works the same way on space names. End the prefix with `-` to mean a label: `home-*` covers `home-cedar-pilot-mango-frost` and `home-office-maple-quilt-tundra-cobalt`.
- `POST /v1/spaces` needs `w` on a scope whose `SPACE` covers every name the request could produce. `home-*` covers `?label=home`, `?name=home-cedar-pilot-mango-frost` needs a scope covering that name, and only `*` covers a request with neither. Creating a space by its first write needs only `w` on that name.
- `whoami` needs only a valid token.
- A request outside every scope returns 403 `out_of_scope`. The scope check comes before the key is looked up, so a narrow token gets 403 rather than 404 for a key outside its scopes, and can't probe what exists.

Examples:

| For | Scopes |
|---|---|
| yourself, everything | `all` |
| a sensor that writes its readings | `home-cedar-pilot-mango-frost/shared/env.*:w` |
| a wall dashboard | `home-cedar-pilot-mango-frost:r` |
| a deploy script that bumps a counter | `homelab-maple-quilt-tundra-cobalt/shared/deploy-count:rw` |
| one poll voter, one token each | `poll-cedar-pilot-mango-frost/shared:r`, `poll-cedar-pilot-mango-frost/users/vote:rw` |
| a team that makes its own spaces | `team-*:rw` |

### Valid until

`--until` gives a token an end:

- a duration from now: `12h`, `90d`
- a date, valid through the end of that day in the server's time zone: `2027-01-31`
- a time in RFC 3339, with or without offset, without meaning the server's time zone: `2027-01-31T18:00`, `2027-01-31T18:00:00Z`

Rules:

- From that moment every request returns 401 `invalid_token`. A watch that's already waiting is answered normally, and the next request fails.
- An expired token shows as `expired` in `admin token list`, and `admin token edit --until` brings it back with the same token and ID.
- If nobody does, the visitor ages out like an anonymous one, counting its end as its last seen time. After `visitor_idle_ttl` the token, visitor and data are deleted.
- Without `--until`, a token is valid until it's revoked.

### Managing tokens

```
cubby admin token new <name> --allow SCOPE [--allow SCOPE ...] [--until T]
cubby admin token list [--json]
cubby admin token edit <name> [--allow SCOPE ...] [--until T | --no-until]
cubby admin token rotate <name>
cubby admin token revoke <name> [--purge]
```

| Command | Does | Prints |
|---|---|---|
| `new` | issue a token for a new visitor. At least one `--allow` is required, there's no implicit full access | the token, and the visitor ID on a second line |
| `list` | every issued token | one line per token, tab-separated: name, visitor ID, state (`active` or `expired`), end or `-`, last seen, scopes separated by spaces |
| `edit` | change scopes or end. Given `--allow`s replace all old scopes. Token and ID stay | nothing |
| `rotate` | a new token for the same visitor, scopes and end. The old one stops working at once. For a leaked token whose data should stay | the new token |
| `revoke` | the token stops working at once and the name is free again. The visitor's data stays and ages out under `visitor_idle_ttl`. `--purge` deletes the visitor and their `users/` and `me/` data now | nothing |

- Like every admin command, these go through `admin.sock` to a running server and take effect on its next request. Any cached token lookups are dropped.
- Scopes are checked when given. A malformed one fails the command with exit code 64 and changes nothing.
- Anonymous visitors aren't listed. `cubby admin stats` counts them.

### Using an issued token

- **Raw HTTP:** like any token, in `Authorization: Bearer` or `?token=`.
- **CLI:** `cubby login <token>` checks it with `whoami` and saves it in `credentials.toml` for that server, replacing any anonymous token. `CUBBY_TOKEN` works as before. If the CLI needs a token and `/v1/anon` returns 403 `server_closed`, it stops with exit code 3 and says to run `login`.
- **JS client:** `kv(space, { token: "kv_..." })`. See [Setup](#setup).

A page can't get its own token from a closed server. A token written into a page's source is readable by anyone who can load the page, which is fine only if the page itself is private. Otherwise give it to the page once, for example in a URL fragment the page saves to localStorage and removes from the address bar.

## 7. Values

### What's accepted

- The body of a `PUT` is the value, exactly as sent.
- It must be valid UTF-8, otherwise 400 `invalid_encoding`.
- It must not be empty, otherwise 400 `empty_value`, whose message points to `DELETE`. Whitespace alone is valid text.
- At most 64 KB (`max_value_size`), counting raw bytes, otherwise 413 `value_too_large` with `limit`. The server stops reading at limit + 1 bytes.
- The request `Content-Type` is ignored. `fetch` sends `text/plain` for string bodies even when they contain JSON.

### JSON or text

The server checks once, when storing, whether the whole body is one valid JSON text (RFC 8259, whitespace allowed around it).

- If so, it's stored as JSON: numbers, `true`, `false`, `null`, quoted strings, arrays, objects.
- Otherwise it's stored as text.
- JSON nested deeper than 64 levels returns 400 `json_too_deep`.

A bare `true`, `123` or `null` is JSON. To store the text "true", send `"true"` with quotes. The JS client always sends JSON.

Values are never reformatted.

Points in a [series](#11-series) follow the same rules, with a smaller size limit.

### What's returned

`GET` returns the stored bytes unchanged, as `application/json; charset=utf-8` for JSON and `text/plain; charset=utf-8` for text. No other content type is ever used for a value.

In list and watch responses, JSON values appear as themselves and text values as JSON strings.

### Size

64 KB is about 10,000 words. It's low on purpose: values are written whole, every watcher gets the whole value on every change, and a small limit pushes people towards a key per item, which also avoids conflicts. Pinned spaces can raise it up to 1 MB.

## 8. Versions

- Each space has one counter. Every successful write or delete in the space, in any area, raises it.
- Each key stores the counter value of its last write: its version.
- Each key also records `by`, the ID of the visitor whose write or delete gave it that version. Reads, lists and watches return it, so a page can tell who changed `shared/` data and recognise its own writes coming back through a watch. Visitor IDs are already public under `users/*/`, so this reveals nothing new. A key written with an issued token shows that token's visitor ID.
- Versions only grow and aren't contiguous per key. `0` means "doesn't exist".
- A new space's counter starts at the current Unix time in milliseconds. Clients must treat versions as opaque increasing numbers. They stay below 2^53, so they're exact in JavaScript.
- A space that doesn't exist behaves as if its counter were the current time in milliseconds. Lists and watches on it return that as `version`, so a client watching an empty space picks up the first write with no special case.
- The server must never start a counter below a version it has already handed out for that name, even if the clock goes backwards.
- A `PUT` with the same bytes as the current value doesn't create a new version and doesn't notify watchers.

## 9. Endpoints

`{area}` stands for `shared`, `users/{id}` or `me`.

### Create a space

```
POST /v1/spaces
POST /v1/spaces?label=poll
POST /v1/spaces?label=poll&locked
POST /v1/spaces?name=poll-cedar-pilot-mango-frost&locked
```

```
201
{"space": "poll-cedar-pilot-mango-frost"}
{"space": "poll-cedar-pilot-mango-frost", "key": "sk_8Hq3vT..."}      with locked
```

- Generates a name (four random words, after the label if given; a label is at most 35 characters), creates the space empty, and returns the name. The name is now taken.
- With `name`, creates exactly that name instead, which must follow the naming rule. A name that's taken, by `POST /v1/spaces` or by a write, returns 409 `space_exists`. `name` and `label` can't be combined.
- Needs a token, and counts against the limits on new spaces.
- Optional for open spaces, since writing to any valid name creates it. Required for locked ones.
- An empty space is cleaned up like any space, after 180 days without activity.

### Read

```
GET /v1/poll/{area}/question
```

```
200
Content-Type: text/plain; charset=utf-8
ETag: "1791449600041"
KV-By: u_k7m2x9q4p1r8t3w6

Pizza or tacos?
```

- `KV-By` is the visitor who last wrote the key.
- 404 `not_found` if the key or the space doesn't exist.
- For a series key, the value is its newest point, and the response also has `KV-Time: 1791449600042`, that point's time in milliseconds. Plain keys don't have this header. Versions are opaque, so they can't tell a page when a reading was taken.
- `HEAD` works the same without a body.
- `If-None-Match: "1791449600041"` returns 304 if unchanged.

### Write

```
PUT /v1/poll/{area}/question
PUT /v1/poll/{area}/question?ifVersion=1791449600041
PUT /v1/poll/{area}/question?ifVersion=0
```

```
201 {"version": 1791449600042}      created
200 {"version": 1791449600042}      replaced or unchanged
```

- The response also has `ETag: "1791449600042"`, like a read, so the new version is available from a header as well as the body.

- Creates or replaces the value, and creates the space if needed and allowed.
- A series key can't be replaced. `PUT` on one returns 409 `is_series`. Add points with `POST {key}/series`, or `DELETE` the key first.
- `ifVersion=N`: succeeds only if the key's version is exactly N. `ifVersion=0`: only if it doesn't exist. A mismatch returns:

```
412
{"error": "version_conflict", "message": "...", "currentVersion": 1791449600045}
```

- `If-Match: "N"` is accepted as an equivalent. The query parameter wins if both are given.

### Delete

```
DELETE /v1/poll/{area}/question
DELETE /v1/poll/{area}/question?ifVersion=1791449600042
```

```
200 {"version": 1791449600043}
```

- 404 `not_found` if the key doesn't exist. `ifVersion` works as for `PUT`.
- Deleting a series key deletes all its points too. Afterwards the name is free for either kind.
- The response has `ETag` with the version of the delete, as for writes.
- Watchers see a delete as a change with `"deleted": true`.

### List

```
GET /v1/poll/{area}/msg.*
GET /v1/poll/{area}/msg.*?limit=50&order=desc
GET /v1/poll/{area}/msg.*?after=msg.1791449600123
GET /v1/poll/users/*/vote
```

```
200
{
  "items": [
    {"key": "vote", "value": "tacos", "version": 1791449600041, "user": "u_k7m2x9q4p1r8t3w6", "by": "u_k7m2x9q4p1r8t3w6"},
    {"key": "vote", "value": "pizza", "version": 1791449600044, "user": "u_p1r8t3w6k7m2x9q4", "by": "u_p1r8t3w6k7m2x9q4"}
  ],
  "version": 1791449600057,
  "next": null
}
```

- Items have `key`, `value`, `version` and `by`. Under `users/*/` they also have `user`, which is always the same as `by` there. Series keys also have `t`, the time of the newest point, and `value` is that point.
- Sorted by key, ascending unless `order=desc`. Under `users/*/` ties are sorted by `user`.
- `limit` is 1 to 1000, default 1000.
- `next` is a cursor when more pages exist, otherwise `null`. Pass it back as `after`.
- `version` is the space's counter when the list was read. Watching from it continues exactly after this list.
- Listing a space that doesn't exist returns an empty list, not an error.
- A target without `*` lists that exact key: zero or one item, or one per visitor under `users/*/`.

### Watch

```
GET /v1/poll/{area}/question/watch?version=1791449600041
GET /v1/poll/{area}/msg.*/watch?version=0
GET /v1/poll/users/*/vote/watch
```

See [Watching](#10-watching).

### Series

```
POST /v1/home/shared/temp/series
POST /v1/home/shared/env.*/series
GET  /v1/home/shared/temp/series
GET  /v1/home/shared/temp/series?since=1791440000000&limit=500
```

See [Series](#11-series).

### Other routes

| Route | Returns |
|---|---|
| `GET /kv.js` | the JS client as an ES module |
| `GET /health` | 200 `{"ok": true, "version": "v0.8.1"}` |
| `OPTIONS` anything | the CORS preflight response |
| anything else, including `/` and `/v1` | 404 `{"error": "not_found", "message": "No such route."}` |

The server doesn't describe itself. Unknown routes all get the same 404 with no hints, so a stray visitor learns nothing about the API. Clients and these docs are where the paths are explained.

## 10. Watching

### The rule

A watch asks: "what changed in this key or pattern after version N?" The server answers with the current state of each matching key whose version is greater than N.

| `version` | Behavior |
|---|---|
| `0` | Returns at once with every existing matching key. A snapshot, without deleted keys. |
| `N > 0` | Returns at once if a matching key changed after N. Otherwise waits until one does, or until the timeout. |
| left out | Uses the space's current counter: only changes from now on. Waits. |

### The response

```json
{
  "changes": [
    {"key": "msg.1791449600999.b2k7", "value": {"text": "hey"}, "version": 1791449600058, "user": "u_k7m2x9q4p1r8t3w6", "by": "u_k7m2x9q4p1r8t3w6"},
    {"key": "vote", "deleted": true, "version": 1791449600059, "user": "u_p1r8t3w6k7m2x9q4", "by": "u_p1r8t3w6k7m2x9q4"}
  ],
  "version": 1791449600059,
  "more": false
}
```

- `changes` is ordered by version. Each key appears at most once, with its current state. Every entry carries `by`; for a deleted key it's whoever deleted it. Under `users/*/` entries also carry `user`. Series keys carry `t`, and their value is the newest point.
- `version` is what the client sends next. It's a position in the space's changes, not any key's version, and it can be far ahead of a watched key's own version. For `ifVersion`, always use the `version` inside a change entry.
- On timeout: 200 with `"changes": []` and the space's current counter as `version`.
- `more: true` means more changes were ready than the 1000 that fit. `version` is then the last included entry's, and the next request returns at once with the rest.

### Timing

- `timeout` is 1 to 55 seconds, default 25, below the common 60-second proxy idle timeout.
- After the first matching change, the server waits 50 ms more so a burst of writes goes out together.

### Why nothing is lost

The client always passes the version from its last response. A write between two requests has a higher version, so the next request returns it at once. Several writes to one key while nobody asks come back as one change with the latest value. Watching is for state, not an event log.

### Deletes

- Deletes leave a marker (tombstone) so watchers learn about them. Tombstones are removed after 7 days (`tombstone_ttl`), and the space remembers the highest removed version as its floor.
- A watch with `0 < version < floor`, or with a version above the space's counter, returns 410 `version_expired`. The client starts again from `version=0` and treats keys it knew that are missing from the new snapshot as deleted.

### Limits

- At most 20 open watches per IP and 1000 on the server. Over the limit: 429 `too_many_watches`.
- Browsers allow 6 HTTP/1.1 connections per domain across all tabs, so serve the API over HTTP/2.

## 11. Series

A series key keeps every value written to it, each with a time. It's for sensor readings, state changes and anything else where the past matters as well as the present. It still behaves like a normal key, whose value is the newest point.

### Series and plain keys

- The first `POST {key}/series` creates the key as a series. The first `PUT` creates a plain key. The kind stays until the key is deleted.
- `PUT` on a series key returns 409 `is_series`. `POST` or `GET` on `{key}/series` for a plain key returns 409 `not_series`.
- Reading the key, listing and watching work unchanged. The value is the newest point, and the point's time comes as `t` in list and watch entries, or as the `KV-Time` header on a read.
- The server stores points and returns them as stored. It never computes anything from them: no sums, averages or downsampling.

### Adding a point

```
POST /v1/home/shared/temp/series
Authorization: Bearer kv_...
Space-Key: sk_...                 only in a locked space's shared/

21.4
```

```
201 {"version": 1791449600058, "t": 1791449600042}      series created
200 {"version": 1791449600058, "t": 1791449600042}      point added
```

- The body is one value, under the same rules as `PUT`: UTF-8, JSON or text, not empty. An object is one point, stored whole. `{"c": 21.4, "ok": true}` is one point, not two.
- A point is at most 1 KB (`max_point_size`), otherwise 413 `value_too_large`.
- `t` is the server's clock in milliseconds when the request arrives. If that isn't later than the series' newest point, the server uses the newest point's time plus 1, so points the server times never collide.
- `?t=1791449000000` sets the time instead, for devices that buffered readings while offline. Times more than 60 seconds in the future return 400 `invalid_parameter`. A point at a time that already exists replaces it.
- A point that becomes the newest creates a new version and wakes watchers, even with the same value as before, because the reading's time changed.
- A point older than the newest goes into the series without changing the key's value or version, and wakes nobody. The response then has the key's current version.
- `ifVersion` isn't accepted. Adding a point never conflicts.

### Adding to several series at once

```
POST /v1/home/shared/env.*/series

{"temp": 21.4, "rh": 45, "co2": 612}
```

```
200 {"version": 1791449600061, "t": 1791449600042, "keys": ["env.temp", "env.rh", "env.co2"]}
```

- A `*` means several keys, as in list and watch. Each top-level name in the body is added to the prefix, so `env.*` and `temp` give `env.temp`. A bare `*` uses the names as they are.
- The body must be a JSON object with 1 to 100 members, otherwise 400 `batch_needs_object`.
- Each member's value is one point, of any JSON type. `{"door": "open"}` stores the JSON string `"open"`. Text that isn't JSON can only be added one key at a time.
- All points get the same `t`. `?t=` works as for one point.
- Each key gets its own version, in the order of the members. The response's `version` is the highest.
- All or nothing. An error names the member that caused it in `key`: an invalid key name, a plain key (`not_series`), or a point that's too large.
- It counts as one write for rate limits, so a device can send all its sensors in one request.

### Reading points

```
GET /v1/home/shared/temp/series                         the newest 1000
GET /v1/home/shared/temp/series?limit=100               the newest 100
GET /v1/home/shared/temp/series?since=1791440000000     the first 1000 after that time
```

```
200
{
  "key": "temp",
  "points": [
    {"t": 1791440000500, "value": 21.3},
    {"t": 1791440060500, "value": {"c": 21.4, "ok": true}},
    {"t": 1791440120500, "value": "sensor reset"}
  ],
  "version": 1791449600058,
  "next": null
}
```

- Points are always oldest first, ready to draw.
- Without `since`: the newest `limit` points. With `since`: the first `limit` points with `t` greater than `since`. `since=0` starts at the oldest point.
- `limit` is 1 to 1000, default 1000.
- `next` is the `since` to send for the following page, when more points come after the last one returned. Otherwise it's `null`, which is always the case without `since`.
- `version` is the space's counter when the points were read. Watching the key from it continues exactly after this read.
- Values appear as in list and watch: JSON as itself, text as a JSON string.
- 404 `not_found` if the key doesn't exist, 409 `not_series` if it's a plain key.
- A reader catching up with `since` doesn't see points added later with an older `?t=`. A page that cares reloads the range it shows.

### Series retention

- A series keeps its newest 50,000 points (`max_series_points`). Adding one more deletes the oldest in the same transaction, so a series never fails for being long.
- 50,000 points is about 35 days at one point a minute, or 6 days at one every 10 seconds.
- Points have no expiry of their own. They live as long as the key, which lives as long as its space, or its visitor under `users/` and `me/`.

### Series storage

- A point's size is its key length plus its value length. A number under an 8-character key is about 12 bytes, so a full series of numbers is around 0.6 MB.
- Points count towards the space and server quotas, but not towards keys per space.
- Towards the visitor quota, a series counts like a plain key: its key and newest value, for whoever wrote last. Otherwise a device in a locked space would hit the 1 MB visitor limit after a few sensors. Space quotas, the limit on new spaces and the per-series cap still bound the total.
- Trimming the oldest point happens before the quota check. A full series at a steady rate never runs into the space quota by itself.
- A 5 MB space holds about eight full numeric series. Pin a space for more.

### Watching

Watching a series key works like watching any key. It returns the newest point, and several points between two polls come back as one change. That suits a live display. To get every point, read `series?since=` with the last `t` you have.

There's no `{key}/series/watch`. Nothing comes after an action.

## 12. Limits and quotas

### Values and keys

| Limit | Default | Config | Error |
|---|---|---|---|
| Value size | 64 KB | `max_value_size` | 413 `value_too_large` |
| Point size in a series | 1 KB | `max_point_size` | 413 `value_too_large` |
| Points per series | 50,000, oldest dropped | `max_series_points` | none, see [Series retention](#series-retention) |
| Members in one batch | 100 | fixed | 400 `batch_needs_object` |
| Key length | 128 | fixed | 400 `invalid_key` |
| JSON depth | 64 | fixed | 400 `json_too_deep` |

### Storage

A key's size is its key length plus its value length. Tombstones count as their key length.

| Limit | Default | Config | Error |
|---|---|---|---|
| Per visitor, all spaces | 1 MB | `max_visitor_bytes` | 507 `visitor_quota_exceeded` |
| Per space | 5 MB | `max_space_bytes` | 507 `space_quota_exceeded` |
| Keys per space | 10,000 | `max_space_keys` | 507 `space_quota_exceeded` |
| Server total | 1 GB | `max_total_bytes` | 507 `server_full` |

- Every key remembers who wrote it last. A visitor's usage is the size of the keys they last wrote, in every area.
- Overwriting someone else's shared key moves its bytes to you. Deleting frees them for whoever wrote last.
- Pinned spaces in config can have their own limits.
- Series points count differently, see [Series storage](#series-storage).

### Rates

Token buckets. A 429 includes `Retry-After` in seconds and `retryAfter` in the body.

| Limit | Default | Error |
|---|---|---|
| New tokens per IP | 10 per hour, burst 10 | 429 `rate_limited` |
| Writes per visitor | 60 per minute, burst 30 | 429 `rate_limited` |
| Writes per IP | 300 per minute | 429 `rate_limited` |
| Write bytes per IP | 10 MB per day | 429 `rate_limited` |
| Reads per IP | 600 per minute | 429 `rate_limited` |
| New spaces per visitor | 5 per day | 429 `space_limit` |
| New spaces per IP | 20 per day | 429 `space_limit` |
| Distinct missing spaces per IP | 30 per hour, burst 30 | 429 `miss_limit` |
| Spaces on the server | 1000 | 507 `server_full` |

A `POST` that adds points counts as one write, however many series it adds to.

The IP-based write limits exist because one person can create many visitors. They're set high enough for a household or office behind one IP.

### Guessing space names

The miss limit makes guessing names slow. It counts how many different spaces that don't exist an address touches.

- A request to a space that doesn't exist costs one miss, the first time. Touching the same missing name again within the hour is free, so a page polling a deleted space costs one miss, not one per poll.
- A missing key inside a space that exists costs nothing. Neither does using a space that exists.
- A write that creates a space counts as a miss. `POST /v1/spaces` doesn't, and its creator can use the new space right away.
- Once an address is out of misses, every space it hasn't used in the last hour answers 429 `miss_limit`, whether the space exists or not. A 429 only for missing spaces would tell a guesser which names exist. Spaces it used in the last hour keep working, so pages already open aren't cut off.
- Refused requests don't cost anything. One miss comes back every 2 minutes.
- IPv6 addresses count per /64, like the other IP limits. `rate_limits = false` turns it off with the rest.

With generated names (48 bits) and 1000 spaces, one address needs about 3 × 10^11 guesses to find any space. At 30 an hour that is about a million years.

### When a limit is hit

Only the request that would go over fails. Nothing stored is touched, and reads and watches keep working.

## 13. Server config and lifecycle

### Lifecycle of a space

- **Created** by `POST /v1/spaces` or by the first successful write. Reads, lists and watches never create a space.
- **Active** whenever it's read, listed, watched or written. Reads count, so a status page that's read daily but updated twice a year stays alive.
- **Deleted** after 180 days without activity (`space_idle_ttl`), file and all, unless pinned.
- **Pinned** spaces are listed in config. They get their own limits and are never deleted automatically.

Most pages are used now and then, not constantly: a poll before each team lunch, a packing list before each trip. The retention defaults are long for that reason. The cost is that spam also lingers longer. Quotas cap how much there can be, and `cubby admin delete` removes it by hand.

### Retention defaults

| What | Default | Config |
|---|---|---|
| Space without activity | 180 days | `space_idle_ttl` |
| Visitor not seen | 365 days | `visitor_idle_ttl` |
| Visitor not seen who never stored anything | 30 days | `empty_visitor_idle_ttl` |
| Delete markers for watchers | 7 days | `tombstone_ttl` |

A watcher that comes back after more than 7 days gets 410 and reloads its snapshot, which is cheap. A longer `tombstone_ttl` would only avoid that reload.

### Fixed intervals

Not configurable, listed so they're in one place:

| What | Value |
|---|---|
| Cleanup job | every hour |
| "Last seen" and "last active" granularity | 1 hour |
| Write-ahead log size kept per space | 4 MB |
| Watch timeout | 25 s default, 1 to 55 s allowed |
| Pause before answering a watch after a change | 50 ms |
| Changes per watch response | 1000 |
| Open space files kept without watchers | 64, least recently used closed first |

### Config file

TOML, passed with `--config`. Everything is optional.

```toml
[server]
listen = ":28229"
data_dir = "./data"
public_url = "https://kv.example.com"   # used by kv.js to find the API
trusted_proxies = ["127.0.0.1/32"]      # whose X-Forwarded-For to believe
access = "open"                         # or "closed", see section 6

[retention]
visitor_idle_ttl = "365d"
empty_visitor_idle_ttl = "30d"
space_idle_ttl = "180d"
tombstone_ttl = "7d"

[limits]
max_value_size = "64KB"
max_point_size = "1KB"
max_series_points = 50000
max_visitor_bytes = "1MB"
max_space_bytes = "5MB"
max_space_keys = 10000
max_total_bytes = "1GB"

[spaces.poll-cedar-pilot-mango-frost]                  # pinned
max_bytes = "100MB"
max_value_size = "256KB"
max_series_points = 500000
```

### Administration

Through `cubby admin`, see [Admin](#admin) in the CLI section. There is no admin HTTP API.

## 14. Errors

```json
{"error": "code", "message": "Human-readable explanation", "...": "extra fields for some codes"}
```

| Status | `error` | When | Extra |
|---|---|---|---|
| 400 | `invalid_space` | name doesn't follow the rule | `suggestion` |
| 400 | `invalid_key` | bad key or pattern, or a batch member that doesn't make a valid key | `key` in a batch |
| 400 | `invalid_encoding` | body isn't UTF-8 | |
| 400 | `empty_value` | empty `PUT` or `POST` body | |
| 400 | `json_too_deep` | JSON nested over 64 levels | |
| 400 | `batch_needs_object` | a batch body that isn't a JSON object with 1 to 100 members | |
| 400 | `invalid_parameter` | bad `limit`, `order`, `after`, `since`, `t`, `timeout`, `version`, `ifVersion`, `label` | `parameter` |
| 401 | `missing_token` | token needed but missing | |
| 401 | `invalid_token` | token needed, unknown, revoked or expired | |
| 403 | `server_closed` | `POST /v1/anon` on a closed server | |
| 403 | `out_of_scope` | request outside every scope of an issued token | |
| 403 | `space_locked` | write to `shared/` of a locked space without the right `Space-Key` | |
| 403 | `not_owner` | write under another visitor's ID | |
| 404 | `not_found` | key or route doesn't exist | |
| 405 | `method_not_allowed` | wrong method for the route, such as `PUT` under `users/*/` or `POST` on a key without `/series` | `allow` |
| 409 | `space_exists` | `POST /v1/spaces?name=` with a taken name | `space` |
| 409 | `is_series` | `PUT` on a series key | |
| 409 | `not_series` | `/series` on a plain key | `key` in a batch |
| 410 | `version_expired` | watch version too old or from elsewhere | |
| 412 | `version_conflict` | `ifVersion` didn't match | `currentVersion` |
| 413 | `value_too_large` | value or point over the limit | `limit`, and `key` in a batch |
| 429 | `rate_limited` | a rate limit | `retryAfter` |
| 429 | `space_limit` | too many new spaces | `retryAfter` |
| 429 | `too_many_watches` | too many open watches | |
| 507 | `visitor_quota_exceeded` | visitor storage full | `limit`, `used` |
| 507 | `space_quota_exceeded` | space storage or key count full | `limit`, `used` |
| 507 | `server_full` | server storage or space count full | |

Clients branch on `error`, never on `message`.

### Query parameters

- Each route accepts only its own parameters, plus `token` everywhere and `_`, which is ignored so cache-busting libraries work. Anything else returns 400 `invalid_parameter` naming it. A typo like `ifversion=0` must fail, not turn a conditional write into an unconditional one.
- A parameter given twice returns 400 `invalid_parameter`.
- Numbers are plain decimal digits: no sign, spaces, hex or decimal point, at most 16 digits.

### Clients check before sending

The CLI and the JS client validate space names, keys, patterns and visitor IDs locally and refuse bad ones without a request. Otherwise a `#` or `?` typed into a key would be read as a fragment or query, and the request would silently address a different key.

## 15. HTTP details

### CORS

On every `/v1` response:

```
Access-Control-Allow-Origin: *
Access-Control-Expose-Headers: ETag, Retry-After, KV-Time, KV-By
```

On preflight:

```
Access-Control-Allow-Methods: GET, HEAD, PUT, DELETE, POST, OPTIONS
Access-Control-Allow-Headers: Authorization, Space-Key, Content-Type, If-Match, If-None-Match
Access-Control-Max-Age: 86400
```

- `*` is safe because there are no cookies.
- Without `Expose-Headers`, browser code can't read `ETag`, `Retry-After`, `KV-Time` and `KV-By` cross-origin.
- Plain reads send no custom headers, so they need no preflight at all. Clients should keep it that way: browsers cache a preflight per URL, and every watch request has a new `version`, so a token on a public read costs an extra round trip each time.

### Security headers on stored values

```
X-Content-Type-Options: nosniff
Content-Security-Policy: default-src 'none'; sandbox
Cache-Control: no-store
```

These stop a browser from treating a stored value as a page or script, even when someone opens its URL directly.

### Transport and logging

- Run behind TLS, for example with Caddy in front, which also gives HTTP/2.
- The client IP comes from `X-Forwarded-For` only if the request came from `trusted_proxies`.
- Log method, path, status, duration, IP and a short hash of the visitor ID. Never log tokens, `Space-Key`, the `token` query parameter or bodies.
- `/kv.js` is cached for an hour. Everything under `/v1` is `no-store`.

## 16. JS client

Optional. Served at `/kv.js` as an ES module with no dependencies. Every method maps to the HTTP calls above.

### Setup

```js
import { kv, newName, validSpace, KvError } from "https://kv.example.com/kv.js"

const db = kv("poll-cedar-pilot-mango-frost")
const admin = kv("poll-cedar-pilot-mango-frost", { key: "sk_8Hq3vT..." })   // can write shared/ when locked

newName("poll")        // "poll-slate-hinge-cocoa-pilot", local, no request
validSpace(fromLink)   // true if a string from a link or a form is a usable space name
```

These four are the module's only exports. There is no default export.

| Option | Default | Purpose |
|---|---|---|
| `key` | none | space key, sent as `Space-Key` on writes to `shared/` |
| `token` | none | an issued token. Used instead of the stored one, sent on every request including reads, and never saved |
| `server` | origin of the `kv.js` URL | API server |
| `tokenKey` | `"cubby:" + server` | localStorage key for the token and ID |
| `fetch` | global `fetch` | for tests or other runtimes |

- `kv()` makes no request, and throws at once for an invalid name, with the server's `suggestion`.
- Several `kv()` instances for different spaces share one token and visitor.
- A page that only reads `shared/` and `users/` never gets a token.
- Reads of `shared/` and `users/` never send the token, even when the visitor has one, so they stay free of CORS preflights. Writes and everything in `me/` send it. A visitor's token stays alive through those, so one that only reads for a year expires with its data.

### Areas

```ts
db.shared: Area
db.me: Area
db.user(id?: string): Area      // no id: your own; another visitor's id: writes throw read_only
db.users: { list(target, opts?), watch(target, onChange, opts?) }    // across all visitors
db.id(): Promise<string>        // your visitor ID, gets a token if needed
```

### Area methods

```ts
get(key): Promise<any | null>
getEntry(key): Promise<Entry | null>                                // without key and user
set(key, value, opts?: { ifVersion?: number }): Promise<number>
add(target, value, opts?: { t?: number }): Promise<{ version, t, keys? }>
series(key, opts?: { since?: number, limit?: number }): Promise<{ key, points: Point[], version, next } | null>
delete(key, opts?: { ifVersion?: number }): Promise<boolean>
update(key, fn: (current: any | null) => any, opts?: { retries?: number }): Promise<any>
list(target, opts?: { limit?: number, order?: "asc" | "desc" }): Promise<List>
watch(target, onChange: (change: Change) => void, opts?: WatchOptions): () => void

type Entry  = { key, value, version, by, mine: boolean, user?, t? }
type Change = Entry & { deleted: boolean }
type List   = { items: Entry[], version: number }
type Point  = { t, value }
type WatchOptions = {
  from?: 0 | "now" | number | List                                  // default 0
  onSync?: (sync: { version: number, initial: boolean, resync: boolean }) => void
  onError?: (err: KvError) => void                                   // default console.warn
}
```

`target` is what goes in the URL: an exact key like `"title"` or a pattern like `"msg.*"`.

### Behavior

- **`get`** parses JSON values and returns text as strings, `null` for a missing key. `getEntry` tells a missing key from a stored `null`.
- **`set`** always sends `JSON.stringify(value)`. Values that can't be serialized throw before any request.
- **Local checks.** Writing to someone else's `db.user(id)`, or to `db.shared` of a space the client knows is locked without a `key`, throws without a request. The client learns a space is locked from its first 403 `space_locked`.
- **`delete`** returns `false` for a missing key.
- **`add`** with a key, like `add("temp", 21.4)`, adds one point and sends `JSON.stringify(value)`, so an object is one point. With a pattern, like `add("env.*", { temp: 21.4, rh: 45 })`, `value` must be a plain object and each property becomes a point in its own series. The target decides, never the shape of the value.
- **`series`** makes one request and returns the response as is, or `null` if the key doesn't exist. To read further, pass `next` back as `since`.
- **`getEntry`** on a series key also returns `t`, from `KV-Time`.
- **`update`** reads, calls `fn`, and writes with `ifVersion`. On 412 it retries after 50 to 200 ms, up to `retries` (default 10), then throws `version_conflict`. If `fn` returns `undefined`, or a value whose `JSON.stringify` equals the current one, nothing is written. `fn` may run more than once, so it must not have side effects.
- **`list`** follows `next` until `limit` or the end. The returned `version` is from the first page.
- **`by` and `mine`.** Every entry and change has `by`, the visitor who last wrote or deleted it, and `mine`, true when that's this visitor. A watch delivers your own writes too; `mine` tells them apart.
- **`watch`**:
  - `from` says where to start. `0`, the default, delivers every existing key first, then changes. `"now"` delivers changes only. A `list()` result continues right after that list, so a page can show the list first and then follow it. A number continues after that version.
  - `onChange` gets one call per changed key. `onSync` follows once all changes from one answer have been delivered. At that point the page's state matches the server as of `version`, so it's the place to render. It also fires after each empty answer, so a working watch syncs at least every 25 seconds. `initial` is true on the first one: with `from: 0`, every existing key has arrived. After `onError`, the next `onSync` means the watch works again.
  - On 410 (version too old or from elsewhere, for example after a week asleep) a watch with state starts again from `0` and reports keys it knew about that are missing from the new snapshot as deleted. It knows the keys it has delivered, plus the items of a `list()` it started from. Starting from a bare number it knows none at first, so the app should reload its state when `resync` is true. A `"now"` watch skips ahead to the present instead of replaying everything. In both cases the next `onSync` has `resync: true`.
  - Errors with `retryable` true (network, 5xx, 429) go to `onError`, then the watch tries again after 1, 2, 4 ... up to 30 seconds, or after `retryAfter` for a 429. Any other error goes to `onError` once and ends the watch.
  - A request that gets no answer in 35 seconds counts as a network error. The server answers within 25.
  - When the page becomes visible, comes back online or is restored from the back/forward cache, the watch drops its current request or wait and asks again at once. A phone that slept may have a dead connection that never fails on its own.
  - After the tab has been hidden for 5 minutes it pauses, and resumes from its last version when visible.
  - The returned function stops the watch and removes its event listeners.
- **Auth.** The first call that needs a token, a write or anything in `me/`, gets one. Concurrent calls share that request. On 401 `invalid_token` it gets a new token and ID and retries once.
- **Given token.** With the `token` option the client never calls `/v1/anon`. A 401 throws `missing_token` or `invalid_token` without a retry. Without the option, a closed server makes reads throw 401 `missing_token` and the first write throw 403 `server_closed`.
- **Storage fallback.** If localStorage throws, the token lives in memory and the visitor is new on every page load. The client warns once.

### Errors

```ts
class KvError extends Error {
  status: number      // HTTP status, 0 for network or local checks
  code: string        // the `error` code, "network_error", or a local one like "read_only"
  details: object     // the rest of the error body
  retryable: boolean  // network error, 5xx or 429: worth trying again later
}
```

`retryable` is what an offline write queue checks: keep the write and try again if true, drop it and tell the user if false.

## 17. CLI

The same binary that runs the server is also a command-line client. It's the main way to use the store from shell scripts, cron jobs and terminals. Raw HTTP stays the contract underneath, for anything that can't run the binary: browsers, microcontrollers, other languages, locked-down machines.

```
cubby serve [-p PORT]           run the server, port 28229 by default
cubby config [--defaults]       print the server config as commented TOML: what serve would use, or the defaults
cubby admin ...                 administer the local server's data, see Admin
cubby <command> ...             everything else is a client talking HTTP to a server
```

### Paths

Commands take paths that mirror the URL after `/v1/`:

```
poll-cedar-pilot-mango-frost/shared/question
poll-cedar-pilot-mango-frost/users/u_k7m2x9q4p1r8t3w6/vote
poll-cedar-pilot-mango-frost/users/*/vote
poll-cedar-pilot-mango-frost/users/me/vote          CLI shorthand for your own visitor ID
poll-cedar-pilot-mango-frost/me/draft
poll-cedar-pilot-mango-frost/shared/msg.*
```

`users/me/` exists only in the CLI. It's replaced with your visitor ID before the request, and getting the ID may fetch a token.

Paths containing `*` must be quoted, as in `cubby ls 'poll-cedar-pilot-mango-frost/users/*/vote'`. Otherwise the shell tries to expand them as file names, and zsh stops with "no matches found".

### Configuration

| Setting | Flag | Environment | Config file |
|---|---|---|---|
| Server URL | `--server` | `CUBBY_SERVER` | `server` |
| Token | `--token` | `CUBBY_TOKEN` | saved automatically |
| Space key | `--space-key` | `CUBBY_SPACE_KEY` | saved by `new --locked`, per space |

The server URL defaults to `http://localhost:28229`. A bare port (`9000` or `:9000`) means `http://localhost:9000`, and a host without a scheme gets `http://`.

- Flags win over environment variables, which win over the config file.
- The config directory is `$XDG_CONFIG_HOME/cubby/`, by default `~/.config/cubby/`.
  - `config.toml` holds settings like `server`.
  - `credentials.toml` holds the token and visitor ID per server, and space keys per space. It's created with mode 0600.
- The first command that needs a token gets one with `POST /v1/anon` and saves it. Scripts never handle tokens unless they want to.
- `CUBBY_TOKEN` gives a script a fixed identity, for example a cron job that should always be the same visitor. A token set this way is never saved.

### Commands

| Command | Does | Prints |
|---|---|---|
| `get <path>` | read a value | the raw value, byte for byte, with no newline added |
| `set <path> [value]` | write a value, from the argument or from stdin if it's missing or `-` | the new version |
| `del <path>` | delete a value | the version of the delete |
| `ls <path>` | list a key or pattern | one line per item, see Output |
| `watch <path>` | wait for changes, until stopped | one line per change, see Output |
| `update <path>` | read, change, write back with `ifVersion`, retry on conflict | the new version |
| `add <path> [value]` | add a point to a series, from the argument or stdin | the point's time |
| `add <pattern> name=value ...` | add one point to each of several series, see [Series from the CLI](#series-from-the-cli) | the points' time |
| `series <path>` | read a series' points | one line per point, see Output |
| `new [label or name]` | create a space with `POST /v1/spaces`. A full space name is created as is, anything else is a label | the name, and the key on a second line if locked |
| `name [label]` | generate a valid space name locally, no request | the name |
| `whoami` | your visitor ID | the ID |
| `token` | your token, for use with `curl` | the token |
| `login <token>` | check an issued token with `whoami` and save it for this server, see [Using an issued token](#using-an-issued-token) | the visitor ID |
| `url <path>` | the full URL for a path, for `curl` or a browser | the URL |

Flags:

| Flag | Commands | Meaning |
|---|---|---|
| `--if-version N` | `set`, `del` | conditional write, `ifVersion=N` |
| `--create` | `set` | only if the key doesn't exist, `ifVersion=0` |
| `-f`, `--force` | `del` | a missing key isn't an error |
| `--json` | `get`, `ls`, `watch` | print full JSON instead of the plain format |
| `--limit N`, `--desc` | `ls` | as in the list endpoint |
| `--limit N` | `series` | as in the series endpoint |
| `--since T` | `series` | milliseconds, or a duration back from now like `90m`, `24h` or `7d` |
| `--time T` | `add` | the point's time in milliseconds, `?t=` |
| `--from N\|now` | `watch` | where to start. Default `0`: current state first, then changes |
| `--state FILE` | `watch` | save the version after every response and resume from it on the next start. If the file exists, it wins over `--from` |
| `--once` | `watch` | exit after the first response that has changes |
| `--raw` | `watch` | for one key: print each new value raw, followed by a newline. Values that contain newlines are ambiguous this way, so use `--json` or `--exec` for those |
| `--exec CMD` | `watch`, `update` | see below |
| `--label L`, `--locked` | `new` | as in `POST /v1/spaces` |
| `--no-save` | `new` | don't save the space key in `credentials.toml` |

### Series from the CLI

```
cubby add home-cedar-pilot-mango-frost/shared/temp 21.4
cubby add 'home-cedar-pilot-mango-frost/shared/env.*' temp=21.4 rh=45 door=open
echo '{"temp": 21.4, "rh": 45}' | cubby add 'home-cedar-pilot-mango-frost/shared/env.*'
cubby series home-cedar-pilot-mango-frost/shared/env.temp --since 24h
```

- With a key, the value is used as given, like `set`. `21.4` is stored as a JSON number and `open` as text.
- With a pattern, each `name=value` is one member of the batch. The value is used as JSON if it parses, otherwise as a JSON string, so `door=open` sends `"open"` and `door='"21"'` sends a string. Without arguments, the CLI reads one JSON object from stdin and sends it as the body.

### Output

`ls` and `watch` print one line per item, tab-separated, so `cut`, `awk` and `while read` work:

```
key<TAB>value
user<TAB>key<TAB>value        under users/*/
```

- The value is written as compact JSON. Text values become JSON strings, so a value with newlines still fits on one line.
- In `watch`, a deleted key prints `-` instead of a value.
- `series` prints `t<TAB>value` per point, oldest first, and follows `next` until `--limit` or the end. Without `--since` it prints the newest 1000.
- `--json` prints one JSON object per line instead, with every field from the API.

### Running commands on changes

`watch --exec CMD` runs `CMD` through `sh -c` once per change, one at a time, in version order:

- the value on stdin, raw, or empty for a delete
- `KV_KEY`, `KV_VERSION`, `KV_DELETED` (`0` or `1`) and, under `users/*/`, `KV_USER` in the environment
- `KV_TIME`, the point's time, when the key is a series

If the command fails, the CLI prints a warning and carries on. With `--state`, the version is saved only after every command for that response has run, so a crash means repeating changes, never skipping them.

`update --exec CMD` pipes the current value into `CMD` (the text `null` if the key is missing) and writes whatever it prints. On a version conflict it reads again and runs `CMD` again. That makes `jq` a natural partner:

```
cubby update homelab-maple-quilt-tundra-cobalt/shared/deploy-count --exec 'jq "(. // 0) + 1"'
```

Without `--exec`, `update` opens the value in `$EDITOR`, and on a conflict reopens it with the newer value.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | not found |
| 2 | version conflict |
| 3 | not allowed: `space_locked`, `not_owner`, `out_of_scope`, `server_closed`, token problems that a new token didn't fix |
| 4 | a limit: rate limit or quota |
| 5 | invalid input: a 400 from the server, a bad path, or the wrong kind of key (`is_series`, `not_series`) |
| 6 | server unreachable or server error |
| 64 | wrong command-line usage |

Errors go to stderr as `cubby: <error code>: <message>`, so scripts can branch on the exit code without parsing anything.

Rate limits are retried automatically when `Retry-After` is 30 seconds or less, up to three times. `watch` retries network errors forever with backoff, like the JS client.

### Admin

`cubby admin` works on the server's own data and runs on the server machine:

```
cubby admin spaces                 list spaces with size, keys, last activity
cubby admin export <space>         dump all keys and series points as JSON lines
cubby admin delete <space>
cubby admin rekey <space>          lock a space or replace its key, print the new key
cubby admin stats                  totals, visitors, largest spaces
cubby admin token ...              issue and manage tokens, see Managing tokens
```

- If the server is running, admin commands talk to it through a Unix socket in the data directory, readable only by the server's user. The server does the work, so files are never changed under it.
- If the server isn't running, they open the data files directly.
- There is no admin HTTP API.
- `rekey` on an open space locks it. It's the operator's tool, so the concern about locking someone else's space doesn't apply.

## 18. Implementation notes

Guidance for the build, not part of the contract.

### Language and libraries

Go.

- `modernc.org/sqlite`, a pure-Go SQLite, so `CGO_ENABLED=0` builds a fully static binary.
- Cross-compiling is one `GOOS`/`GOARCH` pair per target. Release builds for Linux, macOS and Windows on amd64 and arm64, plus Linux arm (for Raspberry Pi and similar).
- `net/http` from the standard library, using its pattern routing, for both the server and the CLI's client. No web framework.
- `embed` puts `kv.js` into the binary.
- A goroutine per waiting watch is cheap enough for the 1000-watch limit.
- The binary is expected to be around 15 MB, mostly SQLite. Client machines carry the server code too, which doesn't matter in practice.

### Files

```
data/
├── visitors.db          visitors, token hashes, last seen, which spaces each visitor has data in, issued tokens
├── admin.sock           Unix socket for `cubby admin`, only accessible to the server's user
└── spaces/{name}.db     one SQLite file per space
```

Space files are opened on demand. See [In-memory state](#in-memory-state) for how many stay open.

### Space schema

```sql
PRAGMA journal_mode = WAL;
PRAGMA auto_vacuum = INCREMENTAL;     -- before the first table
PRAGMA journal_size_limit = 4194304;

CREATE TABLE meta (
  k TEXT PRIMARY KEY,                 -- seq, created_at, last_write_at, tombstone_floor
  v                                   -- also key_hash for locked spaces
);

CREATE TABLE kv (
  area    TEXT NOT NULL,              -- 's' shared, 'u:{visitorId}', 'm:{visitorId}'
  key     TEXT NOT NULL,
  value   BLOB,                       -- NULL means tombstone
  is_json INTEGER NOT NULL,
  version INTEGER NOT NULL,
  writer  TEXT NOT NULL,              -- visitor ID of the last writer
  size    INTEGER NOT NULL,
  series  INTEGER NOT NULL DEFAULT 0, -- 1 for a series key
  t       INTEGER,                    -- newest point's time, series only
  n       INTEGER NOT NULL DEFAULT 0, -- number of points, so trimming needs no count
  PRIMARY KEY (area, key)
) WITHOUT ROWID;

CREATE INDEX kv_by_version ON kv (area, version);
CREATE INDEX kv_users ON kv (key, area) WHERE area >= 'u:' AND area < 'u;';

CREATE TABLE usage (writer TEXT PRIMARY KEY, bytes INTEGER NOT NULL);

CREATE TABLE points (
  area    TEXT NOT NULL,
  key     TEXT NOT NULL,
  t       INTEGER NOT NULL,           -- ms
  value   BLOB NOT NULL,
  is_json INTEGER NOT NULL,
  PRIMARY KEY (area, key, t)
) WITHOUT ROWID;
```

- A write is one transaction: check `ifVersion`, raise `seq`, upsert, adjust `usage` for the old and new writer.
- Adding a point is one transaction too: insert into `points`, delete the oldest points beyond `max_series_points`, and if the point is the newest, raise `seq` and upsert the `kv` row with the point as value, `t` and `n`. Then check quotas on the net growth. A batch does this for every member in one transaction.
- Point sizes go into the space's `bytes` in `meta`, next to the `kv` rows. `usage` holds the series key as a plain key of its newest value, so points never count towards a visitor.
- Space files from before series get the new columns and table when they're opened.
- Reading points is `WHERE area = ? AND key = ? AND t > ? ORDER BY t LIMIT ?` with `since`, or `ORDER BY t DESC LIMIT ?` reversed without it. Both use the primary key.
- Deleting a series key deletes its `points` rows in the same transaction.
- Visitor totals across spaces are the sum of their `usage` rows. See [In-memory state](#in-memory-state).
- `meta` also holds `last_active_at`, and `visitors.db` holds each visitor's `last_seen_at` and whether they've ever stored anything.

### Issued tokens

One more table in `visitors.db`, next to `visitors`:

```sql
CREATE TABLE issued (
  visitor     TEXT PRIMARY KEY,       -- the visitor this token belongs to
  name        TEXT NOT NULL UNIQUE,
  scopes      TEXT NOT NULL,          -- JSON array of scope strings, as given
  valid_until INTEGER,                -- ms, NULL for no end
  created_at  INTEGER NOT NULL
) WITHOUT ROWID;
```

- The token hash stays in `visitors`, so a token lookup is the same query for both kinds. A row in `issued` makes it an issued token.
- `rotate` replaces the hash in `visitors`. `revoke` deletes the `issued` row and clears the hash, leaving the visitor for the cleanup job.
- Parse scopes once per lookup and keep them with the cached visitor. The check per request is a few string prefix comparisons.
- In a closed server, the lookup rejects visitors without an `issued` row as unknown.
- The scope check runs after the path is parsed and the token found, and before anything touches a space file.

### Watch queries

```sql
-- one area
SELECT key, value, is_json, version FROM kv
WHERE area = ? AND version > ? AND key >= ? AND key < ?
ORDER BY version LIMIT 1001;

-- users/*/
SELECT area, key, value, is_json, version FROM kv
WHERE area >= 'u:' AND area < 'u;' AND version > ? AND key >= ? AND key < ?
ORDER BY version LIMIT 1001;
```

Each space has an in-memory notifier. Writers signal it after committing, and waiting watches run their query again. A version-0 snapshot excludes tombstones.

### Cleanup job

Hourly:

1. Write pending "last seen" and "last active" times from memory to disk.
2. Delete visitors unseen past `visitor_idle_ttl`, or past `empty_visitor_idle_ttl` if they never stored anything, with their `u:` and `m:` rows and points in every space they used. Skip visitors with an active issued token. For an expired one, count from its end instead of its last seen time.
3. Delete tombstones older than `tombstone_ttl` and update `tombstone_floor`.
4. Delete space files without activity past `space_idle_ttl`, unless pinned.
5. Drop idle rate-limit buckets and cache entries, as described below.
6. Run `PRAGMA incremental_vacuum` and `PRAGMA wal_checkpoint(TRUNCATE)` on spaces changed since the last run.

### In-memory state

Everything held in memory has a bound, and nothing in it has to survive a restart.

| State | Bound | When it's dropped | Lost on restart means |
|---|---|---|---|
| Open space files | 64 without watchers, plus every space with a waiting watch (at most 1000) | least recently used first | files reopen on demand |
| Notifier per space | one per space with waiting watches | when the last watch on that space ends | watches reconnect and continue from their version |
| Waiting watches | 20 per IP, 1000 total | on answer, timeout or disconnect | clients reconnect |
| Rate-limit buckets, per IP and per visitor | 100,000 entries | by the hourly job once full and untouched for an hour, or least recently used when the cap is reached | everyone starts with a full bucket again |
| Storage used per visitor | 100,000 entries | least recently used | recounted from that visitor's `usage` rows on their next write |
| Pending "last seen" and "last active" times | one entry per active visitor and space | written to disk by the hourly job and on shutdown | up to an hour of activity isn't recorded, which only matters for retention measured in months |

- A bucket that's full and untouched behaves exactly like a new one, so dropping it changes nothing. Dropping one early under the cap is slightly lenient towards that IP or visitor, which is acceptable.
- Request bodies are read only up to the value limit plus one byte, so a single request can't use more than about 64 KB of body memory.

## 19. Later

Ideas from the design discussion that were left out to keep the first version small. Each is an addition, and none changes the API above.

### Abuse protection

- **Proof of work on `/v1/anon`.** The server answers 428 with a signed challenge and a difficulty. The client finds a nonce so that `SHA-256(challenge + nonce)` has that many leading zero bits, and posts it back. A real visitor pays well under a second once. Minting tokens in bulk costs CPU per token, independent of IPs. Switching it on is one config value, so the JS client should handle 428 from the start.
- **Closed mode and origin allowlists** in config: only pinned spaces writable, or only requests from listed `Origin`s accepted.

### Key options

- **`ttl`**: a key that deletes itself after N seconds, for presence and typing indicators without client-side filtering.
- **`lock=owner`** on shared keys: only the creator of a key may change it. Mostly covered by `users/`.

### Sharing and identity

- **Device linking.** Device A gets a short code that's valid for 10 minutes. Device B redeems it with its own token and becomes the same visitor. Brings `me/` along to other devices, and gives "your devices" and "sign out others".
- **Read-only links per room, without operator help.** Spaces whose ID is a hash of a write key, so a page can derive a read link from an edit link with no server state. Explored in depth and dropped because it needs hashing, fragment conventions and remembered keys in the client.
- **Visitor IDs that differ per space**, derived with an HMAC, so a visitor can't be linked across spaces.

### Closed servers

- **Invite codes.** `cubby admin token invite --allow ... --ttl 10m` prints a short one-time code or a link like `https://kv.example.com/#invite=K7M2-X9Q4`. A client redeems it with `POST /v1/enroll` and gets its own issued token. Saves pasting 46-character tokens into browsers and devices.
- **Signed requests** for devices that can't do TLS. `Authorization: KV-HMAC id=..., t=..., sig=...`, signing method, path, time and body hash with a per-token secret. The secret never crosses the network and captured requests can't be replayed outside a short window. Values are still readable in transit.
- **Per-token quotas and rate limits**, for a trusted device that writes more than an anonymous visitor may.
- **Closed per space**, as a third kind next to open and locked, so one server can hold public and private spaces.

### More areas

| Area | Rule | Unlocks |
|---|---|---|
| inbox | anyone adds, only the owner reads | contact forms, RSVPs, private survey answers |
| sealed | writes hidden until a reveal, then shown together | planning poker, sealed bids, quiz answers |
| tally | one value per visitor, readers see only counts | anonymous polls, ratings |

### Series

- **Reading several series in one request**, `GET env.*/series`, returning `{"items": [{"key": "env.temp", "points": [...]}], "version": ..., "next": ...}` in the shape of a list. Also `users/*/{key}/series`.
- **Downsampling with `?step=1h`**, returning the average, minimum, maximum and count per bucket for points that are JSON numbers. The only feature that would make the server calculate, so it waits until a page actually can't cope with raw points.
- **Time-based retention per series**, as an alternative to the point count.

### Text responses for shells

`?format=text` on every route that answers with JSON returns tab-separated lines instead, for shells, `awk` and devices without `jq`.

**Hard rule: this is exactly the CLI's output format.** `cubby ls`, `watch` and `series` print the same bytes the server would send with `?format=text`. Building this means changing the CLI's current output to match: today it prints text values as quoted JSON strings. There must never be two slightly different tab formats.

```
$ curl -s "$S/env.*?format=text"
env.rh	45
env.temp	21.4
#version	1791449600051

$ curl -s "$S/users/*/vote?format=text"
u_k7m2x9q4p1r8t3w6	vote	tacos
u_p1r8t3w6k7m2x9q4	vote	pizza
#version	1791449600057

$ curl -s "$S/env.*/watch?version=1791449600051&format=text"
env.temp	21.5
env.door
#version	1791449600053

$ curl -s "$S/env.temp/series?since=1791440000000&format=text"
1791440000500	21.3
1791440060500	21.4
#next	1791440060500
#version	1791449600058
```

- Fixed columns per route, value always last: `key value`, `user key value`, `t value`. `read -r k v` puts everything after the first tab into `v`.
- A line with a key and no value is a deleted key. Values can never be empty, so this can't clash with a real value.
- Lines starting with `#` are metadata, as `#name<TAB>value`: `#version`, `#next`, `#more`. Keys can't contain `#`, so these never look like data, and `awk` and gnuplot skip them as comments.
- Values are unwrapped for the shell. A JSON string `"open"` prints as `open`, numbers print bare, objects and arrays print as compact one-line JSON, and text prints as it is.
- Inside a value, newline is `\n`, tab is `\t` and backslash is `\\`, as in Postgres `COPY`. `printf '%b'` decodes them.
- Single-key `GET` already returns the raw value and doesn't change. Write responses could be `#version` and `#t` lines.
- Lossy on purpose: the number `21` and the text `21` look the same, and per-key versions and `t` are left out. Anything that needs them, such as `ifVersion` writes, uses JSON.

A complete watch loop in POSIX `sh`, with no `jq`:

```sh
tab=$(printf '\t'); v=
while :; do
  curl -sf "$S/deploy/watch?format=text${v:+&version=$v}" > /tmp/w || { sleep 5; v=; continue; }
  while IFS=$tab read -r k val; do
    case $k in
      '#version') v=$val ;;
      '#'*)       ;;
      *)          [ -n "$val" ] && /usr/local/bin/deploy-ref "$val" ;;
    esac
  done < /tmp/w
done
```

Who it's for: machines where the binary can't run but `curl` or `wget` can, such as OpenWrt routers and BusyBox systems. Build it when one of those actually needs it.

### Smaller additions

- Append with a server-chosen key, so clients don't build `msg.{time}.{random}` themselves.
- Watching several targets in one request.
- Batch reads of several specific keys.
