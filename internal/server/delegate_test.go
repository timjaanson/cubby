package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// Members invite others with rights they hold; chains are visible; revoking
// an inviter keeps the people they invited and ends their open invites.
func TestDelegatedInvites(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "")
	sp := "/v1/" + own.Space
	o := bearer(own.Token)
	idOf := func(tok string) string {
		_, _, b := c.req("GET", sp+"/access", "", bearer(tok)...)
		var a struct {
			ID        string
			CanInvite bool
		}
		json.Unmarshal([]byte(b), &a)
		return a.ID
	}

	// Anna is a member, Vic a viewer.
	_, annaInv := c.mint(own.Space, own.Token, `{"label":"anna"}`)
	_, anna, _ := c.join(own.Space, annaInv, "")
	_, vicInv := c.mint(own.Space, own.Token, `{"allow":"viewer"}`)
	_, vic, _ := c.join(own.Space, vicInv, "")
	if _, _, b := c.req("GET", sp+"/access", "", bearer(anna)...); !strings.Contains(b, `"canInvite":true`) || !strings.Contains(b, `"invite":"members"`) {
		t.Fatalf("access: %s", b)
	}

	// A viewer's invite gives what they have, and no more.
	s, _, b := c.req("POST", sp+"/tokens", `{}`, bearer(vic)...)
	if s != 201 || !strings.Contains(b, `"allow":["shared:r","users:r"]`) {
		t.Fatalf("viewer default: %d %s", s, b)
	}
	if s, _, b := c.req("POST", sp+"/tokens", `{"allow":"member"}`, bearer(vic)...); s != 403 || errCode(b) != "exceeds_rights" {
		t.Fatalf("viewer to member: %d %s", s, b)
	}

	// Anna invites Ben, Ben invites Cleo: a chain.
	benTokID, benInv := c.mint(own.Space, anna, `{"label":"ben","uses":null}`)
	_, ben, _ := c.join(own.Space, benInv, "")
	_, cleoInv := c.mint(own.Space, ben, `{}`)
	_, cleo, _ := c.join(own.Space, cleoInv, "")
	_, _, b = c.req("GET", sp+"/grants", "", o...)
	if !strings.Contains(b, `"invitedBy":"`+idOf(anna)+`"`) || !strings.Contains(b, `"invitedBy":"`+idOf(ben)+`"`) {
		t.Fatalf("chain: %s", b)
	}
	_, _, b = c.req("GET", sp+"/tokens", "", o...)
	if !strings.Contains(b, `"by":"`+idOf(anna)+`"`) {
		t.Fatalf("owner sees minters: %s", b)
	}
	// Members can't touch others' tokens.
	if s, _, _ := c.req("DELETE", sp+"/tokens/"+benTokID, "", bearer(ben)...); s != 404 {
		t.Fatalf("ben deletes anna's token: %d", s)
	}

	// Revoking Anna keeps Ben, and ends her open invite.
	if s, _, _ := c.req("DELETE", sp+"/grants?user="+idOf(anna), "", o...); s != 200 {
		t.Fatal("revoke anna")
	}
	if _, _, b := c.req("GET", sp+"/access", "", bearer(ben)...); !strings.Contains(b, `"grants":[{`) {
		t.Fatalf("ben stays: %s", b)
	}
	if s, _, b := c.join(own.Space, benInv, ""); s != 400 && s != 404 || errCode(b) != "invite_invalid" {
		t.Fatalf("anna's invite after revoke: %d %s", s, b)
	}
	_, _, b = c.req("GET", sp+"/tokens", "", o...)
	if !strings.Contains(b, `"state":"revoked"`) {
		t.Fatalf("revoked state: %s", b)
	}

	// invite: owners stops members and ends their open tokens. Grants stay.
	_, danInv := c.mint(own.Space, cleo, `{}`)
	if s, _, b := c.req("PATCH", sp, `{"invite":"owners"}`, o...); s != 200 || !strings.Contains(b, `"invite":"owners"`) {
		t.Fatalf("settings: %d %s", s, b)
	}
	if s, _, b := c.req("POST", sp+"/tokens", `{}`, bearer(cleo)...); s != 403 || errCode(b) != "owner_only" {
		t.Fatalf("mint with owners only: %d %s", s, b)
	}
	if s, _, _ := c.join(own.Space, danInv, ""); s == 200 || s == 201 {
		t.Fatal("member token works after invite: owners")
	}
	if _, _, b := c.req("GET", sp+"/access", "", bearer(cleo)...); !strings.Contains(b, `"canInvite":false`) || !strings.Contains(b, `"grants":[{`) {
		t.Fatalf("cleo after owners: %s", b)
	}
	// Owners still mint, and the public shorthand doesn't reset invite.
	c.mint(own.Space, own.Token, `{}`)
	if _, _, b := c.req("PATCH", sp, `{"public":"read"}`, o...); !strings.Contains(b, `"invite":"owners"`) {
		t.Fatalf("public keeps invite: %s", b)
	}
	// Created with invite owners.
	s, _, b = c.req("POST", "/v1/spaces", `{"invite":"owners"}`, o...)
	if s != 201 || !strings.Contains(b, `"invite":"owners"`) {
		t.Fatalf("create with invite: %d %s", s, b)
	}
}

// A member's token stops working once they no longer hold what it gives,
// even if they keep other grants.
func TestDelegatedTokenFollowsMinterRights(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "")
	sp := "/v1/" + own.Space
	o := bearer(own.Token)
	_, rwInv := c.mint(own.Space, own.Token, `{"allow":["shared:rw"]}`)
	_, rInv := c.mint(own.Space, own.Token, `{"allow":["shared:r"]}`)
	_, m, _ := c.join(own.Space, rwInv, "")
	c.join(own.Space, rInv, m)
	_, tok := c.mint(own.Space, m, `{"allow":["shared:rw"]}`)
	// Remove the rw grant, keep the r one.
	_, _, b := c.req("GET", sp+"/grants", "", o...)
	var gl struct {
		Items []struct {
			ID    string
			Allow []string
		}
	}
	json.Unmarshal([]byte(b), &gl)
	for _, g := range gl.Items {
		if len(g.Allow) == 1 && g.Allow[0] == "shared:rw" {
			c.req("DELETE", sp+"/grants/"+g.ID, "", o...)
		}
	}
	if s, _, b := c.join(own.Space, tok, ""); s == 200 || s == 201 || errCode(b) != "invite_invalid" {
		t.Fatalf("token beyond minter's rights still works: %d %s", s, b)
	}
}
