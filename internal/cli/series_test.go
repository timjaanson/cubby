package cli

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/timjaanson/cubby/internal/config"
	"github.com/timjaanson/cubby/internal/server"
	"github.com/timjaanson/cubby/internal/store"
)

func TestCLISeriesRange(t *testing.T) {
	cfg := config.Default()
	cfg.Server.DataDir = t.TempDir()
	cfg.Server.RateLimits = new(bool)
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(cfg, st, nil))
	t.Cleanup(func() { ts.Close(); st.Close() })
	t.Setenv("CUBBY_SERVER", ts.URL)
	home := t.TempDir()
	space, code := cli(t, home, "new", "env")
	if code != 0 {
		t.Fatalf("new: %d %s", code, space)
	}
	t0 := time.Now().UnixMilli() - 60_000
	ms := func(n int64) string { return strconv.FormatInt(n, 10) }
	for i := int64(0); i < 5; i++ {
		args := []string{"add", space + "/shared/m1", ms(i), "--t", ms(t0 + i)}
		if i == 4 {
			args = append(args, "--ttl", "1h")
		}
		if out, code := cli(t, home, args...); code != 0 {
			t.Fatalf("add: %d %s", code, out)
		}
	}
	lines := func(s string) []string { return strings.Split(s, "\n") }

	// --order desc pages back past the server's page; --limit stops it.
	out, code := cli(t, home, "series", space+"/shared/m1", "--order", "desc", "--limit", "3")
	if l := lines(out); code != 0 || len(l) != 3 || l[0] != ms(t0+4)+"\t4" || l[2] != ms(t0+2)+"\t2" {
		t.Fatalf("desc: %d %q", code, out)
	}
	// since inclusive, before exclusive.
	out, _ = cli(t, home, "series", space+"/shared/m1", "--since", ms(t0+1), "--before", ms(t0+3))
	if out != ms(t0+2)+"\t2\n"+ms(t0+1)+"\t1" {
		t.Fatalf("since/before: %q", out)
	}
	out, _ = cli(t, home, "series", space+"/shared/m1", "--order", "desc", "--before", ms(t0+1))
	if out != ms(t0)+"\t0" {
		t.Fatalf("desc before: %q", out)
	}
	// Newest first by default; --order asc for oldest first.
	if out, _ = cli(t, home, "series", space+"/shared/m1", "--limit", "2"); out != ms(t0+4)+"\t4\n"+ms(t0+3)+"\t3" {
		t.Fatalf("newest first: %q", out)
	}
	if out, _ = cli(t, home, "series", space+"/shared/m1", "--order", "asc", "--limit", "2"); out != ms(t0)+"\t0\n"+ms(t0+1)+"\t1" {
		t.Fatalf("oldest first: %q", out)
	}
	// Several series, merged by time, each line with its key; pages follow.
	cli(t, home, "add", space+"/shared/m2", "9", "--t", ms(t0+4))
	out, _ = cli(t, home, "series", space+"/shared/m*", "--limit", "3")
	if out != "m1\t"+ms(t0+4)+"\t4\nm2\t"+ms(t0+4)+"\t9\nm1\t"+ms(t0+3)+"\t3" {
		t.Fatalf("pattern: %q", out)
	}
	if out, _ = cli(t, home, "series", space+"/shared/m*", "--order", "asc"); len(lines(out)) != 6 || !strings.HasPrefix(out, "m1\t"+ms(t0)) {
		t.Fatalf("pattern asc: %q", out)
	}
	// --json is NDJSON and carries expires when a point has one.
	out, _ = cli(t, home, "series", space+"/shared/m1", "--json", "--order", "desc", "--limit", "2")
	l := lines(out)
	want := `{"t":` + ms(t0+4) + `,"value":4,"expires":` + ms(t0+4+3_600_000) + `}`
	if len(l) != 2 || l[0] != want || l[1] != `{"t":`+ms(t0+3)+`,"value":3}` {
		t.Fatalf("json: %q", out)
	}
	if _, code := cli(t, home, "series", space+"/shared/m1", "--order", "newest"); code != 64 {
		t.Fatal("bad --order accepted")
	}
}

func TestCLINewSlugifies(t *testing.T) {
	cfg := config.Default()
	cfg.Server.DataDir = t.TempDir()
	cfg.Server.RateLimits = new(bool)
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(cfg, st, nil))
	t.Cleanup(func() { ts.Close(); st.Close() })
	t.Setenv("CUBBY_SERVER", ts.URL)
	home := t.TempDir()
	if out, code := cli(t, home, "new", "Home environment"); code != 0 || !strings.HasPrefix(out, "home-environment-") || strings.Count(out, "-") != 5 {
		t.Fatalf("new: %d %q", code, out)
	}
	if out, code := cli(t, home, "new", "  Kitchen & Bath!! "); code != 0 || !strings.HasPrefix(out, "kitchen-bath-") {
		t.Fatalf("punctuation: %d %q", code, out)
	}
	long := strings.Repeat("Very long label ", 10)
	if out, code := cli(t, home, "new", long); code != 0 || len(out) > 63 {
		t.Fatalf("long: %d %q", code, out)
	}
	if _, code := cli(t, home, "new", "!!! ???"); code != 5 {
		t.Fatalf("nothing left: %d", code)
	}
}

func TestCLIOwnerInvite(t *testing.T) {
	cfg := config.Default()
	cfg.Server.DataDir = t.TempDir()
	cfg.Server.RateLimits = new(bool)
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(cfg, st, nil))
	t.Cleanup(func() { ts.Close(); st.Close() })
	t.Setenv("CUBBY_SERVER", ts.URL)
	owner, laptop := t.TempDir(), t.TempDir()
	space, _ := cli(t, owner, "new", "home")
	inv, code := cli(t, owner, "invite", "new", space, "--access", "owner")
	if code != 0 || !strings.HasPrefix(inv, "inv_") {
		t.Fatalf("invite new: %d %q", code, inv)
	}
	if _, code := cli(t, owner, "invite", "new", space, "--access", "owner", "--uses", "3"); code == 0 {
		t.Fatal("owner invite with 3 uses accepted")
	}
	if out, code := cli(t, laptop, "join", space, inv); code != 0 || out != "owner" {
		t.Fatalf("join: %d %q", code, out)
	}
}

// When nothing named a server, a failed connection says how to name one.
func TestCLIDefaultServerHint(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CUBBY_SERVER", "")
	old := defaultServer
	defaultServer = "http://127.0.0.1:1"
	t.Cleanup(func() { defaultServer = old })
	f, _ := parseFlags(nil, nil, nil)
	c, err := newClient(f)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.raw("GET", "/v1/time", nil, nil)
	if msg := err.Error(); !strings.Contains(msg, "http://127.0.0.1:1") || !strings.Contains(msg, "--server, CUBBY_SERVER") || !strings.Contains(msg, "config.toml") {
		t.Fatalf("default: %v", err)
	}
	// A server someone named fails with the plain error.
	t.Setenv("CUBBY_SERVER", "http://127.0.0.1:1")
	c, _ = newClient(f)
	if _, _, err = c.raw("GET", "/v1/time", nil, nil); strings.Contains(err.Error(), "--server") {
		t.Fatalf("named: %v", err)
	}
}
