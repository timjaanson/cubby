package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Store keeps everything in one SQLite file in WAL mode. The spec sketches a
// file per space. One file is simpler and is plenty for the default limits
// (1000 spaces, 1 GB).
type Store struct {
	db  *sql.DB
	cfg *Config
	wmu sync.Mutex // serializes write transactions inside this process

	touchMu sync.Mutex
	touched map[string]int64 // "s:name" or "v:id" -> unix seconds last written to disk
}

const schema = `
CREATE TABLE IF NOT EXISTS spaces (
  name            TEXT PRIMARY KEY,
  seq             INTEGER NOT NULL,
  key_hash        TEXT,
  created_at      INTEGER NOT NULL,
  last_active_at  INTEGER NOT NULL,
  tombstone_floor INTEGER NOT NULL DEFAULT 0,
  bytes           INTEGER NOT NULL DEFAULT 0,
  keys            INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS kv (
  space      TEXT NOT NULL,
  area       TEXT NOT NULL,      -- 's', 'u:{id}', 'm:{id}'
  key        TEXT NOT NULL,
  value      BLOB,               -- NULL is a tombstone
  is_json    INTEGER NOT NULL,
  version    INTEGER NOT NULL,
  writer     TEXT NOT NULL,
  size       INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (space, area, key)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS kv_by_version ON kv (space, version);
CREATE INDEX IF NOT EXISTS kv_by_key ON kv (space, key, area);
CREATE INDEX IF NOT EXISTS kv_by_area ON kv (area);
CREATE INDEX IF NOT EXISTS kv_tombstones ON kv (updated_at) WHERE value IS NULL;
CREATE TABLE IF NOT EXISTS visitors (
  id           TEXT PRIMARY KEY,
  token_hash   TEXT NOT NULL UNIQUE,
  created_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  stored       INTEGER NOT NULL DEFAULT 0,
  bytes        INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS graveyard (
  name TEXT PRIMARY KEY,
  seq  INTEGER NOT NULL
);
`

func openStore(cfg *Config) (*Store, error) {
	dir := cfg.Server.DataDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "tiny-kv.db")
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=journal_size_limit(4194304)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	if _, err := db.Exec("PRAGMA auto_vacuum = INCREMENTAL"); err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &Store{db: db, cfg: cfg, touched: map[string]int64{}}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func nowMs() int64 { return time.Now().UnixMilli() }
func nowS() int64  { return time.Now().Unix() }

// ---------- visitors ----------

func (s *Store) newVisitor() (token, id string, err error) {
	token, id = randomToken("kv_"), newVisitorID()
	now := nowS()
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err = s.db.Exec(`INSERT INTO visitors (id, token_hash, created_at, last_seen_at) VALUES (?, ?, ?, ?)`,
		id, hashSecret(token), now, now)
	return
}

// visitorByToken returns "" for an unknown token.
func (s *Store) visitorByToken(token string) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM visitors WHERE token_hash = ?`, hashSecret(token)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err == nil {
		s.touch("v:"+id, `UPDATE visitors SET last_seen_at = ? WHERE id = ?`, id)
	}
	return id, err
}

// touch records activity at most once an hour per space or visitor.
func (s *Store) touch(k, query, arg string) {
	now := nowS()
	s.touchMu.Lock()
	last := s.touched[k]
	if now-last < 3600 {
		s.touchMu.Unlock()
		return
	}
	s.touched[k] = now
	s.touchMu.Unlock()
	s.wmu.Lock()
	s.db.Exec(query, now, arg)
	s.wmu.Unlock()
}

func (s *Store) touchSpace(name string) {
	s.touch("s:"+name, `UPDATE spaces SET last_active_at = ? WHERE name = ?`, name)
}

// ---------- spaces ----------

type spaceInfo struct {
	exists  bool
	seq     int64
	locked  bool
	keyHash string
	floor   int64
}

func (s *Store) spaceInfo(q interface {
	QueryRow(string, ...any) *sql.Row
}, name string) (spaceInfo, error) {
	var si spaceInfo
	var kh sql.NullString
	err := q.QueryRow(`SELECT seq, key_hash, tombstone_floor FROM spaces WHERE name = ?`, name).Scan(&si.seq, &kh, &si.floor)
	if errors.Is(err, sql.ErrNoRows) {
		// A space that doesn't exist acts as if its counter were the current
		// time, but never below a version already handed out for that name.
		si.seq = nowMs()
		var g int64
		if q.QueryRow(`SELECT seq FROM graveyard WHERE name = ?`, name).Scan(&g) == nil && g > si.seq {
			si.seq = g
		}
		return si, nil
	}
	if err != nil {
		return si, err
	}
	si.exists, si.locked, si.keyHash = true, kh.Valid, kh.String
	return si, nil
}

func (s *Store) startSeq(tx *sql.Tx, name string) int64 {
	seq := nowMs()
	var g int64
	if tx.QueryRow(`SELECT seq FROM graveyard WHERE name = ?`, name).Scan(&g) == nil && g > seq {
		seq = g
	}
	return seq
}

func (s *Store) spaceCount(q interface {
	QueryRow(string, ...any) *sql.Row
}) (n int) {
	q.QueryRow(`SELECT count(*) FROM spaces`).Scan(&n)
	return
}

// createSpace makes a new empty space with a generated name. With locked it
// also returns the space key.
func (s *Store) createSpace(label string, locked bool) (name, key string, err error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	if s.spaceCount(tx) >= s.cfg.Limits.MaxSpaces {
		return "", "", errServerFull
	}
	for range 10 {
		name = newSpaceName(label)
		var x int
		if tx.QueryRow(`SELECT 1 FROM spaces WHERE name = ?`, name).Scan(&x) == nil {
			continue
		}
		var kh any
		if locked {
			key = randomToken("sk_")
			kh = hashSecret(key)
		}
		now := nowS()
		if _, err = tx.Exec(`INSERT INTO spaces (name, seq, key_hash, created_at, last_active_at) VALUES (?, ?, ?, ?, ?)`,
			name, s.startSeq(tx, name), kh, now, now); err != nil {
			return "", "", err
		}
		return name, key, tx.Commit()
	}
	return "", "", errors.New("could not generate a free name")
}

// rekey locks a space or replaces its key. Used by admin.
func (s *Store) rekey(name string) (string, error) {
	key := randomToken("sk_")
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.Exec(`UPDATE spaces SET key_hash = ? WHERE name = ?`, hashSecret(key), name)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", fmt.Errorf("no space named %s", name)
	}
	return key, nil
}

// ---------- reads ----------

type Entry struct {
	Key     string
	Value   []byte // nil for a tombstone
	IsJSON  bool
	Version int64
	User    string
}

func (s *Store) get(space, area, key string) (*Entry, error) {
	e := &Entry{Key: key}
	err := s.db.QueryRow(`SELECT value, is_json, version FROM kv WHERE space = ? AND area = ? AND key = ? AND value IS NOT NULL`,
		space, area, key).Scan(&e.Value, &e.IsJSON, &e.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err == nil {
		s.touchSpace(space)
	}
	return e, err
}

// target selects keys in an area. allUsers means users/*/.
type target struct {
	space    string
	area     string // ignored when allUsers
	allUsers bool
	key      string // exact key, or prefix when pattern
	pattern  bool
}

func (t target) where() (string, []any) {
	var b strings.Builder
	args := []any{t.space}
	b.WriteString("space = ?")
	if t.allUsers {
		b.WriteString(" AND area >= 'u:' AND area < 'u;'")
	} else {
		b.WriteString(" AND area = ?")
		args = append(args, t.area)
	}
	if t.pattern {
		if t.key != "" {
			lo, hi := prefixRange(t.key)
			b.WriteString(" AND key >= ? AND key < ?")
			args = append(args, lo, hi)
		}
	} else {
		b.WriteString(" AND key = ?")
		args = append(args, t.key)
	}
	return b.String(), args
}

func scanEntries(rows *sql.Rows, allUsers bool) ([]Entry, error) {
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var area string
		if err := rows.Scan(&area, &e.Key, &e.Value, &e.IsJSON, &e.Version); err != nil {
			return nil, err
		}
		if allUsers {
			e.User = strings.TrimPrefix(area, "u:")
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// list returns live entries in key order, plus a cursor if there are more.
func (s *Store) list(t target, after string, limit int, desc bool) (items []Entry, next string, version int64, err error) {
	si, err := s.spaceInfo(s.db, t.space)
	if err != nil || !si.exists {
		return nil, "", si.seq, err
	}
	s.touchSpace(t.space)
	where, args := t.where()
	where += " AND value IS NOT NULL"
	cmp, ord := ">", "ASC"
	if desc {
		cmp, ord = "<", "DESC"
	}
	if after != "" {
		if ak, au, ok := strings.Cut(after, "/"); ok && t.allUsers {
			where += fmt.Sprintf(" AND (key %s ? OR (key = ? AND area %s ?))", cmp, cmp)
			args = append(args, ak, ak, "u:"+au)
		} else {
			where += fmt.Sprintf(" AND key %s ?", cmp)
			args = append(args, after)
		}
	}
	q := fmt.Sprintf(`SELECT area, key, value, is_json, version FROM kv WHERE %s ORDER BY key %s, area %s LIMIT ?`, where, ord, ord)
	rows, err := s.db.Query(q, append(args, limit+1)...)
	if err != nil {
		return nil, "", 0, err
	}
	items, err = scanEntries(rows, t.allUsers)
	if err != nil {
		return nil, "", 0, err
	}
	if len(items) > limit {
		items = items[:limit]
		last := items[limit-1]
		next = last.Key
		if t.allUsers {
			next += "/" + last.User
		}
	}
	return items, next, si.seq, nil
}

var errVersionExpired = errors.New("version expired")

// changes returns entries changed after version, ordered by version, at most
// max+1 so the caller can tell whether there are more. It also returns the
// space's current counter.
func (s *Store) changes(t target, after int64, max int) ([]Entry, int64, error) {
	si, err := s.spaceInfo(s.db, t.space)
	if err != nil {
		return nil, 0, err
	}
	if after > 0 && (after < si.floor || after > si.seq) {
		return nil, si.seq, errVersionExpired
	}
	if !si.exists {
		return nil, si.seq, nil
	}
	where, args := t.where()
	where += " AND version > ?"
	args = append(args, after)
	if after == 0 {
		where += " AND value IS NOT NULL"
	}
	rows, err := s.db.Query(`SELECT area, key, value, is_json, version FROM kv WHERE `+where+` ORDER BY version LIMIT ?`, append(args, max+1)...)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanEntries(rows, t.allUsers)
	return out, si.seq, err
}

// ---------- writes ----------

type writeReq struct {
	space, area, key string
	value            []byte // nil means delete
	isJSON           bool
	writer           string
	ifVersion        *int64
	spaceKey         string
	onCreate         func() error // called inside the transaction before a new space is created
}

type writeResult struct {
	version int64
	created bool // a new key, for 201
	changed bool // false for an identical rewrite
}

func (s *Store) write(ctx context.Context, r writeReq) (res writeResult, err error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	_, maxBytes, maxKeys := s.cfg.limitsFor(r.space)
	now := nowS()
	del := r.value == nil

	si, err := s.spaceInfo(tx, r.space)
	if err != nil {
		return res, err
	}
	if !si.exists && del {
		return res, errNotFound
	}
	if si.locked && r.area == "s" && (r.spaceKey == "" || hashSecret(r.spaceKey) != si.keyHash) {
		return res, errSpaceLocked
	}

	var old struct {
		value   []byte
		version int64
		writer  string
		size    int64
		row     bool
	}
	err = tx.QueryRow(`SELECT value, version, writer, size FROM kv WHERE space = ? AND area = ? AND key = ?`,
		r.space, r.area, r.key).Scan(&old.value, &old.version, &old.writer, &old.size)
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
	if del && !live {
		return res, errNotFound
	}
	if r.ifVersion != nil && *r.ifVersion != cur {
		return res, &apiError{Status: 412, Code: "version_conflict", Message: "The key changed since that version.", Extra: map[string]any{"currentVersion": cur}}
	}
	if live && !del && bytes.Equal(old.value, r.value) {
		return writeResult{version: cur}, nil
	}

	newSize := int64(len(r.key))
	if !del {
		newSize += int64(len(r.value))
	}
	oldSize := int64(0)
	if old.row {
		oldSize = old.size
	}
	spaceDelta := newSize - oldSize
	keysDelta := 0
	switch {
	case !del && !live:
		keysDelta = 1
	case del:
		keysDelta = -1
	}

	// Quota checks only when growing.
	if !si.exists {
		if r.onCreate != nil {
			if err := r.onCreate(); err != nil {
				return res, err
			}
		}
		if s.spaceCount(tx) >= s.cfg.Limits.MaxSpaces {
			return res, errServerFull
		}
		si.seq = s.startSeq(tx, r.space)
		if _, err := tx.Exec(`INSERT INTO spaces (name, seq, created_at, last_active_at) VALUES (?, ?, ?, ?)`, r.space, si.seq, now, now); err != nil {
			return res, err
		}
	}
	if !del {
		var sb int64
		var sk int
		tx.QueryRow(`SELECT bytes, keys FROM spaces WHERE name = ?`, r.space).Scan(&sb, &sk)
		if keysDelta > 0 && sk+keysDelta > maxKeys {
			return res, quotaErr("space_quota_exceeded", "The space has as many keys as it may hold.", int64(maxKeys), int64(sk))
		}
		if spaceDelta > 0 && sb+spaceDelta > maxBytes {
			return res, quotaErr("space_quota_exceeded", "The space is full.", maxBytes, sb)
		}
		visitorDelta := newSize
		if live && old.writer == r.writer {
			visitorDelta -= oldSize
		}
		if visitorDelta > 0 {
			var vb int64
			tx.QueryRow(`SELECT bytes FROM visitors WHERE id = ?`, r.writer).Scan(&vb)
			if lim := int64(s.cfg.Limits.MaxVisitorBytes); vb+visitorDelta > lim {
				return res, quotaErr("visitor_quota_exceeded", "You have stored as much as one visitor may.", lim, vb)
			}
		}
		if spaceDelta > 0 {
			var total int64
			tx.QueryRow(`SELECT coalesce(sum(bytes), 0) FROM spaces`).Scan(&total)
			if total+spaceDelta > int64(s.cfg.Limits.MaxTotalBytes) {
				return res, errServerFull
			}
		}
	}

	version := si.seq + 1
	writer := r.writer
	if del {
		writer = old.writer
	}
	var val any
	if !del {
		val = r.value
	}
	if _, err := tx.Exec(`INSERT INTO kv (space, area, key, value, is_json, version, writer, size, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (space, area, key) DO UPDATE SET value = excluded.value, is_json = excluded.is_json,
		version = excluded.version, writer = excluded.writer, size = excluded.size, updated_at = excluded.updated_at`,
		r.space, r.area, r.key, val, r.isJSON, version, writer, newSize, now); err != nil {
		return res, err
	}
	if _, err := tx.Exec(`UPDATE spaces SET seq = ?, bytes = bytes + ?, keys = keys + ?, last_active_at = ? WHERE name = ?`,
		version, spaceDelta, keysDelta, now, r.space); err != nil {
		return res, err
	}
	// Visitor usage: the old writer gives up the old bytes, the new writer
	// takes the new ones. Tombstones belong to the space only.
	if live {
		tx.Exec(`UPDATE visitors SET bytes = max(0, bytes - ?) WHERE id = ?`, oldSize, old.writer)
	}
	if !del {
		tx.Exec(`UPDATE visitors SET bytes = bytes + ?, stored = 1 WHERE id = ?`, newSize, r.writer)
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	return writeResult{version: version, created: !live, changed: true}, nil
}

// ---------- admin and cleanup ----------

type spaceRow struct {
	Name       string
	Locked     bool
	Bytes      int64
	Keys       int
	LastActive int64
	Created    int64
}

func (s *Store) spaces() ([]spaceRow, error) {
	rows, err := s.db.Query(`SELECT name, key_hash IS NOT NULL, bytes, keys, last_active_at, created_at FROM spaces ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []spaceRow
	for rows.Next() {
		var r spaceRow
		if err := rows.Scan(&r.Name, &r.Locked, &r.Bytes, &r.Keys, &r.LastActive, &r.Created); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// deleteSpaceTx removes a space and gives its bytes back to the visitors who
// wrote them. It remembers the counter so a recreated space continues above it.
func deleteSpaceTx(tx *sql.Tx, name string) error {
	var seq int64
	if err := tx.QueryRow(`SELECT seq FROM spaces WHERE name = ?`, name).Scan(&seq); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no space named %s", name)
		}
		return err
	}
	if _, err := tx.Exec(`UPDATE visitors SET bytes = max(0, bytes - (SELECT coalesce(sum(size), 0) FROM kv
		WHERE kv.space = ? AND kv.writer = visitors.id AND kv.value IS NOT NULL))
		WHERE id IN (SELECT DISTINCT writer FROM kv WHERE space = ?)`, name, name); err != nil {
		return err
	}
	stmts := []struct {
		q    string
		args []any
	}{
		{`DELETE FROM kv WHERE space = ?`, []any{name}},
		{`DELETE FROM spaces WHERE name = ?`, []any{name}},
		{`INSERT INTO graveyard (name, seq) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET seq = max(seq, excluded.seq)`, []any{name, seq}},
	}
	for _, st := range stmts {
		if _, err := tx.Exec(st.q, st.args...); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) deleteSpace(name string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteSpaceTx(tx, name); err != nil {
		return err
	}
	return tx.Commit()
}

type cleanupReport struct {
	Visitors, Tombstones, Spaces int
}

func (s *Store) cleanup() (rep cleanupReport, err error) {
	now := nowS()
	r := s.cfg.Retention
	s.wmu.Lock()
	defer s.wmu.Unlock()

	// 1. Visitors past their idle time, with their users/ and me/ data.
	rows, err := s.db.Query(`SELECT id FROM visitors WHERE last_seen_at < ? OR (stored = 0 AND last_seen_at < ?)`,
		now-int64(time.Duration(r.VisitorIdleTTL).Seconds()), now-int64(time.Duration(r.EmptyVisitorIdleTTL).Seconds()))
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
		tx, err := s.db.Begin()
		if err != nil {
			return rep, err
		}
		areas := []any{"u:" + id, "m:" + id}
		tx.Exec(`UPDATE spaces SET
			bytes = bytes - (SELECT coalesce(sum(size), 0) FROM kv WHERE kv.space = spaces.name AND kv.area IN (?, ?)),
			keys = keys - (SELECT count(*) FROM kv WHERE kv.space = spaces.name AND kv.area IN (?, ?) AND kv.value IS NOT NULL)
			WHERE name IN (SELECT DISTINCT space FROM kv WHERE area IN (?, ?))`, append(append(areas, areas...), areas...)...)
		tx.Exec(`DELETE FROM kv WHERE area IN (?, ?)`, areas...)
		tx.Exec(`DELETE FROM visitors WHERE id = ?`, id)
		if err := tx.Commit(); err != nil {
			tx.Rollback()
			return rep, err
		}
		rep.Visitors++
	}

	// 2. Old tombstones. The space remembers the highest removed version.
	cut := now - int64(time.Duration(r.TombstoneTTL).Seconds())
	tx, err := s.db.Begin()
	if err != nil {
		return rep, err
	}
	tx.Exec(`UPDATE spaces SET
		tombstone_floor = max(tombstone_floor, (SELECT coalesce(max(version), 0) FROM kv WHERE kv.space = spaces.name AND value IS NULL AND updated_at < ?)),
		bytes = bytes - (SELECT coalesce(sum(size), 0) FROM kv WHERE kv.space = spaces.name AND value IS NULL AND updated_at < ?)
		WHERE name IN (SELECT DISTINCT space FROM kv WHERE value IS NULL AND updated_at < ?)`, cut, cut, cut)
	res, _ := tx.Exec(`DELETE FROM kv WHERE value IS NULL AND updated_at < ?`, cut)
	if n, err := res.RowsAffected(); err == nil {
		rep.Tombstones = int(n)
	}
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		return rep, err
	}

	// 3. Idle spaces, unless pinned.
	idle := now - int64(time.Duration(r.SpaceIdleTTL).Seconds())
	rows, err = s.db.Query(`SELECT name FROM spaces WHERE last_active_at < ?`, idle)
	if err != nil {
		return rep, err
	}
	var names []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		if !s.cfg.pinned(n) {
			names = append(names, n)
		}
	}
	rows.Close()
	for _, n := range names {
		tx, err := s.db.Begin()
		if err != nil {
			return rep, err
		}
		if err := deleteSpaceTx(tx, n); err != nil {
			tx.Rollback()
			return rep, err
		}
		if err := tx.Commit(); err != nil {
			return rep, err
		}
		rep.Spaces++
	}

	s.db.Exec(`PRAGMA incremental_vacuum`)
	s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)

	s.touchMu.Lock()
	for k, t := range s.touched {
		if now-t > 3600 {
			delete(s.touched, k)
		}
	}
	s.touchMu.Unlock()
	return rep, nil
}
