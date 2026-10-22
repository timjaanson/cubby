package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/timjaanson/cubby/internal/config"
	"github.com/timjaanson/cubby/internal/store"
)

func TestTTLWatchAndMetadata(t *testing.T) {
	c := newTest(t)
	c.cfg.Limits.TTLMinSeconds = 1
	tok := c.open("ttl-test-cedar-mango", "")
	auth := []string{"Authorization", "Bearer " + tok}
	path := "/v1/ttl-test-cedar-mango/shared/here"
	start := time.Now().UnixMilli()
	status, h, b := c.req("PUT", path+"?ttl=1", "present", auth...)
	if status != 201 {
		t.Fatalf("set %d %s", status, b)
	}
	version := decode[struct{ Version int64 }](t, b).Version
	written, _ := strconv.ParseInt(h.Get("KV-Time"), 10, 64)
	expires, _ := strconv.ParseInt(h.Get("KV-Expires"), 10, 64)
	if written < start || expires-written != 1000 {
		t.Fatalf("write metadata %v", h)
	}
	status, headers, _ := c.req("GET", path, "")
	if status != 200 || headers.Get("KV-Expires") != h.Get("KV-Expires") || headers.Get("KV-Time") != h.Get("KV-Time") {
		t.Fatalf("read metadata %d %v", status, headers)
	}
	// Renew the same bytes: no new version, no reset of value timestamp.
	status, h, b = c.req("PUT", path+"?ttl=1&ifVersion="+itoa(version), "present", auth...)
	if status != 200 || decode[struct{ Version int64 }](t, b).Version != version || h.Get("KV-Time") != strconv.FormatInt(written, 10) {
		t.Fatalf("renew %d %s %v", status, b, h)
	}
	// The real long poll gets a marked deletion when the sweep runs.
	done := make(chan string, 1)
	go func() {
		_, _, body := c.req("GET", path+"/watch?version="+itoa(version)+"&timeout=3", "")
		done <- body
	}()
	time.Sleep(1100 * time.Millisecond)
	if err := c.store.SweepExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case b = <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("watch did not wake")
	}
	changes := decode[struct{ Changes []itemJSON }](t, b).Changes
	if len(changes) != 1 || !changes[0].Deleted || !changes[0].Expired || changes[0].By == "" || changes[0].Version <= version {
		t.Fatalf("expiry watch %s", b)
	}
	if status, _, _ := c.req("GET", path, ""); status != 404 {
		t.Fatal("expired key readable")
	}
	_, _, b = c.req("GET", "/v1/ttl-test-cedar-mango/shared/*", "")
	if strings.Contains(b, `"key":"here"`) {
		t.Fatalf("expired key listed %s", b)
	}
	// Reusing the key clears the expired flag.
	c.req("PUT", path, "back", auth...)
	_, _, b = c.req("GET", path+"/watch?version="+itoa(version), "")
	if strings.Contains(b, `"expired":true`) {
		t.Fatalf("stale expiry flag %s", b)
	}
}

func TestDeadlinesAndTTLValidation(t *testing.T) {
	c := newTest(t)
	tok := c.open("deadline-test-mango", "")
	auth := []string{"Authorization", "Bearer " + tok}
	path := "/v1/deadline-test-mango/shared/x"
	for _, test := range []struct {
		method, suffix, body string
		status               int
		code                 string
	}{
		{"PUT", "?ttl=4", "x", 400, "invalid_parameter"}, {"PUT", "?ttl=2592001", "x", 400, "invalid_parameter"}, {"PUT", "?ttl=5&ttl=6", "x", 400, "invalid_parameter"}, {"PUT", "?ttl=1.5", "x", 400, "invalid_parameter"}, {"DELETE", "?ttl=30", "", 400, "invalid_parameter"},
		{"PUT", "?ifBefore=0", "x", 412, "too_late"}, {"DELETE", "?ifBefore=0", "", 412, "too_late"}, {"POST", "/series?ifBefore=0", "1", 412, "too_late"}, {"POST", "/series?ttl=30", "1", 400, "invalid_parameter"}, {"PUT", "?ifBefore=-1", "x", 400, "invalid_parameter"}, {"PUT", "?ifBefore=1&ifBefore=2", "x", 400, "invalid_parameter"},
	} {
		status, _, b := c.req(test.method, path+test.suffix, test.body, auth...)
		if status != test.status || errCode(b) != test.code {
			t.Errorf("%s %s: %d %s", test.method, test.suffix, status, b)
		}
	}
	status, _, b := c.req("PUT", path+"?ifVersion=0&ifBefore="+itoa(store.NowMs()+10000), "yes", auth...)
	if status != 201 {
		t.Fatalf("combined conditions %d %s", status, b)
	}
	status, _, b = c.req("DELETE", path+"?ifVersion=0&ifBefore="+itoa(store.NowMs()+10000), "", auth...)
	if status != 412 || errCode(b) != "version_conflict" {
		t.Fatalf("combined conflict %d %s", status, b)
	}
	c.req("POST", path+".series/series", "1", auth...)
	if status, _, b = c.req("PUT", path+".series?ttl=30", "1", auth...); status != 409 || errCode(b) != "is_series" {
		t.Fatalf("series ttl %d %s", status, b)
	}
}

func TestLimitsAndBudgetsMatchConfiguration(t *testing.T) {
	cfg := config.Default()
	cfg.Server.DataDir = t.TempDir()
	cfg.Limits.WriteBurst = 2
	cfg.Limits.WritesPerMinute = 1
	cfg.Limits.WritesPerIPPerMinute = 5
	cfg.Limits.MissesPerIPPerHour = 2
	const space = "limits-test-cedar-mango"
	cfg.Spaces = map[string]config.PinnedSpace{space: {MaxBytes: 12345, MaxKeys: 37}}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, st, nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	defer st.Close()
	c := &tc{t: t, url: ts.URL, store: st, cfg: cfg}
	_, _, b := c.req("GET", "/v1/limits", "")
	limits := decode[map[string]any](t, b)
	if limits["writes_per_minute"] != float64(1) || limits["write_burst"] != float64(2) || limits["watches_per_ip"] != float64(50) || limits["rate_limits"] != true {
		t.Fatalf("server limits %s", b)
	}
	tok := c.open(space, "")
	auth := []string{"Authorization", "Bearer " + tok}
	for i := 0; i < 3; i++ {
		status, h, b := c.req("PUT", "/v1/"+space+"/shared/x", "x", auth...)
		if i < 2 && status >= 300 || i == 2 && status != 429 {
			t.Fatalf("budget %d: %d %s", i, status, b)
		}
		want := fmt.Sprintf(`"visitor";r=%d,`, max(0, 1-i))
		if !strings.Contains(h.Get("RateLimit"), want) || !strings.Contains(h.Get("RateLimit-Policy"), `"visitor";q=2;w=120`) || !strings.Contains(h.Get("Access-Control-Expose-Headers"), "RateLimit") {
			t.Fatalf("headers %v", h)
		}
	}
	_, _, b = c.req("GET", "/v1/"+space+"/access", "", auth...)
	a := decode[struct {
		Limits struct{ SpaceBytes, SpaceKeys int }
		Usage  struct{ Bytes, Keys, VisitorBytes int }
	}](t, b)
	if a.Limits.SpaceBytes != 12345 || a.Limits.SpaceKeys != 37 || a.Usage.Bytes != 2 || a.Usage.Keys != 1 || a.Usage.VisitorBytes != 2 {
		t.Fatalf("effective limits %s", b)
	}
	if srv.lim.writesPerIP.rate != 5.0/60 || srv.lim.misses.budget.burst != 2 || srv.watches.perIPLimit != 50 {
		t.Fatal("configuration not enforced")
	}
	cfg.Server.RateLimits = new(bool)
	_, h, _ := c.req("PUT", "/v1/"+space+"/shared/x", "x", auth...)
	if h.Get("RateLimit") != "" {
		t.Fatal("disabled budget advertised")
	}
	_, _, b = c.req("GET", "/v1/limits", "")
	if decode[map[string]any](t, b)["rate_limits"] != false {
		t.Fatalf("disabled limits %s", b)
	}
	status, _, b := c.req("POST", "/v1/time", "")
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("time method %d %s", status, b)
	}
	before := store.NowMs()
	_, _, b = c.req("GET", "/v1/time", "")
	now := decode[struct{ T int64 }](t, b).T
	if now < before || now > store.NowMs() {
		t.Fatalf("time %s", b)
	}
}

func TestAdaptiveWatchPause(t *testing.T) {
	n := notifier{spaces: map[string]*spaceSignal{}}
	n.join("space")
	defer n.leave("space")
	n.fire("space")
	if n.burst("space") {
		t.Fatal("isolated change delayed")
	}
	n.fire("space")
	if !n.burst("space") {
		t.Fatal("burst not batched")
	}
	n.mu.Lock()
	n.spaces["space"].last = time.Now().Add(-time.Second)
	n.mu.Unlock()
	n.fire("space")
	if n.burst("space") {
		t.Fatal("quiet period still delayed")
	}
}
