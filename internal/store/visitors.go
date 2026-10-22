package store

import (
	"database/sql"
	"errors"

	"github.com/timjaanson/cubby/internal/names"
)

func (s *Store) NewVisitor() (token, id string, err error) {
	token, id = names.RandomToken("vt_"), names.NewVisitorID()
	now := nowS()
	_, err = s.srv.Exec(`INSERT INTO visitors (id, token_hash, created_at, last_seen_at) VALUES (?, ?, ?, ?)`,
		id, names.HashSecret(token), now, now)
	return
}

// visitorByToken returns "" for an unknown token.
func (s *Store) VisitorByToken(token string) (string, error) {
	var id string
	err := s.srv.QueryRow(`SELECT id FROM visitors WHERE token_hash = ?`, names.HashSecret(token)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	now := nowS()
	s.cacheMu.Lock()
	stale := now-s.vtouched[id] >= 3600
	if stale {
		s.vtouched[id] = now
	}
	s.cacheMu.Unlock()
	if stale {
		s.srv.Exec(`UPDATE visitors SET last_seen_at = ? WHERE id = ?`, now, id)
	}
	return id, nil
}

// visitorTotal is a visitor's storage use across every space.
func (s *Store) VisitorTotal(id string) (int64, error) {
	s.cacheMu.Lock()
	if n, ok := s.vbytes[id]; ok {
		s.cacheMu.Unlock()
		return n, nil
	}
	s.cacheMu.Unlock()
	rows, err := s.srv.Query(`SELECT space FROM visitor_spaces WHERE visitor = ?`, id)
	if err != nil {
		return 0, err
	}
	var spaces []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		spaces = append(spaces, n)
	}
	rows.Close()
	var total int64
	for _, name := range spaces {
		sp, err := s.Acquire(name)
		if err != nil {
			return 0, err
		}
		if sp == nil {
			continue
		}
		var b int64
		sp.db.QueryRow(`SELECT bytes FROM usage WHERE writer = ?`, id).Scan(&b)
		s.Release(sp)
		total += b
	}
	s.cacheMu.Lock()
	if len(s.vbytes) >= 100_000 {
		s.vbytes = map[string]int64{}
	}
	s.vbytes[id] = total
	s.cacheMu.Unlock()
	return total, nil
}

func (s *Store) adjustVisitor(id string, delta int64) {
	s.cacheMu.Lock()
	if n, ok := s.vbytes[id]; ok {
		s.vbytes[id] = max(0, n+delta)
	}
	s.cacheMu.Unlock()
}

// noteVisitorSpace remembers that a visitor has data in a space, so cleanup
// and usage counting know where to look.
func (s *Store) noteVisitorSpace(id, space string) {
	k := id + " " + space
	s.cacheMu.Lock()
	seen := s.vspaces[k]
	if !seen {
		if len(s.vspaces) >= 100_000 {
			s.vspaces = map[string]bool{}
		}
		s.vspaces[k] = true
	}
	s.cacheMu.Unlock()
	if !seen {
		s.srv.Exec(`INSERT OR IGNORE INTO visitor_spaces (visitor, space) VALUES (?, ?)`, id, space)
		s.srv.Exec(`UPDATE visitors SET stored = 1 WHERE id = ? AND stored = 0`, id)
	}
}

// DeleteVisitor removes a visitor that was just created and never used, for
// a join that turned out to fail.
func (s *Store) DeleteVisitor(id string) {
	s.srv.Exec(`DELETE FROM visitor_spaces WHERE visitor = ?`, id)
	s.srv.Exec(`DELETE FROM visitors WHERE id = ?`, id)
}
