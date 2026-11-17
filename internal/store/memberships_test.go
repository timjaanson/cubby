package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/timjaanson/cubby/internal/access"
	"github.com/timjaanson/cubby/internal/config"
)

// A v0.19 server.db has no membership times; opening it fills them from
// the space files.
func TestMigrateMembership(t *testing.T) {
	cfg := config.Default()
	cfg.Server.DataDir = t.TempDir()
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	id := newVisitor(t, s)
	name, err := s.CreateSpace("", "", access.Default, id)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	db, err := sql.Open("sqlite", filepath.Join(cfg.Server.DataDir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX visitor_spaces_by_changed; ALTER TABLE visitor_spaces DROP COLUMN since;
		ALTER TABLE visitor_spaces DROP COLUMN changed; PRAGMA user_version = 2;`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	items, _, err := s.ListSpaces(id, nil, 10)
	if err != nil || len(items) != 1 || items[0].Space != name || items[0].Since == 0 || items[0].Access != access.Owner {
		t.Fatalf("after migration %+v %v", items, err)
	}
	var v int
	s.srv.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != serverVersion {
		t.Fatalf("user_version %d", v)
	}
}
