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
	Key, From, To, By, User string
	Value                   json.RawMessage
	Deleted                 bool
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
	alice := c.create(`{"public":"all"}`, "")
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
	// users/me/ is the caller's own users/ area, on the server.
	c.req("PUT", sp+"/users/me/name", `"Bob"`, b...)
	if _, _, body := c.req("GET", sp+"/users/"+bobID+"/name", ""); body != `"Bob"` {
		t.Fatalf("users/me: %s", body)
	}
	// The old area is gone, with a pointer.
	if s, _, body := c.req("GET", sp+"/me/draft", "", b...); s != 404 || !strings.Contains(body, "to/me/") {
		t.Fatalf("me/: %d %s", s, body)
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
	own := c.create(`{"public":"respond"}`, "")
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
	c.req("POST", sp+"/grants", `{"user":"`+newID+`","owner":true}`, o...)
	if _, _, body := c.req("GET", sp+"/inbox/owners/*/*", "", bearer(newTok)...); len(listOf(t, body)) != 1 {
		t.Fatalf("second owner %s", body)
	}
	// respond lets anyone send but not write shared/.
	if s, _, _ := c.req("PUT", sp+"/shared/x", `1`, b...); s != 403 {
		t.Fatalf("respond shared write: %d", s)
	}
}

func TestToScopes(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "") // private
	sp := "/v1/" + own.Space
	o := bearer(own.Token)
	_, friendID := c.anon()

	// A contact form: send to the owners, nothing else.
	_, form := c.mint(own.Space, own.Token, `{"allow":["to/owners/*:w"]}`)
	s, vt, body := c.join(own.Space, form, "")
	if s != 201 {
		t.Fatalf("join: %d %s", s, body)
	}
	v := bearer(vt)
	if s, _, body := c.req("PUT", sp+"/to/owners/msg", `"hello"`, v...); s != 201 {
		t.Fatalf("send to owners: %d %s", s, body)
	}
	if s, _, body := c.req("PUT", sp+"/to/"+friendID+"/msg", `"hello"`, v...); s != 403 || errCode(body) != "no_access" {
		t.Fatalf("send to a visitor: %d %s", s, body)
	}
	if s, _, body := c.req("PUT", sp+"/to/me/note", `1`, v...); s != 403 {
		t.Fatalf("to/me without scope: %d %s", s, body)
	}
	// Write-only: the space stays dark for reads.
	if s, _, body := c.req("GET", sp+"/to/owners/msg", "", v...); s != 404 || errCode(body) != "no_space" {
		t.Fatalf("read without r: %d %s", s, body)
	}
	// Reading your inbox takes r on to.
	_, reader := c.mint(own.Space, own.Token, `{"allow":["to:r"]}`)
	_, rt, _ := c.join(own.Space, reader, "")
	if s, _, body := c.req("GET", sp+"/inbox/*/*", "", bearer(rt)...); s != 200 {
		t.Fatalf("to:r inbox: %d %s", s, body)
	}
	// Scope grammar: the recipient comes first.
	for _, bad := range []string{"to/answer:w", "me:rw", "to/u_x/a:w"} {
		if s, _, body := c.req("POST", sp+"/tokens", `{"allow":["`+bad+`"]}`, o...); s != 400 || errCode(body) != "invalid_scope" {
			t.Fatalf("scope %s: %d %s", bad, s, body)
		}
	}
	if s, _, body := c.req("POST", sp+"/tokens", `{"allow":["to/*/msg.*:w","to/me:rw"]}`, o...); s != 201 || !strings.Contains(body, `"to/*/msg.*:w"`) || !strings.Contains(body, `"to/me:rw"`) {
		t.Fatalf("valid to scopes: %d %s", s, body)
	}
	// write settings name to.
	if s, _, body := c.req("PATCH", sp, `{"public":"respond"}`, o...); s != 200 || !strings.Contains(body, `"to":"anyone"`) {
		t.Fatalf("respond: %d %s", s, body)
	}
}

func TestDirectedPulses(t *testing.T) {
	c := newTest(t)
	alice := c.create(`{"public":"all"}`, "")
	sp := "/v1/" + alice.Space
	a := bearer(alice.Token)
	bobTok, bobID := c.anon()
	caraTok, _ := c.anon()

	toAlice := c.listen(sp+"/inbox/*/rtc.*/pulse?timeout=2", a...)
	toCara := c.listen(sp+"/inbox/*/rtc.*/pulse?timeout=1", bearer(caraTok)...)
	allUsers := c.listen(sp+"/users/*/rtc.*/pulse?timeout=1", bearer(caraTok)...)
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
		t.Fatalf("users/* got %+v", p)
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
	alice := c.create(`{"public":"all"}`, "")
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
	own := c.create(`{"public":"all"}`, "")
	sp := "/v1/" + own.Space
	o := bearer(own.Token)
	bobTok, bobID := c.anon()
	b := bearer(bobTok)
	c.req("PUT", sp+"/users/me/msg.1", `"spam"`, b...)
	memberTok, _ := c.anon()
	c.req("POST", sp+"/grants", `{"user":"`+bobID+`","allow":"member"}`, o...)

	// Only owners ban, and owners can't be banned.
	if s, _, body := c.req("PUT", sp+"/bans/"+bobID, ``, bearer(memberTok)...); s != 403 || errCode(body) != "owner_only" {
		t.Fatalf("member bans: %d %s", s, body)
	}
	if s, _, body := c.req("PUT", sp+"/bans/"+own.ID, ``, o...); s != 409 || errCode(body) != "is_owner" {
		t.Fatalf("ban owner: %d %s", s, body)
	}
	// A live socket is closed when the ban lands.
	w := c.dial(own.Space, b...)
	w.call("1", `{"id":1,"method":"GET","path":"shared/x"}`)
	w.call("2", `{"id":2,"op":"will","path":"users/me/here/pulse","body":0}`)
	gone := c.listen(sp+"/users/*/here/pulse?timeout=1", o...)

	if s, _, body := c.req("PUT", sp+"/bans/"+bobID, `{"until":"1h"}`, o...); s != 201 || !strings.Contains(body, `"until":`) {
		t.Fatalf("ban: %d %s", s, body)
	}
	if s, _, _ := c.req("PUT", sp+"/bans/"+bobID, `{"until":null}`, o...); s != 200 {
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
	for _, r := range [][2]string{{"GET", "/shared/x"}, {"PUT", "/shared/x"}, {"GET", "/access"}, {"POST", "/users/me/x/pulse"}, {"GET", "/inbox/*/*"}} {
		s, _, body := c.req(r[0], sp+r[1], `1`, b...)
		if s != 403 || errCode(body) != "banned" || !strings.Contains(body, `"until":null`) {
			t.Fatalf("%s %s banned: %d %s", r[0], r[1], s, body)
		}
	}
	// Anonymous reading of a public space still works.
	if s, _, _ := c.req("GET", sp+"/users/"+bobID+"/msg.1", ""); s != 200 {
		t.Fatalf("anonymous read: %d", s)
	}
	// Nothing was deleted or revoked; the owner lists the ban.
	_, _, body := c.req("GET", sp+"/bans", "", o...)
	if !strings.Contains(body, bobID) || !strings.Contains(body, `"by":"`+own.ID+`"`) {
		t.Fatalf("bans list %s", body)
	}
	// Unbanning restores exactly what was there.
	if s, _, body := c.req("DELETE", sp+"/bans/"+bobID, "", o...); s != 200 || !strings.Contains(body, `"lifted":true`) {
		t.Fatalf("unban: %d %s", s, body)
	}
	if _, _, body := c.req("GET", sp+"/access", "", b...); !strings.Contains(body, `"allow":["*:rw"]`) {
		t.Fatalf("grants after unban %s", body)
	}
	if s, _, _ := c.req("PUT", sp+"/shared/x", `1`, b...); s != 201 {
		t.Fatalf("write after unban: %d", s)
	}
}

func TestBanBlocksJoin(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "") // private
	sp := "/v1/" + own.Space
	o := bearer(own.Token)
	_, link := c.mint(own.Space, own.Token, `{"uses":null}`)
	_, bobTok, _ := c.join(own.Space, link, "")
	_, bobID := c.anon()
	_ = bobID
	var who struct{ ID string }
	_, _, body := c.req("GET", "/v1/whoami", "", bearer(bobTok)...)
	json.Unmarshal([]byte(body), &who)

	// A member's invite stops working while they're banned, and comes back.
	_, memberLink := c.mint(own.Space, bobTok, ``)
	c.req("PUT", sp+"/bans/"+who.ID, ``, o...)
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
	c.req("DELETE", sp+"/bans/"+who.ID, "", o...)
	if s, _, body := c.join(own.Space, memberLink, ""); s != 201 {
		t.Fatalf("member's invite after unban: %d %s", s, body)
	}
	// A ban with an end lapses by itself.
	c.req("PUT", sp+"/bans/"+who.ID, `{"until":`+itoa(time.Now().Add(300*time.Millisecond).UnixMilli())+`}`, o...)
	if s, _, _ := c.req("GET", sp+"/shared/x", "", bearer(bobTok)...); s != 403 {
		t.Fatalf("timed ban: %d", s)
	}
	time.Sleep(400 * time.Millisecond)
	if s, _, body := c.req("GET", sp+"/access", "", bearer(bobTok)...); s != 200 {
		t.Fatalf("after lapse: %d %s", s, body)
	}
}
