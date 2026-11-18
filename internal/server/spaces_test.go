package server

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"
)

type spacesOut struct {
	Items []struct {
		Space, Access, Via string
		Since, LastActive  int64
		Expires            *int64
		Members, Keys      int
		Bytes              int64
		Settings           struct{ Anyone, Inviters string }
	}
	Next *string
}

func (c *tc) spaces(tok, query string) spacesOut {
	c.t.Helper()
	st, _, b := c.req("GET", "/v1/spaces"+query, "", bearer(tok)...)
	var out spacesOut
	if st != 200 || json.Unmarshal([]byte(b), &out) != nil {
		c.t.Fatalf("spaces: %d %s", st, b)
	}
	return out
}

func spaceNames(o spacesOut) []string {
	var n []string
	for _, it := range o.Items {
		n = append(n, it.Space)
	}
	return n
}

func TestSpacesList(t *testing.T) {
	c := newTest(t)
	aliceTok, _ := c.anon()
	a := c.create(``, aliceTok)
	time.Sleep(3 * time.Millisecond)
	b := c.create(`{"anyone":"read"}`, aliceTok)
	c.req("PUT", "/v1/"+b.Space+"/shared/x", `"hello"`, bearer(aliceTok)...)

	// Newest first, with what's cheap to know.
	out := c.spaces(aliceTok, "")
	if got := spaceNames(out); len(got) != 2 || got[0] != b.Space || got[1] != a.Space || out.Next != nil {
		t.Fatalf("alice's spaces %v", got)
	}
	it := out.Items[0]
	if it.Access != "owner" || it.Via != "created" || it.Members != 1 || it.Keys != 1 || it.Bytes == 0 ||
		it.Since == 0 || it.LastActive == 0 || it.Settings.Anyone != "read" || it.Expires != nil {
		t.Fatalf("item %+v", it)
	}

	// Activity moves a space up: a write to A puts it first.
	time.Sleep(1100 * time.Millisecond) // lastActive has second granularity
	c.req("PUT", "/v1/"+a.Space+"/shared/x", `1`, bearer(aliceTok)...)
	if got := spaceNames(c.spaces(aliceTok, "")); got[0] != a.Space {
		t.Fatalf("most recently active first: %v", got)
	}
	c.req("PUT", "/v1/"+b.Space+"/shared/x", `2`, bearer(aliceTok)...)
	time.Sleep(1100 * time.Millisecond)
	c.req("PUT", "/v1/"+b.Space+"/shared/x", `3`, bearer(aliceTok)...)

	// Paging: one at a time.
	p1 := c.spaces(aliceTok, "?limit=1")
	if len(p1.Items) != 1 || p1.Items[0].Space != b.Space || p1.Next == nil {
		t.Fatalf("page 1 %+v", p1)
	}
	p2 := c.spaces(aliceTok, "?limit=1&after="+url.QueryEscape(*p1.Next))
	if len(p2.Items) != 1 || p2.Items[0].Space != a.Space || p2.Next != nil {
		t.Fatalf("page 2 %+v", p2)
	}

	// Bob joins A. His other device sees it too: membership is the visitor's.
	time.Sleep(3 * time.Millisecond)
	_, inv := c.mint(a.Space, aliceTok, `{"access":"post"}`)
	_, bobTok, _ := c.join(a.Space, inv, "")
	bob := c.spaces(bobTok, "")
	if len(bob.Items) != 1 || bob.Items[0].Access != "post" || bob.Items[0].Via != "invite" || bob.Items[0].Members != 2 {
		t.Fatalf("bob %+v", bob)
	}
	since := bob.Items[0].Since
	code := c.pairingCode(bobTok, "")
	bob2Tok, _ := c.newDevice(code.Code)
	if b2 := c.spaces(bob2Tok, ""); len(b2.Items) != 1 || b2.Items[0].Since != since {
		t.Fatalf("bob's second device %+v", b2)
	}
	// Open spaces Bob only wrote to are not his.
	open := c.create(`{"anyone":"all"}`, aliceTok)
	c.req("PUT", "/v1/"+open.Space+"/shared/y", `1`, bearer(bobTok)...)
	if got := spaceNames(c.spaces(bobTok, "")); len(got) != 1 {
		t.Fatalf("open space listed for bob %v", got)
	}

	// An owner changes his level: changed moves, since stays.
	time.Sleep(3 * time.Millisecond)
	var bobID string
	var me struct{ ID string }
	_, _, mb := c.req("GET", "/v1/me", "", bearer(bobTok)...)
	json.Unmarshal([]byte(mb), &me)
	bobID = me.ID
	if s, _, body := c.req("PUT", "/v1/"+a.Space+"/members/"+bobID, `{"access":"read"}`, bearer(aliceTok)...); s != 200 {
		t.Fatalf("set member %d %s", s, body)
	}
	bob = c.spaces(bobTok, "")
	if bob.Items[0].Access != "read" || bob.Items[0].Via != "added" || bob.Items[0].Since != since {
		t.Fatalf("after level change %+v", bob.Items[0])
	}
	// Alice's A moved to the top of her list? No: her own membership didn't change.
	if got := spaceNames(c.spaces(aliceTok, "")); got[len(got)-1] != a.Space {
		t.Fatalf("alice's order changed %v", got)
	}

	// Leaving and joining again starts a new since, listed in between or not.
	c.req("DELETE", "/v1/"+a.Space+"/members/me", "", bearer(bobTok)...)
	time.Sleep(3 * time.Millisecond)
	_, inv1 := c.mint(a.Space, aliceTok, ``)
	c.join(a.Space, inv1, bobTok)
	if bob = c.spaces(bobTok, ""); len(bob.Items) != 1 || bob.Items[0].Since <= since {
		t.Fatalf("rejoined unlisted %+v", bob)
	}
	since = bob.Items[0].Since
	c.req("DELETE", "/v1/"+a.Space+"/members/me", "", bearer(bobTok)...)
	if got := spaceNames(c.spaces(bobTok, "")); len(got) != 0 {
		t.Fatalf("after leaving %v", got)
	}
	time.Sleep(3 * time.Millisecond)
	_, inv2 := c.mint(a.Space, aliceTok, ``)
	c.join(a.Space, inv2, bobTok)
	if bob = c.spaces(bobTok, ""); len(bob.Items) != 1 || bob.Items[0].Since <= since {
		t.Fatalf("rejoined %+v", bob)
	}

	// Banned: gone. Deleted space: gone.
	c.req("PUT", "/v1/"+a.Space+"/members/"+bobID, `{"access":"banned"}`, bearer(aliceTok)...)
	if got := spaceNames(c.spaces(bobTok, "")); len(got) != 0 {
		t.Fatalf("banned %v", got)
	}
	c.req("DELETE", "/v1/"+b.Space, "", bearer(aliceTok)...)
	if got := spaceNames(c.spaces(aliceTok, "")); len(got) != 2 || got[0] != open.Space || got[1] != a.Space {
		t.Fatalf("after delete %v", got)
	}
}

func TestSpacesListRefusals(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "")
	if s, _, b := c.req("GET", "/v1/spaces", ""); s != 401 || errCode(b) != "missing_token" {
		t.Fatalf("no token %d %s", s, b)
	}
	for _, q := range []string{"?limit=0", "?limit=1001", "?after=1:2:x", "?after=x", "?after=12:NOT_A_SPACE", "?bogus=1"} {
		if s, _, b := c.req("GET", "/v1/spaces"+q, "", bearer(own.Token)...); s != 400 {
			t.Fatalf("%s: %d %s", q, s, b)
		}
	}
	if s, _, _ := c.req("DELETE", "/v1/spaces", "", bearer(own.Token)...); s != 405 {
		t.Fatalf("DELETE: %d", s)
	}
	_, _, b := c.req("POST", "/v1/me/api-keys", `{"space":"`+own.Space+`"}`, bearer(own.Token)...)
	var k struct{ Key string }
	json.Unmarshal([]byte(b), &k)
	if s, _, b := c.req("GET", "/v1/spaces", "", bearer(k.Key)...); s != 403 || errCode(b) != "api_key_not_allowed" {
		t.Fatalf("api key %d %s", s, b)
	}
}
