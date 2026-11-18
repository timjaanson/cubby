package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/timjaanson/cubby/internal/config"
	"github.com/timjaanson/cubby/internal/store"
)

// Only growth of stored data counts against an IP's daily byte budget.
// Rewrites in place are free; once the budget runs out, writes wait.
func TestStoredGrowthBudget(t *testing.T) {
	c := newTest(t, func(cfg *config.Config) { cfg.Limits.StoredBytesPerIPPerDay = 1000 })
	space := "grow-cedar-pilot-mango"
	tok := c.open(space, "")
	auth := []string{"Authorization", "Bearer " + tok}
	path := "/v1/" + space + "/shared/"
	put := func(key, body string) (int, string) {
		s, _, b := c.req("PUT", path+key, body, auth...)
		return s, b
	}
	if s, b := put("a", strings.Repeat("a", 600)); s != 201 {
		t.Fatalf("first: %d %s", s, b)
	}
	for i := range 20 {
		if s, b := put("a", strings.Repeat(string(rune('b'+i%20)), 600)); s != 200 {
			t.Fatalf("rewrite %d: %d %s", i, s, b)
		}
	}
	// 399 left: enough to start, and this write runs the budget below zero.
	s, h, b := c.req("PUT", path+"b", strings.Repeat("b", 600), auth...)
	if s != 201 {
		t.Fatalf("second: %d %s", s, b)
	}
	if !strings.Contains(h.Get("RateLimit-Policy"), `"stored";q=1000;w=86400;qu="bytes"`) || !strings.Contains(h.Get("RateLimit"), `"stored";r=0`) {
		t.Fatalf("headers %v", h)
	}
	if s, b := put("c", "x"); s != 429 || errCode(b) != "stored_limit" {
		t.Fatalf("over budget: %d %s", s, b)
	}
	if s, _, b := c.req("POST", path+"temp/series", "1", auth...); s != 429 || errCode(b) != "stored_limit" {
		t.Fatalf("series over budget: %d %s", s, b)
	}
	if s, _, b := c.req("DELETE", path+"b", "", auth...); s != 200 {
		t.Fatalf("delete over budget: %d %s", s, b)
	}
}

// Lists and watches stop a page at store.PageBytes and continue from there.
func TestPagesStopAtPageBytes(t *testing.T) {
	c := newTest(t)
	space := "page-cedar-pilot-mango"
	tok := c.open(space, "")
	auth := []string{"Authorization", "Bearer " + tok}
	big := strings.Repeat("x", 512<<10)
	for i := range 10 {
		if s, _, b := c.req("PUT", "/v1/"+space+"/shared/big."+itoa(int64(i)), big, auth...); s != 201 {
			t.Fatalf("put %d: %d %s", i, s, b)
		}
	}
	perPage := store.PageBytes / len(big)

	var keys []string
	after := ""
	pages := 0
	for {
		q := "/v1/" + space + "/shared/big.*"
		if after != "" {
			q += "?after=" + after
		}
		_, _, b := c.req("GET", q, "", auth...)
		var l struct {
			Items []struct{ Key string }
			Next  *string
		}
		if err := json.Unmarshal([]byte(b), &l); err != nil {
			t.Fatal(err)
		}
		if pages == 0 && len(l.Items) != perPage {
			t.Fatalf("first page has %d items, want %d", len(l.Items), perPage)
		}
		for _, it := range l.Items {
			keys = append(keys, it.Key)
		}
		pages++
		if l.Next == nil {
			break
		}
		after = *l.Next
	}
	if len(keys) != 10 || pages != 2 {
		t.Fatalf("listed %d keys in %d pages", len(keys), pages)
	}

	var seen int
	from := "0"
	for range 5 {
		_, _, b := c.req("GET", "/v1/"+space+"/shared/big.*/watch?after="+from, "", auth...)
		var w struct {
			Changes []struct{ Key string }
			Version int64
			More    bool
		}
		if err := json.Unmarshal([]byte(b), &w); err != nil {
			t.Fatal(err)
		}
		seen += len(w.Changes)
		from = itoa(w.Version)
		if !w.More {
			break
		}
		if len(w.Changes) != perPage {
			t.Fatalf("watch page has %d changes, want %d", len(w.Changes), perPage)
		}
	}
	if seen != 10 {
		t.Fatalf("watched %d changes", seen)
	}
}

// A value at the size limit goes over a live connection too.
func TestLiveTakesLargestValue(t *testing.T) {
	c := newTest(t)
	space := "wide-cedar-pilot-mango"
	tok := c.open(space, "")
	w := c.dial(space, "Authorization", "Bearer "+tok)
	text := strings.Repeat("y", int(c.cfg.Limits.MaxValueSize)-2)
	if f := w.call("1", `{"id":1,"method":"PUT","path":"shared/doc","body":"`+text+`"}`); f.Status != 201 {
		t.Fatalf("live put: %d %s", f.Status, f.Body)
	}
	// Text values travel escaped; quotes double in size.
	quoted := strings.Repeat(`\"`, int(c.cfg.Limits.MaxValueSize)/2)
	if f := w.call("2", `{"id":2,"method":"PUT","path":"shared/quotes","text":"`+quoted+`"}`); f.Status != 201 {
		t.Fatalf("live text put: %d %s", f.Status, f.Body)
	}
	if f, ok := w.next(50 * time.Millisecond); ok {
		t.Fatalf("unexpected frame %+v", f)
	}
}
