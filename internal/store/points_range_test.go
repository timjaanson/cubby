package store

import (
	"context"
	"strings"
	"testing"
)

// since, before, after and desc page both ways, and the 4 MB page cap sets next.
func TestReadPointsRange(t *testing.T) {
	s, space, id := testStore(t)
	ctx := context.Background()
	t0 := NowMs() - 10_000_000
	big := `"` + strings.Repeat("x", 4000) + `"`
	// 1100 points of 4 KB: more than one 4 MB page.
	for i := 0; i < 1100; i += 100 {
		var reqs []WriteReq
		for j := i; j < i+100; j++ {
			reqs = append(reqs, WriteReq{Space: space, Area: "s", Key: "m", Writer: id, Value: []byte(big), IsJSON: true, Point: true, T: t0 + int64(j)})
		}
		if _, err := s.WriteBatch(ctx, reqs); err != nil {
			t.Fatal(err)
		}
	}
	p := func(n int64) *int64 { return &n }
	read := func(q PointsQuery) ([]Point, *int64) {
		t.Helper()
		pts, next, _, err := s.ReadPoints(space, "s", "m", q)
		if err != nil {
			t.Fatal(err)
		}
		return pts, next
	}

	// Desc: newest first, next continues as after.
	pts, next := read(PointsQuery{Desc: true, Limit: 3})
	if len(pts) != 3 || pts[0].T != t0+1099 || pts[2].T != t0+1097 || next == nil || *next != t0+1097 {
		t.Fatalf("desc page 1: %d points, next %v", len(pts), next)
	}
	pts, next = read(PointsQuery{Desc: true, After: next, Limit: 3})
	if pts[0].T != t0+1096 || *next != t0+1094 {
		t.Fatalf("desc page 2 starts at %d", pts[0].T)
	}
	// since inclusive, before exclusive; the last page has no next.
	pts, next = read(PointsQuery{Desc: true, Since: p(t0 + 10), Before: p(t0 + 14), Limit: 10})
	if len(pts) != 4 || pts[0].T != t0+13 || pts[3].T != t0+10 || next != nil {
		t.Fatalf("desc between: %+v next %v", len(pts), next)
	}
	pts, next = read(PointsQuery{Since: p(t0 + 10), Before: p(t0 + 14), Limit: 2})
	if len(pts) != 2 || pts[0].T != t0+10 || next == nil || *next != t0+11 {
		t.Fatalf("asc between: %d next %v", len(pts), next)
	}
	pts, next = read(PointsQuery{Since: p(t0 + 10), Before: p(t0 + 14), After: next, Limit: 2})
	if len(pts) != 2 || pts[0].T != t0+12 || next != nil {
		t.Fatalf("asc between, page 2: %d next %v", len(pts), next)
	}
	// Ascending without since starts at the oldest point.
	pts, next = read(PointsQuery{Limit: 5})
	if len(pts) != 5 || pts[0].T != t0 || next == nil || *next != t0+4 {
		t.Fatalf("oldest first: %d next %v", len(pts), next)
	}

	// A 10,000 limit stops at 4 MB and continues from next, both ways.
	for _, desc := range []bool{false, true} {
		q := PointsQuery{Desc: desc, Limit: 10000}
		seen, pages := 0, 0
		for {
			pts, next := read(q)
			seen += len(pts)
			pages++
			if next == nil {
				break
			}
			q.After = next
		}
		if seen != 1100 || pages != 2 {
			t.Fatalf("desc=%v: %d points in %d pages", desc, seen, pages)
		}
	}
}
