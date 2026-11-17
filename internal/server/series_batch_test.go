package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/timjaanson/cubby/internal/store"
)

type pointsResp struct {
	Points []struct {
		T       int64
		Value   json.RawMessage
		Expires int64
	}
}

func TestBatchSeriesPoints(t *testing.T) {
	c := newTest(t)
	tok, _ := c.anon()
	c.open("home-cedar-pilot-mango", tok)
	auth := []string{"Authorization", "Bearer " + tok}
	sp := "/v1/home-cedar-pilot-mango"
	t0 := store.NowMs() - 60_000

	// The device's minute: the live value and points with their own times,
	// two of them in one series.
	st, _, b := c.req("POST", sp+"/batch", `[
		{"method":"PUT","path":"shared/now","body":{"temp":21.4}},
		{"method":"POST","path":"shared/m1/series","query":{"t":`+itoa(t0)+`,"ttl":3600},"body":{"temp":21.3}},
		{"method":"POST","path":"shared/m1/series","query":{"t":`+itoa(t0+60_000)+`,"ttl":3600},"body":{"temp":21.4}},
		{"method":"POST","path":"shared/m5/series","query":{"t":`+itoa(t0)+`},"body":{"temp":[21.3,21.2,21.5]}}]`, auth...)
	if st != 200 {
		t.Fatalf("batch: %d %s", st, b)
	}
	out := decode[struct {
		Version int64
		Items   []struct {
			Path    string
			Version int64
			Created bool
			T       int64
		}
	}](t, b)
	if len(out.Items) != 4 || out.Items[0].T != 0 || out.Items[1].T != t0 || out.Items[2].T != t0+60_000 || out.Items[3].T != t0 {
		t.Fatalf("items %s", b)
	}
	if !out.Items[1].Created || out.Items[2].Created || out.Items[2].Version != out.Items[1].Version+1 {
		t.Fatalf("second point of one series takes the next version, not a new key: %s", b)
	}
	_, _, b = c.req("GET", sp+"/shared/m1/series", "")
	r := decode[pointsResp](t, b)
	if len(r.Points) != 2 || r.Points[0].Expires != t0+3_600_000 || r.Points[1].Expires != t0+60_000+3_600_000 {
		t.Fatalf("points with expiry from t: %s", b)
	}
	_, _, b = c.req("GET", sp+"/shared/m5/series", "")
	if strings.Contains(b, "expires") {
		t.Fatalf("a point without ttl has no expires: %s", b)
	}
	if _, _, b = c.req("GET", sp+"/shared/m1", ""); b != `{"temp":21.4}` {
		t.Fatalf("key reads as newest point: %s", b)
	}

	// Resending is idempotent: the same times replace their points.
	st, _, b = c.req("POST", sp+"/batch", `[{"method":"POST","path":"shared/m1/series","query":{"t":`+itoa(t0)+`,"ttl":3600},"body":{"temp":21.3}}]`, auth...)
	if st != 200 {
		t.Fatalf("resend: %d %s", st, b)
	}
	if _, _, b = c.req("GET", sp+"/shared/m1/series", ""); len(decode[pointsResp](t, b).Points) != 2 {
		t.Fatalf("resend added a point: %s", b)
	}

	// Points without t share the server's time, after each series' newest.
	_, _, b = c.req("POST", sp+"/batch", `[{"method":"POST","path":"shared/a/series","body":1},{"method":"POST","path":"shared/b/series","body":2}]`, auth...)
	shared := decode[struct{ Items []struct{ T int64 } }](t, b)
	if len(shared.Items) != 2 || shared.Items[0].T == 0 || shared.Items[0].T != shared.Items[1].T {
		t.Fatalf("shared time: %s", b)
	}

	bad := []struct{ body, code string }{
		{`[{"method":"POST","path":"shared/x/series","body":1},{"method":"POST","path":"shared/x/series","body":2}]`, "bad_batch"},
		{`[{"method":"POST","path":"shared/x/series","query":{"t":5},"body":1},{"method":"POST","path":"shared/x/series","query":{"t":5},"body":2}]`, "bad_batch"},
		{`[{"method":"POST","path":"shared/x/series","query":{"t":5},"body":1},{"method":"PUT","path":"shared/x","body":2}]`, "bad_batch"},
		{`[{"method":"PUT","path":"shared/y","body":1},{"method":"POST","path":"shared/env.*/series","body":{"a":1}}]`, "bad_batch"},
		{`[{"method":"PUT","path":"shared/y","body":1},{"method":"POST","path":"shared/y","body":2}]`, "bad_batch"},
		{`[{"method":"PUT","path":"shared/y","body":1},{"method":"POST","path":"shared/m5/series","query":{"t":5,"ttl":60},"body":2}]`, "invalid_parameter"},
		{`[{"method":"PUT","path":"shared/y","body":1},{"method":"POST","path":"shared/now/series","body":2}]`, "not_series"},
		{`[{"method":"POST","path":"shared/m5/series","query":{"t":` + itoa(t0+1) + `},"body":2},{"method":"PUT","path":"shared/m1","body":2}]`, "is_series"},
	}
	for i, tc := range bad {
		st, _, b := c.req("POST", sp+"/batch", tc.body, auth...)
		e := decode[struct {
			Error string
			Index *int
		}](t, b)
		if st < 400 || e.Error != tc.code || e.Index == nil || *e.Index != 1 {
			t.Errorf("case %d: %d %s, want %s at index 1", i, st, b, tc.code)
		}
	}
	// Nothing of a failed batch was written.
	if st, _, _ := c.req("GET", sp+"/shared/y", ""); st != 404 {
		t.Fatalf("failed batch wrote: %d", st)
	}
	if _, _, b = c.req("GET", sp+"/shared/m5/series", ""); len(decode[pointsResp](t, b).Points) != 1 {
		t.Fatalf("failed batch added a point: %s", b)
	}
}

func TestSeriesPointTTL(t *testing.T) {
	c := newTest(t)
	tok, _ := c.anon()
	c.open("home-cedar-pilot-mango", tok)
	auth := []string{"Authorization", "Bearer " + tok}
	S := "/v1/home-cedar-pilot-mango/shared"

	st, h, b := c.req("POST", S+"/m1/series?ttl=60", "1", auth...)
	if st != 201 {
		t.Fatalf("point with ttl: %d %s", st, b)
	}
	at := decode[struct{ T int64 }](t, b).T
	if h.Get("KV-Expires") != "" {
		t.Fatalf("the key itself doesn't expire")
	}
	_, _, b = c.req("GET", S+"/m1/series", "")
	if r := decode[pointsResp](t, b); len(r.Points) != 1 || r.Points[0].Expires != at+60_000 {
		t.Fatalf("expires: %s", b)
	}
	// A late point whose time is already over is refused.
	if st, _, b := c.req("POST", S+"/m1/series?ttl=60&t="+itoa(store.NowMs()-120_000), "0", auth...); st != 400 || !strings.Contains(b, `"ttl"`) {
		t.Fatalf("expired on arrival: %d %s", st, b)
	}
	if st, _, _ := c.req("POST", S+"/m1/series?ttl=1", "0", auth...); st != 400 {
		t.Fatalf("ttl under the minimum: %d", st)
	}
	// A prefix write takes the ttl for every member.
	_, _, b = c.req("POST", S+"/env.*/series?ttl=60", `{"a":1,"b":2}`, auth...)
	pt := decode[struct{ T int64 }](t, b).T
	_, _, b = c.req("GET", S+"/env.b/series", "")
	if r := decode[pointsResp](t, b); len(r.Points) != 1 || r.Points[0].Expires != pt+60_000 {
		t.Fatalf("prefix ttl: %s", b)
	}
}
