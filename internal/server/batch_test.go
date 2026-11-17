package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/timjaanson/cubby/internal/config"
)

type batchOut struct {
	Version int64
	Items   []struct {
		Path    string
		Version int64
		Created bool
	}
	Error string
	Index *int
}

func batchBody(t *testing.T, b string) batchOut {
	t.Helper()
	var out batchOut
	if err := json.Unmarshal([]byte(b), &out); err != nil {
		t.Fatalf("batch body %s", b)
	}
	return out
}

func TestBatchWrites(t *testing.T) {
	c := newTest(t)
	own := c.create(`{"anyone":"all"}`, "")
	sp := "/v1/" + own.Space
	a := bearer(own.Token)
	_, bobID := c.anon()
	c.req("PUT", sp+"/shared/old", `1`, a...)

	st, _, b := c.req("POST", sp+"/batch", `[
		{"method":"PUT","path":"shared/title","body":"Groceries"},
		{"method":"PUT","path":"by/me/cursor","text":"{not json","query":{"ttl":60}},
		{"method":"PUT","path":"to/`+bobID+`/note","body":{"hi":1}},
		{"method":"DELETE","path":"shared/old"}]`, a...)
	if st != 200 {
		t.Fatalf("batch: %d %s", st, b)
	}
	out := batchBody(t, b)
	if len(out.Items) != 4 || out.Items[0].Path != "shared/title" || !out.Items[0].Created || out.Items[3].Created {
		t.Fatalf("items %s", b)
	}
	for i := 1; i < 4; i++ {
		if out.Items[i].Version != out.Items[i-1].Version+1 {
			t.Fatalf("versions not consecutive: %s", b)
		}
	}
	if out.Version != out.Items[3].Version {
		t.Fatalf("top version %s", b)
	}
	if _, h, body := c.req("GET", sp+"/by/me/cursor", "", a...); body != "{not json" || !strings.HasPrefix(h.Get("Content-Type"), "text/plain") || h.Get("KV-Expires") == "" {
		t.Fatalf("text value with ttl: %s %v", body, h)
	}
	if s, _, _ := c.req("GET", sp+"/shared/old", "", a...); s != 404 {
		t.Fatalf("deleted key: %d", s)
	}

	// An identical rewrite takes no version, as alone.
	_, _, b = c.req("POST", sp+"/batch", `[{"method":"PUT","path":"shared/title","body":"Groceries"},{"method":"PUT","path":"shared/n","body":2}]`, a...)
	out2 := batchBody(t, b)
	if out2.Items[0].Version != out.Items[0].Version || out2.Items[1].Version != out.Version+1 {
		t.Fatalf("identical rewrite: %s", b)
	}
}

func TestBatchAllOrNothing(t *testing.T) {
	c := newTest(t, func(cfg *config.Config) { cfg.Limits.MaxSpaceKeys = 4 })
	own := c.create(`{"anyone":"read"}`, "")
	sp := "/v1/" + own.Space
	a := bearer(own.Token)
	c.req("PUT", sp+"/shared/x", `1`, a...)
	_, _, before := c.req("GET", sp+"/shared/*", "", a...)
	unchanged := func(what string) {
		t.Helper()
		if _, _, now := c.req("GET", sp+"/shared/*", "", a...); now != before {
			t.Fatalf("%s left changes: %s", what, now)
		}
	}

	// A version conflict in the middle: nothing lands, the error names it.
	st, _, b := c.req("POST", sp+"/batch", `[
		{"method":"PUT","path":"shared/a","body":1},
		{"method":"PUT","path":"shared/b","body":2},
		{"method":"PUT","path":"shared/x","body":3,"query":{"ifVersion":1}}]`, a...)
	if out := batchBody(t, b); st != 412 || out.Error != "version_conflict" || out.Index == nil || *out.Index != 2 {
		t.Fatalf("conflict: %d %s", st, b)
	}
	unchanged("conflict")

	// The space's key limit, reached by the batch itself.
	st, _, b = c.req("POST", sp+"/batch", `[
		{"method":"PUT","path":"shared/a","body":1},{"method":"PUT","path":"shared/b","body":1},
		{"method":"PUT","path":"shared/c","body":1},{"method":"PUT","path":"shared/d","body":1}]`, a...)
	if out := batchBody(t, b); st != 507 || out.Error != "space_quota_exceeded" || *out.Index != 3 {
		t.Fatalf("quota: %d %s", st, b)
	}
	unchanged("quota")

	// Deleting what isn't there fails the batch, as alone.
	st, _, b = c.req("POST", sp+"/batch", `[{"method":"PUT","path":"shared/a","body":1},{"method":"DELETE","path":"shared/nope"}]`, a...)
	if out := batchBody(t, b); st != 404 || *out.Index != 1 {
		t.Fatalf("missing delete: %d %s", st, b)
	}
	unchanged("missing delete")

	// Access is checked per write, before anything is written.
	stranger, _ := c.anon()
	st, _, b = c.req("POST", sp+"/batch", `[{"method":"PUT","path":"by/me/a","body":1}]`, bearer(stranger)...)
	if out := batchBody(t, b); st != 403 || out.Error != "no_access" || *out.Index != 0 {
		t.Fatalf("no access: %d %s", st, b)
	}
	_, ownID := own.Token, own.ID
	st, _, b = c.req("POST", sp+"/batch", `[{"method":"PUT","path":"shared/ok","body":1},{"method":"PUT","path":"by/`+ownID+`/k","body":1}]`, bearer(stranger)...)
	if st != 403 {
		t.Fatalf("other's by/: %d %s", st, b)
	}
	unchanged("access")
	if s, _, _ := c.req("POST", sp+"/batch", `[{"method":"PUT","path":"shared/a","body":1}]`); s != 401 {
		t.Fatalf("no token: %d", s)
	}
}

func TestBatchShape(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "")
	sp := "/v1/" + own.Space
	a := bearer(own.Token)
	many := make([]string, 101)
	for i := range many {
		many[i] = fmt.Sprintf(`{"method":"PUT","path":"shared/k%d","body":1}`, i)
	}
	for _, tc := range []struct{ body, code string }{
		{`{"method":"PUT"}`, "bad_batch"},
		{`[]`, "bad_batch"},
		{`not json`, "bad_batch"},
		{"[" + strings.Join(many, ",") + "]", "bad_batch"},
		{`[{"method":"GET","path":"shared/a"}]`, "bad_batch"},
		{`[{"method":"PUT","path":"shared/a","body":1,"id":3}]`, "bad_batch"},
		{`[{"method":"PUT","path":"shared/a","body":1},{"method":"DELETE","path":"shared/a"}]`, "bad_batch"},
		{`[{"method":"PUT","path":"batch","body":[]}]`, "bad_batch"},
		{`[{"method":"PUT","path":"shared/a*","body":1}]`, "method_not_allowed"},
		{`[{"method":"PUT","path":"shared/a/series","body":1}]`, "method_not_allowed"},
		{`[{"method":"PUT","path":"members/x","body":1}]`, "bad_message"},
		{`[{"method":"PUT","path":"shared/a","body":1,"query":{"bogus":1}}]`, "invalid_parameter"},
		{`[{"method":"PUT","path":"shared/a","text":""}]`, "empty_value"},
	} {
		st, _, b := c.req("POST", sp+"/batch", tc.body, a...)
		if st < 400 || errCode(b) != tc.code {
			t.Errorf("%.60s: %d %s", tc.body, st, b)
		}
	}
	if s, _, b := c.req("GET", sp+"/batch", "", a...); s != 405 {
		t.Fatalf("GET batch: %d %s", s, b)
	}
	if _, _, b := c.req("GET", sp+"/shared/*", "", a...); len(listOf(t, b)) != 0 {
		t.Fatalf("shape errors wrote: %s", b)
	}
}

func TestBatchWatchAndLive(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "")
	sp := "/v1/" + own.Space
	a := bearer(own.Token)
	w := c.dial(own.Space, a...)
	w.send(`{"id":"w","method":"GET","path":"shared/*/watch"}`)
	_, from := watchBody(t, w.reply(`"w"`))

	ops := make([]string, 100)
	for i := range ops {
		ops[i] = fmt.Sprintf(`{"method":"PUT","path":"shared/k%03d","body":%d}`, i, i)
	}
	f := w.call(`"b"`, `{"id":"b","method":"POST","path":"batch","body":[`+strings.Join(ops, ",")+`]}`)
	if f.Status != 200 {
		t.Fatalf("live batch: %d %s", f.Status, f.Body)
	}
	// The watch sees the whole batch in one push.
	ch, v := watchBody(t, w.reply(`"w"`))
	if len(ch) != 100 || v != from+100 {
		t.Fatalf("watch push: %d changes, version %d from %d", len(ch), v, from)
	}

	// Errors carry the index on the socket too.
	f = w.call(`"e"`, `{"id":"e","method":"POST","path":"batch","body":[{"method":"PUT","path":"shared/k000","body":1,"query":{"ifVersion":0}}]}`)
	var e struct {
		Error string
		Index int
	}
	json.Unmarshal(f.Body, &e)
	if f.Status != 412 || e.Index != 0 {
		t.Fatalf("live batch error: %d %s", f.Status, f.Body)
	}
	if s, _, b := c.req("GET", sp+"/shared/k099", "", a...); s != 200 || b != "99" {
		t.Fatalf("read back: %d %s", s, b)
	}
}
