package cli

import (
	"strings"
	"testing"
)

func TestCLIPaths(t *testing.T) {
	c := &client{dc: &deviceCreds{Visitor: "u_aaaaaaaaaaaaaaaa"}}
	good := map[string]string{
		"poll-cedar-pilot-mango/shared/q":                "/v1/poll-cedar-pilot-mango/shared/q",
		"/v1/poll-cedar-pilot-mango/shared/q":            "/v1/poll-cedar-pilot-mango/shared/q",
		"poll-cedar-pilot-mango/shared/msg.*":            "/v1/poll-cedar-pilot-mango/shared/msg.*",
		"poll-cedar-pilot-mango/by/*/vote":               "/v1/poll-cedar-pilot-mango/by/*/vote",
		"poll-cedar-pilot-mango/by/me/vote":              "/v1/poll-cedar-pilot-mango/by/me/vote",
		"poll-cedar-pilot-mango/to/me/a:b":               "/v1/poll-cedar-pilot-mango/to/me/a:b",
		"poll-cedar-pilot-mango/to/owners/x":             "/v1/poll-cedar-pilot-mango/to/owners/x",
		"poll-cedar-pilot-mango/inbox/*/x.*":             "/v1/poll-cedar-pilot-mango/inbox/*/x.*",
		"poll-cedar-pilot-mango/inbox/owners/*/x":        "/v1/poll-cedar-pilot-mango/inbox/owners/*/x",
		"poll-cedar-pilot-mango/by/u_bbbbbbbbbbbbbbbb/x": "/v1/poll-cedar-pilot-mango/by/u_bbbbbbbbbbbbbbbb/x",
	}
	for in, want := range good {
		p, err := c.parsePath(in)
		if err != nil || p.url() != want {
			t.Errorf("parsePath(%q) = %q, %v; want %q", in, p.url(), err, want)
		}
	}
	for _, in := range []string{
		"poll-cedar-pilot-mango/shared/a#b", "poll-cedar-pilot-mango/shared/a?x=1", "poll-cedar-pilot-mango/shared/a%2Fb",
		"poll-cedar-pilot-mango/shared/..", "poll-cedar-pilot-mango/shared/.", "poll-cedar-pilot-mango/shared/watch",
		"poll-cedar-pilot-mango/shared/a b", "poll-cedar-pilot-mango/shared/", "poll-cedar-pilot-mango/by/../x",
		"poll-cedar-pilot-mango/by/bob/x", "poll-cedar-pilot-mango/users/me/x", "poll/shared/x", "../poll-cedar-pilot-mango/shared/x",
		"poll-cedar-pilot-mango/shared/a/b", "poll-cedar-pilot-mango/other/x", "poll-cedar-pilot-mango/me/x", "poll-cedar-pilot-mango/to/bob/x", "poll-cedar-pilot-mango/to/me/café",
	} {
		if _, err := c.parsePath(in); err == nil {
			t.Errorf("parsePath(%q) accepted", in)
		}
	}
}

func TestCLIFlags(t *testing.T) {
	f, err := parseFlags([]string{"poll-cedar-pilot-mango/shared/t", "-5", "--if-version", "3"}, []string{"if-version"}, nil)
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

// Every command's help is its own, and token takes --ttl.
func TestTokenFlags(t *testing.T) {
	if h := commandFlags["token"].help; !strings.HasPrefix(h, "token [--ttl") {
		t.Errorf("token help: %q", h)
	}
	if v := commandFlags["token"].vals; len(v) != 1 || v[0] != "ttl" {
		t.Errorf("token flags: %v", v)
	}
	for name, c := range commandFlags {
		if strings.Contains(c.help, "#t=") || strings.Contains(c.help, "vt_") || strings.Contains(c.help, "ot_") {
			t.Errorf("%s help mentions old credentials", name)
		}
	}
}
