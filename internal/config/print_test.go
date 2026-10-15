package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// roundTrip writes c out and loads it back the way serve would.
func roundTrip(t *testing.T, c *Config) (*Config, string) {
	t.Helper()
	out := c.TOML(Default(), "data goes somewhere")
	path := filepath.Join(t.TempDir(), "cubby.toml")
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("printed config doesn't load: %v\n%s", err, out)
	}
	return got, out
}

func comparable(c *Config) *Config {
	cp := *c
	cp.Trusted = nil
	return &cp
}

func TestPrintDefaultsRoundTrip(t *testing.T) {
	def, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	got, out := roundTrip(t, def)
	got.Server.RateLimits = nil // printed explicitly as true; nil means the same
	if !reflect.DeepEqual(comparable(got), comparable(def)) {
		t.Fatalf("defaults changed in a round trip:\n%s", out)
	}
	if strings.Contains(out, "# default:") {
		t.Fatalf("defaults shouldn't carry default notes:\n%s", out)
	}
}

func TestPrintChangedRoundTrip(t *testing.T) {
	c := Default()
	c.Server.Listen = "127.0.0.1:9000"
	c.Server.DataDir = `/srv/cubby "data"`
	off := false
	c.Server.RateLimits = &off
	c.Retention.SpaceIdleTTL = Duration(12 * time.Hour)
	c.Retention.TombstoneTTL = Duration(90 * time.Second)
	c.Limits.MaxValueSize = 1500
	c.Limits.MaxSpaces = 7
	c.Spaces = map[string]PinnedSpace{
		"status-k7m2x9q4": {MaxBytes: 100 << 20, MaxSeriesPoints: 500000},
		"home-k7m2x9q4":   {MaxKeys: 3},
	}
	got, out := roundTrip(t, c)
	if !reflect.DeepEqual(comparable(got), comparable(c)) {
		t.Fatalf("round trip changed values:\n%s", out)
	}
	for _, want := range []string{
		`listen = "127.0.0.1:9000"   # default: ":28229"`,
		`space_idle_ttl = "12h"   # default: "180d"`,
		`tombstone_ttl = "90s"`,
		`max_value_size = "1500B"`,
		"[spaces.home-k7m2x9q4]\nmax_keys = 3\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// Every setting Load understands must show up in the printed file, so a
// new field can't be added without documenting it there.
func TestPrintCoversEveryField(t *testing.T) {
	out := Default().TOML(Default(), "")
	var walk func(t reflect.Type)
	walk = func(rt reflect.Type) {
		for i := range rt.NumField() {
			f := rt.Field(i)
			tag := f.Tag.Get("toml")
			if tag == "" || tag == "-" {
				continue
			}
			ft := f.Type
			if ft.Kind() == reflect.Map {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				walk(ft)
				continue
			}
			if !strings.Contains(out, "\n"+tag+" = ") && !strings.Contains(out, "\n# "+tag+" = ") {
				t.Errorf("setting %s (%s) isn't in the printed config", tag, f.Name)
			}
		}
	}
	walk(reflect.TypeOf(Config{}))
}
