package store

import (
	"context"
	"testing"

	"github.com/timjaanson/cubby/internal/access"
)

// A space from before to/ keeps its private data and scopes under to/me/.
func TestMigrateMeToTo(t *testing.T) {
	s, space, id := testStore(t)
	sp, err := s.Acquire(space)
	if err != nil {
		t.Fatal(err)
	}
	sp.wmu.Lock()
	for _, q := range []string{
		`INSERT INTO kv (area, key, value, is_json, version, writer, size, updated_at, t) VALUES ('m:` + id + `', 'draft', '"x"', 1, 7, '` + id + `', 8, 1, 1000)`,
		`INSERT INTO kv (area, key, value, is_json, version, writer, size, updated_at, series, t, n) VALUES ('m:` + id + `', 'sleep', '7', 1, 8, '` + id + `', 6, 1, 1, 2000, 1)`,
		`INSERT INTO points (area, key, t, value, is_json) VALUES ('m:` + id + `', 'sleep', 2000, '7', 1)`,
		`INSERT INTO grants (id, visitor, allow, source, created_at) VALUES ('g_aaaaaaaa', 'u_bbbbbbbbbbbbbbbb', '["shared:r","me:rw","me/notes.*:w"]', 'direct', 1)`,
		`PRAGMA user_version = 2`,
	} {
		if _, err := sp.db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	if err := migrateSpace(sp.db); err != nil {
		t.Fatal(err)
	}
	sp.dropAccess()
	sp.wmu.Unlock()
	s.Release(sp)

	e, err := s.Get(space, ToArea(id, id), "draft")
	if err != nil || e == nil || string(e.Value) != `"x"` || e.Version != 7 || e.From != id || e.To != id {
		t.Fatalf("draft after migration: %+v %v", e, err)
	}
	pts, _, _, err := s.ReadPoints(space, ToArea(id, id), "sleep", nil, 10)
	if err != nil || len(pts) != 1 {
		t.Fatalf("points: %v %v", pts, err)
	}
	gs, err := s.ListGrants(space, "u_bbbbbbbbbbbbbbbb")
	if err != nil || len(gs) != 1 {
		t.Fatalf("grants %v %v", gs, err)
	}
	want := []string{"shared:r", "to/me:rw", "to/me/notes.*:w"}
	for i, a := range gs[0].Allow {
		if a != want[i] {
			t.Fatalf("allow %v", gs[0].Allow)
		}
		if _, err := access.ParseScope(a); err != nil {
			t.Fatalf("scope %s: %v", a, err)
		}
	}
}

// Deleting a visitor removes what they sent and what was sent to them, and
// gives the senders their quota back.
func TestVisitorCleanupTo(t *testing.T) {
	s, space, alice := testStore(t)
	_, bob, _ := s.NewVisitor()
	ctx := context.Background()
	for _, w := range []WriteReq{
		{Space: space, Area: ToArea(bob, alice), Key: "hi", Value: []byte("1"), Writer: alice},
		{Space: space, Area: ToArea(alice, bob), Key: "yo", Value: []byte("22"), Writer: bob},
		{Space: space, Area: ToArea("owners", bob), Key: "q", Value: []byte("3"), Writer: bob},
		{Space: space, Area: ToArea(alice, alice), Key: "note", Value: []byte("4"), Writer: alice},
	} {
		if _, err := s.Write(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := s.VisitorTotal(alice)
	s.srv.Exec(`UPDATE visitors SET last_seen_at = 0 WHERE id = ?`, bob)
	if rep, err := s.Cleanup(); err != nil || rep.Visitors != 1 {
		t.Fatalf("cleanup %+v %v", rep, err)
	}
	for _, area := range []string{ToArea(bob, alice), ToArea(alice, bob), ToArea("owners", bob)} {
		items, _, _, _ := s.List(Target{Space: space, Area: area, Pattern: true}, "", 10, false)
		if len(items) != 0 {
			t.Fatalf("%s still has %v", area, items)
		}
	}
	if e, _ := s.Get(space, ToArea(alice, alice), "note"); e == nil {
		t.Fatal("alice's own note went too")
	}
	after, _ := s.VisitorTotal(alice)
	if after >= before {
		t.Fatalf("alice's usage %d -> %d", before, after)
	}
}
