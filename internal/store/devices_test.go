package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timjaanson/cubby/internal/auth"
	"github.com/timjaanson/cubby/internal/config"
)

// newVisitor registers a fresh key as a new visitor.
func newVisitor(t *testing.T, s *Store) string {
	t.Helper()
	k, err := auth.NewSigner()
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := s.NewVisitor(k.Key, "")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDeviceKeys(t *testing.T) {
	s, _, _ := testStore(t)
	k, _ := auth.NewEdSigner()
	id, created, err := s.NewVisitor(k.Key, "phone")
	if err != nil || !created {
		t.Fatal(err)
	}
	if again, created, _ := s.NewVisitor(k.Key, ""); again != id || created {
		t.Fatalf("again: %s %v", again, created)
	}
	v, key, err := s.DeviceKey(k.Key.ID)
	if err != nil || v != id || key.ID != k.Key.ID || key.Alg != "EdDSA" {
		t.Fatalf("device key: %s %+v %v", v, key, err)
	}
	if v, key, _ := s.DeviceKey("nope"); v != "" || key != nil {
		t.Fatal("unknown device")
	}
}

// Data from before v0.19 is refused with a clear message, not migrated.
func TestRefusesOldData(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`CREATE TABLE visitors (id TEXT PRIMARY KEY); PRAGMA user_version = 1`)
	db.Close()
	cfg := config.Default()
	cfg.Server.DataDir = dir
	if _, err := Open(cfg); err == nil || !strings.Contains(err.Error(), "v0.19") {
		t.Fatalf("old server.db: %v", err)
	}

	// An old space file next to a new server.db.
	dir2 := t.TempDir()
	cfg.Server.DataDir = dir2
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	os.MkdirAll(filepath.Join(dir2, "spaces"), 0o700)
	db, _ = openDB(filepath.Join(dir2, "spaces", "old-cedar-pilot-mango.db"))
	db.Exec(`CREATE TABLE meta (k TEXT PRIMARY KEY, v); PRAGMA user_version = 4`)
	db.Close()
	if _, err := Open(cfg); err == nil || !strings.Contains(err.Error(), "v0.19") {
		t.Fatalf("old space: %v", err)
	}
}
