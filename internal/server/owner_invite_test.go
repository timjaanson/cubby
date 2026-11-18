package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestOwnerInvite(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "")
	sp := "/v1/" + own.Space
	o := bearer(own.Token)

	// Defaults: one use, 7 days.
	s, _, b := c.req("POST", sp+"/invites", `{"access":"owner","label":"laptop"}`, o...)
	var inv struct {
		ID, Invite, Access string
		Uses               *int64
		Until              *int64 `json:"expires"`
	}
	json.Unmarshal([]byte(b), &inv)
	week := time.Now().Add(7 * 24 * time.Hour).UnixMilli()
	if s != 201 || inv.Access != "owner" || inv.Uses == nil || *inv.Uses != 1 || inv.Until == nil || *inv.Until > week || *inv.Until < week-60_000 {
		t.Fatalf("mint: %d %s", s, b)
	}
	// uses is 1, until within 7 days and never open-ended.
	for _, body := range []string{
		`{"access":"owner","uses":2}`, `{"access":"owner","uses":null}`, `{"access":"owner","uses":0}`,
		`{"access":"owner","ttl":null}`, `{"access":"owner","ttl":"8d"}`, `{"access":"owner","ttl":` + itoa(8*24*3600) + `}`,
	} {
		if s, _, b := c.req("POST", sp+"/invites", body, o...); s != 400 || errCode(b) != "invalid_parameter" {
			t.Errorf("%s: %d %s", body, s, b)
		}
	}
	if s, _, b := c.req("POST", sp+"/invites", `{"access":"owner","uses":1,"ttl":"2d"}`, o...); s != 201 {
		t.Fatalf("explicit: %d %s", s, b)
	}
	// PATCH keeps the same rules; uses 0 stops it.
	for _, body := range []string{`{"uses":2}`, `{"uses":null}`, `{"ttl":null}`, `{"ttl":"30d"}`} {
		if s, _, b := c.req("PATCH", sp+"/invites/"+inv.ID, body, o...); s != 400 || errCode(b) != "invalid_parameter" {
			t.Errorf("patch %s: %d %s", body, s, b)
		}
	}
	if s, _, b := c.req("PATCH", sp+"/invites/"+inv.ID, `{"ttl":"3d"}`, o...); s != 200 {
		t.Fatalf("patch until: %d %s", s, b)
	}
	// Other invites keep their old rules.
	plainID, _ := c.mint(own.Space, own.Token, `{"access":"read"}`)
	if s, _, b := c.req("PATCH", sp+"/invites/"+plainID, `{"uses":null,"ttl":"30d"}`, o...); s != 200 {
		t.Fatalf("patch plain: %d %s", s, b)
	}

	// Members list shows it as usual.
	if _, _, b := c.req("GET", sp+"/invites", "", o...); !strings.Contains(b, `"access":"owner"`) || !strings.Contains(b, "laptop") {
		t.Fatalf("invites list: %s", b)
	}

	// Redeeming makes the visitor owner, once.
	s, newTok, b := c.join(own.Space, inv.Invite, "")
	if s != 200 || !strings.Contains(b, `"access":"owner"`) {
		t.Fatalf("join: %d %s", s, b)
	}
	if s, _, b := c.req("PATCH", sp, `{"anyone":"read"}`, bearer(newTok)...); s != 200 {
		t.Fatalf("new owner settings: %d %s", s, b)
	}
	if s, _, _ := c.join(own.Space, inv.Invite, ""); s == 200 {
		t.Fatal("second visitor redeemed a one-use owner invite")
	}

	// Only owners mint one: an all member gets 403.
	_, all := c.mint(own.Space, own.Token, `{"access":"all"}`)
	_, mTok, _ := c.join(own.Space, all, "")
	if s, _, b := c.req("POST", sp+"/invites", `{"access":"owner"}`, bearer(mTok)...); s != 403 {
		t.Fatalf("member mints owner: %d %s", s, b)
	}
}
