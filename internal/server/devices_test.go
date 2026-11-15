package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type linkOut struct {
	ID, Code, Display, State string
	Expires                  int64
	Device                   *struct{ ID, Label string }
}

func (c *tc) link(tok, body string) linkOut {
	c.t.Helper()
	s, _, b := c.req("POST", "/v1/links", body, bearer(tok)...)
	if s != 201 {
		c.t.Fatalf("create link: %d %s", s, b)
	}
	var l linkOut
	json.Unmarshal([]byte(b), &l)
	return l
}

type redeemOut struct {
	Token, ID, Device string
	Previous          *string
	Deleted           bool
}

func (c *tc) redeem(body string, hdr ...string) (int, redeemOut, string) {
	c.t.Helper()
	s, _, b := c.req("POST", "/v1/links/redeem", body, hdr...)
	var r redeemOut
	json.Unmarshal([]byte(b), &r)
	return s, r, b
}

type devicesOut struct {
	ID, Device string
	Items      []struct {
		ID, Label string
		Current   bool
		LinkedBy  *string
	}
}

func (c *tc) devices(tok string) devicesOut {
	c.t.Helper()
	s, _, b := c.req("GET", "/v1/devices", "", bearer(tok)...)
	if s != 200 {
		c.t.Fatalf("devices: %d %s", s, b)
	}
	var d devicesOut
	json.Unmarshal([]byte(b), &d)
	return d
}

func TestDeviceLink(t *testing.T) {
	c := newTest(t)
	alice := c.create(`{"label":"notes"}`, "") // private, owned by alice
	l := c.link(alice.Token, "")
	if !strings.HasPrefix(l.Code, "lk_") || len(l.Code) != 13 || l.Display != l.Code[3:8]+"-"+l.Code[8:] || l.State != "open" {
		t.Fatalf("link %+v", l)
	}
	if left := l.Expires - time.Now().UnixMilli(); left < 590_000 || left > 600_000 {
		t.Fatalf("default ttl: %d ms left", left)
	}

	// A device with no token redeems the display form, as plain text.
	s, r, b := c.redeem(strings.ToLower(l.Display))
	if s != 201 || r.ID != alice.ID || r.Previous != nil || r.Deleted || !strings.HasPrefix(r.Device, "d_") {
		t.Fatalf("redeem: %d %s", s, b)
	}
	// The new device is alice everywhere: it reads her private space.
	if s, _, b := c.req("PUT", "/v1/"+alice.Space+"/shared/x", "1", bearer(r.Token)...); s != 201 {
		t.Fatalf("linked device writes: %d %s", s, b)
	}
	d := c.devices(r.Token)
	if d.ID != alice.ID || d.Device != r.Device || len(d.Items) != 2 || !d.Items[1].Current || d.Items[1].LinkedBy == nil || *d.Items[1].LinkedBy != d.Items[0].ID {
		t.Fatalf("devices %+v", d)
	}

	// Used once.
	if s, _, b := c.redeem(l.Code); s != 410 || errCode(b) != "link_used" {
		t.Fatalf("second redeem: %d %s", s, b)
	}
	// The status says who used it.
	_, _, b = c.req("GET", "/v1/links/"+l.ID, "", bearer(alice.Token)...)
	var st linkOut
	json.Unmarshal([]byte(b), &st)
	if st.State != "used" || st.Device == nil || st.Device.ID != r.Device {
		t.Fatalf("status %s", b)
	}
	// Nobody else sees it.
	other, _ := c.anon()
	if s, _, _ := c.req("GET", "/v1/links/"+l.ID, "", bearer(other)...); s != 404 {
		t.Fatalf("other's status: %d", s)
	}
}

func TestDeviceLinkReplaces(t *testing.T) {
	c := newTest(t)
	alice := c.create(`{"public":"all"}`, "")
	sp := "/v1/" + alice.Space

	// Bob has an identity with data, then links himself to alice.
	bobTok, bobID := c.anon()
	c.req("PUT", sp+"/users/me/name", `"bob"`, bearer(bobTok)...)
	l := c.link(alice.Token, `{"ttl":60}`)
	s, r, b := c.redeem(`{"code":"`+l.Code+`","label":"Bob's phone"}`, bearer(bobTok)...)
	if s != 201 || r.ID != alice.ID || r.Previous == nil || *r.Previous != bobID || !r.Deleted {
		t.Fatalf("redeem over data: %d %s", s, b)
	}
	// The old token is gone, and so is bob with his data.
	if s, _, b := c.req("GET", "/v1/whoami", "", bearer(bobTok)...); s != 401 || errCode(b) != "invalid_token" {
		t.Fatalf("old token: %d %s", s, b)
	}
	if s, _, _ := c.req("GET", sp+"/users/"+bobID+"/name", ""); s != 404 {
		t.Fatalf("bob's data: %d", s)
	}
	if d := c.devices(alice.Token); len(d.Items) != 2 || d.Items[1].Label != "Bob's phone" {
		t.Fatalf("label: %+v", d)
	}

	// A device of a visitor with other devices leaves only itself.
	l2 := c.link(alice.Token, "")
	_, r2, _ := c.redeem(l2.Code, bearer(r.Token)...) // same visitor
	if r2.Token != "" {
		t.Fatal("same visitor redeemed")
	}
	carolTok, carolID := c.anon()
	l3 := c.link(carolTok, "")
	s, r3, b := c.redeem(l3.Code, bearer(r.Token)...)
	if s != 201 || r3.ID != carolID || r3.Deleted || r3.Previous == nil || *r3.Previous != alice.ID {
		t.Fatalf("leave alice: %d %s", s, b)
	}
	if d := c.devices(alice.Token); len(d.Items) != 1 {
		t.Fatalf("alice keeps one device: %+v", d)
	}
	// l2 was never used up by the same-visitor attempt.
	if s, _, b := c.redeem(l2.Code); s != 201 {
		t.Fatalf("l2 after same_visitor: %d %s", s, b)
	}
}

func TestDeviceLinkErrors(t *testing.T) {
	c := newTest(t)
	tok, _ := c.anon()
	l := c.link(tok, "")

	if s, _, b := c.redeem(l.Code, bearer(tok)...); s != 409 || errCode(b) != "same_visitor" {
		t.Fatalf("same visitor: %d %s", s, b)
	}
	// Cancel, then redeeming says so.
	if s, _, b := c.req("DELETE", "/v1/links/"+l.ID, "", bearer(tok)...); s != 200 || !strings.Contains(b, `"cancelled"`) {
		t.Fatalf("cancel: %d %s", s, b)
	}
	if s, _, b := c.redeem(l.Code); s != 410 || errCode(b) != "link_cancelled" {
		t.Fatalf("cancelled: %d %s", s, b)
	}
	// TTL bounds.
	for _, body := range []string{`{"ttl":5}`, `{"ttl":7200}`, `{"ttl":"10m"}`, `{"uses":2}`} {
		if s, _, b := c.req("POST", "/v1/links", body, bearer(tok)...); s != 400 {
			t.Fatalf("%s: %d %s", body, s, b)
		}
	}
	// At most three open.
	for i := 0; i < 3; i++ {
		c.link(tok, "")
	}
	if s, _, b := c.req("POST", "/v1/links", "", bearer(tok)...); s != 409 || errCode(b) != "too_many_links" {
		t.Fatalf("fourth: %d %s", s, b)
	}
	// Creating needs a token, redeeming needs a code.
	if s, _, _ := c.req("POST", "/v1/links", ""); s != 401 {
		t.Fatalf("anonymous create: %d", s)
	}
	if s, _, b := c.redeem(""); s != 400 {
		t.Fatalf("empty: %d %s", s, b)
	}
	// Unknown codes cost the IP; the eleventh in ten minutes waits.
	for i := 0; i < 10; i++ {
		if s, _, b := c.redeem("ZZZZZ-ZZZZZ"); s != 404 || errCode(b) != "bad_link" {
			t.Fatalf("bad code %d: %d %s", i, s, b)
		}
	}
	if s, h, b := c.req("POST", "/v1/links/redeem", "ZZZZZ-ZZZZZ"); s != 429 || h.Get("Retry-After") == "" {
		t.Fatalf("eleventh: %d %s", s, b)
	}
}

func TestLinkStatusWaits(t *testing.T) {
	c := newTest(t)
	tok, _ := c.anon()
	l := c.link(tok, "")
	done := make(chan string, 1)
	go func() {
		_, _, b := c.req("GET", "/v1/links/"+l.ID+"?wait=10", "", bearer(tok)...)
		done <- b
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	_, r, _ := c.redeem(l.Code)
	select {
	case b := <-done:
		if !strings.Contains(b, `"used"`) || !strings.Contains(b, r.Device) || time.Since(start) > 2*time.Second {
			t.Fatalf("waited status: %s", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("status didn't wake")
	}
	if s, _, _ := c.req("GET", "/v1/links/"+l.ID+"?wait=61", "", bearer(tok)...); s != 400 {
		t.Fatalf("wait 61: %d", s)
	}
}

func TestSignOutDevices(t *testing.T) {
	c := newTest(t)
	alice := c.create(`{"public":"all"}`, "")
	_, phone, _ := c.redeem(c.link(alice.Token, "").Code)
	_, laptop, _ := c.redeem(c.link(alice.Token, "").Code)

	// The phone's live connection loses its identity when signed out.
	w := c.dial(alice.Space, bearer(phone.Token)...)
	if f := w.call("1", `{"id":1,"method":"PUT","path":"shared/x","body":1}`); f.Status != 201 {
		t.Fatalf("phone writes live: %d %s", f.Status, f.Body)
	}
	if s, _, b := c.req("DELETE", "/v1/devices/"+phone.Device, "", bearer(laptop.Token)...); s != 200 {
		t.Fatalf("laptop signs phone out: %d %s", s, b)
	}
	if s, _, b := c.req("GET", "/v1/whoami", "", bearer(phone.Token)...); s != 401 || errCode(b) != "invalid_token" {
		t.Fatalf("phone token after: %d %s", s, b)
	}
	if f := w.call("2", `{"id":2,"method":"PUT","path":"shared/x","body":2}`); f.code() != "invalid_token" {
		t.Fatalf("phone writes live after: %d %s", f.Status, f.Body)
	}

	// Rename, then sign out everything else from the laptop.
	if s, _, b := c.req("PATCH", "/v1/devices/me", `{"label":"Laptop"}`, bearer(laptop.Token)...); s != 200 || !strings.Contains(b, `"Laptop"`) {
		t.Fatalf("rename: %d %s", s, b)
	}
	if s, _, b := c.req("DELETE", "/v1/devices", "", bearer(laptop.Token)...); s != 400 {
		t.Fatalf("without others=1: %d %s", s, b)
	}
	if s, _, b := c.req("DELETE", "/v1/devices?others=1", "", bearer(laptop.Token)...); s != 200 || !strings.Contains(b, `"removed":["d_`) {
		t.Fatalf("others: %d %s", s, b)
	}
	if s, _, _ := c.req("GET", "/v1/whoami", "", bearer(alice.Token)...); s != 401 {
		t.Fatalf("first device after others: %d", s)
	}
	// The laptop is alice now, and the last device stays.
	if d := c.devices(laptop.Token); d.ID != alice.ID || len(d.Items) != 1 {
		t.Fatalf("after others: %+v", d)
	}
	if s, _, b := c.req("DELETE", "/v1/devices/me", "", bearer(laptop.Token)...); s != 409 || errCode(b) != "last_device" {
		t.Fatalf("last: %d %s", s, b)
	}
	// Someone else's device is not found.
	bob, _ := c.anon()
	if s, _, _ := c.req("DELETE", "/v1/devices/"+laptop.Device, "", bearer(bob)...); s != 404 {
		t.Fatalf("other's device: %d", s)
	}
	// Owner rights came along.
	if s, _, b := c.req("GET", "/v1/"+alice.Space+"/access", "", bearer(laptop.Token)...); !strings.Contains(b, `"owner":true`) {
		t.Fatalf("laptop access: %d %s", s, b)
	}
}
