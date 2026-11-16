package store

import (
	"database/sql"
	"encoding/json"
	"strings"
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
