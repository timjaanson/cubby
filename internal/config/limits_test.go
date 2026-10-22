package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigurableRates(t *testing.T) {
	c := Default()
	c.Limits.WritesPerMinute = 241
	c.Limits.WriteBurst = 17
	c.Limits.ReadsPerIPPerMinute = 456
	c.Limits.WatchesPerIP = 12
	c.Limits.TTLMinSeconds = 10
	c.Limits.TTLMaxSeconds = 60
	got, _ := roundTrip(t, c)
	if got.Limits != c.Limits {
		t.Fatal("rate settings did not round trip")
	}
	for _, bad := range []string{"writes_per_minute = 0", "write_burst = -1", "watches_per_ip = 0", "ttl_min_seconds = 10\nttl_max_seconds = 5", "ttl_max_seconds = 315360001"} {
		p := filepath.Join(t.TempDir(), "bad.toml")
		if err := os.WriteFile(p, []byte("[limits]\n"+bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}
