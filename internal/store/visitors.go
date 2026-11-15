package store

import (
	"database/sql"
	"errors"
	"strings"

	"github.com/timjaanson/cubby/internal/names"
)

// NewVisitor creates a visitor with its first device.
func (s *Store) NewVisitor() (token, id string, err error) {
	token, id = names.RandomToken("vt_"), names.NewVisitorID()
	tx, err := s.srv.Begin()
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO visitors (id, created_at, last_seen_at) VALUES (?, ?, ?)`, id, nowS(), nowS()); err != nil {
		return "", "", err
	}
	if err = insertDevice(tx, names.NewDeviceID(), id, token, "", nil); err != nil {
		return "", "", err
	}
	return token, id, tx.Commit()
}

func insertDevice(tx *sql.Tx, dev, visitor, token, label string, linkedBy *string) error {
	now := NowMs()
	_, err := tx.Exec(`INSERT INTO devices (id, visitor, token_hash, label, created_at, last_seen_at, linked_by) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		dev, visitor, names.HashSecret(token), label, now, now, linkedBy)
	return err
}

// VisitorByToken returns the visitor and device a token belongs to, or ""
// for an unknown token. It also counts both as seen, at most once an hour.
func (s *Store) VisitorByToken(token string) (id, device string, err error) {
	err = s.srv.QueryRow(`SELECT visitor, id FROM devices WHERE token_hash = ?`, names.HashSecret(token)).Scan(&id, &device)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	now := nowS()
	s.cacheMu.Lock()
	stale := now-s.vtouched[device] >= 3600
	if stale {
		s.vtouched[device] = now
	}
	s.cacheMu.Unlock()
	if stale {
		s.srv.Exec(`UPDATE devices SET last_seen_at = ? WHERE id = ?`, NowMs(), device)
		s.srv.Exec(`UPDATE visitors SET last_seen_at = ? WHERE id = ?`, now, id)
	}
	return id, device, nil
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
	s.srv.Exec(`DELETE FROM devices WHERE visitor = ?`, id)
	s.srv.Exec(`DELETE FROM visitors WHERE id = ?`, id)
}

// PurgeVisitor deletes a visitor and everything they had, the way expiry
// does: their users/ data, what they sent and received under to/, their
// grants and bans in every space, their devices and links.
func (s *Store) PurgeVisitor(id string) error {
	rows, err := s.srv.Query(`SELECT space FROM visitor_spaces WHERE visitor = ?`, id)
	if err != nil {
		return err
	}
	var spaces []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		spaces = append(spaces, n)
	}
	rows.Close()
	for _, sp := range spaces {
		if err := s.dropVisitorRows(sp, id); err != nil {
			return err
		}
	}
	s.srv.Exec(`DELETE FROM visitor_spaces WHERE visitor = ?`, id)
	s.srv.Exec(`DELETE FROM links WHERE visitor = ?`, id)
	s.srv.Exec(`DELETE FROM devices WHERE visitor = ?`, id)
	s.srv.Exec(`DELETE FROM visitors WHERE id = ?`, id)
	s.cacheMu.Lock()
	delete(s.vbytes, id)
	for k := range s.vspaces {
		if strings.HasPrefix(k, id+" ") {
			delete(s.vspaces, k)
		}
	}
	s.cacheMu.Unlock()
	return nil
}

// noteRecipient records the space for the recipient of a to/ slot, so their
// cleanup finds what was sent to them even where they never wrote.
func (s *Store) noteRecipient(area, space string) {
	if r, from, ok := SplitTo(area); ok && r != "owners" && r != from {
		s.noteVisitorSpace(r, space)
	}
}
