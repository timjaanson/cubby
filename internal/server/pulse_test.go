package server

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

type pulseBody struct {
	Pulses []struct {
		Key, User, By string
		Value         json.RawMessage
		T             int64
	}
	Seq    int64
	Missed bool
}

func parsePulses(t *testing.T, b string) pulseBody {
	t.Helper()
	var p pulseBody
	if err := json.Unmarshal([]byte(b), &p); err != nil {
		t.Fatalf("pulse body %s", b)
	}
	return p
}

// listen starts an HTTP long poll and returns its body when it answers.
func (c *tc) listen(path string, hdr ...string) <-chan string {
	out := make(chan string, 1)
	go func() {
		_, _, b := c.req("GET", path, "", hdr...)
		out <- b
	}()
	time.Sleep(50 * time.Millisecond)
	return out
}

func TestPulseHTTP(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "")
	sp := "/v1/" + own.Space
	o := bearer(own.Token)
	_, otherID := c.anon()

	// A listener gets a pulse sent after it started, with who and when.
	got := c.listen(sp+"/shared/cursor/pulse?timeout=5", o...)
	s, _, b := c.req("POST", sp+"/shared/cursor/pulse", `{"x":1,"y":2}`, o...)
	if s != 202 || !strings.Contains(b, `"t":`) {
		t.Fatalf("send: %d %s", s, b)
	}
	p := parsePulses(t, <-got)
	if len(p.Pulses) != 1 || string(p.Pulses[0].Value) != `{"x":1,"y":2}` || p.Pulses[0].By != own.ID || p.Pulses[0].T == 0 || p.Missed {
		t.Fatalf("received %+v", p)
	}
	// Continuing from seq picks up what came in between: text, by pattern.
	c.req("POST", sp+"/shared/react.1/pulse", `hi`, o...)
	c.req("POST", sp+"/shared/other/pulse", `1`, o...)
	_, _, b = c.req("GET", sp+"/shared/react.*/pulse?after="+itoa(p.Seq)+"&timeout=1", "", o...)
	if p = parsePulses(t, b); len(p.Pulses) != 1 || string(p.Pulses[0].Value) != `"hi"` {
		t.Fatalf("pattern catch-up %s", b)
	}

	// Nothing is stored, and stored keys aren't touched.
	if s, _, _ := c.req("GET", sp+"/shared/cursor", "", o...); s != 404 {
		t.Fatalf("pulse stored: %d", s)
	}
	// Pulses don't wake stored watches.
	_, _, lb := c.req("GET", sp+"/shared/*", "", o...)
	var l struct{ Version int64 }
	json.Unmarshal([]byte(lb), &l)
	watch := c.listen(sp+"/shared/*/watch?timeout=1&version="+itoa(l.Version), o...)
	c.req("POST", sp+"/shared/cursor/pulse", `2`, o...)
	if b := <-watch; !strings.Contains(b, `"changes":[]`) {
		t.Fatalf("pulse woke a watch: %s", b)
	}

	// users/*/ gets everyone's, with user set; nobody sends as someone else.
	got = c.listen(sp+"/users/*/cursor/pulse?timeout=5", o...)
	c.req("POST", sp+"/users/"+own.ID+"/cursor/pulse", `3`, o...)
	if p = parsePulses(t, <-got); len(p.Pulses) != 1 || p.Pulses[0].User != own.ID {
		t.Fatalf("users pulse %+v", p)
	}
	for _, r := range []struct {
		method, path, body, code string
		status                   int
	}{
		{"POST", "/users/" + otherID + "/cursor/pulse", "1", "not_yours", 403},
		{"POST", "/shared/c.*/pulse", "1", "invalid_key", 400},
		{"DELETE", "/shared/cursor/pulse", "", "method_not_allowed", 405},
		{"POST", "/shared/cursor/pulse", strings.Repeat("x", 4097), "value_too_large", 413},
		{"POST", "/shared/cursor/pulse?ttl=5", "1", "invalid_parameter", 400},
		{"PUT", "/shared/pulse", "1", "invalid_key", 400},
	} {
		s, _, b := c.req(r.method, sp+r.path, r.body, o...)
		if s != r.status || errCode(b) != r.code {
			t.Errorf("%s %s: %d %s", r.method, r.path, s, b)
		}
	}
	// A stranger can't send or listen in a private space.
	stranger, _ := c.anon()
	if s, _, b := c.req("POST", sp+"/shared/cursor/pulse", "1", bearer(stranger)...); s != 404 || errCode(b) != "no_space" {
		t.Fatalf("stranger send: %d %s", s, b)
	}
}

func TestPulseRateLimit(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "")
	sp := "/v1/" + own.Space
	limited := 0
	for i := 0; i < 130; i++ {
		if s, _, b := c.req("POST", sp+"/shared/x/pulse", "1", bearer(own.Token)...); s == 429 {
			if errCode(b) != "rate_limited" {
				t.Fatalf("429 body %s", b)
			}
			limited++
		}
	}
	if limited == 0 || limited > 15 {
		t.Fatalf("burst of 120 then limited, got %d limited of 130", limited)
	}
	// Stored writes have their own budget.
	if s, _, b := c.req("PUT", sp+"/shared/x", "1", bearer(own.Token)...); s != 201 {
		t.Fatalf("write after pulses: %d %s", s, b)
	}
}

func TestPulseMissed(t *testing.T) {
	c := newTest(t)
	off := false
	c.cfg.Server.RateLimits = &off
	own := c.create(``, "")
	sp := "/v1/" + own.Space
	o := bearer(own.Token)
	got := c.listen(sp+"/shared/n/pulse?timeout=1", o...)
	start := parsePulses(t, <-got).Seq
	for i := 0; i < 1100; i++ {
		c.req("POST", sp+"/shared/n/pulse", itoa(int64(i)), o...)
	}
	_, _, b := c.req("GET", sp+"/shared/n/pulse?after="+itoa(start)+"&timeout=1", "", o...)
	p := parsePulses(t, b)
	if !p.Missed || len(p.Pulses) != pulseBatchMax || string(p.Pulses[len(p.Pulses)-1].Value) != "1099" {
		t.Fatalf("missed %v, %d pulses, last %s", p.Missed, len(p.Pulses), p.Pulses[len(p.Pulses)-1].Value)
	}
}

func TestPulseLive(t *testing.T) {
	c := newTest(t)
	own := c.create(``, "")
	sp := "/v1/" + own.Space
	other, _ := c.anon()
	_, tok := c.mint(own.Space, own.Token, `{}`)
	c.join(own.Space, tok, other)

	a := c.dial(own.Space, bearer(own.Token)...)
	b := c.dial(own.Space)
	b.call("0", `{"id":0,"op":"auth","token":"`+other+`"}`)
	b.send(`{"id":"l","method":"GET","path":"users/*/cursor/pulse"}`)
	if f := b.reply(`"l"`); f.Status != 200 || !strings.Contains(string(f.Body), `"pulses":[]`) {
		t.Fatalf("first frame %d %s", f.Status, f.Body)
	}

	// Many quick pulses over the socket all arrive, in order, as they're sent.
	for i := 0; i < 50; i++ {
		a.send(`{"id":` + itoa(int64(i+1)) + `,"method":"POST","path":"users/` + own.ID + `/cursor/pulse","body":{"x":` + itoa(int64(i)) + `}}`)
	}
	var xs []string
	deadline := time.Now().Add(3 * time.Second)
	for len(xs) < 50 && time.Now().Before(deadline) {
		f := b.reply(`"l"`)
		var p pulseBody
		json.Unmarshal(f.Body, &p)
		for _, q := range p.Pulses {
			xs = append(xs, string(q.Value))
		}
	}
	if len(xs) != 50 || xs[0] != `{"x":0}` || xs[49] != `{"x":49}` {
		t.Fatalf("got %d pulses: %v", len(xs), xs)
	}
	for i := 1; i <= 50; i++ {
		if f := a.reply(itoa(int64(i))); f.Status != 202 {
			t.Fatalf("send reply %d: %d %s", i, f.Status, f.Body)
		}
	}
	// An HTTP sender reaches a socket listener.
	c.req("POST", sp+"/users/"+own.ID+"/cursor/pulse", `"http"`, bearer(own.Token)...)
	if f := b.reply(`"l"`); !strings.Contains(string(f.Body), `"http"`) {
		t.Fatalf("http to socket %s", f.Body)
	}
	// Stop ends it.
	b.call(`"s"`, `{"id":"s","op":"stop","watch":"l"}`)
	c.req("POST", sp+"/users/"+own.ID+"/cursor/pulse", `1`, bearer(own.Token)...)
	if f, ok := b.next(300 * time.Millisecond); ok {
		t.Fatalf("pulse after stop %s", f.Body)
	}
}

// Senders in parallel: every listener sees each pulse exactly once.
func TestPulseParallelSenders(t *testing.T) {
	p := newPulseHubs()
	h := p.hub("s")
	match := func(*pulse) bool { return true }
	done := make(chan struct{})
	seen := map[string]int{}
	go func() {
		defer close(done)
		cursor := int64(0)
		for len(seen) < 1000 { // within the 1024-pulse window, so nothing may drop
			sig := h.wait()
			out, next, _ := h.since(cursor, match)
			for _, q := range out {
				seen[string(q.Value)]++
			}
			cursor = next
			select {
			case <-sig:
			case <-time.After(time.Second):
				return
			}
		}
	}()
	time.Sleep(10 * time.Millisecond)
	var wg sync.WaitGroup
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				p.send("s", pulse{area: "s", key: "k", value: []byte(itoa(int64(g*1000 + i))), isJSON: true, at: time.Now()})
			}
		}()
	}
	wg.Wait()
	<-done
	for v, n := range seen {
		if n != 1 {
			t.Fatalf("pulse %s seen %d times", v, n)
		}
	}
	if len(seen) != 1000 {
		t.Fatalf("saw %d of 1000", len(seen))
	}
}

// One space, 50 listeners each reading their whole stream, senders in
// parallel: the cost of one send plus its fan-out.
func BenchmarkPulseFanout50(b *testing.B) {
	p := newPulseHubs()
	h := p.hub("s")
	match := func(*pulse) bool { return true }
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cursor := int64(0)
			for {
				sig := h.wait()
				_, next, _ := h.since(cursor, match)
				cursor = next
				select {
				case <-sig:
				case <-stop:
					return
				}
			}
		}()
	}
	value := []byte(`{"x":123,"y":456}`)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			p.send("s", pulse{area: "u:u_aaaaaaaaaaaaaaaa", key: "cursor", by: "u_aaaaaaaaaaaaaaaa", value: value, isJSON: true, at: time.Now()})
		}
	})
	b.StopTimer()
	close(stop)
	wg.Wait()
}
