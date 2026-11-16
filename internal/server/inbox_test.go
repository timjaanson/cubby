package server

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type listItem struct {
	Key, From, To, By string
	Value             json.RawMessage
	Deleted           bool
}

func listOf(t *testing.T, b string) []listItem {
	t.Helper()
	var out struct{ Items []listItem }
	if err := json.Unmarshal([]byte(b), &out); err != nil {
		t.Fatalf("list body %s", b)
	}
	return out.Items
}

func TestToAndInbox(t *testing.T) {
	c := newTest(t)
	alice := c.create(`{"anyone":"all"}`, "")
	sp := "/v1/" + alice.Space
	a := bearer(alice.Token)
	bobTok, bobID := c.anon()
	caraTok, caraID := c.anon()
	b, cr := bearer(bobTok), bearer(caraTok)

	// Two senders, one key: two slots.
	if s, _, body := c.req("PUT", sp+"/to/"+alice.ID+"/message", `"yo"`, b...); s != 201 {
		t.Fatalf("bob sends: %d %s", s, body)
	}
	if s, _, body := c.req("PUT", sp+"/to/"+alice.ID+"/message", `"hi"`, cr...); s != 201 {
		t.Fatalf("cara sends: %d %s", s, body)
	}
	// One sender's, raw.
	if s, _, body := c.req("GET", sp+"/inbox/"+bobID+"/message", "", a...); s != 200 || body != `"yo"` {
		t.Fatalf("alice reads bob's: %d %s", s, body)
	}
	// Every sender's, with from and to.
	_, _, body := c.req("GET", sp+"/inbox/*/message", "", a...)
	items := listOf(t, body)
	if len(items) != 2 {
		t.Fatalf("inbox list %s", body)
	}
	for _, it := range items {
		if it.To != alice.ID || (it.From != bobID && it.From != caraID) || it.By != it.From {
			t.Fatalf("inbox item %+v", it)
		}
	}
	// The sender reads back their own slot, and only that.
	if s, _, body := c.req("GET", sp+"/to/"+alice.ID+"/message", "", b...); s != 200 || body != `"yo"` {
		t.Fatalf("bob reads back: %d %s", s, body)
	}
	_, _, body = c.req("GET", sp+"/to/*/*", "", b...)
	if items := listOf(t, body); len(items) != 1 || items[0].To != alice.ID || items[0].From != bobID {
		t.Fatalf("bob's sent list %s", body)
	}
	// Nobody else reads it: Cara's inbox is empty, Alice's to/ is her own.
	if _, _, body := c.req("GET", sp+"/inbox/*/*", "", cr...); len(listOf(t, body)) != 0 {
		t.Fatalf("cara's inbox %s", body)
	}
	if s, _, _ := c.req("GET", sp+"/to/"+alice.ID+"/message", "", a...); s != 404 {
		t.Fatalf("alice's own to/ slot: %d", s)
	}
	// Reading needs a visitor: an inbox belongs to someone.
	if s, _, _ := c.req("GET", sp+"/inbox/*/*", ""); s != 401 {
		t.Fatalf("inbox without token: %d", s)
	}
	// to/me/ is the private slot, the same entry as inbox/me/.
	c.req("PUT", sp+"/to/me/draft", `"secret"`, b...)
	if _, _, body := c.req("GET", sp+"/inbox/me/draft", "", b...); body != `"secret"` {
		t.Fatalf("inbox/me: %s", body)
	}
	if _, _, body := c.req("GET", sp+"/inbox/"+bobID+"/draft", "", b...); body != `"secret"` {
		t.Fatalf("inbox/{own id}: %s", body)
	}
	// by/me/ is the caller's own by/ area, on the server.
	c.req("PUT", sp+"/by/me/name", `"Bob"`, b...)
	if _, _, body := c.req("GET", sp+"/by/"+bobID+"/name", ""); body != `"Bob"` {
		t.Fatalf("by/me: %s", body)
	}
	// users/ is gone, with a pointer.
	if s, _, body := c.req("GET", sp+"/users/me/name", "", b...); s != 404 || !strings.Contains(body, "by/") {
		t.Fatalf("users/: %d %s", s, body)
	}
	// The inbox is read and delete only.
	if s, _, _ := c.req("PUT", sp+"/inbox/"+bobID+"/message", `1`, a...); s != 405 {
		t.Fatalf("PUT inbox: %d", s)
	}
	if s, _, _ := c.req("POST", sp+"/inbox/"+bobID+"/message/pulse", `1`, a...); s != 405 {
		t.Fatalf("pulse into inbox: %d", s)
	}
	if s, _, _ := c.req("PUT", sp+"/to/*/message", `1`, b...); s != 405 {
		t.Fatalf("PUT to/*: %d", s)
	}
	// Sending to someone who doesn't exist fails; garbage IDs are a 400.
	if s, _, body := c.req("PUT", sp+"/to/u_aaaaaaaaaaaaaaaa/x", `1`, b...); s != 404 || errCode(body) != "no_visitor" {
		t.Fatalf("unknown recipient: %d %s", s, body)
	}
	if s, _, _ := c.req("PUT", sp+"/to/bob/x", `1`, b...); s != 400 {
		t.Fatalf("bad recipient: %d", s)
	}
	// The recipient deletes; the sender sees it go.
	if s, _, body := c.req("DELETE", sp+"/inbox/"+caraID+"/message", "", a...); s != 200 {
		t.Fatalf("alice deletes cara's: %d %s", s, body)
	}
	if s, _, _ := c.req("GET", sp+"/to/"+alice.ID+"/message", "", cr...); s != 404 {
		t.Fatalf("cara's slot after delete: %d", s)
	}
	// List cursors name the sender.
	c.req("PUT", sp+"/to/"+alice.ID+"/message", `"again"`, cr...)
	_, _, body = c.req("GET", sp+"/inbox/*/message?limit=1", "", a...)
	var page struct{ Next string }
	json.Unmarshal([]byte(body), &page)
	if !strings.HasPrefix(page.Next, "message/u_") {
		t.Fatalf("cursor %s", body)
	}
	_, _, body = c.req("GET", sp+"/inbox/*/message?limit=1&after="+page.Next, "", a...)
	if items := listOf(t, body); len(items) != 1 {
		t.Fatalf("second page %s", body)
	}
}

func TestToOwners(t *testing.T) {
	c := newTest(t)
	own := c.create(`{"anyone":"post"}`, "")
	sp := "/v1/" + own.Space
	o := bearer(own.Token)
	bobTok, bobID := c.anon()
	b := bearer(bobTok)

	if s, _, body := c.req("PUT", sp+"/to/owners/answer", `42`, b...); s != 201 {
		t.Fatalf("send to owners: %d %s", s, body)
	}
	if _, _, body := c.req("GET", sp+"/to/owners/answer", "", b...); body != `42` {
		t.Fatalf("read back: %s", body)
	}
	_, _, body := c.req("GET", sp+"/inbox/owners/*/answer", "", o...)
	if items := listOf(t, body); len(items) != 1 || items[0].From != bobID || items[0].To != "owners" {
		t.Fatalf("owners' inbox %s", body)
	}
	if _, _, body := c.req("GET", sp+"/inbox/owners/"+bobID+"/answer", "", o...); body != `42` {
		t.Fatalf("one sender: %s", body)
	}
	// Mail to the group isn't in an owner's own inbox.
	if _, _, body := c.req("GET", sp+"/inbox/*/*", "", o...); len(listOf(t, body)) != 0 {
		t.Fatalf("personal inbox %s", body)
	}
	// Only owners read it. A second owner sees it too.
	if s, _, body := c.req("GET", sp+"/inbox/owners/*/*", "", b...); s != 403 || errCode(body) != "owner_only" {
		t.Fatalf("member reads owners' inbox: %d %s", s, body)
	}
	newTok, newID := c.anon()
	c.req("PUT", sp+"/members/"+newID, `{"access":"owner"}`, o...)
	if _, _, body := c.req("GET", sp+"/inbox/owners/*/*", "", bearer(newTok)...); len(listOf(t, body)) != 1 {
		t.Fatalf("second owner %s", body)
	}
	// post lets anyone send but not write shared/.
	if s, _, _ := c.req("PUT", sp+"/shared/x", `1`, b...); s != 403 {
		t.Fatalf("post shared write: %d", s)
	}
}

func TestToLevels(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "") // private
	sp := "/v1/" + own.Space
	_, friendID := c.anon()

	// A reader reads their inbox but sends nothing.
	_, reader := c.mint(own.Space, own.Token, `{"access":"read"}`)
	_, rt, _ := c.join(own.Space, reader, "")
	r := bearer(rt)
	if s, _, body := c.req("GET", sp+"/inbox/*/*", "", r...); s != 200 {
		t.Fatalf("read inbox: %d %s", s, body)
	}
	if s, _, body := c.req("PUT", sp+"/to/owners/msg", `"hello"`, r...); s != 403 || errCode(body) != "no_access" || !strings.Contains(body, `"need":"post"`) {
		t.Fatalf("reader sends: %d %s", s, body)
	}
	// Post sends to anyone, and to yourself.
	_, poster := c.mint(own.Space, own.Token, `{"access":"post"}`)
	_, pt, _ := c.join(own.Space, poster, "")
	p := bearer(pt)
	for _, to := range []string{"owners", friendID, "me"} {
		if s, _, body := c.req("PUT", sp+"/to/"+to+"/msg", `"hello"`, p...); s != 201 {
			t.Fatalf("send to %s: %d %s", to, s, body)
		}
	}
}

func TestDirectedPulses(t *testing.T) {
	c := newTest(t)
	alice := c.create(`{"anyone":"all"}`, "")
	sp := "/v1/" + alice.Space
	a := bearer(alice.Token)
	bobTok, bobID := c.anon()
	caraTok, _ := c.anon()

	toAlice := c.listen(sp+"/inbox/*/rtc.*/pulse?timeout=2", a...)
	toCara := c.listen(sp+"/inbox/*/rtc.*/pulse?timeout=1", bearer(caraTok)...)
	allUsers := c.listen(sp+"/by/*/rtc.*/pulse?timeout=1", bearer(caraTok)...)
	if s, _, body := c.req("POST", sp+"/to/"+alice.ID+"/rtc.offer/pulse", `{"sdp":"x"}`, bearer(bobTok)...); s != 202 {
		t.Fatalf("send: %d %s", s, body)
	}
	p := parsePulses(t, <-toAlice)
	if len(p.Pulses) != 1 || p.Pulses[0].By != bobID {
		t.Fatalf("alice got %+v", p)
	}
	var raw struct{ Pulses []struct{ From, To string } }
	if p := parsePulses(t, <-toCara); len(p.Pulses) != 0 {
		t.Fatalf("cara got %+v", p)
	}
	if p := parsePulses(t, <-allUsers); len(p.Pulses) != 0 {
		t.Fatalf("by/* got %+v", p)
	}
	// from and to come with it.
	got := c.listen(sp+"/inbox/*/rtc.*/pulse?timeout=2", a...)
	c.req("POST", sp+"/to/"+alice.ID+"/rtc.ice/pulse", `1`, bearer(bobTok)...)
	json.Unmarshal([]byte(<-got), &raw)
	if len(raw.Pulses) != 1 || raw.Pulses[0].From != bobID || raw.Pulses[0].To != alice.ID {
		t.Fatalf("pulse parties %+v", raw)
	}
}

func TestInboxLiveIdentity(t *testing.T) {
	c := newTest(t)
	alice := c.create(`{"anyone":"all"}`, "")
	bobTok, _ := c.anon()
	w := c.dial(alice.Space, bearer(alice.Token)...)
	w.send(`{"id":"in","method":"GET","path":"inbox/*/*/watch","query":{"version":0}}`)
	watchBody(t, w.reply(`"in"`))
	c.req("PUT", "/v1/"+alice.Space+"/to/"+alice.ID+"/hello", `1`, bearer(bobTok)...)
	changes, _ := watchBody(t, w.reply(`"in"`))
	if len(changes) != 1 || changes[0]["from"] == nil {
		t.Fatalf("pushed %+v", changes)
	}
	// Another identity: the inbox watch ends.
	w.call(`"a"`, `{"id":"a","op":"auth","token":"`+bobTok+`"}`)
	if f := w.reply(`"in"`); f.Status != 409 || f.code() != "identity_changed" {
		t.Fatalf("after auth: %+v", f)
	}
}

func TestBans(t *testing.T) {
	c := newTest(t)
	own := c.create(`{"anyone":"all"}`, "")
	sp := "/v1/" + own.Space
	o := bearer(own.Token)
	bobTok, bobID := c.anon()
	b := bearer(bobTok)
	c.req("PUT", sp+"/by/me/msg.1", `"spam"`, b...)
	memberTok, _ := c.anon()
	c.req("PUT", sp+"/members/"+bobID, `{"access":"all"}`, o...)

	// Only owners ban.
	if s, _, body := c.req("PUT", sp+"/members/"+bobID, `{"access":"banned"}`, bearer(memberTok)...); s != 403 || errCode(body) != "owner_only" {
		t.Fatalf("member bans: %d %s", s, body)
	}
	// A live socket is closed when the ban lands.
	w := c.dial(own.Space, b...)
	w.call("1", `{"id":1,"method":"GET","path":"shared/x"}`)
	w.call("2", `{"id":2,"op":"will","path":"by/me/here/pulse","body":0}`)
	gone := c.listen(sp+"/by/*/here/pulse?timeout=1", o...)

	if s, _, body := c.req("PUT", sp+"/members/"+bobID, `{"access":"banned","until":"1h"}`, o...); s != 200 || !strings.Contains(body, `"until":1`) {
		t.Fatalf("ban: %d %s", s, body)
	}
	if s, _, _ := c.req("PUT", sp+"/members/"+bobID, `{"access":"banned"}`, o...); s != 200 {
		t.Fatalf("replace ban: %d", s)
	}
	if f, ok := w.next(2 * time.Second); !ok || f.code() != "banned" {
		t.Fatalf("live frame %+v", f)
	}
	var ce websocket.CloseError
	if err := w.readErr(); !errors.As(err, &ce) || ce.Code != closeBanned {
		t.Fatalf("live close %v", err)
	}
	if p := parsePulses(t, <-gone); len(p.Pulses) != 0 {
		t.Fatalf("a banned visitor's will fired: %+v", p)
	}
	// Every request with the token says so, reads and access included.
	for _, r := range [][2]string{{"GET", "/shared/x"}, {"PUT", "/shared/x"}, {"GET", "/access"}, {"POST", "/by/me/x/pulse"}, {"GET", "/inbox/*/*"}} {
		s, _, body := c.req(r[0], sp+r[1], `1`, b...)
		if s != 403 || errCode(body) != "banned" || !strings.Contains(body, `"until":null`) {
			t.Fatalf("%s %s banned: %d %s", r[0], r[1], s, body)
		}
	}
	// Anonymous reading of an open space still works, and their data stays.
	if s, _, _ := c.req("GET", sp+"/by/"+bobID+"/msg.1", ""); s != 200 {
		t.Fatalf("anonymous read: %d", s)
	}
	_, _, body := c.req("GET", sp+"/members", "", o...)
	if !strings.Contains(body, `"id":"`+bobID+`","access":"banned"`) {
		t.Fatalf("members list %s", body)
	}
	// Removing the ban makes them a stranger: the anyone level, not what
	// they had before.
	if s, _, body := c.req("DELETE", sp+"/members/"+bobID, "", o...); s != 200 || !strings.Contains(body, `"removed":true`) {
		t.Fatalf("unban: %d %s", s, body)
	}
	if _, _, body := c.req("GET", sp+"/access", "", b...); !strings.Contains(body, `"access":"all"`) {
		t.Fatalf("after unban %s", body)
	}
}

func TestBanBlocksJoin(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "") // private
	sp := "/v1/" + own.Space
	o := bearer(own.Token)
	_, link := c.mint(own.Space, own.Token, `{"uses":null}`)
	_, bobTok, _ := c.join(own.Space, link, "")
	var who struct{ ID string }
	_, _, body := c.req("GET", "/v1/whoami", "", bearer(bobTok)...)
	json.Unmarshal([]byte(body), &who)

	// A banned member can't rejoin, and their own invites end.
	_, memberLink := c.mint(own.Space, bobTok, ``)
	c.req("PUT", sp+"/members/"+who.ID, `{"access":"banned"}`, o...)
	if s, _, body := c.join(own.Space, link, bobTok); s != 403 || errCode(body) != "banned" {
		t.Fatalf("rejoin while banned: %d %s", s, body)
	}
	if s, _, body := c.join(own.Space, memberLink, ""); s != 404 || errCode(body) != "invite_invalid" {
		t.Fatalf("banned member's invite: %d %s", s, body)
	}
	// Private space: still explicit.
	if s, _, body := c.req("GET", sp+"/shared/x", "", bearer(bobTok)...); s != 403 || errCode(body) != "banned" {
		t.Fatalf("private read: %d %s", s, body)
	}
	// A ban with an end lapses by itself, and they can join again.
	c.req("PUT", sp+"/members/"+who.ID, `{"access":"banned","until":`+itoa(time.Now().Add(300*time.Millisecond).UnixMilli())+`}`, o...)
	if s, _, _ := c.req("GET", sp+"/shared/x", "", bearer(bobTok)...); s != 403 {
		t.Fatalf("timed ban: %d", s)
	}
	time.Sleep(400 * time.Millisecond)
	if s, _, body := c.join(own.Space, link, bobTok); s != 200 || !strings.Contains(body, `"access":"all"`) {
		t.Fatalf("join after lapse: %d %s", s, body)
	}
}
