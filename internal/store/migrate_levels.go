package store

import (
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/timjaanson/cubby/internal/access"
)

// spaceVersion is a space file's user_version. New files start at it.
const spaceVersion = 4

// migrateLevels moves a space from scopes and per-area settings to levels,
// and users/ to by/:
//   - grants and tokens get the level their scopes came closest to: owner
//     grants owner, any write to shared/ (or every area) all, any other
//     write post, reading only read
//   - settings become the anyone level of the old public shorthand, and
//     invite "members" becomes post, "owners" owner
//   - a banned visitor loses their grants, as a ban does now; bans on
//     owners, which never applied, go
//   - by/ keys keep their values and versions
func migrateLevels(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"kv", "points"} {
		if _, err := tx.Exec(`UPDATE ` + table + ` SET area = 'b:' || substr(area, 3) WHERE area >= 'u:' AND area < 'u;'`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DROP INDEX IF EXISTS kv_users`); err != nil {
		return err
	}

	type row struct {
		id, allow string
		owner     bool
	}
	read := func(q string) ([]row, error) {
		rows, err := tx.Query(q)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.allow, &r.owner); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		return out, rows.Err()
	}
	grants, err := read(`SELECT id, allow, owner FROM grants`)
	if err != nil {
		return err
	}
	tokens, err := read(`SELECT id, allow, 0 FROM tokens`)
	if err != nil {
		return err
	}
	for _, stmt := range []string{
		`CREATE TABLE grants_new (
		  id           TEXT PRIMARY KEY,
		  visitor      TEXT NOT NULL,
		  access       TEXT NOT NULL,
		  source       TEXT NOT NULL,
		  until        INTEGER,
		  created_at   INTEGER NOT NULL,
		  last_used_at INTEGER
		) WITHOUT ROWID`,
		`INSERT INTO grants_new SELECT id, visitor, 'none', source, until, created_at, last_used_at FROM grants`,
		`DROP TABLE grants`,
		`ALTER TABLE grants_new RENAME TO grants`,
		`CREATE INDEX grants_by_visitor ON grants (visitor)`,
		`CREATE UNIQUE INDEX grants_once ON grants (visitor, source) WHERE source <> 'direct'`,
		`CREATE TABLE tokens_new (
		  id               TEXT PRIMARY KEY,
		  hash             TEXT NOT NULL UNIQUE,
		  label            TEXT NOT NULL DEFAULT '',
		  access           TEXT NOT NULL,
		  uses_left        INTEGER,
		  until            INTEGER,
		  grant_for        INTEGER,
		  created_at       INTEGER NOT NULL,
		  ended_at         INTEGER,
		  last_redeemed_at INTEGER,
		  minted_by        TEXT,
		  revoked_at       INTEGER
		) WITHOUT ROWID`,
		`INSERT INTO tokens_new SELECT id, hash, label, 'none', uses_left, until, grant_for, created_at, ended_at, last_redeemed_at, minted_by, revoked_at FROM tokens`,
		`DROP TABLE tokens`,
		`ALTER TABLE tokens_new RENAME TO tokens`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	for _, g := range grants {
		if _, err := tx.Exec(`UPDATE grants SET access = ? WHERE id = ?`, scopesLevel(g.allow, g.owner).String(), g.id); err != nil {
			return err
		}
	}
	for _, t := range tokens {
		l := min(scopesLevel(t.allow, false), access.All)
		if _, err := tx.Exec(`UPDATE tokens SET access = ? WHERE id = ?`, l.String(), t.id); err != nil {
			return err
		}
	}

	now := NowMs()
	if _, err := tx.Exec(`DELETE FROM bans WHERE visitor IN (SELECT visitor FROM grants WHERE access = 'owner')`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM grants WHERE visitor IN (SELECT visitor FROM bans WHERE until IS NULL OR until > ?)`, now); err != nil {
		return err
	}

	var old sql.NullString
	tx.QueryRow(`SELECT v FROM meta WHERE k = 'settings'`).Scan(&old)
	if err := setMeta(tx, "settings", legacySettings(old.String).Encode()); err != nil {
		return err
	}
	if _, err := tx.Exec(`PRAGMA user_version = 4`); err != nil {
		return err
	}
	return tx.Commit()
}

// scopesLevel is the level closest to an old allow list.
func scopesLevel(allow string, owner bool) access.Level {
	if owner {
		return access.Owner
	}
	var list []string
	json.Unmarshal([]byte(allow), &list)
	l := access.Read
	for _, sc := range list {
		body, rights, _ := strings.Cut(sc, ":")
		if !strings.Contains(rights, "w") {
			continue
		}
		area, _, _ := strings.Cut(body, "/")
		if area == "*" || area == "shared" {
			return access.All
		}
		l = access.Post
	}
	return l
}

// legacySettings reads the settings stored before version 4: read, write
// shared, write users, write to ('a' anyone, 'g' granted), then invite
// ('m' members, 'o' owners).
func legacySettings(v string) access.Settings {
	s := access.Default
	if len(v) != 4 && len(v) != 5 {
		return s
	}
	switch {
	case v[1] == 'a':
		s.Anyone = access.All
	case v[2] == 'a' || v[3] == 'a':
		s.Anyone = access.Post
	case v[0] == 'a':
		s.Anyone = access.Read
	}
	if len(v) == 5 && v[4] == 'o' {
		s.Invite = access.Owner
	}
	return s
}
