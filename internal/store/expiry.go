package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/timjaanson/cubby/internal/apierr"
)

// checkSpaceVersion refuses a space file from before sign-in with keys.
// v0.19 started over with an empty data directory; there's no migration.
func checkSpaceVersion(db *sql.DB, path string) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v != spaceVersion {
		return fmt.Errorf("%s is format %d, from a cubby before v0.19. v0.19 changed sign-in to device keys and starts with an empty data directory: move the old one away", path, v)
	}
	return nil
}

func nextExpiry(q queryer) (int64, error) {
	var next int64
	err := q.QueryRow(`SELECT coalesce(min(expires),0) FROM kv WHERE expires > 0 AND value IS NOT NULL`).Scan(&next)
	return next, err
}

func (s *Store) rememberExpiry(name string, next int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if next == 0 {
		delete(s.expiring, name)
	} else {
		s.expiring[name] = next
	}
}

func (s *Store) setNextExpiry(name string, q queryer) error {
	next, err := nextExpiry(q)
	if err == nil {
		s.rememberExpiry(name, next)
	}
	return err
}

// expireLocked runs with the space write mutex held, before any conditional
// write. A failed condition cannot roll back an expiry that already happened.
func (s *Store) expireLocked(sp *spaceDB) (map[string]int64, error) {
	s.mu.Lock()
	next := s.expiring[sp.name]
	s.mu.Unlock()
	now := NowMs()
	if next == 0 || next > now {
		return nil, nil
	}
	tx, err := sp.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT area,key,writer,size FROM kv WHERE expires > 0 AND expires <= ? AND value IS NOT NULL ORDER BY expires,area,key`, now)
	if err != nil {
		return nil, err
	}
	type dead struct {
		area, key, writer string
		size              int64
	}
	var ds []dead
	for rows.Next() {
		var d dead
		if err := rows.Scan(&d.area, &d.key, &d.writer, &d.size); err != nil {
			rows.Close()
			return nil, err
		}
		ds = append(ds, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	m, err := readMeta(tx)
	if err != nil {
		return nil, err
	}
	removed := map[string]int64{}
	delta := int64(0)
	for _, d := range ds {
		m.seq++
		delta += int64(len(d.key)) - d.size
		removed[d.writer] += d.size
		if _, err := tx.Exec(`UPDATE kv SET value=NULL,version=?,size=?,updated_at=?,t=?,expires=0,expired=1 WHERE area=? AND key=?`, m.seq, len(d.key), now/1000, now, d.area, d.key); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE usage SET bytes=max(0,bytes-?) WHERE writer=?`, d.size, d.writer); err != nil {
			return nil, err
		}
	}
	for k, v := range map[string]any{"seq": m.seq, "bytes": m.bytes + delta, "keys": m.keys - len(ds)} {
		if err := setMeta(tx, k, v); err != nil {
			return nil, err
		}
	}
	next, err = nextExpiry(tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.rememberExpiry(sp.name, next)
	s.mu.Lock()
	if st := s.stats[sp.name]; st != nil {
		st.bytes += delta
		st.keys -= len(ds)
	}
	s.total += delta
	s.mu.Unlock()
	for id, n := range removed {
		s.adjustVisitor(id, -n)
	}
	if len(ds) > 0 {
		s.OnChange(sp.name)
	}
	return removed, nil
}

func (s *Store) expire(sp *spaceDB) error {
	s.mu.Lock()
	next := s.expiring[sp.name]
	s.mu.Unlock()
	if next == 0 || next > NowMs() {
		return nil
	}
	sp.wmu.Lock()
	defer sp.wmu.Unlock()
	_, err := s.expireLocked(sp)
	return err
}

// SweepExpired only opens files that have a due expiry. The serve loop calls
// it once a second. Reads and writes also expire on demand.
func (s *Store) SweepExpired(ctx context.Context) error {
	now := NowMs()
	var due []string
	s.mu.Lock()
	for name, t := range s.expiring {
		if t <= now {
			due = append(due, name)
		}
	}
	s.mu.Unlock()
	for _, name := range due {
		if err := ctx.Err(); err != nil {
			return err
		}
		sp, err := s.Acquire(name)
		if err != nil {
			return err
		}
		if sp == nil {
			continue
		}
		err = s.expire(sp)
		s.Release(sp)
		if err != nil {
			return fmt.Errorf("expire %s: %w", name, err)
		}
	}
	return nil
}

// CheckBefore is checked under the write mutex, before any mutation.
func CheckBefore(before *int64, now int64) error {
	if before != nil && now >= *before {
		return &apierr.Error{Status: 412, Code: "too_late", Message: "The write deadline has passed.", Extra: map[string]any{"t": now}}
	}
	return nil
}
