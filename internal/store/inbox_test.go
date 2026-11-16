package store

import (
	"context"
	"testing"

	"github.com/timjaanson/cubby/internal/access"
)

// legacyAccess puts a space file's access tables back the way they were
// before levels: scopes in allow, an owner flag, settings as letters.
func legacyAccess(t *testing.T, db execer, owner, settings string) {
	t.Helper()
	for _, q := range []string{
		`DROP TABLE grants`,
		`DROP TABLE tokens`,
		`CREATE TABLE tokens (id TEXT PRIMARY KEY, hash TEXT NOT NULL UNIQUE, label TEXT NOT NULL DEFAULT '', allow TEXT NOT NULL,
		  uses_left INTEGER, until INTEGER, grant_for INTEGER, created_at INTEGER NOT NULL, ended_at INTEGER, last_redeemed_at INTEGER,
		  minted_by TEXT, revoked_at INTEGER) WITHOUT ROWID`,
		`CREATE TABLE grants (id TEXT PRIMARY KEY, visitor TEXT NOT NULL, owner INTEGER NOT NULL DEFAULT 0, allow TEXT NOT NULL,
		  source TEXT NOT NULL, until INTEGER, created_at INTEGER NOT NULL, last_used_at INTEGER) WITHOUT ROWID`,
		`CREATE INDEX grants_by_visitor ON grants (visitor)`,
		`CREATE UNIQUE INDEX grants_once ON grants (visitor, source) WHERE source <> 'direct'`,
		`INSERT INTO grants (id, visitor, owner, allow, source, created_at) VALUES ('g_owner000', '` + owner + `', 1, '[]', 'create', 1)`,
		`UPDATE meta SET v = '` + settings + `' WHERE k = 'settings'`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
}

// A space from before levels keeps its data, moves me/ to to/me/ and
// users/ to by/, and gives every grant and invite the level closest to its
// scopes.
func TestMigrateOldSpace(t *testing.T) {
	s, space, id := testStore(t)
	visitors := make([]string, 5)
	for i := range visitors {
		_, visitors[i], _ = s.NewVisitor()
	}
	all, viewer, poster, banned, ownerBanned := visitors[0], visitors[1], visitors[2], visitors[3], visitors[4]
	sp, err := s.Acquire(space)
	if err != nil {
		t.Fatal(err)
	}
	sp.wmu.Lock()
	legacyAccess(t, sp.db, id, "agaam")
	for _, q := range []string{
		`INSERT INTO kv (area, key, value, is_json, version, writer, size, updated_at, t) VALUES ('m:` + id + `', 'draft', '"x"', 1, 7, '` + id + `', 8, 1, 1000)`,
		`INSERT INTO kv (area, key, value, is_json, version, writer, size, updated_at, series, t, n) VALUES ('m:` + id + `', 'sleep', '7', 1, 8, '` + id + `', 6, 1, 1, 2000, 1)`,
		`INSERT INTO points (area, key, t, value, is_json) VALUES ('m:` + id + `', 'sleep', 2000, '7', 1)`,
		`INSERT INTO kv (area, key, value, is_json, version, writer, size, updated_at, t) VALUES ('u:` + all + `', 'vote', '3', 1, 9, '` + all + `', 5, 1, 1000)`,
		`INSERT INTO grants (id, visitor, allow, source, created_at) VALUES ('g_a0000000', '` + all + `', '["*:rw"]', 'direct', 1)`,
		`INSERT INTO grants (id, visitor, allow, source, created_at) VALUES ('g_v0000000', '` + viewer + `', '["shared:r","users:r"]', 'direct', 1)`,
		`INSERT INTO grants (id, visitor, allow, source, created_at) VALUES ('g_p0000000', '` + poster + `', '["shared:r","me:rw"]', 'direct', 1)`,
		`INSERT INTO grants (id, visitor, allow, source, created_at) VALUES ('g_b0000000', '` + banned + `', '["*:rw"]', 'direct', 1)`,
		`INSERT INTO grants (id, visitor, owner, allow, source, created_at) VALUES ('g_o0000000', '` + ownerBanned + `', 1, '[]', 'direct', 1)`,
		`INSERT INTO bans (visitor, by, created_at) VALUES ('` + banned + `', '` + id + `', 1), ('` + ownerBanned + `', '` + id + `', 1)`,
		`INSERT INTO tokens (id, hash, allow, created_at) VALUES ('t_full0000', 'h1', '["*:rw"]', 1), ('t_look0000', 'h2', '["shared:r"]', 1), ('t_stat0000', 'h3', '["shared/status.*:w"]', 1)`,
		`PRAGMA user_version = 2`,
	} {
		if _, err := sp.db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	if err := migrateSpace(sp.db); err != nil {
		t.Fatal(err)
	}
	var v int
	sp.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != spaceVersion {
		t.Fatalf("user_version %d", v)
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
	e, err = s.Get(space, ByArea(all), "vote")
	if err != nil || e == nil || e.Version != 9 || e.By() != all {
		t.Fatalf("by/ after migration: %+v %v", e, err)
	}

	want := map[string]access.Level{id: access.Owner, all: access.All, viewer: access.Read, poster: access.Post, banned: access.Banned, ownerBanned: access.Owner}
	for who, l := range want {
		r, err := s.Rights(space, who)
		if err != nil || r.Level() != max(l, r.Settings.Anyone) && l != access.Banned || l == access.Banned && r.Level() != access.Banned {
			t.Fatalf("%s: want %v, got %+v %v", who, l, r, err)
		}
		if l != access.Banned && r.Member != l {
			t.Fatalf("%s member %v, want %v", who, r.Member, l)
		}
	}
	r, _ := s.Rights(space, "")
	if r.Settings != (access.Settings{Anyone: access.Post, Invite: access.Post}) {
		t.Fatalf("settings %+v", r.Settings)
	}
	toks, err := s.ListTokens(space, "")
	got := map[string]access.Level{}
	for _, tk := range toks {
		got[tk.ID] = tk.Access
	}
	if err != nil || got["t_full0000"] != access.All || got["t_look0000"] != access.Read || got["t_stat0000"] != access.All {
		t.Fatalf("tokens %+v %v", toks, err)
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
