package store

import (
	"time"

	"github.com/timjaanson/cubby/internal/config"
)

type CleanupReport struct {
	Visitors   int `json:"visitors"`
	Tombstones int `json:"tombstones"`
	Spaces     int `json:"spaces"`
}

// dropVisitorRows removes a visitor's data, grants and ban from one space:
// their users/ keys, what they sent under to/ and what was sent to them. All
// of it becomes tombstones, so watchers on the other side see it go; their
// own to/me/ slots go too.
func (s *Store) dropVisitorRows(space, id string) error {
	sp, err := s.Acquire(space)
	if err != nil || sp == nil {
		return err
	}
	defer s.Release(sp)
	sp.wmu.Lock()
	defer sp.wmu.Unlock()
	tx, err := sp.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	m, err := readMeta(tx)
	if err != nil {
		return err
	}
	const mine = `(area = ? OR (area >= 'i:' AND area < 'i;' AND (substr(area, 1, ?) = ? OR substr(area, ?) = ?)))`
	in := "i:" + id + ":"
	mineArgs := []any{"u:" + id, len(in), in, -(len(id) + 1), ":" + id}
	rows, err := tx.Query(`SELECT area, key, size, value IS NOT NULL, writer FROM kv WHERE `+mine, mineArgs...)
	if err != nil {
		return err
	}
	type row struct {
		area, key, writer string
		size              int64
		live              bool
	}
	var rs []row
	for rows.Next() {
		var r row
		rows.Scan(&r.area, &r.key, &r.size, &r.live, &r.writer)
		rs = append(rs, r)
	}
	rows.Close()
	now := nowS()
	seq, bytesD, keysD, changed := m.seq, int64(0), 0, false
	others := map[string]int64{} // usage given back by other writers: what they sent to this visitor
	for _, r := range rs {
		if r.live {
			keysD--
		}
		if r.live && r.writer != id {
			others[r.writer] += r.size
			tx.Exec(`UPDATE usage SET bytes = max(0, bytes - ?) WHERE writer = ?`, r.size, r.writer)
		}
		if r.live {
			seq++
			bytesD += int64(len(r.key)) - r.size
			tx.Exec(`UPDATE kv SET value = NULL, version = ?, size = ?, updated_at = ?, series = 0, t = ?, n = 0, expires=0, expired=0 WHERE area = ? AND key = ?`, seq, len(r.key), now, NowMs(), r.area, r.key)
			changed = true
		} else {
			bytesD -= r.size
			tx.Exec(`DELETE FROM kv WHERE area = ? AND key = ?`, r.area, r.key)
		}
	}
	var pointBytes int64
	tx.QueryRow(`SELECT coalesce(sum(length(key) + length(value)), 0) FROM points WHERE `+mine, mineArgs...).Scan(&pointBytes)
	tx.Exec(`DELETE FROM points WHERE `+mine, mineArgs...)
	bytesD -= pointBytes
	tx.Exec(`DELETE FROM usage WHERE writer = ?`, id)
	tx.Exec(`DELETE FROM grants WHERE visitor = ?`, id)
	tx.Exec(`DELETE FROM bans WHERE visitor = ?`, id)
	for k, v := range map[string]any{"seq": seq, "bytes": m.bytes + bytesD, "keys": m.keys + keysD} {
		setMeta(tx, k, v)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	sp.dropAccess()
	for w, b := range others {
		s.adjustVisitor(w, -b)
	}
	s.mu.Lock()
	if st := s.stats[space]; st != nil {
		st.bytes += bytesD
		st.keys += keysD
	}
	s.total += bytesD
	s.mu.Unlock()
	if changed {
		s.OnChange(space)
	}
	return nil
}

func (s *Store) Cleanup() (rep CleanupReport, err error) {
	now := nowS()
	r := s.cfg.Retention
	secs := func(d config.Duration) int64 { return int64(time.Duration(d).Seconds()) }
	s.Rescan()

	// 1. Visitors past their idle time, with their users/ data and what they
	// sent and received under to/.
	rows, err := s.srv.Query(`SELECT id FROM visitors WHERE last_seen_at < ? OR (stored = 0 AND last_seen_at < ?)`,
		now-secs(r.VisitorIdleTTL), now-secs(r.EmptyVisitorIdleTTL))
	if err != nil {
		return rep, err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := s.PurgeVisitor(id); err != nil {
			return rep, err
		}
		rep.Visitors++
	}

	// Devices unseen as long as a visitor may be, and links that ended a
	// day ago. A visitor whose other devices are in use stays.
	nowMs := NowMs()
	s.srv.Exec(`DELETE FROM devices WHERE last_seen_at < ?`, nowMs-secs(r.VisitorIdleTTL)*1000)
	s.srv.Exec(`DELETE FROM links WHERE coalesce(ended_at, expires) < ?`, nowMs-24*3600*1000)

	// 2. Idle spaces, unless pinned.
	idle := now - secs(r.SpaceIdleTTL)
	var names, all []string
	s.mu.Lock()
	for n, st := range s.stats {
		all = append(all, n)
		if st.lastActive < idle && !s.cfg.Pinned(n) {
			names = append(names, n)
		}
	}
	s.mu.Unlock()
	for _, n := range names {
		if err := s.DeleteSpace(n); err == nil {
			rep.Spaces++
		}
	}

	// 3. Grants past their end and access tokens long dead.
	for _, n := range all {
		if err := s.pruneAccess(n); err != nil {
			return rep, err
		}
	}

	// 4. Old tombstones, then compact. The space remembers the highest
	// removed version as its floor.
	cut := now - secs(r.TombstoneTTL)
	for _, n := range all {
		k, err := s.compactSpace(n, cut)
		if err != nil {
			return rep, err
		}
		rep.Tombstones += k
	}

	s.cacheMu.Lock()
	for k, t := range s.vtouched {
		if now-t > 3600 {
			delete(s.vtouched, k)
		}
	}
	s.cacheMu.Unlock()
	return rep, nil
}

func (s *Store) compactSpace(name string, cut int64) (int, error) {
	sp, err := s.Acquire(name)
	if err != nil || sp == nil {
		return 0, err
	}
	defer s.Release(sp)
	sp.wmu.Lock()
	defer sp.wmu.Unlock()
	tx, err := sp.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var n int
	var maxV, size int64
	tx.QueryRow(`SELECT count(*), coalesce(max(version), 0), coalesce(sum(size), 0) FROM kv WHERE value IS NULL AND updated_at < ?`, cut).Scan(&n, &maxV, &size)
	if n == 0 {
		return 0, nil
	}
	m, err := readMeta(tx)
	if err != nil {
		return 0, err
	}
	tx.Exec(`DELETE FROM kv WHERE value IS NULL AND updated_at < ?`, cut)
	setMeta(tx, "tombstone_floor", max(m.floor, maxV))
	setMeta(tx, "bytes", m.bytes-size)
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	if st := s.stats[name]; st != nil {
		st.bytes -= size
	}
	s.total -= size
	s.mu.Unlock()
	sp.db.Exec(`PRAGMA incremental_vacuum`)
	sp.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return n, nil
}
