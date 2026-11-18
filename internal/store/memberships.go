package store

import (
	"database/sql"
	"sort"

	"github.com/timjaanson/cubby/internal/access"
)

// noteMember records that a visitor got a grant in a space at t. fresh:
// they held none just before, so membership starts again at t.
func (s *Store) noteMember(visitor, space string, t int64, fresh bool) {
	s.noteVisitorSpace(visitor, space)
	s.srv.Exec(`INSERT INTO visitor_spaces (visitor, space, since, changed) VALUES (?, ?, ?, ?)
		ON CONFLICT (visitor, space) DO UPDATE SET since = CASE WHEN ? OR since IS NULL THEN excluded.since ELSE since END, changed = excluded.changed`,
		visitor, space, t, t, fresh)
}

// hasGrant reports whether the visitor holds a current grant, inside a
// space's transaction.
func hasGrant(tx *sql.Tx, visitor string, now int64) bool {
	var one int
	return tx.QueryRow(`SELECT 1 FROM grants WHERE visitor = ? AND (until IS NULL OR until > ?) LIMIT 1`, visitor, now).Scan(&one) == nil
}

// indexMembers adds a space file's members to visitor_spaces where they're
// missing: when server.db is migrated from v0.19, and for a space file
// copied in from elsewhere. Rescan calls it for every file it finds.
func (s *Store) indexMembers(space string, db *sql.DB) error {
	rows, err := db.Query(`SELECT visitor, min(created_at), max(created_at) FROM grants
		WHERE until IS NULL OR until > ? GROUP BY visitor`, NowMs())
	if err != nil {
		return err
	}
	type g struct {
		visitor        string
		since, changed int64
	}
	var gs []g
	for rows.Next() {
		var x g
		if err := rows.Scan(&x.visitor, &x.since, &x.changed); err != nil {
			rows.Close()
			return err
		}
		gs = append(gs, x)
	}
	rows.Close()
	for _, x := range gs {
		if _, err := s.srv.Exec(`INSERT INTO visitor_spaces (visitor, space, since, changed) VALUES (?, ?, ?, ?)
			ON CONFLICT (visitor, space) DO UPDATE SET since = coalesce(since, excluded.since), changed = coalesce(changed, excluded.changed)`,
			x.visitor, space, x.since, x.changed); err != nil {
			return err
		}
	}
	return nil
}

// Membership is one space in a visitor's list.
type Membership struct {
	Space      string          `json:"space"`
	Access     access.Level    `json:"access"`
	Via        string          `json:"via"` // created, invite or added
	Since      int64           `json:"since"`
	Changed    int64           `json:"-"`
	Until      *int64          `json:"expires"`
	Members    int             `json:"members"`
	LastActive int64           `json:"lastActive"`
	Bytes      int64           `json:"bytes"`
	Keys       int             `json:"keys"`
	Settings   access.Settings `json:"settings"`
}

// SpacesCursor is where a page of ListSpaces starts: after the space with
// this changed time and name.
type SpacesCursor struct {
	Active, Since int64 // of the last item, ms
	Space         string
}

// before reports whether m sorts before the cursor's item: more recently
// active, then the newer membership, then by name.
func (c SpacesCursor) before(m Membership) bool {
	if m.LastActive != c.Active {
		return m.LastActive > c.Active
	}
	if m.Since != c.Since {
		return m.Since > c.Since
	}
	return m.Space <= c.Space
}

// ListSpaces lists the spaces where the visitor holds a grant, the most
// recently active first (then the newer membership, then by name). Activity is kept in each space's
// file, so it reads every membership and sorts them here; a visitor holds
// few. Rows whose grant is gone are cleared on the way.
func (s *Store) ListSpaces(visitor string, after *SpacesCursor, limit int) (items []Membership, next *SpacesCursor, err error) {
	rows, err := s.srv.Query(`SELECT space, since, changed FROM visitor_spaces WHERE visitor = ? AND changed IS NOT NULL`, visitor)
	if err != nil {
		return nil, nil, err
	}
	type row struct {
		space          string
		since, changed int64
	}
	var rs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.space, &r.since, &r.changed); err != nil {
			rows.Close()
			return nil, nil, err
		}
		rs = append(rs, r)
	}
	rows.Close()
	all := []Membership{}
	for _, r := range rs {
		m, err := s.membership(r.space, visitor)
		if err != nil {
			return nil, nil, err
		}
		if m == nil {
			s.srv.Exec(`UPDATE visitor_spaces SET since = NULL, changed = NULL WHERE visitor = ? AND space = ?`, visitor, r.space)
			continue
		}
		m.Since, m.Changed = r.since, r.changed
		all = append(all, *m)
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		return SpacesCursor{Active: b.LastActive, Since: b.Since, Space: b.Space}.before(a) && a.Space != b.Space
	})
	items = []Membership{}
	for _, m := range all {
		if after != nil && after.before(m) {
			continue
		}
		if len(items) == limit {
			last := items[limit-1]
			next = &SpacesCursor{Active: last.LastActive, Since: last.Since, Space: last.Space}
			break
		}
		items = append(items, m)
	}
	return items, next, nil
}

// membership reads the visitor's grant in a space and what the list shows
// about the space. nil: no current grant, or no such space.
func (s *Store) membership(space, visitor string) (*Membership, error) {
	sp, err := s.Acquire(space)
	if err != nil || sp == nil {
		return nil, err
	}
	defer s.Release(sp)
	now := NowMs()
	rows, err := sp.db.Query(`SELECT access, source, until FROM grants WHERE visitor = ? AND (until IS NULL OR until > ?)`, visitor, now)
	if err != nil {
		return nil, err
	}
	var m *Membership
	for rows.Next() {
		var lv, source string
		var until sql.NullInt64
		if err := rows.Scan(&lv, &source, &until); err != nil {
			rows.Close()
			return nil, err
		}
		l, ok := access.ParseLevel(lv)
		if !ok {
			continue
		}
		u := nullable(until)
		// The grant that gives the most; of equal ones, the one lasting longest.
		if m != nil && (l < m.Access || l == m.Access && (m.Until == nil || u != nil && *u <= *m.Until)) {
			continue
		}
		via := "invite"
		switch source {
		case "create":
			via = "created"
		case "direct":
			via = "added"
		}
		m = &Membership{Space: space, Access: l, Via: via, Until: u}
	}
	rows.Close()
	if m == nil {
		return nil, nil
	}
	if err := sp.db.QueryRow(`SELECT count(DISTINCT visitor) FROM grants WHERE until IS NULL OR until > ?`, now).Scan(&m.Members); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if st := s.stats[space]; st != nil {
		m.Bytes, m.Keys, m.LastActive, m.Settings = st.bytes, st.keys, st.lastActive*1000, st.settings
	}
	s.mu.Unlock()
	return m, nil
}
