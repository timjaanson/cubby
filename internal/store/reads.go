package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/names"
)

type Entry struct {
	Key     string
	Value   []byte // nil for a tombstone
	IsJSON  bool
	Version int64
	User    string
	Writer  string
	Area    string
	Series  bool
	T       int64 // newest point's time, series only
}

func (s *Store) Get(space, area, key string) (*Entry, error) {
	sp, _, err := s.Acquire(space, false, "")
	if err != nil || sp == nil {
		return nil, err
	}
	defer s.Release(sp)
	e := &Entry{Key: key}
	var t sql.NullInt64
	err = sp.db.QueryRow(`SELECT value, is_json, version, series, t FROM kv WHERE area = ? AND key = ? AND value IS NOT NULL`,
		area, key).Scan(&e.Value, &e.IsJSON, &e.Version, &e.Series, &t)
	e.T = t.Int64
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.touchSpace(sp)
	return e, nil
}

// target selects keys in an area. allUsers means users/*/.
type Target struct {
	Space    string
	Area     string // ignored when allUsers
	AllUsers bool
	Key      string // exact key, or prefix when pattern
	Pattern  bool
}

func (t Target) where() (string, []any) {
	var b strings.Builder
	var args []any
	if t.AllUsers {
		b.WriteString("area >= 'u:' AND area < 'u;'")
	} else {
		b.WriteString("area = ?")
		args = append(args, t.Area)
	}
	if t.Pattern {
		if t.Key != "" {
			lo, hi := names.PrefixRange(t.Key)
			b.WriteString(" AND key >= ? AND key < ?")
			args = append(args, lo, hi)
		}
	} else {
		b.WriteString(" AND key = ?")
		args = append(args, t.Key)
	}
	return b.String(), args
}

func scanEntries(rows *sql.Rows, allUsers bool) ([]Entry, error) {
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var t sql.NullInt64
		if err := rows.Scan(&e.Area, &e.Key, &e.Value, &e.IsJSON, &e.Version, &e.Series, &t); err != nil {
			return nil, err
		}
		e.T = t.Int64
		if allUsers {
			e.User = strings.TrimPrefix(e.Area, "u:")
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// currentSeq is the space's counter, or the phantom counter if it doesn't exist.
func (s *Store) CurrentSeq(space string) (int64, error) {
	sp, _, err := s.Acquire(space, false, "")
	if err != nil {
		return 0, err
	}
	if sp == nil {
		return s.phantomSeq(space), nil
	}
	defer s.Release(sp)
	m, err := readMeta(sp.db)
	return m.seq, err
}

// list returns live entries in key order, plus a cursor if there are more.
func (s *Store) List(t Target, after string, limit int, desc bool) (items []Entry, next string, version int64, err error) {
	sp, _, err := s.Acquire(t.Space, false, "")
	if err != nil {
		return nil, "", 0, err
	}
	if sp == nil {
		return nil, "", s.phantomSeq(t.Space), nil
	}
	defer s.Release(sp)
	s.touchSpace(sp)
	m, err := readMeta(sp.db)
	if err != nil {
		return nil, "", 0, err
	}
	where, args := t.where()
	where += " AND value IS NOT NULL"
	cmp, ord := ">", "ASC"
	if desc {
		cmp, ord = "<", "DESC"
	}
	if after != "" {
		if ak, au, ok := strings.Cut(after, "/"); ok && t.AllUsers {
			where += fmt.Sprintf(" AND (key %s ? OR (key = ? AND area %s ?))", cmp, cmp)
			args = append(args, ak, ak, "u:"+au)
		} else {
			where += fmt.Sprintf(" AND key %s ?", cmp)
			args = append(args, after)
		}
	}
	q := fmt.Sprintf(`SELECT area, key, value, is_json, version, series, t FROM kv WHERE %s ORDER BY key %s, area %s LIMIT ?`, where, ord, ord)
	rows, err := sp.db.Query(q, append(args, limit+1)...)
	if err != nil {
		return nil, "", 0, err
	}
	items, err = scanEntries(rows, t.AllUsers)
	if err != nil {
		return nil, "", 0, err
	}
	if len(items) > limit {
		items = items[:limit]
		last := items[limit-1]
		next = last.Key
		if t.AllUsers {
			next += "/" + last.User
		}
	}
	return items, next, m.seq, nil
}

var ErrVersionExpired = errors.New("version expired")

// changes returns entries changed after version, ordered by version, at most
// max+1 so the caller can tell whether there are more. It also returns the
// space's current counter.
func (s *Store) Changes(t Target, after int64, limit int) ([]Entry, int64, error) {
	sp, _, err := s.Acquire(t.Space, false, "")
	if err != nil {
		return nil, 0, err
	}
	if sp == nil {
		g := s.graveSeq(t.Space)
		seq := max(NowMs(), g+1)
		// A version from before the space was deleted, or from the future.
		if after > 0 && ((g > 0 && after <= g) || after > seq) {
			return nil, seq, ErrVersionExpired
		}
		return nil, seq, nil
	}
	defer s.Release(sp)
	m, err := readMeta(sp.db)
	if err != nil {
		return nil, 0, err
	}
	if after > 0 && (after < m.floor || after > m.seq) {
		return nil, m.seq, ErrVersionExpired
	}
	where, args := t.where()
	where += " AND version > ?"
	args = append(args, after)
	if after == 0 {
		where += " AND value IS NOT NULL"
	}
	rows, err := sp.db.Query(`SELECT area, key, value, is_json, version, series, t FROM kv WHERE `+where+` ORDER BY version LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanEntries(rows, t.AllUsers)
	return out, m.seq, err
}

// Point is one entry of a series.
type Point struct {
	Area, Key string
	T         int64
	Value     []byte
	IsJSON    bool
}

// export returns every live entry with its area and writer, then every
// series point.
func (s *Store) Export(space string, fn func(Entry) error, pfn func(Point) error) error {
	sp, _, err := s.Acquire(space, false, "")
	if err != nil {
		return err
	}
	if sp == nil {
		return apierr.NoSpace
	}
	defer s.Release(sp)
	rows, err := sp.db.Query(`SELECT area, key, value, is_json, version, writer, series, t FROM kv WHERE value IS NOT NULL ORDER BY area, key`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var e Entry
		var t sql.NullInt64
		if err := rows.Scan(&e.Area, &e.Key, &e.Value, &e.IsJSON, &e.Version, &e.Writer, &e.Series, &t); err != nil {
			return err
		}
		e.T = t.Int64
		if err := fn(e); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	prows, err := sp.db.Query(`SELECT area, key, t, value, is_json FROM points ORDER BY area, key, t`)
	if err != nil {
		return err
	}
	defer prows.Close()
	for prows.Next() {
		var p Point
		if err := prows.Scan(&p.Area, &p.Key, &p.T, &p.Value, &p.IsJSON); err != nil {
			return err
		}
		if err := pfn(p); err != nil {
			return err
		}
	}
	return prows.Err()
}
