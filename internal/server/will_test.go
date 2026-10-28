package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/timjaanson/cubby/internal/config"
	"github.com/timjaanson/cubby/internal/store"
)

func TestLastWill(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "")
	sp := "/v1/" + own.Space
	o := bearer(own.Token)
	_, otherID := c.anon()
	here := "users/" + own.ID + "/here/pulse"

	// Registered on one connection, sent when it drops.
	w := c.dial(own.Space, o...)
	if f := w.call(`1`, `{"id":1,"op":"will","path":"`+here+`","body":{"left":true}}`); f.Status != 200 {
		t.Fatalf("register: %d %s", f.Status, f.Body)
	}
	got := c.listen(sp+"/users/*/here/pulse?timeout=5", o...)
	w.conn.CloseNow()
	select {
	case b := <-got:
		p := parsePulses(t, b)
		if len(p.Pulses) != 1 || p.Pulses[0].User != own.ID || string(p.Pulses[0].Value) != `{"left":true}` {
			t.Fatalf("will: %s", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no will")
	}

	// Cleared, nothing is sent. A clean close fires too, so clear means clear.
	w = c.dial(own.Space, o...)
	w.call(`1`, `{"id":1,"op":"will","path":"`+here+`","text":"bye"}`)
	w.call(`2`, `{"id":2,"op":"will","path":"`+here+`"}`)
	got = c.listen(sp+"/users/*/here/pulse?timeout=1", o...)
	w.conn.Close(websocket.StatusNormalClosure, "")
	if p := parsePulses(t, <-got); len(p.Pulses) != 0 {
		t.Fatalf("cleared will sent: %+v", p)
	}

	// Checked like a pulse when registered.
	w = c.dial(own.Space, o...)
	if f := w.call(`1`, `{"id":1,"op":"will","path":"users/`+otherID+`/here/pulse","body":1}`); f.code() != "not_yours" {
		t.Fatalf("other's area: %d %s", f.Status, f.Body)
	}
	if f := w.call(`2`, `{"id":2,"op":"will","path":"shared/x","body":1}`); f.code() != "bad_message" {
		t.Fatalf("not a pulse path: %s", f.Body)
	}
	if f := w.call(`3`, `{"id":3,"op":"will","path":"shared/x/pulse","body":"`+strings.Repeat("x", 5000)+`"}`); f.code() != "value_too_large" {
		t.Fatalf("too large: %s", f.Body)
	}
	for i := 0; i < 8; i++ {
		if f := w.call(fmt.Sprint(10+i), fmt.Sprintf(`{"id":%d,"op":"will","path":"shared/w%d/pulse","body":1}`, 10+i, i)); f.Status != 200 {
			t.Fatalf("will %d: %s", i, f.Body)
		}
	}
	if f := w.call(`30`, `{"id":30,"op":"will","path":"shared/w8/pulse","body":1}`); f.code() != "too_many_wills" {
		t.Fatalf("ninth: %s", f.Body)
	}
	if f := w.call(`31`, `{"id":31,"op":"will","path":"shared/w0/pulse","body":2}`); f.Status != 200 {
		t.Fatalf("replacing at the cap: %s", f.Body)
	}
}

// Access is checked again when the will fires.
func TestLastWillRechecksAccess(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "")
	sp := "/v1/" + own.Space
	o := bearer(own.Token)
	_, inv := c.mint(own.Space, own.Token, `{}`)
	_, m, _ := c.join(own.Space, inv, "")
	w := c.dial(own.Space, bearer(m)...)
	if f := w.call(`1`, `{"id":1,"op":"will","path":"shared/door/pulse","body":"gone"}`); f.Status != 200 {
		t.Fatalf("member will: %s", f.Body)
	}
	_, _, b := c.req("GET", sp+"/access", "", bearer(m)...)
	var a struct{ ID string }
	json.Unmarshal([]byte(b), &a)
	if s, _, _ := c.req("DELETE", sp+"/grants?user="+a.ID, "", o...); s != 200 {
		t.Fatal("revoke")
	}
	got := c.listen(sp+"/shared/door/pulse?timeout=1", o...)
	w.conn.CloseNow()
	if p := parsePulses(t, <-got); len(p.Pulses) != 0 {
		t.Fatalf("will sent after access ended: %+v", p)
	}
}

// A server shutdown doesn't fire wills.
func TestLastWillNotOnShutdown(t *testing.T) {
	cfg := config.Default()
	cfg.Server.DataDir = t.TempDir()
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, st, nil)
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { ts.Close(); st.Close() })
	c := &tc{t: t, url: ts.URL, store: st, cfg: cfg}
	own := c.create(``, "")
	w := c.dial(own.Space, bearer(own.Token)...)
	w.call(`1`, `{"id":1,"op":"will","path":"shared/door/pulse","body":"gone"}`)
	srv.CloseLive()
	time.Sleep(100 * time.Millisecond)
	if out, next, _ := srv.pulses.hub(own.Space).since(0, func(*pulse) bool { return true }); len(out) != 0 {
		t.Fatalf("will on shutdown: %+v %d", out, next)
	}
}
