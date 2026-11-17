package cli

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timjaanson/cubby/internal/config"
	"github.com/timjaanson/cubby/internal/server"
	"github.com/timjaanson/cubby/internal/store"
)

func TestCLISpacesAndBatch(t *testing.T) {
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

	space, _ := cli(t, home, "new", "trip")
	other, _ := cli(t, home, "new", "notes")
	out, code := cli(t, home, "spaces")
	lines := strings.Split(out, "\n")
	if code != 0 || len(lines) != 2 || !strings.HasPrefix(lines[0], other+"\towner\t") || !strings.HasPrefix(lines[1], space+"\towner\t") {
		t.Fatalf("spaces: %d %q", code, out)
	}
	if out, _ := cli(t, home, "spaces", "--json"); !strings.Contains(out, `"via":"created"`) {
		t.Fatalf("spaces --json: %q", out)
	}

	f := filepath.Join(t.TempDir(), "w.json")
	os.WriteFile(f, []byte(`[{"method":"PUT","path":"shared/title","body":"Trip"},{"method":"PUT","path":"by/me/n","text":"x"}]`), 0o600)
	out, code = cli(t, home, "batch", space, f)
	if code != 0 || !strings.HasPrefix(out, "shared/title\t") || !strings.Contains(out, "\nby/me/n\t") {
		t.Fatalf("batch: %d %q", code, out)
	}
	if out, _ := cli(t, home, "get", space+"/shared/title"); out != `"Trip"` {
		t.Fatalf("get after batch: %q", out)
	}
	os.WriteFile(f, []byte(`[{"method":"PUT","path":"shared/a","body":1},{"method":"DELETE","path":"shared/missing"}]`), 0o600)
	if _, code := cli(t, home, "batch", space, f); code == 0 {
		t.Fatal("failing batch exited 0")
	}
	if _, code := cli(t, home, "get", space+"/shared/a"); code != 1 {
		t.Fatalf("failed batch wrote: %d", code)
	}
}
