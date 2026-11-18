package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"

	"github.com/timjaanson/cubby/internal/apierr"
)

type WriteReq struct {
	Space, Area, Key string
	Value            []byte // nil means delete
	IsJSON           bool
	Writer           string
	IfVersion        *int64
	IfBefore         *int64
	TTL              int64 // seconds, zero makes permanent; for a point, counted from T
	Point            bool  // add a series point instead of setting the key
	Check            bool  // only compare IfVersion with the key's version; write nothing
	T                int64 // a point's time in ms; 0 for the server's clock
}

type WriteResult struct {
	Version    int64
	Created    bool // a new key, for 201
	Changed    bool // false for an identical rewrite
	T, Expires int64
	Grown      int64 // bytes the space grew by, zero when it shrank
}

// BatchError is a write in a batch that failed; the whole batch was
// rolled back.
type BatchError struct {
	Index int
	Err   error
}

func (e *BatchError) Error() string { return e.Err.Error() }
func (e *BatchError) Unwrap() error { return e.Err }

// Write is a batch of one.
func (s *Store) Write(ctx context.Context, r WriteReq) (WriteResult, error) {
	res, err := s.WriteBatch(ctx, []WriteReq{r})
	var be *BatchError
	if errors.As(err, &be) {
		err = be.Err
	}
	if err != nil {
		return WriteResult{}, err
	}
	return res[0], nil
}

// batchState is what a batch has done so far inside its transaction.
type batchState struct {
	seq, bytes  int64
	keys        int
	vTotal      int64 // the writer's bytes across spaces, as of this write
	spaceDelta  int64
	changed     bool // a version was taken
	touched     bool // something was written to the file
	active      bool // last_active_at moves
	usage       map[string]int64
	maxBytes    int64
	maxKeys     int
	serverTotal int64
}

// WriteBatch writes to several keys of one space, all by one writer, in one
// transaction: every write lands or none does. Each changed key takes the
// next version, in order. A failure is a *BatchError naming the write.
func (s *Store) WriteBatch(ctx context.Context, reqs []WriteReq) (out []WriteResult, err error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	space, writer := reqs[0].Space, reqs[0].Writer
	for _, r := range reqs {
		if r.Space != space || r.Writer != writer {
			return nil, errors.New("store: a batch has one space and one writer")
		}
	}
	fail := func(i int, err error) ([]WriteResult, error) { return nil, &BatchError{Index: i, Err: err} }

	// Counted before taking the space, since counting may open other spaces.
	var vTotal int64
	for _, r := range reqs {
		if r.Value != nil {
			if vTotal, err = s.VisitorTotal(writer); err != nil {
				return nil, err
			}
			break
		}
	}

	sp, err := s.Acquire(space)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return fail(0, apierr.NoSpace)
	}
	defer s.Release(sp)

	_, maxBytes, maxKeys := s.cfg.LimitsFor(space)

	sp.wmu.Lock()
	defer sp.wmu.Unlock()
	removed, err := s.expireLocked(sp)
	if err != nil {
		return nil, err
	}
	vTotal -= removed[writer]
	nowMS := NowMs()
	now := nowMS / 1000
	for i, r := range reqs {
		if err := CheckBefore(r.IfBefore, nowMS); err != nil {
			return fail(i, err)
		}
	}
	tx, err := sp.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	m, err := readMeta(tx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	total := s.total
	s.mu.Unlock()
	st := &batchState{seq: m.seq, bytes: m.bytes, keys: m.keys, vTotal: vTotal, usage: map[string]int64{},
		maxBytes: maxBytes, maxKeys: maxKeys, serverTotal: total}
	pointT, err := serverPointTime(tx, reqs, nowMS)
	if err != nil {
		return nil, err
	}
	_, maxPoints := s.cfg.SeriesLimitsFor(space)

	out = make([]WriteResult, len(reqs))
	for i, r := range reqs {
		var res WriteResult
		if r.Check {
			res, err = checkStep(tx, r)
		} else if r.Point {
			t := r.T
			if t == 0 {
				t = pointT
			}
			res, err = s.pointStep(tx, st, r, t, maxPoints, nowMS)
		} else {
			res, err = s.writeStep(tx, st, r, nowMS)
		}
		if err != nil {
			return fail(i, err)
		}
		out[i] = res
	}
	if !st.touched {
		return out, nil
	}
	meta := map[string]any{}
	if st.changed || st.spaceDelta != 0 {
		meta["seq"], meta["bytes"], meta["keys"] = st.seq, st.bytes, st.keys
	}
	if st.active {
		meta["last_active_at"] = now
	}
	for k, v := range meta {
		if err := setMeta(tx, k, v); err != nil {
			return nil, err
		}
	}
	next, err := nextExpiry(tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.rememberExpiry(space, next)
	if st.active {
		sp.metaActive = now
	}

	s.mu.Lock()
	if x := s.stats[space]; x != nil {
		x.bytes += st.spaceDelta
		x.keys += st.keys - m.keys
		if st.active {
			x.lastActive = now
		}
	}
	s.total += st.spaceDelta
	s.mu.Unlock()
	for w, d := range st.usage {
		s.adjustVisitor(w, d)
	}
	for _, r := range reqs {
		if r.Value != nil {
			s.noteVisitorSpace(writer, space)
			s.noteRecipient(r.Area, space)
		}
	}
	return out, nil
}

// checkStep compares a key's version, as the batch has left it so far,
// with IfVersion: 0 means the key must not exist. It writes nothing.
func checkStep(tx *sql.Tx, r WriteReq) (res WriteResult, err error) {
	var cur int64
	err = tx.QueryRow(`SELECT version FROM kv WHERE area = ? AND key = ? AND value IS NOT NULL`, r.Area, r.Key).Scan(&cur)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return res, err
	}
	if r.IfVersion == nil || *r.IfVersion != cur {
		return res, &apierr.Error{Status: 412, Code: "version_conflict", Message: "The key changed since that version.", Extra: map[string]any{"currentVersion": cur}}
	}
	return WriteResult{Version: cur}, nil
}

// writeStep is one write inside a batch's transaction.
func (s *Store) writeStep(tx *sql.Tx, st *batchState, r WriteReq, nowMS int64) (res WriteResult, err error) {
	del := r.Value == nil
	now := nowMS / 1000
	expires := int64(0)
	if r.TTL > 0 {
		expires = nowMS + r.TTL*1000
	}

	var old struct {
		value   []byte
		version int64
		writer  string
		size    int64
		row     bool
		series  bool
		t       int64
		expires int64
	}
	err = tx.QueryRow(`SELECT value, version, writer, size, series, coalesce(t,0), expires FROM kv WHERE area = ? AND key = ?`,
		r.Area, r.Key).Scan(&old.value, &old.version, &old.writer, &old.size, &old.series, &old.t, &old.expires)
	if err == nil {
		old.row = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return res, err
	}
	live := old.row && old.value != nil
	cur := int64(0)
	if live {
		cur = old.version
	}
	if r.IfVersion != nil && *r.IfVersion != cur {
		return res, &apierr.Error{Status: 412, Code: "version_conflict", Message: "The key changed since that version.", Extra: map[string]any{"currentVersion": cur}}
	}
	if del && !live {
		return res, apierr.NotFound
	}
	if live && old.series && !del {
		return res, apierr.IsSeries(r.Key)
	}
	// Deleting a series deletes its points.
	var pointBytes int64
	if del && old.series {
		if err := tx.QueryRow(`SELECT coalesce(sum(length(key) + length(value)), 0) FROM points WHERE area = ? AND key = ?`,
			r.Area, r.Key).Scan(&pointBytes); err != nil {
			return res, err
		}
		if _, err := tx.Exec(`DELETE FROM points WHERE area = ? AND key = ?`, r.Area, r.Key); err != nil {
			return res, err
		}
	}
	if live && !del && bytes.Equal(old.value, r.Value) {
		if expires == old.expires {
			return WriteResult{Version: cur, T: old.t, Expires: expires}, nil
		}
		if _, err := tx.Exec(`UPDATE kv SET expires=? WHERE area=? AND key=?`, expires, r.Area, r.Key); err != nil {
			return res, err
		}
		st.touched = true
		if r.TTL > 0 {
			st.active = true
		}
		return WriteResult{Version: cur, T: old.t, Expires: expires}, nil
	}

	newSize := int64(len(r.Key))
	if !del {
		newSize += int64(len(r.Value))
	}
	oldSize := int64(0)
	if old.row {
		oldSize = old.size
	}
	spaceDelta := newSize - oldSize - pointBytes
	keysDelta := 0
	switch {
	case !del && !live:
		keysDelta = 1
	case del:
		keysDelta = -1
	}

	// Quotas apply only to growth.
	visitorDelta := int64(0)
	if !del {
		if keysDelta > 0 && st.keys+keysDelta > st.maxKeys {
			return res, apierr.Quota("space_quota_exceeded", "The space has as many keys as it may hold.", int64(st.maxKeys), int64(st.keys))
		}
		if spaceDelta > 0 && st.bytes+spaceDelta > st.maxBytes {
			return res, apierr.Quota("space_quota_exceeded", "The space is full.", st.maxBytes, st.bytes)
		}
		visitorDelta = newSize
		if live && old.writer == r.Writer {
			visitorDelta -= oldSize
		}
		if lim := int64(s.cfg.Limits.MaxVisitorBytes); visitorDelta > 0 && st.vTotal+visitorDelta > lim {
			return res, apierr.Quota("visitor_quota_exceeded", "You have stored as much as one visitor may.", lim, st.vTotal)
		}
		if spaceDelta > 0 && st.serverTotal+st.spaceDelta+spaceDelta > int64(s.cfg.Limits.MaxTotalBytes) {
			return res, apierr.ServerFull
		}
	} else if live && old.writer == r.Writer {
		visitorDelta = -oldSize
	}

	st.seq++
	version := st.seq
	// A tombstone records who deleted the key. Usage is moved by old.writer
	// below, and tombstones count against the space only.
	var val any
	if !del {
		val = r.Value
	}
	if _, err := tx.Exec(`INSERT INTO kv (area, key, value, is_json, version, writer, size, updated_at, series, t, n, expires, expired)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, 0, ?, 0)
		ON CONFLICT (area, key) DO UPDATE SET value = excluded.value, is_json = excluded.is_json,
		version = excluded.version, writer = excluded.writer, size = excluded.size, updated_at = excluded.updated_at,
		series = 0, t = excluded.t, n = 0, expires=excluded.expires, expired=0`,
		r.Area, r.Key, val, r.IsJSON, version, r.Writer, newSize, now, nowMS, expires); err != nil {
		return res, err
	}
	// The old writer gives up the old bytes, the new writer takes the new
	// ones. Tombstones count against the space only.
	if live {
		if _, err := tx.Exec(`UPDATE usage SET bytes = max(0, bytes - ?) WHERE writer = ?`, oldSize, old.writer); err != nil {
			return res, err
		}
		st.usage[old.writer] -= oldSize
	}
	if !del {
		if _, err := tx.Exec(`INSERT INTO usage (writer, bytes) VALUES (?, ?) ON CONFLICT (writer) DO UPDATE SET bytes = bytes + excluded.bytes`, r.Writer, newSize); err != nil {
			return res, err
		}
		st.usage[r.Writer] += newSize
	}
	st.bytes += spaceDelta
	st.keys += keysDelta
	st.spaceDelta += spaceDelta
	st.vTotal += visitorDelta
	st.changed, st.touched, st.active = true, true, true
	return WriteResult{Version: version, Created: !live, Changed: true, T: nowMS, Expires: expires, Grown: max(0, spaceDelta)}, nil
}
