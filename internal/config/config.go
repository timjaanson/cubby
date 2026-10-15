// Package config loads the server's TOML config, fills in defaults, and
// finds the data directory.
package config

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/timjaanson/cubby/internal/names"
)

// Size is a byte count written like "64KB" in config.
type Size int64

func (s *Size) UnmarshalText(b []byte) error {
	n, err := parseSize(string(b))
	*s = Size(n)
	return err
}

// Duration is written like "30d", "12h" or "90s" in config.
type Duration time.Duration

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := ParseDuration(string(b))
	*d = Duration(v)
	return err
}

func parseSize(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(s, u.suf) {
			s, mult = strings.TrimSpace(strings.TrimSuffix(s, u.suf)), u.m
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return n * mult, nil
}

func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if n, ok := strings.CutSuffix(s, "d"); ok {
		v, err := strconv.Atoi(n)
		if err != nil {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		return time.Duration(v) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

type PinnedSpace struct {
	MaxBytes        Size `toml:"max_bytes"`
	MaxValueSize    Size `toml:"max_value_size"`
	MaxKeys         int  `toml:"max_keys"`
	MaxSeriesPoints int  `toml:"max_series_points"`
}

type Config struct {
	Server struct {
		Listen         string   `toml:"listen"`
		DataDir        string   `toml:"data_dir"`
		TrustedProxies []string `toml:"trusted_proxies"`
		RateLimits     *bool    `toml:"rate_limits"`
	} `toml:"server"`
	Retention struct {
		VisitorIdleTTL      Duration `toml:"visitor_idle_ttl"`
		EmptyVisitorIdleTTL Duration `toml:"empty_visitor_idle_ttl"`
		SpaceIdleTTL        Duration `toml:"space_idle_ttl"`
		TombstoneTTL        Duration `toml:"tombstone_ttl"`
	} `toml:"retention"`
	Limits struct {
		MaxValueSize    Size `toml:"max_value_size"`
		MaxPointSize    Size `toml:"max_point_size"`
		MaxSeriesPoints int  `toml:"max_series_points"`
		MaxVisitorBytes Size `toml:"max_visitor_bytes"`
		MaxSpaceBytes   Size `toml:"max_space_bytes"`
		MaxSpaceKeys    int  `toml:"max_space_keys"`
		MaxTotalBytes   Size `toml:"max_total_bytes"`
		MaxSpaces       int  `toml:"max_spaces"`
	} `toml:"limits"`
	Spaces map[string]PinnedSpace `toml:"spaces"`

	Trusted []netip.Prefix `toml:"-"` // parsed from Server.TrustedProxies
}

func Default() *Config {
	c := &Config{}
	c.Server.Listen = DefaultListen
	c.Server.TrustedProxies = []string{"127.0.0.1/32", "::1/128"}
	c.Retention.VisitorIdleTTL = Duration(365 * 24 * time.Hour)
	c.Retention.EmptyVisitorIdleTTL = Duration(30 * 24 * time.Hour)
	c.Retention.SpaceIdleTTL = Duration(180 * 24 * time.Hour)
	c.Retention.TombstoneTTL = Duration(7 * 24 * time.Hour)
	c.Limits.MaxValueSize = 64 << 10
	c.Limits.MaxPointSize = 1 << 10
	c.Limits.MaxSeriesPoints = 50000
	c.Limits.MaxVisitorBytes = 1 << 20
	c.Limits.MaxSpaceBytes = 5 << 20
	c.Limits.MaxSpaceKeys = 10000
	c.Limits.MaxTotalBytes = 1 << 30
	c.Limits.MaxSpaces = 1000
	return c
}

// Load reads an optional TOML file over the defaults. Missing values keep
// their defaults. Flags and environment variables are applied by the caller,
// on top of what this returns.
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		md, err := toml.DecodeFile(path, c)
		if err != nil {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
		if un := md.Undecoded(); len(un) > 0 {
			return nil, fmt.Errorf("config %s: unknown setting %s", path, un[0])
		}
	}
	if l, err := ListenAddr(c.Server.Listen); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	} else {
		c.Server.Listen = l
	}
	for _, p := range c.Server.TrustedProxies {
		if !strings.Contains(p, "/") {
			if strings.Contains(p, ":") {
				p += "/128"
			} else {
				p += "/32"
			}
		}
		pfx, err := netip.ParsePrefix(p)
		if err != nil {
			return nil, fmt.Errorf("trusted_proxies: %w", err)
		}
		c.Trusted = append(c.Trusted, pfx)
	}
	if c.Limits.MaxPointSize < 1 || c.Limits.MaxSeriesPoints < 1 {
		return nil, fmt.Errorf("config %s: max_point_size and max_series_points must be positive", path)
	}
	for name := range c.Spaces {
		if !names.ValidSpace(name) {
			return nil, fmt.Errorf("config: pinned space %q is not a valid name", name)
		}
	}
	return c, nil
}

func (c *Config) RateLimitsOn() bool {
	return c.Server.RateLimits == nil || *c.Server.RateLimits
}

// limitsFor returns the value size, byte and key limits for a space.
func (c *Config) LimitsFor(space string) (maxValue, maxBytes int64, maxKeys int) {
	maxValue, maxBytes, maxKeys = int64(c.Limits.MaxValueSize), int64(c.Limits.MaxSpaceBytes), c.Limits.MaxSpaceKeys
	if p, ok := c.Spaces[space]; ok {
		if p.MaxValueSize > 0 {
			maxValue = min(int64(p.MaxValueSize), 1<<20)
		}
		if p.MaxBytes > 0 {
			maxBytes = int64(p.MaxBytes)
		}
		if p.MaxKeys > 0 {
			maxKeys = p.MaxKeys
		}
	}
	return
}

// seriesLimitsFor returns the point size and points-per-series limits.
func (c *Config) SeriesLimitsFor(space string) (maxPoint int64, maxPoints int) {
	maxPoint, maxPoints = int64(c.Limits.MaxPointSize), c.Limits.MaxSeriesPoints
	if p, ok := c.Spaces[space]; ok && p.MaxSeriesPoints > 0 {
		maxPoints = p.MaxSeriesPoints
	}
	return
}

func (c *Config) Pinned(space string) bool {
	_, ok := c.Spaces[space]
	return ok
}
