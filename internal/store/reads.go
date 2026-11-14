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
	User    string // across users/*/
	From    string // under to/ and inbox/: the sender
	To      string // under to/ and inbox/: the recipient, a visitor ID or "owners"
	Writer  string // visitor who last wrote or deleted the key
	Area    string
	Series  bool
	T       int64 // current value time in ms (newest point time for series)
	Expires int64
	Expired bool
}

func (s *Store) Get(space, area, key string) (*Entry, error) {
	sp, err := s.Acquire(space)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, apierr.NoSpace
	}
	defer s.Release(sp)
	if err := s.expire(sp); err != nil {
		return nil, err
	}
	e := &Entry{Key: key, Area: area}
	e.fill(OneArea)
	var t sql.NullInt64
	err = sp.db.QueryRow(`SELECT value, is_json, version, writer, series, t, expires, expired FROM kv WHERE area = ? AND key = ? AND value IS NOT NULL`,
		area, key).Scan(&e.Value, &e.IsJSON, &e.Version, &e.Writer, &e.Series, &t, &e.Expires, &e.Expired)
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

// Across says which areas a target covers besides a single one.
type Across int

const (
	OneArea  Across = iota
	AllUsers        // users/*/
	Inbox           // inbox/*/: every sender to Fixed, the recipient
	Sent            // to/*/: every recipient of Fixed, the sender
)

// ToArea is the stored area of the slot a sender fills for a recipient: one
// per recipient, sender and key. rcpt is a visitor ID or "owners".
func ToArea(rcpt, sender string) string { return "i:" + rcpt + ":" + sender }

// SplitTo returns the recipient and sender of a to/ area.
func SplitTo(area string) (rcpt, sender string, ok bool) {
	rest, ok := strings.CutPrefix(area, "i:")
	if !ok {
		return "", "", false
	}
	i := strings.LastIndexByte(rest, ':')
	if i < 0 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// Target selects keys in one area, or in several (Across).
type Target struct {
	Space   string
	Area    string // when Across is OneArea
	Across  Across
	Fixed   string // the recipient for Inbox, the sender for Sent
	Key     string // exact key, or prefix when pattern
	Pattern bool
	// Self: what the target means depends on who asks (inbox/, to/,
	// users/me/). A live watch on it ends when the connection changes
	// identity.
	Self bool
}

// Many reports whether the target spans several areas, so its items name
// the user, sender or recipient they belong to.
func (t Target) Many() bool { return t.Across != OneArea }

// Match reports whether a stored area is in the target.
func (t Target) Match(area string) bool {
	switch t.Across {
	case AllUsers:
		return strings.HasPrefix(area, "u:")
	case Inbox:
		return strings.HasPrefix(area, "i:"+t.Fixed+":")
	case Sent:
		return strings.HasPrefix(area, "i:") && strings.HasSuffix(area, ":"+t.Fixed)
	}
	return area == t.Area
}

// AreaFor is the area of one party in a target spanning several: the user,
// the sender (Inbox) or the recipient (Sent). Used for list cursors.
func (t Target) AreaFor(party string) string {
	switch t.Across {
	case AllUsers:
		return "u:" + party
	case Inbox:
		return ToArea(t.Fixed, party)
	case Sent:
		return ToArea(party, t.Fixed)
	}
	return t.Area
}

// Party is the inverse of AreaFor.
func (t Target) Party(area string) string {
	switch t.Across {
	case AllUsers:
		return strings.TrimPrefix(area, "u:")
	case Inbox:
		_, s, _ := SplitTo(area)
		return s
	case Sent:
		r, _, _ := SplitTo(area)
		return r
	}
	return ""
}

func (t Target) where() (string, []any) {
	var b strings.Builder
	var args []any
	switch t.Across {
	case AllUsers:
		b.WriteString("area >= 'u:' AND area < 'u;'")
	case Inbox:
		b.WriteString("area >= ? AND area < ?")
		args = append(args, "i:"+t.Fixed+":", "i:"+t.Fixed+";")
	case Sent:
		b.WriteString("area >= 'i:' AND area < 'i;' AND substr(area, ?) = ?")
		args = append(args, -(len(t.Fixed) + 1), ":"+t.Fixed)
	default:
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

func scanEntries(rows *sql.Rows, across Across) ([]Entry, error) {
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var t sql.NullInt64
		if err := rows.Scan(&e.Area, &e.Key, &e.Value, &e.IsJSON, &e.Version, &e.Writer, &e.Series, &t, &e.Expires, &e.Expired); err != nil {
			return nil, err
		}
		e.T = t.Int64
		e.fill(across)
		out = append(out, e)
	}
	return out, rows.Err()
}

// fill sets the parties an entry carries: user across users/*/, from and
// to for every entry under to/ and inbox/.
func (e *Entry) fill(across Across) {
	if across == AllUsers {
		e.User = strings.TrimPrefix(e.Area, "u:")
	}
	e.To, e.From, _ = SplitTo(e.Area)
}

// CurrentSeq is the space's counter.
func (s *Store) CurrentSeq(space string) (int64, error) {
	sp, err := s.Acquire(space)
	if err != nil {
		return 0, err
	}
	if sp == nil {
		return 0, apierr.NoSpace
	}
	defer s.Release(sp)
	if err := s.expire(sp); err != nil {
		return 0, err
	}
	m, err := readMeta(sp.db)
	return m.seq, err
}

// list returns live entries in key order, plus a cursor if there are more.
func (s *Store) List(t Target, after string, limit int, desc bool) (items []Entry, next string, version int64, err error) {
	sp, err := s.Acquire(t.Space)
	if err != nil {
		return nil, "", 0, err
	}
	if sp == nil {
		return nil, "", 0, apierr.NoSpace
	}
	defer s.Release(sp)
	if err := s.expire(sp); err != nil {
		return nil, "", 0, err
	}
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
		if ak, au, ok := strings.Cut(after, "/"); ok && t.Many() {
			where += fmt.Sprintf(" AND (key %s ? OR (key = ? AND area %s ?))", cmp, cmp)
			args = append(args, ak, ak, t.AreaFor(au))
		} else {
			where += fmt.Sprintf(" AND key %s ?", cmp)
			args = append(args, after)
		}
	}
	q := fmt.Sprintf(`SELECT area, key, value, is_json, version, writer, series, t, expires, expired FROM kv WHERE %s ORDER BY key %s, area %s LIMIT ?`, where, ord, ord)
	rows, err := sp.db.Query(q, append(args, limit+1)...)
	if err != nil {
		return nil, "", 0, err
	}
	items, err = scanEntries(rows, t.Across)
	if err != nil {
		return nil, "", 0, err
	}
	if len(items) > limit {
		items = items[:limit]
		last := items[limit-1]
		next = last.Key
		if t.Many() {
			next += "/" + t.Party(last.Area)
		}
	}
	return items, next, m.seq, nil
}

var ErrVersionExpired = errors.New("version expired")

// changes returns entries changed after version, ordered by version, at most
// max+1 so the caller can tell whether there are more. It also returns the
// space's current counter.
func (s *Store) Changes(t Target, after int64, limit int) ([]Entry, int64, error) {
	sp, err := s.Acquire(t.Space)
	if err != nil {
		return nil, 0, err
	}
	if sp == nil {
		return nil, 0, apierr.NoSpace
	}
	defer s.Release(sp)
	if err := s.expire(sp); err != nil {
		return nil, 0, err
	}
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
	rows, err := sp.db.Query(`SELECT area, key, value, is_json, version, writer, series, t, expires, expired FROM kv WHERE `+where+` ORDER BY version LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanEntries(rows, t.Across)
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
	sp, err := s.Acquire(space)
	if err != nil {
		return err
	}
	if sp == nil {
		return apierr.NoSpace
	}
	defer s.Release(sp)
	if err := s.expire(sp); err != nil {
		return err
	}
	rows, err := sp.db.Query(`SELECT area, key, value, is_json, version, writer, series, t, expires, expired FROM kv WHERE value IS NOT NULL ORDER BY area, key`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var e Entry
		var t sql.NullInt64
		if err := rows.Scan(&e.Area, &e.Key, &e.Value, &e.IsJSON, &e.Version, &e.Writer, &e.Series, &t, &e.Expires, &e.Expired); err != nil {
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
