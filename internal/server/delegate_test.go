package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// Members at the invite level invite others with at most their own level;
// chains are visible; removing an inviter keeps the people they invited and
// ends their open invites.
func TestDelegatedInvites(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "")
	sp := "/v1/" + own.Space
	o := bearer(own.Token)
	idOf := func(tok string) string {
		_, _, b := c.req("GET", sp+"/access", "", bearer(tok)...)
		var a struct{ ID string }
		json.Unmarshal([]byte(b), &a)
		return a.ID
	}

	// Anna has all, Vic only reads.
	_, annaInv := c.mint(own.Space, own.Token, `{"label":"anna"}`)
	_, anna, _ := c.join(own.Space, annaInv, "")
	_, vicInv := c.mint(own.Space, own.Token, `{"access":"read"}`)
	_, vic, _ := c.join(own.Space, vicInv, "")
	if _, _, b := c.req("GET", sp+"/access", "", bearer(anna)...); !strings.Contains(b, `"canInvite":true`) || !strings.Contains(b, `"invite":"post"`) {
		t.Fatalf("access: %s", b)
	}
	// Below the invite level (post), a reader can't invite at all.
	if _, _, b := c.req("GET", sp+"/access", "", bearer(vic)...); !strings.Contains(b, `"canInvite":false`) {
		t.Fatalf("reader access: %s", b)
	}
	if s, _, b := c.req("POST", sp+"/invites", `{}`, bearer(vic)...); s != 403 || errCode(b) != "owner_only" {
		t.Fatalf("reader mints: %d %s", s, b)
	}

	// Anna invites Ben, Ben invites Cleo: a chain.
	benTokID, benInv := c.mint(own.Space, anna, `{"label":"ben","uses":null}`)
	_, ben, _ := c.join(own.Space, benInv, "")
	_, cleoInv := c.mint(own.Space, ben, `{}`)
	_, cleo, _ := c.join(own.Space, cleoInv, "")
	_, _, b := c.req("GET", sp+"/members", "", o...)
	if !strings.Contains(b, `"invitedBy":"`+idOf(anna)+`"`) || !strings.Contains(b, `"invitedBy":"`+idOf(ben)+`"`) {
		t.Fatalf("chain: %s", b)
	}
	_, _, b = c.req("GET", sp+"/invites", "", o...)
	if !strings.Contains(b, `"by":"`+idOf(anna)+`"`) {
		t.Fatalf("owner sees minters: %s", b)
	}
	// Members can't touch others' invites.
	if s, _, _ := c.req("DELETE", sp+"/invites/"+benTokID, "", bearer(ben)...); s != 404 {
		t.Fatalf("ben deletes anna's invite: %d", s)
	}

	// Removing Anna keeps Ben, and ends her open invite.
	if s, _, _ := c.req("DELETE", sp+"/members/"+idOf(anna), "", o...); s != 200 {
		t.Fatal("remove anna")
	}
	if _, _, b := c.req("GET", sp+"/access", "", bearer(ben)...); !strings.Contains(b, `"access":"all"`) {
		t.Fatalf("ben stays: %s", b)
	}
	if s, _, b := c.join(own.Space, benInv, ""); s != 404 || errCode(b) != "invite_invalid" {
		t.Fatalf("anna's invite after removal: %d %s", s, b)
	}
	_, _, b = c.req("GET", sp+"/invites", "", o...)
	if !strings.Contains(b, `"state":"revoked"`) {
		t.Fatalf("revoked state: %s", b)
	}

	// invite owner stops members and ends their open invites. Grants stay.
	_, danInv := c.mint(own.Space, cleo, `{}`)
	if s, _, b := c.req("PATCH", sp, `{"invite":"owner"}`, o...); s != 200 || !strings.Contains(b, `"invite":"owner"`) {
		t.Fatalf("settings: %d %s", s, b)
	}
	if s, _, b := c.req("POST", sp+"/invites", `{}`, bearer(cleo)...); s != 403 || errCode(b) != "owner_only" {
		t.Fatalf("mint with owner only: %d %s", s, b)
	}
	if s, _, _ := c.join(own.Space, danInv, ""); s == 200 || s == 201 {
		t.Fatal("member invite works after invite: owner")
	}
	if _, _, b := c.req("GET", sp+"/access", "", bearer(cleo)...); !strings.Contains(b, `"canInvite":false`) || !strings.Contains(b, `"access":"all"`) {
		t.Fatalf("cleo after owner only: %s", b)
	}
	// Owners still mint, and changing anyone doesn't reset invite.
	c.mint(own.Space, own.Token, `{}`)
	if _, _, b := c.req("PATCH", sp, `{"anyone":"read"}`, o...); !strings.Contains(b, `"invite":"owner"`) {
		t.Fatalf("anyone keeps invite: %s", b)
	}
	// Created with invite owner.
	s, _, b := c.req("POST", "/v1/spaces", `{"invite":"owner"}`, o...)
	if s != 201 || !strings.Contains(b, `"invite":"owner"`) {
		t.Fatalf("create with invite: %d %s", s, b)
	}
}

// A member's invite stops working once they no longer have the level it
// gives, even if they keep a lower one.
func TestDelegatedTokenFollowsMinterLevel(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "")
	sp := "/v1/" + own.Space
	o := bearer(own.Token)
	_, allInv := c.mint(own.Space, own.Token, `{}`)
	_, m, _ := c.join(own.Space, allInv, "")
	_, gives := c.mint(own.Space, m, `{"access":"all"}`)
	_, keeps := c.mint(own.Space, m, `{"access":"post"}`)
	var a struct{ ID string }
	_, _, b := c.req("GET", sp+"/access", "", bearer(m)...)
	json.Unmarshal([]byte(b), &a)
	c.req("PUT", sp+"/members/"+a.ID, `{"access":"post"}`, o...)
	if s, _, b := c.join(own.Space, gives, ""); s != 404 || errCode(b) != "invite_invalid" {
		t.Fatalf("invite beyond minter's level still works: %d %s", s, b)
	}
	if s, _, b := c.join(own.Space, keeps, ""); s != 200 || !strings.Contains(b, `"access":"post"`) {
		t.Fatalf("invite within minter's level: %d %s", s, b)
	}
}
