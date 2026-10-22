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
	TTL              int64 // seconds, zero makes permanent
}

type writeResult struct {
	Version    int64
	Created    bool // a new key, for 201
	Changed    bool // false for an identical rewrite
	T, Expires int64
}

func (s *Store) Write(ctx context.Context, r WriteReq) (res writeResult, err error) {
	del := r.Value == nil
	// Counted before taking the space, since counting may open other spaces.
	var vTotal int64
	if !del {
		if vTotal, err = s.VisitorTotal(r.Writer); err != nil {
			return res, err
		}
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
	now := nowS()

	sp.wmu.Lock()
	defer sp.wmu.Unlock()
	removed, err := s.expireLocked(sp)
	if err != nil {
		return res, err
	}
	vTotal -= removed[r.Writer]
	nowMS := NowMs()
	now = nowMS / 1000
	if err := CheckBefore(r.IfBefore, nowMS); err != nil {
		return res, err
	}
	expires := int64(0)
	if r.TTL > 0 {
		expires = nowMS + r.TTL*1000
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
			return writeResult{Version: cur, T: old.t, Expires: expires}, nil
		}
		if _, err := tx.Exec(`UPDATE kv SET expires=? WHERE area=? AND key=?`, expires, r.Area, r.Key); err != nil {
			return res, err
		}
		if r.TTL > 0 {
			if err := setMeta(tx, "last_active_at", now); err != nil {
				return res, err
			}
		}
		next, err := nextExpiry(tx)
		if err != nil {
			return res, err
		}
		if err := tx.Commit(); err != nil {
			return res, err
		}
		s.rememberExpiry(r.Space, next)
		if r.TTL > 0 {
			sp.metaActive = now
			s.mu.Lock()
			if st := s.stats[r.Space]; st != nil {
				st.lastActive = now
			}
			s.mu.Unlock()
		}
		return writeResult{Version: cur, T: old.t, Expires: expires}, nil
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
	if !del {
		if keysDelta > 0 && m.keys+keysDelta > maxKeys {
			return res, apierr.Quota("space_quota_exceeded", "The space has as many keys as it may hold.", int64(maxKeys), int64(m.keys))
		}
		if spaceDelta > 0 && m.bytes+spaceDelta > maxBytes {
			return res, apierr.Quota("space_quota_exceeded", "The space is full.", maxBytes, m.bytes)
		}
		visitorDelta := newSize
		if live && old.writer == r.Writer {
			visitorDelta -= oldSize
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
	}

	version := m.seq + 1
	// A tombstone records who deleted the key. Usage is moved by old.writer
	// below, and tombstones count against the space only.
	writer := r.Writer
	var val any
	if !del {
		val = r.Value
	}
	if _, err := tx.Exec(`INSERT INTO kv (area, key, value, is_json, version, writer, size, updated_at, series, t, n, expires, expired)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, 0, ?, 0)
		ON CONFLICT (area, key) DO UPDATE SET value = excluded.value, is_json = excluded.is_json,
		version = excluded.version, writer = excluded.writer, size = excluded.size, updated_at = excluded.updated_at,
		series = 0, t = excluded.t, n = 0, expires=excluded.expires, expired=0`,
		r.Area, r.Key, val, r.IsJSON, version, writer, newSize, now, nowMS, expires); err != nil {
		return res, err
	}
	for k, v := range map[string]any{"seq": version, "bytes": m.bytes + spaceDelta, "keys": m.keys + keysDelta, "last_active_at": now} {
		if err := setMeta(tx, k, v); err != nil {
			return res, err
		}
	}
	// The old writer gives up the old bytes, the new writer takes the new
	// ones. Tombstones count against the space only.
	if live {
		if _, err := tx.Exec(`UPDATE usage SET bytes = max(0, bytes - ?) WHERE writer = ?`, oldSize, old.writer); err != nil {
			return res, err
		}
	}
	if !del {
		if _, err := tx.Exec(`INSERT INTO usage (writer, bytes) VALUES (?, ?) ON CONFLICT (writer) DO UPDATE SET bytes = bytes + excluded.bytes`, r.Writer, newSize); err != nil {
			return res, err
		}
	}
	next, err := nextExpiry(tx)
	if err != nil {
		return res, err
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	s.rememberExpiry(r.Space, next)
	sp.metaActive = now

	s.mu.Lock()
	if st := s.stats[r.Space]; st != nil {
		st.bytes += spaceDelta
		st.keys += keysDelta
		st.lastActive = now
	}
	s.total += spaceDelta
	s.mu.Unlock()
	if live {
		s.adjustVisitor(old.writer, -oldSize)
	}
	if !del {
		s.adjustVisitor(r.Writer, newSize)
		s.noteVisitorSpace(r.Writer, r.Space)
	}
	return writeResult{Version: version, Created: !live, Changed: true, T: nowMS, Expires: expires}, nil
}
