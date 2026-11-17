package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/timjaanson/cubby/internal/access"
	"github.com/timjaanson/cubby/internal/config"
)

// expirePointsAt marks the points at these times of a series as due.
func expirePointsAt(t *testing.T, s *Store, space, key string, ts ...int64) {
	t.Helper()
	sp, err := s.Acquire(space)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Release(sp)
	sp.wmu.Lock()
	defer sp.wmu.Unlock()
	for _, at := range ts {
		if _, err := sp.db.Exec(`UPDATE points SET expires=? WHERE key=? AND t=?`, NowMs()-1, key, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.setNextExpiry(space, sp.db); err != nil {
		t.Fatal(err)
	}
}

func TestPointExpiry(t *testing.T) {
	s, space, id := testStore(t)
	ctx := context.Background()
	add := func(key string, at, ttl int64, v string) {
		t.Helper()
		if _, err := s.Write(ctx, WriteReq{Space: space, Area: "s", Key: key, Writer: id, Value: []byte(v), IsJSON: true, Point: true, T: at, TTL: ttl}); err != nil {
			t.Fatal(err)
		}
	}
	base0, keys0, vis0, _ := s.Usage(space, id)
	t0 := NowMs() - 10_000
	add("m1", t0, 0, "1")
	add("m1", t0+1, 3600, "22")
	add("m1", t0+2, 3600, "333")

	pts, _, _, err := s.ReadPoints(space, "s", "m1", PointsQuery{Limit: 10})
	if err != nil || len(pts) != 3 || pts[0].Expires != 0 || pts[2].Expires != t0+2+3_600_000 {
		t.Fatalf("points %+v %v", pts, err)
	}
	before, _ := s.Get(space, "s", "m1")

	// The newest point expires: the key shows the next newest, as a change.
	expirePointsAt(t, s, space, "m1", t0+2)
	pts, _, _, _ = s.ReadPoints(space, "s", "m1", PointsQuery{Limit: 10})
	after, _ := s.Get(space, "s", "m1")
	if len(pts) != 2 || string(after.Value) != "22" || after.T != t0+1 || after.Version <= before.Version {
		t.Fatalf("after newest expired: %d points, key %+v", len(pts), after)
	}
	// An older point expires: the key stays as it is.
	add("m1", t0+3, 0, "4")
	k1, _ := s.Get(space, "s", "m1")
	expirePointsAt(t, s, space, "m1", t0+1)
	k2, _ := s.Get(space, "s", "m1")
	if k2.Version != k1.Version || string(k2.Value) != "4" {
		t.Fatalf("older point expiry changed the key: %+v -> %+v", k1, k2)
	}

	// A series whose last point expires is gone, and its bytes with it.
	add("gone", t0, 3600, `"x"`)
	expirePointsAt(t, s, space, "gone", t0)
	if _, _, _, err := s.ReadPoints(space, "s", "gone", PointsQuery{Limit: 10}); !isCode(err, "not_found") {
		t.Fatalf("expired series: %v", err)
	}
	if e, err := s.Get(space, "s", "gone"); e != nil || err != nil {
		t.Fatalf("expired series key: %+v %v", e, err)
	}

	// What's left is m1 with points 1 and 4, its key holding "4".
	bytes, keys, vis, _ := s.Usage(space, id)
	wantSpace := base0 + int64(len("m1")+len("1")) + int64(len("m1")+len("4")) + int64(len("m1")+len("4")) + int64(len("gone"))
	if keys != keys0+1 || bytes != wantSpace || vis != vis0+int64(len("m1")+len("4")) {
		t.Fatalf("usage: bytes %d want %d, keys %d want %d, visitor %d want %d", bytes, wantSpace, keys, keys0+1, vis, vis0+3)
	}

	// The sweeper finds due points without a read.
	add("swept", t0, 3600, "1")
	expirePointsAt(t, s, space, "swept", t0)
	if err := s.SweepExpired(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.PointCount(space); n != 2 {
		t.Fatalf("points after sweep: %d", n)
	}
}

func TestSpaceMigrationV5(t *testing.T) {
	cfg := config.Default()
	cfg.Server.DataDir = t.TempDir()
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	id := newVisitor(t, s)
	space, err := s.CreateSpace("", "", access.Default, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(context.Background(), WriteReq{Space: space, Area: "s", Key: "m5", Writer: id, Value: []byte("1"), Point: true, T: 1000}); err != nil {
		t.Fatal(err)
	}
	path := s.spacePath(space)
	s.Close()

	// Turn the file back into a v0.20 one.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX points_by_expiry; ALTER TABLE points DROP COLUMN expires; PRAGMA user_version = 5;`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err = Open(cfg)
	if err != nil {
		t.Fatalf("open v5: %v", err)
	}
	defer s.Close()
	if _, err := s.Write(context.Background(), WriteReq{Space: space, Area: "s", Key: "m5", Writer: id, Value: []byte("2"), Point: true, T: NowMs(), TTL: 60}); err != nil {
		t.Fatal(err)
	}
	pts, _, _, err := s.ReadPoints(space, "s", "m5", PointsQuery{Limit: 10})
	if err != nil || len(pts) != 2 || pts[0].Expires != 0 || pts[1].Expires == 0 {
		t.Fatalf("after migration: %+v %v", pts, err)
	}
}
