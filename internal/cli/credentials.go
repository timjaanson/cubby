package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/timjaanson/cubby/internal/auth"
)

type cliConfig struct {
	Server string `toml:"server"`
}

// deviceCreds is one device key on one server: the credentials file format,
// also what `cubby device backup` prints and `cubby login` reads. Invites and
// API keys are never stored.
type deviceCreds struct {
	Server  string          `json:"server"`
	Visitor string          `json:"visitor,omitempty"`
	Device  string          `json:"device"`
	Key     json.RawMessage `json:"key"` // private JWK
}

// credentials is ~/.config/cubby/credentials.json: one device per server.
type credentials struct {
	Servers map[string]*deviceCreds `json:"servers"`
}

func configDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "cubby")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "cubby")
}

func credsPath() string { return filepath.Join(configDir(), "credentials.json") }

func loadCreds() *credentials {
	c := &credentials{}
	if b, err := os.ReadFile(credsPath()); err == nil {
		json.Unmarshal(b, c)
	}
	if c.Servers == nil {
		c.Servers = map[string]*deviceCreds{}
	}
	return c
}

func (c *credentials) save() error {
	dir := configDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".credentials.json.tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, credsPath())
}

// readDeviceCreds reads one credentials object, from a file, stdin or env.
func readDeviceCreds(b []byte, server string) (*deviceCreds, *auth.Signer, error) {
	var d deviceCreds
	if err := json.Unmarshal(b, &d); err != nil || len(d.Key) == 0 {
		return nil, nil, errors.New(`credentials are JSON: {"server": ..., "device": ..., "key": {private JWK}}`)
	}
	if d.Server != "" && server != "" && d.Server != server {
		return nil, nil, fmt.Errorf("these credentials are for %s, not %s. Use --server %s", d.Server, server, d.Server)
	}
	s, err := auth.SignerFromJWK(d.Key)
	if err != nil {
		return nil, nil, fmt.Errorf("credentials: %w", err)
	}
	d.Device = s.Key.ID
	return &d, s, nil
}
