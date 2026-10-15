package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every way a path or parameter can be bent, and what the server must answer.
func TestHTTPValidation(t *testing.T) {
	c := newTest(t)
	tok, id := c.anon()
	sp := "/v1/poll-k7m2x9q4"
	cases := []struct {
		method, path string
		status       int
		code         string
	}{
		// keys
		{"PUT", sp + "/shared/..", 400, "invalid_key"},
		{"PUT", sp + "/shared/%2E%2E", 400, "invalid_key"},
		{"PUT", sp + "/shared/.", 400, "invalid_key"},
		{"PUT", sp + "/shared/watch", 400, "invalid_key"},
		{"PUT", sp + "/shared/%77atch", 400, "invalid_key"},
		{"PUT", sp + "/shared/a%2Fb", 400, "invalid_key"},
		{"PUT", sp + "/shared/a%00b", 400, "invalid_key"},
		{"PUT", sp + "/shared/a%0Ab", 400, "invalid_key"},
		{"PUT", sp + "/shared/a%20b", 400, "invalid_key"},
		{"PUT", sp + "/shared/a%25b", 400, "invalid_key"},
		{"PUT", sp + "/shared/a%3Fb", 400, "invalid_key"},
		{"PUT", sp + "/shared/a%23b", 400, "invalid_key"},
		{"PUT", sp + "/shared/caf%C3%A9", 400, "invalid_key"},
		{"PUT", sp + "/shared/" + strings.Repeat("k", 129), 400, "invalid_key"},
		{"PUT", sp + "/shared//x", 400, "invalid_key"},
		{"PUT", sp + "/shared/x/", 400, "invalid_key"},
		{"PUT", sp + "/shared/a*b", 400, "invalid_key"},
		{"PUT", sp + "/shared/msg.*", 405, "method_not_allowed"},
		{"PUT", sp + "/shared/" + strings.Repeat("k", 128), 201, ""},
		{"PUT", sp + "/shared/a.b:c-d_e", 201, ""},
		{"PUT", sp + "/shared/...", 201, ""},
		// spaces
		{"PUT", "/v1/..%2F..%2Fetc-k7m2x9q4/shared/x", 400, "invalid_space"},
		{"PUT", "/v1/%2E%2E/shared/x", 400, "invalid_space"},
		{"PUT", "/v1/poll-k7m2x9q4%00/shared/x", 400, "invalid_space"},
		{"PUT", "/v1/POLL-K7M2X9Q4/shared/x", 400, "invalid_space"},
		{"PUT", "/v1/poll--k7m2x9q4/shared/x", 400, "invalid_space"},
		{"PUT", "/v1/-k7m2x9q4/shared/x", 400, "invalid_space"},
		{"PUT", "/v1/" + strings.Repeat("a", 55) + "-k7m2x9q4/shared/x", 400, "invalid_space"},
		{"PUT", "/v1//shared/x", 400, "invalid_space"},
		{"GET", "/v1/anon/shared/x", 400, "invalid_space"},
		// visitor IDs
		{"PUT", sp + "/users/../shared/x", 400, "invalid_key"},
		{"PUT", sp + "/users/u_aaaaaaaaaaaaaaaa%2F..%2Fx/k", 400, "invalid_key"},
		{"PUT", sp + "/users/U_AAAAAAAAAAAAAAAA/k", 400, "invalid_key"},
		{"PUT", sp + "/users/" + id + "/k", 201, ""},
		{"PUT", sp + "/users/u_aaaaaaaaaaaaaaaa/k", 403, "not_owner"},
		{"PUT", sp + "/users/*/k", 405, "method_not_allowed"},
		// areas and routes
		{"GET", sp + "/private/x", 404, "not_found"},
		{"GET", sp + "/shared", 404, "not_found"},
		{"GET", "/v%31/poll-k7m2x9q4/shared/x", 404, "not_found"},
		// parameters
		{"PUT", sp + "/shared/p?ifversion=0", 400, "invalid_parameter"},
		{"PUT", sp + "/shared/p?ifVersion=1&ifVersion=0", 400, "invalid_parameter"},
		{"PUT", sp + "/shared/p?ifVersion=%2B0", 400, "invalid_parameter"},
		{"PUT", sp + "/shared/p?ifVersion=-1", 400, "invalid_parameter"},
		{"PUT", sp + "/shared/p?ifVersion=0x1", 400, "invalid_parameter"},
		{"PUT", sp + "/shared/p?ifVersion=%200", 400, "invalid_parameter"},
		{"PUT", sp + "/shared/p?ifVersion=99999999999999999", 400, "invalid_parameter"},
		{"PUT", sp + "/shared/p?ifVersion=0&_=123", 201, ""},
		{"GET", sp + "/shared/p?limit=5", 400, "invalid_parameter"},
		{"GET", sp + "/shared/*?limit=0", 400, "invalid_parameter"},
		{"GET", sp + "/shared/*?limit=1001", 400, "invalid_parameter"},
		{"GET", sp + "/shared/*?limit=%2B5", 400, "invalid_parameter"},
		{"GET", sp + "/shared/*?order=DESC", 400, "invalid_parameter"},
		{"GET", sp + "/shared/*?after=a%2Fb", 400, "invalid_parameter"},
		{"GET", sp + "/shared/*?after=a%23b", 400, "invalid_parameter"},
		{"GET", sp + "/shared/*?timeout=5", 400, "invalid_parameter"},
		{"GET", sp + "/shared/x/watch?timeout=0", 400, "invalid_parameter"},
		{"GET", sp + "/shared/x/watch?timeout=56", 400, "invalid_parameter"},
		{"GET", sp + "/shared/x/watch?version=-1", 400, "invalid_parameter"},
		{"GET", sp + "/shared/x/watch?since=0", 400, "invalid_parameter"},
		{"GET", sp + "/shared/x/watch?version=1;2", 400, "invalid_parameter"},
		{"POST", "/v1/spaces?label=Poll", 400, "invalid_parameter"},
		{"POST", "/v1/spaces?label=a--b", 400, "invalid_parameter"},
		{"POST", "/v1/spaces?label=../x", 400, "invalid_parameter"},
		{"POST", "/v1/spaces?locked=yes", 400, "invalid_parameter"},
		{"POST", "/v1/spaces?lock", 400, "invalid_parameter"},
		{"POST", "/v1/anon?x=1", 400, "invalid_parameter"},
	}
	for _, tc := range cases {
		s, _, b := c.req(tc.method, tc.path, "v", "Authorization", "Bearer "+tok)
		if s != tc.status || (tc.code != "" && errCode(b) != tc.code) {
			t.Errorf("%s %s: got %d %s, want %d %s", tc.method, tc.path, s, strings.TrimSpace(b), tc.status, tc.code)
		}
	}
}

func TestHTTPTokens(t *testing.T) {
	c := newTest(t)
	tok, id := c.anon()
	for _, h := range []string{"Bearer " + tok, "bearer " + tok, "BEARER  " + tok} {
		if _, _, b := c.req("GET", "/v1/whoami", "", "Authorization", h); !strings.Contains(b, id) {
			t.Errorf("Authorization %.8s...: %s", h, b)
		}
	}
	if _, _, b := c.req("GET", "/v1/whoami?token="+tok, ""); !strings.Contains(b, id) {
		t.Errorf("token parameter: %s", b)
	}
	for _, bad := range []string{"kv_short", tok + "x", "sk_" + tok[3:], "kv_" + strings.Repeat("!", 43)} {
		if s, _, b := c.req("GET", "/v1/whoami", "", "Authorization", "Bearer "+bad); s != 401 || errCode(b) != "invalid_token" {
			t.Errorf("token %q: %d %s", bad, s, b)
		}
	}
	// A bad token on a read that doesn't need one is ignored.
	if s, _, _ := c.req("GET", "/v1/poll-k7m2x9q4/shared/x", "", "Authorization", "Bearer junk"); s != 404 {
		t.Errorf("read with junk token: %d", s)
	}
}

func TestCLIPaths(t *testing.T) {
	c := &client{sc: &serverCreds{Token: "kv_x", ID: "u_aaaaaaaaaaaaaaaa"}}
	good := map[string]string{
		"poll-k7m2x9q4/shared/q":                   "/v1/poll-k7m2x9q4/shared/q",
		"/v1/poll-k7m2x9q4/shared/q":               "/v1/poll-k7m2x9q4/shared/q",
		"poll-k7m2x9q4/shared/msg.*":               "/v1/poll-k7m2x9q4/shared/msg.*",
		"poll-k7m2x9q4/users/*/vote":               "/v1/poll-k7m2x9q4/users/*/vote",
		"poll-k7m2x9q4/users/me/vote":              "/v1/poll-k7m2x9q4/users/u_aaaaaaaaaaaaaaaa/vote",
		"poll-k7m2x9q4/me/a:b":                     "/v1/poll-k7m2x9q4/me/a:b",
		"poll-k7m2x9q4/users/u_bbbbbbbbbbbbbbbb/x": "/v1/poll-k7m2x9q4/users/u_bbbbbbbbbbbbbbbb/x",
	}
	for in, want := range good {
		p, err := c.parsePath(in)
		if err != nil || p.url() != want {
			t.Errorf("parsePath(%q) = %q, %v; want %q", in, p.url(), err, want)
		}
	}
	for _, in := range []string{
		"poll-k7m2x9q4/shared/a#b", "poll-k7m2x9q4/shared/a?x=1", "poll-k7m2x9q4/shared/a%2Fb",
		"poll-k7m2x9q4/shared/..", "poll-k7m2x9q4/shared/.", "poll-k7m2x9q4/shared/watch",
		"poll-k7m2x9q4/shared/a b", "poll-k7m2x9q4/shared/", "poll-k7m2x9q4/users/../x",
		"poll-k7m2x9q4/users/bob/x", "poll/shared/x", "../poll-k7m2x9q4/shared/x",
		"poll-k7m2x9q4/shared/a/b", "poll-k7m2x9q4/other/x", "poll-k7m2x9q4/me/café",
	} {
		if _, err := c.parsePath(in); err == nil {
			t.Errorf("parsePath(%q) accepted", in)
		}
	}
}

func TestCLIFlags(t *testing.T) {
	f, err := parseFlags([]string{"poll-k7m2x9q4/shared/t", "-5", "--if-version", "3"}, []string{"if-version"}, nil)
	if err != nil || len(f.args) != 2 || f.args[1] != "-5" || f.str("if-version") != "3" {
		t.Fatalf("negative value: %v %v", f, err)
	}
	if _, err := parseFlags([]string{"x", "-z"}, nil, nil); err == nil {
		t.Fatal("unknown flag accepted")
	}
	if f, _ := parseFlags([]string{"x", "--", "-z"}, nil, nil); len(f.args) != 2 || f.args[1] != "-z" {
		t.Fatal("-- didn't end flags")
	}
	for _, s := range []string{"ftp://x", "http://", "http://x?y=1", "http://x#y", "http://[::1"} {
		t.Setenv("CUBBY_SERVER", s)
		if _, err := newClient(&flags{vals: map[string]string{}, set: map[string]bool{}}); err == nil {
			t.Errorf("server %q accepted", s)
		}
	}
}

// FuzzRoute throws arbitrary paths and queries at the router. Nothing may
// answer 500 or panic, and no file may appear outside spaces/.
func FuzzRoute(f *testing.F) {
	for _, s := range []string{"/v1/poll-k7m2x9q4/shared/x", "/v1/poll-k7m2x9q4/users/*/v", "/v1/a/../b", "/v1/%2e%2e/x/y", "/v1/k7m2x9q4/me/x/watch?version=0&timeout=1"} {
		f.Add("PUT", s)
		f.Add("GET", s)
	}
	cfg := defaultConfig()
	cfg.Server.DataDir = f.TempDir()
	cfg.Server.RateLimits = new(bool)
	store, err := openStore(cfg)
	if err != nil {
		f.Fatal(err)
	}
	srv := newServer(cfg, store, nil)
	tok, _, _ := store.newVisitor()
	f.Fuzz(func(t *testing.T, method, target string) {
		if method != "GET" && method != "PUT" && method != "DELETE" && method != "POST" {
			return
		}
		r, err := http.NewRequest(method, "http://x"+target, strings.NewReader("v"))
		if err != nil || r.URL.Host != "x" || strings.Contains(target, "timeout") {
			return
		}
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		if w.Code >= 500 {
			t.Fatalf("%s %s: %d %s", method, target, w.Code, w.Body.String())
		}
		entries, _ := os.ReadDir(cfg.Server.DataDir)
		for _, e := range entries {
			switch n := e.Name(); {
			case n == "spaces", strings.HasPrefix(n, "server.db"):
			default:
				t.Fatalf("%s %s created %s", method, target, filepath.Join(cfg.Server.DataDir, n))
			}
		}
	})
}
