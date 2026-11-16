package store

import (
	"context"
	"testing"
)

// Deleting a visitor removes what they sent and what was sent to them, and
// gives the senders their quota back.
func TestVisitorCleanupTo(t *testing.T) {
	s, space, alice := testStore(t)
	bob := newVisitor(t, s)
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
