package server

import (
	"encoding/json"
	"github.com/timjaanson/cubby/internal/auth"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func bearer(tok string) []string { return []string{"Authorization", "Bearer " + tok} }

// mint makes an invite as the owner and returns it.
func (c *tc) mint(space, ownerTok, body string) (id, invite string) {
	c.t.Helper()
	s, _, b := c.req("POST", "/v1/"+space+"/invites", body, bearer(ownerTok)...)
	var out struct{ ID, Invite string }
	json.Unmarshal([]byte(b), &out)
	if s != 201 || !strings.HasPrefix(out.Invite, "inv_") {
		c.t.Fatalf("mint %s: %d %s", body, s, b)
	}
	return out.ID, out.Invite
}

// join redeems invite as tok, or as a new visitor, and returns that
// visitor's token.
func (c *tc) join(space, invite, tok string) (int, string, string) {
	c.t.Helper()
	if tok == "" {
		tok, _ = c.anon()
	}
	s, _, b := c.req("POST", "/v1/"+space+"/join", `{"invite":"`+invite+`"}`, bearer(tok)...)
	return s, tok, b
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
		{"PUT", "/shared/k", "x"}, {"GET", "/to/me/k", ""}, {"GET", "/by/*/k", ""}, {"GET", "/shared/k/series", ""},
		{"GET", "/access", ""}, {"GET", "/invites", ""}, {"PATCH", "", `{"anyone":"all"}`}, {"DELETE", "", ""},
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
	_, _, b1 := c.join(own.Space, "inv_"+strings.Repeat("a", 43), stranger)
	_, _, b2 := c.join("gone-cedar-pilot-mango-frost", "inv_"+strings.Repeat("a", 43), stranger)
	if errCode(b1) != "invite_invalid" || b1 != b2 {
		t.Fatalf("join: %s / %s", b1, b2)
	}
}

func errBody(c *tc, path string) string { _, _, b := c.req("GET", path, ""); return b }

func TestAnyoneLevels(t *testing.T) {
	c := newTest(t)
	c.cfg.Server.RateLimits = new(bool)
	stranger, sid := c.anon()
	cases := []struct {
		anyone                     string
		read, shared, by, to, list bool
	}{
		{"read", true, false, false, false, false},
		{"post", true, false, true, true, false},
		{"all", true, true, true, true, false},
	}
	for _, k := range cases {
		own := c.create(`{"anyone":"`+k.anyone+`"}`, "")
		sp := "/v1/" + own.Space
		c.req("PUT", sp+"/shared/k", `1`, bearer(own.Token)...)
		if s, _, _ := c.req("GET", sp+"/shared/k", ""); (s == 200) != k.read {
			t.Errorf("%s read: %d", k.anyone, s)
		}
		want := func(ok bool) int {
			if ok {
				return 201
			}
			return 403
		}
		if s, _, b := c.req("PUT", sp+"/shared/x", `1`, bearer(stranger)...); s != want(k.shared) || (s == 403 && (errCode(b) != "no_access" || !strings.Contains(b, `"need":"all"`))) {
			t.Errorf("%s shared write: %d %s", k.anyone, s, b)
		}
		if s, _, b := c.req("PUT", sp+"/by/"+sid+"/x", `1`, bearer(stranger)...); s != want(k.by) || (s == 403 && !strings.Contains(b, `"need":"post"`)) {
			t.Errorf("%s by write: %d %s", k.anyone, s, b)
		}
		if s, _, _ := c.req("PUT", sp+"/to/me/x", `1`, bearer(stranger)...); s != want(k.to) {
			t.Errorf("%s to write: %d", k.anyone, s)
		}
		if _, _, b := c.req("GET", sp+"/access", ""); !strings.Contains(b, `"anyone":"`+k.anyone+`"`) || !strings.Contains(b, `"access":"`+k.anyone+`"`) || !strings.Contains(b, `"id":null`) {
			t.Errorf("%s access: %s", k.anyone, b)
		}
		// Strangers neither invite nor list members, whatever the anyone level.
		if s, _, _ := c.req("POST", sp+"/invites", ``, bearer(stranger)...); s != 403 {
			t.Errorf("%s stranger invites: %d", k.anyone, s)
		}
		if s, _, b := c.req("GET", sp+"/members", ``, bearer(stranger)...); s != 403 || errCode(b) != "not_member" {
			t.Errorf("%s stranger lists members: %d %s", k.anyone, s, b)
		}
	}

	// The owner changes settings; it takes effect at once.
	own := c.create(`{"anyone":"all"}`, "")
	sp := "/v1/" + own.Space
	if s, _, b := c.req("PATCH", sp, `{"anyone":"read"}`, bearer(stranger)...); s != 403 || errCode(b) != "owner_only" {
		t.Fatalf("non-owner patch: %d %s", s, b)
	}
	for _, bad := range []string{`{"anyone":"owner"}`, `{"anyone":"respond"}`, `{"invite":"none"}`, `{"public":"all"}`} {
		if s, _, b := c.req("PATCH", sp, bad, bearer(own.Token)...); s != 400 {
			t.Fatalf("patch %s: %d %s", bad, s, b)
		}
	}
	if s, _, b := c.req("PATCH", sp, `{"invite":"owner"}`, bearer(own.Token)...); s != 200 || !strings.Contains(b, `{"anyone":"all","invite":"owner"}`) {
		t.Fatalf("patch invite: %d %s", s, b)
	}
	if s, _, b := c.req("PATCH", sp, `{"anyone":"none"}`, bearer(own.Token)...); s != 200 || !strings.Contains(b, `"anyone":"none"`) {
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

	// Defaults: access all, 10 uses, 7 days.
	s, _, b := c.req("POST", sp+"/invites", ``, bearer(own.Token)...)
	var mint struct {
		ID, Invite, Access string
		Uses               *int64
		Until              *int64
		GrantFor           *int64
	}
	json.Unmarshal([]byte(b), &mint)
	week := time.Now().Add(7 * 24 * time.Hour).UnixMilli()
	if s != 201 || mint.Access != "all" || mint.Uses == nil || *mint.Uses != 10 ||
		mint.Until == nil || *mint.Until < week-60_000 || *mint.Until > week+60_000 || mint.GrantFor != nil {
		t.Fatalf("defaults: %d %s", s, b)
	}
	// Ten visitors join, the eleventh can't. Each is a new visitor.
	var members []string
	for i := range 10 {
		s, vt, b := c.join(own.Space, mint.Invite, "")
		if s != 200 || vt == "" {
			t.Fatalf("join %d: %d %s", i, s, b)
		}
		members = append(members, vt)
	}
	if s, _, b := c.join(own.Space, mint.Invite, ""); s != 404 || errCode(b) != "invite_invalid" {
		t.Fatalf("11th: %d %s", s, b)
	}
	// Redeeming again is a no-op for the same visitor, even now it's used up.
	if s, vt, b := c.join(own.Space, mint.Invite, members[0]); s != 200 || vt != members[0] {
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
	if s, _, b := c.req("POST", sp+"/invites", `{"use":1}`, bearer(own.Token)...); s != 400 || !strings.Contains(b, `"parameter":"use"`) {
		t.Fatalf("typo: %d %s", s, b)
	}
	for _, bad := range []string{`{"access":"editor"}`, `{"access":"owner","uses":2}`, `{"access":"banned"}`, `{"allow":"member"}`} {
		if s, _, b := c.req("POST", sp+"/invites", bad, bearer(own.Token)...); s != 400 {
			t.Fatalf("bad access %s: %d %s", bad, s, b)
		}
	}
	// Members invite by default, and see only their own tokens.
	if s, _, b := c.req("POST", sp+"/invites", `{"label":"from member"}`, m...); s != 201 || !strings.Contains(b, `"access":"all"`) {
		t.Fatalf("member mint: %d %s", s, b)
	}
	if _, _, b := c.req("GET", sp+"/invites", "", m...); strings.Count(b, `"id"`) != 1 || !strings.Contains(b, "from member") {
		t.Fatalf("member list: %s", b)
	}
	// Owner-only routes stay owner-only.
	if s, _, b := c.req("PATCH", sp, `{"anyone":"read"}`, m...); s != 403 || errCode(b) != "owner_only" {
		t.Fatalf("member settings: %d %s", s, b)
	}

	// The list shows state and grant counts, never token values.
	_, _, b = c.req("GET", sp+"/invites", "", bearer(own.Token)...)
	if !strings.Contains(b, `"state":"used_up"`) || !strings.Contains(b, `"members":10`) || strings.Contains(b, "at_") {
		t.Fatalf("list: %s", b)
	}

	// Revoking the token removes its grants.
	if s, _, _ := c.req("DELETE", sp+"/invites/"+mint.ID, "", bearer(own.Token)...); s != 200 {
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
	if s, _, _ := c.join(own.Space, brief, ""); s != 200 {
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

func TestMembers(t *testing.T) {
	c := newTest(t)
	c.cfg.Server.RateLimits = new(bool)
	own := c.create(`{"anyone":"read"}`, "")
	sp := "/v1/" + own.Space
	o := bearer(own.Token)
	vt, id := c.anon()
	a := bearer(vt)
	idPost, post := c.mint(own.Space, own.Token, `{"access":"post"}`)
	_, readInv := c.mint(own.Space, own.Token, `{"access":"read"}`)
	if s, _, b := c.join(own.Space, readInv, vt); s != 200 || !strings.Contains(b, `"access":"read"`) {
		t.Fatalf("join read: %d %s", s, b)
	}
	// Grants stack: the highest counts.
	if s, _, b := c.join(own.Space, post, vt); s != 200 || !strings.Contains(b, `"access":"post"`) {
		t.Fatalf("join post: %d %s", s, b)
	}
	for path, want := range map[string]int{"/by/me/vote": 201, "/to/owners/q": 201, "/shared/x": 403} {
		if s, _, b := c.req("PUT", sp+path, `1`, a...); s != want {
			t.Errorf("post member %s: %d %s", path, s, b)
		}
	}
	// A post member may invite (the default invite level) but not give all.
	if s, _, b := c.req("POST", sp+"/invites", `{"access":"all"}`, a...); s != 403 || errCode(b) != "exceeds_rights" {
		t.Fatalf("give more: %d %s", s, b)
	}
	if s, _, b := c.req("POST", sp+"/invites", ``, a...); s != 201 || !strings.Contains(b, `"access":"post"`) {
		t.Fatalf("member default invite: %d %s", s, b)
	}
	// Members see who is in, nothing more; owners see the details.
	_, _, b := c.req("GET", sp+"/members", "", a...)
	if !strings.Contains(b, `"id":"`+id+`","access":"post"`) || !strings.Contains(b, `"access":"owner"`) || strings.Contains(b, "invitedBy") {
		t.Fatalf("member list: %s", b)
	}
	if _, _, b := c.req("GET", sp+"/members", "", o...); !strings.Contains(b, `"invitedBy"`) || !strings.Contains(b, `"lastUsedAt"`) {
		t.Fatalf("owner list: %s", b)
	}
	// Revoking one invite leaves the other grant.
	c.req("DELETE", sp+"/invites/"+idPost, "", o...)
	if _, _, b := c.req("GET", sp+"/access", "", a...); !strings.Contains(b, `"access":"read"`) {
		t.Fatalf("after revoking post: %s", b)
	}

	// Setting a level replaces every grant.
	if s, _, b := c.req("PUT", sp+"/members/"+id, `{"access":"all"}`, a...); s != 403 || errCode(b) != "owner_only" {
		t.Fatalf("member sets levels: %d %s", s, b)
	}
	if s, _, b := c.req("PUT", sp+"/members/"+id, `{"access":"all"}`, o...); s != 200 || !strings.Contains(b, `"access":"all"`) {
		t.Fatalf("set all: %d %s", s, b)
	}
	if s, _, _ := c.req("PUT", sp+"/shared/x", `1`, a...); s != 201 {
		t.Fatalf("all can't write shared: %d", s)
	}
	if s, _, b := c.req("PUT", sp+"/members/u_aaaaaaaaaaaaaaaa", `{"access":"read"}`, o...); s != 404 || errCode(b) != "no_visitor" {
		t.Fatalf("set nobody: %d %s", s, b)
	}
	if s, _, b := c.req("PUT", sp+"/members/"+id, `{"access":"none"}`, o...); s != 400 {
		t.Fatalf("set none: %d %s", s, b)
	}

	// A ban keeps them out, takes their grants, and shows to owners only.
	if s, _, b := c.req("PUT", sp+"/members/"+id, `{"access":"banned","until":"1h"}`, o...); s != 200 || !strings.Contains(b, `"access":"banned"`) {
		t.Fatalf("ban: %d %s", s, b)
	}
	if s, _, b := c.req("GET", sp+"/shared/x", "", a...); s != 403 || errCode(b) != "banned" {
		t.Fatalf("banned read: %d %s", s, b)
	}
	if _, _, b := c.req("GET", sp+"/members", "", o...); !strings.Contains(b, `"id":"`+id+`","access":"banned"`) {
		t.Fatalf("owner sees ban: %s", b)
	}
	if s, _, b := c.req("PUT", sp+"/members/me", `{"access":"banned"}`, o...); s != 400 {
		t.Fatalf("self ban: %d %s", s, b)
	}
	// Removing them makes them a stranger: anyone read.
	if s, _, b := c.req("DELETE", sp+"/members/"+id, "", o...); s != 200 || !strings.Contains(b, `"removed":true`) {
		t.Fatalf("remove: %d %s", s, b)
	}
	if _, _, b := c.req("GET", sp+"/access", "", a...); !strings.Contains(b, `"access":"read"`) {
		t.Fatalf("after removing: %s", b)
	}

	// Anyone may leave; only owners remove others.
	other, _ := c.anon()
	c.join(own.Space, readInv, other)
	c.req("PUT", sp+"/members/"+id, `{"access":"post"}`, o...)
	if s, _, _ := c.req("DELETE", sp+"/members/"+id, "", bearer(other)...); s != 403 {
		t.Fatalf("member removes another: %d", s)
	}
	if s, _, b := c.req("DELETE", sp+"/members/me", "", a...); s != 200 || !strings.Contains(b, `"removed":true`) {
		t.Fatalf("leave: %d %s", s, b)
	}
	if s, _, _ := c.req("PUT", sp+"/by/me/vote", `2`, a...); s != 403 {
		t.Fatalf("left but still posts: %d", s)
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
	_, _, b := c.req("GET", "/v1/"+own.Space+"/invites", "", bearer(own.Token)...)
	if !strings.Contains(b, `"usesLeft":1`) || !strings.Contains(b, `"members":1`) {
		t.Fatalf("double redeem: %s", b)
	}
}

func TestAdminOwner(t *testing.T) {
	c := newTest(t)
	c.cfg.Server.RateLimits = new(bool)
	own := c.create(``, "")
	sp := "/v1/" + own.Space
	// Every owner device is gone; the operator makes another visitor owner.
	dev, devID := c.anon()
	if s, _, _ := c.req("POST", sp+"/invites", ``, bearer(dev)...); s != 404 {
		t.Fatalf("a stranger sees the private space: %d", s)
	}
	srv := New(c.cfg, c.store, nil)
	rec := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, httptest.NewRequest("PUT", "/spaces/"+own.Space+"/owners/"+devID, nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"owner":"`+devID+`"`) {
		t.Fatalf("admin owner: %d %s", rec.Code, rec.Body)
	}
	if s, _, _ := c.req("POST", sp+"/invites", ``, bearer(dev)...); s != 201 {
		t.Fatal("the new owner can't mint")
	}
	rec = httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, httptest.NewRequest("PUT", "/spaces/"+own.Space+"/owners/u_aaaaaaaaaaaaaaaa", nil))
	if rec.Code != 404 {
		t.Fatalf("unknown visitor: %d %s", rec.Code, rec.Body)
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
func TestVisitorsWithinLimit(t *testing.T) {
	c := newTest(t)
	ip := []string{"X-Forwarded-For", "203.0.113.50"}
	n := 0
	for range 120 {
		k, _ := auth.NewSigner()
		s, _, _ := c.req("POST", "/v1/visitors", "", append(ip, "Authorization", "Bearer "+k.Token(c.url, time.Minute, true))...)
		if s == 429 {
			break
		}
		n++
	}
	if n != 100 {
		t.Fatalf("visitors per IP: %d", n)
	}
}
