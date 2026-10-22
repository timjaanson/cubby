package server

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

type seriesResp struct {
	Key    string
	Points []struct {
		T     int64
		Value json.RawMessage
	}
	Version int64
	Next    *int64
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
	_, _, b = c.req("GET", S+"/temp/series", "")
	r := decode[seriesResp](t, b)
	if r.Key != "temp" || len(r.Points) != 4 || r.Next != nil {
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

	// limit without since: the newest ones.
	_, _, b = c.req("GET", S+"/temp/series?limit=2", "")
	r = decode[seriesResp](t, b)
	if len(r.Points) != 2 || string(r.Points[1].Value) != want[3] {
		t.Fatalf("newest 2: %s", b)
	}

	// since pages forward with next.
	_, _, b = c.req("GET", S+"/temp/series?since=0&limit=3", "")
	r = decode[seriesResp](t, b)
	if len(r.Points) != 3 || r.Next == nil || *r.Next != r.Points[2].T {
		t.Fatalf("page 1: %s", b)
	}
	_, _, b = c.req("GET", S+"/temp/series?since="+itoa(*r.Next)+"&limit=3", "")
	r = decode[seriesResp](t, b)
	if len(r.Points) != 1 || r.Next != nil || string(r.Points[0].Value) != want[3] {
		t.Fatalf("page 2: %s", b)
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
	_, _, b = c.req("GET", S+"/*/watch?version=0", "")
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
		{"GET", S + "/env.*/series", "", 400, "invalid_key"},
		{"GET", "/v1/home-cedar-pilot-mango/users/*/temp/series", "", 400, "invalid_key"},
		{"GET", S + "/temp/series/watch", "", 400, "invalid_key"},
		{"GET", S + "/temp/history", "", 400, "invalid_key"},
		{"POST", S + "/env.*", "{}", 405, "method_not_allowed"},
		{"POST", S + "/temp/series", "", 400, "empty_value"},
		{"POST", S + "/temp/series?ifVersion=1", "5", 400, "invalid_parameter"},
		{"POST", S + "/temp/series?t=abc", "5", 400, "invalid_parameter"},
		{"POST", S + "/temp/series?t=" + itoa(time.Now().UnixMilli()+120_000), "5", 400, "invalid_parameter"},
		{"POST", S + "/temp/series", strings.Repeat("x", 1025), 413, "value_too_large"},
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

func TestSeriesBatch(t *testing.T) {
	c := newTest(t)
	tok, _ := c.anon()
	c.open("home-cedar-pilot-mango", tok)
	auth := []string{"Authorization", "Bearer " + tok}
	S := "/v1/home-cedar-pilot-mango/shared"

	st, _, b := c.req("POST", S+"/env.*/series", `{"temp": 21.4, "rh":45, "door":"open", "gps":{"lat":59.43}}`, auth...)
	if st != 200 {
		t.Fatalf("batch: %d %s", st, b)
	}
	res := decode[struct {
		Version, T int64
		Keys       []string
	}](t, b)
	if strings.Join(res.Keys, ",") != "env.temp,env.rh,env.door,env.gps" {
		t.Fatalf("keys in member order: %v", res.Keys)
	}
	for key, want := range map[string]string{"env.temp": "21.4", "env.rh": "45", "env.door": `"open"`, "env.gps": `{"lat":59.43}`} {
		_, h, b := c.req("GET", S+"/"+key, "")
		if b != want || h.Get("KV-Time") != itoa(res.T) {
			t.Errorf("%s = %s (t %s), want %s at %d", key, b, h.Get("KV-Time"), want, res.T)
		}
	}
	// An object posted to one key is one point.
	c.req("POST", S+"/pos/series", `{"lat": 59.43, "lon": 24.75}`, auth...)
	_, _, b = c.req("GET", S+"/pos/series", "")
	if r := decode[seriesResp](t, b); len(r.Points) != 1 || string(r.Points[0].Value) != `{"lat":59.43,"lon":24.75}` {
		t.Fatalf("object point: %s", b)
	}
	// Bare * uses the names as they are.
	if st, _, b := c.req("POST", S+"/*/series", `{"co2": 612}`, auth...); st != 200 || !strings.Contains(b, `"keys":["co2"]`) {
		t.Fatalf("bare *: %d %s", st, b)
	}

	c.req("PUT", S+"/env.note", `"hi"`, auth...)
	for _, tt := range []struct {
		body, code, key string
	}{
		{`[1, 2]`, "batch_needs_object", ""},
		{`21.4`, "batch_needs_object", ""},
		{`not json`, "batch_needs_object", ""},
		{`{}`, "batch_needs_object", ""},
		{`{"temp": 1, "temp": 2}`, "batch_needs_object", "env.temp"},
		{`{"temp": 22, "bad name": 1}`, "invalid_key", "env.bad name"},
		{`{"temp": 22, "note": 1}`, "not_series", "env.note"},
		{`{"temp": 22, "big": "` + strings.Repeat("x", 1100) + `"}`, "value_too_large", "env.big"},
	} {
		st, _, b := c.req("POST", S+"/env.*/series", tt.body, auth...)
		if errCode(b) != tt.code || st < 400 {
			t.Errorf("%s: %d %s, want %s", tt.body, st, b, tt.code)
		}
		if tt.key != "" && !strings.Contains(b, `"key":"`+tt.key+`"`) {
			t.Errorf("%s: error doesn't name %s: %s", tt.body, tt.key, b)
		}
	}
	// All or nothing: none of the failed batches added to env.temp.
	_, _, b = c.req("GET", S+"/env.temp/series", "")
	if r := decode[seriesResp](t, b); len(r.Points) != 1 {
		t.Fatalf("a failed batch left points behind: %s", b)
	}
	// Only the bare names are reserved: env.series is a fine key, series isn't.
	if st, _, b := c.req("POST", S+"/*/series", `{"series": 1}`, auth...); st != 400 || !strings.Contains(b, `"key":"series"`) {
		t.Errorf("reserved member name: %d %s", st, b)
	}
	var members []string
	for i := range 101 {
		members = append(members, `"k`+itoa(int64(i))+`": 1`)
	}
	if st, _, b := c.req("POST", S+"/m.*/series", "{"+strings.Join(members, ",")+"}", auth...); st != 400 || errCode(b) != "batch_needs_object" {
		t.Errorf("101 members: %d %s", st, b)
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
	_, _, b = c.req("GET", S+"/temp/series", "")
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
	_, _, b := c.req("GET", S+"/temp/series", "")
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
	if st, _, b := c.req("POST", sp+"/users/"+id+"/weight/series", "71.2", auth...); st != 201 {
		t.Fatalf("own users/: %d %s", st, b)
	}
	if st, _, b := c.req("POST", sp+"/users/"+id+"/weight/series", "71.0", "Authorization", "Bearer "+tok2); st != 403 || errCode(b) != "not_yours" {
		t.Fatalf("other visitor: %d %s", st, b)
	}
	if st, _, _ := c.req("GET", sp+"/users/"+id+"/weight/series", ""); st != 200 {
		t.Fatalf("public read: %d", st)
	}
	if st, _, b := c.req("POST", sp+"/me/sleep/series", "7.5", auth...); st != 201 {
		t.Fatalf("me/: %d %s", st, b)
	}
	if st, _, _ := c.req("GET", sp+"/me/sleep/series", ""); st != 401 {
		t.Fatalf("me/ without token: %d", st)
	}
	if st, _, b := c.req("POST", sp+"/users/*/weight/series", "1", auth...); st != 400 {
		t.Fatalf("users/*/ series: %d %s", st, b)
	}
}

// A device with a write-only grant adds points to a space it can't read.
func TestSeriesWriteOnlyGrant(t *testing.T) {
	c := newTest(t)
	own := c.create(`{"label":"room","public":"read"}`, "")
	o := []string{"Authorization", "Bearer " + own.Token}
	_, _, b := c.req("POST", "/v1/"+own.Space+"/tokens", `{"allow":["shared/env.*:w"],"uses":1}`, o...)
	tk := decode[struct{ Token string }](t, b)
	_, _, b = c.req("POST", "/v1/"+own.Space+"/join", `{"token":"`+tk.Token+`"}`)
	dev := decode[struct{ Token string }](t, b)
	d := []string{"Authorization", "Bearer " + dev.Token}
	S := "/v1/" + own.Space + "/shared"
	if st, _, b := c.req("POST", S+"/env.*/series", `{"temp":1}`, d...); st != 200 {
		t.Fatalf("device write: %d %s", st, b)
	}
	if st, _, b := c.req("POST", S+"/other/series", `1`, d...); st != 403 || errCode(b) != "no_access" {
		t.Fatalf("outside its scope: %d %s", st, b)
	}
	if st, _, b := c.req("POST", S+"/*/series", `{"env.x":1}`, d...); st != 403 || errCode(b) != "no_access" {
		t.Fatalf("a batch wider than its scope: %d %s", st, b)
	}
	if st, _, b := c.req("POST", S+"/env.*/series", `{"temp":1}`); st != 401 {
		t.Fatalf("no token: %d %s", st, b)
	}
}
