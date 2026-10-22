package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/timjaanson/cubby/internal/apierr"
)

// A series key keeps every value written to it with POST {key}/series, each
// with a time in milliseconds. Its kv row holds the newest point, so reads,
// lists and watches treat it like any key. The points live in their own table
// and are only ever stored and returned, never computed on.

type PointIn struct {
	Key    string
	Value  []byte
	IsJSON bool
}

type PointsReq struct {
	Space, Area string
	Points      []PointIn // one per key, in request order
	T           int64     // 0: the server's clock
	Writer      string
	IfBefore    *int64
}

type pointsResult struct {
	Version int64 // highest version among the keys afterwards
	T       int64
	Created bool // some key was new
	Changed bool // some key got a new newest point
}

func (s *Store) AddPoints(ctx context.Context, r PointsReq) (res pointsResult, err error) {
	vTotal, err := s.VisitorTotal(r.Writer)
	if err != nil {
		return res, err
	}
	sp, err := s.Acquire(r.Space)
	if err != nil {
		return res, err
	}
	if sp == nil {
		return res, apierr.NoSpace
	}
	defer s.Release(sp)

	_, maxBytes, maxKeys := s.cfg.LimitsFor(r.Space)
	_, maxPoints := s.cfg.SeriesLimitsFor(r.Space)
	now := nowS()

	sp.wmu.Lock()
	defer sp.wmu.Unlock()
	removed, err := s.expireLocked(sp)
	if err != nil {
		return res, err
	}
	vTotal -= removed[r.Writer]
	if err := CheckBefore(r.IfBefore, NowMs()); err != nil {
		return res, err
	}
	tx, err := sp.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	m, err := readMeta(tx)
	if err != nil {
		return res, err
	}

	type oldRow struct {
		value   []byte
		version int64
		writer  string
		size    int64
		series  bool
		t, n    int64
		row     bool
	}
	olds := make([]oldRow, len(r.Points))
	for i, p := range r.Points {
		var o oldRow
		var t sql.NullInt64
		err := tx.QueryRow(`SELECT value, version, writer, size, series, t, n FROM kv WHERE area = ? AND key = ?`, r.Area, p.Key).
			Scan(&o.value, &o.version, &o.writer, &o.size, &o.series, &t, &o.n)
		switch {
		case err == nil:
			o.row, o.t = true, t.Int64
		case !errors.Is(err, sql.ErrNoRows):
			return res, err
		}
		if o.row && o.value != nil && !o.series {
			return res, apierr.NotSeries(p.Key)
		}
		olds[i] = o
	}

	// One time for every point in the request. Without ?t= the server's
	// clock, but always after each series' newest point, so points the
	// server times never collide.
	t := r.T
	if t == 0 {
		t = NowMs()
		for _, o := range olds {
			if o.row && o.value != nil {
				t = max(t, o.t+1)
			}
		}
	}

	seq := m.seq
	var spaceDelta, visitorDelta int64
	keysDelta := 0
	type usageMove struct {
		writer string
		delta  int64
	}
	var moves []usageMove

	for i, p := range r.Points {
		o := olds[i]
		live := o.row && o.value != nil
		n := int64(0)
		if live {
			n = o.n
		}

		// A point at an existing time replaces it.
		var replaced int64
		err := tx.QueryRow(`SELECT length(key) + length(value) FROM points WHERE area = ? AND key = ? AND t = ?`, r.Area, p.Key, t).Scan(&replaced)
		switch {
		case err == nil:
		case errors.Is(err, sql.ErrNoRows):
			n++
		default:
			return res, err
		}
		if _, err := tx.Exec(`INSERT INTO points (area, key, t, value, is_json) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (area, key, t) DO UPDATE SET value = excluded.value, is_json = excluded.is_json`,
			r.Area, p.Key, t, p.Value, p.IsJSON); err != nil {
			return res, err
		}
		spaceDelta += int64(len(p.Key)+len(p.Value)) - replaced

		// Keep the newest maxPoints.
		if extra := n - int64(maxPoints); extra > 0 {
			var dropped int64
			if err := tx.QueryRow(`SELECT coalesce(sum(length(key) + length(value)), 0) FROM
				(SELECT key, value FROM points WHERE area = ? AND key = ? ORDER BY t LIMIT ?)`, r.Area, p.Key, extra).Scan(&dropped); err != nil {
				return res, err
			}
			if _, err := tx.Exec(`DELETE FROM points WHERE area = ? AND key = ? AND t IN
				(SELECT t FROM points WHERE area = ? AND key = ? ORDER BY t LIMIT ?)`, r.Area, p.Key, r.Area, p.Key, extra); err != nil {
				return res, err
			}
			spaceDelta -= dropped
			n -= extra
		}

		newest := !live || t >= o.t
		if !newest {
			// An older point: the key keeps its value and version.
			if _, err := tx.Exec(`UPDATE kv SET n = ? WHERE area = ? AND key = ?`, n, r.Area, p.Key); err != nil {
				return res, err
			}
			res.Version = max(res.Version, o.version)
			continue
		}

		seq++
		newSize := int64(len(p.Key) + len(p.Value))
		oldSize := int64(0)
		if o.row {
			oldSize = o.size
		}
		spaceDelta += newSize - oldSize
		if !live {
			keysDelta++
			res.Created = true
		}
		// The series key counts towards the writer like a plain key of its
		// newest value. The points count towards the space only.
		if live {
			moves = append(moves, usageMove{o.writer, -oldSize})
			if o.writer == r.Writer {
				visitorDelta -= oldSize
			}
		}
		moves = append(moves, usageMove{r.Writer, newSize})
		visitorDelta += newSize

		if _, err := tx.Exec(`INSERT INTO kv (area, key, value, is_json, version, writer, size, updated_at, series, t, n)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
			ON CONFLICT (area, key) DO UPDATE SET value = excluded.value, is_json = excluded.is_json,
			version = excluded.version, writer = excluded.writer, size = excluded.size, updated_at = excluded.updated_at,
			series = 1, t = excluded.t, n = excluded.n, expires=0, expired=0`,
			r.Area, p.Key, p.Value, p.IsJSON, seq, r.Writer, newSize, now, t, n); err != nil {
			return res, err
		}
		res.Version = max(res.Version, seq)
		res.Changed = true
	}

	// Quotas apply to growth only. Trimming came first, so a full series at
	// a steady rate doesn't grow.
	if keysDelta > 0 && m.keys+keysDelta > maxKeys {
		return res, apierr.Quota("space_quota_exceeded", "The space has as many keys as it may hold.", int64(maxKeys), int64(m.keys))
	}
	if spaceDelta > 0 && m.bytes+spaceDelta > maxBytes {
		return res, apierr.Quota("space_quota_exceeded", "The space is full.", maxBytes, m.bytes)
	}
	if lim := int64(s.cfg.Limits.MaxVisitorBytes); visitorDelta > 0 && vTotal+visitorDelta > lim {
		return res, apierr.Quota("visitor_quota_exceeded", "You have stored as much as one visitor may.", lim, vTotal)
	}
	s.mu.Lock()
	total := s.total
	s.mu.Unlock()
	if spaceDelta > 0 && total+spaceDelta > int64(s.cfg.Limits.MaxTotalBytes) {
		return res, apierr.ServerFull
	}

	for _, mv := range moves {
		if _, err := tx.Exec(`INSERT INTO usage (writer, bytes) VALUES (?, max(0, ?)) ON CONFLICT (writer) DO UPDATE SET bytes = max(0, bytes + ?)`,
			mv.writer, mv.delta, mv.delta); err != nil {
			return res, err
		}
	}
	for k, v := range map[string]any{"seq": seq, "bytes": m.bytes + spaceDelta, "keys": m.keys + keysDelta, "last_active_at": now} {
		if err := setMeta(tx, k, v); err != nil {
			return res, err
		}
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	sp.metaActive = now

	s.mu.Lock()
	if st := s.stats[r.Space]; st != nil {
		st.bytes += spaceDelta
		st.keys += keysDelta
		st.lastActive = now
	}
	s.total += spaceDelta
	s.mu.Unlock()
	for _, mv := range moves {
		s.adjustVisitor(mv.writer, mv.delta)
	}
	s.noteVisitorSpace(r.Writer, r.Space)
	res.T = t
	return res, nil
}

// readPoints returns points oldest first: with since, the first limit after
// it and the next since if there are more; without, the newest limit.
func (s *Store) ReadPoints(space, area, key string, since *int64, limit int) (points []Point, next *int64, version int64, err error) {
	sp, err := s.Acquire(space)
	if err != nil {
		return nil, nil, 0, err
	}
	if sp == nil {
		return nil, nil, 0, apierr.NoSpace
	}
	defer s.Release(sp)
	if err := s.expire(sp); err != nil {
		return nil, nil, 0, err
	}
	var series bool
	err = sp.db.QueryRow(`SELECT series FROM kv WHERE area = ? AND key = ? AND value IS NOT NULL`, area, key).Scan(&series)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, 0, apierr.NotFound
	}
	if err != nil {
		return nil, nil, 0, err
	}
	if !series {
		return nil, nil, 0, apierr.NotSeries(key)
	}
	s.touchSpace(sp)
	m, err := readMeta(sp.db)
	if err != nil {
		return nil, nil, 0, err
	}

	var rows *sql.Rows
	if since != nil {
		rows, err = sp.db.Query(`SELECT t, value, is_json FROM points WHERE area = ? AND key = ? AND t > ? ORDER BY t LIMIT ?`, area, key, *since, limit+1)
	} else {
		rows, err = sp.db.Query(`SELECT t, value, is_json FROM points WHERE area = ? AND key = ? ORDER BY t DESC LIMIT ?`, area, key, limit)
	}
	if err != nil {
		return nil, nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		p := Point{Area: area, Key: key}
		if err := rows.Scan(&p.T, &p.Value, &p.IsJSON); err != nil {
			return nil, nil, 0, err
		}
		points = append(points, p)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, err
	}
	if since == nil {
		for i, j := 0, len(points)-1; i < j; i, j = i+1, j-1 {
			points[i], points[j] = points[j], points[i]
		}
	} else if len(points) > limit {
		points = points[:limit]
		nt := points[limit-1].T
		next = &nt
	}
	return points, next, m.seq, nil
}
