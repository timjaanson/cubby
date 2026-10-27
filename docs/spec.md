# cubby specification

The detailed contract for the server, its built-in CLI and the optional JS client. [design.md](design.md) has the overview. Where they disagree, this file wins.

"Must" means a client or server that doesn't do it is broken. "Should" means a strong default with room for a reason not to.

Contents:

1. [Conventions](#1-conventions)
2. [Names](#2-names)
3. [Visitors](#3-visitors)
4. [Spaces and access](#4-spaces-and-access)
5. [Access tokens and grants](#5-access-tokens-and-grants)
6. [Managing a space](#6-managing-a-space)
7. [Values](#7-values)
8. [Versions](#8-versions)
9. [Endpoints](#9-endpoints)
10. [Watching](#10-watching)
11. [Series](#11-series)
12. [Pulses](#12-pulses)
13. [Limits and quotas](#13-limits-and-quotas)
14. [Server config and lifecycle](#14-server-config-and-lifecycle)
15. [Errors](#15-errors)
16. [HTTP details](#16-http-details)
17. [JS client](#17-js-client)
18. [CLI](#18-cli)
19. [Implementation notes](#19-implementation-notes)
20. [Later](#20-later)

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

- Anyone using the same name uses the same space. In a space with `read: anyone`, the name is the only thing between a stranger and its `shared/` and `users/` data, so how hard it is to guess matters. A private space also needs a grant, see [Private spaces](#private-spaces).
- The server only checks length and characters. A hand-picked name like `team-lunch-dashboard` is valid and as easy to guess as it looks. For privacy, let cubby generate the name.
- A space exists only once someone creates it with [`POST /v1/spaces`](#create-a-space). Writing to a name never creates it.
- Words like `anon`, `spaces` or `whoami` can never be valid names, so system routes can't collide with spaces.
- An invalid name returns 400 `invalid_space` with a `suggestion`: a valid name built from it, such as `poll-slate-hinge-cocoa-pilot` for `poll`.

Getting a name:

- **Generate it.** Four words drawn from a fixed list of 4096 short English words (3 to 6 letters), joined by hyphens. That's 48 random bits, about 2.8 × 10^14 names. A label goes in front: `poll-cedar-pilot-mango-frost`. A label is at most 35 characters, so a labeled name always fits in 63. The JS client exports `newName("poll")`, the CLI has `cubby name poll`, the server uses the same list. Pass the name to `POST /v1/spaces` to create it.
- **Let the server generate it** when creating the space, with an optional label, see [Create a space](#create-a-space).

### Visitor ID

`u_` followed by 16 random lowercase base32 characters, for example `u_k7m2x9q4p1r8t3w6`. The same ID in every space.

### Key

```
^[A-Za-z0-9._:-]{1,128}$
```

- No slashes, so a key is always one path segment. Slashes and `%2F` return 400 `invalid_key`.
- Case-sensitive.
- `watch`, `series` and `pulse` aren't keys. They're the action words that can follow a key (see [Paths](#paths)), and a key with the same name would make URLs like `/shared/series/series` hard to read. Any later action word is reserved the same way.
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
| `GET env.*/series` | none | 400 `invalid_key`. Series are read one key at a time for now, see [Later](#20-later) |
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
- `{action}` is left out, `watch`, `series` or `pulse`. Without it, the request is about the target's current value.
- Nothing comes after the action.

Besides data URLs, each space has a few routes of its own. Their names aren't areas, so they can't collide with data:

| Route | For | Section |
|---|---|---|
| `PATCH`, `DELETE /v1/{space}` | settings, deleting the space | [Managing a space](#6-managing-a-space) |
| `GET /v1/{space}/access` | what the requesting visitor may do | [What can I do here](#what-can-i-do-here) |
| `POST /v1/{space}/join` | redeem an access or owner token | [Redeeming: join](#redeeming-join) |
| `/v1/{space}/tokens[/{id}]` | mint, list, change and revoke access tokens | [Minting](#minting), [Managing tokens](#managing-tokens) |
| `/v1/{space}/grants[/{id}]` | list, add and revoke grants | [Managing grants](#managing-grants) |
| `POST /v1/{space}/owner-token` | rotate the owner token | [Owner token](#owner-token) |

The method and the action together say what happens:

| | no action | `/watch` | `/series` | `/pulse` |
|---|---|---|---|---|
| `GET` | read a key, or list a pattern | wait for changes | read a series' points | wait for [pulses](#12-pulses) |
| `PUT` | replace a value | 405 | 405 | 405 |
| `POST` | 405 | 405 | add a point, or one point to each of several series with a pattern | send a pulse |
| `DELETE` | delete a key and its points | 405 | 405 | 405 |

A 405 names the methods that work and, where it helps, the route you probably meant. `POST /shared/temp` answers "to add a point use /temp/series, to replace the value use PUT".

## 3. Visitors

A visitor is an anonymous identity: one browser, one script, one device. Everything a request is allowed to do follows from who the visitor is and which grants they hold in the space, see [Access](#4-spaces-and-access).

### Getting a visitor token

```
POST /v1/anon
```

```
201
{"token": "vt_4Jx9...", "id": "u_k7m2x9q4p1r8t3w6"}
```

- A visitor token is `vt_` plus 32 random bytes in base64url. It belongs to one visitor. The server stores only its SHA-256 hash.
- The response includes the visitor's ID, so a client needs no second request to know who it is.
- Two other routes also create a visitor when called without a token: [`POST /v1/spaces`](#create-a-space) and [`POST /v1/{space}/join`](#redeeming-join). A page that starts by creating a space or redeeming an invite never needs `/v1/anon`.
- Creating a visitor, by any of the three routes, is rate limited by IP: 30 per hour with a burst of 30. IPv6 addresses are grouped by /64.

### Sending it

```
Authorization: Bearer vt_4Jx9...
```

or `?token=vt_4Jx9...` where a header can't be set, such as a browser tab, or where a header would cost a CORS preflight on every watch. The header wins if both are given. The scheme name `Bearer` is case-insensitive. A token that doesn't have the shape `vt_` plus 43 base64url characters is treated as unknown without a lookup. The server must not log query strings containing `token`.

**This is the only credential ever sent on a request.** Access tokens and owner tokens are redeemed once with `join` and never travel again.

### When a visitor token is needed

| Request | Visitor token |
|---|---|
| any write or delete | yes |
| anything in `me/` | yes |
| anything in a space with `read: granted` | yes |
| owner routes, `GET /v1/whoami` | yes |
| reads of `shared/` and `users/` in a space with `read: anyone` | no |
| `GET /v1/{space}/access` | no |
| `POST /v1/anon`, `POST /v1/spaces`, `POST /v1/{space}/join` | no, one is created if missing |

- A token sent with a read that doesn't need one is still used for "last seen". If it's unknown or expired, it's ignored.
- Where a token is needed, a missing one returns 401 `missing_token` and an unknown or expired one returns 401 `invalid_token`. The exception is a space the request can't read, which answers as if it didn't exist, see [Private spaces](#private-spaces).

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

- A visitor counts as seen whenever a request carries their valid token, reads included.
- After 365 days unseen (`visitor_idle_ttl`), a cleanup job deletes the visitor, their token, their grants, and their `users/` and `me/` data in every space.
- Visitors who never stored anything and hold no grant are deleted after 30 days unseen (`empty_visitor_idle_ttl`).
- `visitor_idle_ttl` should stay longer than `space_idle_ttl`. Otherwise a visitor's votes or grants could vanish from a space that's still in use.
- Shared keys they wrote stay. Their bytes stop counting against any visitor and keep counting against the space.
- A client that gets 401 `invalid_token` must drop the token, get a new one and retry once. The new token is a new visitor with a new ID and no grants, so in a space that needs grants the retry fails with `no_access` or `no_space`, and the page should ask for a new invite.

## 4. Spaces and access

### Areas

| Area | Path | Holds |
|---|---|---|
| shared | `/v1/{space}/shared/{key}` | the space's common data |
| a visitor | `/v1/{space}/users/{id}/{key}` | one visitor's public data. Only that visitor writes it |
| all visitors | `/v1/{space}/users/*/{key or pattern}` | the same key across every visitor. Read-only |
| private | `/v1/{space}/me/{key}` | the requesting visitor's private data. Nobody else can address it |

All areas support read, write, delete, list, watch and series, except that `users/*/` is read-only and doesn't read series yet (see [Later](#20-later)).

- **shared/** is for things the group works on together: lists, boards, documents, room settings, game state, a poll's question.
- **users/{id}/** is for things that belong to one visitor and everyone who can read the space may see: a vote, a display name, a score, messages they wrote. Writing or deleting under another visitor's ID returns 403 `not_yours`, whatever their grants. Only the space's owner may delete there, as moderation. The data goes away with the visitor.
- **users/*/** reads one key, or one pattern, across every visitor. Each item includes `user`. Only `GET`, `HEAD` and watch; other methods return 405.
- **me/** is the visitor's private data in this space. Every request needs a visitor token, and no visitor, owner included, can read another's.

### Settings

Every space has two settings. They decide what a visitor may do without a grant. **A new space is private unless its creator says otherwise:** only its owners and visitors they invite can read or write it.

| Setting | Values | Default |
|---|---|---|
| `read` | `anyone`, `granted` | `granted` |
| `write` | per area: `{"shared": ..., "users": ..., "me": ...}`, each `anyone` or `granted` | `granted` for all three |

- `read` covers every area: `shared/`, `users/`, `users/*/` and `me/`. `granted` makes the space [private](#private-spaces).
- `write` covers `PUT`, `DELETE` and `POST` in that area. `"write": "granted"` is shorthand for all three areas `granted`, and `"write": "anyone"` for all three `anyone`.
- `read: granted` requires `write: granted` in every area. Anything else returns 400 `invalid_settings`, because an open write into an unreadable space would tell a stranger the space exists.
- The owner changes settings at any time with [`PATCH /v1/{space}`](#settings-and-deletion). Data and grants stay as they are.

### The public shorthand

Opening a space up has to be a decision someone makes and can see in the code. `public` names the four useful combinations in one word:

| `public` | `read` | `write` | For |
|---|---|---|---|
| `none` (default) | `granted` | `granted` | people who know each other: a family list, a team board, a journal, notes between friends, a game among friends |
| `read` | `anyone` | `granted` | publishing: status pages, schedules, sensor readings, script output |
| `respond` | `anyone` | `shared` granted, `users` and `me` anyone | one author or a few editors, everyone responds: polls, quizzes, RSVPs, sign-up sheets |
| `all` | `anyone` | `anyone` | anyone with the link reads and writes: a public chat room, a party playlist, a demo |

- `POST /v1/spaces` and `PATCH /v1/{space}` take either `public` or `read`/`write`, not both. Both together return 400 `invalid_settings`.
- Every response with settings has all three fields, `public` being the matching name or `custom`:

```json
{"public": "respond", "read": "anyone", "write": {"shared": "granted", "users": "anyone", "me": "anyone"}}
```

### The access rule

A request is allowed when any of these holds:

1. The space's setting for this kind of request is `anyone`.
2. The visitor holds a live grant in the space with a scope covering the request. See [Scopes](#scopes).
3. The visitor owns the space.

On top of that, and never overridden:

- Writes need a visitor token, so every entry has a `by`.
- `users/{id}/` is written only by visitor `{id}`. The owner may also delete there.
- `me/` is always the requesting visitor's own.

A write that isn't allowed returns 403 `no_access`. In a private space the visitor can't read, every request answers as if the space didn't exist.

### Private spaces

A space with `read: granted` is private, which is the default. To a request without a grant that covers reading, it doesn't exist:

- Every route answers exactly as for a space that doesn't exist, with the same status, body and headers. That includes reads, lists, watches, writes, `access` and owner routes:

```
404
{"error": "no_space", "message": "No such space, or it's private and this visitor hasn't joined it. Redeem an invite with POST /v1/{space}/join, or check the name."}
```

- The message names both causes because the server can't say which one it is. It has to, so the people and the coding agents reading it check both the name and the invite, instead of searching for a typo that isn't there.
- A failed `join` answers 404 `invite_invalid`, the same as for a space that doesn't exist.
- Such a request costs a miss under the [guessing limit](#guessing-space-names), like any request for a missing space.

So someone trying names learns nothing, whether the space exists or not. A visitor with a grant that covers only writing, like a sensor's `shared/env.*:w`, can write but gets `no_space` on reads.

### Owners

- Whoever creates a space owns it. Creating gives the creator an owner grant and returns an [owner token](#owner-token) once.
- An owner may do everything in the space: read and write every area as allowed for any visitor, delete in anyone's `users/{id}/`, change settings, mint and revoke access tokens, add and revoke grants, rotate the owner token and delete the space.
- A space can have several owners, through the owner token or a direct grant with `"owner": true`.
- Owning is a grant, so it ends like one: when it's revoked, or when the visitor is deleted after `visitor_idle_ttl`. The owner token is how an owner gets back in.

### Who wrote what

Every entry records `by`, the visitor who last wrote or deleted it. Owners act with their visitor identity like anyone else, so an owner's moderation delete shows the owner's ID.

## 5. Access tokens and grants

An access token is an invite. A grant is what redeeming it leaves behind: "visitor X has these scopes in this space". The token is used once per visitor and then no longer matters to that visitor. The grant is what every later request is checked against.

```
owner ── mints ──> access token at_... ── link, chat, QR ──> visitor redeems with join ──> grant on the visitor
```

### Minting

```
POST /v1/{space}/tokens
Authorization: Bearer vt_...           an owner of the space

{"label": "anna"}
```

```
201
{
  "id": "t_3k9f2m7q",
  "token": "at_Qz7r...",
  "label": "anna",
  "allow": ["*:rw"],
  "uses": 10,
  "until": 1792054400000,
  "grantFor": null
}
```

| Field | Means | Default |
|---|---|---|
| `label` | a note for the owner, at most 63 characters | none |
| `allow` | scopes the grant will have, see [Scopes](#scopes), or a preset name: `member` or `viewer` | `member`, i.e. `["*:rw"]` |
| `uses` | how many visitors can redeem it, 1 or more, or `null` for no limit | `10` |
| `until` | until when it can be redeemed: a duration like `7d`, a date, an RFC 3339 time, or `null` for no end | `7d` |
| `grantFor` | how long each grant lasts after its redemption: a duration, or `null` for until revoked | `null` |

- **The defaults make a token that cleans up after itself.** A link someone forgot in a chat stops working after a week or ten people, whichever comes first. Unlimited uses or no end have to be asked for with an explicit `null`.
- **Limits apply to redeeming only.** The token is the invite. How long someone stays is the grant's business: until revoked by default, or `grantFor` after they joined.
- The token is shown once. The server stores only its SHA-256 hash. It's `at_` plus 32 random bytes in base64url.
- `until` as a date means the end of that day in the server's time zone. A time without an offset is also in the server's time zone. The response always gives milliseconds.
- `uses` and `until` can be changed later, see [Managing tokens](#managing-tokens). `allow` and `grantFor` can't, so every grant matches the token it came from.

### Handing it out

Any channel works: a link, a chat message, a QR code, a config file. The convention for pages, used by the JS client, is a URL fragment:

```
https://example.com/poll/#t=at_Qz7r...
https://example.com/chat/#room=room-slate-hinge-cocoa-pilot&t=at_Qz7r...
```

- The fragment never reaches any server and isn't sent in `Referer`.
- After redeeming, the page removes `t` from the address bar with `history.replaceState`, so it isn't kept in history or passed on when someone copies the URL.
- Nobody keeps the access token after redeeming. If a person might need it again, on a new device or after clearing their browser, they keep the link.

A token written into a public page's source belongs to anyone who can load the page. That's the same as opening the space to everyone for those scopes, with the difference that the owner can revoke it. For "everyone may respond", use the space's `write` setting instead.

### Redeeming: join

```
POST /v1/{space}/join
Authorization: Bearer vt_...           optional

{"token": "at_Qz7r..."}
```

```
200                                    with a visitor token: the grant is added to that visitor
{"space": "poll-amber-lotus-quill-ridge", "id": "u_k7m2x9q4p1r8t3w6", "owner": false,
 "allow": ["*:rw"], "until": null,
 "settings": {"public": "respond", "read": "anyone", "write": {"shared": "granted", "users": "anyone", "me": "anyone"}}}

201                                    without one: a new visitor is created and gets the grant
{"token": "vt_8Rn2...", "id": "u_p1r8t3w6k7m2x9q4", ...same fields...}
```

- **Send the visitor token you have.** Without one, `join` creates a new visitor. A page that already has an identity and leaves it out splits one person into two: the new one has the grant, the old one has their `users/` data.
- **A client must save a returned `token` before doing anything else.** If the response is lost, so is the identity holding the grant, and with `uses: 1` the invite too.
- **Redeeming is idempotent per visitor.** Redeeming a token the visitor has already redeemed returns 200 with the existing grant and doesn't use up a use, even if the token has since run out. Reloading a page with the link still in it is harmless.
- Each redemption by a new visitor uses one use. The grant gets `until` = now + `grantFor`, or `null`.
- `allow`, `owner` and `until` in the response describe this grant. The visitor's full rights are in [`access`](#what-can-i-do-here).
- An unknown, expired, used-up or revoked token, or a space that doesn't exist, returns 404 `invite_invalid` and costs one miss under the [guessing limit](#guessing-space-names). The response doesn't say which, so a stranger can't tell a real space from a missing one.
- An [owner token](#owner-token) is redeemed the same way and gives an owner grant.

### Grants

- **A visitor can hold any number of grants in a space.** One per redeemed token, plus any [direct grants](#direct-grants) from an owner.
- **They add up.** A request is allowed if any live grant covers it. There are no deny rules, so no grant can take away what another gives, and order never matters.
- **Each lives on its own.** A grant has its source (a token ID, `direct`, `owner-token` or `create`), its own `until`, and a last-used time. Revoking one doesn't touch the others.
- **A grant ends** when its `until` passes, when it's revoked, when the token it came from is revoked, or when the visitor is deleted. An ended grant stops working at once. A watch already waiting is answered normally, and the next request fails.
- A grant never changes. To give someone different rights, add a grant or revoke one.

### Scopes

```
AREA[/TARGET]:RIGHTS
```

| Part | Values | If left out |
|---|---|---|
| `AREA` | `shared`, `users`, `me` or `*` | required |
| `TARGET` | a key or a pattern, see [Names](#2-names) | `*` |
| `RIGHTS` | `r`, `w` or `rw` | required |

Two presets can be given instead of a list, as `"allow": "member"`. Responses always show the scopes they stand for.

| Preset | Scopes | For |
|---|---|---|
| `member` (default) | `["*:rw"]` | someone who joins to take part: read, write, their own `users/` and `me/` |
| `viewer` | `["shared:r", "users:r"]` | someone who should only look, in a private space: a wall screen, a relative following a trip |

| Scope | Allows |
|---|---|
| `*:rw` | everything a visitor can do, in every area. A full member |
| `shared:r` | reading `shared/`, in a private space |
| `shared/*:rw` | reading and writing all of `shared/` |
| `shared/status.*:w` | writing keys starting with `status.` and nothing else, for a script |
| `shared/config:w` | writing exactly the key `config` |
| `users:w` | writing your own `users/{id}/` |
| `me:rw` | using your own `me/` |

Rules:

- `r` covers `GET` and `HEAD`: reading, listing, watching, reading series and listening for pulses. It only matters in a private space, since elsewhere reading is open.
- `w` covers `PUT`, `DELETE` and `POST`: writing, deleting, adding points and sending pulses. It doesn't include reading, but write responses still return versions and times, so conditional writes work.
- `users` covers both `users/{id}/` and `users/*/`. Writing is always only into your own ID.
- A key covers only itself. A pattern `p*` covers every key that starts with `p`, and every pattern `q*` where `q` starts with `p`. `env.*` covers `env.temp`, `env.*` and `env.t*`, but not `*` or `e*`.
- A request with a pattern needs one scope that covers the whole pattern. Scopes don't add up across keys: `env.temp:r` and `env.rh:r` together don't allow `GET env.*`. A batch `POST env.*/series` needs `w` on a scope covering `env.*`.
- A scope only adds what the [access rule](#the-access-rule) allows. It never lets anyone write another visitor's `users/{id}/` or read another's `me/`.
- An invalid scope returns 400 `invalid_scope` naming it, and nothing is created.

### Direct grants

An owner can grant rights to a visitor ID they already know, with no token involved. For example, promoting someone whose vote is visible in `users/*/` to editor:

```
POST /v1/{space}/grants
Authorization: Bearer vt_...           an owner

{"user": "u_p1r8t3w6k7m2x9q4", "allow": ["shared/*:rw"]}
```

```
201 {"id": "g_7d2kq9wm", "user": "u_p1r8t3w6k7m2x9q4", "owner": false, "allow": ["shared/*:rw"],
     "source": "direct", "until": null}
```

- `allow` defaults to `member`, and takes the same presets. `until` is optional, in the same forms as for tokens.
- `"owner": true` instead of `allow` makes the visitor an owner.
- The visitor must exist on the server, otherwise 404 `not_found`.

### Owner token

`POST /v1/spaces` returns an owner token, `ot_` plus 32 random bytes in base64url, once. It's the recovery key for the space:

- Redeeming it with `join` makes the redeeming visitor an owner. That's how an owner gets in from a new device, a cleared browser, or after their visitor expired.
- It has no use limit and no end.
- It must never be in a page. Keep it in a password manager.
- An owner rotates it with `POST /v1/{space}/owner-token`, which returns a new one and invalidates the old one at once. Rotating also removes every owner grant that came from the old token, except the caller's, because a rotation usually means the old one leaked. Owner grants from creating the space or from direct grants stay.
- If every owner is gone and the token is lost, the server operator can issue a new one with `cubby admin owner-token <space>`.

### What can I do here

```
GET /v1/{space}/access
Authorization: Bearer vt_...           optional
```

```
200
{
  "id": "u_k7m2x9q4p1r8t3w6",
  "owner": false,
  "settings": {"public": "respond", "read": "anyone", "write": {"shared": "granted", "users": "anyone", "me": "anyone"}},
  "grants": [
    {"id": "g_7d2kq9wm", "allow": ["shared/poll.*:rw"], "source": "t_3k9f2m7q", "until": null},
    {"id": "g_m4x8r2pt", "allow": ["shared/notes.*:rw"], "source": "t_9wq4n6zd", "until": 1794646400000}
  ]
}
```

- Without a visitor token, `id` is `null` and `grants` is empty. The answer then says what anyone may do.
- A page uses it to show or hide edit controls without waiting for a 403.
- In a private space without a readable grant, it returns 404 `no_space` like everything else.

## 6. Managing a space

Every route here needs a visitor token of an owner. Anyone else gets 403 `owner_only`, or 404 `no_space` if they can't read the space.

### Settings and deletion

```
PATCH  /v1/{space}        {"public": "read"}                     → 200 {"settings": {...}}
PATCH  /v1/{space}        {"write": {"users": "anyone"}}
DELETE /v1/{space}                                                → 200 {}
```

- `PATCH` takes either `public`, which replaces both settings, or `read` and `write`, both optional. `write` may be the shorthand string or an object with some areas. Areas left out keep their value. The result must be valid, see [Settings](#settings).
- `DELETE` deletes the space with all its data, tokens and grants. Waiting watches get 404 `no_space`. The name is free again afterwards.

### Managing tokens

```
GET    /v1/{space}/tokens
PATCH  /v1/{space}/tokens/{id}     {"uses": 20, "until": "30d"}
DELETE /v1/{space}/tokens/{id}
```

```
200
{"items": [
  {"id": "t_3k9f2m7q", "label": "anna", "allow": ["*:rw"], "usesLeft": 9, "until": 1792054400000,
   "grantFor": null, "state": "active", "grants": 1, "createdAt": 1791449600000, "lastRedeemedAt": 1791450000000}
]}
```

- Token values are never shown again. The list has what the owner needs to recognise them.
- `state` is `active`, `expired` or `used_up`. A token that can't be redeemed stays listed while any grant from it exists, and for 30 days after it ended.
- `PATCH` changes only `uses` (uses left from now) and `until`. It extends an invite or ends it early. `{"uses": 0}` stops new redemptions and keeps everyone who already joined.
- `DELETE` removes the token and every grant redeemed from it.

### Managing grants

```
GET    /v1/{space}/grants
GET    /v1/{space}/grants?user=u_p1r8t3w6k7m2x9q4
POST   /v1/{space}/grants                       see Direct grants
DELETE /v1/{space}/grants/{id}
DELETE /v1/{space}/grants?user=u_p1r8t3w6k7m2x9q4
```

```
200
{"items": [
  {"id": "g_7d2kq9wm", "user": "u_p1r8t3w6k7m2x9q4", "owner": false, "allow": ["*:rw"],
   "source": "t_3k9f2m7q", "until": null, "createdAt": 1791450000000, "lastUsedAt": 1791459600000}
]}
```

- Owner grants are in the list with `"owner": true`.
- `DELETE ...?user=` removes everything that visitor holds in the space, ownership included.
- An owner can remove their own owner grant. The owner token still works afterwards.

### Rotating the owner token

```
POST /v1/{space}/owner-token          → 200 {"owner": "ot_..."}
```

See [Owner token](#owner-token).

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
- Each key also records `by`, the ID of the visitor whose write or delete gave it that version. Reads, lists and watches return it, so a page can tell who changed `shared/` data and recognise its own writes coming back through a watch. Visitor IDs are already public under `users/*/`, so this reveals nothing new.
- Versions only grow and aren't contiguous per key. `0` means "doesn't exist".
- A new space's counter starts at the current Unix time in milliseconds. Clients must treat versions as opaque increasing numbers. They stay below 2^53, so they're exact in JavaScript.
- A new space has no keys but already has a counter. Lists and watches on it return the counter as `version`, so a client watching an empty space picks up the first write with no special case.
- The server must never start a counter below a version it has already handed out for that name, even if the clock goes backwards.
- A `PUT` with the same bytes as the current value keeps its version, writer and value timestamp, and does not notify watchers. It still applies the supplied TTL, or clears expiry when TTL is omitted.

## 9. Endpoints

`{area}` stands for `shared`, `users/{id}` or `me`.

### Create a space

```
POST /v1/spaces
Authorization: Bearer vt_...           optional

{"label": "poll", "public": "respond"}
```

```
201
{
  "space": "poll-cedar-pilot-mango-frost",
  "owner": "ot_8Hq3vT...",
  "id": "u_k7m2x9q4p1r8t3w6",
  "settings": {"public": "respond", "read": "anyone", "write": {"shared": "granted", "users": "anyone", "me": "anyone"}}
}
```

The body is optional. An empty body creates a private space with a generated name.

| Field | Means |
|---|---|
| `label` | generate a name with this label in front. At most 35 characters |
| `name` | create exactly this name instead. It must follow the [naming rule](#space-name). Can't be combined with `label` |
| `public` | `none`, `read`, `respond` or `all`, see [The public shorthand](#the-public-shorthand). Left out: `none`, private |
| `read`, `write` | the space's [settings](#settings) in full, instead of `public` |

- The caller becomes the space's owner. The response has the [owner token](#owner-token), shown once, and the caller's visitor `id`.
- Without a visitor token, a new visitor is created and becomes the owner. The response then also has `token`, the new visitor token, which the client must save before anything else.
- A name that's taken returns 409 `space_exists`.
- Counts against the limits on new spaces and, without a token, on new visitors.
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
- 404 `not_found` if the key doesn't exist. 404 `no_space` if the space doesn't exist or the request can't read it.
- Every key has `KV-Time`, its value time in server milliseconds. For a series it remains the newest point's time, including a caller-supplied point time. Plain keys use the server time when the write is processed under the space write mutex. Versions remain opaque counters. Expiring keys also have `KV-Expires`, their expiry in server milliseconds.
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

- Creates or replaces the value. A space that doesn't exist returns 404 `no_space`. Only `POST /v1/spaces` creates spaces.
- Needs a visitor token, and the [access rule](#the-access-rule) decides whether this visitor may write here.
- A series key can't be replaced. `PUT` on one returns 409 `is_series`. Add points with `POST {key}/series`, or `DELETE` the key first.
- `ifVersion=N`: succeeds only if the key's version is exactly N. `ifVersion=0`: only if it doesn't exist. A mismatch returns:

```
412
{"error": "version_conflict", "message": "...", "currentVersion": 1791449600045}
```

- `If-Match: "N"` is accepted as an equivalent. The query parameter wins if both are given.

### Expiring keys and leases

`PUT {key}?ttl=N` gives a plain key a lifetime of N whole seconds. The default bounds are 5 seconds to 30 days, configured by `ttl_min_seconds` and `ttl_max_seconds`. TTL is accepted on PUT only, in every area. Series keys cannot have TTL.

- Every PUT states its lifetime. Omitting `ttl` clears any previous expiry, including on an identical-value write. Zero, fractions and out-of-range values return 400 `invalid_parameter`.
- Expiry uses the server clock. Reads and writes check for due expiry before using the key. An expired key is absent for GET, list, initial watch, series reads and `ifVersion=0` acquisition.
- Expiry makes a normal versioned tombstone. Incremental watchers get `deleted: true`, `expired: true`, `by` naming the last writer, and `t` giving the expiry processing time. Explicit deletion has no `expired` flag. Recreating a key clears that flag.
- A once-per-second sweep visits only spaces with due expiries and wakes waiting watchers. Delivery is normally within about a second after expiry, subject to server load. Expiry is not a precise timer.
- Lists and watches include `expires` on expiring live entries. GET and PUT responses include `KV-Expires`. Identical-value renewals keep version, `by` and `t`; they update expiry silently. A cached watch entry's `expires` can therefore be stale. Fetch the key for current expiry; do not drive a countdown from a watch's cached expiry.
- Expiry removes the value's bytes from the writer's usage and leaves only the key-sized tombstone in the space quota. TTL renewals spend the usual write budget and keep the space active.

Presence uses `users/{id}/here` with `ttl=30`, renewed every 15 to 20 seconds. Watch `users/*/here` to see arrivals and expiry deletions. `me/` is private and is unsuitable for a shared roster.

A lease works through ordinary conditional requests. Choose a unique holder value for each actor:

```sh
# Take. 201 means acquired; 412 means an existing live key holds it.
cubby set "$SPACE/shared/lock.backup" "$HOLDER" --create --ttl 60s
# Save the printed version as VERSION. Renew with the same value and version.
cubby set "$SPACE/shared/lock.backup" "$HOLDER" --if-version "$VERSION" --ttl 60s
# Release only that version.
cubby del "$SPACE/shared/lock.backup" --if-version "$VERSION"
```

Expiry and conditional writes serialize under the same space mutex. An expired lease cannot be revived using its old version, even before the background sweep runs. Reacquisition receives a newer version, so a stale holder cannot renew or delete the replacement. A 412 during renewal means the actor must stop. A sleeping process can continue work after losing its lease until it checks again. This is a cooperative lease, without fencing of external systems. Automatic command runners and JS lock helpers are deferred.

### Write deadlines

`ifBefore=T` is accepted on PUT, DELETE and series POST, including batch series POST. T is a server timestamp in milliseconds. The server checks it under the space write mutex before starting the write transaction. At or after T, it returns `412 {"error":"too_late","message":"...","t":SERVER_TIME}` and performs no requested write. An expiry that was already due may still be processed.

Combine it with `ifVersion` on PUT or DELETE. Series still do not accept `ifVersion`. A quiz can set `ifBefore` to the round's closing time to refuse late answers. Conditions also apply to identical-value PUTs. Clients do not automatically resend a write after a network failure, 5xx or 429; the app or script decides. The existing one-time recovery from a rejected invalid visitor token and explicit `update` conflict handling remain supported.

### Limits and server time

`GET /v1/limits` needs no token and returns effective server defaults as JSON. Names match `[limits]` config keys; all values are numbers in bytes, counts or the unit in the key name. The extra `rate_limits` boolean reports whether request-rate limiting is enabled. Configured watch concurrency caps still apply when rate limiting is off.

`GET /v1/{space}/access` adds the effective limits for this space and the caller's usage:

```json
{
  "limits": {"valueBytes":65536,"pointBytes":1024,"seriesPoints":50000,"spaceBytes":5242880,"spaceKeys":10000,"visitorBytes":1048576,"writesPerMinute":120,"writeBurst":30,"rateLimits":true,"ttlMinSeconds":5,"ttlMaxSeconds":2592000},
  "usage": {"bytes":183204,"keys":412,"visitorBytes":20480}
}
```

Existing access fields remain. Pinned overrides appear here. `usage.visitorBytes` is the caller's stored bytes across all spaces, zero for a caller without a visitor token. Space totals include tombstones and series points as described under storage accounting. These are snapshots, not reserved capacity. Inaccessible spaces return the existing `no_space` response without usage. `/access`, `/limits` and `/time` consume the read budget; continuing long-poll requests still do too.

Data-write responses, including refusals after authorization, expose `RateLimit-Policy` and `RateLimit` headers using the [IETF draft 11 syntax](https://www.ietf.org/archive/id/draft-ietf-httpapi-ratelimit-headers-11.html):

```text
RateLimit-Policy: "visitor";q=30;w=15, "ip";q=300;w=60, "bytes";q=10485760;w=86400;qu="content-bytes"
RateLimit: "visitor";r=29, "ip";r=299, "bytes";r=10485750
```

Each member describes a continuously refilling token bucket. `q` is capacity, `w` is its full-refill duration rounded up to seconds, `r` is the remaining whole tokens. The optional effective-window `t` is omitted because a continuously refilling bucket has no fixed reset. Visitor and IP buckets count requests; the bytes bucket declares `qu="content-bytes"` and counts the key plus raw write body. Use Retry-After on a refusal. Check every bucket, since any can refuse the next request. Values are advisory snapshots and never reserve the next write. With `rate_limits=false` these headers are omitted. CORS exposes them to pages, along with `KV-Time` and `KV-Expires`.

`GET /v1/time` returns only `{"t":SERVER_MILLISECONDS}`, needs no token, and supports HEAD like `/limits`. Both return `Cache-Control: no-store`.

### Delete

```
DELETE /v1/poll/{area}/question
DELETE /v1/poll/{area}/question?ifVersion=1791449600042
```

```
200 {"version": 1791449600043}
```

- 404 `not_found` if the key does not exist. A supplied nonzero `ifVersion` on an absent or expired key returns 412 `version_conflict`, with currentVersion 0. `ifVersion` and `ifBefore` work as for PUT.
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
- Listing in a space that doesn't exist, or that the request can't read, returns 404 `no_space`. A pattern with no matches returns an empty list.
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
| `GET /share.js` | the share component as an ES module |
| `GET /health` | 200 `{"ok": true, "version": "v0.8.1"}` |
| `OPTIONS` anything | the CORS preflight response |
| `/v1/anon`, `/v1/whoami`, `/v1/spaces` | see [Visitors](#3-visitors) and [Create a space](#create-a-space) |
| `/v1/{space}` and its `access`, `join`, `tokens`, `grants`, `owner-token` | see [Paths](#paths) |
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
- A matching change after quiet is answered immediately. During a burst the server waits up to 50 ms once to combine changes; further writes do not extend that pause.

### Why nothing is lost

The client always passes the version from its last response. A write between two requests has a higher version, so the next request returns it at once. Several writes to one key while nobody asks come back as one change with the latest value. Watching is for state, not an event log.

### Deletes

- Deletes leave a marker (tombstone) so watchers learn about them. Tombstones are removed after 7 days (`tombstone_ttl`), and the space remembers the highest removed version as its floor.
- A watch with `0 < version < floor`, or with a version above the space's counter, returns 410 `version_expired`. The client starts again from `version=0` and treats keys it knew that are missing from the new snapshot as deleted.

### Limits

- At most 50 open watches per IP and 1000 on the server (`watches_per_ip`, `watches_total`). A [live connection](#live-connection) counts as one, however many watches it carries. Over the limit: 429 `too_many_watches`.
- Browsers allow 6 HTTP/1.1 connections per domain across all tabs, so serve the API over HTTP/2.

### Live connection

```
GET /v1/{space}/live        Upgrade: websocket
```

One WebSocket per space carries reads, writes and watches. It's the same store as HTTP: a write over either wakes watchers on both, versions are shared, and a `curl` script and a page on a socket see each other's changes as they happen. Watched changes are pushed with no request per change. HTTP stays the basic way, and keeps every feature.

Only data goes over the socket: get, set, delete, list, watch, series and pulses, under `shared/`, `me/` and `users/`. Creating spaces, visitors, `join`, `access`, settings, tokens, grants, `owner-token`, `whoami`, `/limits` and `/time` stay HTTP only.

#### Connecting and auth

- A token never goes in the URL. `?token=` on this route is refused with 400 before the upgrade.
- **An auth message.** Browsers can't set headers on a WebSocket, so the first message is usually `{"id":1,"op":"auth","token":"vt_..."}`. The server waits up to 10 seconds for a first message, then closes with 4408. Any first message ends the wait. A data message first means no visitor, as an HTTP request without a token.
- **Or a header.** Clients that can set headers may send `Authorization: Bearer vt_...` with the upgrade. A bad one fails before the upgrade with 401 `invalid_token`. There's no wait then.
- Both together are fine, and `auth` may come again at any time, for example after the page creates a visitor for its first write. Each message runs as the visitor from the latest `auth` (or the header).
- `auth` answers `200 {"id":"u_..."}`, or `{"id":null}` for `{"op":"auth"}` without a token. A bad token answers 401 `invalid_token`, and what follows behaves exactly like HTTP with a bad token: reads run without a visitor, writes answer `invalid_token`.
- Access is checked per message, never at connect, so a page may connect before it joins and use a grant the moment it gets one. A space the caller can't see answers each message with `no_space`, exactly like a missing space, and costs the same [miss](#guessing-space-names).
- Switching to a different visitor ends watches on `me/` with 409 `identity_changed`. `me/` meant the old visitor's area. Start them again from their last version.

#### Messages

Messages are JSON text frames. A data message is an HTTP request in an envelope:

```
→ {"id":2,"method":"PUT","path":"shared/note.42","query":{"ifVersion":17,"ttl":30},"body":{"text":"Buy milk"}}
→ {"id":3,"method":"GET","path":"shared/note.42"}
→ {"id":4,"method":"PUT","path":"shared/memo","text":"stored as text"}
→ {"id":5,"method":"GET","path":"shared/board.*/watch","query":{"version":0}}
→ {"op":"stop","watch":5}
→ {"id":6,"op":"ping"}
```

- `method` is GET, PUT, DELETE or POST. `path` is what follows `/v1/{space}/` in the HTTP route, without a query string. `query` holds the route's parameters as strings, numbers or booleans, the same ones the route takes over HTTP. Unknown fields anywhere are refused with `bad_message`, and unknown parameters with `invalid_parameter`, as on HTTP.
- `body` is a JSON value, stored byte for byte as sent. `text` is a string stored as text. Only PUT and POST take one.
- `id` is a number or a string, and every reply carries it back. Replies may arrive in any order.
- `op` messages are `auth`, `ping` (answers `200 {}`) and `stop` (`watch` names the watch's id; a reply only if it has its own `id`).

Replies are the HTTP response in an envelope:

```
← {"id":3,"status":200,"headers":{"ETag":"\"17\"","KV-By":"u_...","KV-Time":"1791449600000"},"body":{"text":"Buy milk"}}
← {"id":4,"status":201,"headers":{"ETag":"\"18\"","RateLimit":"\"visitor\";r=28, ...","RateLimit-Policy":"..."},"body":{"version":18}}
← {"id":2,"status":412,"body":{"error":"version_conflict","message":"...","current":18}}
```

- `status` and `body` are what HTTP would answer. The handlers are the same, so rules, limits and errors can't differ.
- `headers` holds the response headers that carry data: `ETag`, `KV-By`, `KV-Time`, `KV-Expires`, `RateLimit`, `RateLimit-Policy`, `Retry-After`, `Allow`.
- A stored value is `body` byte for byte. A text value is a JSON string with `"text":true`, where HTTP would say `text/plain`.
- A message that isn't a valid envelope answers 400 `bad_message`, with its `id` if one could be read.

#### Watches over the socket

- The first push comes at once, even with no changes, so the client knows the watch runs and from which version. Without `version` that's an empty push at the current version.
- After that, each push is the same body an HTTP watch returns, whenever matching changes exist. A burst of writes is combined as on HTTP. `more: true` means the next push follows at once.
- A slow client loses nothing. Each push carries everything since the last one, merged per key, as long-poll does.
- `timeout` isn't taken. A watch runs until `stop`, an error or the end of the connection. Pushes already on the way may still arrive after `stop`; ignore ids you no longer follow.
- Before each push the server checks access again, as a new request would. A grant that ended ends the watch with `no_space`. 410 `version_expired` and other errors end it the same way: one error frame with the watch's id.
- At most 20 watches per connection. One more answers 429 `too_many_watches`. Reusing the id of a running watch answers 400 `id_in_use`.

#### Limits and keepalive

- A data message costs what its HTTP request costs: a read, or a write against the write limits. Starting a watch costs one read; pushes cost nothing. `auth`, `ping` and refused envelopes cost one read each, `stop` nothing.
- The server pings every 30 seconds and closes a connection that doesn't answer within 15. A client that can't take a frame for 10 seconds is disconnected.
- Messages are at most 256 KB.
- Close codes: 1000 normal, 1001 server restarting (reconnect at once), 1003 a binary frame, 1009 too large, 4408 no first message in 10 seconds.
- Proxies must pass WebSocket upgrades. For nginx: `proxy_http_version 1.1`, `proxy_set_header Upgrade $http_upgrade` and `proxy_set_header Connection $connection_upgrade` with the usual `map`.

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
Authorization: Bearer vt_...

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
- Towards the visitor quota, a series counts like a plain key: its key and newest value, for whoever wrote last. Otherwise a device publishing readings would hit the 1 MB visitor limit after a few sensors. Space quotas, the limit on new spaces and the per-series cap still bound the total.
- Trimming the oldest point happens before the quota check. A full series at a steady rate never runs into the space quota by itself.
- A 5 MB space holds about eight full numeric series. Pin a space for more.

### Watching

Watching a series key works like watching any key. It returns the newest point, and several points between two polls come back as one change. That suits a live display. To get every point, read `series?since=` with the last `t` you have.

There's no `{key}/series/watch`. Nothing comes after an action.

## 12. Pulses

A pulse is a value that is never stored. It goes to whoever is listening at that moment and is then gone: no version, no disk, nothing to read back later. Pulses are for things that change many times a second, or that matter as events rather than as state: a cursor, a dragged card, a reaction, a doorbell, a game input.

Stored values and pulses use the same keys, areas and rights, and never touch each other. A pulse doesn't change a key, doesn't wake a watch and never appears in a get, list or watch. A stored write never produces a pulse.

```
POST /v1/{space}/{area}/{key}/pulse                  body: the value     → 202 {"t": 1791449600123}
GET  /v1/{space}/{area}/{key or pattern}/pulse?after=SEQ                 → {"pulses": [...], "seq": SEQ, "missed": false}
```

### Sending

- The body follows the rules for a value: UTF-8, JSON or text, not empty. At most 4 KB (`max_pulse_size`). The route takes no parameters.
- It needs the right to write that key. Under `users/{id}/` only that visitor sends; owners can't send as someone else. A pulse to `me/` reaches only your own listeners, such as your other tabs and devices.
- The answer is `202 Accepted` with the server time `t`, whether or not anyone listens. There's no version, `ETag` or `RateLimit` header.
- Pulses have their own rate limits, separate from writes: 60 per second per visitor with a burst of 120, and 300 per second per IP. Sending pulses never uses up the write budget, and writes never use up the pulse budget.
- A pulse goes to one key. Patterns and `users/*/` are for listening.

### Listening

- `GET .../pulse` is a long poll, like a watch. Without `after` it listens from now. It answers as soon as pulses after its cursor exist, or after `timeout` seconds (1 to 55, default 25) with none.
- The answer is `{"pulses": [{"key", "user", "by", "value", "t"}], "seq", "missed"}`. `user` is set under `users/*/`. Text values are JSON strings, as in lists. Pulses come in the order the server received them, including the listener's own.
- Pass `seq` back as `after`. Pulses sent between two requests arrive with the next one.
- Over HTTP, once a pulse arrives the server waits 50 ms before answering, to take the ones right behind it in the same answer.
- It needs the right to read the key or pattern. One request costs one read and counts as an open watch while it waits.

### The window

Pulses aren't stored, but each space keeps the most recent ones in memory for a short time, so a listener between two requests, or reconnecting a socket, doesn't lose them.

- A pulse stays in the window for 10 seconds, and a space keeps at most 1024 pulses or 256 KB. An answer carries at most 256, the newest.
- `missed: true` means some pulses for this listener left the window before it got to them, or didn't fit in the answer. It's best effort: a listener away longer than the window might not be told.
- The window lives in the server's memory only. A restart empties it.
- Anything not meant to be lost belongs in a stored key.

### On a live connection

```
→ {"id":1,"method":"POST","path":"users/u_k7m2x9q4p1r8t3w6/cursor/pulse","body":{"x":120,"y":40}}
← {"id":1,"status":202,"body":{"t":1791449600123}}
→ {"id":"c","method":"GET","path":"users/*/cursor/pulse"}
← {"id":"c","status":200,"body":{"pulses":[],"seq":1793107200000001,"missed":false}}
← {"id":"c","status":200,"body":{"pulses":[{"key":"cursor","user":"u_...","by":"u_...","value":{"x":120,"y":40},"t":...}],"seq":...,"missed":false}}
→ {"op":"stop","watch":"c"}
```

- Sending is the same request in the envelope. On one connection, pulses arrive at the server in the order they were sent.
- A listener sends its first frame at once, then one whenever matching pulses arrive, without the 50 ms wait. It takes `after` to resume, no `timeout`, and runs until `stop`. It counts towards the 20 watches per connection.
- Access is checked again before each frame, as for watches.

## 13. Limits and quotas

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

### Access

| Limit | Default | Error |
|---|---|---|
| Access tokens per space, listed ones included | 1000 | 507 `space_quota_exceeded` |
| Grants per space | 10,000 | 507 `space_quota_exceeded` |
| Scopes per token or grant | 20 | 400 `invalid_scope` |

A token with `uses: null` in a public page could otherwise make a grant per visitor forever. When a space is full of grants, redeeming fails and existing grants keep working.

### Rates

Every rate in this table is configurable under `[limits]`, including `write_burst`. Watch caps use `watches_per_ip` and `watches_total`. Rate and TTL bounds must be positive; TTL maximum must be at least the minimum and at most 10 years. Other bursts equal their rate-period quota. Token buckets. A 429 includes `Retry-After` in seconds and `retryAfter` in the body.

| Limit | Default | Error |
|---|---|---|
| New visitors per IP, through `/v1/anon`, `/v1/spaces` or `join` | 30 per hour, burst 30 | 429 `rate_limited` |
| Writes per visitor | 120 per minute, burst 30 | 429 `rate_limited` |
| Writes per IP | 300 per minute | 429 `rate_limited` |
| Write bytes per IP | 10 MB per day | 429 `rate_limited` |
| Reads per IP | 600 per minute | 429 `rate_limited` |
| New spaces per visitor | 5 per day | 429 `space_limit` |
| New spaces per IP | 20 per day | 429 `space_limit` |
| Distinct missing spaces per IP | 30 per hour, burst 30 | 429 `miss_limit` |
| Pulses per visitor | 60 per second, burst 120 | 429 `rate_limited` |
| Pulses per IP | 300 per second, burst 600 | 429 `rate_limited` |
| Spaces on the server | 1000 | 507 `server_full` |

A `POST` that adds points counts as one write, however many series it adds to.

The IP-based write limits exist because one person can create many visitors. They're set high enough for a household or office behind one IP.

### Guessing space names

The miss limit makes guessing names slow. It counts how many different spaces that don't exist an address touches.

- A request to a space that doesn't exist costs one miss, the first time. Touching the same missing name again within the hour is free, so a page polling a deleted space costs one miss, not one per poll.
- A missing key inside a space that exists costs nothing. Neither does using a space that exists.
- A request to a private space that the request can't read counts exactly like a request to a missing space, since that's what it gets back.
- A failed `join` costs one miss, whether the space exists or not.
- `POST /v1/spaces` doesn't count, and its creator can use the new space right away.
- Once an address is out of misses, every space it hasn't used in the last hour answers 429 `miss_limit`, whether the space exists or not. A 429 only for missing spaces would tell a guesser which names exist. Spaces it used in the last hour keep working, so pages already open aren't cut off.
- Refused requests don't cost anything. One miss comes back every 2 minutes.
- IPv6 addresses count per /64, like the other IP limits. `rate_limits = false` turns it off with the rest.

With generated names (48 bits) and 1000 spaces, one address needs about 3 × 10^11 guesses to find any space. At 30 an hour that is about a million years.

### When a limit is hit

Only the request that would go over fails. Nothing stored is touched, and reads and watches keep working.

## 14. Server config and lifecycle

### Lifecycle of a space

- **Created** only by `POST /v1/spaces`, which gives it an owner. Reads, writes, lists and watches never create a space.
- **Active** whenever it's read, listed, watched or written. Reads count, so a status page that's read daily but updated twice a year stays alive.
- **Deleted** by an owner with `DELETE /v1/{space}`, by the operator with `cubby admin delete`, or after 180 days without activity (`space_idle_ttl`), file and all, unless pinned. Its tokens and grants go with it.
- **Pinned** spaces are listed in config. They get their own limits and are never deleted automatically.

Most pages are used now and then, not constantly: a poll before each team lunch, a packing list before each trip. The retention defaults are long for that reason. The cost is that spam also lingers longer. Quotas cap how much there can be, and `cubby admin delete` removes it by hand.

### Retention defaults

| What | Default | Config |
|---|---|---|
| Space without activity | 180 days | `space_idle_ttl` |
| Visitor not seen | 365 days | `visitor_idle_ttl` |
| Visitor not seen who never stored anything and holds no grant | 30 days | `empty_visitor_idle_ttl` |
| Delete markers for watchers | 7 days | `tombstone_ttl` |
| Access token that can no longer be redeemed and has no grants left | 30 days after it ended | fixed |
| Grant past its `until` | removed by the next cleanup, stops working at once | fixed |

A watcher that comes back after more than 7 days gets 410 and reloads its snapshot, which is cheap. A longer `tombstone_ttl` would only avoid that reload.

### Fixed intervals

Not configurable, listed so they're in one place:

| What | Value |
|---|---|
| Cleanup job | every hour |
| "Last seen" and "last active" granularity | 1 hour |
| Write-ahead log size kept per space | 4 MB |
| Watch timeout | 25 s default, 1 to 55 s allowed |
| Pause before answering a watch after a change | up to 50 ms during bursts; no pause after quiet |
| Changes per watch response | 1000 |
| Open space files kept without watchers | 64, least recently used closed first |

### Config file

TOML, passed with `--config`. Everything is optional.

```toml
[server]
listen = ":28229"
data_dir = "./data"
trusted_proxies = ["127.0.0.1/32"]      # whose X-Forwarded-For to believe
rate_limits = true

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
max_spaces = 1000
writes_per_minute = 120
write_burst = 30
writes_per_ip_per_minute = 300
reads_per_ip_per_minute = 600
write_bytes_per_ip_per_day = 10485760
visitors_per_ip_per_hour = 30
spaces_per_visitor_per_day = 5
spaces_per_ip_per_day = 20
misses_per_ip_per_hour = 30
watches_per_ip = 50
watches_total = 1000
ttl_min_seconds = 5
ttl_max_seconds = 2592000

[spaces.poll-cedar-pilot-mango-frost]                  # pinned
max_bytes = "100MB"
max_value_size = "256KB"
max_series_points = 500000
```

### Administration

Through `cubby admin`, see [Admin](#admin) in the CLI section. There is no admin HTTP API.

## 15. Errors

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
| 400 | `invalid_parameter` | bad `limit`, `order`, `after`, `since`, `t`, `timeout`, `version`, `ifVersion`, `ttl`, `ifBefore`, `label`, `name`, `uses`, `until`, `grantFor`, or a malformed JSON body | `parameter` |
| 400 | `invalid_settings` | unknown setting or value, `public` together with `read` or `write`, or `read: granted` with any area writable by anyone | |
| 400 | `invalid_scope` | malformed scope or unknown preset, or more than 20 scopes | `scope` |
| 401 | `missing_token` | visitor token needed but missing | |
| 401 | `invalid_token` | visitor token needed, unknown or expired | |
| 403 | `no_access` | a write the space's settings and the visitor's grants don't allow, in a space the request can read | |
| 403 | `owner_only` | an owner route, called by someone who isn't an owner but can read the space | |
| 403 | `not_yours` | write under another visitor's ID | |
| 404 | `not_found` | key, route, token ID, grant ID or visitor doesn't exist | |
| 404 | `no_space` | the space doesn't exist, or it's private and the request can't read it. The message says both, see [Private spaces](#private-spaces) | |
| 404 | `invite_invalid` | `join` with a token that's unknown, expired, used up or revoked, or for a space that doesn't exist | |
| 405 | `method_not_allowed` | wrong method for the route, such as `PUT` under `users/*/` or `POST` on a key without `/series` | `allow` |
| 409 | `space_exists` | `POST /v1/spaces?name=` with a taken name | `space` |
| 409 | `is_series` | `PUT` on a series key | |
| 409 | `not_series` | `/series` on a plain key | `key` in a batch |
| 410 | `version_expired` | watch version too old or from elsewhere | |
| 412 | `too_late` | `ifBefore` deadline reached | `t` |
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
- JSON request bodies follow the same rule: an unknown field returns 400 `invalid_parameter` naming it, so `{"use": 1}` can't silently become an invite with the default 10 uses.
- Numbers are plain decimal digits: no sign, spaces, hex or decimal point, at most 16 digits.

### Clients check before sending

The CLI and the JS client validate space names, keys, patterns and visitor IDs locally and refuse bad ones without a request. Otherwise a `#` or `?` typed into a key would be read as a fragment or query, and the request would silently address a different key.

## 16. HTTP details

### CORS

On every `/v1` response:

```
Access-Control-Allow-Origin: *
Access-Control-Expose-Headers: ETag, Retry-After, KV-Time, KV-By, KV-Expires, RateLimit, RateLimit-Policy
```

On preflight:

```
Access-Control-Allow-Methods: GET, HEAD, PUT, PATCH, DELETE, POST, OPTIONS
Access-Control-Allow-Headers: Authorization, Content-Type, If-Match, If-None-Match
Access-Control-Max-Age: 86400
```

- `*` is safe because there are no cookies.
- Without `Expose-Headers`, browser code can't read `ETag`, `Retry-After`, `KV-Time` and `KV-By` cross-origin.
- Plain reads send no custom headers, so they need no preflight at all. Clients should keep it that way: browsers cache a preflight per URL, and every watch request has a new `version`, so an `Authorization` header on a read costs an extra round trip each time. kv.js sends the token on reads as `?token=` for that reason.

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
- Log method, path, status, duration, IP and a short hash of the visitor ID. Never log any token, the `token` query parameter or bodies. `join`, `tokens` and `owner-token` carry tokens in their bodies.
- A live connection logs one line per data message, prefixed `WS`, and one line for the connection when it closes, with status 101 and its duration. Pushes and `op` messages aren't logged.
- `/kv.js` is cached for an hour. Everything under `/v1` is `no-store`.

## 17. JS client

Optional. Served at `/kv.js` as an ES module with no dependencies. Every method maps to the HTTP calls above. It works in browsers, workers, Node and Deno, and touches the DOM only through `location` and `history` in `takeInvite` and `inviteLink`. The ready-made sharing UI is a separate module, see [Share component](#share-component).

### Guide comment

Many pages using cubby are written by coding agents for people who don't know what a space or a token is. kv.js starts with a comment that tells the reader, human or agent, how to do the safe thing. It's part of the contract, kept under 40 lines, and its text is:

```js
// cubby client. A guide for whoever writes code with this, person or coding agent.
//
// Spaces are PRIVATE by default. Only the creator (the owner) and people they
// invite can read or write. Everyone else gets 404 no_space, exactly as if the
// space didn't exist, so a missing invite looks like a wrong name. Check both.
//
// Every page starts the same way:
//   const db = kv(SPACE)
//   const me = await db.open()     // redeems an invite link (#t=...), says what you may do
//   if (!me.inside) { /* show: "ask the owner for an invite link" */ }
//
// Creating a space (once from a shell with `cubby new`, or per room in the page):
//   const { space, ownerToken } = await createSpace()
//   Show ownerToken to the user once. It's their recovery key. Never store it in code.
//
// Sharing with specific people, the usual case:
//   const { link } = await db.share({ label: "Anna" })       // member, 10 uses, 7 days
//   const { link } = await db.share({ allow: "viewer", uses: 1 })
//   Ready-made UI with link, QR code and member list: import "<server>/share.js"
//   and put <cubby-share space="..."></cubby-share> in the page.
//
// Make a space public only when the user asked for it:
//   createSpace({ public: "read" })     anyone reads, invited people write: status pages
//   createSpace({ public: "respond" })  anyone reads and adds their own entry: polls, RSVPs
//   createSpace({ public: "all" })      anyone with the name reads and writes
//
// Never put an access token (at_...) or owner token (ot_...) in page source.
// Anyone who can load the page could use it. Invite links are for sending to people.
//
// Docs: docs/agents.md in the cubby repository.
```

### Setup

```js
import { kv, createSpace, takeInvite, inviteLink, newName, validSpace, KvError } from "https://kv.example.com/kv.js"

const db = kv("poll-cedar-pilot-mango-frost")
const me = await db.open()

newName("poll")        // "poll-slate-hinge-cocoa-pilot", local, no request
validSpace(fromLink)   // true if a string from a link or a form is a usable space name
```

These seven are the module's only exports. There is no default export.

| Option | Default | Purpose |
|---|---|---|
| `server` | origin of the `kv.js` URL | API server |
| `tokenKey` | `"cubby:" + server` | localStorage key for the visitor token and ID |
| `token` | none | a visitor token to use instead of the stored one, for scripts and tests. Never saved |
| `fetch` | global `fetch` | for tests or other runtimes |
| `live` | `false` | use a [live connection](#live-connection) for data while it's open. Same API, same results |
| `WebSocket` | global `WebSocket` | for tests or other runtimes |

- `kv()` makes no request, and throws at once for an invalid name, with the server's `suggestion`.
- Several `kv()` instances for different spaces share one visitor.
- A page that only reads a public space never gets a visitor token. Every other page needs one, since spaces are private by default.

### Identity

- The first call that needs a visitor token gets one: a write, anything in `me/`, an owner call, or a read in a private space. Concurrent calls share that request.
- `createSpace` and `join` called without a stored token create the visitor through their own response and store it before returning.
- On 401 `invalid_token` the client drops the token, gets a new one and retries once. The new visitor has no grants. In a space that needs them, the retry fails with `no_access` or `no_space`, and the page should ask for a new invite.
- With the `token` option the client never creates visitors. A 401 throws without a retry.
- If localStorage throws, the token lives in memory and the visitor is new on every page load. The client warns once.

### Sending the token

- Writes, `me/`, `join` and owner calls send the token as `Authorization`.
- Reads, lists and watches send it as `?token=` whenever the client has one, so a private space works with no extra option and reads stay free of CORS preflights.
- With no token at all, reads go out without one. They work in a public space, and get `no_space` in a private one, which is right: a visitor without a token can't hold a grant.

### Opening, creating and sharing

```ts
createSpace(opts?: { label?, name?, public?, read?, write?, server? }): Promise<{ space: string, ownerToken: string }>
takeInvite(param = "t"): string | null        // reads #t=... from location.hash and removes it from the address bar
inviteLink(token: string, base = location.href): string   // base with t=token added to its fragment

db.open(opts?: { invite?: string | null }): Promise<Access>   // the first call on every page
db.join(token: string | null): Promise<Access | null>         // null token: does nothing, returns null
db.access(): Promise<Access>
db.share(opts?: { label?, allow?, uses?, until?, grantFor?, base? }): Promise<Invite>   // owners

type Access = {
  inside: boolean,               // false: no such space, or private and this visitor isn't in it
  joined: boolean,               // an invite was redeemed by this call
  inviteFailed: boolean,         // the link had an invite that was unknown, expired or used up
  id: string | null, owner: boolean,
  settings: { public: "none" | "read" | "respond" | "all" | "custom", read: "anyone" | "granted", write: { shared, users, me } } | null,
  grants: { id, allow: string[], source: string, until: number | null }[],
  can(method: "read" | "write", area: "shared" | "users" | "me", key?: string): boolean   // evaluated locally
}

type Invite = { link: string, token: string, id: string, allow: string[], uses: number | null, until: number | null }
```

- **`db.open()`** is how every page starts. It takes an invite from the URL with `takeInvite()` unless `invite` is given, redeems it with `join` if there is one, then asks `access`. It never throws for `no_space` or `invite_invalid`. It returns `inside: false` or `inviteFailed: true`, so the page can show a plain message. A page opened by the space's owner, a member, or anyone in a public space gets `inside: true`.
- **`createSpace`** makes the caller the owner and defaults to private. `public` takes `none`, `read`, `respond` or `all`. Show `ownerToken` to the person once and tell them to keep it. The client doesn't store it.
- **`db.share()`** mints an access token and returns the link to send, built with `inviteLink(token, base)`. `base` defaults to the current page. It takes the server's defaults: `member`, 10 uses, 7 days. `allow` takes `"member"`, `"viewer"` or a list of scopes.
- `takeInvite` keeps other fragment parameters, so `#room=...&t=at_...` becomes `#room=...`. `inviteLink` keeps them too: `inviteLink(token)` on a page at `/chat/#room=x` gives `/chat/#room=x&t=at_...`.
- `db.join` sends the stored visitor token if there is one, as the server requires. Use it directly only for tokens that don't come from the URL, such as one typed into a form.
- `access().can()` applies the [access rule](#the-access-rule) to the answer locally, for showing and hiding controls.

### Client limits and clock

```js
const { limits, usage } = await db.limits() // same fields as db.access()
const budget = db.writeBudget             // null until a data-write response with headers
// budget.visitor, budget.ip, budget.bytes each have { remaining }.
// budget.observedAt is local Date.now() when that response arrived.

const { offset, rtt } = await db.clock.sync() // milliseconds, 5 samples by default
const now = db.clock.now()                   // estimated server ms, throws before sync
const cancel = db.clock.at(Math.ceil(now) + 2000, () => reveal())
cancel()                                    // optional cancellation
// db.clock.dispose() removes listeners and cancels scheduled callbacks.
```

`sync({samples: N})` accepts 1 to 20 samples and keeps the successful sample with the shortest round trip. It estimates `offset = serverT - (sent + received)/2`. Request and response delay may differ, so half the RTT is an uncertainty estimate, not an accuracy guarantee. The clock uses elapsed monotonic time after sync and re-syncs when the page becomes visible or comes online. Re-sync adjusts pending timers. Browser throttling can still delay callbacks. Neither a clock sync nor a timer writes stored data or creates a visitor.

Use `opensAt` a little in the future to start a round together, and `ifBefore: closesAt` to enforce its deadline. Plain-key `t` is server processing time; series `t` remains point time and may be supplied by the caller. A quiz should use plain answer keys when server timestamps matter.

### Owner calls

Every method needs the visitor to be an owner and throws `owner_only` otherwise.

```ts
db.owner.settings(patch: { public? } | { read?, write? }): Promise<Settings>
db.owner.deleteSpace(): Promise<void>
db.owner.rotateOwnerToken(): Promise<string>

db.owner.tokens.create(opts?: { label?, allow?, uses?, until?, grantFor? }): Promise<{ id, token, ... }>
db.owner.tokens.list(): Promise<TokenInfo[]>
db.owner.tokens.update(id, { uses?, until? }): Promise<TokenInfo>
db.owner.tokens.revoke(id): Promise<void>

db.owner.grants.list(opts?: { user? }): Promise<Grant[]>
db.owner.grants.add(user, opts?: { allow?, until?, owner? }): Promise<Grant>
db.owner.grants.revoke(id): Promise<void>
db.owner.grants.revokeUser(user): Promise<void>
```

Defaults are the server's: a token with no options is a full member, 10 uses, 7 days. `until` and `grantFor` take a duration string, a `Date`, milliseconds, or `null`. `db.share()` is `tokens.create` plus the link.

### Areas

```ts
db.shared: Area
db.me: Area
db.user(id?: string): Area      // no id: your own; another visitor's id: writes throw not_yours, delete works for owners
db.users: { list(target, opts?), watch(target, onChange, opts?), onPulse(target, onPulse, opts?) }    // across all visitors
db.id(): Promise<string>        // your visitor ID, gets a token if needed
```

### Area methods

```ts
get(key): Promise<any | null>
getEntry(key): Promise<Entry | null>                                // without key and user
set(key, value, opts?: { ifVersion?: number, ttl?: number, ifBefore?: number }): Promise<number>
add(target, value, opts?: { t?: number, ifBefore?: number }): Promise<{ version, t, keys? }>
series(key, opts?: { since?: number, limit?: number }): Promise<{ key, points: Point[], version, next } | null>
delete(key, opts?: { ifVersion?: number, ifBefore?: number }): Promise<boolean>
update(key, fn: (current: any | null) => any, opts?: { retries?: number, ttl?: number, ifBefore?: number }): Promise<any>
list(target, opts?: { limit?: number, order?: "asc" | "desc" }): Promise<List>
watch(target, onChange: (change: Change) => void, opts?: WatchOptions): () => void
pulse(key, value): Promise<void>                                    // not stored; see Pulses
onPulse(target, onPulse: (pulse: Pulse) => void, opts?: { onMissed?: () => void, onError?: (err: KvError) => void }): () => void

type Entry  = { key, value, version, by, mine: boolean, user?, t?, expires? }
type Change = Entry & { deleted: boolean, expired?: boolean }
type List   = { items: Entry[], version: number }
type Point  = { t, value }
type Pulse  = { key, value, by, mine: boolean, user?, t }
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
- **Local checks.** Writing to someone else's `db.user(id)` throws `not_yours` without a request, unless it's a delete by an owner. Everything else about access is left to the server.
- **`delete`** returns `false` for a missing key.
- **`add`** with a key, like `add("temp", 21.4)`, adds one point and sends `JSON.stringify(value)`, so an object is one point. With a pattern, like `add("env.*", { temp: 21.4, rh: 45 })`, `value` must be a plain object and each property becomes a point in its own series. The target decides, never the shape of the value.
- **`series`** makes one request and returns the response as is, or `null` if the key doesn't exist. To read further, pass `next` back as `since`.
- **`getEntry`** returns `t` from `KV-Time` for every key, and `expires` from `KV-Expires` when present. `set` takes TTL in whole seconds. Use `set` to renew a heartbeat; `update` still skips writing an identical computed value.
- **`update`** reads, calls `fn`, and writes with `ifVersion`. On 412 it retries after 50 to 200 ms, up to `retries` (default 10), then throws `version_conflict`. If `fn` returns `undefined`, or a value whose `JSON.stringify` equals the current one, nothing is written. `fn` may run more than once, so it must not have side effects.
- **`list`** follows `next` until `limit` or the end. The returned `version` is from the first page.
- **`by` and `mine`.** Every entry and change has `by`, the visitor who last wrote or deleted it, and `mine`, true when that's this visitor. A watch delivers your own writes too; `mine` tells them apart.
- **`watch`**:
  - `from` says where to start. `0`, the default, delivers every existing key first, then changes. `"now"` delivers changes only. A `list()` result continues right after that list, so a page can show the list first and then follow it. A number continues after that version.
  - `onChange` gets one call per changed key. `onSync` follows once all changes from one answer have been delivered. At that point the page's state matches the server as of `version`, so it's the place to render. It also fires after each empty answer, so a working watch syncs at least every 25 seconds. `initial` is true on the first one: with `from: 0`, every existing key has arrived. After `onError`, the next `onSync` means the watch works again.
  - On 410 (version too old or from elsewhere, for example after a week asleep) a watch with state starts again from `0` and reports keys it knew about that are missing from the new snapshot as deleted. It knows the keys it has delivered, plus the items of a `list()` it started from. Starting from a bare number it knows none at first, so the app should reload its state when `resync` is true. A `"now"` watch skips ahead to the present instead of replaying everything. In both cases the next `onSync` has `resync: true`.
  - Errors with `retryable` true (network, 5xx, 429) go to `onError`, then the watch tries again after 1, 2, 4 ... up to 30 seconds, or after `retryAfter` for a 429. Any other error goes to `onError` once and ends the watch.
  - The browser offline event aborts a pending watch, reports a retryable network error and pauses requests until online.
  - A request that gets no answer in 35 seconds counts as a network error. The server answers within 25.
  - When the page becomes visible, comes back online or is restored from the back/forward cache, the watch drops its current request or wait and asks again at once. A phone that slept may have a dead connection that never fails on its own.
  - After the tab has been hidden for 5 minutes it pauses, and resumes from its last version when visible.
  - The returned function stops the watch and removes its event listeners.
- **`live: true`** changes the transport, not the API. Every method returns the same values and errors, and callbacks fire the same way.
  - The socket opens on first use. Data requests use it while it's open and HTTP otherwise, so the first requests of a page never wait for it. Everything else (`open`, `join`, `access`, owner calls, `createSpace`, `clock`) is always HTTP.
  - A watch waits up to 3 seconds for a connecting socket, then uses HTTP, and moves over when the socket opens. A watch on the socket syncs at once, also with `from: "now"`.
  - When the socket drops, requests in flight fail with a retryable `network_error` and are never resent. Watches resume from their last version, over HTTP until the socket is back, so nothing is lost. Reconnects back off from 1 to 30 seconds after failures.
  - The client pings every 25 seconds and drops the socket after 10 seconds of silence, and pings with a 3-second limit when the page becomes visible again. A phone that slept can hold a socket that looks open and is dead.
  - The socket closes on `pagehide`, so the page can enter the back/forward cache, and after 5 minutes hidden. It reopens when needed after `pageshow` or when visible.
  - If the socket fails twice before ever opening (a proxy that blocks WebSockets, Chrome's Local Network Access rules for a public page calling a local server, an older server), the client stays on HTTP for that `kv()` without an error.
  - `db.live.status` is `"http"` (live off, or fell back), `"connecting"`, `"open"`, `"closed"` (not needed right now, or between retries) or `"offline"`. Assign `db.live.onStatus` to hear changes. Pages don't need either.

### Errors

```ts
class KvError extends Error {
  status: number      // HTTP status, 0 for network or local checks
  code: string        // the `error` code, "network_error", or a local one like "not_yours"
  retryAfter?: number // seconds, also retained in details.retryAfter
  details: object     // the rest of the error body
  retryable: boolean  // network error, 5xx or 429: worth trying again later
}
```

`retryable` is what an offline write queue checks: keep the write and try again if true, drop it and tell the user if false.

### Share component

A second module, `/share.js`, gives owners a ready-made sharing panel. It's separate so kv.js stays usable without a DOM, and pages that never share don't load it.

```html
<script type="module" src="https://kv.example.com/share.js"></script>

<cubby-share space="family-cedar-pilot-mango-frost"></cubby-share>
```

`<cubby-share>` is a custom element with a shadow DOM. It loads kv.js from the same server and uses the page's stored visitor, so it acts as whoever the page acts as.

What it shows depends on who's looking:

| Visitor | Shows |
|---|---|
| an owner | a "Share" button that opens the panel |
| a member, with `members` set | "Shared with you by the owner", and nothing to click |
| anyone else | nothing |

The panel has four parts:

1. **Invite.** Name the person, then pick "one person" (1 use, 7 days), "a group link" (10 uses, 7 days) or "view only" (`viewer`, 1 use, 7 days). It shows the link with Copy, Share (`navigator.share` where the browser has it) and a QR code. Custom uses and dates are behind "More options", where unlimited needs a second click that says what it means.
2. **People.** Members listed by the label of the invite they used, with when they joined and were last active. Each has Remove (`revokeUser`). Pending invites show uses left and expiry, with Extend and Cancel. Giving each person their own one-use invite is what makes this list readable, which is why "one person" is the first choice.
3. **Who can see this.** The current `public` value in plain words, and the four choices. Choosing anything but `none` asks for confirmation with a sentence like "Anyone who has the address will be able to read everything here."
4. **Recovery key.** Rotating the owner token, with the new one shown once. Set the token from `createSpace` as the `ownerToken` property, and the panel opens at this step, once, so the person saves it.

| Attribute or property | Default | Purpose |
|---|---|---|
| `space` | required | the space name. The element renders nothing until it's set, so a page can set it after creating the space |
| `server` | the origin `share.js` was loaded from | API server |
| `base` | the current page | the page invite links open, built with `inviteLink` |
| `members` | off | also show the member line to non-owners |
| `ownerToken` (property only) | none | the token from `createSpace`, to show the recovery key step once |
| `open` | off | open the panel at once instead of showing the button |

- Events: `cubby-invite` with the new `Invite` in `detail`, `cubby-change` after any change to people or settings.
- Styling: it inherits the page's font and colors. `--cubby-accent`, `--cubby-radius` and `::part(button)` and `::part(panel)` adjust the rest.
- The QR code is drawn as SVG by a small built-in encoder (byte mode, error correction M), so there's no request to a third party and the link never leaves the page. An invite link of about 120 characters fits a version 7 code.
- Headless use: `import { qrSvg } from ".../share.js"` returns the SVG markup for any text, for pages with their own UI.
- Budget: under 25 KB unminified, no dependencies. Served and cached like kv.js.

## 18. CLI

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
| Visitor token | `--token` | `CUBBY_TOKEN` | saved automatically |

The server URL defaults to `http://localhost:28229`. A bare port (`9000` or `:9000`) means `http://localhost:9000`, and a host without a scheme gets `http://`.

- Flags win over environment variables, which win over the config file.
- The config directory is `$XDG_CONFIG_HOME/cubby/`, by default `~/.config/cubby/`.
  - `config.toml` holds settings like `server`.
  - `credentials.toml` holds the visitor token and ID per server. It's created with mode 0600. It never holds access or owner tokens.
- The first command that needs a token gets one and saves it: through `new` or `join` when those come first, otherwise with `POST /v1/anon`. Scripts never handle tokens unless they want to.
- The CLI sends its token on every request once it has one, reads included. There's no CORS cost outside a browser, and it makes private spaces work without flags.
- `CUBBY_TOKEN` gives a script a fixed identity, for example a cron job or a CI runner that should always be the same visitor. A token set this way is never saved. To make one for CI, see [Identities for machines](#identities-for-machines).

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
| `limits [space]` | effective server limits, or space limits and caller usage | formatted JSON |
| `time` | sample server time five times; choose shortest RTT | `offset_ms` and `rtt_ms`, tab-separated |
| `series <path>` | read a series' points | one line per point, see Output |
| `pulse <path> [value]` | send a pulse | nothing |
| `pulses <path or pattern> [--exec CMD] [--json]` | print pulses as they arrive, from now on | one line per pulse |
| `new [label or name] [--public P]` | create a space with `POST /v1/spaces`, as its owner. Private unless `--public read\|respond\|all`. A full space name is created as is, anything else is a label | the name, then the owner token on a second line. Keep it, it's shown once |
| `name [label]` | generate a valid space name locally, no request | the name |
| `join <space> <token or link>` | redeem an access or owner token. A link is searched for `t=` in its fragment | your scopes from this grant, one per line |
| `access <space>` | what you may do here | `owner` or `member`, then one line per grant: ID, `until` or `-`, scopes |
| `whoami [--show-token]` | your visitor ID, or with `--show-token` your visitor token, for use with `curl` | the ID or the token |
| `url <path>` | the full URL for a path, for `curl` or a browser | the URL |

Owner commands, each for one space:

| Command | Does | Prints |
|---|---|---|
| `settings <space> [--public P \| --read R --write W]` | print the settings, or change them. `W` is `anyone`, `granted`, or a list like `shared=granted,me=granted` | the settings after the change, as `public`, `read`, `write.shared`, `write.users`, `write.me`, one `name<TAB>value` per line |
| `drop <space>` | delete the space and everything in it. Asks for the name again unless `--yes` | nothing |
| `owner-token <space>` | rotate the owner token | the new owner token |
| `token new <space>` | mint an access token, flags below | the token, or with `--link URL` the link to share |
| `token ls <space>` | list access tokens | one line per token: ID, state, uses left or `-`, until or `-`, grants, label, scopes |
| `token set <space> <id> [--uses N\|unlimited] [--until T\|never]` | change how long and how often it can still be redeemed | nothing |
| `token rm <space> <id>` | revoke a token and every grant from it | nothing |
| `grant ls <space> [--user ID]` | list grants | one line per grant: ID, user, `owner` or `member`, source, until or `-`, scopes |
| `grant add <space> <user> [--allow S ...] [--until T] [--owner]` | a direct grant | the grant ID |
| `grant rm <space> <grant-id>`, `grant rm <space> --user ID` | revoke one grant, or all of a visitor's | nothing |

`token new` flags, all optional, defaults from the server:

| Flag | Default | Meaning |
|---|---|---|
| `--allow SCOPE`, repeatable | `member` | what the grant allows: scopes, or the preset `member` or `viewer` |
| `--uses N\|unlimited` | 10 | how many visitors can redeem it |
| `--until T\|never` | `7d` | how long it can be redeemed: `12h`, `30d`, `2027-01-31`, or an RFC 3339 time |
| `--grant-for D` | until revoked | how long each grant lasts after redeeming |
| `--label L` | none | a note for the token list |
| `--link URL` | none | print `URL` with `t=TOKEN` added to its fragment instead of the bare token |

### Identities for machines

A CI runner, a cron job on another machine or a microcontroller needs a fixed visitor token that holds a grant. Redeem an access token once without an identity and keep the visitor token it returns:

```
$ cubby token new homelab-maple-quilt-tundra-cobalt --allow 'shared/deploy:w' --uses 1 --until 1h --label ci
at_Qz7r...
$ curl -s -X POST -d '{"token": "at_Qz7r..."}' https://kv.example.com/v1/homelab-maple-quilt-tundra-cobalt/join
{"token": "vt_8Rn2...", "id": "u_p1r8t3w6k7m2x9q4", ...}
```

Store `vt_8Rn2...` as the CI secret `CUBBY_TOKEN`, or flash it to the device. The access token is used up and worthless. The grant shows up in `cubby grant ls` labelled by its source token, and `cubby token rm` revokes the machine.

Flags:

| Flag | Commands | Meaning |
|---|---|---|
| `--if-version N` | `set`, `del` | conditional write, `ifVersion=N` |
| `--ttl D` | `set` | lifetime such as 30s, 1h or 7d, whole seconds only; omission clears expiry |
| `--if-before MS` | `set`, `del`, `add` | server deadline in milliseconds |
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
| `--label L` | `new` | as in `POST /v1/spaces` |
| `--public P` | `new` | `none` (default), `read`, `respond` or `all` |
| `--read R`, `--write W` | `new` | the space's settings in full, instead of `--public` |
| `--yes` | `drop` | don't ask for confirmation |

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
- `KV_TIME`, the value time for every key, or the newest point time for a series

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
| 1 | not found: `not_found`, `no_space` |
| 2 | version conflict or passed write deadline |
| 3 | not allowed: `no_access`, `owner_only`, `not_yours`, `invite_invalid`, token problems that a new token didn't fix |
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
cubby admin owner-token <space>    issue a new owner token, for an owner who lost theirs, and print it
cubby admin stats                  totals, visitors, largest spaces
```

- If the server is running, admin commands talk to it through a Unix socket in the data directory, readable only by the server's user. The server does the work, so files are never changed under it.
- If the server isn't running, they open the data files directly.
- There is no admin HTTP API.
- `owner-token` invalidates the old owner token like a rotation, and removes the owner grants that came from it. Grants from creating the space and direct grants stay.
- `admin spaces` shows each space's settings in two columns: `read`, and `write` as `anyone`, `granted`, or a list like `shared=granted`.

## 19. Implementation notes

Guidance for the build, not part of the contract.

### Language and libraries

Go.

- `modernc.org/sqlite`, a pure-Go SQLite, so `CGO_ENABLED=0` builds a fully static binary.
- Cross-compiling is one `GOOS`/`GOARCH` pair per target. Release builds for Linux, macOS and Windows on amd64 and arm64, plus Linux arm (for Raspberry Pi and similar).
- `net/http` from the standard library, using its pattern routing, for both the server and the CLI's client. No web framework.
- `embed` puts `kv.js` into the binary.
- `github.com/coder/websocket` for live connections. Small, no dependencies of its own.
- A goroutine per waiting watch is cheap enough for the 1000-watch limit.
- The binary is expected to be around 15 MB, mostly SQLite. Client machines carry the server code too, which doesn't matter in practice.

### Files

```
data/
├── visitors.db          visitors, token hashes, last seen, which spaces each visitor has data or grants in
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
  k TEXT PRIMARY KEY,                 -- seq, created_at, last_write_at, tombstone_floor,
  v                                   -- settings (JSON), owner_hash
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
  updated_at INTEGER NOT NULL,         -- seconds, for retention
  t       INTEGER,                    -- value time in ms; newest point time for series
  expires INTEGER NOT NULL DEFAULT 0, -- server ms, zero is permanent
  expired INTEGER NOT NULL DEFAULT 0, -- tombstone came from expiry
  n       INTEGER NOT NULL DEFAULT 0, -- number of points, so trimming needs no count
  PRIMARY KEY (area, key)
) WITHOUT ROWID;

CREATE INDEX kv_by_version ON kv (area, version);
CREATE INDEX kv_expiry ON kv (expires) WHERE expires > 0 AND value IS NOT NULL;
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
- Existing v0.10 space files receive the expiry columns and index transactionally at startup, with PRAGMA user_version recording the migration. Old plain-key timestamps are backfilled from updated_at in whole seconds converted to ms; their original millisecond write time is unavailable. Values, versions and access grants are preserved. New writes record milliseconds.
- Reading points is `WHERE area = ? AND key = ? AND t > ? ORDER BY t LIMIT ?` with `since`, or `ORDER BY t DESC LIMIT ?` reversed without it. Both use the primary key.
- Deleting a series key deletes its `points` rows in the same transaction.
- Visitor totals across spaces are the sum of their `usage` rows. See [In-memory state](#in-memory-state).
- `meta` also holds `last_active_at`, and `visitors.db` holds each visitor's `last_seen_at` and whether they've ever stored anything.

### Access tables

Two more tables in each space file, so a space's access travels and disappears with it:

```sql
CREATE TABLE tokens (
  id          TEXT PRIMARY KEY,        -- t_ + 8 base32
  hash        BLOB NOT NULL UNIQUE,    -- SHA-256 of the at_ token
  label       TEXT,
  allow       TEXT NOT NULL,           -- JSON array of scopes, as given
  uses_left   INTEGER,                 -- NULL: no limit
  until       INTEGER,                 -- ms, NULL: no end
  grant_for   INTEGER,                 -- ms, NULL: grants last until revoked
  created_at  INTEGER NOT NULL,
  ended_at    INTEGER,                 -- when it stopped being redeemable, for cleanup
  last_redeemed_at INTEGER
) WITHOUT ROWID;

CREATE TABLE grants (
  id          TEXT PRIMARY KEY,        -- g_ + 8 base32
  visitor     TEXT NOT NULL,
  owner       INTEGER NOT NULL DEFAULT 0,
  allow       TEXT NOT NULL,           -- JSON array of scopes; [] for owner grants
  source      TEXT NOT NULL,           -- 'create', 'owner-token', 'direct', or a token id
  until       INTEGER,                 -- ms, NULL: until revoked
  created_at  INTEGER NOT NULL,
  last_used_at INTEGER
) WITHOUT ROWID;

CREATE INDEX grants_by_visitor ON grants (visitor);
CREATE UNIQUE INDEX grants_once ON grants (visitor, source) WHERE source <> 'direct';
```

- **Redeeming** is one transaction: find the token by hash; if a grant for (visitor, token id) exists, return it; otherwise check `until` and `uses_left`, insert the grant, decrement `uses_left`, set `ended_at` when it reaches 0. The unique index makes a double click safe.
- **Owner token**: `owner_hash` in `meta`. Redeeming it inserts an owner grant with source `owner-token`. Rotating replaces the hash and deletes `owner-token` grants except the caller's.
- **The access check** runs after the path is parsed and the visitor found, and before any data is touched: settings, then grants, then the fixed rules. Keep each open space's settings and a map of visitor to parsed grants in memory, dropped whenever that space's tokens, grants or settings change. The check per request is then a few string prefix comparisons.
- **Grant `until`** is checked on every request, so an ended grant stops at once without waiting for cleanup.
- **visitors.db** tracks which spaces a visitor holds grants in, next to where they have data, so deleting a visitor reaches every grant.
- **No existence oracle.** Code paths for "space missing" and "space private, no read grant" must return the same response, including headers, and take the same miss-limit path. Test this directly.

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
2. Delete visitors unseen past `visitor_idle_ttl`, or past `empty_visitor_idle_ttl` if they never stored anything and hold no grant, with their `u:` and `m:` rows, points and grants in every space they used.
3. Delete grants past their `until`, and access tokens that can't be redeemed, have no grants left and ended over 30 days ago.
4. Delete tombstones older than `tombstone_ttl` and update `tombstone_floor`.
5. Delete space files without activity past `space_idle_ttl`, unless pinned.
6. Drop idle rate-limit buckets and cache entries, as described below.
7. Run `PRAGMA incremental_vacuum` and `PRAGMA wal_checkpoint(TRUNCATE)` on spaces changed since the last run.

### In-memory state

Everything held in memory has a bound, and nothing in it has to survive a restart.

| State | Bound | When it's dropped | Lost on restart means |
|---|---|---|---|
| Open space files | 64 without watchers, plus every space with a waiting watch (at most 1000) | least recently used first | files reopen on demand |
| Notifier per space | one per space with waiting watches | when the last watch on that space ends | watches reconnect and continue from their version |
| Waiting watches | 50 per IP, 1000 total | on answer, timeout or disconnect | clients reconnect |
| Rate-limit buckets, per IP and per visitor | 100,000 entries | by the hourly job once full and untouched for an hour, or least recently used when the cap is reached | everyone starts with a full bucket again |
| Settings and parsed grants per space | one per open space file | with the space file, or when its access changes | reloaded from the space file |
| Storage used per visitor | 100,000 entries | least recently used | recounted from that visitor's `usage` rows on their next write |
| Pending "last seen" and "last active" times | one entry per active visitor and space | written to disk by the hourly job and on shutdown | up to an hour of activity isn't recorded, which only matters for retention measured in months |

- A bucket that's full and untouched behaves exactly like a new one, so dropping it changes nothing. Dropping one early under the cap is slightly lenient towards that IP or visitor, which is acceptable.
- Request bodies are read only up to the value limit plus one byte, so a single request can't use more than about 64 KB of body memory.

## 20. Later

Ideas from the design discussion that were left out to keep the first version small. Each is an addition, and none changes the API above. Items that are agreed and planned, with their design, are in [plans.md](plans.md).

### Abuse protection

- **Proof of work on `/v1/anon`.** The server answers 428 with a signed challenge and a difficulty. The client finds a nonce so that `SHA-256(challenge + nonce)` has that many leading zero bits, and posts it back. A real visitor pays well under a second once. Minting tokens in bulk costs CPU per token, independent of IPs. Switching it on is one config value, so the JS client should handle 428 from the start.
- **Who may create spaces.** Today any visitor can, within the new-space limits. A config switch could require a creator token issued by the operator (`cubby admin creator new`), redeemed like an access token, which gives the visitor the right to call `POST /v1/spaces`. That's the remaining part of the old "closed server" idea: everything else about it is covered by private spaces. Server-wide default settings for new spaces would go with it.
- **Origin allowlists** per space: only requests from listed `Origin`s accepted. Weak against scripts, which set any header, but it stops other sites embedding a public space.

### Key options

- **`lock=owner`** on shared keys: only the creator of a key may change it. Mostly covered by `users/`.
- **An `add` right** in scopes, for create-only writes. Considered with grants and not needed: `users/{id}/` already gives "add your own, nobody else can touch it", and owners can delete anything. The one real gap is "anyone adds, only the owner reads", which is the `inbox` area below.

### Sharing and identity

- **Device linking.** Device A gets a short code that's valid for 10 minutes. Device B redeems it with its own token and becomes the same visitor. Brings `me/` and grants along to other devices, and gives "your devices" and "sign out others". Until then, a person on a second device redeems the same invite link again, which works within its uses.
- **Read-only links per room, without operator help.** Spaces whose ID is a hash of a write key, so a page can derive a read link from an edit link with no server state. Explored in depth and dropped because it needs hashing, fragment conventions and remembered keys in the client.
- **Visitor IDs that differ per space**, derived with an HMAC, so a visitor can't be linked across spaces.

### Access

- **Short invite codes.** An access token as a short code like `K7M2-X9Q4`, for typing into a device or reading out loud. It needs a tighter use limit and expiry, and redeem attempts limited per space, since 40 bits can be guessed where 256 can't.
- **Signed owner keys.** The owner keeps an Ed25519 private key and the space stores only the public key. The owner signs grants on their own machine, and the server never holds anything that can make an owner, so a leaked database can't take over spaces. Plain signatures over a small payload, or Biscuit if holders should narrow tokens offline. JWT is the wrong fit: tokens here are redeemed once and turned into server state, so stateless verification buys nothing, and JWT adds size and known pitfalls.
- **Signed requests** for devices that can't do TLS. `Authorization: KV-HMAC id=..., t=..., sig=...`, signing method, path, time and body hash with a per-visitor secret. The secret never crosses the network and captured requests can't be replayed outside a short window. Values are still readable in transit.
- **Per-grant quotas and rate limits**, for a trusted device that writes more than an anonymous visitor may.

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
