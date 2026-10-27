package server

import (
	"fmt"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/timjaanson/cubby/internal/apierr"
)

func (s *Server) serverLimits() map[string]any {
	out := map[string]any{"rate_limits": s.cfg.RateLimitsOn()}
	v := reflect.ValueOf(s.cfg.Limits)
	typ := v.Type()
	for i := 0; i < v.NumField(); i++ {
		out[typ.Field(i).Tag.Get("toml")] = v.Field(i).Int()
	}
	return out
}

func (s *Server) spaceLimits(space string) map[string]any {
	value, bytes, keys := s.cfg.LimitsFor(space)
	point, points := s.cfg.SeriesLimitsFor(space)
	c := s.cfg.Limits
	return map[string]any{"valueBytes": value, "pointBytes": point, "seriesPoints": points, "spaceBytes": bytes, "spaceKeys": keys, "visitorBytes": c.MaxVisitorBytes, "writesPerMinute": c.WritesPerMinute, "writeBurst": c.WriteBurst, "rateLimits": s.cfg.RateLimitsOn(), "ttlMinSeconds": c.TTLMinSeconds, "ttlMaxSeconds": c.TTLMaxSeconds, "pulseBytes": int64(c.MaxPulseSize), "pulsesPerSecond": c.PulsesPerSecond, "pulseBurst": c.PulseBurst}
}

// Each header member describes a token bucket. r is an advisory snapshot.
// Omit the optional effective-window t: a refill time is not a fixed window.
func (s *Server) writeBudget(c *reqCtx) {
	sw := c.w.(*statusWriter)
	sw.before = func() {
		if !s.cfg.RateLimitsOn() {
			return
		}
		var policy, budget []string
		for _, b := range []struct {
			name, key string
			l         *limiter
		}{{"visitor", c.visitor, s.lim.writesPerVisitor}, {"ip", c.ip, s.lim.writesPerIP}, {"bytes", c.ip, s.lim.writeBytesPerIP}} {
			remaining := b.l.remaining(b.key)
			item := fmt.Sprintf(`"%s";q=%d;w=%d`, b.name, int64(b.l.burst), int64(math.Ceil(b.l.burst/b.l.rate)))
			if b.name == "bytes" {
				item += `;qu="content-bytes"`
			}
			policy = append(policy, item)
			budget = append(budget, fmt.Sprintf(`"%s";r=%d`, b.name, remaining))
		}
		c.header().Set("RateLimit-Policy", strings.Join(policy, ", "))
		c.header().Set("RateLimit", strings.Join(budget, ", "))
	}
}

func (l *limiter) remaining(key string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	tokens := l.burst
	if b := l.buckets[key]; b != nil {
		tokens = math.Min(l.burst, b.tokens+l.now().Sub(b.last).Seconds()*l.rate)
	}
	return int64(math.Floor(tokens))
}

func parseIfBefore(r *http.Request) (*int64, error) {
	if !r.URL.Query().Has("ifBefore") {
		return nil, nil
	}
	n, ok := parseUint(r.URL.Query().Get("ifBefore"))
	if !ok {
		return nil, apierr.BadParam("ifBefore", "ifBefore is a deadline in server milliseconds since 1970.")
	}
	return &n, nil
}

func (s *Server) parseTTL(r *http.Request) (int64, error) {
	if !r.URL.Query().Has("ttl") {
		return 0, nil
	}
	n, ok := parseUint(r.URL.Query().Get("ttl"))
	if !ok || n < int64(s.cfg.Limits.TTLMinSeconds) || n > int64(s.cfg.Limits.TTLMaxSeconds) {
		return 0, apierr.BadParam("ttl", fmt.Sprintf("ttl is %d to %d seconds.", s.cfg.Limits.TTLMinSeconds, s.cfg.Limits.TTLMaxSeconds))
	}
	return n, nil
}

func writeMetadata(c *reqCtx, t, expires int64) {
	c.header().Set("KV-Time", strconv.FormatInt(t, 10))
	if expires > 0 {
		c.header().Set("KV-Expires", strconv.FormatInt(expires, 10))
	}
}

func (s *Server) accessUsage(space, id string) (map[string]any, error) {
	bytes, keys, visitor, err := s.store.Usage(space, id)
	return map[string]any{"bytes": bytes, "keys": keys, "visitorBytes": visitor}, err
}
