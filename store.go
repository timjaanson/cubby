package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Data layout:
//
//	{data}/server.db           visitors, which spaces each visitor wrote to, deleted-space counters
//	{data}/spaces/{name}.db    one SQLite file per space, self-contained
//	{data}/admin.sock          Unix socket for `tiny-kv admin`
//
// A space file holds everything about the space, including its counter and
// lock key, so it can be copied to another server or deleted on its own.

const serverSchema = `
CREATE TABLE IF NOT EXISTS visitors (
  id           TEXT PRIMARY KEY,
  token_hash   TEXT NOT NULL UNIQUE,
  created_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  stored       INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS visitor_spaces (
  visitor TEXT NOT NULL,
  space   TEXT NOT NULL,
  PRIMARY KEY (visitor, space)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS visitor_spaces_by_space ON visitor_spaces (space);
CREATE TABLE IF NOT EXISTS graveyard (
  name TEXT PRIMARY KEY,
  seq  INTEGER NOT NULL
);
`

const spaceSchema = `
CREATE TABLE IF NOT EXISTS meta (
  k TEXT PRIMARY KEY,   -- seq, created_at, last_active_at, tombstone_floor, bytes, keys, key_hash
  v
);
CREATE TABLE IF NOT EXISTS kv (
  area       TEXT NOT NULL,      -- 's', 'u:{id}', 'm:{id}'
  key        TEXT NOT NULL,
  value      BLOB,               -- NULL is a tombstone
  is_json    INTEGER NOT NULL,
  version    INTEGER NOT NULL,
  writer     TEXT NOT NULL,
  size       INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  series     INTEGER NOT NULL DEFAULT 0, -- 1 for a series key
  t          INTEGER,                    -- newest point's time in ms, series only
  n          INTEGER NOT NULL DEFAULT 0, -- number of points, series only
  PRIMARY KEY (area, key)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS kv_by_version ON kv (area, version);
CREATE INDEX IF NOT EXISTS kv_users ON kv (key, area) WHERE area >= 'u:' AND area < 'u;';
CREATE INDEX IF NOT EXISTS kv_tombstones ON kv (updated_at) WHERE value IS NULL;
CREATE TABLE IF NOT EXISTS usage (writer TEXT PRIMARY KEY, bytes INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS points (
  area    TEXT NOT NULL,
  key     TEXT NOT NULL,
  t       INTEGER NOT NULL,
  value   BLOB NOT NULL,
  is_json INTEGER NOT NULL,
  PRIMARY KEY (area, key, t)
) WITHOUT ROWID;
`

// migrateSpace brings a space file from before series up to date. Files
// created since have everything already, and running it twice is harmless.
func migrateSpace(db *sql.DB) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('kv')`)
	if err != nil {
		return err
	}
	has := map[string]bool{}
	for rows.Next() {
		var n string
		rows.Scan(&n)
		has[n] = true
	}
	rows.Close()
	for _, col := range []string{"series INTEGER NOT NULL DEFAULT 0", "t INTEGER", "n INTEGER NOT NULL DEFAULT 0"} {
		name, _, _ := strings.Cut(col, " ")
		if !has[name] {
			if _, err := db.Exec(`ALTER TABLE kv ADD COLUMN ` + col); err != nil {
				return err
			}
		}
	}
	_, err = db.Exec(spaceSchema)
	return err
}

const maxOpenSpaces = 64

type spaceDB struct {
	name string
	db   *sql.DB
	gate sync.RWMutex // held shared while in use, exclusively to close or delete
	wmu  sync.Mutex   // serializes writes to this file
	dead bool         // closed; whoever holds a stale pointer must look it up again

	lastUsed   time.Time // guarded by Store.mu
	metaActive int64     // last_active_at as written to the file, guarded by wmu
}

type spaceStat struct {
	bytes      int64
	keys       int
	lastActive int64
	locked     bool
}

type Store struct {
	cfg *Config
	dir string
	srv *sql.DB
	// onChange is called after the server itself changes a space outside a
	// client write (cleanup, admin), so watchers wake up.
	onChange func(space string)

	mu    sync.Mutex
	open  map[string]*spaceDB
	stats map[string]*spaceStat
	pins  map[string]int // spaces with waiting watches stay open
	total int64

	cacheMu  sync.Mutex
	vbytes   map[string]int64 // visitor ID -> bytes across spaces, recounted on a miss
	vspaces  map[string]bool  // "id space" pairs already in visitor_spaces
	vtouched map[string]int64 // visitor ID -> last_seen_at written
}

func openDB(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=journal_size_limit(4194304)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetConnMaxIdleTime(time.Minute)
	return db, nil
}

func openStore(cfg *Config) (*Store, error) {
	dir := cfg.Server.DataDir
	if err := os.MkdirAll(filepath.Join(dir, "spaces"), 0o700); err != nil {
		return nil, fmt.Errorf("data directory: %w", err)
	}
	srv, err := openDB(filepath.Join(dir, "server.db"))
	if err != nil {
		return nil, err
	}
	if _, err := srv.Exec(serverSchema); err != nil {
		srv.Close()
		return nil, fmt.Errorf("open %s: %w", filepath.Join(dir, "server.db"), err)
	}
	s := &Store{
		cfg: cfg, dir: dir, srv: srv, onChange: func(string) {},
		open: map[string]*spaceDB{}, stats: map[string]*spaceStat{}, pins: map[string]int{},
		vbytes: map[string]int64{}, vspaces: map[string]bool{}, vtouched: map[string]int64{},
	}
	if err := s.rescan(); err != nil {
		srv.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	open := s.open
	s.open = map[string]*spaceDB{}
	s.mu.Unlock()
	for _, sp := range open {
		sp.gate.Lock()
		sp.dead = true
		sp.db.Close()
		sp.gate.Unlock()
	}
	return s.srv.Close()
}

// touch records activity on a space without reading it.
func (s *Store) touch(name string) {
	if sp, _, err := s.acquire(name, false, ""); err == nil && sp != nil {
		s.touchSpace(sp)
		s.release(sp)
	}
}

func nowMs() int64 { return time.Now().UnixMilli() }
func nowS() int64  { return time.Now().Unix() }

// spacePath maps a space to its file. Names are checked here as well as in
// the router, so no caller can ever produce a path outside spaces/.
func (s *Store) spacePath(name string) string {
	if !validSpace(name) {
		panic("tiny-kv: unchecked space name " + strconv.Quote(name))
	}
	return filepath.Join(s.dir, "spaces", name+".db")
}

// ---------- space files ----------

type spaceMeta struct {
	seq, createdAt, lastActive, floor, bytes int64
	keys                                     int
	keyHash                                  string
}

type queryer interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

func readMeta(q queryer) (spaceMeta, error) {
	var m spaceMeta
	rows, err := q.Query(`SELECT k, v FROM meta`)
	if err != nil {
		return m, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var v any
		if err := rows.Scan(&k, &v); err != nil {
			return m, err
		}
		n := toInt(v)
		switch k {
		case "seq":
			m.seq = n
		case "created_at":
			m.createdAt = n
		case "last_active_at":
			m.lastActive = n
		case "tombstone_floor":
			m.floor = n
		case "bytes":
			m.bytes = n
		case "keys":
			m.keys = int(n)
		case "key_hash":
			if str, ok := v.(string); ok {
				m.keyHash = str
			}
		}
	}
	return m, rows.Err()
}

func toInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	case []byte:
		n, _ := strconv.ParseInt(string(x), 10, 64)
		return n
	}
	return 0
}

type execer interface {
	Exec(string, ...any) (sql.Result, error)
}

func setMeta(e execer, k string, v any) error {
	_, err := e.Exec(`INSERT INTO meta (k, v) VALUES (?, ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v`, k, v)
	return err
}

// graveSeq is the last counter of a deleted space with this name, or 0.
func (s *Store) graveSeq(name string) int64 {
	var g int64
	s.srv.QueryRow(`SELECT seq FROM graveyard WHERE name = ?`, name).Scan(&g)
	return g
}

// phantomSeq is the counter a space that doesn't exist pretends to have: the
// current time, but always above anything already handed out for the name.
func (s *Store) phantomSeq(name string) int64 {
	return max(nowMs(), s.graveSeq(name)+1)
}

// initSpace creates the tables of a new space file.
func (s *Store) initSpace(db *sql.DB, name, keyHash string) error {
	if _, err := db.Exec(`PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
		return err
	}
	if _, err := db.Exec(spaceSchema); err != nil {
		return err
	}
	g := s.graveSeq(name)
	now := nowS()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range map[string]any{
		"seq": max(nowMs(), g+1), "created_at": now, "last_active_at": now,
		// Watchers from a deleted space of the same name must start over.
		"tombstone_floor": g + 1, "bytes": 0, "keys": 0,
	} {
		if err := setMeta(tx, k, v); err != nil {
			return err
		}
	}
	if keyHash != "" {
		if err := setMeta(tx, "key_hash", keyHash); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// acquire returns an open space with its gate held shared. It returns nil if
// the space doesn't exist and create is false. Call release when done.
func (s *Store) acquire(name string, create bool, keyHash string) (sp *spaceDB, created bool, err error) {
	if !validSpace(name) {
		return nil, false, &apiError{Status: 400, Code: "invalid_space", Message: "Not a valid space name."}
	}
	for {
		s.mu.Lock()
		sp = s.open[name]
		if sp == nil {
			path := s.spacePath(name)
			_, statErr := os.Stat(path)
			exists := statErr == nil
			if !exists && !create {
				s.mu.Unlock()
				return nil, false, nil
			}
			db, err := openDB(path)
			if err != nil {
				s.mu.Unlock()
				return nil, false, err
			}
			if !exists {
				if err := s.initSpace(db, name, keyHash); err != nil {
					db.Close()
					os.Remove(path)
					s.mu.Unlock()
					return nil, false, fmt.Errorf("create space %s: %w", name, err)
				}
				created = true
			} else if err := migrateSpace(db); err != nil {
				db.Close()
				s.mu.Unlock()
				return nil, false, fmt.Errorf("open space %s: %w", name, err)
			}
			m, err := readMeta(db)
			if err != nil {
				db.Close()
				s.mu.Unlock()
				return nil, false, fmt.Errorf("open space %s: %w", name, err)
			}
			sp = &spaceDB{name: name, db: db, metaActive: m.lastActive, lastUsed: time.Now()}
			s.open[name] = sp
			if s.stats[name] == nil {
				s.stats[name] = &spaceStat{bytes: m.bytes, keys: m.keys, lastActive: m.lastActive, locked: m.keyHash != ""}
				s.total += m.bytes
			}
			s.evictLocked(name)
		}
		sp.lastUsed = time.Now()
		s.mu.Unlock()

		sp.gate.RLock()
		if sp.dead {
			sp.gate.RUnlock()
			continue
		}
		return sp, created, nil
	}
}

func (s *Store) release(sp *spaceDB) { sp.gate.RUnlock() }

// evictLocked closes the least recently used idle files beyond the limit.
func (s *Store) evictLocked(keep string) {
	if len(s.open) <= maxOpenSpaces {
		return
	}
	var idle []*spaceDB
	for n, sp := range s.open {
		if s.pins[n] == 0 && n != keep {
			idle = append(idle, sp)
		}
	}
	sort.Slice(idle, func(i, j int) bool { return idle[i].lastUsed.Before(idle[j].lastUsed) })
	for _, sp := range idle {
		if len(s.open) <= maxOpenSpaces {
			return
		}
		if sp.gate.TryLock() {
			sp.dead = true
			sp.db.Close()
			delete(s.open, sp.name)
			sp.gate.Unlock()
		}
	}
}

func (s *Store) pin(name string) {
	s.mu.Lock()
	s.pins[name]++
	s.mu.Unlock()
}

func (s *Store) unpin(name string) {
	s.mu.Lock()
	if s.pins[name]--; s.pins[name] <= 0 {
		delete(s.pins, name)
	}
	s.mu.Unlock()
}

func (s *Store) spaceExists(name string) bool {
	s.mu.Lock()
	_, ok := s.stats[name]
	s.mu.Unlock()
	if ok {
		return true
	}
	_, err := os.Stat(s.spacePath(name))
	return err == nil
}

func (s *Store) spaceCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.stats)
}

// rescan syncs the in-memory totals with the files in spaces/. Files copied
// in are picked up, files removed by hand are forgotten.
func (s *Store) rescan() error {
	entries, err := os.ReadDir(filepath.Join(s.dir, "spaces"))
	if err != nil {
		return err
	}
	onDisk := map[string]bool{}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".db")
		if !ok || e.IsDir() {
			continue
		}
		if !validSpace(name) {
			log.Printf("ignoring spaces/%s: not a valid space name", e.Name())
			continue
		}
		onDisk[name] = true
		s.mu.Lock()
		known := s.stats[name] != nil
		s.mu.Unlock()
		if known {
			continue
		}
		db, err := openDB(s.spacePath(name))
		if err != nil {
			return err
		}
		m, err := readMeta(db)
		db.Close()
		if err != nil {
			log.Printf("ignoring spaces/%s: %v", e.Name(), err)
			continue
		}
		s.mu.Lock()
		if s.stats[name] == nil {
			s.stats[name] = &spaceStat{bytes: m.bytes, keys: m.keys, lastActive: m.lastActive, locked: m.keyHash != ""}
			s.total += m.bytes
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	var gone []string
	for name := range s.stats {
		if !onDisk[name] {
			gone = append(gone, name)
		}
	}
	s.mu.Unlock()
	for _, name := range gone {
		s.forget(name)
	}
	return nil
}

// forget drops a space from memory after its file disappeared.
func (s *Store) forget(name string) {
	s.mu.Lock()
	sp := s.open[name]
	if st := s.stats[name]; st != nil {
		s.total -= st.bytes
		delete(s.stats, name)
	}
	delete(s.open, name)
	s.mu.Unlock()
	if sp != nil {
		sp.gate.Lock()
		sp.dead = true
		sp.db.Close()
		sp.gate.Unlock()
	}
	s.cacheMu.Lock()
	s.vbytes = map[string]int64{}
	s.cacheMu.Unlock()
	s.onChange(name)
}

// touchSpace records activity, writing it to the file at most once an hour.
func (s *Store) touchSpace(sp *spaceDB) {
	now := nowS()
	s.mu.Lock()
	if st := s.stats[sp.name]; st != nil {
		st.lastActive = now
	}
	s.mu.Unlock()
	sp.wmu.Lock()
	defer sp.wmu.Unlock()
	if now-sp.metaActive >= 3600 {
		if setMeta(sp.db, "last_active_at", now) == nil {
			sp.metaActive = now
		}
	}
}

// ---------- visitors ----------

func (s *Store) newVisitor() (token, id string, err error) {
	token, id = randomToken("kv_"), newVisitorID()
	now := nowS()
	_, err = s.srv.Exec(`INSERT INTO visitors (id, token_hash, created_at, last_seen_at) VALUES (?, ?, ?, ?)`,
		id, hashSecret(token), now, now)
	return
}

// visitorByToken returns "" for an unknown token.
func (s *Store) visitorByToken(token string) (string, error) {
	var id string
	err := s.srv.QueryRow(`SELECT id FROM visitors WHERE token_hash = ?`, hashSecret(token)).Scan(&id)
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
func (s *Store) visitorTotal(id string) (int64, error) {
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
		sp, _, err := s.acquire(name, false, "")
		if err != nil {
			return 0, err
		}
		if sp == nil {
			continue
		}
		var b int64
		sp.db.QueryRow(`SELECT bytes FROM usage WHERE writer = ?`, id).Scan(&b)
		s.release(sp)
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

// ---------- spaces ----------

// createSpace makes a new empty space with a generated name. With locked it
// also returns the space key.
func (s *Store) createSpace(label string, locked bool) (name, key string, err error) {
	if s.spaceCount() >= s.cfg.Limits.MaxSpaces {
		return "", "", errServerFull
	}
	for range 10 {
		name = newSpaceName(label)
		if s.spaceExists(name) {
			continue
		}
		kh := ""
		if locked {
			key = randomToken("sk_")
			kh = hashSecret(key)
		}
		sp, created, err := s.acquire(name, true, kh)
		if err != nil {
			return "", "", err
		}
		s.release(sp)
		if created {
			return name, key, nil
		}
	}
	return "", "", errors.New("could not generate a free name")
}

var errNoSpace = &apiError{Status: 404, Code: "not_found", Message: "No such space."}

// rekey locks a space or replaces its key.
func (s *Store) rekey(name string) (string, error) {
	sp, _, err := s.acquire(name, false, "")
	if err != nil {
		return "", err
	}
	if sp == nil {
		return "", errNoSpace
	}
	defer s.release(sp)
	key := randomToken("sk_")
	sp.wmu.Lock()
	err = setMeta(sp.db, "key_hash", hashSecret(key))
	sp.wmu.Unlock()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	if st := s.stats[name]; st != nil {
		st.locked = true
	}
	s.mu.Unlock()
	return key, nil
}

// deleteSpace removes a space's file. Its counter is remembered, so a space
// created later under the same name continues above it.
func (s *Store) deleteSpace(name string) error {
	for {
		sp, _, err := s.acquire(name, false, "")
		if err != nil {
			return err
		}
		if sp == nil {
			return errNoSpace
		}
		s.release(sp)
		sp.gate.Lock()
		if sp.dead {
			sp.gate.Unlock()
			continue
		}
		m, _ := readMeta(sp.db)
		sp.dead = true
		sp.db.Close()
		s.mu.Lock()
		delete(s.open, name)
		if st := s.stats[name]; st != nil {
			s.total -= st.bytes
			delete(s.stats, name)
		}
		s.mu.Unlock()
		path := s.spacePath(name)
		for _, suf := range []string{"", "-wal", "-shm"} {
			os.Remove(path + suf)
		}
		sp.gate.Unlock()

		s.srv.Exec(`INSERT INTO graveyard (name, seq) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET seq = max(seq, excluded.seq)`, name, m.seq)
		s.srv.Exec(`DELETE FROM visitor_spaces WHERE space = ?`, name)
		s.cacheMu.Lock()
		s.vbytes = map[string]int64{}
		for k := range s.vspaces {
			if strings.HasSuffix(k, " "+name) {
				delete(s.vspaces, k)
			}
		}
		s.cacheMu.Unlock()
		s.onChange(name)
		return nil
	}
}

type spaceRow struct {
	Name       string `json:"name"`
	Locked     bool   `json:"locked"`
	Pinned     bool   `json:"pinned"`
	Bytes      int64  `json:"bytes"`
	Keys       int    `json:"keys"`
	LastActive int64  `json:"lastActive"`
}

func (s *Store) spaces() []spaceRow {
	s.rescan()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]spaceRow, 0, len(s.stats))
	for n, st := range s.stats {
		out = append(out, spaceRow{Name: n, Locked: st.locked, Pinned: s.cfg.pinned(n), Bytes: st.bytes, Keys: st.keys, LastActive: st.lastActive})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ---------- reads ----------

type Entry struct {
	Key     string
	Value   []byte // nil for a tombstone
	IsJSON  bool
	Version int64
	User    string
	Writer  string
	Area    string
	Series  bool
	T       int64 // newest point's time, series only
}

func (s *Store) get(space, area, key string) (*Entry, error) {
	sp, _, err := s.acquire(space, false, "")
	if err != nil || sp == nil {
		return nil, err
	}
	defer s.release(sp)
	e := &Entry{Key: key}
	var t sql.NullInt64
	err = sp.db.QueryRow(`SELECT value, is_json, version, series, t FROM kv WHERE area = ? AND key = ? AND value IS NOT NULL`,
		area, key).Scan(&e.Value, &e.IsJSON, &e.Version, &e.Series, &t)
	e.T = t.Int64
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.touchSpace(sp)
	return e, nil
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
	var args []any
	if t.allUsers {
		b.WriteString("area >= 'u:' AND area < 'u;'")
	} else {
		b.WriteString("area = ?")
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
		var t sql.NullInt64
		if err := rows.Scan(&e.Area, &e.Key, &e.Value, &e.IsJSON, &e.Version, &e.Series, &t); err != nil {
			return nil, err
		}
		e.T = t.Int64
		if allUsers {
			e.User = strings.TrimPrefix(e.Area, "u:")
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// currentSeq is the space's counter, or the phantom counter if it doesn't exist.
func (s *Store) currentSeq(space string) (int64, error) {
	sp, _, err := s.acquire(space, false, "")
	if err != nil {
		return 0, err
	}
	if sp == nil {
		return s.phantomSeq(space), nil
	}
	defer s.release(sp)
	m, err := readMeta(sp.db)
	return m.seq, err
}

// list returns live entries in key order, plus a cursor if there are more.
func (s *Store) list(t target, after string, limit int, desc bool) (items []Entry, next string, version int64, err error) {
	sp, _, err := s.acquire(t.space, false, "")
	if err != nil {
		return nil, "", 0, err
	}
	if sp == nil {
		return nil, "", s.phantomSeq(t.space), nil
	}
	defer s.release(sp)
	s.touchSpace(sp)
	m, err := readMeta(sp.db)
	if err != nil {
		return nil, "", 0, err
	}
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
	q := fmt.Sprintf(`SELECT area, key, value, is_json, version, series, t FROM kv WHERE %s ORDER BY key %s, area %s LIMIT ?`, where, ord, ord)
	rows, err := sp.db.Query(q, append(args, limit+1)...)
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
	return items, next, m.seq, nil
}

var errVersionExpired = errors.New("version expired")

// changes returns entries changed after version, ordered by version, at most
// max+1 so the caller can tell whether there are more. It also returns the
// space's current counter.
func (s *Store) changes(t target, after int64, limit int) ([]Entry, int64, error) {
	sp, _, err := s.acquire(t.space, false, "")
	if err != nil {
		return nil, 0, err
	}
	if sp == nil {
		g := s.graveSeq(t.space)
		seq := max(nowMs(), g+1)
		// A version from before the space was deleted, or from the future.
		if after > 0 && ((g > 0 && after <= g) || after > seq) {
			return nil, seq, errVersionExpired
		}
		return nil, seq, nil
	}
	defer s.release(sp)
	m, err := readMeta(sp.db)
	if err != nil {
		return nil, 0, err
	}
	if after > 0 && (after < m.floor || after > m.seq) {
		return nil, m.seq, errVersionExpired
	}
	where, args := t.where()
	where += " AND version > ?"
	args = append(args, after)
	if after == 0 {
		where += " AND value IS NOT NULL"
	}
	rows, err := sp.db.Query(`SELECT area, key, value, is_json, version, series, t FROM kv WHERE `+where+` ORDER BY version LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanEntries(rows, t.allUsers)
	return out, m.seq, err
}

// Point is one entry of a series.
type Point struct {
	Area, Key string
	T         int64
	Value     []byte
	IsJSON    bool
}

// export returns every live entry with its area and writer, then every
// series point.
func (s *Store) export(space string, fn func(Entry) error, pfn func(Point) error) error {
	sp, _, err := s.acquire(space, false, "")
	if err != nil {
		return err
	}
	if sp == nil {
		return errNoSpace
	}
	defer s.release(sp)
	rows, err := sp.db.Query(`SELECT area, key, value, is_json, version, writer, series, t FROM kv WHERE value IS NOT NULL ORDER BY area, key`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var e Entry
		var t sql.NullInt64
		if err := rows.Scan(&e.Area, &e.Key, &e.Value, &e.IsJSON, &e.Version, &e.Writer, &e.Series, &t); err != nil {
			return err
		}
		e.T = t.Int64
		if err := fn(e); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	prows, err := sp.db.Query(`SELECT area, key, t, value, is_json FROM points ORDER BY area, key, t`)
	if err != nil {
		return err
	}
	defer prows.Close()
	for prows.Next() {
		var p Point
		if err := prows.Scan(&p.Area, &p.Key, &p.T, &p.Value, &p.IsJSON); err != nil {
			return err
		}
		if err := pfn(p); err != nil {
			return err
		}
	}
	return prows.Err()
}

// ---------- writes ----------

type writeReq struct {
	space, area, key string
	value            []byte // nil means delete
	isJSON           bool
	writer           string
	ifVersion        *int64
	spaceKey         string
	onCreate         func() error // called before a write creates the space
}

type writeResult struct {
	version int64
	created bool // a new key, for 201
	changed bool // false for an identical rewrite
}

func (s *Store) write(ctx context.Context, r writeReq) (res writeResult, err error) {
	del := r.value == nil
	// Counted before taking the space, since counting may open other spaces.
	var vTotal int64
	if !del {
		if vTotal, err = s.visitorTotal(r.writer); err != nil {
			return res, err
		}
	}

	sp, _, err := s.acquire(r.space, false, "")
	if err != nil {
		return res, err
	}
	if sp == nil {
		if del {
			return res, errNotFound
		}
		if s.spaceCount() >= s.cfg.Limits.MaxSpaces {
			return res, errServerFull
		}
		if r.onCreate != nil {
			if err := r.onCreate(); err != nil {
				return res, err
			}
		}
		if sp, _, err = s.acquire(r.space, true, ""); err != nil {
			return res, err
		}
	}
	defer s.release(sp)

	_, maxBytes, maxKeys := s.cfg.limitsFor(r.space)
	now := nowS()

	sp.wmu.Lock()
	defer sp.wmu.Unlock()
	tx, err := sp.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	m, err := readMeta(tx)
	if err != nil {
		return res, err
	}
	if m.keyHash != "" && r.area == "s" && (r.spaceKey == "" || subtle.ConstantTimeCompare([]byte(hashSecret(r.spaceKey)), []byte(m.keyHash)) != 1) {
		return res, errSpaceLocked
	}

	var old struct {
		value   []byte
		version int64
		writer  string
		size    int64
		row     bool
		series  bool
	}
	err = tx.QueryRow(`SELECT value, version, writer, size, series FROM kv WHERE area = ? AND key = ?`,
		r.area, r.key).Scan(&old.value, &old.version, &old.writer, &old.size, &old.series)
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
	if live && old.series && !del {
		return res, errIsSeries(r.key)
	}
	// Deleting a series deletes its points.
	var pointBytes int64
	if del && old.series {
		if err := tx.QueryRow(`SELECT coalesce(sum(length(key) + length(value)), 0) FROM points WHERE area = ? AND key = ?`,
			r.area, r.key).Scan(&pointBytes); err != nil {
			return res, err
		}
		if _, err := tx.Exec(`DELETE FROM points WHERE area = ? AND key = ?`, r.area, r.key); err != nil {
			return res, err
		}
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
			return res, quotaErr("space_quota_exceeded", "The space has as many keys as it may hold.", int64(maxKeys), int64(m.keys))
		}
		if spaceDelta > 0 && m.bytes+spaceDelta > maxBytes {
			return res, quotaErr("space_quota_exceeded", "The space is full.", maxBytes, m.bytes)
		}
		visitorDelta := newSize
		if live && old.writer == r.writer {
			visitorDelta -= oldSize
		}
		if lim := int64(s.cfg.Limits.MaxVisitorBytes); visitorDelta > 0 && vTotal+visitorDelta > lim {
			return res, quotaErr("visitor_quota_exceeded", "You have stored as much as one visitor may.", lim, vTotal)
		}
		s.mu.Lock()
		total := s.total
		s.mu.Unlock()
		if spaceDelta > 0 && total+spaceDelta > int64(s.cfg.Limits.MaxTotalBytes) {
			return res, errServerFull
		}
	}

	version := m.seq + 1
	writer := r.writer
	if del {
		writer = old.writer
	}
	var val any
	if !del {
		val = r.value
	}
	if _, err := tx.Exec(`INSERT INTO kv (area, key, value, is_json, version, writer, size, updated_at, series, t, n)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, NULL, 0)
		ON CONFLICT (area, key) DO UPDATE SET value = excluded.value, is_json = excluded.is_json,
		version = excluded.version, writer = excluded.writer, size = excluded.size, updated_at = excluded.updated_at,
		series = 0, t = NULL, n = 0`,
		r.area, r.key, val, r.isJSON, version, writer, newSize, now); err != nil {
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
		if _, err := tx.Exec(`INSERT INTO usage (writer, bytes) VALUES (?, ?) ON CONFLICT (writer) DO UPDATE SET bytes = bytes + excluded.bytes`, r.writer, newSize); err != nil {
			return res, err
		}
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	sp.metaActive = now

	s.mu.Lock()
	if st := s.stats[r.space]; st != nil {
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
		s.adjustVisitor(r.writer, newSize)
		s.noteVisitorSpace(r.writer, r.space)
	}
	return writeResult{version: version, created: !live, changed: true}, nil
}

// ---------- cleanup ----------

type cleanupReport struct {
	Visitors   int `json:"visitors"`
	Tombstones int `json:"tombstones"`
	Spaces     int `json:"spaces"`
}

// dropVisitorRows removes a visitor's data from one space. Their users/ keys
// become tombstones so watchers see them go; me/ rows just disappear.
func (s *Store) dropVisitorRows(space, id string) error {
	sp, _, err := s.acquire(space, false, "")
	if err != nil || sp == nil {
		return err
	}
	defer s.release(sp)
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
	rows, err := tx.Query(`SELECT area, key, size, value IS NOT NULL FROM kv WHERE area IN (?, ?)`, "u:"+id, "m:"+id)
	if err != nil {
		return err
	}
	type row struct {
		area, key string
		size      int64
		live      bool
	}
	var rs []row
	for rows.Next() {
		var r row
		rows.Scan(&r.area, &r.key, &r.size, &r.live)
		rs = append(rs, r)
	}
	rows.Close()
	now := nowS()
	seq, bytesD, keysD, changed := m.seq, int64(0), 0, false
	for _, r := range rs {
		if r.live {
			keysD--
		}
		if strings.HasPrefix(r.area, "u:") && r.live {
			seq++
			bytesD += int64(len(r.key)) - r.size
			tx.Exec(`UPDATE kv SET value = NULL, version = ?, size = ?, updated_at = ?, series = 0, t = NULL, n = 0 WHERE area = ? AND key = ?`, seq, len(r.key), now, r.area, r.key)
			changed = true
		} else {
			bytesD -= r.size
			tx.Exec(`DELETE FROM kv WHERE area = ? AND key = ?`, r.area, r.key)
		}
	}
	var pointBytes int64
	tx.QueryRow(`SELECT coalesce(sum(length(key) + length(value)), 0) FROM points WHERE area IN (?, ?)`, "u:"+id, "m:"+id).Scan(&pointBytes)
	tx.Exec(`DELETE FROM points WHERE area IN (?, ?)`, "u:"+id, "m:"+id)
	bytesD -= pointBytes
	tx.Exec(`DELETE FROM usage WHERE writer = ?`, id)
	for k, v := range map[string]any{"seq": seq, "bytes": m.bytes + bytesD, "keys": m.keys + keysD} {
		setMeta(tx, k, v)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	if st := s.stats[space]; st != nil {
		st.bytes += bytesD
		st.keys += keysD
	}
	s.total += bytesD
	s.mu.Unlock()
	if changed {
		s.onChange(space)
	}
	return nil
}

func (s *Store) cleanup() (rep cleanupReport, err error) {
	now := nowS()
	r := s.cfg.Retention
	secs := func(d Duration) int64 { return int64(time.Duration(d).Seconds()) }
	s.rescan()

	// 1. Visitors past their idle time, with their users/ and me/ data.
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
		srows, err := s.srv.Query(`SELECT space FROM visitor_spaces WHERE visitor = ?`, id)
		if err != nil {
			return rep, err
		}
		var spaces []string
		for srows.Next() {
			var n string
			srows.Scan(&n)
			spaces = append(spaces, n)
		}
		srows.Close()
		for _, sp := range spaces {
			if err := s.dropVisitorRows(sp, id); err != nil {
				return rep, err
			}
		}
		s.srv.Exec(`DELETE FROM visitor_spaces WHERE visitor = ?`, id)
		s.srv.Exec(`DELETE FROM visitors WHERE id = ?`, id)
		s.cacheMu.Lock()
		delete(s.vbytes, id)
		delete(s.vtouched, id)
		s.cacheMu.Unlock()
		rep.Visitors++
	}

	// 2. Idle spaces, unless pinned.
	idle := now - secs(r.SpaceIdleTTL)
	var names, all []string
	s.mu.Lock()
	for n, st := range s.stats {
		all = append(all, n)
		if st.lastActive < idle && !s.cfg.pinned(n) {
			names = append(names, n)
		}
	}
	s.mu.Unlock()
	for _, n := range names {
		if err := s.deleteSpace(n); err == nil {
			rep.Spaces++
		}
	}

	// 3. Old tombstones, then compact. The space remembers the highest
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
	sp, _, err := s.acquire(name, false, "")
	if err != nil || sp == nil {
		return 0, err
	}
	defer s.release(sp)
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
