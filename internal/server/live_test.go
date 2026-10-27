package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type frame struct {
	ID      json.RawMessage   `json:"id"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
	Text    bool              `json:"text"`
}

func (f frame) code() string { return errCode(string(f.Body)) }

type ws struct {
	t      *testing.T
	conn   *websocket.Conn
	frames chan frame
	closed chan error
	held   []frame
}

func (c *tc) dial(space string, hdr ...string) *ws {
	c.t.Helper()
	h := http.Header{}
	for i := 0; i+1 < len(hdr); i += 2 {
		h.Set(hdr[i], hdr[i+1])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(c.url, "http")+"/v1/"+space+"/live", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { conn.CloseNow() })
	w := &ws{t: c.t, conn: conn, frames: make(chan frame, 100), closed: make(chan error, 1)}
	go func() {
		for {
			_, b, err := conn.Read(context.Background())
			if err != nil {
				w.closed <- err
				return
			}
			var f frame
			if json.Unmarshal(b, &f) != nil {
				f.Status = -1
			}
			w.frames <- f
		}
	}()
	return w
}

func (w *ws) send(m string) {
	w.t.Helper()
	if err := w.conn.Write(context.Background(), websocket.MessageText, []byte(m)); err != nil {
		w.t.Fatal(err)
	}
}

func (w *ws) next(timeout time.Duration) (frame, bool) {
	w.t.Helper()
	if len(w.held) > 0 {
		f := w.held[0]
		w.held = w.held[1:]
		return f, true
	}
	select {
	case f := <-w.frames:
		return f, true
	case <-time.After(timeout):
		return frame{}, false
	}
}

func (w *ws) readErr() error {
	select {
	case err := <-w.closed:
		return err
	case <-time.After(2 * time.Second):
		return nil
	}
}

// reply waits for the frame with id, keeping others for later.
func (w *ws) reply(id string) frame {
	w.t.Helper()
	var skipped []frame
	defer func() { w.held = append(skipped, w.held...) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(w.held) > 0 {
			f := w.held[0]
			w.held = w.held[1:]
			if string(f.ID) == id {
				return f
			}
			skipped = append(skipped, f)
			continue
		}
		f, ok := w.next(time.Until(deadline))
		if !ok {
			break
		}
		if string(f.ID) == id {
			return f
		}
		skipped = append(skipped, f)
	}
	w.t.Fatalf("no reply for id %s", id)
	return frame{}
}

func (w *ws) call(id, m string) frame {
	w.t.Helper()
	w.send(m)
	return w.reply(id)
}

func watchBody(t *testing.T, f frame) (changes []map[string]any, version int64) {
	t.Helper()
	var b struct {
		Changes []map[string]any
		Version int64
	}
	if f.Status != 200 || json.Unmarshal(f.Body, &b) != nil {
		t.Fatalf("watch frame %d %s", f.Status, f.Body)
	}
	return b.Changes, b.Version
}

func TestLiveRouteHTTPErrors(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "")
	sp := "/v1/" + own.Space + "/live"
	if s, _, b := c.req("GET", sp, ""); s != 426 || errCode(b) != "upgrade_required" {
		t.Fatalf("plain GET: %d %s", s, b)
	}
	up := []string{"Upgrade", "websocket", "Connection", "Upgrade", "Sec-WebSocket-Version", "13", "Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ=="}
	if s, _, b := c.req("GET", sp+"?token="+own.Token, "", up...); s != 400 || !strings.Contains(b, "auth") {
		t.Fatalf("token in URL: %d %s", s, b)
	}
	if s, _, b := c.req("GET", sp, "", append(up, bearer("vt_"+strings.Repeat("x", 43))...)...); s != 401 || errCode(b) != "invalid_token" {
		t.Fatalf("bad header token: %d %s", s, b)
	}
	if s, _, b := c.req("POST", sp, ""); s != 405 {
		t.Fatalf("POST: %d %s", s, b)
	}
}

func TestLiveRequestsMatchHTTP(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "")
	w := c.dial(own.Space)
	if f := w.call("1", `{"id":1,"op":"auth","token":"`+own.Token+`"}`); f.Status != 200 || !strings.Contains(string(f.Body), own.ID) {
		t.Fatalf("auth: %d %s", f.Status, f.Body)
	}

	f := w.call("2", `{"id":2,"method":"PUT","path":"shared/note","query":{"ttl":30,"ifVersion":0},"body":{"text":"milk", "n": 1.50}}`)
	if f.Status != 201 || f.Headers["ETag"] == "" || f.Headers["KV-Expires"] == "" || !strings.Contains(f.Headers["RateLimit"], `"visitor";r=29`) {
		t.Fatalf("put: %d %v %s", f.Status, f.Headers, f.Body)
	}
	// Same bytes as HTTP GET, inside the envelope.
	_, _, httpBody := c.req("GET", "/v1/"+own.Space+"/shared/note", "", bearer(own.Token)...)
	f = w.call(`"g"`, `{"id":"g","method":"GET","path":"shared/note"}`)
	if f.Status != 200 || string(f.Body) != httpBody || f.Text || f.Headers["KV-By"] != own.ID || f.Headers["KV-Time"] == "" {
		t.Fatalf("get: %d %v %s (http %s)", f.Status, f.Headers, f.Body, httpBody)
	}
	// Text stays text.
	w.call("3", `{"id":3,"method":"PUT","path":"shared/t","text":"plain words"}`)
	if f = w.call("4", `{"id":4,"method":"GET","path":"shared/t"}`); !f.Text || string(f.Body) != `"plain words"` {
		t.Fatalf("text get: %v %s", f.Text, f.Body)
	}
	if f = w.call("5", `{"id":5,"method":"GET","path":"shared/*"}`); f.Status != 200 || !strings.Contains(string(f.Body), `"items"`) {
		t.Fatalf("list: %d %s", f.Status, f.Body)
	}
	if f = w.call("6", `{"id":6,"method":"POST","path":"shared/temp/series","body":21.5}`); f.Status != 201 {
		t.Fatalf("series add: %d %s", f.Status, f.Body)
	}
	if f = w.call("7", `{"id":7,"method":"GET","path":"shared/temp/series"}`); f.Status != 200 || !strings.Contains(string(f.Body), "21.5") {
		t.Fatalf("series read: %d %s", f.Status, f.Body)
	}
	if f = w.call("8", `{"id":8,"method":"DELETE","path":"shared/t","query":{"ifVersion":"1"}}`); f.Status != 412 {
		t.Fatalf("conditional delete: %d %s", f.Status, f.Body)
	}
	if f = w.call("9", `{"id":9,"method":"DELETE","path":"shared/t"}`); f.Status != 200 {
		t.Fatalf("delete: %d %s", f.Status, f.Body)
	}
	if f = w.call("10", `{"id":10,"method":"GET","path":"shared/t"}`); f.Status != 404 || f.code() != "not_found" {
		t.Fatalf("deleted get: %d %s", f.Status, f.Body)
	}
	// Mistakes get the HTTP errors, or bad_message for the envelope.
	for _, m := range []struct{ msg, code string }{
		{`{"id":11,"method":"PUT","path":"shared/k","query":{"ifversion":0},"body":1}`, "invalid_parameter"},
		{`{"id":11,"method":"GET","path":"access"}`, "bad_message"},
		{`{"id":11,"method":"GET","path":"shared/k?x=1"}`, "bad_message"},
		{`{"id":11,"method":"GET","path":"shared/k","query":{"token":"vt_x"}}`, "invalid_parameter"},
		{`{"id":11,"method":"GET","path":"shared/k","body":1}`, "bad_message"},
		{`{"id":11,"method":"PATCH","path":"shared/k"}`, "bad_message"},
		{`{"id":11,"op":"dance"}`, "bad_message"},
		{`{"id":11,"method":"GET","path":"shared/k","ifVersion":3}`, "bad_message"},
		{`{"id":11,"method":"GET","path":"shared/k/watch","query":{"timeout":5}}`, "invalid_parameter"},
	} {
		if f := w.call("11", m.msg); f.code() != m.code {
			t.Errorf("%s: %d %s", m.msg, f.Status, f.Body)
		}
	}
	if f, _ := w.next(time.Second); f.Status != 0 {
		t.Fatalf("unexpected frame %s", f.Body)
	}
	w.send(`not json`)
	if f, _ := w.next(time.Second); f.code() != "bad_message" || f.ID != nil {
		t.Fatalf("garbage: %d %s %s", f.Status, f.ID, f.Body)
	}
	if f := w.call("12", `{"id":12,"op":"ping"}`); f.Status != 200 {
		t.Fatalf("ping %d", f.Status)
	}
}

func TestLiveAccessPerMessage(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "")
	c.req("PUT", "/v1/"+own.Space+"/shared/k", `1`, bearer(own.Token)...)
	stranger, _ := c.anon()

	// A stranger connects fine, and every message answers like HTTP.
	w := c.dial(own.Space)
	w.call("1", `{"id":1,"op":"auth","token":"`+stranger+`"}`)
	_, _, httpBody := c.req("GET", "/v1/"+own.Space+"/shared/k", "", bearer(stranger)...)
	if f := w.call("2", `{"id":2,"method":"GET","path":"shared/k"}`); f.Status != 404 || string(f.Body) != strings.TrimSpace(httpBody) {
		t.Fatalf("stranger: %d %s, http %s", f.Status, f.Body, httpBody)
	}
	// After joining over HTTP, the same connection can read.
	_, tok := c.mint(own.Space, own.Token, `{}`)
	if s, _, b := c.join(own.Space, tok, stranger); s != 200 {
		t.Fatalf("join %d %s", s, b)
	}
	if f := w.call("3", `{"id":3,"method":"GET","path":"shared/k"}`); f.Status != 200 {
		t.Fatalf("after join: %d %s", f.Status, f.Body)
	}
	// A running watch ends with no_space once the grant goes.
	w.send(`{"id":"w","method":"GET","path":"shared/*/watch","query":{"version":0}}`)
	watchBody(t, w.reply(`"w"`))
	_, id := func() (string, string) { // stranger's ID
		_, _, b := c.req("GET", "/v1/whoami", "", bearer(stranger)...)
		var o struct{ ID string }
		json.Unmarshal([]byte(b), &o)
		return "", o.ID
	}()
	if s, _, b := c.req("DELETE", "/v1/"+own.Space+"/grants?user="+id, "", bearer(own.Token)...); s != 200 {
		t.Fatalf("revoke %d %s", s, b)
	}
	c.req("PUT", "/v1/"+own.Space+"/shared/k", `2`, bearer(own.Token)...)
	if f := w.reply(`"w"`); f.Status != 404 || f.code() != "no_space" {
		t.Fatalf("revoked watch: %d %s", f.Status, f.Body)
	}
}

func TestLiveWatchPushes(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "")
	sp := "/v1/" + own.Space
	w := c.dial(own.Space, bearer(own.Token)...) // header auth, no message needed

	// "now": the first push comes at once, empty.
	w.send(`{"id":"a","method":"GET","path":"shared/item.*/watch"}`)
	ch, v := watchBody(t, w.reply(`"a"`))
	if len(ch) != 0 || v == 0 {
		t.Fatalf("first push %v %d", ch, v)
	}
	// An HTTP write reaches the socket.
	c.req("PUT", sp+"/shared/item.1", `"x"`, bearer(own.Token)...)
	if ch, _ = watchBody(t, w.reply(`"a"`)); len(ch) != 1 || ch[0]["key"] != "item.1" {
		t.Fatalf("http write pushed: %v", ch)
	}
	// A socket write reaches an HTTP watch.
	done := make(chan string)
	go func() {
		_, _, b := c.req("GET", sp+"/shared/item.*/watch?version="+itoa(v+1)+"&timeout=5", "", bearer(own.Token)...)
		done <- b
	}()
	time.Sleep(100 * time.Millisecond)
	w.call(`"p"`, `{"id":"p","method":"PUT","path":"shared/item.2","body":"y"}`)
	if b := <-done; !strings.Contains(b, "item.2") {
		t.Fatalf("socket write to http watch: %s", b)
	}
	if ch, _ = watchBody(t, w.reply(`"a"`)); len(ch) != 1 || ch[0]["key"] != "item.2" {
		t.Fatalf("own write pushed: %v", ch)
	}
	// A second watch with the same id is refused; stop ends pushes.
	if f := w.call(`"a"`, `{"id":"a","method":"GET","path":"shared/*/watch"}`); f.code() != "id_in_use" {
		t.Fatalf("dup id: %s", f.Body)
	}
	w.call(`"s"`, `{"id":"s","op":"stop","watch":"a"}`)
	c.req("PUT", sp+"/shared/item.3", `"z"`, bearer(own.Token)...)
	if f, ok := w.next(300 * time.Millisecond); ok {
		t.Fatalf("push after stop: %s %s", f.ID, f.Body)
	}
	// From 0: the snapshot.
	w.send(`{"id":"b","method":"GET","path":"shared/item.*/watch","query":{"version":0}}`)
	if ch, _ = watchBody(t, w.reply(`"b"`)); len(ch) != 3 {
		t.Fatalf("snapshot: %v", ch)
	}
	// Too old a version: 410, as on HTTP.
	if f := w.call(`"c"`, `{"id":"c","method":"GET","path":"shared/*/watch","query":{"version":99999999999999}}`); f.Status != 410 {
		t.Fatalf("future version: %d %s", f.Status, f.Body)
	}
}

func TestLiveAuth(t *testing.T) {
	c := newTest(t)
	own := c.open("live-auth-cedar-pilot-mango", "")
	other, otherID := c.anon()

	// Header and an auth message together are fine.
	w := c.dial("live-auth-cedar-pilot-mango", bearer(own)...)
	if f := w.call("1", `{"id":1,"op":"auth","token":"`+own+`"}`); f.Status != 200 {
		t.Fatalf("auth after header: %d %s", f.Status, f.Body)
	}
	w.call("2", `{"id":2,"method":"PUT","path":"me/x","body":1}`)
	w.send(`{"id":"me","method":"GET","path":"me/*/watch","query":{"version":0}}`)
	watchBody(t, w.reply(`"me"`))
	w.send(`{"id":"sh","method":"GET","path":"shared/*/watch"}`)
	watchBody(t, w.reply(`"sh"`))

	// Switching visitor ends me/ watches only.
	if f := w.call("3", `{"id":3,"op":"auth","token":"`+other+`"}`); !strings.Contains(string(f.Body), otherID) {
		t.Fatalf("switch: %s", f.Body)
	}
	if f := w.reply(`"me"`); f.Status != 409 || f.code() != "identity_changed" {
		t.Fatalf("me watch: %d %s", f.Status, f.Body)
	}
	w.call("4", `{"id":4,"method":"PUT","path":"shared/y","body":1}`)
	if ch, _ := watchBody(t, w.reply(`"sh"`)); len(ch) != 1 || ch[0]["by"] != otherID {
		t.Fatalf("shared watch after switch: %v", ch)
	}

	// A bad token: reads go on without a visitor, writes say invalid_token.
	if f := w.call("5", `{"id":5,"op":"auth","token":"vt_`+strings.Repeat("x", 43)+`"}`); f.code() != "invalid_token" {
		t.Fatalf("bad auth: %s", f.Body)
	}
	if f := w.call("6", `{"id":6,"method":"GET","path":"shared/y"}`); f.Status != 200 {
		t.Fatalf("read after bad auth: %d %s", f.Status, f.Body)
	}
	if f := w.call("7", `{"id":7,"method":"PUT","path":"shared/y","body":2}`); f.code() != "invalid_token" {
		t.Fatalf("write after bad auth: %s", f.Body)
	}
	// Anonymous: auth with no token, writes need one.
	w.call("8", `{"id":8,"op":"auth"}`)
	if f := w.call("9", `{"id":9,"method":"PUT","path":"shared/y","body":2}`); f.code() != "missing_token" {
		t.Fatalf("anon write: %s", f.Body)
	}
	// Any first message settles the connection; data first is anonymous.
	w2 := c.dial("live-auth-cedar-pilot-mango")
	if f := w2.call("1", `{"id":1,"method":"GET","path":"shared/y"}`); f.Status != 200 {
		t.Fatalf("data first: %d %s", f.Status, f.Body)
	}
}

func TestLiveAuthTimeout(t *testing.T) {
	liveAuthWait = 200 * time.Millisecond
	defer func() { liveAuthWait = liveAuthWaitDefault }()
	c := newTest(t)
	own := c.create(``, "")
	w := c.dial(own.Space)
	if err := w.readErr(); websocket.CloseStatus(err) != closeAuthTimeout {
		t.Fatalf("expected close %d, got %v", closeAuthTimeout, err)
	}
	// With a header there's no wait.
	w2 := c.dial(own.Space, bearer(own.Token)...)
	time.Sleep(400 * time.Millisecond)
	if f := w2.call("1", `{"id":1,"op":"ping"}`); f.Status != 200 {
		t.Fatalf("header conn closed")
	}
}
