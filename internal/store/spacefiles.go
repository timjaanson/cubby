package store

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/names"
)

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
	return max(NowMs(), s.graveSeq(name)+1)
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
		"seq": max(NowMs(), g+1), "created_at": now, "last_active_at": now,
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
func (s *Store) Acquire(name string, create bool, keyHash string) (sp *spaceDB, created bool, err error) {
	if !names.SafeSpaceFile(name) {
		return nil, false, &apierr.Error{Status: 400, Code: "invalid_space", Message: "Not a usable space name."}
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

func (s *Store) Release(sp *spaceDB) { sp.gate.RUnlock() }

// evictLocked closes the least recently used idle files beyond the limit.
func (s *Store) evictLocked(keep string) {
	if len(s.open) <= MaxOpenSpaces {
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
		if len(s.open) <= MaxOpenSpaces {
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

func (s *Store) Pin(name string) {
	s.mu.Lock()
	s.pins[name]++
	s.mu.Unlock()
}

func (s *Store) Unpin(name string) {
	s.mu.Lock()
	if s.pins[name]--; s.pins[name] <= 0 {
		delete(s.pins, name)
	}
	s.mu.Unlock()
}

func (s *Store) SpaceExists(name string) bool {
	s.mu.Lock()
	_, ok := s.stats[name]
	s.mu.Unlock()
	if ok {
		return true
	}
	_, err := os.Stat(s.spacePath(name))
	return err == nil
}

func (s *Store) SpaceCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.stats)
}

// rescan syncs the in-memory totals with the files in spaces/. Files copied
// in are picked up, files removed by hand are forgotten.
func (s *Store) Rescan() error {
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
		if !names.SafeSpaceFile(name) {
			log.Printf("ignoring spaces/%s: not a usable space file name", e.Name())
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
		s.Forget(name)
	}
	return nil
}

// forget drops a space from memory after its file disappeared.
func (s *Store) Forget(name string) {
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
	s.OnChange(name)
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
