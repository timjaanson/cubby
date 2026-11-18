package server

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

type seriesResp struct {
	Points []struct {
		T     int64
		Value json.RawMessage
	}
	Version int64
	Next    *int64
}

// UnmarshalJSON reads a series page: items are the points.
func (r *seriesResp) UnmarshalJSON(b []byte) error {
	var raw struct {
		Items []struct {
			T     int64
			Value json.RawMessage
		}
		Version int64
		Next    *int64
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	r.Version, r.Next = raw.Version, raw.Next
	r.Points = r.Points[:0]
	for _, p := range raw.Items {
		r.Points = append(r.Points, struct {
			T     int64
			Value json.RawMessage
		}{p.T, p.Value})
	}
	return nil
}

// mustNext returns a series response's next, as JSON.
func mustNext(t *testing.T, body []byte) json.RawMessage {
	t.Helper()
	r := decode[seriesResp](t, string(body))
	if r.Next == nil {
		t.Fatalf("no next in %s", body)
	}
	return json.RawMessage(itoa(*r.Next))
}

func decode[T any](t *testing.T, b string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(b), &v); err != nil {
		t.Fatalf("decode %q: %v", b, err)
	}
	return v
}

func TestSeriesBasics(t *testing.T) {
	c := newTest(t)
	tok, _ := c.anon()
	c.open("home-cedar-pilot-mango", tok)
	auth := []string{"Authorization", "Bearer " + tok}
	S := "/v1/home-cedar-pilot-mango/shared"

	// First POST creates the series.
	st, h, b := c.req("POST", S+"/temp/series", "21.4", auth...)
	if st != 201 {
		t.Fatalf("first point: %d %s", st, b)
	}
	first := decode[struct{ Version, T int64 }](t, b)
	if first.T == 0 || h.Get("ETag") != etag(first.Version) {
		t.Fatalf("response %s, etag %s", b, h.Get("ETag"))
	}
	st, _, b = c.req("POST", S+"/temp/series", `"sensor reset"`, auth...)
	if st != 200 {
		t.Fatalf("second point: %d %s", st, b)
	}
	second := decode[struct{ Version, T int64 }](t, b)
	if second.T <= first.T || second.Version <= first.Version {
		t.Fatalf("server times must grow: %d then %d", first.T, second.T)
	}
	c.req("POST", S+"/temp/series", "plain text", auth...)
	c.req("POST", S+"/temp/series", `{"c": 21.6, "ok": true}`, auth...)

	// The key reads as its newest point, with KV-Time.
	st, h, b = c.req("GET", S+"/temp", "")
	if st != 200 || b != `{"c": 21.6, "ok": true}` || h.Get("KV-Time") == "" {
		t.Fatalf("read: %d %q KV-Time=%q", st, b, h.Get("KV-Time"))
	}
	if !strings.Contains(h.Get("Access-Control-Expose-Headers"), "KV-Time") {
		t.Fatal("KV-Time not exposed to browsers")
	}

	// Points come back oldest first, values as stored.
	_, _, b = c.req("GET", S+"/temp/series?order=asc", "")
	r := decode[seriesResp](t, b)
	if len(r.Points) != 4 || r.Next != nil {
		t.Fatalf("series: %s", b)
	}
	// Responses that wrap values compact JSON, as list does. A plain GET returns the bytes as sent.
	want := []string{`21.4`, `"sensor reset"`, `"plain text"`, `{"c":21.6,"ok":true}`}
	for i, p := range r.Points {
		if string(p.Value) != want[i] {
			t.Fatalf("point %d = %s, want %s", i, p.Value, want[i])
		}
		if i > 0 && p.T <= r.Points[i-1].T {
			t.Fatal("points not oldest first")
		}
	}
	if strconv.FormatInt(r.Points[3].T, 10) != h.Get("KV-Time") {
		t.Fatal("KV-Time isn't the newest point's time")
	}

	// order=asc: the oldest ones, and next to continue.
	_, _, b = c.req("GET", S+"/temp/series?order=asc&limit=3", "")
	r = decode[seriesResp](t, b)
	if len(r.Points) != 3 || string(r.Points[0].Value) != want[0] || r.Next == nil || *r.Next != r.Points[2].T {
		t.Fatalf("page 1: %s", b)
	}
	_, _, b = c.req("GET", S+"/temp/series?order=asc&limit=3&after="+itoa(*r.Next), "")
	r = decode[seriesResp](t, b)
	if len(r.Points) != 1 || r.Next != nil || string(r.Points[0].Value) != want[3] {
		t.Fatalf("page 2: %s", b)
	}

	// The default is newest first; next continues as after too.
	_, _, b = c.req("GET", S+"/temp/series?limit=3", "")
	r = decode[seriesResp](t, b)
	if len(r.Points) != 3 || string(r.Points[0].Value) != want[3] || r.Next == nil || *r.Next != r.Points[2].T {
		t.Fatalf("desc page 1: %s", b)
	}
	_, _, b = c.req("GET", S+"/temp/series?limit=3&after="+itoa(*r.Next), "")
	r = decode[seriesResp](t, b)
	if len(r.Points) != 1 || r.Next != nil || string(r.Points[0].Value) != want[0] {
		t.Fatalf("desc page 2: %s", b)
	}
	// since is inclusive, before exclusive.
	t0 := r.Points[0].T
	_, _, b = c.req("GET", S+"/temp/series?since="+itoa(t0)+"&before="+itoa(t0+1), "")
	if r = decode[seriesResp](t, b); len(r.Points) != 1 || string(r.Points[0].Value) != want[0] {
		t.Fatalf("since and before: %s", b)
	}
	if _, _, b = c.req("GET", S+"/temp/series?since="+itoa(t0+1), ""); len(decode[seriesResp](t, b).Points) != 3 {
		t.Fatalf("since inclusive: %s", b)
	}
	if st, _, b := c.req("GET", S+"/temp/series?limit=10000", ""); st != 200 {
		t.Fatalf("limit 10000: %d %s", st, b)
	}
	for _, q := range []string{"limit=10001", "limit=0", "order=newest", "until=soon", "until=-1"} {
		if st, _, b := c.req("GET", S+"/temp/series?"+q, ""); st != 400 || errCode(b) != "invalid_parameter" {
			t.Fatalf("%s: %d %s", q, st, b)
		}
	}

	// List and watch carry t for both plain and series keys.
	c.req("PUT", S+"/plain", "1", auth...)
	_, _, b = c.req("GET", S+"/*", "")
	if !strings.Contains(b, `"key":"temp","value":{"c":21.6,"ok":true}`) || !strings.Contains(b, `"t":`+h.Get("KV-Time")) {
		t.Fatalf("list: %s", b)
	}
	items := decode[struct{ Items []map[string]any }](t, b).Items
	for _, it := range items {
		if _, has := it["t"]; !has {
			t.Fatalf("t on the wrong items: %s", b)
		}
	}
	_, _, b = c.req("GET", S+"/*/watch?after=0", "")
	if !strings.Contains(b, `"t":`+h.Get("KV-Time")) {
		t.Fatalf("watch: %s", b)
	}
}

func TestSeriesKinds(t *testing.T) {
	c := newTest(t)
	tok, _ := c.anon()
	c.open("home-cedar-pilot-mango", tok)
	auth := []string{"Authorization", "Bearer " + tok}
	S := "/v1/home-cedar-pilot-mango/shared"
	c.req("POST", S+"/temp/series", "21", auth...)
	c.req("PUT", S+"/note", "hi", auth...)

	for _, tt := range []struct {
		method, path, body string
		status             int
		code               string
	}{
		{"PUT", S + "/temp", "5", 409, "is_series"},
		{"POST", S + "/note/series", "5", 409, "not_series"},
		{"GET", S + "/note/series", "", 409, "not_series"},
		{"GET", S + "/missing/series", "", 404, "not_found"},
		{"POST", S + "/temp", "5", 405, "method_not_allowed"},
		{"PUT", S + "/temp/series", "5", 405, "method_not_allowed"},
		{"DELETE", S + "/temp/series", "", 405, "method_not_allowed"},
		{"POST", S + "/temp/watch", "", 405, "method_not_allowed"},
		{"PUT", S + "/series", "5", 400, "invalid_key"},
		{"PUT", S + "/watch", "5", 400, "invalid_key"},
		{"GET", S + "/env.*/series?after=5", "", 400, "invalid_parameter"},
		{"GET", "/v1/home-cedar-pilot-mango/by/*/temp/series", "", 400, "invalid_key"},
		{"GET", S + "/temp/series/watch", "", 400, "invalid_key"},
		{"GET", S + "/temp/history", "", 400, "invalid_key"},
		{"POST", S + "/env.*", "{}", 405, "method_not_allowed"},
		{"POST", S + "/temp/series", "", 400, "empty_value"},
		{"POST", S + "/temp/series?ifVersion=1", "5", 400, "invalid_parameter"},
		{"POST", S + "/temp/series?t=abc", "5", 400, "invalid_parameter"},
		{"POST", S + "/temp/series?t=" + itoa(time.Now().UnixMilli()+120_000), "5", 400, "invalid_parameter"},
		{"POST", S + "/temp/series", strings.Repeat("x", 4097), 413, "value_too_large"},
		{"GET", S + "/temp/series?limit=0", "", 400, "invalid_parameter"},
		{"GET", S + "/temp/series?since=-1", "", 400, "invalid_parameter"},
	} {
		st, h, b := c.req(tt.method, tt.path, tt.body, auth...)
		if st != tt.status || errCode(b) != tt.code {
			t.Errorf("%s %s: %d %s, want %d %s", tt.method, tt.path, st, b, tt.status, tt.code)
		}
		if st == 405 && h.Get("Allow") == "" {
			t.Errorf("%s %s: 405 without Allow", tt.method, tt.path)
		}
	}
	// The hint points to the right route.
	_, _, b := c.req("POST", S+"/temp", "5", auth...)
	if !strings.Contains(b, "temp/series") {
		t.Errorf("405 hint: %s", b)
	}

	// Deleting a series frees the name for a plain key, and its points go.
	if st, _, b := c.req("DELETE", S+"/temp", "", auth...); st != 200 {
		t.Fatalf("delete: %d %s", st, b)
	}
	if st, _, b := c.req("PUT", S+"/temp", "plain now", auth...); st != 201 {
		t.Fatalf("put after delete: %d %s", st, b)
	}
	if st, _, _ := c.req("GET", S+"/temp/series", ""); st != 409 {
		t.Fatalf("old points still readable: %d", st)
	}
}

// A point goes to one series; several at once go through a batch.
func TestSeriesPatternRefused(t *testing.T) {
	c := newTest(t)
	tok, _ := c.anon()
	c.open("home-cedar-pilot-mango", tok)
	auth := []string{"Authorization", "Bearer " + tok}
	S := "/v1/home-cedar-pilot-mango/shared"
	if st, _, b := c.req("POST", S+"/env.*/series", `{"temp": 21.4}`, auth...); st != 400 || errCode(b) != "invalid_key" || !strings.Contains(b, "batch") {
		t.Fatalf("pattern point: %d %s", st, b)
	}
	// An object posted to one key is one point.
	c.req("POST", S+"/pos/series", `{"lat": 59.43, "lon": 24.75}`, auth...)
	_, _, b := c.req("GET", S+"/pos/series", "")
	if r := decode[seriesResp](t, b); len(r.Points) != 1 || string(r.Points[0].Value) != `{"lat":59.43,"lon":24.75}` {
		t.Fatalf("object point: %s", b)
	}
}

func TestSeriesTimes(t *testing.T) {
	c := newTest(t)
	tok, _ := c.anon()
	c.open("home-cedar-pilot-mango", tok)
	auth := []string{"Authorization", "Bearer " + tok}
	S := "/v1/home-cedar-pilot-mango/shared"
	base := time.Now().UnixMilli() - 3_600_000

	_, _, b := c.req("POST", S+"/temp/series?t="+itoa(base+2000), "22", auth...)
	newest := decode[struct{ Version, T int64 }](t, b)
	if newest.T != base+2000 {
		t.Fatalf("explicit t ignored: %s", b)
	}
	// An older point goes into the series without touching the key.
	st, _, b := c.req("POST", S+"/temp/series?t="+itoa(base+1000), "21", auth...)
	older := decode[struct{ Version, T int64 }](t, b)
	if st != 200 || older.Version != newest.Version {
		t.Fatalf("older point changed the version: %d %s", st, b)
	}
	if _, h, b := c.req("GET", S+"/temp", ""); b != "22" || h.Get("KV-Time") != itoa(base+2000) {
		t.Fatalf("older point became the value: %s", b)
	}
	// A point at an existing time replaces it.
	c.req("POST", S+"/temp/series?t="+itoa(base+1000), "20.5", auth...)
	_, _, b = c.req("GET", S+"/temp/series?order=asc", "")
	r := decode[seriesResp](t, b)
	if len(r.Points) != 2 || string(r.Points[0].Value) != "20.5" {
		t.Fatalf("replace at same t: %s", b)
	}
	// The same value as before still makes a new newest point and version.
	_, _, b = c.req("POST", S+"/temp/series", "22", auth...)
	again := decode[struct{ Version, T int64 }](t, b)
	if again.Version <= newest.Version || again.T <= newest.T {
		t.Fatalf("same value didn't count as a new reading: %s", b)
	}
	// Server time never goes below the newest point, even one set ahead.
	future := time.Now().UnixMilli() + 30_000
	c.req("POST", S+"/ahead/series?t="+itoa(future), "1", auth...)
	_, _, b = c.req("POST", S+"/ahead/series", "2", auth...)
	if got := decode[struct{ T int64 }](t, b).T; got != future+1 {
		t.Fatalf("server time %d, want %d", got, future+1)
	}
}

func TestSeriesRetentionAndBytes(t *testing.T) {
	c := newTest(t)
	c.cfg.Limits.MaxSeriesPoints = 5
	tok, id := c.anon()
	c.open("home-cedar-pilot-mango", tok)
	auth := []string{"Authorization", "Bearer " + tok}
	S := "/v1/home-cedar-pilot-mango/shared"
	for i := range 12 {
		if st, _, b := c.req("POST", S+"/temp/series", itoa(int64(i)), auth...); st >= 300 {
			t.Fatalf("point %d: %d %s", i, st, b)
		}
	}
	_, _, b := c.req("GET", S+"/temp/series?order=asc", "")
	r := decode[seriesResp](t, b)
	if len(r.Points) != 5 || string(r.Points[0].Value) != "7" || string(r.Points[4].Value) != "11" {
		t.Fatalf("kept points: %s", b)
	}

	// Space bytes: the key row plus every kept point. Visitor bytes: the key row only.
	keyRow := int64(len("temp") + len("11"))
	points := int64(5 * (len("temp") + 1))
	points += int64(len("10") + len("11") - 2) // two of them have two digits
	got := c.store.SpaceBytes("home-cedar-pilot-mango")
	if got != keyRow+points {
		t.Fatalf("space bytes %d, want %d", got, keyRow+points)
	}
	c.store.ForgetVisitorBytes(id)
	if vb, _ := c.store.VisitorTotal(id); vb != keyRow {
		t.Fatalf("visitor bytes %d, want %d", vb, keyRow)
	}

	// Deleting the series frees everything but the tombstone.
	c.req("DELETE", S+"/temp", "", auth...)
	got = c.store.SpaceBytes("home-cedar-pilot-mango")
	if got != int64(len("temp")) {
		t.Fatalf("after delete: %d bytes", got)
	}
	n, _ := c.store.PointCount("home-cedar-pilot-mango")
	if n != 0 {
		t.Fatalf("%d points left after delete", n)
	}
}

func TestSeriesQuota(t *testing.T) {
	c := newTest(t)
	c.cfg.Limits.MaxSpaceBytes = 200
	off := false
	c.cfg.Server.RateLimits = &off
	c.cfg.Limits.MaxSeriesPoints = 10
	tok, _ := c.anon()
	c.open("home-cedar-pilot-mango", tok)
	auth := []string{"Authorization", "Bearer " + tok}
	S := "/v1/home-cedar-pilot-mango/shared"
	// A full series at a steady rate never hits the quota by itself:
	// 10 points of 5 bytes plus the key row is far below 200.
	for i := range 50 {
		if st, _, b := c.req("POST", S+"/t/series", "1234", auth...); st >= 300 {
			t.Fatalf("point %d: %d %s", i, st, b)
		}
	}
	// A big one does.
	st, _, b := c.req("POST", S+"/big/series", `"`+strings.Repeat("x", 300)+`"`, auth...)
	if st != 507 || errCode(b) != "space_quota_exceeded" {
		t.Fatalf("quota: %d %s", st, b)
	}
}

func TestSeriesUsersAndMe(t *testing.T) {
	c := newTest(t)
	tok, id := c.anon()
	c.open("home-cedar-pilot-mango", tok)
	tok2, _ := c.anon()
	auth := []string{"Authorization", "Bearer " + tok}
	sp := "/v1/home-cedar-pilot-mango"
	if st, _, b := c.req("POST", sp+"/by/"+id+"/weight/series", "71.2", auth...); st != 201 {
		t.Fatalf("own users/: %d %s", st, b)
	}
	if st, _, b := c.req("POST", sp+"/by/"+id+"/weight/series", "71.0", "Authorization", "Bearer "+tok2); st != 403 || errCode(b) != "not_yours" {
		t.Fatalf("other visitor: %d %s", st, b)
	}
	if st, _, _ := c.req("GET", sp+"/by/"+id+"/weight/series", ""); st != 200 {
		t.Fatalf("public read: %d", st)
	}
	if st, _, b := c.req("POST", sp+"/to/me/sleep/series", "7.5", auth...); st != 201 {
		t.Fatalf("me/: %d %s", st, b)
	}
	if st, _, _ := c.req("GET", sp+"/to/me/sleep/series", ""); st != 401 {
		t.Fatalf("me/ without token: %d", st)
	}
	if st, _, b := c.req("POST", sp+"/by/*/weight/series", "1", auth...); st != 400 {
		t.Fatalf("by/*/ series: %d %s", st, b)
	}
}

// A device invited with post writes its own series and nothing common.
func TestSeriesPostDevice(t *testing.T) {
	c := newTest(t)
	own := c.create(`{"prefix":"room","anyone":"read"}`, "")
	o := []string{"Authorization", "Bearer " + own.Token}
	_, _, b := c.req("POST", "/v1/"+own.Space+"/invites", `{"access":"post","uses":1}`, o...)
	tk := decode[struct{ Invite string }](t, b)
	devTok, devID := c.anon()
	c.join(own.Space, tk.Invite, devTok)
	d := []string{"Authorization", "Bearer " + devTok}
	sp := "/v1/" + own.Space
	if st, _, b := c.req("POST", sp+"/by/me/env.temp/series", `1`, d...); st != 201 {
		t.Fatalf("device write: %d %s", st, b)
	}
	if st, _, b := c.req("POST", sp+"/shared/env.temp/series", `1`, d...); st != 403 || errCode(b) != "no_access" {
		t.Fatalf("common keys: %d %s", st, b)
	}
	// Anyone reads it, without knowing the device's ID.
	if st, _, b := c.req("GET", sp+"/by/*/env.temp", ""); st != 200 || !strings.Contains(b, `"by":"`+devID+`"`) {
		t.Fatalf("read across: %d %s", st, b)
	}
}

// A pattern reads every series under it, merged by time, with keys; pages
// carry over ties at one t.
func TestSeriesPattern(t *testing.T) {
	c := newTest(t)
	tok, _ := c.anon()
	c.open("home-cedar-pilot-mango", tok)
	auth := []string{"Authorization", "Bearer " + tok}
	S := "/v1/home-cedar-pilot-mango/shared"
	t0 := time.Now().UnixMilli() - 60_000
	var w []string
	for i := int64(0); i < 3; i++ {
		w = append(w, `{"add":"shared/env.temp","value":`+itoa(20+i)+`,"t":`+itoa(t0+i)+`}`, `{"add":"shared/env.rh","value":`+itoa(40+i)+`,"t":`+itoa(t0+i)+`}`)
	}
	c.req("POST", "/v1/home-cedar-pilot-mango/batch", "["+strings.Join(w, ",")+"]", auth...)
	c.req("PUT", S+"/env.note", `"not a series"`, auth...)
	type page struct {
		Items []struct {
			Key   string
			T     int64
			Value json.RawMessage
		}
		Next *string
	}
	read := func(q string) page {
		t.Helper()
		st, _, b := c.req("GET", S+"/env.*/series"+q, "")
		if st != 200 {
			t.Fatalf("%s: %d %s", q, st, b)
		}
		return decode[page](t, b)
	}
	got := func(p page) string {
		var s []string
		for _, it := range p.Items {
			s = append(s, it.Key+"@"+itoa(it.T-t0))
		}
		return strings.Join(s, " ")
	}
	// Newest first; one t: keys in order. Pages split a tie and carry on.
	p := read("?limit=3")
	if got(p) != "env.rh@2 env.temp@2 env.rh@1" || p.Next == nil || *p.Next != itoa(t0+1)+":env.rh" {
		t.Fatalf("page 1: %s %v", got(p), p.Next)
	}
	p = read("?limit=3&after=" + *p.Next)
	if got(p) != "env.temp@1 env.rh@0 env.temp@0" || p.Next != nil {
		t.Fatalf("page 2: %s %v", got(p), p.Next)
	}
	if p = read("?order=asc&limit=4"); got(p) != "env.rh@0 env.temp@0 env.rh@1 env.temp@1" {
		t.Fatalf("asc: %s", got(p))
	}
	if p = read("?order=asc&after=" + *p.Next); got(p) != "env.rh@2 env.temp@2" {
		t.Fatalf("asc page 2: %s", got(p))
	}
	if p = read("?since=" + itoa(t0+1) + "&before=" + itoa(t0+2)); got(p) != "env.rh@1 env.temp@1" {
		t.Fatalf("bounds: %s", got(p))
	}
	if p = read(""); string(p.Items[0].Value) != "42" {
		t.Fatalf("values: %s", p.Items[0].Value)
	}
	// Nothing matching is an empty page; a key outside the prefix as after is refused.
	if st, _, b := c.req("GET", S+"/nothing.*/series", ""); st != 200 || !strings.Contains(b, `"items":[]`) {
		t.Fatalf("no series: %d %s", st, b)
	}
	if st, _, _ := c.req("GET", S+"/env.*/series?after="+itoa(t0)+":other", ""); st != 400 {
		t.Fatal("after outside the prefix")
	}
	// More than 100 series under one prefix: narrow it.
	w = w[:0]
	for i := range 101 {
		w = append(w, `{"add":"shared/many.`+itoa(int64(i))+`","value":1}`)
	}
	c.req("POST", "/v1/home-cedar-pilot-mango/batch", "["+strings.Join(w[:100], ",")+"]", auth...)
	c.req("POST", S+"/many.x/series", "1", auth...)
	if st, _, b := c.req("GET", S+"/many.*/series", ""); st != 400 || errCode(b) != "too_many_series" {
		t.Fatalf("too many: %d %s", st, b)
	}
}
