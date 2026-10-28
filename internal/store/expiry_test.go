package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/timjaanson/cubby/internal/access"
	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/config"
)

func testStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	cfg := config.Default()
	cfg.Server.DataDir = t.TempDir()
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	_, id, err := s.NewVisitor()
	if err != nil {
		t.Fatal(err)
	}
	name, _, err := s.CreateSpace("", "", access.Private, id)
	if err != nil {
		t.Fatal(err)
	}
	return s, name, id
}

func forceExpiry(t *testing.T, s *Store, space, key string) {
	t.Helper()
	sp, err := s.Acquire(space)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Release(sp)
	sp.wmu.Lock()
	defer sp.wmu.Unlock()
	if _, err = sp.db.Exec(`UPDATE kv SET expires=? WHERE key=?`, NowMs()-1, key); err != nil {
		t.Fatal(err)
	}
	if err = s.setNextExpiry(space, sp.db); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseExpiryAndContenders(t *testing.T) {
	s, space, id := testStore(t)
	ctx := context.Background()
	zero := int64(0)
	req := WriteReq{Space: space, Area: "s", Key: "lock", Writer: id, Value: []byte("holder"), TTL: 60, IfVersion: &zero}
	first, err := s.Write(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.IfVersion = &first.Version
	renewal, err := s.Write(ctx, req)
	if err != nil || renewal.Version != first.Version || renewal.Changed || renewal.T != first.T {
		t.Fatalf("renewal: %+v %v", renewal, err)
	}
	before, err := s.VisitorTotal(id)
	if err != nil {
		t.Fatal(err)
	}
	forceExpiry(t, s, space, "lock")
	// A failed old renewal still commits the expiry and its deletion marker.
	if _, err = s.Write(ctx, req); !isCode(err, "version_conflict") {
		t.Fatalf("stale renewal: %v", err)
	}
	changes, _, err := s.Changes(Target{Space: space, Area: "s", Key: "lock"}, first.Version, 100)
	if err != nil || len(changes) != 1 || changes[0].Value != nil || !changes[0].Expired || changes[0].Writer != id {
		t.Fatalf("expiry changes: %+v %v", changes, err)
	}
	after, err := s.VisitorTotal(id)
	if err != nil || before-after != int64(len("lockholder")) {
		t.Fatalf("usage %d -> %d: %v", before, after, err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	var winner int64
	for range 12 {
		wg.Go(func() {
			r := req
			r.IfVersion = &zero
			res, err := s.Write(ctx, r)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
				winner = res.Version
			} else if !isCode(err, "version_conflict") {
				t.Errorf("contender: %v", err)
			}
		})
	}
	wg.Wait()
	if wins != 1 || winner <= first.Version {
		t.Fatalf("wins=%d version=%d", wins, winner)
	}
	// An old holder cannot delete the replacement lease.
	req.Value = nil
	if _, err = s.Write(ctx, req); !isCode(err, "version_conflict") {
		t.Fatalf("old release: %v", err)
	}
	e, err := s.Get(space, "s", "lock")
	if err != nil || e == nil || e.Version != winner {
		t.Fatalf("winner missing: %+v %v", e, err)
	}
}

func isCode(err error, code string) bool {
	var e *apierr.Error
	return errors.As(err, &e) && e.Code == code
}

func TestTTLRemovalAndRestart(t *testing.T) {
	s, space, id := testStore(t)
	ctx := context.Background()
	req := WriteReq{Space: space, Area: "s", Key: "presence", Writer: id, Value: []byte("here"), TTL: 60}
	first, err := s.Write(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.TTL = 0
	permanent, err := s.Write(ctx, req)
	if err != nil || permanent.Expires != 0 || permanent.Version != first.Version {
		t.Fatalf("make permanent: %+v %v", permanent, err)
	}
	if err = s.SweepExpired(ctx); err != nil {
		t.Fatal(err)
	}
	e, err := s.Get(space, "s", req.Key)
	if err != nil || e.Expires != 0 {
		t.Fatalf("permanent: %+v %v", e, err)
	}
	req.TTL = 60
	if _, err = s.Write(ctx, req); err != nil {
		t.Fatal(err)
	}
	forceExpiry(t, s, space, req.Key)
	cfg := s.cfg
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	// Due spaces are discovered at startup without requiring a client request.
	if err = reopened.SweepExpired(ctx); err != nil {
		t.Fatal(err)
	}
	changes, _, err := reopened.Changes(Target{Space: space, Area: "s", Key: req.Key}, first.Version, 100)
	if err != nil || len(changes) != 1 || !changes[0].Expired {
		t.Fatalf("restart: %+v %v", changes, err)
	}
}

func TestMigrationPreservesOldData(t *testing.T) {
	cfg := config.Default()
	cfg.Server.DataDir = t.TempDir()
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, id, _ := s.NewVisitor()
	space, _, err := s.CreateSpace("", "", access.Private, id)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Write(context.Background(), WriteReq{Space: space, Area: "s", Key: "old", Writer: id, Value: []byte("saved")})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateToken(space, TokenSpec{By: id, Allow: []string{"*:rw"}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	db, err := openDB(filepath.Join(cfg.Server.DataDir, "spaces", space+".db"))
	if err != nil {
		t.Fatal(err)
	}
	// Recreate exactly the pre-migration column set, including NULL plain-key t.
	_, err = db.Exec(`DROP INDEX IF EXISTS kv_expiry; ALTER TABLE kv DROP COLUMN expires; ALTER TABLE kv DROP COLUMN expired;
		ALTER TABLE tokens DROP COLUMN minted_by; ALTER TABLE tokens DROP COLUMN revoked_at; UPDATE kv SET t=NULL,updated_at=1700000000; PRAGMA user_version=0`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e, err := s.Get(space, "s", "old")
	if err != nil || e == nil || string(e.Value) != "saved" || e.Version != first.Version || e.T != 1700000000000 || e.Expires != 0 {
		t.Fatalf("migrated: %+v %v", e, err)
	}
	rights, err := s.Rights(space, id)
	if err != nil || !rights.Owner {
		t.Fatalf("lost ownership: %+v %v", rights, err)
	}
	toks, err := s.ListTokens(space, "")
	if err != nil || len(toks) != 1 || toks[0].By != nil || toks[0].State != "active" {
		t.Fatalf("tokens after migration: %+v %v", toks, err)
	}
}

func TestDeadlineBoundary(t *testing.T) {
	deadline := int64(1234)
	if CheckBefore(&deadline, 1233) != nil || !isCode(CheckBefore(&deadline, 1234), "too_late") || !isCode(CheckBefore(&deadline, 1235), "too_late") {
		t.Fatal("deadline boundary")
	}
}

func TestExpiredReadsAndLeaseBeforeSweep(t *testing.T) {
	for _, read := range []string{"get", "list", "watch", "series"} {
		t.Run(read, func(t *testing.T) {
			s, space, id := testStore(t)
			first, err := s.Write(context.Background(), WriteReq{Space: space, Area: "s", Key: "gone", Writer: id, Value: []byte("temporary"), TTL: 60})
			if err != nil {
				t.Fatal(err)
			}
			forceExpiry(t, s, space, "gone")
			switch read {
			case "get":
				e, err := s.Get(space, "s", "gone")
				if err != nil || e != nil {
					t.Fatalf("expired get %+v %v", e, err)
				}
			case "list":
				items, _, _, err := s.List(Target{Space: space, Area: "s", Pattern: true}, "", 100, false)
				if err != nil || len(items) != 0 {
					t.Fatalf("expired list %+v %v", items, err)
				}
			case "watch":
				items, _, err := s.Changes(Target{Space: space, Area: "s", Pattern: true}, 0, 100)
				if err != nil || len(items) != 0 {
					t.Fatalf("expired snapshot %+v %v", items, err)
				}
			case "series":
				_, _, _, err := s.ReadPoints(space, "s", "gone", nil, 100)
				if !isCode(err, "not_found") {
					t.Fatalf("expired series %v", err)
				}
			}
			changes, _, err := s.Changes(Target{Space: space, Area: "s", Key: "gone"}, first.Version, 100)
			if err != nil || len(changes) != 1 || !changes[0].Expired {
				t.Fatalf("expiry lost %+v %v", changes, err)
			}
		})
	}
}
