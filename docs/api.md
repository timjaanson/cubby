# cubby HTTP API

For clients other than kv.js and the CLI. Everything is under `/v1/`, JSON in and out, CORS open to every origin. [usage.md](usage.md) explains the model.

## Signing in

A **device** is a key pair: ES256 (ECDSA P-256) or EdDSA (Ed25519). The server stores the public key; its ID is the key's JWK thumbprint (RFC 7638). Each request carries a JWT the device signed itself:

```
header  {"alg": "ES256", "typ": "JWT", "kid": "<device id>"}
payload {"aud": "https://cubby.example.com", "iat": 1794787200, "exp": 1794790800}
```

- `alg` must match the key: `ES256` for P-256, `EdDSA` for Ed25519. ES256 signatures are raw `r || s`, 64 bytes, not DER.
- `aud` is the server's public origin (`public_url` in its config, else the origin the request came in on).
- `exp - iat` at most 3600 s. `iat` at most 5 minutes ahead, `exp` not passed (5 minutes of skew allowed).
- `GET /v1/time` returns `{"t": ms}` with no auth, for a client whose clock is off.

**Registering.** The first request from a new key is `POST /v1/visitors`, signed by that key with the public JWK in the header as `jwk` (as ACME does), optional body `{"label": "Firefox on Linux"}`. It answers 201 `{"id": "u_...", "device": "<kid>"}`, or 200 if the key is already registered.

**Sending.** `Authorization: Bearer <jwt>` on any request, or `?token=<jwt>` on reads (which avoids a CORS preflight). On a WebSocket, the first message is `{"op": "auth", "token": "<jwt>"}`.

**Refused.** A credential that was sent and refused is a 401 on every route except `/time`, `/limits`, `/visitors` and `/pair`, reads of open spaces included, with `WWW-Authenticate: Bearer error="invalid_token"` and one of: `token_expired`, `token_not_yet_valid`, `wrong_audience`, `token_too_long`, `bad_signature`, `bad_token`, `unknown_key` (never registered, or signed out), `invalid_api_key`. No credential at all is anonymous, and routes that need a visitor answer `missing_token`.

**API keys.** `cubby_...` static secrets, sent the same way. One space, read and write only; see [below](#api-keys).

**Credentials file**, as `cubby device backup` writes it and `cubby login` and kv.js `credentials` read it:

```json
{"server": "https://cubby.example.com", "visitor": "u_...", "device": "<kid>", "key": {"kty": "EC", "crv": "P-256", "x": "...", "y": "...", "d": "..."}}
```

Examples that mint tokens with jose, PyJWT and openssl are in `tools/interop/`.

## Conventions

- A **time** is milliseconds since 1970. A **duration** is whole seconds, or a string `"90s"`, `"10m"`, `"12h"`, `"14d"`.
- `ttl` is how long something lives, wherever something ends: keys, series points, invites, the access an invite gives (`accessTtl`), member levels, bans, API keys, pairing codes. `null` means no end where that's allowed. Answers say when it ends as `expires` (a time, or `null`).
- A paged read takes `limit`, `order=asc|desc` and `after`, and answers `{"items": [...], "next"}`. Pass `next` back as `after` for the following page; `null` means there's no more. `next` is opaque. Key lists, series and your spaces work this way.
- Order: what's sorted by time comes newest first (series, your spaces), what's sorted by name A to Z (key lists). `order` turns either around. Streams deliver in the order things happened.
- A stream takes `after` and `timeout` (seconds) and answers at once if there's something after the cursor, else when something arrives or the timeout passes. Watches and pulse listeners work this way.
- Record fields: `created` when something was made, `since` when a membership began, `expires` when something ends, `lastSeen` for devices and members, `lastUsed` for invites and API keys.
- Unknown or repeated query parameters and unknown JSON fields are 400 `invalid_parameter`, so a typo never turns a conditional write into a plain one. `_` is ignored for cache busting.

## Routes

| Route | Does |
|---|---|
| `GET /health` | `{"ok": true, "version"}` |
| `GET /kv.js`, `/share.js` | the JS client and the share panel |
| `GET /v1/time`, `/v1/limits` | server time `{"t"}`; the server's limits, named as in its config file |
| `POST /v1/visitors` | register a key, above |
| `GET /v1/me` | `{"id", "device"}` |
| `GET /v1/me/devices` | `{"id", "device", "items": [{"id", "label", "created", "lastSeen", "pairedBy", "current"}]}` |
| `PATCH /v1/me/devices/{id or current}` | `{"label"}` |
| `DELETE /v1/me/devices/{id or current}` | sign it out; 409 `last_device` for the last one |
| `DELETE /v1/me/devices?others=1` | sign out every other device: `{"removed": [...]}` |
| `POST /v1/me/pairing-codes` | `{"ttl"}` (30 s to 1 h, default 10 min) → `{"id", "code", "display", "created", "expires", "state"}`. At most 3 open |
| `GET /v1/me/pairing-codes/{id}?timeout=55` | waits until used: `"state": "used"`, `"device"` |
| `DELETE /v1/me/pairing-codes/{id}` | cancel |
| `POST /v1/pair` | `{"code", "label"}`, signed by the new key with `jwk` in the header → `{"id", "device", "previous", "deleted"}` |
| `POST /v1/me/api-keys` | `{"space", "label", "access", "ttl"}` → `{"id", "key", ...}`, the key shown once, [below](#api-keys) |
| `GET /v1/me/api-keys`, `DELETE /v1/me/api-keys/{id}` | list, revoke |
| `POST /v1/spaces` | `{"prefix" or "name", "anyone", "inviters"}`, all optional → 201 `{"space", "id", "settings"}`, you are owner |
| `GET /v1/spaces` | the spaces you belong to, a paged read, [below](#your-spaces) |
| `GET /v1/{space}` | `{"space", "id", "access", "expires", "canInvite", "settings", "usage", "limits"}`: your level and when it ends |
| `PATCH /v1/{space}` | owners: `{"anyone", "inviters"}` → `{"settings"}` |
| `DELETE /v1/{space}` | owners: delete the space |
| `POST /v1/{space}/join` | `{"invite": "inv_..."}` → `{"space", "id", "access", "expires", "settings"}` |
| `POST /v1/{space}/invites` | `{"label", "access", "uses", "ttl", "accessTtl"}` → the invite record plus `"invite": "inv_..."`, [below](#invites) |
| `GET /v1/{space}/invites`, `PATCH`/`DELETE /invites/{id}` | yours, or all for owners; `PATCH {"uses", "ttl"}`; delete revokes it and its grants |
| `GET /v1/{space}/members` | `{"items": [{"id", "access", "since"}]}`, highest level first; owners also see `expires`, `invitedBy`, `lastSeen`, `label` and bans |
| `PUT /v1/{space}/members/{id}` | owners: `{"access": "read"|"post"|"all"|"owner"|"banned", "ttl"}` |
| `DELETE /v1/{space}/members/{id or me}` | owners remove; `me` leaves |
| `GET /v1/{space}/live` | WebSocket, [below](#live-connection) |
| `POST /v1/{space}/batch` | several writes, all or nothing, [below](#batch) |
| `/v1/{space}/{area}/{key or pattern}[/watch|/series|/pulse]` | data, below |

Anything else answers the same bare 404 `not_found`. Space names (four words or more, 16+ characters) can't collide with `me`, `spaces`, `visitors`, `pair`, `time` or `limits`.

## Data

Areas: `shared/{key}`, `by/{id|me}/{key}`, `to/{id|me|owners}/{key}` (send), `inbox/{sender|*}/{key}` and `inbox/owners/{sender|*}/{key}` (read and delete what was sent to you). A pattern is a key prefix ending in `*`. `by/*/`, `to/*/` and `inbox/*/` read across every author, recipient or sender.

| Request | Answer |
|---|---|
| `GET .../{key}` | the value; `ETag: "version"`, `KV-By`, `KV-Time`, `KV-Expires` |
| `PUT .../{key}` body = value | 200 or 201 `{"version"}`. Query: `ifVersion` (0 = only if missing), `ttl` (5 s to 90 d), `ifBefore` (a time) |
| `DELETE .../{key}` | `{"version"}`. Query: `ifVersion`, `ifBefore` |
| `GET .../{pattern}` | paged, in key order: `{"items": [{"key", "value", "version", "by", "t"?, "expires"?}], "version", "next"}`. `limit` ≤ 1000 (default 1000) |
| `GET .../{pattern}/watch` | a stream, below |
| `POST .../{key}/series` body = value | add a point: `{"version", "t"}`. Query: `t` (the reading's own time; the same `t` replaces that point), `ttl` (after `t`), `ifBefore` |
| `GET .../{key}/series` | paged, newest first: `{"items": [{"t", "value", "expires"?}], "version", "next"}`. Query: `since` (inclusive), `before` (exclusive), `order=asc` for oldest first, `limit` ≤ 10000 (default 1000) |
| `GET .../{prefix*}/series` | every series under the prefix (at most 100), merged by time, ties by key: items carry `"key"`. Same query |
| `POST .../{key}/pulse` body = value | 202 `{"t"}`, not stored |
| `GET .../{pattern}/pulse?after=SEQ` | a stream: `{"pulses": [{"key", "by", "value", "t"}], "seq", "missed"}` |

- A value is UTF-8, not empty, at most 512 KB (points and pulses 4 KB). Valid JSON is stored and returned as `application/json`, anything else as `text/plain`, byte for byte. In lists and watches, text values are JSON strings.
- Versions grow per space. Pass the `version` of a key's own entry as `ifVersion`, and a list's or watch's `version` as a watch's `after`.
- A series keeps its newest `max_series_points` (1,000,000 by default). A point with `ttl` is removed at `t + ttl`; one whose `t + ttl` has already passed is refused (400, `parameter: "ttl"`). When the newest point goes, the key's value becomes the next newest under a new version; when the last goes, the key is removed and watchers see `"expired": true`.
- A series page stops at `limit` or 4 MB of values. Points go to several series at once through a [batch](#batch); a pattern reads them back together. More than 100 series under one prefix is 400 `too_many_series`.

### Watching

`GET .../{key or pattern}/watch?after=N&timeout=25` answers with every matching key whose version is above N:

```json
{"changes": [{"key": "msg.1", "value": {"text": "hey"}, "version": 58, "by": "u_..."},
             {"key": "vote", "deleted": true, "version": 59, "by": "u_..."}],
 "version": 59, "more": false}
```

- `after=0`, the default, is everything that exists now, then changes. `after=now` is only what changes from here on.
- It answers at once if something changed after N, else waits up to `timeout` (1 to 55 s) and answers with no changes.
- Send the answer's `version` as `after` next time and nothing is lost. Several writes to one key between requests come back as its latest state. `more: true` means ask again at once.
- Deletes are kept 7 days. An older `after` answers 410 `version_expired`: start again from 0.
- A pulse listener is the same with `after=seq`, from now by default. The server keeps 10 seconds of pulses per space; `missed: true` means some fell out.

### Batch

`POST /v1/{space}/batch` with a JSON array of 1 to 100 writes. Each names what it does and its path, with the same options as the single request:

```json
[{"check": "shared/state", "ifVersion": 40},
 {"set": "shared/title", "value": "Trip"},
 {"set": "by/me/cursor", "text": "plain text", "ttl": 60},
 {"delete": "shared/draft", "ifVersion": 41},
 {"add": "shared/temp", "value": 21.4, "t": 1794927600000, "ttl": "14d"}]
```
→ `200 {"version": 59, "items": [{"path", "version", "created"?, "t"?}, ...]}`, in request order; `t` for points.

- `set` takes `value` (or `text` for a plain-text value), `ifVersion`, `ttl`, `ifBefore`. `delete` takes `ifVersion`, `ifBefore`. `add` takes `value` (or `text`), `t`, `ttl`, `ifBefore`.
- `check` takes `ifVersion` only and writes nothing: the batch fails with `version_conflict` unless the key still has that version (`0`: doesn't exist). It needs read access, sees the batch's earlier writes, and answers the key's version in `items`. Use it for a key the batch depends on but doesn't write; a written key checks itself with `ifVersion`.
- All or nothing. Every write is checked exactly as if it came alone (path, access, parameters, value, quotas), and the first failure answers its usual error plus `"index"`. Nothing is written then.
- Each key once (checks included), except points to one series that each give a different `t`. Points without `t` share one server time. Anything else is 400 `bad_batch` with `index`.
- Changed keys take consecutive versions in array order, in one transaction: a watch sees all of them or none, in one answer unless it pages (`more: true`). An identical rewrite keeps its version.
- Counts as one write for rate limits. The body is at most twice the largest value plus 64 KB.
- On a live connection: `{"id": 5, "method": "POST", "path": "batch", "body": [...]}`.

### Live connection

`GET /v1/{space}/live` upgrades to a WebSocket that carries the same data requests. First message: `{"id": 1, "op": "auth", "token": "<jwt>"}` (or an `Authorization` header on the upgrade; never `?token=`).

```
→ {"id":2,"method":"PUT","path":"shared/note","query":{"ifVersion":17},"body":{"text":"hi"}}
← {"id":2,"status":200,"headers":{"ETag":"\"18\""},"body":{"version":18}}
→ {"id":3,"method":"GET","path":"shared/board.*/watch","query":{"after":0}}
← {"id":3,"status":200,"body":{"changes":[...],"version":40,"more":false}}     then a push per change
→ {"op":"stop","watch":3}
→ {"id":4,"op":"will","path":"by/me/here/pulse","body":{"left":true}}   sent by the server when the connection ends
```

- Replies are what HTTP would answer, in an envelope with the same `id`. `text` instead of `body` sends a text value.
- Watches and pulse listeners push until `stop`; at most 20 per connection, 8 wills.
- `auth` may come again at any time. Banned: a 403 `banned` frame, then close 4403. A signed-out device: data messages answer `unknown_key` until a new `auth`.
- The server pings every 30 s. Close codes: 1001 restarting (reconnect), 1009 too large, 4408 no first message in 10 s.

## Invites

- Minted by owners, and by members at the space's `inviters` level (`post` unless changed). Members see and change only their own.
- `access` defaults to `all`, or your own level if lower. Nobody gives more than they have. `uses` defaults to no limit, `ttl` to 7 days. `accessTtl` makes the access it gives end that long after it's redeemed.
- `"access": "owner"`: owners only, `uses` 1, `ttl` at most 7 days, else 400 `invalid_parameter`. A `PATCH` keeps it within those bounds.
- The record: `{"id", "label", "access", "uses" (left), "expires", "accessTtl", "state" (active, expired, used_up, revoked), "members", "created", "lastUsed", "by"}`.
- An invite travels in a link as `#space=NAME&invite=inv_...`; kv.js and the CLI make and read that form.

## Your spaces

`GET /v1/spaces?limit=100&after=<next>` (signed in, not with an API key) lists the spaces where you hold a grant, owner included, most recently active first:

```json
{"items": [{"space": "golf-lamp-sat-hazard-milky", "access": "owner", "via": "created",
            "since": 1794787200000, "expires": null, "members": 3,
            "lastActive": 1794790000000, "bytes": 18234, "keys": 41,
            "settings": {"anyone": "none", "inviters": "post"}}],
 "next": null}
```

- `since`: when this membership began. Grants belong to the visitor, so every paired device sees the same list.
- `via`: `created`, `invite` or `added` (by an owner). `access` and `expires` are your current level and its end.
- `limit` 1 to 1000 (default 100). A space that becomes active while you page can move.
- Open spaces you wrote to without a grant aren't listed. Leaving, removal, bans, expiry and deletion take a space off at once.

## API keys

- Minted with a signed request: `POST /v1/me/api-keys {"space": "...", "label": "...", "access": "post", "ttl": "30d"}`. `access` is `read`, `post` or `all`, at most your level, and defaults as an invite's does. `ttl` defaults to 30 days; `null` never expires and the answer carries a `warning`.
- The server keeps only a hash. The key belongs to the device that made it: signing that device out ends it.
- It reads and writes in its space, and reads `GET /v1/{space}`. Anything else, including another space, answers 403 `api_key_not_allowed` or `api_key_space`.

## Errors

`{"error": "code", "message": "...", ...}`. Branch on `error`, never on `message`.

| Status | `error` |
|---|---|
| 400 | `invalid_space` (+`suggestion`), `invalid_key`, `too_many_series` (+`limit`), `invalid_parameter` (+`parameter`), `invalid_settings`, `invalid_encoding`, `empty_value`, `json_too_deep`, `bad_batch` (+`index`), `bad_key`, `bad_message`, `id_in_use` |
| 401 | `missing_token`, `unknown_key`, `token_expired`, `token_not_yet_valid`, `wrong_audience`, `token_too_long`, `bad_signature`, `bad_token`, `invalid_api_key` (+`serverTime`) |
| 403 | `no_access` (+`need`), `owner_only`, `not_yours`, `not_member`, `exceeds_rights`, `banned` (+`expires`), `api_key_not_allowed`, `api_key_space` |
| 404 | `no_space` (missing, or private and not yours to see), `not_found`, `invite_invalid`, `no_visitor`, `bad_pairing_code` |
| 405 | `method_not_allowed` (+`allow`) |
| 409 | `space_exists`, `is_series`, `not_series`, `last_device`, `same_visitor`, `too_many_pairing_codes`, `identity_changed` |
| 410 | `version_expired`, `pairing_code_used`, `pairing_code_expired`, `pairing_code_cancelled` |
| 412 | `version_conflict` (+`currentVersion`), `too_late` (+`t`) |
| 413 | `value_too_large` (+`limit`) |
| 429 | `rate_limited`, `stored_limit`, `space_limit`, `too_many_watches`, `too_many_wills` (+`retryAfter`, `Retry-After`) |
| 507 | `visitor_quota_exceeded`, `space_quota_exceeded`, `server_full` |

Data writes carry `RateLimit` and `RateLimit-Policy` headers with what's left. `GET /v1/limits` and `GET /v1/{space}` report the actual limits, which the operator can change.
