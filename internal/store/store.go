// Package store keeps all data on disk: one SQLite file per space plus
// server.db for visitors. It knows nothing about HTTP.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/timjaanson/cubby/internal/config"
	"github.com/timjaanson/cubby/internal/names"
	_ "modernc.org/sqlite"
)

// Data layout:
//
//	{data}/server.db           visitors, which spaces each visitor wrote to, deleted-space counters
//	{data}/spaces/{name}.db    one SQLite file per space, self-contained
//	{data}/admin.sock          Unix socket for `cubby admin`
//
// A space file holds everything about the space, including its counter and
// lock key, so it can be copied to another server or deleted on its own.

const MaxOpenSpaces = 64

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
	cfg *config.Config
	dir string
	srv *sql.DB
	// onChange is called after the server itself changes a space outside a
	// client write (cleanup, admin), so watchers wake up.
	OnChange func(space string)

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

func Open(cfg *config.Config) (*Store, error) {
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
		cfg: cfg, dir: dir, srv: srv, OnChange: func(string) {},
		open: map[string]*spaceDB{}, stats: map[string]*spaceStat{}, pins: map[string]int{},
		vbytes: map[string]int64{}, vspaces: map[string]bool{}, vtouched: map[string]int64{},
	}
	if err := s.Rescan(); err != nil {
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
func (s *Store) Touch(name string) {
	if sp, _, err := s.Acquire(name, false, ""); err == nil && sp != nil {
		s.touchSpace(sp)
		s.Release(sp)
	}
}

func NowMs() int64 { return time.Now().UnixMilli() }
func nowS() int64  { return time.Now().Unix() }

// spacePath maps a space to its file. Names are checked here as well as in
// the router, so no caller can ever produce a path outside spaces/.
func (s *Store) spacePath(name string) string {
	if !names.ValidSpace(name) {
		panic("cubby: unchecked space name " + strconv.Quote(name))
	}
	return filepath.Join(s.dir, "spaces", name+".db")
}
