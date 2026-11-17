package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/timjaanson/cubby/internal/apierr"
)

// checkSpaceVersion refuses a space file from before sign-in with keys
// (v0.19 started over with an empty data directory) and moves version 5
// (v0.19, v0.20) to 6: points get an expiry.
func checkSpaceVersion(db *sql.DB, path string) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v == 5 {
		if _, err := db.Exec(fmt.Sprintf(`BEGIN;
ALTER TABLE points ADD COLUMN expires INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS points_by_expiry ON points (expires) WHERE expires > 0;
PRAGMA user_version = %d;
COMMIT;`, spaceVersion)); err != nil {
			db.Exec(`ROLLBACK`)
			return fmt.Errorf("migrate %s to format %d: %w", path, spaceVersion, err)
		}
		v = spaceVersion
	}
	if v != spaceVersion {
		return fmt.Errorf("%s is format %d, from a cubby before v0.19. v0.19 changed sign-in to device keys and starts with an empty data directory: move the old one away", path, v)
	}
	return nil
}

// nextExpiry is the earliest expiry of a key or a series point, 0 for none.
func nextExpiry(q queryer) (int64, error) {
	var next int64
	err := q.QueryRow(`SELECT coalesce(min(e), 0) FROM (
		SELECT min(expires) AS e FROM kv WHERE expires > 0 AND value IS NOT NULL
		UNION ALL SELECT min(expires) FROM points WHERE expires > 0)`).Scan(&next)
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
	gone := len(ds)
	pd, pg, pchanged, err := expirePoints(tx, &m.seq, now, removed)
	if err != nil {
		return nil, err
	}
	delta += pd
	gone += pg
	for k, v := range map[string]any{"seq": m.seq, "bytes": m.bytes + delta, "keys": m.keys - gone} {
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
		st.keys -= gone
	}
	s.total += delta
	s.mu.Unlock()
	for id, n := range removed {
		s.adjustVisitor(id, -n)
	}
	if len(ds) > 0 || pchanged {
		s.OnChange(sp.name)
	}
	return removed, nil
}

// expirePoints removes the series points that are due. A series whose
// newest point went shows its newest remaining point as a new version; one
// with no points left expires like a key. It returns the change in space
// bytes, the number of keys gone, and whether any key changed.
func expirePoints(tx *sql.Tx, seq *int64, now int64, removed map[string]int64) (delta int64, gone int, changed bool, err error) {
	rows, err := tx.Query(`SELECT area, key, count(*), sum(length(key) + length(value)) FROM points
		WHERE expires > 0 AND expires <= ? GROUP BY area, key ORDER BY area, key`, now)
	if err != nil {
		return 0, 0, false, err
	}
	type due struct {
		area, key string
		n, bytes  int64
	}
	var ds []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.area, &d.key, &d.n, &d.bytes); err != nil {
			rows.Close()
			return 0, 0, false, err
		}
		ds = append(ds, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, 0, false, err
	}
	for _, d := range ds {
		if _, err := tx.Exec(`DELETE FROM points WHERE area = ? AND key = ? AND expires > 0 AND expires <= ?`, d.area, d.key, now); err != nil {
			return 0, 0, false, err
		}
		delta -= d.bytes
		var writer string
		var size, t, n int64
		err := tx.QueryRow(`SELECT writer, size, coalesce(t, 0), n FROM kv WHERE area = ? AND key = ? AND series = 1 AND value IS NOT NULL`,
			d.area, d.key).Scan(&writer, &size, &t, &n)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, 0, false, err
		}
		n = max(0, n-d.n)
		var has int
		if err := tx.QueryRow(`SELECT count(*) FROM points WHERE area = ? AND key = ? AND t = ?`, d.area, d.key, t).Scan(&has); err != nil {
			return 0, 0, false, err
		}
		if has > 0 {
			// The newest point stays: the key's value doesn't change.
			if _, err := tx.Exec(`UPDATE kv SET n = ? WHERE area = ? AND key = ?`, n, d.area, d.key); err != nil {
				return 0, 0, false, err
			}
			continue
		}
		*seq++
		changed = true
		var pt int64
		var value []byte
		var isJSON bool
		err = tx.QueryRow(`SELECT t, value, is_json FROM points WHERE area = ? AND key = ? ORDER BY t DESC LIMIT 1`, d.area, d.key).Scan(&pt, &value, &isJSON)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// No points left: the key expires.
			delta += int64(len(d.key)) - size
			removed[writer] += size
			gone++
			if _, err := tx.Exec(`UPDATE kv SET value=NULL, version=?, size=?, updated_at=?, t=?, n=0, expires=0, expired=1 WHERE area=? AND key=?`,
				*seq, len(d.key), now/1000, now, d.area, d.key); err != nil {
				return 0, 0, false, err
			}
			if _, err := tx.Exec(`UPDATE usage SET bytes=max(0,bytes-?) WHERE writer=?`, size, writer); err != nil {
				return 0, 0, false, err
			}
		case err != nil:
			return 0, 0, false, err
		default:
			newSize := int64(len(d.key) + len(value))
			delta += newSize - size
			removed[writer] += size - newSize
			if _, err := tx.Exec(`UPDATE kv SET value=?, is_json=?, version=?, size=?, updated_at=?, t=?, n=? WHERE area=? AND key=?`,
				value, isJSON, *seq, newSize, now/1000, pt, n, d.area, d.key); err != nil {
				return 0, 0, false, err
			}
			if _, err := tx.Exec(`INSERT INTO usage (writer, bytes) VALUES (?, max(0, ?)) ON CONFLICT (writer) DO UPDATE SET bytes = max(0, bytes + ?)`,
				writer, newSize-size, newSize-size); err != nil {
				return 0, 0, false, err
			}
		}
	}
	return delta, gone, changed, nil
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
