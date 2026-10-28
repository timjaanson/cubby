package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func bearer(tok string) []string { return []string{"Authorization", "Bearer " + tok} }

// mint makes an access token as the owner and returns it.
func (c *tc) mint(space, ownerTok, body string) (id, token string) {
	c.t.Helper()
	s, _, b := c.req("POST", "/v1/"+space+"/tokens", body, bearer(ownerTok)...)
	var out struct{ ID, Token string }
	json.Unmarshal([]byte(b), &out)
	if s != 201 || !strings.HasPrefix(out.Token, "at_") {
		c.t.Fatalf("mint %s: %d %s", body, s, b)
	}
	return out.ID, out.Token
}

// join redeems token as tok, or as a new visitor, and returns the visitor token.
func (c *tc) join(space, token, tok string) (int, string, string) {
	c.t.Helper()
	var hdr []string
	if tok != "" {
		hdr = bearer(tok)
	}
	s, _, b := c.req("POST", "/v1/"+space+"/join", `{"token":"`+token+`"}`, hdr...)
	var out struct{ Token string }
	json.Unmarshal([]byte(b), &out)
	if out.Token == "" {
		out.Token = tok
	}
	return s, out.Token, b
}

func TestPrivateByDefault(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "")
	sp := "/v1/" + own.Space
	o := bearer(own.Token)

	// The owner can do everything.
	if s, _, b := c.req("PUT", sp+"/shared/k", `"v"`, o...); s != 201 {
		t.Fatalf("owner write: %d %s", s, b)
	}
	if s, _, b := c.req("GET", sp+"/shared/k?token="+own.Token, ""); s != 200 || b != `"v"` {
		t.Fatalf("owner read with ?token=: %d %s", s, b)
	}

	// To everyone else it's a missing space, on every route, byte for byte.
	stranger, _ := c.anon()
	gone := "/v1/gone-cedar-pilot-mango-frost"
	for _, r := range []struct{ method, path, body string }{
		{"GET", "/shared/k", ""}, {"GET", "/shared/*", ""}, {"GET", "/shared/*/watch?version=0", ""},
		{"PUT", "/shared/k", "x"}, {"GET", "/me/k", ""}, {"GET", "/users/*/k", ""}, {"GET", "/shared/k/series", ""},
		{"GET", "/access", ""}, {"GET", "/tokens", ""}, {"PATCH", "", `{"public":"all"}`}, {"DELETE", "", ""},
	} {
		for _, who := range [][]string{nil, bearer(stranger)} {
			s1, h1, b1 := c.req(r.method, sp+r.path, r.body, who...)
			s2, h2, b2 := c.req(r.method, gone+r.path, r.body, who...)
			if s1 != 404 || errCode(b1) != "no_space" || s1 != s2 || b1 != b2 {
				t.Errorf("%s %s: private %d %s, missing %d %s", r.method, r.path, s1, b1, s2, b2)
			}
			delete(h1, "Date")
			delete(h2, "Date")
			if len(h1) != len(h2) {
				t.Errorf("%s %s: headers differ %v / %v", r.method, r.path, h1, h2)
			}
		}
	}
	if !strings.Contains(errBody(c, sp+"/shared/k"), "hasn't joined") {
		t.Error("no_space message doesn't name both causes")
	}
	// A failed join looks the same for both.
	_, _, b1 := c.join(own.Space, "at_"+strings.Repeat("a", 43), stranger)
	_, _, b2 := c.join("gone-cedar-pilot-mango-frost", "at_"+strings.Repeat("a", 43), stranger)
	if errCode(b1) != "invite_invalid" || b1 != b2 {
		t.Fatalf("join: %s / %s", b1, b2)
	}
}

func errBody(c *tc, path string) string { _, _, b := c.req("GET", path, ""); return b }

func TestPublicSettings(t *testing.T) {
	c := newTest(t)
	c.cfg.Server.RateLimits = new(bool)
	stranger, sid := c.anon()
	cases := []struct {
		public                            string
		read, shared, users, me, response bool
	}{
		{"read", true, false, false, false, true},
		{"respond", true, false, true, true, true},
		{"all", true, true, true, true, true},
	}
	for _, k := range cases {
		own := c.create(`{"public":"`+k.public+`"}`, "")
		sp := "/v1/" + own.Space
		c.req("PUT", sp+"/shared/k", `1`, bearer(own.Token)...)
		if s, _, _ := c.req("GET", sp+"/shared/k", ""); (s == 200) != k.read {
			t.Errorf("%s read: %d", k.public, s)
		}
		want := func(ok bool) int {
			if ok {
				return 201
			}
			return 403
		}
		if s, _, b := c.req("PUT", sp+"/shared/x", `1`, bearer(stranger)...); s != want(k.shared) || (s == 403 && errCode(b) != "no_access") {
			t.Errorf("%s shared write: %d %s", k.public, s, b)
		}
		if s, _, _ := c.req("PUT", sp+"/users/"+sid+"/x", `1`, bearer(stranger)...); s != want(k.users) {
			t.Errorf("%s users write: %d", k.public, s)
		}
		if s, _, _ := c.req("PUT", sp+"/me/x", `1`, bearer(stranger)...); s != want(k.me) {
			t.Errorf("%s me write: %d", k.public, s)
		}
		if _, _, b := c.req("GET", sp+"/access", ""); !strings.Contains(b, `"public":"`+k.public+`"`) || !strings.Contains(b, `"id":null`) {
			t.Errorf("%s access: %s", k.public, b)
		}
	}

	// The owner changes settings; it takes effect at once.
	own := c.create(`{"public":"all"}`, "")
	sp := "/v1/" + own.Space
	if s, _, b := c.req("PATCH", sp, `{"public":"read"}`, bearer(stranger)...); s != 403 || errCode(b) != "owner_only" {
		t.Fatalf("non-owner patch: %d %s", s, b)
	}
	if s, _, b := c.req("PATCH", sp, `{"write":{"me":"granted"}}`, bearer(own.Token)...); s != 200 || !strings.Contains(b, `"public":"custom"`) {
		t.Fatalf("patch: %d %s", s, b)
	}
	if s, _, b := c.req("PATCH", sp, `{"public":"none"}`, bearer(own.Token)...); s != 200 || !strings.Contains(b, `"public":"none"`) {
		t.Fatalf("patch private: %d %s", s, b)
	}
	if s, _, _ := c.req("GET", sp+"/shared/x", ""); s != 404 {
		t.Fatalf("closed space still readable: %d", s)
	}
}

func TestInvites(t *testing.T) {
	c := newTest(t)
	c.cfg.Server.RateLimits = new(bool)
	own := c.create(`{"label":"family"}`, "")
	sp := "/v1/" + own.Space

	// Defaults: member, 10 uses, 7 days.
	s, _, b := c.req("POST", sp+"/tokens", ``, bearer(own.Token)...)
	var mint struct {
		ID, Token string
		Allow     []string
		Uses      *int64
		Until     *int64
		GrantFor  *int64
	}
	json.Unmarshal([]byte(b), &mint)
	week := time.Now().Add(7 * 24 * time.Hour).UnixMilli()
	if s != 201 || len(mint.Allow) != 1 || mint.Allow[0] != "*:rw" || mint.Uses == nil || *mint.Uses != 10 ||
		mint.Until == nil || *mint.Until < week-60_000 || *mint.Until > week+60_000 || mint.GrantFor != nil {
		t.Fatalf("defaults: %d %s", s, b)
	}
	// Ten visitors join, the eleventh can't. Each is a new visitor.
	var members []string
	for i := range 10 {
		s, vt, b := c.join(own.Space, mint.Token, "")
		if s != 201 || !strings.HasPrefix(vt, "vt_") {
			t.Fatalf("join %d: %d %s", i, s, b)
		}
		members = append(members, vt)
	}
	if s, _, b := c.join(own.Space, mint.Token, ""); s != 404 || errCode(b) != "invite_invalid" {
		t.Fatalf("11th: %d %s", s, b)
	}
	// Redeeming again is a no-op for the same visitor, even now it's used up.
	if s, vt, b := c.join(own.Space, mint.Token, members[0]); s != 200 || vt != members[0] {
		t.Fatalf("again: %d %s", s, b)
	}
	// Members read and write.
	m := bearer(members[0])
	if s, _, b := c.req("PUT", sp+"/shared/item.1", `{"text":"milk"}`, m...); s != 201 {
		t.Fatalf("member write: %d %s", s, b)
	}
	if s, _, _ := c.req("GET", sp+"/shared/item.1?token="+members[1], ""); s != 200 {
		t.Fatalf("member read: %d", s)
	}
	// Unknown fields are refused instead of falling back to defaults.
	if s, _, b := c.req("POST", sp+"/tokens", `{"use":1}`, bearer(own.Token)...); s != 400 || !strings.Contains(b, `"parameter":"use"`) {
		t.Fatalf("typo: %d %s", s, b)
	}
	if s, _, b := c.req("POST", sp+"/tokens", `{"allow":"editor"}`, bearer(own.Token)...); s != 400 || errCode(b) != "invalid_scope" {
		t.Fatalf("bad preset: %d %s", s, b)
	}
	// Members invite by default, and see only their own tokens.
	if s, _, b := c.req("POST", sp+"/tokens", `{"label":"from member"}`, m...); s != 201 || !strings.Contains(b, `"allow":["*:rw"]`) {
		t.Fatalf("member mint: %d %s", s, b)
	}
	if _, _, b := c.req("GET", sp+"/tokens", "", m...); strings.Count(b, `"id"`) != 1 || !strings.Contains(b, "from member") {
		t.Fatalf("member list: %s", b)
	}
	// Owner-only routes stay owner-only.
	if s, _, b := c.req("GET", sp+"/grants", "", m...); s != 403 || errCode(b) != "owner_only" {
		t.Fatalf("member grants: %d %s", s, b)
	}

	// The list shows state and grant counts, never token values.
	_, _, b = c.req("GET", sp+"/tokens", "", bearer(own.Token)...)
	if !strings.Contains(b, `"state":"used_up"`) || !strings.Contains(b, `"grants":10`) || strings.Contains(b, "at_") {
		t.Fatalf("list: %s", b)
	}

	// Revoking the token removes its grants.
	if s, _, _ := c.req("DELETE", sp+"/tokens/"+mint.ID, "", bearer(own.Token)...); s != 200 {
		t.Fatal("revoke")
	}
	if s, _, b := c.req("GET", sp+"/shared/item.1", "", m...); s != 404 || errCode(b) != "no_space" {
		t.Fatalf("revoked member: %d %s", s, b)
	}
}

func TestGrantLifetimes(t *testing.T) {
	c := newTest(t)
	c.cfg.Server.RateLimits = new(bool)
	own := c.create(``, "")
	sp := "/v1/" + own.Space

	// The token's window is for joining. The grant outlives it.
	_, short := c.mint(own.Space, own.Token, `{"until":"1s"}`)
	_, vt, _ := c.join(own.Space, short, "")
	// grantFor ends each grant on its own.
	_, brief := c.mint(own.Space, own.Token, `{"grantFor":"1s","uses":null,"until":null}`)
	_, vt2, b := c.join(own.Space, brief, "")
	if !strings.Contains(b, `"until":1`) {
		t.Fatalf("grantFor until: %s", b)
	}
	time.Sleep(1100 * time.Millisecond)
	if s, _, _ := c.join(own.Space, short, ""); s != 404 {
		t.Fatalf("expired token joined: %d", s)
	}
	if s, _, b := c.req("GET", sp+"/shared/*", "", bearer(vt)...); s != 200 {
		t.Fatalf("grant ended with its token: %d %s", s, b)
	}
	if s, _, _ := c.req("GET", sp+"/shared/*", "", bearer(vt2)...); s != 404 {
		t.Fatalf("grant past grantFor still works: %d", s)
	}
	// The unlimited token still lets new people in.
	if s, _, _ := c.join(own.Space, brief, ""); s != 201 {
		t.Fatalf("unlimited token: %d", s)
	}
	// Cleanup removes the ended grant and keeps the live one.
	if _, err := c.store.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := c.req("GET", sp+"/shared/*", "", bearer(vt)...); s != 200 {
		t.Fatal("cleanup removed a live grant")
	}
}

func TestStackingAndScopes(t *testing.T) {
	c := newTest(t)
	c.cfg.Server.RateLimits = new(bool)
	own := c.create(`{"public":"read"}`, "")
	sp := "/v1/" + own.Space
	vt, id := c.anon()
	idPoll, poll := c.mint(own.Space, own.Token, `{"allow":["shared/poll.*:rw"]}`)
	_, notes := c.mint(own.Space, own.Token, `{"allow":["shared/notes.*:w"]}`)
	c.join(own.Space, poll, vt)
	c.join(own.Space, notes, vt)
	a := bearer(vt)
	for key, want := range map[string]int{"poll.q": 201, "notes.1": 201, "other": 403} {
		if s, _, b := c.req("PUT", sp+"/shared/"+key, `1`, a...); s != want {
			t.Errorf("%s: %d %s", key, s, b)
		}
	}
	_, _, b := c.req("GET", sp+"/access", "", a...)
	if strings.Count(b, `"source":"t_`) != 2 {
		t.Fatalf("access: %s", b)
	}
	// Revoking one token leaves the other grant.
	c.req("DELETE", sp+"/tokens/"+idPoll, "", bearer(own.Token)...)
	if s, _, _ := c.req("PUT", sp+"/shared/poll.q", `2`, a...); s != 403 {
		t.Fatalf("revoked scope: %d", s)
	}
	if s, _, _ := c.req("PUT", sp+"/shared/notes.2", `2`, a...); s != 201 {
		t.Fatalf("other grant gone: %d", s)
	}

	// Direct grants, and removing everything a visitor holds.
	if s, _, b := c.req("POST", sp+"/grants", `{"user":"`+id+`","allow":"member"}`, bearer(own.Token)...); s != 201 {
		t.Fatalf("direct grant: %d %s", s, b)
	}
	if s, _, _ := c.req("PUT", sp+"/shared/other", `1`, a...); s != 201 {
		t.Fatalf("direct grant didn't apply: %d", s)
	}
	if s, _, b := c.req("POST", sp+"/grants", `{"user":"u_aaaaaaaaaaaaaaaa"}`, bearer(own.Token)...); s != 404 {
		t.Fatalf("grant to nobody: %d %s", s, b)
	}
	if s, _, b := c.req("DELETE", sp+"/grants?user="+id, "", bearer(own.Token)...); s != 200 || !strings.Contains(b, `"removed":2`) {
		t.Fatalf("remove user: %d %s", s, b)
	}
	if s, _, _ := c.req("PUT", sp+"/shared/notes.3", `3`, a...); s != 403 {
		t.Fatalf("after removing all: %d", s)
	}
	// me/ in a space with granted writes needs a scope too.
	if s, _, b := c.req("PUT", sp+"/me/x", `1`, a...); s != 403 || errCode(b) != "no_access" {
		t.Fatalf("me/ without scope: %d %s", s, b)
	}
}

func TestConcurrentRedeem(t *testing.T) {
	c := newTest(t)
	c.cfg.Server.RateLimits = new(bool)
	own := c.create(``, "")
	vt, _ := c.anon()
	_, tok := c.mint(own.Space, own.Token, `{"uses":2}`)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); c.join(own.Space, tok, vt) }()
	}
	wg.Wait()
	_, _, b := c.req("GET", "/v1/"+own.Space+"/tokens", "", bearer(own.Token)...)
	if !strings.Contains(b, `"usesLeft":1`) || !strings.Contains(b, `"grants":1`) {
		t.Fatalf("double redeem: %s", b)
	}
}

func TestOwnerToken(t *testing.T) {
	c := newTest(t)
	c.cfg.Server.RateLimits = new(bool)
	own := c.create(``, "")
	sp := "/v1/" + own.Space

	// A second device becomes an owner with the owner token.
	s, dev, b := c.join(own.Space, own.Owner, "")
	if s != 201 || !strings.Contains(b, `"owner":true`) {
		t.Fatalf("owner join: %d %s", s, b)
	}
	if s, _, _ := c.req("POST", sp+"/tokens", ``, bearer(dev)...); s != 201 {
		t.Fatal("second owner can't mint")
	}
	// Rotating keeps the caller and the creator, drops grants from the old token.
	s, _, b = c.req("POST", sp+"/owner-token", "", bearer(own.Token)...)
	var rot struct{ Owner string }
	json.Unmarshal([]byte(b), &rot)
	if s != 200 || !strings.HasPrefix(rot.Owner, "ot_") {
		t.Fatalf("rotate: %d %s", s, b)
	}
	if s, _, _ := c.req("POST", sp+"/tokens", ``, bearer(dev)...); s != 404 {
		t.Fatalf("old owner-token grant survived: %d", s)
	}
	if s, _, _ := c.join(own.Space, own.Owner, ""); s != 404 {
		t.Fatal("old owner token still works")
	}
	if s, _, _ := c.join(own.Space, rot.Owner, dev); s != 200 {
		t.Fatal("new owner token doesn't work")
	}
	// The operator can issue a fresh one with no owner around.
	srv := New(c.cfg, c.store, nil)
	rec := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/spaces/"+own.Space+"/owner-token", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"owner":"ot_`) {
		t.Fatalf("admin owner-token: %d %s", rec.Code, rec.Body)
	}
	if s, _, _ := c.join(own.Space, rot.Owner, ""); s != 404 {
		t.Fatal("rotated-away token still works after admin reissue")
	}
	// The owner deletes the space.
	if s, _, _ := c.req("DELETE", sp, "", bearer(own.Token)...); s != 200 {
		t.Fatal("delete")
	}
	if s, _, _ := c.req("GET", sp+"/access", "", bearer(own.Token)...); s != 404 {
		t.Fatal("deleted space still there")
	}
	_ = http.MethodGet
}

// Browsers preflight PATCH, used for settings and token changes.
func TestPreflightAllowsPatch(t *testing.T) {
	c := newTest(t)
	_, h, _ := c.req("OPTIONS", "/v1/family-cedar-pilot-mango", "")
	if !strings.Contains(h.Get("Access-Control-Allow-Methods"), "PATCH") {
		t.Fatalf("preflight: %q", h.Get("Access-Control-Allow-Methods"))
	}
}

// A visitor created by join counts against the same per-IP limit as /anon.
func TestJoinCreatesVisitorsWithinLimit(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "")
	_, tok := c.mint(own.Space, own.Token, `{"uses":null}`)
	ip := []string{"X-Forwarded-For", "203.0.113.50"}
	n := 0
	for range 40 {
		s, _, _ := c.req("POST", "/v1/"+own.Space+"/join", `{"token":"`+tok+`"}`, ip...)
		if s == 429 {
			break
		}
		n++
	}
	if n != 30 {
		t.Fatalf("visitors per IP through join: %d", n)
	}
}
