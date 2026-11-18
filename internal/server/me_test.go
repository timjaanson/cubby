package server

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/timjaanson/cubby/internal/auth"
)

type pairingOut struct {
	ID, Code, Display, State string
	Expires                  int64
	Device                   *struct{ ID, Label string }
}

func (c *tc) pairingCode(tok, body string) pairingOut {
	c.t.Helper()
	s, _, b := c.req("POST", "/v1/me/pairing-codes", body, bearer(tok)...)
	if s != 201 {
		c.t.Fatalf("create pairing code: %d %s", s, b)
	}
	var l pairingOut
	json.Unmarshal([]byte(b), &l)
	return l
}

type pairedOut struct {
	ID, Device string
	Previous   *string
	Deleted    bool
}

// pair uses a code, signed by s with its key in the header. It returns a
// token from s for after.
func (c *tc) pair(s *auth.Signer, body string) (int, pairedOut, string, string) {
	c.t.Helper()
	if !strings.HasPrefix(body, "{") {
		body = `{"code":"` + body + `"}`
	}
	st, _, b := c.req("POST", "/v1/pair", body, bearer(s.Token(c.url, time.Minute, true))...)
	var r pairedOut
	json.Unmarshal([]byte(b), &r)
	return st, r, b, s.Token(c.url, time.Hour, false)
}

// newDevice pairs a fresh key with code and returns its token and device ID.
func (c *tc) newDevice(code string) (string, string) {
	c.t.Helper()
	s, _ := auth.NewSigner()
	st, r, b, tok := c.pair(s, code)
	if st != 201 {
		c.t.Fatalf("pair: %d %s", st, b)
	}
	return tok, r.Device
}

type devicesOut struct {
	ID, Device string
	Items      []struct {
		ID, Label string
		Current   bool
		PairedBy  *string
	}
}

func (c *tc) devices(tok string) devicesOut {
	c.t.Helper()
	s, _, b := c.req("GET", "/v1/me/devices", "", bearer(tok)...)
	if s != 200 {
		c.t.Fatalf("devices: %d %s", s, b)
	}
	var d devicesOut
	json.Unmarshal([]byte(b), &d)
	return d
}

func TestRegisterVisitor(t *testing.T) {
	c := newTest(t)
	s, _ := auth.NewSigner()
	reg := func(tok string) (int, string) {
		st, _, b := c.req("POST", "/v1/visitors", `{"label":"Laptop"}`, bearer(tok)...)
		return st, b
	}
	st, b := reg(s.Token(c.url, time.Minute, true))
	var out struct{ ID, Device string }
	json.Unmarshal([]byte(b), &out)
	if st != 201 || out.Device != s.Key.ID || out.ID == "" {
		t.Fatalf("register: %d %s", st, b)
	}
	// Registering again is harmless and names the same visitor.
	if st, b := reg(s.Token(c.url, time.Minute, true)); st != 200 || !strings.Contains(b, out.ID) {
		t.Fatalf("again: %d %s", st, b)
	}
	tok := s.Token(c.url, time.Hour, false)
	if _, _, b := c.req("GET", "/v1/me", "", bearer(tok)...); !strings.Contains(b, `"device":"`+s.Key.ID+`"`) || !strings.Contains(b, out.ID) {
		t.Fatalf("me: %s", b)
	}
	if d := c.devices(tok); len(d.Items) != 1 || d.Items[0].Label != "Laptop" || !d.Items[0].Current {
		t.Fatalf("devices: %+v", d)
	}

	// EdDSA keys work the same way.
	ed, _ := auth.NewEdSigner()
	if st, b := reg(ed.Token(c.url, time.Minute, true)); st != 201 {
		t.Fatalf("EdDSA: %d %s", st, b)
	}
	if st, _, b := c.req("GET", "/v1/me", "", bearer(ed.Token(c.url, time.Hour, false))...); st != 200 {
		t.Fatalf("EdDSA token: %d %s", st, b)
	}

	// Refused: no key in the header, the private key, a wrong kid, another
	// server's audience, and a token signed by some other key.
	other, _ := auth.NewSigner()
	hdr := func(h map[string]any) string {
		hb, _ := json.Marshal(h)
		pb, _ := json.Marshal(map[string]any{"aud": c.url, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()})
		return b64(hb) + "." + b64(pb) + ".AAAA"
	}
	for name, tc := range map[string]struct{ tok, code string }{
		"no jwk":       {other.Token(c.url, time.Minute, false), "bad_token"},
		"private jwk":  {hdr(map[string]any{"alg": "ES256", "jwk": json.RawMessage(other.PrivateJWK())}), "bad_key"},
		"kid mismatch": {hdr(map[string]any{"alg": "ES256", "kid": s.Key.ID, "jwk": json.RawMessage(other.Key.JWK)}), "bad_token"},
		"other server": {other.Token("https://elsewhere.example", time.Minute, true), "wrong_audience"},
		"bad sig":      {hdr(map[string]any{"alg": "ES256", "jwk": json.RawMessage(other.Key.JWK)}), "bad_signature"},
		"too long":     {other.Token(c.url, 2*time.Hour, true), "token_too_long"},
	} {
		if st, b := reg(tc.tok); st < 400 || errCode(b) != tc.code {
			t.Errorf("%s: %d %s", name, st, b)
		}
	}
	// A signed-out key is unknown: its tokens stop working.
	s2, _ := auth.NewSigner()
	l := c.pairingCode(tok, "")
	c.pair(s2, l.Code)
	if st, _, b := c.req("DELETE", "/v1/me/devices/"+s2.Key.ID, "", bearer(tok)...); st != 200 {
		t.Fatalf("sign out: %d %s", st, b)
	}
	if st, _, b := c.req("GET", "/v1/me", "", bearer(s2.Token(c.url, time.Hour, false))...); st != 401 || errCode(b) != "unknown_key" {
		t.Fatalf("signed-out key: %d %s", st, b)
	}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func TestPairing(t *testing.T) {
	c := newTest(t)
	alice := c.create(`{"prefix":"notes"}`, "") // private, owned by alice
	l := c.pairingCode(alice.Token, "")
	if len(l.Code) != 10 || l.Display != l.Code[:5]+"-"+l.Code[5:] || l.State != "open" || !strings.HasPrefix(l.ID, "pc_") {
		t.Fatalf("pairing code %+v", l)
	}
	if left := l.Expires - time.Now().UnixMilli(); left < 590_000 || left > 600_000 {
		t.Fatalf("default ttl: %d ms left", left)
	}

	// A new key uses the display form, as plain text.
	s, _ := auth.NewSigner()
	st, r, b, tok := c.pair(s, strings.ToLower(l.Display))
	if st != 201 || r.ID != alice.ID || r.Previous != nil || r.Deleted || r.Device != s.Key.ID {
		t.Fatalf("pair: %d %s", st, b)
	}
	// The new device is alice everywhere: it writes her private space.
	if st, _, b := c.req("PUT", "/v1/"+alice.Space+"/shared/x", "1", bearer(tok)...); st != 201 {
		t.Fatalf("paired device writes: %d %s", st, b)
	}
	d := c.devices(tok)
	if d.ID != alice.ID || d.Device != r.Device || len(d.Items) != 2 || !d.Items[1].Current || d.Items[1].PairedBy == nil || *d.Items[1].PairedBy != d.Items[0].ID {
		t.Fatalf("devices %+v", d)
	}
	// Used once.
	s3, _ := auth.NewSigner()
	if st, _, b, _ := c.pair(s3, l.Code); st != 410 || errCode(b) != "pairing_code_used" {
		t.Fatalf("second use: %d %s", st, b)
	}
	// The status says who used it; nobody else sees it.
	_, _, b = c.req("GET", "/v1/me/pairing-codes/"+l.ID, "", bearer(alice.Token)...)
	var stat pairingOut
	json.Unmarshal([]byte(b), &stat)
	if stat.State != "used" || stat.Device == nil || stat.Device.ID != r.Device {
		t.Fatalf("status %s", b)
	}
	other, _ := c.anon()
	if st, _, _ := c.req("GET", "/v1/me/pairing-codes/"+l.ID, "", bearer(other)...); st != 404 {
		t.Fatalf("other's status: %d", st)
	}
}

func TestPairingMovesAKey(t *testing.T) {
	c := newTest(t)
	alice := c.create(`{"anyone":"all"}`, "")
	sp := "/v1/" + alice.Space

	// Bob has an identity with data, then pairs his key with alice.
	bobTok, bobID, bobKey := c.visitor()
	c.req("PUT", sp+"/by/me/name", `"bob"`, bearer(bobTok)...)
	l := c.pairingCode(alice.Token, `{"ttl":60}`)
	st, _, b := c.req("POST", "/v1/pair", `{"code":"`+l.Code+`","label":"Bob's phone"}`, bearer(bobTok)...) // a known key: no jwk needed
	var r pairedOut
	json.Unmarshal([]byte(b), &r)
	if st != 201 || r.ID != alice.ID || r.Device != bobKey.Key.ID || r.Previous == nil || *r.Previous != bobID || !r.Deleted {
		t.Fatalf("pair over data: %d %s", st, b)
	}
	// The same key is alice now, and bob is gone with his data.
	if _, _, b := c.req("GET", "/v1/me", "", bearer(bobTok)...); !strings.Contains(b, alice.ID) {
		t.Fatalf("key after: %s", b)
	}
	if st, _, _ := c.req("GET", sp+"/by/"+bobID+"/name", ""); st != 404 {
		t.Fatalf("bob's data: %d", st)
	}
	if d := c.devices(alice.Token); len(d.Items) != 2 || d.Items[1].Label != "Bob's phone" {
		t.Fatalf("label: %+v", d)
	}
	// Same visitor: refused, the code stays usable.
	l2 := c.pairingCode(alice.Token, "")
	if st, _, b, _ := c.pair(bobKey, l2.Code); st != 409 || errCode(b) != "same_visitor" {
		t.Fatalf("same visitor: %d %s", st, b)
	}
	// A device of a visitor with other devices leaves only itself.
	carolTok, carolID := c.anon()
	st, r3, b, _ := c.pair(bobKey, c.pairingCode(carolTok, "").Code)
	if st != 201 || r3.ID != carolID || r3.Deleted || r3.Previous == nil || *r3.Previous != alice.ID {
		t.Fatalf("leave alice: %d %s", st, b)
	}
	if d := c.devices(alice.Token); len(d.Items) != 1 {
		t.Fatalf("alice keeps one device: %+v", d)
	}
	if _, dev := c.newDevice(l2.Code); dev == "" {
		t.Fatal("l2 after same_visitor")
	}
}

func TestPairingErrors(t *testing.T) {
	c := newTest(t)
	tok, _ := c.anon()
	l := c.pairingCode(tok, "")
	// Cancel, then using it says so.
	if st, _, b := c.req("DELETE", "/v1/me/pairing-codes/"+l.ID, "", bearer(tok)...); st != 200 || !strings.Contains(b, `"cancelled"`) {
		t.Fatalf("cancel: %d %s", st, b)
	}
	s, _ := auth.NewSigner()
	if st, _, b, _ := c.pair(s, l.Code); st != 410 || errCode(b) != "pairing_code_cancelled" {
		t.Fatalf("cancelled: %d %s", st, b)
	}
	for _, body := range []string{`{"ttl":5}`, `{"ttl":7200}`, `{"ttl":"2h"}`, `{"uses":2}`} {
		if st, _, b := c.req("POST", "/v1/me/pairing-codes", body, bearer(tok)...); st != 400 {
			t.Fatalf("%s: %d %s", body, st, b)
		}
	}
	for range 3 {
		c.pairingCode(tok, "")
	}
	if st, _, b := c.req("POST", "/v1/me/pairing-codes", "", bearer(tok)...); st != 409 || errCode(b) != "too_many_pairing_codes" {
		t.Fatalf("fourth: %d %s", st, b)
	}
	// Making a code needs a device, using one needs a key and a code.
	if st, _, _ := c.req("POST", "/v1/me/pairing-codes", ""); st != 401 {
		t.Fatalf("anonymous create: %d", st)
	}
	if st, _, b := c.req("POST", "/v1/pair", `{"code":"ZZZZZ-ZZZZZ"}`); st != 401 || errCode(b) != "missing_token" {
		t.Fatalf("unsigned pair: %d %s", st, b)
	}
	if st, _, b, _ := c.pair(s, ""); st != 400 {
		t.Fatalf("empty: %d %s", st, b)
	}
	// Unknown codes cost the IP; the eleventh in ten minutes waits.
	for i := range 10 {
		if st, _, b, _ := c.pair(s, "ZZZZZ-ZZZZZ"); st != 404 || errCode(b) != "bad_pairing_code" {
			t.Fatalf("bad code %d: %d %s", i, st, b)
		}
	}
	if st, _, b, _ := c.pair(s, "ZZZZZ-ZZZZZ"); st != 429 {
		t.Fatalf("eleventh: %d %s", st, b)
	}
}

func TestPairingStatusWaits(t *testing.T) {
	c := newTest(t)
	tok, _ := c.anon()
	l := c.pairingCode(tok, "")
	done := make(chan string, 1)
	go func() {
		_, _, b := c.req("GET", "/v1/me/pairing-codes/"+l.ID+"?timeout=10", "", bearer(tok)...)
		done <- b
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	_, dev := c.newDevice(l.Code)
	select {
	case b := <-done:
		if !strings.Contains(b, `"used"`) || !strings.Contains(b, dev) || time.Since(start) > 2*time.Second {
			t.Fatalf("waited status: %s", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("status didn't wake")
	}
	if st, _, _ := c.req("GET", "/v1/me/pairing-codes/"+l.ID+"?timeout=61", "", bearer(tok)...); st != 400 {
		t.Fatalf("wait 61: %d", st)
	}
}

func TestSignOutDevices(t *testing.T) {
	c := newTest(t)
	alice := c.create(`{"anyone":"all"}`, "")
	phone, phoneID := c.newDevice(c.pairingCode(alice.Token, "").Code)
	laptop, laptopID := c.newDevice(c.pairingCode(alice.Token, "").Code)

	// The phone's live connection loses its identity when signed out.
	w := c.dial(alice.Space, bearer(phone)...)
	if f := w.call("1", `{"id":1,"method":"PUT","path":"shared/x","body":1}`); f.Status != 201 {
		t.Fatalf("phone writes live: %d %s", f.Status, f.Body)
	}
	if st, _, b := c.req("DELETE", "/v1/me/devices/"+phoneID, "", bearer(laptop)...); st != 200 {
		t.Fatalf("laptop signs phone out: %d %s", st, b)
	}
	if st, _, b := c.req("GET", "/v1/me", "", bearer(phone)...); st != 401 || errCode(b) != "unknown_key" {
		t.Fatalf("phone token after: %d %s", st, b)
	}
	if f := w.call("2", `{"id":2,"method":"PUT","path":"shared/x","body":2}`); f.code() != "unknown_key" {
		t.Fatalf("phone writes live after: %d %s", f.Status, f.Body)
	}

	if st, _, b := c.req("PATCH", "/v1/me/devices/current", `{"label":"Laptop"}`, bearer(laptop)...); st != 200 || !strings.Contains(b, `"Laptop"`) {
		t.Fatalf("rename: %d %s", st, b)
	}
	if st, _, b := c.req("DELETE", "/v1/me/devices", "", bearer(laptop)...); st != 400 {
		t.Fatalf("without others=1: %d %s", st, b)
	}
	if st, _, b := c.req("DELETE", "/v1/me/devices?others=1", "", bearer(laptop)...); st != 200 || !strings.Contains(b, `"removed":["`) {
		t.Fatalf("others: %d %s", st, b)
	}
	if st, _, _ := c.req("GET", "/v1/me", "", bearer(alice.Token)...); st != 401 {
		t.Fatalf("first device after others: %d", st)
	}
	if d := c.devices(laptop); d.ID != alice.ID || len(d.Items) != 1 {
		t.Fatalf("after others: %+v", d)
	}
	if st, _, b := c.req("DELETE", "/v1/me/devices/current", "", bearer(laptop)...); st != 409 || errCode(b) != "last_device" {
		t.Fatalf("last: %d %s", st, b)
	}
	bob, _ := c.anon()
	if st, _, _ := c.req("DELETE", "/v1/me/devices/"+laptopID, "", bearer(bob)...); st != 404 {
		t.Fatalf("other's device: %d", st)
	}
	if _, _, b := c.req("GET", "/v1/"+alice.Space, "", bearer(laptop)...); !strings.Contains(b, `"access":"owner"`) {
		t.Fatalf("laptop access: %s", b)
	}
}

func TestAPIKeys(t *testing.T) {
	c := newTest(t)
	own := c.create(`{"prefix":"hook"}`, "")
	other := c.create(`{"prefix":"other"}`, own.Token)
	sp := "/v1/" + own.Space
	o := bearer(own.Token)

	mk := func(body string) (int, map[string]any, string) {
		st, _, b := c.req("POST", "/v1/me/api-keys", body, o...)
		var out map[string]any
		json.Unmarshal([]byte(b), &out)
		return st, out, b
	}
	st, k, b := mk(`{"space":"` + own.Space + `","label":"github","access":"post"}`)
	key, _ := k["key"].(string)
	month := float64(time.Now().Add(30 * 24 * time.Hour).UnixMilli())
	if st != 201 || !strings.HasPrefix(key, "cubby_") || k["access"] != "post" || k["expires"] == nil ||
		k["expires"].(float64) < month-60_000 || k["expires"].(float64) > month+60_000 || k["warning"] != nil {
		t.Fatalf("mint: %d %s", st, b)
	}
	kb := bearer(key)
	// It writes its own keys in its space, as the visitor, at post.
	if st, _, b := c.req("PUT", sp+"/by/me/event", `{"n":1}`, kb...); st != 201 {
		t.Fatalf("api key write: %d %s", st, b)
	}
	if st, _, b := c.req("PUT", sp+"/shared/x", `1`, kb...); st != 403 || errCode(b) != "no_access" {
		t.Fatalf("api key above its level: %d %s", st, b)
	}
	if st, _, b := c.req("GET", sp+"/by/me/event?token="+key, ""); st != 200 || !strings.Contains(b, `"n":1`) {
		t.Fatalf("api key in the URL: %d %s", st, b)
	}
	if _, _, b := c.req("GET", sp, "", kb...); !strings.Contains(b, `"access":"post"`) || !strings.Contains(b, `"canInvite":false`) {
		t.Fatalf("access as api key: %s", b)
	}
	// Nothing outside its space, nothing but data.
	for _, r := range []struct{ method, path, body, code string }{
		{"GET", "/v1/" + other.Space + "/shared/x", "", "api_key_space"},
		{"GET", "/v1/me", "", "api_key_not_allowed"},
		{"POST", "/v1/me/api-keys", `{"space":"` + own.Space + `"}`, "api_key_not_allowed"},
		{"POST", "/v1/spaces", ``, "api_key_not_allowed"},
		{"POST", sp + "/invites", ``, "api_key_not_allowed"},
		{"GET", sp + "/members", ``, "api_key_not_allowed"},
		{"POST", sp + "/join", `{"invite":"x"}`, "api_key_not_allowed"},
		{"PATCH", sp, `{"anyone":"all"}`, "api_key_not_allowed"},
		{"POST", "/v1/pair", `x`, "api_key_not_allowed"},
	} {
		if st, _, b := c.req(r.method, r.path, r.body, kb...); st != 403 || errCode(b) != r.code {
			t.Errorf("%s %s: %d %s", r.method, r.path, st, b)
		}
	}
	// No live connection with an API key.
	w := c.dial(own.Space)
	if f := w.call("1", `{"id":1,"op":"auth","token":"`+key+`"}`); f.code() != "api_key_not_allowed" {
		t.Fatalf("live auth: %s", f.Body)
	}

	// Never owner, never above the minter, never for a space they can't see.
	for _, body := range []string{
		`{"space":"` + own.Space + `","access":"owner"}`,
		`{"label":"no space"}`,
		`{"space":"` + own.Space + `","ttl":"yesterday"}`,
	} {
		if st, _, b := mk(body); st != 400 {
			t.Errorf("%s: %d %s", body, st, b)
		}
	}
	reader, _ := c.anon()
	_, inv := c.mint(own.Space, own.Token, `{"access":"read"}`)
	c.join(own.Space, inv, reader)
	if st, _, b := c.req("POST", "/v1/me/api-keys", `{"space":"`+own.Space+`","access":"post"}`, bearer(reader)...); st != 403 || errCode(b) != "exceeds_rights" {
		t.Fatalf("above the minter: %d %s", st, b)
	}
	if st, _, b := c.req("POST", "/v1/me/api-keys", `{"space":"`+other.Space+`"}`, bearer(reader)...); st != 404 {
		t.Fatalf("a space they can't see: %d %s", st, b)
	}
	// A permanent one takes an explicit null and says what it costs.
	st, k2, b := mk(`{"space":"` + own.Space + `","ttl":null,"access":"all"}`)
	if st != 201 || k2["expires"] != nil || k2["warning"] == nil {
		t.Fatalf("permanent: %d %s", st, b)
	}
	// Listed without secrets; deleting revokes.
	_, _, b = c.req("GET", "/v1/me/api-keys", "", o...)
	if strings.Count(b, `"id":"ak_`) != 2 || strings.Contains(b, "cubby_") {
		t.Fatalf("list: %s", b)
	}
	if st, _, _ := c.req("DELETE", "/v1/me/api-keys/"+k["id"].(string), "", o...); st != 200 {
		t.Fatal("delete")
	}
	if st, _, b := c.req("GET", "/v1/me", "", kb...); st != 401 || errCode(b) != "invalid_api_key" {
		t.Fatalf("deleted key: %d %s", st, b)
	}
	// Signing out the device that minted a key ends the key.
	l := c.pairingCode(own.Token, "")
	phone, phoneID := c.newDevice(l.Code)
	st, k3, _ := func() (int, map[string]any, string) {
		st, _, b := c.req("POST", "/v1/me/api-keys", `{"space":"`+own.Space+`"}`, bearer(phone)...)
		var out map[string]any
		json.Unmarshal([]byte(b), &out)
		return st, out, b
	}()
	c.req("DELETE", "/v1/me/devices/"+phoneID, "", o...)
	if st, _, b := c.req("GET", "/v1/me", "", bearer(k3["key"].(string))...); st != 401 || errCode(b) != "invalid_api_key" {
		t.Fatalf("key of a signed-out device: %d %s", st, b)
	}
	// Deleting the space deletes its keys.
	key2 := k2["key"].(string)
	c.req("DELETE", sp, "", o...)
	if st, _, b := c.req("GET", "/v1/me", "", bearer(key2)...); st != 401 || errCode(b) != "invalid_api_key" {
		t.Fatalf("key of a deleted space: %d %s", st, b)
	}
}
