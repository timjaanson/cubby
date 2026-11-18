package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

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
	TTL         int64     // seconds after T; 0 for no expiry
	Writer      string
	IfBefore    *int64
}

type pointsResult struct {
	Version int64 // highest version among the keys afterwards
	T       int64
	Created bool  // some key was new
	Changed bool  // some key got a new newest point
	Grown   int64 // bytes the space grew by, zero when it shrank
}

// AddPoints adds one point to each key, all at one time: a batch of points.
func (s *Store) AddPoints(ctx context.Context, r PointsReq) (res pointsResult, err error) {
	reqs := make([]WriteReq, len(r.Points))
	for i, p := range r.Points {
		reqs[i] = WriteReq{Space: r.Space, Area: r.Area, Key: p.Key, Value: p.Value, IsJSON: p.IsJSON,
			Writer: r.Writer, IfBefore: r.IfBefore, Point: true, T: r.T, TTL: r.TTL}
	}
	out, err := s.WriteBatch(ctx, reqs)
	var be *BatchError
	if errors.As(err, &be) {
		err = be.Err
	}
	if err != nil {
		return res, err
	}
	for _, o := range out {
		res.Version = max(res.Version, o.Version)
		res.T = o.T
		res.Created = res.Created || o.Created
		res.Changed = res.Changed || o.Changed
		res.Grown += o.Grown
	}
	return res, nil
}

// serverPointTime is the time for the points of a batch that don't carry
// one: the server's clock, but after the newest point of each of their
// series, so points the server times never collide.
func serverPointTime(tx *sql.Tx, reqs []WriteReq, nowMS int64) (int64, error) {
	t := nowMS
	for _, r := range reqs {
		if !r.Point || r.T != 0 {
			continue
		}
		var newest sql.NullInt64
		err := tx.QueryRow(`SELECT t FROM kv WHERE area = ? AND key = ? AND series = 1 AND value IS NOT NULL`, r.Area, r.Key).Scan(&newest)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if newest.Valid {
			t = max(t, newest.Int64+1)
		}
	}
	return t, nil
}

// pointStep adds one point at time t inside a batch's transaction. A point
// at an existing time replaces it. The series keeps its newest maxPoints;
// its kv row holds the newest point and counts towards the writer like a
// plain key, while the points count towards the space only.
func (s *Store) pointStep(tx *sql.Tx, st *batchState, r WriteReq, t int64, maxPoints int, nowMS int64) (res WriteResult, err error) {
	expires := int64(0)
	if r.TTL > 0 {
		expires = t + r.TTL*1000
		if expires <= nowMS {
			return res, apierr.BadParam("ttl", "This point expired before it arrived: t + ttl is in the past.")
		}
	}

	var old struct {
		value   []byte
		version int64
		writer  string
		size    int64
		series  bool
		t, n    int64
		row     bool
	}
	var ot sql.NullInt64
	err = tx.QueryRow(`SELECT value, version, writer, size, series, t, n FROM kv WHERE area = ? AND key = ?`, r.Area, r.Key).
		Scan(&old.value, &old.version, &old.writer, &old.size, &old.series, &ot, &old.n)
	switch {
	case err == nil:
		old.row, old.t = true, ot.Int64
	case !errors.Is(err, sql.ErrNoRows):
		return res, err
	}
	live := old.row && old.value != nil
	if live && !old.series {
		return res, apierr.NotSeries(r.Key)
	}
	n := int64(0)
	if live {
		n = old.n
	}

	var replaced int64
	err = tx.QueryRow(`SELECT length(key) + length(value) FROM points WHERE area = ? AND key = ? AND t = ?`, r.Area, r.Key, t).Scan(&replaced)
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		n++
	default:
		return res, err
	}
	if _, err := tx.Exec(`INSERT INTO points (area, key, t, value, is_json, expires) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (area, key, t) DO UPDATE SET value = excluded.value, is_json = excluded.is_json, expires = excluded.expires`,
		r.Area, r.Key, t, r.Value, r.IsJSON, expires); err != nil {
		return res, err
	}
	spaceDelta := int64(len(r.Key)+len(r.Value)) - replaced

	// Keep the newest maxPoints.
	if extra := n - int64(maxPoints); extra > 0 {
		var dropped int64
		if err := tx.QueryRow(`SELECT coalesce(sum(length(key) + length(value)), 0) FROM
			(SELECT key, value FROM points WHERE area = ? AND key = ? ORDER BY t LIMIT ?)`, r.Area, r.Key, extra).Scan(&dropped); err != nil {
			return res, err
		}
		if _, err := tx.Exec(`DELETE FROM points WHERE area = ? AND key = ? AND t IN
			(SELECT t FROM points WHERE area = ? AND key = ? ORDER BY t LIMIT ?)`, r.Area, r.Key, r.Area, r.Key, extra); err != nil {
			return res, err
		}
		spaceDelta -= dropped
		n -= extra
	}

	newest := !live || t >= old.t
	newSize, oldSize := int64(0), int64(0)
	keysDelta, visitorDelta := 0, int64(0)
	if newest {
		newSize = int64(len(r.Key) + len(r.Value))
		if old.row {
			oldSize = old.size
		}
		spaceDelta += newSize - oldSize
		if !live {
			keysDelta = 1
		}
		visitorDelta = newSize
		if live && old.writer == r.Writer {
			visitorDelta -= oldSize
		}
	}

	// Quotas apply to growth only. Trimming came first, so a full series at
	// a steady rate doesn't grow.
	if keysDelta > 0 && st.keys+keysDelta > st.maxKeys {
		return res, apierr.Quota("space_quota_exceeded", "The space has as many keys as it may hold.", int64(st.maxKeys), int64(st.keys))
	}
	if spaceDelta > 0 && st.bytes+spaceDelta > st.maxBytes {
		return res, apierr.Quota("space_quota_exceeded", "The space is full.", st.maxBytes, st.bytes)
	}
	if lim := int64(s.cfg.Limits.MaxVisitorBytes); visitorDelta > 0 && st.vTotal+visitorDelta > lim {
		return res, apierr.Quota("visitor_quota_exceeded", "You have stored as much as one visitor may.", lim, st.vTotal)
	}
	if spaceDelta > 0 && st.serverTotal+st.spaceDelta+spaceDelta > int64(s.cfg.Limits.MaxTotalBytes) {
		return res, apierr.ServerFull
	}
	st.bytes += spaceDelta
	st.spaceDelta += spaceDelta
	st.touched, st.active = true, true

	if !newest {
		// An older point: the key keeps its value and version.
		if _, err := tx.Exec(`UPDATE kv SET n = ? WHERE area = ? AND key = ?`, n, r.Area, r.Key); err != nil {
			return res, err
		}
		return WriteResult{Version: old.version, T: t, Expires: expires, Grown: max(0, spaceDelta)}, nil
	}

	st.seq++
	if _, err := tx.Exec(`INSERT INTO kv (area, key, value, is_json, version, writer, size, updated_at, series, t, n)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT (area, key) DO UPDATE SET value = excluded.value, is_json = excluded.is_json,
		version = excluded.version, writer = excluded.writer, size = excluded.size, updated_at = excluded.updated_at,
		series = 1, t = excluded.t, n = excluded.n, expires=0, expired=0`,
		r.Area, r.Key, r.Value, r.IsJSON, st.seq, r.Writer, newSize, nowMS/1000, t, n); err != nil {
		return res, err
	}
	if live {
		if _, err := tx.Exec(`UPDATE usage SET bytes = max(0, bytes - ?) WHERE writer = ?`, oldSize, old.writer); err != nil {
			return res, err
		}
		st.usage[old.writer] -= oldSize
	}
	if _, err := tx.Exec(`INSERT INTO usage (writer, bytes) VALUES (?, ?) ON CONFLICT (writer) DO UPDATE SET bytes = bytes + excluded.bytes`, r.Writer, newSize); err != nil {
		return res, err
	}
	st.usage[r.Writer] += newSize
	st.keys += keysDelta
	st.vTotal += visitorDelta
	st.changed = true
	return WriteResult{Version: st.seq, Created: !live, Changed: true, T: t, Expires: expires, Grown: max(0, spaceDelta)}, nil
}

// PointsQuery selects points of one series, or of every series under a
// prefix. Desc is newest first.
type PointsQuery struct {
	Since, Before *int64 // ms: Since inclusive, Before exclusive
	After         *int64 // the previous page's next: past it in reading order
	AfterKey      string // with After, across several series: the key at that time
	Desc          bool
	Limit         int
}

// MaxSeriesPerRead is how many series one prefix read may span.
const MaxSeriesPerRead = 100

// ReadSeriesPrefix returns points of every series whose key starts with
// prefix, merged by time (then by key), within the query's bounds. Each
// series is read through its own index and the pages merged here, so a long
// series costs no more than one page. next is the last point's t and key.
func (s *Store) ReadSeriesPrefix(space, area, prefix string, pq PointsQuery) (points []Point, next *Point, version int64, err error) {
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
	s.touchSpace(sp)
	m, err := readMeta(sp.db)
	if err != nil {
		return nil, nil, 0, err
	}
	rows, err := sp.db.Query(`SELECT key FROM kv WHERE area = ? AND series = 1 AND value IS NOT NULL AND substr(key, 1, ?) = ? ORDER BY key`,
		area, len(prefix), prefix)
	if err != nil {
		return nil, nil, 0, err
	}
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return nil, nil, 0, err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if len(keys) > MaxSeriesPerRead {
		return nil, nil, 0, &apierr.Error{Status: 400, Code: "too_many_series", Extra: map[string]any{"limit": MaxSeriesPerRead},
			Message: fmt.Sprintf("%d series match. One read spans at most %d: use a longer prefix.", len(keys), MaxSeriesPerRead)}
	}
	ord := "ASC"
	if pq.Desc {
		ord = "DESC"
	}
	for _, k := range keys {
		where, args := "area = ? AND key = ?", []any{area, k}
		if pq.Since != nil {
			where += " AND t >= ?"
			args = append(args, *pq.Since)
		}
		if pq.Before != nil {
			where += " AND t < ?"
			args = append(args, *pq.Before)
		}
		if pq.After != nil {
			// Past (After, AfterKey) in reading order: ties at the same t go by key.
			cmp := map[bool]string{true: "<", false: ">"}[pq.Desc]
			if k > pq.AfterKey {
				cmp += "="
			}
			where += " AND t " + cmp + " ?"
			args = append(args, *pq.After)
		}
		r, err := sp.db.Query(`SELECT t, value, is_json, expires FROM points WHERE `+where+` ORDER BY t `+ord+` LIMIT ?`, append(args, pq.Limit+1)...)
		if err != nil {
			return nil, nil, 0, err
		}
		for r.Next() {
			p := Point{Area: area, Key: k}
			if err := r.Scan(&p.T, &p.Value, &p.IsJSON, &p.Expires); err != nil {
				r.Close()
				return nil, nil, 0, err
			}
			points = append(points, p)
		}
		r.Close()
	}
	sort.Slice(points, func(i, j int) bool {
		if points[i].T != points[j].T {
			return points[i].T < points[j].T != pq.Desc
		}
		return points[i].Key < points[j].Key
	})
	size, cut := 0, len(points)
	for i, p := range points {
		if size >= PageBytes || i == pq.Limit {
			cut = i
			break
		}
		size += len(p.Value)
	}
	if cut < len(points) {
		points = points[:cut]
		last := points[cut-1]
		next = &last
	}
	return points, next, m.seq, nil
}

// ReadPoints returns up to Limit points with Since <= t < Before, newest
// first with Desc, else oldest first. After continues a previous page. When a
// page stops early, at Limit or at PageBytes, next is the t to pass as
// After.
func (s *Store) ReadPoints(space, area, key string, pq PointsQuery) (points []Point, next *int64, version int64, err error) {
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

	where, args := "area = ? AND key = ?", []any{area, key}
	if pq.Since != nil {
		where += " AND t >= ?"
		args = append(args, *pq.Since)
	}
	if pq.Before != nil {
		where += " AND t < ?"
		args = append(args, *pq.Before)
	}
	ord := "ASC"
	if pq.Desc {
		ord = "DESC"
	}
	if pq.After != nil {
		if pq.Desc {
			where += " AND t < ?"
		} else {
			where += " AND t > ?"
		}
		args = append(args, *pq.After)
	}
	fetch := pq.Limit + 1
	rows, err := sp.db.Query(`SELECT t, value, is_json, expires FROM points WHERE `+where+` ORDER BY t `+ord+` LIMIT ?`, append(args, fetch)...)
	if err != nil {
		return nil, nil, 0, err
	}
	defer rows.Close()
	size, cut := 0, false
	for rows.Next() {
		if size >= PageBytes {
			cut = true
			break
		}
		p := Point{Area: area, Key: key}
		if err := rows.Scan(&p.T, &p.Value, &p.IsJSON, &p.Expires); err != nil {
			return nil, nil, 0, err
		}
		points = append(points, p)
		size += len(p.Value)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, err
	}
	if len(points) > pq.Limit || cut {
		points = points[:min(pq.Limit, len(points))]
		nt := points[len(points)-1].T
		next = &nt
	}
	return points, next, m.seq, nil
}
