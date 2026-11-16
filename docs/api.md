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

## Routes

| Route | Does |
|---|---|
| `GET /health` | `{"ok": true, "version"}` |
| `GET /kv.js`, `/share.js` | the JS client and the share panel |
| `GET /v1/time`, `/v1/limits` | server time; the server's limits |
| `POST /v1/visitors` | register a key, above |
| `GET /v1/me` | `{"id", "device"}` |
| `GET /v1/me/devices` | `{"id", "device", "items": [{"id", "label", "created", "lastSeen", "pairedBy", "current"}]}` |
| `PATCH /v1/me/devices/{id or current}` | `{"label"}` |
| `DELETE /v1/me/devices/{id or current}` | sign it out; 409 `last_device` for the last one |
| `DELETE /v1/me/devices?others=1` | sign out every other device: `{"removed": [...]}` |
| `POST /v1/me/pairing-codes` | optional `{"ttl": 30..3600}` (default 600) → `{"id", "code", "display", "expires", "state"}`. At most 3 open |
| `GET /v1/me/pairing-codes/{id}?wait=55` | waits until used: `"state": "used"`, `"device"` |
| `DELETE /v1/me/pairing-codes/{id}` | cancel |
| `POST /v1/pair` | `{"code", "label"}` or the code as the body, signed by the new key with `jwk` in the header → `{"id", "device", "previous", "deleted"}` |
| `POST /v1/me/api-keys` | `{"space", "label", "access", "until"}` → `{"id", "key", ...}`, the key shown once |
| `GET /v1/me/api-keys`, `DELETE /v1/me/api-keys/{id}` | list, revoke |
| `POST /v1/spaces` | `{"label", "name", "anyone", "invite"}`, all optional → 201 `{"space", "id", "settings"}`, you are owner |
| `GET /v1/{space}/access` | `{"id", "access", "canInvite", "settings", "usage", "limits"}` |
| `PATCH /v1/{space}` | owners: `{"anyone", "invite"}` |
| `DELETE /v1/{space}` | owners: delete the space |
| `POST /v1/{space}/join` | `{"invite": "inv_..."}` → `{"space", "id", "access", "until", "settings"}` |
| `POST /v1/{space}/invites` | `{"label", "access", "uses", "until", "grantFor"}` → `{"id", "invite": "inv_...", ...}`. Defaults: all (or your level if lower), 10 uses, 7 days |
| `GET /v1/{space}/invites`, `PATCH`/`DELETE /invites/{id}` | yours, or all for owners; `PATCH {"uses", "until"}`; delete revokes it and its grants |
| `GET /v1/{space}/members` | `{"items": [{"id", "access", "since", ...}]}`, owners also see `until`, `invitedBy`, bans |
| `PUT /v1/{space}/members/{id}` | owners: `{"access": "read"|"post"|"all"|"owner"|"banned", "until"}` |
| `DELETE /v1/{space}/members/{id or me}` | owners remove; `me` leaves |
| `GET /v1/{space}/live` | WebSocket, [below](#live-connection) |
| `/v1/{space}/{area}/{key or pattern}[/watch|/series|/pulse]` | data, below |

Anything else answers the same bare 404 `not_found`. Space names (four words or more, 16+ characters) can't collide with `me`, `spaces`, `visitors`, `pair`, `time` or `limits`.

`until` takes milliseconds, `"12h"`, `"30d"`, a date, an RFC 3339 time, or `null` for no end.

## Data

Areas: `shared/{key}`, `by/{id|me}/{key}`, `to/{id|me|owners}/{key}` (send), `inbox/{sender|*}/{key}` and `inbox/owners/{sender|*}/{key}` (read and delete what was sent to you). A pattern is a key prefix ending in `*`. `by/*/{key}` reads every visitor's.

| Request | Answer |
|---|---|
| `GET .../{key}` | the value; `ETag: "version"`, `KV-By`, `KV-Time`, `KV-Expires` |
| `PUT .../{key}` body = value | 200 or 201 `{"version"}`. Query: `ifVersion` (0 = only if missing), `ttl` (5 s to 90 d), `ifBefore` (ms) |
| `DELETE .../{key}` | `ifVersion`, `ifBefore` |
| `GET .../{pattern}` | `{"items": [{"key", "value", "version", "by", "t"?, "expires"?}], "version", "next"}`. Query: `limit` (≤1000), `order=desc`, `after=<next>` |
| `GET .../{pattern}/watch?version=N` | long poll, below |
| `POST .../{key}/series` body = value | add a point; `?t=ms` for a late reading |
| `POST .../{prefix*}/series` body = object | a point in `prefix + name` for each member, up to 100 |
| `GET .../{key}/series?since=ms&limit=N` | `{"key", "points": [{"t", "value"}], "version", "next"}` |
| `POST .../{key}/pulse` body = value | 202 `{"t"}`, not stored |
| `GET .../{pattern}/pulse?after=SEQ` | long poll: `{"pulses": [{"key", "by", "value", "t"}], "seq", "missed"}` |

- A value is UTF-8, not empty, at most 512 KB (points and pulses 4 KB). Valid JSON is stored and returned as `application/json`, anything else as `text/plain`, byte for byte. In lists and watches, text values are JSON strings.
- Versions grow per space. Pass the `version` of a key's own entry as `ifVersion`.
- Unknown or repeated query parameters and unknown JSON fields are 400 `invalid_parameter`, so a typo never turns a conditional write into a plain one. `_` is ignored for cache busting.

### Watching

`GET .../{key or pattern}/watch?version=N&timeout=25` answers with every matching key whose version is above N:

```json
{"changes": [{"key": "msg.1", "value": {"text": "hey"}, "version": 58, "by": "u_..."},
             {"key": "vote", "deleted": true, "version": 59, "by": "u_..."}],
 "version": 59, "more": false}
```

- `version=0` is a snapshot of what exists now. No `version` means changes from now on.
- It answers at once if something changed after N, else waits up to `timeout` (1 to 55 s) and answers with no changes.
- Send the answer's `version` next time and nothing is lost. Several writes to one key between requests come back as its latest state. `more: true` means ask again at once.
- Deletes are kept 7 days. An older `version` answers 410 `version_expired`: start again from 0.
- A pulse listener is the same with `after=seq`. The server keeps 10 seconds of pulses per space; `missed: true` means some fell out.

### Live connection

`GET /v1/{space}/live` upgrades to a WebSocket that carries the same data requests. First message: `{"id": 1, "op": "auth", "token": "<jwt>"}` (or an `Authorization` header on the upgrade; never `?token=`).

```
→ {"id":2,"method":"PUT","path":"shared/note","query":{"ifVersion":17},"body":{"text":"hi"}}
← {"id":2,"status":200,"headers":{"ETag":"\"18\""},"body":{"version":18}}
→ {"id":3,"method":"GET","path":"shared/board.*/watch","query":{"version":0}}
← {"id":3,"status":200,"body":{"changes":[...],"version":40,"more":false}}     then a push per change
→ {"op":"stop","watch":3}
→ {"id":4,"op":"will","path":"by/me/here/pulse","body":{"left":true}}   sent by the server when the connection ends
```

- Replies are what HTTP would answer, in an envelope with the same `id`. `text` instead of `body` sends a text value.
- Watches and pulse listeners push until `stop`; at most 20 per connection, 8 wills.
- `auth` may come again at any time. Banned: a 403 `banned` frame, then close 4403. A signed-out device: data messages answer `unknown_key` until a new `auth`.
- The server pings every 30 s. Close codes: 1001 restarting (reconnect), 1009 too large, 4408 no first message in 10 s.

## API keys

- Minted with a signed request: `POST /v1/me/api-keys {"space": "...", "label": "...", "access": "post", "until": "30d"}`. `access` is `read`, `post` (default) or `all`, at most your level. `until` defaults to 30 days; `null` never expires and the answer carries a `warning`.
- The server keeps only a hash. The key belongs to the device that made it: signing that device out ends it.
- It reads and writes in its space only. Anything else, including another space, answers 403 `api_key_not_allowed` or `api_key_space`.

## Errors

`{"error": "code", "message": "...", ...}`. Branch on `error`, never on `message`.

| Status | `error` |
|---|---|
| 400 | `invalid_space` (+`suggestion`), `invalid_key`, `invalid_parameter` (+`parameter`), `invalid_settings`, `invalid_encoding`, `empty_value`, `json_too_deep`, `batch_needs_object`, `bad_key`, `bad_message`, `id_in_use` |
| 401 | `missing_token`, `unknown_key`, `token_expired`, `token_not_yet_valid`, `wrong_audience`, `token_too_long`, `bad_signature`, `bad_token`, `invalid_api_key` (+`serverTime`) |
| 403 | `no_access` (+`need`), `owner_only`, `not_yours`, `not_member`, `exceeds_rights`, `banned` (+`until`), `api_key_not_allowed`, `api_key_space` |
| 404 | `no_space` (missing, or private and not yours to see), `not_found`, `invite_invalid`, `no_visitor`, `bad_pairing_code` |
| 405 | `method_not_allowed` (+`allow`) |
| 409 | `space_exists`, `is_series`, `not_series`, `last_device`, `same_visitor`, `too_many_pairing_codes`, `identity_changed` |
| 410 | `version_expired`, `pairing_code_used`, `pairing_code_expired`, `pairing_code_cancelled` |
| 412 | `version_conflict` (+`currentVersion`), `too_late` (+`t`) |
| 413 | `value_too_large` (+`limit`) |
| 429 | `rate_limited`, `stored_limit`, `space_limit`, `too_many_watches`, `too_many_wills` (+`retryAfter`, `Retry-After`) |
| 507 | `visitor_quota_exceeded`, `space_quota_exceeded`, `server_full` |

Data writes carry `RateLimit` and `RateLimit-Policy` headers with what's left. `GET /v1/limits` and `/access` report the actual limits, which the operator can change.
