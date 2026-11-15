package store

import (
	"path/filepath"
	"testing"

	"github.com/timjaanson/cubby/internal/config"
	"github.com/timjaanson/cubby/internal/names"
)

// A server.db from before devices keeps every visitor's token working, as
// that visitor's first device.
func TestMigrateServerDevices(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	tok := names.RandomToken("vt_")
	for _, q := range []string{
		`CREATE TABLE visitors (id TEXT PRIMARY KEY, token_hash TEXT NOT NULL UNIQUE, created_at INTEGER NOT NULL, last_seen_at INTEGER NOT NULL, stored INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE visitor_spaces (visitor TEXT NOT NULL, space TEXT NOT NULL, PRIMARY KEY (visitor, space)) WITHOUT ROWID`,
		`INSERT INTO visitors VALUES ('u_aaaaaaaaaaaaaaaa', '` + names.HashSecret(tok) + `', 1700000000, 1700000100, 1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	db.Close()

	cfg := config.Default()
	cfg.Server.DataDir = dir
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, dev, err := s.VisitorByToken(tok)
	if err != nil || id != "u_aaaaaaaaaaaaaaaa" || !names.ValidDeviceID(dev) {
		t.Fatalf("token after migration: %q %q %v", id, dev, err)
	}
	ds, _ := s.Devices(id, dev)
	if len(ds) != 1 || ds[0].Created != 1700000000_000 || !ds[0].Current {
		t.Fatalf("devices %+v", ds)
	}
	var stored, v int
	s.srv.QueryRow(`SELECT stored FROM visitors WHERE id = ?`, id).Scan(&stored)
	s.srv.QueryRow(`PRAGMA user_version`).Scan(&v)
	if stored != 1 || v != serverVersion {
		t.Fatalf("stored %d, version %d", stored, v)
	}
	// A new visitor works alongside.
	tok2, id2, err := s.NewVisitor()
	if got, _, _ := s.VisitorByToken(tok2); err != nil || got != id2 {
		t.Fatalf("new visitor %q %v", got, err)
	}
}
