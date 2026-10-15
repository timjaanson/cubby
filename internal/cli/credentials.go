package cli

import (
	"bytes"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

type cliConfig struct {
	Server string `toml:"server"`
}

type serverCreds struct {
	Token     string            `toml:"token,omitempty"`
	ID        string            `toml:"id,omitempty"`
	SpaceKeys map[string]string `toml:"space_keys,omitempty"`
}

type credentials struct {
	Servers map[string]*serverCreds `toml:"servers"`
}

func configDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "cubby")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "cubby")
}

func loadCreds() *credentials {
	c := &credentials{Servers: map[string]*serverCreds{}}
	toml.DecodeFile(filepath.Join(configDir(), "credentials.toml"), c)
	if c.Servers == nil {
		c.Servers = map[string]*serverCreds{}
	}
	return c
}

func (c *credentials) save() error {
	dir := configDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("# Written by cubby. Tokens and space keys are secrets.\n")
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".credentials.toml.tmp")
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "credentials.toml"))
}
