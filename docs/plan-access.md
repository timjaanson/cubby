# Plan: the access model (v0.10.0)

The work to build what [spec.md](spec.md) sections 3 to 6 and 16 describe: owners, space settings with private as the default, the `public` shorthand, access tokens, grants, the client's `open` and `share`, and the `/share.js` panel. Delete this file once it's built and the README matches.

## Ground rules

- **Build it clean.** No data on the live server is worth keeping, so there's no migration, no compatibility code, no old routes kept alive and no conversion of old space files. The deploy wipes the data directory.
- **One code path per decision.** Access is decided in one function, private-space answers come from one function, and the JS client and CLI don't re-implement the rules beyond showing and hiding controls.
- **The spec is the contract.** If building shows that something in it is wrong, change the spec in the same commit, so docs and code never disagree.

## What gets removed

| Thing | Where |
|---|---|
| Locked spaces, `?locked`, `sk_` keys, `Space-Key` header, `space_locked` | server handlers, store (`locked`, key hash in `meta`), CORS headers, CLI `--locked`, `--space-key`, `CUBBY_SPACE_KEY`, `SpaceKeys` in credentials, kv.js `key` option |
| Creating a space by writing to it | store write path, miss-limit special case for "write that creates a space" |
| `cubby admin rekey` | admin socket route, CLI |
| Pre-v0.8.0 space names (`names.SafeSpaceFile` and the admin and store paths for names under 16 characters) | names, store, admin. Every space on disk now has a valid API name |
| `kv_` token prefix | names (`TokenRe`), server, CLI, kv.js |
| `not_owner` error code | renamed `not_yours` |
| `cubby token` printing the visitor token | becomes `cubby whoami --token`; `token` is now the access-token command group |
| "Listing a missing space returns an empty list" | missing spaces answer 404 `no_space` everywhere |

## Server and store

1. **Names and formats.** `vt_`, `at_`, `ot_` tokens (prefix plus 43 base64url characters), `t_` and `g_` IDs (8 base32 characters). One shape check per kind.
2. **Space file.** `meta` gets `settings` (JSON) and `owner_hash`. New tables `tokens` and `grants` with the indexes in spec §18. New space files only; no upgrade path for old ones.
3. **visitors.db / server.db.** Visitor token hash with the new prefix. Track spaces a visitor holds grants in, next to spaces they have data in.
4. **Create.** `POST /v1/spaces` with a JSON body: label or name, `public` or `read`/`write`, validation (`invalid_settings`), owner grant with source `create`, owner token, and visitor creation when no token is sent. **Default settings are private** (`public: none`).
   - **Settings representation.** One function maps between `public` (`none`, `read`, `respond`, `all`) and `read`/`write`, and names anything else `custom`. Every response with settings carries all three fields.
5. **Access check.** One function: (space settings, visitor, grants, method, area, target, owner) → allow, `no_access`, `not_yours`, or "can't read". Cached settings and parsed grants per open space, dropped on any access change. Grant `until` checked per request.
6. **Private-space answers.** One function produces the `no_space` response, with the message that names both causes (spec §4), and runs the miss accounting. Every route calls it for both "missing" and "can't read".
7. **Join.** Redeem transaction (spec §18), idempotent per visitor, owner-token path, visitor creation, `invite_invalid` plus one miss on any failure.
8. **Access route.** `GET /v1/{space}/access`.
9. **Owner routes.** `PATCH`/`DELETE /v1/{space}`, tokens (create, list, patch, delete with its grants), grants (list, direct add, delete by id, delete by user), owner-token rotation. `owner_only` versus `no_space` as in spec §6.
10. **Moderation.** Owners may `DELETE` in any `users/{id}/`.
11. **Defaults and presets.** Access token: `allow` `member` (`["*:rw"]`), `uses` 10, `until` 7 days, `grantFor` null. `allow` presets `member` and `viewer` (`["shared:r", "users:r"]`), expanded on input, shown expanded. Parsing of durations, dates and RFC 3339 times shared with the CLI.
12. **Limits.** New visitors 30 per hour per IP across `/anon`, `/spaces` and `join`. 1000 tokens and 10,000 grants per space, 20 scopes per token or grant. Unknown JSON fields rejected.
13. **Cleanup.** Expired grants, dead tokens 30 days after ending with no grants left, visitor deletion reaching grants, `empty_visitor_idle_ttl` skipping visitors with grants.
14. **Admin.** `cubby admin owner-token <space>`. `admin spaces` shows `read` and `write` columns.
15. **CORS and logging.** Drop `Space-Key` from allowed headers. Never log bodies of `join`, `tokens` or `owner-token`.

## JS client (web/kv.js)

- The guide comment from spec §16, word for word, at the top of the file.
- Exports `kv, createSpace, takeInvite, inviteLink, newName, validSpace, KvError`.
- Identity from `createSpace` and `join` responses, saved before returning.
- Reads, lists and watches send `?token=` whenever there's a token.
- `db.open()` (takeInvite, join, access; never throws for `no_space` or `invite_invalid`), `db.share()`, `db.join`, `db.access` with local `can()`, `db.owner.*`.
- `db.user(id).delete` allowed for owners, other writes to someone else's area still throw `not_yours` locally.
- Remove the `key` option and all locked-space handling.

## Share component (web/share.js), second step

Built after the server, kv.js and CLI work, as its own commit, since nothing else depends on it.

- `<cubby-share>` custom element with shadow DOM, as in spec §16: Invite (one person, group link, view only, more options), People, Who can see this, Recovery key.
- A small QR encoder, byte mode, error correction M, output as SVG. Port a known-good one (Nayuki's QR Code generator is MIT and small) rather than writing one, and test it against decoded output.
- `qrSvg()` exported for headless use. Events `cubby-invite` and `cubby-change`.
- Served at `/share.js` from the binary, cached like kv.js. Under 25 KB unminified.
- Manual test page in `web/` covering owner, member and outsider views.

## Docs for agents

- [agents.md](agents.md) is written. Keep it in step with the guide comment and link it from the README.

## CLI

- `new` with `--public` (default private) or `--read` and `--write`, printing the name and the owner token.
- `settings --public`. `token new --allow` accepts `member` and `viewer`.
- `join`, `access`, `settings`, `drop`, `owner-token`, `token new|ls|set|rm`, `grant ls|add|rm`, `whoami --token`.
- Token on every request once there is one.
- Exit codes as in spec §17.

## Tests that must exist

- **No existence oracle.** For every route, a missing space and a private space without a read grant give byte-identical responses (status, body, headers other than date) and the same miss accounting. Includes `join` with a bad token on both.
- **Private by default.** A space created with an empty body answers `no_space` to anyone without a grant, on every route.
- **Public shorthand.** Each of `none`, `read`, `respond`, `all` round-trips through create, `PATCH` and responses. `public` with `read` is rejected. A hand-set combination reads back as `custom`.
- **Defaults.** A token minted with an empty body is `member`, 10 uses, 7 days. The 11th redeemer and a redeem after 7 days both get `invite_invalid`.
- **`db.open()`** returns `inside: false` for a missing space and for a private one without a grant, `inviteFailed: true` for a bad invite, and redeems a URL invite exactly once.
- **Idempotent redeem.** The same visitor redeeming twice, including after the token ran out, gets 200 and uses no use. Concurrent double redeem makes one grant.
- **Grant lifetimes.** Grants outlive their token's `until`. `grantFor` ends each grant relative to its own redemption. An expired grant fails on the next request without cleanup having run.
- **Stacking and revoking.** Two grants from two tokens both work. Revoking one token removes only its grants. `DELETE grants?user=` removes all, ownership included.
- **Scopes.** Pattern coverage rules from spec §5, including "scopes don't add up across keys" and batch series writes.
- **Settings.** `read: granted` with any `anyone` write is rejected. Changing settings takes effect on the next request for cached spaces.
- **Owner token.** Rotation invalidates the old token and removes its owner grants except the caller's. `admin owner-token` works with no owner left.
- **Fixed rules.** Nobody writes another visitor's `users/{id}/`, owners may delete there, nobody reads another's `me/`, whatever their grants.
- **Visitor creation** through `/spaces` and `join` returns a token and counts against the per-IP limit.

## Release

1. Build, test, update README to describe the new model and remove its "Differences" entry for this.
2. Tag v0.10.0.
3. On 084446.xyz: stop cubby, move `/var/lib/cubby` aside, `cubby-upgrade v0.10.0`, check `/health`.
4. Rebuild throw-count-shared on the new client: a private space per round from `createSpace`, `db.open()` at load, and the share link from `db.share()` (or `<cubby-share>`) instead of a bare `#s=` name. Re-publish it.
