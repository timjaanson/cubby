package config

import "testing"

func TestListenAddr(t *testing.T) {
	ok := map[string]string{
		"9000":           ":9000",
		":9000":          ":9000",
		"127.0.0.1:9000": "127.0.0.1:9000",
		"[::1]:9000":     "[::1]:9000",
		"localhost:1":    "localhost:1",
	}
	for in, want := range ok {
		if got, err := ListenAddr(in); err != nil || got != want {
			t.Errorf("ListenAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "65536", "x", ":x", "host", "host:", "::1", "09000"} {
		if got, err := ListenAddr(in); err == nil {
			t.Errorf("ListenAddr(%q) = %q, want an error", in, got)
		}
	}
}

func TestServerURL(t *testing.T) {
	cases := map[string]string{
		"9000":                    "http://localhost:9000",
		":9000":                   "http://localhost:9000",
		"kv.example.com":          "http://kv.example.com",
		"https://kv.example.com/": "https://kv.example.com",
		"10.0.0.5:28229":          "http://10.0.0.5:28229",
	}
	for in, want := range cases {
		if got := ServerURL(in); got != want {
			t.Errorf("ServerURL(%q) = %q, want %q", in, got, want)
		}
	}
}
