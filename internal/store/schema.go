package store

// serverVersion is server.db's user_version and spaceVersion a space
// file's. v0.19 (sign-in with device keys) started both over; older files
// are refused, not migrated. Server version 2 (v0.19) is migrated to 3
// (membership times in visitor_spaces). Space version 5 is migrated to 6
// (points.expires, for a ttl on series points).
const (
	serverVersion = 3
	spaceVersion  = 6
)

const serverSchema = `
CREATE TABLE IF NOT EXISTS visitors (
  id           TEXT PRIMARY KEY,
  created_at   INTEGER NOT NULL,           -- s
  last_seen_at INTEGER NOT NULL,           -- s, the newest of its devices
  stored       INTEGER NOT NULL DEFAULT 0
);
-- A device is a public key. A visitor has one or more.
CREATE TABLE IF NOT EXISTS devices (
  id           TEXT PRIMARY KEY,           -- the key's JWK thumbprint (RFC 7638)
  visitor      TEXT NOT NULL,
  key          TEXT NOT NULL,              -- the canonical public JWK
  label        TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,           -- ms
  last_seen_at INTEGER NOT NULL,           -- ms
  paired_by    TEXT                        -- the device whose pairing code added this one, NULL for the first
);
CREATE INDEX IF NOT EXISTS devices_by_visitor ON devices (visitor);
-- One-use codes that add another device to the same visitor.
CREATE TABLE IF NOT EXISTS pairing_codes (
  id         TEXT PRIMARY KEY,             -- pc_ + 8 base32
  code_hash  TEXT NOT NULL UNIQUE,         -- SHA-256 of the 10-character code
  visitor    TEXT NOT NULL,
  by_device  TEXT NOT NULL,
  created_at INTEGER NOT NULL,             -- ms
  expires    INTEGER NOT NULL,             -- ms
  state      TEXT NOT NULL DEFAULT 'open', -- open, used, cancelled
  ended_at   INTEGER,                      -- ms
  device     TEXT                          -- the device it added
);
CREATE INDEX IF NOT EXISTS pairing_codes_by_visitor ON pairing_codes (visitor);
-- Static credentials for tools that can't sign: one space, a capped level.
CREATE TABLE IF NOT EXISTS api_keys (
  id           TEXT PRIMARY KEY,           -- ak_ + 8 base32
  hash         TEXT NOT NULL UNIQUE,       -- SHA-256 of the cubby_ key
  visitor      TEXT NOT NULL,
  device       TEXT NOT NULL,              -- the device that minted it; signing it out ends the key
  space        TEXT NOT NULL,
  access       TEXT NOT NULL,              -- read, post or all
  label        TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,           -- ms
  until        INTEGER,                    -- ms, NULL: no end
  last_used_at INTEGER                     -- ms
);
CREATE INDEX IF NOT EXISTS api_keys_by_visitor ON api_keys (visitor);
CREATE INDEX IF NOT EXISTS api_keys_by_device ON api_keys (device);
CREATE INDEX IF NOT EXISTS api_keys_by_space ON api_keys (space);
-- Spaces where a visitor has data, was sent something, or holds a grant.
-- since and changed are set when a grant is made (NULL: no grant known);
-- a listing that finds the grant gone sets them back to NULL.
CREATE TABLE IF NOT EXISTS visitor_spaces (
  visitor TEXT NOT NULL,
  space   TEXT NOT NULL,
  since   INTEGER,                         -- ms, the first grant
  changed INTEGER,                         -- ms, the newest grant
  PRIMARY KEY (visitor, space)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS visitor_spaces_by_space ON visitor_spaces (space);
CREATE INDEX IF NOT EXISTS visitor_spaces_by_changed ON visitor_spaces (visitor, changed DESC, space) WHERE changed IS NOT NULL;
CREATE TABLE IF NOT EXISTS graveyard (
  name TEXT PRIMARY KEY,
  seq  INTEGER NOT NULL
);
`

const spaceSchema = `
CREATE TABLE IF NOT EXISTS meta (
  k TEXT PRIMARY KEY,   -- seq, created_at, last_active_at, tombstone_floor, bytes, keys, settings
  v
);
CREATE TABLE IF NOT EXISTS kv (
  area       TEXT NOT NULL,      -- 's', 'b:{author}', 'i:{recipient}:{sender}' (recipient is an id or 'owners')
  key        TEXT NOT NULL,
  value      BLOB,               -- NULL is a tombstone
  is_json    INTEGER NOT NULL,
  version    INTEGER NOT NULL,
  writer     TEXT NOT NULL,
  size       INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  series     INTEGER NOT NULL DEFAULT 0, -- 1 for a series key
  t          INTEGER,                    -- value time in ms; newest point time for series
  expires    INTEGER NOT NULL DEFAULT 0,
  expired    INTEGER NOT NULL DEFAULT 0,
  n          INTEGER NOT NULL DEFAULT 0, -- number of points, series only
  PRIMARY KEY (area, key)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS kv_by_version ON kv (area, version);
CREATE INDEX IF NOT EXISTS kv_by ON kv (key, area) WHERE area >= 'b:' AND area < 'b;';
CREATE INDEX IF NOT EXISTS kv_tombstones ON kv (updated_at) WHERE value IS NULL;
CREATE TABLE IF NOT EXISTS usage (writer TEXT PRIMARY KEY, bytes INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS points (
  area    TEXT NOT NULL,
  key     TEXT NOT NULL,
  t       INTEGER NOT NULL,
  value   BLOB NOT NULL,
  is_json INTEGER NOT NULL,
  expires INTEGER NOT NULL DEFAULT 0, -- ms, 0 for none
  PRIMARY KEY (area, key, t)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS points_by_expiry ON points (expires) WHERE expires > 0;
CREATE TABLE IF NOT EXISTS invites (
  id               TEXT PRIMARY KEY,     -- i_ + 8 base32
  hash             TEXT NOT NULL UNIQUE, -- SHA-256 of the inv_ value
  label            TEXT NOT NULL DEFAULT '',
  access           TEXT NOT NULL,        -- the level it gives: read, post or all
  uses_left        INTEGER,              -- NULL: no limit
  until            INTEGER,              -- ms, NULL: no end
  grant_for        INTEGER,              -- ms, NULL: grants last until revoked
  created_at       INTEGER NOT NULL,     -- ms
  ended_at         INTEGER,              -- ms, when it ran out of uses
  last_redeemed_at INTEGER,
  minted_by        TEXT NOT NULL,        -- the visitor who minted it
  revoked_at       INTEGER               -- ms, ended because its minter may no longer invite
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS grants (
  id           TEXT PRIMARY KEY,         -- g_ + 8 base32
  visitor      TEXT NOT NULL,
  access       TEXT NOT NULL,            -- read, post, all or owner
  source       TEXT NOT NULL,            -- 'create', 'direct' or an invite id
  until        INTEGER,                  -- ms, NULL: until revoked
  created_at   INTEGER NOT NULL,         -- ms
  last_used_at INTEGER
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS grants_by_visitor ON grants (visitor);
CREATE UNIQUE INDEX IF NOT EXISTS grants_once ON grants (visitor, source) WHERE source <> 'direct';
CREATE TABLE IF NOT EXISTS bans (
  visitor    TEXT PRIMARY KEY,
  until      INTEGER,                  -- ms, NULL: until lifted
  by         TEXT NOT NULL,            -- the owner who banned
  created_at INTEGER NOT NULL          -- ms
) WITHOUT ROWID;
`
