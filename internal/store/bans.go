package store

import (
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/timjaanson/cubby/internal/apierr"
)

// migrateMeToTo moves me/ into to/me/: a visitor's private slot is the one
// they fill for themselves. Rows keep their values and versions. Scopes
// naming me move to to/me.
func migrateMeToTo(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"kv", "points"} {
		if _, err := tx.Exec(`UPDATE ` + table + ` SET area = 'i:' || substr(area, 3) || ':' || substr(area, 3) WHERE area >= 'm:' AND area < 'm;'`); err != nil {
			return err
		}
	}
	for _, table := range []string{"grants", "tokens"} {
		rows, err := tx.Query(`SELECT id, allow FROM ` + table + ` WHERE allow LIKE '%"me%'`)
		if err != nil {
			return err
		}
		type row struct{ id, allow string }
		var rs []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.allow); err != nil {
				rows.Close()
				return err
			}
			rs = append(rs, r)
		}
		rows.Close()
		for _, r := range rs {
			var list []string
			if json.Unmarshal([]byte(r.allow), &list) != nil {
				continue
			}
			for i, sc := range list {
				if rest, ok := strings.CutPrefix(sc, "me"); ok && (strings.HasPrefix(rest, ":") || strings.HasPrefix(rest, "/")) {
					list[i] = "to/me" + rest
				}
			}
			b, _ := json.Marshal(list)
			if _, err := tx.Exec(`UPDATE `+table+` SET allow = ? WHERE id = ?`, string(b), r.id); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`PRAGMA user_version = 3`); err != nil {
		return err
	}
	return tx.Commit()
}

// BanInfo is one ban, for the owner's list.
type BanInfo struct {
	User      string `json:"user"`
	Until     *int64 `json:"until"`
	By        string `json:"by"`
	CreatedAt int64  `json:"t"`
}

var errBanOwner = &apierr.Error{Status: 409, Code: "is_owner",
	Message: "Owners can't be banned. Remove their owner grant first."}

// Ban bans a visitor from a space, or replaces their ban. It deletes and
// revokes nothing. created is false when a ban was replaced.
func (s *Store) Ban(space, user, by string, until *int64) (b BanInfo, created bool, err error) {
	now := NowMs()
	err = s.withSpace(space, func(tx *sql.Tx) error {
		r, err := rightsTx(tx, user, now)
		if err != nil {
			return err
		}
		if r.Owner {
			return errBanOwner
		}
		var n int
		tx.QueryRow(`SELECT count(*) FROM bans WHERE visitor = ? AND (until IS NULL OR until > ?)`, user, now).Scan(&n)
		created = n == 0
		var u any
		if until != nil {
			u = *until
		}
		if _, err := tx.Exec(`INSERT INTO bans (visitor, until, by, created_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (visitor) DO UPDATE SET until = excluded.until, by = excluded.by, created_at = excluded.created_at`,
			user, u, by, now); err != nil {
			return err
		}
		b = BanInfo{User: user, Until: until, By: by, CreatedAt: now}
		return nil
	})
	return b, created, err
}

// Unban lifts a ban. Lifting one that isn't there is not an error.
func (s *Store) Unban(space, user string) (bool, error) {
	var lifted bool
	err := s.withSpace(space, func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM bans WHERE visitor = ? AND (until IS NULL OR until > ?)`, user, NowMs())
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		lifted = n > 0
		_, err = tx.Exec(`DELETE FROM bans WHERE visitor = ?`, user)
		return err
	})
	return lifted, err
}

// Bans lists the bans in force, newest first.
func (s *Store) Bans(space string) ([]BanInfo, error) {
	sp, err := s.Acquire(space)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, apierr.NoSpace
	}
	defer s.Release(sp)
	rows, err := sp.db.Query(`SELECT visitor, until, by, created_at FROM bans WHERE until IS NULL OR until > ? ORDER BY created_at DESC, visitor`, NowMs())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BanInfo{}
	for rows.Next() {
		var b BanInfo
		var until sql.NullInt64
		if err := rows.Scan(&b.User, &until, &b.By, &b.CreatedAt); err != nil {
			return nil, err
		}
		b.Until = nullable(until)
		out = append(out, b)
	}
	return out, rows.Err()
}
