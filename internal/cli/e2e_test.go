package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/timjaanson/cubby/internal/config"
	"github.com/timjaanson/cubby/internal/server"
	"github.com/timjaanson/cubby/internal/store"
)

// cli runs one command with its own config directory, as a separate
// machine would, and returns stdout and the exit code.
func cli(t *testing.T, home string, args ...string) (string, int) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", home)
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	code := Run(args)
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return strings.TrimSpace(string(out)), code
}

func TestCLISharing(t *testing.T) {
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
	t.Setenv("CUBBY_TOKEN", "")
	owner, member, stranger := t.TempDir(), t.TempDir(), t.TempDir()

	out, code := cli(t, owner, "new", "family")
	lines := strings.Split(out, "\n")
	if code != 0 || len(lines) != 2 || !strings.HasPrefix(lines[0], "family-") || !strings.HasPrefix(lines[1], "ot_") {
		t.Fatalf("new: %d %q", code, out)
	}
	space := lines[0]
	if out, code := cli(t, owner, "set", space+"/shared/item.1", "milk"); code != 0 {
		t.Fatalf("owner set: %d %s", code, out)
	}
	if out, _ := cli(t, owner, "settings", space); !strings.Contains(out, "public\tnone") {
		t.Fatalf("settings: %q", out)
	}
	// A stranger sees nothing: exit 1, like a missing space.
	if _, code := cli(t, stranger, "get", space+"/shared/item.1"); code != 1 {
		t.Fatalf("stranger get: %d", code)
	}
	link, code := cli(t, owner, "token", "new", space, "--label", "anna", "--uses", "1", "--link", "https://example.com/list/#list="+space)
	if code != 0 || !strings.Contains(link, "&t=at_") {
		t.Fatalf("token new: %d %q", code, link)
	}
	if out, code := cli(t, member, "join", space, link); code != 0 || out != "*:rw" {
		t.Fatalf("join: %d %q", code, out)
	}
	if out, _ := cli(t, member, "get", space+"/shared/item.1"); out != "milk" {
		t.Fatalf("member get: %q", out)
	}
	if out, _ := cli(t, member, "access", space); !strings.HasPrefix(out, "member") {
		t.Fatalf("access: %q", out)
	}
	if _, code := cli(t, stranger, "join", space, link); code != 3 {
		t.Fatalf("used-up invite: %d", code)
	}
	if out, _ := cli(t, owner, "token", "ls", space); !strings.Contains(out, "used_up") || !strings.Contains(out, "anna") {
		t.Fatalf("token ls: %q", out)
	}
	if out, code := cli(t, member, "token", "ls", space); code != 0 || strings.Contains(out, "anna") {
		t.Fatalf("member token ls shows only their own: %d %q", code, out)
	}
	out, _ = cli(t, owner, "grant", "ls", space)
	var memberGrant string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "\tmember\t") {
			memberGrant = strings.Fields(l)[0]
		}
	}
	if memberGrant == "" {
		t.Fatalf("grant ls: %q", out)
	}
	if _, code := cli(t, owner, "grant", "rm", space, memberGrant); code != 0 {
		t.Fatal("grant rm")
	}
	if _, code := cli(t, member, "get", space+"/shared/item.1"); code != 1 {
		t.Fatalf("removed member: %d", code)
	}
	// Opening up.
	if out, code := cli(t, owner, "settings", space, "--public", "read"); code != 0 || !strings.Contains(out, "public\tread") {
		t.Fatalf("settings change: %d %q", code, out)
	}
	if out, _ := cli(t, stranger, "get", space+"/shared/item.1"); out != "milk" {
		t.Fatalf("public read: %q", out)
	}
	if _, code := cli(t, stranger, "set", space+"/shared/x", "1"); code != 3 {
		t.Fatalf("stranger write: %d", code)
	}
	// The owner token makes a second machine an owner.
	if out, code := cli(t, stranger, "join", space, lines[1]); code != 0 || out != "owner" {
		t.Fatalf("owner join: %d %q", code, out)
	}
	if _, code := cli(t, stranger, "drop", space, "--yes"); code != 0 {
		t.Fatal("drop")
	}
	if _, code := cli(t, owner, "get", space+"/shared/item.1"); code != 1 {
		t.Fatal("dropped space still there")
	}
	if out, _ := cli(t, owner, "whoami", "--show-token"); !strings.HasPrefix(out, "vt_") {
		t.Fatalf("whoami --show-token: %q", out)
	}
}

func TestCLIMetadata(t *testing.T) {
	cfg := config.Default()
	cfg.Server.DataDir = t.TempDir()
	cfg.Server.RateLimits = new(bool)
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(cfg, st, nil))
	defer ts.Close()
	defer st.Close()
	t.Setenv("CUBBY_SERVER", ts.URL)
	t.Setenv("CUBBY_TOKEN", "")
	home := t.TempDir()
	out, code := cli(t, home, "limits")
	if code != 0 || !strings.Contains(out, `"writes_per_minute": 120`) {
		t.Fatalf("limits %d %s", code, out)
	}
	out, code = cli(t, home, "new")
	if code != 0 {
		t.Fatal("new failed")
	}
	space := strings.Split(out, "\n")[0]
	path := space + "/shared/lease"
	out, code = cli(t, home, "set", path, "holder", "--ttl", "30s", "--create")
	if code != 0 {
		t.Fatalf("set ttl %d %s", code, out)
	}
	e, err := st.Get(space, "s", "lease")
	if err != nil || e.Expires-e.T != 30000 {
		t.Fatalf("ttl not forwarded %+v %v", e, err)
	}
	for _, args := range [][]string{{"set", path, "late", "--if-before", "0"}, {"del", path, "--if-before", "0"}, {"add", space + "/shared/points", "1", "--if-before", "0"}} {
		if _, code = cli(t, home, args...); code != 2 {
			t.Fatalf("deadline %v -> %d", args, code)
		}
	}
	out, code = cli(t, home, "limits", space)
	if code != 0 || !strings.Contains(out, `"keys": 1`) || !strings.Contains(out, `"rateLimits": false`) {
		t.Fatalf("space limits %d %s", code, out)
	}
	out, code = cli(t, home, "time")
	if code != 0 || !strings.Contains(out, "offset_ms\t") || !strings.Contains(out, "rtt_ms\t") {
		t.Fatalf("time %d %s", code, out)
	}
	if _, code = cli(t, home, "set", path, "x", "--ttl", "100ms"); code != 64 {
		t.Fatalf("fractional ttl %d", code)
	}
}

func TestCLIWriteDoesNotRetryRateLimit(t *testing.T) {
	count := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(429)
		w.Write([]byte(`{"error":"rate_limited","retryAfter":0}`))
	}))
	defer ts.Close()
	c := &client{server: ts.URL, envToken: "vt_test", http: ts.Client()}
	resp, _, err := c.do("PUT", "/v1/test/shared/x", []byte("x"), reqOpts{})
	if err != nil || resp.StatusCode != 429 || count != 1 {
		t.Fatalf("write retried count=%d err=%v", count, err)
	}
}
