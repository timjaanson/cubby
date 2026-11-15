package store

// serverVersion is server.db's user_version. 1: tokens moved from visitors
// to devices.
const serverVersion = 1

const serverSchema = `
CREATE TABLE IF NOT EXISTS visitors (
  id           TEXT PRIMARY KEY,
  created_at   INTEGER NOT NULL,           -- s
  last_seen_at INTEGER NOT NULL,           -- s, the newest of its devices
  stored       INTEGER NOT NULL DEFAULT 0
);
-- Every visitor token is a device. A visitor has one or more.
CREATE TABLE IF NOT EXISTS devices (
  id           TEXT PRIMARY KEY,           -- d_ + 8 base32
  visitor      TEXT NOT NULL,
  token_hash   TEXT NOT NULL UNIQUE,       -- SHA-256 of the vt_ token
  label        TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,           -- ms
  last_seen_at INTEGER NOT NULL,           -- ms
  linked_by    TEXT                        -- the device whose link made this one, NULL for the first
);
CREATE INDEX IF NOT EXISTS devices_by_visitor ON devices (visitor);
-- One-use codes that make another device the same visitor.
CREATE TABLE IF NOT EXISTS links (
  id         TEXT PRIMARY KEY,             -- l_ + 8 base32
  code_hash  TEXT NOT NULL UNIQUE,         -- SHA-256 of lk_ + the 10-character code
  visitor    TEXT NOT NULL,
  by_device  TEXT NOT NULL,
  created_at INTEGER NOT NULL,             -- ms
  expires    INTEGER NOT NULL,             -- ms
  state      TEXT NOT NULL DEFAULT 'open', -- open, used, cancelled
  ended_at   INTEGER,                      -- ms
  device     TEXT                          -- the device it made
);
CREATE INDEX IF NOT EXISTS links_by_visitor ON links (visitor);
CREATE TABLE IF NOT EXISTS visitor_spaces (
  visitor TEXT NOT NULL,
  space   TEXT NOT NULL,
  PRIMARY KEY (visitor, space)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS visitor_spaces_by_space ON visitor_spaces (space);
CREATE TABLE IF NOT EXISTS graveyard (
  name TEXT PRIMARY KEY,
  seq  INTEGER NOT NULL
);
`

const spaceSchema = `
CREATE TABLE IF NOT EXISTS meta (
  k TEXT PRIMARY KEY,   -- seq, created_at, last_active_at, tombstone_floor, bytes, keys, settings, owner_hash
  v
);
CREATE TABLE IF NOT EXISTS kv (
  area       TEXT NOT NULL,      -- 's', 'u:{id}', 'i:{recipient}:{sender}' (recipient is an id or 'owners')
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
CREATE INDEX IF NOT EXISTS kv_users ON kv (key, area) WHERE area >= 'u:' AND area < 'u;';
CREATE INDEX IF NOT EXISTS kv_tombstones ON kv (updated_at) WHERE value IS NULL;
CREATE TABLE IF NOT EXISTS usage (writer TEXT PRIMARY KEY, bytes INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS points (
  area    TEXT NOT NULL,
  key     TEXT NOT NULL,
  t       INTEGER NOT NULL,
  value   BLOB NOT NULL,
  is_json INTEGER NOT NULL,
  PRIMARY KEY (area, key, t)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS tokens (
  id               TEXT PRIMARY KEY,     -- t_ + 8 base32
  hash             TEXT NOT NULL UNIQUE, -- SHA-256 of the at_ token
  label            TEXT NOT NULL DEFAULT '',
  allow            TEXT NOT NULL,        -- JSON array of scopes
  uses_left        INTEGER,              -- NULL: no limit
  until            INTEGER,              -- ms, NULL: no end
  grant_for        INTEGER,              -- ms, NULL: grants last until revoked
  created_at       INTEGER NOT NULL,     -- ms
  ended_at         INTEGER,              -- ms, when it ran out of uses
  last_redeemed_at INTEGER,
  minted_by        TEXT,                 -- visitor who minted it, NULL before this was recorded
  revoked_at       INTEGER               -- ms, ended because its minter may no longer invite
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS grants (
  id           TEXT PRIMARY KEY,         -- g_ + 8 base32
  visitor      TEXT NOT NULL,
  owner        INTEGER NOT NULL DEFAULT 0,
  allow        TEXT NOT NULL,            -- JSON array of scopes, [] for owner grants
  source       TEXT NOT NULL,            -- 'create', 'owner-token', 'direct' or a token id
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
