package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timjaanson/cubby/internal/config"
	"github.com/timjaanson/cubby/internal/names"
	"github.com/timjaanson/cubby/internal/store"
)

type tc struct {
	t     *testing.T
	url   string
	store *store.Store
	cfg   *config.Config
}

func newTest(t *testing.T) *tc {
	cfg := config.Default()
	cfg.Server.DataDir = t.TempDir()
	cfg2, _ := config.Load("")
	cfg.Trusted = cfg2.Trusted
	store, err := store.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(cfg, store, nil))
	t.Cleanup(func() { ts.Close(); store.Close() })
	return &tc{t: t, url: ts.URL, store: store, cfg: cfg}
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

// open creates a space under name with public "all", owned by the visitor
// with tok, and returns tok. With tok "" it creates the visitor too.
func (c *tc) open(name, tok string) string {
	c.t.Helper()
	return c.create(`{"name":"`+name+`","public":"all"}`, tok).Token
}

type created struct {
	Space, Owner, ID, Token string
}

// create posts body to /v1/spaces as tok, or as a new visitor.
func (c *tc) create(body, tok string) created {
	c.t.Helper()
	var hdr []string
	if tok != "" {
		hdr = []string{"Authorization", "Bearer " + tok}
	}
	s, _, b := c.req("POST", "/v1/spaces", body, hdr...)
	var out created
	json.Unmarshal([]byte(b), &out)
	if s != 201 {
		c.t.Fatalf("create %s: %d %s", body, s, b)
	}
	if out.Token == "" {
		out.Token = tok
	}
	return out
}

func errCode(b string) string {
	var e struct{ Error string }
	json.Unmarshal([]byte(b), &e)
	return e.Error
}

func TestNames(t *testing.T) {
	for name, want := range map[string]bool{
		"poll-cedar-pilot-mango": true, "cedar-pilot-mango-frost": true, "team-lunch-poll-9d24vw6e": true,
		"poll": false, "poll-7f3a": false, "poll-12345678": false, "poll-chatroom": false, "Poll-cedar-pilot-mango": false,
		"anon": false, "spaces": false, "whoami": false,
	} {
		if names.ValidSpace(name) != want {
			t.Errorf("validSpace(%q) = %v", name, !want)
		}
	}
	for range 100 {
		if n := names.NewSpaceName("My Poll!"); !names.ValidSpace(n) || !strings.HasPrefix(n, "my-poll-") {
			t.Fatalf("bad generated name %q", n)
		}
	}
	if !names.ValidVisitor(names.NewVisitorID()) {
		t.Fatal("bad visitor id")
	}
}

func TestReadWriteDelete(t *testing.T) {
	c := newTest(t)
	tok := c.open("poll-cedar-pilot-mango", "")
	auth := []string{"Authorization", "Bearer " + tok}
	sp := "/v1/poll-cedar-pilot-mango"

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
	c.open("poll-cedar-pilot-mango", tokA)
	a := []string{"Authorization", "Bearer " + tokA}
	b := []string{"Authorization", "Bearer " + tokB}
	sp := "/v1/poll-cedar-pilot-mango"

	c.req("PUT", sp+"/users/"+idA+"/vote", `"tacos"`, a...)
	c.req("PUT", sp+"/users/"+idB+"/vote", `"pizza"`, b...)
	if s, _, body := c.req("PUT", sp+"/users/"+idA+"/vote", `"x"`, b...); s != 403 || errCode(body) != "not_yours" {
		t.Fatalf("not_yours: %d %s", s, body)
	}
	// The owner (A) may delete B's entry, as moderation, but not write it.
	if s, _, body := c.req("PUT", sp+"/users/"+idB+"/vote", `"x"`, a...); s != 403 || errCode(body) != "not_yours" {
		t.Fatalf("owner writing someone's entry: %d %s", s, body)
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
	if s, _, body := c.req("GET", sp+"/me/draft", "", "Authorization", "Bearer vt_nope"); s != 401 || errCode(body) != "invalid_token" {
		t.Fatalf("bad token: %d %s", s, body)
	}
	if _, _, body := c.req("GET", "/v1/whoami", "", a...); !strings.Contains(body, idA) {
		t.Fatalf("whoami: %s", body)
	}
	// Moderation delete, last, since it changes B's vote.
	if s, _, body := c.req("DELETE", sp+"/users/"+idB+"/vote", "", a...); s != 200 {
		t.Fatalf("owner delete: %d %s", s, body)
	}
}

func TestListAndWatch(t *testing.T) {
	c := newTest(t)
	tok := c.open("chat-cedar-pilot-mango", "")
	a := []string{"Authorization", "Bearer " + tok}
	sp := "/v1/chat-cedar-pilot-mango"

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

	// A snapshot of nothing returns at once.
	c.open("none-cedar-pilot-mango", tok)
	start := time.Now()
	_, _, body = c.req("GET", "/v1/none-cedar-pilot-mango/shared/*/watch?version=0", "")
	if time.Since(start) > time.Second || !strings.Contains(body, `"changes":[]`) {
		t.Fatalf("empty snapshot: %s after %s", body, time.Since(start))
	}
	// Watching a space that doesn't exist is no_space.
	if s, _, body := c.req("GET", "/v1/gone-cedar-pilot-mango/shared/*/watch?version=0", ""); s != 404 || errCode(body) != "no_space" {
		t.Fatalf("missing space watch: %d %s", s, body)
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

func TestSpaceFiles(t *testing.T) {
	c := newTest(t)
	tok, _ := c.anon()
	a := []string{"Authorization", "Bearer " + tok}
	sp := "chat-cedar-pilot-mango"
	file := filepath.Join(c.cfg.Server.DataDir, "spaces", sp+".db")

	// Neither reads nor writes create a space. Only POST /v1/spaces does.
	c.req("GET", "/v1/"+sp+"/shared/x", "")
	if s, _, body := c.req("PUT", "/v1/"+sp+"/shared/x", "1", a...); s != 404 || errCode(body) != "no_space" {
		t.Fatalf("write to a missing space: %d %s", s, body)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("a read or write created a space file")
	}
	c.open(sp, tok)
	if _, err := os.Stat(file); err != nil {
		t.Fatal("creating didn't make the space file")
	}
	c.req("PUT", "/v1/"+sp+"/shared/x", "1", a...)
	_, _, body := c.req("GET", "/v1/"+sp+"/shared/*/watch?version=0", "")
	var w struct{ Version int64 }
	json.Unmarshal([]byte(body), &w)

	// Deleting the space wakes watchers with no_space, and removes the file.
	done := make(chan int)
	go func() {
		s, _, _ := c.req("GET", fmt.Sprintf("/v1/%s/shared/*/watch?timeout=5&version=%d", sp, w.Version), "")
		done <- s
	}()
	time.Sleep(200 * time.Millisecond)
	if err := c.store.DeleteSpace(sp); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-done:
		if s != 404 {
			t.Fatalf("watch after delete: %d", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch didn't wake on delete")
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("file still there")
	}

	// A recreated space continues above the old counter, and old watchers start over.
	c.open(sp, tok)
	_, h, _ := c.req("PUT", "/v1/"+sp+"/shared/x", "2", a...)
	if v := parseInt(h.Get("ETag")); v <= w.Version {
		t.Fatalf("recreated version %d not above %d", v, w.Version)
	}
	if s, _, _ := c.req("GET", fmt.Sprintf("/v1/%s/shared/*/watch?version=%d", sp, w.Version), ""); s != 410 {
		t.Fatalf("old version on recreated space: %d", s)
	}
}

func TestCopiedSpaceFile(t *testing.T) {
	src := newTest(t)
	sp := "moved-cedar-pilot-mango"
	tok := src.open(sp, "")
	src.req("PUT", "/v1/"+sp+"/shared/greeting", "hello", "Authorization", "Bearer "+tok)
	src.store.Close()

	dst := newTest(t)
	b, err := os.ReadFile(filepath.Join(src.cfg.Server.DataDir, "spaces", sp+".db"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dst.cfg.Server.DataDir, "spaces", sp+".db"), b, 0o600)
	if _, _, body := dst.req("GET", "/v1/"+sp+"/shared/greeting", ""); body != "hello" {
		t.Fatalf("copied space: %q", body)
	}
	found := false
	for _, r := range dst.store.Spaces() {
		found = found || (r.Name == sp && r.Keys == 1)
	}
	if !found {
		t.Fatal("copied space not listed")
	}
}

func TestVisitorQuotaAcrossSpaces(t *testing.T) {
	c := newTest(t)
	c.cfg.Limits.MaxVisitorBytes = 100
	tok, _ := c.anon()
	a := []string{"Authorization", "Bearer " + tok}
	v := strings.Repeat("x", 40)
	for _, n := range []string{"one", "two", "three"} {
		c.open(n+"-cedar-pilot-mango", tok)
	}
	c.req("PUT", "/v1/one-cedar-pilot-mango/shared/k", v, a...)
	c.store.ForgetVisitorBytes("") // force a recount from the space files
	c.req("PUT", "/v1/two-cedar-pilot-mango/shared/k", v, a...)
	if s, _, body := c.req("PUT", "/v1/three-cedar-pilot-mango/shared/k", v, a...); s != 507 || errCode(body) != "visitor_quota_exceeded" {
		t.Fatalf("quota: %d %s", s, body)
	}
}

func TestManySpacesEvict(t *testing.T) {
	c := newTest(t)
	c.cfg.Server.RateLimits = new(bool)
	tok, _ := c.anon()
	a := []string{"Authorization", "Bearer " + tok}
	for i := range store.MaxOpenSpaces + 20 {
		c.open(fmt.Sprintf("s%03d-cedar-pilot-mango", i), tok)
		c.req("PUT", fmt.Sprintf("/v1/s%03d-cedar-pilot-mango/shared/k", i), "v", a...)
	}
	open := c.store.OpenSpaces()
	if open > store.MaxOpenSpaces {
		t.Fatalf("%d files open", open)
	}
	if _, _, body := c.req("GET", "/v1/s000-cedar-pilot-mango/shared/k", ""); body != "v" {
		t.Fatalf("evicted space: %q", body)
	}
}

func parseInt(s string) int64 {
	var n int64
	fmt.Sscan(strings.Trim(s, `"`), &n)
	return n
}

func TestCreateSpace(t *testing.T) {
	c := newTest(t)
	tok, _ := c.anon()
	a := []string{"Authorization", "Bearer " + tok}

	if st, _, body := c.req("POST", "/v1/spaces", `{"name":"throws-amber-otter-tundra"}`, a...); st != 201 || !strings.Contains(body, `"space":"throws-amber-otter-tundra"`) || !strings.Contains(body, `"owner":"ot_`) {
		t.Fatalf("create by name: %d %s", st, body)
	}
	if st, _, body := c.req("POST", "/v1/spaces", `{"name":"throws-amber-otter-tundra"}`, a...); st != 409 || errCode(body) != "space_exists" {
		t.Fatalf("taken name: %d %s", st, body)
	}
	if st, _, body := c.req("POST", "/v1/spaces", `{"name":"throws"}`, a...); st != 400 || errCode(body) != "invalid_space" {
		t.Fatalf("bad name: %d %s", st, body)
	}
	if st, _, body := c.req("POST", "/v1/spaces", `{"name":"throws-amber-otter-tundra","label":"x"}`, a...); st != 400 || errCode(body) != "invalid_parameter" {
		t.Fatalf("name and label: %d %s", st, body)
	}
	if st, _, body := c.req("POST", "/v1/spaces", `{"lable":"x"}`, a...); st != 400 || errCode(body) != "invalid_parameter" || !strings.Contains(body, `"parameter":"lable"`) {
		t.Fatalf("unknown field: %d %s", st, body)
	}
	if st, _, body := c.req("POST", "/v1/spaces", `{"public":"read","read":"anyone"}`, a...); st != 400 || errCode(body) != "invalid_settings" {
		t.Fatalf("public and read: %d %s", st, body)
	}
	if st, _, body := c.req("POST", "/v1/spaces", `{"write":"anyone"}`, a...); st != 400 || errCode(body) != "invalid_settings" {
		t.Fatalf("open writes into a private space: %d %s", st, body)
	}
	if st, _, body := c.req("POST", "/v1/spaces?label=x", ``, a...); st != 400 || errCode(body) != "invalid_parameter" {
		t.Fatalf("query parameter: %d %s", st, body)
	}

	// An empty body: a generated name, private, owned by the caller.
	st, _, body := c.req("POST", "/v1/spaces", ``, a...)
	var out struct {
		Space, Owner, ID, Token string
		Settings                struct{ Public string }
	}
	json.Unmarshal([]byte(body), &out)
	if st != 201 || !names.ValidSpace(out.Space) || out.Settings.Public != "none" || out.Token != "" {
		t.Fatalf("empty body: %d %s", st, body)
	}

	// Without a token, the creator is a new visitor and gets its token.
	st, _, body = c.req("POST", "/v1/spaces", `{"label":"poll","public":"respond"}`)
	json.Unmarshal([]byte(body), &out)
	if st != 201 || !strings.HasPrefix(out.Space, "poll-") || !strings.HasPrefix(out.Token, "vt_") || out.Settings.Public != "respond" {
		t.Fatalf("create without a token: %d %s", st, body)
	}
	// That token works, and its visitor owns the space.
	if _, _, body := c.req("GET", "/v1/"+out.Space+"/access", "", "Authorization", "Bearer "+out.Token); !strings.Contains(body, `"owner":true`) {
		t.Fatalf("creator isn't owner: %s", body)
	}
}

// Unknown routes, the root and /v1 itself all give the same bare 404, with no
// hints about the API.
func TestQuietRoutes(t *testing.T) {
	c := newTest(t)
	var first string
	for _, p := range []string{"/", "/v1", "/v1/", "/v1/x", "/v1/x/y", "/nope", "/v2/a/b/c",
		"/v1/cedar-pilot-mango-frost/other/k", "/v1/cedar-pilot-mango-frost/users/k"} {
		s, _, b := c.req("GET", p, "")
		if s != 404 || errCode(b) != "not_found" {
			t.Fatalf("%s: %d %s", p, s, b)
		}
		if first == "" {
			first = b
		} else if b != first {
			t.Errorf("%s: body %s differs from %s", p, b, first)
		}
		for _, leak := range []string{"/v1", "shared", "kv.js", "health", "cubby"} {
			if strings.Contains(b, leak) {
				t.Errorf("%s: body mentions %q: %s", p, leak, b)
			}
		}
	}
	s, _, b := c.req("GET", "/health", "")
	var h struct {
		OK      bool
		Version string
	}
	json.Unmarshal([]byte(b), &h)
	if s != 200 || !h.OK || h.Version == "" {
		t.Fatalf("health: %d %s", s, b)
	}
}

// Every entry says which visitor last wrote it, and a delete says who deleted.
func TestBy(t *testing.T) {
	c := newTest(t)
	tokA, idA := c.anon()
	tokB, idB := c.anon()
	c.open("game-cedar-pilot-mango", tokA)
	a := []string{"Authorization", "Bearer " + tokA}
	b := []string{"Authorization", "Bearer " + tokB}
	sp := "/v1/game-cedar-pilot-mango"

	c.req("PUT", sp+"/shared/game.1", `{"hole":1}`, a...)
	if _, h, _ := c.req("GET", sp+"/shared/game.1", ""); h.Get("KV-By") != idA {
		t.Fatalf("KV-By: %q, want %q", h.Get("KV-By"), idA)
	}
	if _, h, _ := c.req("GET", sp+"/shared/game.1", ""); !strings.Contains(h.Get("Access-Control-Expose-Headers"), "KV-By") {
		t.Fatal("KV-By isn't exposed to browsers")
	}

	_, _, body := c.req("GET", sp+"/shared/*/watch?version=0", "")
	var w struct {
		Changes []struct {
			Key, By string
			Deleted bool
		}
		Version int64
	}
	json.Unmarshal([]byte(body), &w)
	if len(w.Changes) != 1 || w.Changes[0].By != idA {
		t.Fatalf("snapshot by: %s", body)
	}

	// B moves the hole: the change and the list name B.
	c.req("PUT", sp+"/shared/game.1", `{"hole":2}`, b...)
	_, _, body = c.req("GET", sp+"/shared/*", "")
	if !strings.Contains(body, `"by":"`+idB+`"`) {
		t.Fatalf("list by: %s", body)
	}

	// B deletes A's key: the tombstone names B.
	v := w.Version
	c.req("DELETE", sp+"/shared/game.1", "", b...)
	_, _, body = c.req("GET", sp+"/shared/*/watch?version="+itoa(v), "")
	json.Unmarshal([]byte(body), &w)
	if len(w.Changes) != 1 || !w.Changes[0].Deleted || w.Changes[0].By != idB {
		t.Fatalf("delete by: %s", body)
	}

	// users/*/ entries carry by as well as user.
	c.req("PUT", sp+"/users/"+idA+"/name", `"Ann"`, a...)
	_, _, body = c.req("GET", sp+"/users/*/name", "")
	if !strings.Contains(body, `"user":"`+idA+`","by":"`+idA+`"`) {
		t.Fatalf("users by: %s", body)
	}
}
