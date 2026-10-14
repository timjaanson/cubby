package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type tc struct {
	t   *testing.T
	url string
}

func newTest(t *testing.T) *tc {
	cfg := defaultConfig()
	cfg.Server.DataDir = t.TempDir()
	cfg2, _ := loadConfig("")
	cfg.trusted = cfg2.trusted
	store, err := openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(newServer(cfg, store, nil))
	t.Cleanup(func() { ts.Close(); store.Close() })
	return &tc{t: t, url: ts.URL}
}

func (c *tc) req(method, path, body string, hdr ...string) (int, http.Header, string) {
	c.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r, _ := http.NewRequest(method, c.url+path, rd)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

func (c *tc) anon() (token, id string) {
	_, _, b := c.req("POST", "/v1/anon", "")
	var out struct{ Token, ID string }
	json.Unmarshal([]byte(b), &out)
	if out.Token == "" {
		c.t.Fatalf("no token: %s", b)
	}
	return out.Token, out.ID
}

func errCode(b string) string {
	var e struct{ Error string }
	json.Unmarshal([]byte(b), &e)
	return e.Error
}

func TestNames(t *testing.T) {
	for name, want := range map[string]bool{
		"poll-k7m2x9q4": true, "k7m2x9q4": true, "team-lunch-poll-9d24vw6e": true,
		"poll": false, "poll-7f3a": false, "poll-12345678": false, "poll-chatroom": false, "Poll-k7m2x9q4": false,
		"anon": false, "spaces": false, "whoami": false,
	} {
		if validSpace(name) != want {
			t.Errorf("validSpace(%q) = %v", name, !want)
		}
	}
	for range 100 {
		if n := newSpaceName("My Poll!"); !validSpace(n) || !strings.HasPrefix(n, "my-poll-") {
			t.Fatalf("bad generated name %q", n)
		}
	}
	if !validVisitor(newVisitorID()) {
		t.Fatal("bad visitor id")
	}
}

func TestReadWriteDelete(t *testing.T) {
	c := newTest(t)
	tok, _ := c.anon()
	auth := []string{"Authorization", "Bearer " + tok}
	sp := "/v1/poll-k7m2x9q4"

	if s, _, b := c.req("GET", sp+"/shared/question", ""); s != 404 || errCode(b) != "not_found" {
		t.Fatalf("missing read: %d %s", s, b)
	}
	if s, _, b := c.req("PUT", sp+"/shared/question", "hi"); s != 401 || errCode(b) != "missing_token" {
		t.Fatalf("write without token: %d %s", s, b)
	}
	s, h, b := c.req("PUT", sp+"/shared/question", "Pizza or tacos?", auth...)
	if s != 201 {
		t.Fatalf("create: %d %s", s, b)
	}
	v1 := h.Get("ETag")

	s, h, b = c.req("GET", sp+"/shared/question", "")
	if s != 200 || b != "Pizza or tacos?" || !strings.HasPrefix(h.Get("Content-Type"), "text/plain") || h.Get("ETag") != v1 {
		t.Fatalf("read: %d %q %v", s, b, h)
	}
	if h.Get("Content-Security-Policy") == "" || h.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("missing security or CORS headers")
	}
	if s, _, _ := c.req("GET", sp+"/shared/question", "", "If-None-Match", v1); s != 304 {
		t.Fatalf("If-None-Match: %d", s)
	}

	// Same bytes: no new version.
	_, h, _ = c.req("PUT", sp+"/shared/question", "Pizza or tacos?", auth...)
	if h.Get("ETag") != v1 {
		t.Fatal("identical write made a new version")
	}

	// JSON is detected and returned unchanged.
	s, _, _ = c.req("PUT", sp+"/shared/poll", `{"a": [1, 2]}`, auth...)
	_, h, b = c.req("GET", sp+"/shared/poll", "")
	if !strings.HasPrefix(h.Get("Content-Type"), "application/json") || b != `{"a": [1, 2]}` {
		t.Fatalf("json: %s %q", h.Get("Content-Type"), b)
	}

	// Conditional writes.
	if s, _, b := c.req("PUT", sp+"/shared/question?ifVersion=0", "x", auth...); s != 412 || errCode(b) != "version_conflict" {
		t.Fatalf("ifVersion=0 on existing: %d %s", s, b)
	}
	if s, _, b := c.req("PUT", sp+"/shared/question", "y", append(auth, "If-Match", v1)...); s != 200 {
		t.Fatalf("If-Match: %d %s", s, b)
	}

	if s, _, b := c.req("PUT", sp+"/shared/empty", "", auth...); s != 400 || errCode(b) != "empty_value" {
		t.Fatalf("empty: %d %s", s, b)
	}
	if s, _, b := c.req("PUT", sp+"/shared/big", strings.Repeat("x", 64<<10+1), auth...); s != 413 {
		t.Fatalf("big: %d %s", s, b)
	}
	deep := strings.Repeat("[", 65) + strings.Repeat("]", 65)
	if s, _, b := c.req("PUT", sp+"/shared/deep", deep, auth...); s != 400 || errCode(b) != "json_too_deep" {
		t.Fatalf("deep: %d %s", s, b)
	}
	if s, _, b := c.req("PUT", sp+"/shared/a%2Fb", "x", auth...); s != 400 || errCode(b) != "invalid_key" {
		t.Fatalf("slash key: %d %s", s, b)
	}
	if s, _, b := c.req("GET", "/v1/poll/shared/x", ""); s != 400 || errCode(b) != "invalid_space" || !strings.Contains(b, "suggestion") {
		t.Fatalf("bad space: %d %s", s, b)
	}

	if s, _, _ := c.req("DELETE", sp+"/shared/question", "", auth...); s != 200 {
		t.Fatalf("delete: %d", s)
	}
	if s, _, _ := c.req("DELETE", sp+"/shared/question", "", auth...); s != 404 {
		t.Fatalf("delete again: %d", s)
	}
}

func TestAreas(t *testing.T) {
	c := newTest(t)
	tokA, idA := c.anon()
	tokB, idB := c.anon()
	a := []string{"Authorization", "Bearer " + tokA}
	b := []string{"Authorization", "Bearer " + tokB}
	sp := "/v1/poll-k7m2x9q4"

	c.req("PUT", sp+"/users/"+idA+"/vote", `"tacos"`, a...)
	c.req("PUT", sp+"/users/"+idB+"/vote", `"pizza"`, b...)
	if s, _, body := c.req("PUT", sp+"/users/"+idA+"/vote", `"x"`, b...); s != 403 || errCode(body) != "not_owner" {
		t.Fatalf("not_owner: %d %s", s, body)
	}
	_, _, body := c.req("GET", sp+"/users/*/vote", "")
	var l struct {
		Items []struct{ Key, User string }
		Next  *string
	}
	json.Unmarshal([]byte(body), &l)
	if len(l.Items) != 2 || l.Next != nil {
		t.Fatalf("users/*: %s", body)
	}
	if s, _, _ := c.req("PUT", sp+"/users/*/vote", "x", a...); s != 405 {
		t.Fatalf("PUT users/*: %d", s)
	}
	// Pagination across users.
	_, _, body = c.req("GET", sp+"/users/*/vote?limit=1", "")
	json.Unmarshal([]byte(body), &l)
	if len(l.Items) != 1 || l.Next == nil {
		t.Fatalf("page 1: %s", body)
	}
	_, _, body = c.req("GET", sp+"/users/*/vote?limit=1&after="+*l.Next, "")
	first := l.Items[0].User
	json.Unmarshal([]byte(body), &l)
	if len(l.Items) != 1 || l.Items[0].User == first {
		t.Fatalf("page 2: %s", body)
	}

	// me/ is private.
	c.req("PUT", sp+"/me/draft", "secret", a...)
	if _, _, body := c.req("GET", sp+"/me/draft", "", b...); !strings.Contains(body, "not_found") {
		t.Fatalf("B sees A's me/: %s", body)
	}
	if _, _, body := c.req("GET", sp+"/me/draft", "", a...); body != "secret" {
		t.Fatalf("A's me/: %s", body)
	}
	if s, _, _ := c.req("GET", sp+"/me/draft", ""); s != 401 {
		t.Fatalf("me/ without token: %d", s)
	}
	if s, _, body := c.req("GET", sp+"/me/draft", "", "Authorization", "Bearer kv_nope"); s != 401 || errCode(body) != "invalid_token" {
		t.Fatalf("bad token: %d %s", s, body)
	}
	if _, _, body := c.req("GET", "/v1/whoami", "", a...); !strings.Contains(body, idA) {
		t.Fatalf("whoami: %s", body)
	}
}

func TestLockedSpace(t *testing.T) {
	c := newTest(t)
	tok, id := c.anon()
	a := []string{"Authorization", "Bearer " + tok}
	s, _, body := c.req("POST", "/v1/spaces?label=poll&locked", "", a...)
	var out struct{ Space, Key string }
	json.Unmarshal([]byte(body), &out)
	if s != 201 || !strings.HasPrefix(out.Space, "poll-") || !strings.HasPrefix(out.Key, "sk_") {
		t.Fatalf("create locked: %d %s", s, body)
	}
	sp := "/v1/" + out.Space
	if s, _, body := c.req("PUT", sp+"/shared/q", "x", a...); s != 403 || errCode(body) != "space_locked" {
		t.Fatalf("locked write: %d %s", s, body)
	}
	if s, _, _ := c.req("PUT", sp+"/shared/q", "x", append(a, "Space-Key", out.Key)...); s != 201 {
		t.Fatalf("keyed write: %d", s)
	}
	if s, _, _ := c.req("PUT", sp+"/users/"+id+"/vote", "x", a...); s != 201 {
		t.Fatalf("users/ in locked space: %d", s)
	}
}

func TestListAndWatch(t *testing.T) {
	c := newTest(t)
	tok, _ := c.anon()
	a := []string{"Authorization", "Bearer " + tok}
	sp := "/v1/chat-k7m2x9q4"

	_, _, body := c.req("GET", sp+"/shared/msg.*", "")
	if !strings.Contains(body, `"items":[]`) {
		t.Fatalf("empty list: %s", body)
	}
	for _, k := range []string{"msg.1", "msg.2", "msg.3", "other"} {
		c.req("PUT", sp+"/shared/"+k, `"`+k+`"`, a...)
	}
	_, _, body = c.req("GET", sp+"/shared/msg.*?order=desc", "")
	var l struct {
		Items   []struct{ Key string }
		Version int64
	}
	json.Unmarshal([]byte(body), &l)
	if len(l.Items) != 3 || l.Items[0].Key != "msg.3" {
		t.Fatalf("list: %s", body)
	}

	// Snapshot from 0.
	_, _, body = c.req("GET", sp+"/shared/msg.*/watch?version=0", "")
	var w struct {
		Changes []struct {
			Key     string
			Deleted bool
			Version int64
		}
		Version int64
	}
	json.Unmarshal([]byte(body), &w)
	if len(w.Changes) != 3 {
		t.Fatalf("snapshot: %s", body)
	}

	// Long poll wakes on a write.
	done := make(chan string)
	go func() {
		_, _, b := c.req("GET", sp+"/shared/msg.*/watch?timeout=5&version="+itoa(w.Version), "")
		done <- b
	}()
	time.Sleep(200 * time.Millisecond)
	c.req("DELETE", sp+"/shared/msg.1", "", a...)
	select {
	case b := <-done:
		json.Unmarshal([]byte(b), &w)
		if len(w.Changes) != 1 || !w.Changes[0].Deleted || w.Changes[0].Key != "msg.1" {
			t.Fatalf("watch change: %s", b)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch didn't wake")
	}

	// Timeout returns empty changes.
	_, _, body = c.req("GET", sp+"/shared/msg.*/watch?timeout=1", "")
	if !strings.Contains(body, `"changes":[]`) {
		t.Fatalf("timeout: %s", body)
	}
	// A version from the future is expired.
	if s, _, _ := c.req("GET", sp+"/shared/msg.*/watch?version="+itoa(w.Version+1000), ""); s != 410 {
		t.Fatalf("future version: %d", s)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
