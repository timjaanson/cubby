package cli

import "testing"

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
