package store

import (
	"database/sql"
	"strings"
)

const serverSchema = `
CREATE TABLE IF NOT EXISTS visitors (
  id           TEXT PRIMARY KEY,
  token_hash   TEXT NOT NULL UNIQUE,
  created_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  stored       INTEGER NOT NULL DEFAULT 0
);
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
  k TEXT PRIMARY KEY,   -- seq, created_at, last_active_at, tombstone_floor, bytes, keys, key_hash
  v
);
CREATE TABLE IF NOT EXISTS kv (
  area       TEXT NOT NULL,      -- 's', 'u:{id}', 'm:{id}'
  key        TEXT NOT NULL,
  value      BLOB,               -- NULL is a tombstone
  is_json    INTEGER NOT NULL,
  version    INTEGER NOT NULL,
  writer     TEXT NOT NULL,
  size       INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  series     INTEGER NOT NULL DEFAULT 0, -- 1 for a series key
  t          INTEGER,                    -- newest point's time in ms, series only
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
`

// migrateSpace brings a space file from before series up to date. Files
// created since have everything already, and running it twice is harmless.
func migrateSpace(db *sql.DB) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('kv')`)
	if err != nil {
		return err
	}
	has := map[string]bool{}
	for rows.Next() {
		var n string
		rows.Scan(&n)
		has[n] = true
	}
	rows.Close()
	for _, col := range []string{"series INTEGER NOT NULL DEFAULT 0", "t INTEGER", "n INTEGER NOT NULL DEFAULT 0"} {
		name, _, _ := strings.Cut(col, " ")
		if !has[name] {
			if _, err := db.Exec(`ALTER TABLE kv ADD COLUMN ` + col); err != nil {
				return err
			}
		}
	}
	_, err = db.Exec(spaceSchema)
	return err
}
