package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TOML renders c as a config file that Load reads back to the same values.
// Every setting gets a comment, and one that differs from def gets a
// "default:" note. dataDirNote is printed when data_dir is unset, to say
// where data would go instead.
func (c *Config) TOML(def *Config, dataDirNote string) string {
	w := &tomlWriter{}

	w.section("server")
	w.set("listen", quote(c.Server.Listen), quote(def.Server.Listen),
		`Address to bind: ":PORT" for every interface, "127.0.0.1:PORT" for this machine only.`)
	if c.Server.DataDir != "" {
		w.set("data_dir", quote(c.Server.DataDir), "",
			"Where spaces and server.db live. Must be writable, or the server stops.")
	} else {
		w.comment("Where spaces and server.db live. Unset, cubby picks a place itself.")
		w.comment(dataDirNote)
		w.line(`# data_dir = "/var/lib/cubby"`)
	}
	w.set("trusted_proxies", list(c.Server.TrustedProxies), list(def.Server.TrustedProxies),
		"Proxies whose X-Forwarded-For header to believe for the client's IP.")
	w.set("rate_limits", strconv.FormatBool(c.RateLimitsOn()), strconv.FormatBool(def.RateLimitsOn()),
		"Per-IP and per-visitor request limits. Turn off only behind something that limits for you.")

	w.section("retention")
	w.set("space_idle_ttl", dur(c.Retention.SpaceIdleTTL), dur(def.Retention.SpaceIdleTTL),
		"Delete spaces nobody read or wrote for this long. Pinned spaces are kept.")
	w.set("visitor_idle_ttl", dur(c.Retention.VisitorIdleTTL), dur(def.Retention.VisitorIdleTTL),
		"Forget visitors idle this long. Their users/ keys become tombstones.")
	w.set("empty_visitor_idle_ttl", dur(c.Retention.EmptyVisitorIdleTTL), dur(def.Retention.EmptyVisitorIdleTTL),
		"Forget visitors who never stored anything after this long.")
	w.set("tombstone_ttl", dur(c.Retention.TombstoneTTL), dur(def.Retention.TombstoneTTL),
		"Keep records of deleted keys this long, so watchers catching up still see the delete.")

	w.section("limits")
	w.set("max_value_size", size(c.Limits.MaxValueSize), size(def.Limits.MaxValueSize),
		"Largest single value.")
	w.set("max_point_size", size(c.Limits.MaxPointSize), size(def.Limits.MaxPointSize),
		"Largest single point in a series.")
	w.set("max_series_points", strconv.Itoa(c.Limits.MaxSeriesPoints), strconv.Itoa(def.Limits.MaxSeriesPoints),
		"Points kept per series. The oldest are dropped.")
	w.set("max_visitor_bytes", size(c.Limits.MaxVisitorBytes), size(def.Limits.MaxVisitorBytes),
		"What one visitor may store across all spaces.")
	w.set("max_space_bytes", size(c.Limits.MaxSpaceBytes), size(def.Limits.MaxSpaceBytes),
		"Size of one space, series points included.")
	w.set("max_space_keys", strconv.Itoa(c.Limits.MaxSpaceKeys), strconv.Itoa(def.Limits.MaxSpaceKeys),
		"Keys in one space.")
	w.set("max_total_bytes", size(c.Limits.MaxTotalBytes), size(def.Limits.MaxTotalBytes),
		"Everything on this server together.")
	w.set("max_spaces", strconv.Itoa(c.Limits.MaxSpaces), strconv.Itoa(def.Limits.MaxSpaces),
		"Spaces on this server.")
	w.set("max_pulse_size", size(c.Limits.MaxPulseSize), size(def.Limits.MaxPulseSize),
		"One pulse. Pulses aren't stored, so they don't count towards storage.")

	w.set("writes_per_minute", strconv.Itoa(c.Limits.WritesPerMinute), strconv.Itoa(def.Limits.WritesPerMinute), "Writes per visitor per minute, across spaces.")
	w.set("write_burst", strconv.Itoa(c.Limits.WriteBurst), strconv.Itoa(def.Limits.WriteBurst), "Visitor write bucket capacity.")
	w.set("writes_per_ip_per_minute", strconv.Itoa(c.Limits.WritesPerIPPerMinute), strconv.Itoa(def.Limits.WritesPerIPPerMinute), "Writes per IP per minute. Bucket capacity equals this value.")
	w.set("reads_per_ip_per_minute", strconv.Itoa(c.Limits.ReadsPerIPPerMinute), strconv.Itoa(def.Limits.ReadsPerIPPerMinute), "Reads per IP per minute, including continuing watches.")
	w.set("write_bytes_per_ip_per_day", strconv.Itoa(c.Limits.WriteBytesPerIPPerDay), strconv.Itoa(def.Limits.WriteBytesPerIPPerDay), "Write bytes per IP per day. Bucket capacity equals this value.")
	w.set("visitors_per_ip_per_hour", strconv.Itoa(c.Limits.VisitorsPerIPPerHour), strconv.Itoa(def.Limits.VisitorsPerIPPerHour), "New visitors per IP per hour.")
	w.set("spaces_per_visitor_per_day", strconv.Itoa(c.Limits.SpacesPerVisitorPerDay), strconv.Itoa(def.Limits.SpacesPerVisitorPerDay), "New spaces per visitor per day.")
	w.set("spaces_per_ip_per_day", strconv.Itoa(c.Limits.SpacesPerIPPerDay), strconv.Itoa(def.Limits.SpacesPerIPPerDay), "New spaces per IP per day.")
	w.set("misses_per_ip_per_hour", strconv.Itoa(c.Limits.MissesPerIPPerHour), strconv.Itoa(def.Limits.MissesPerIPPerHour), "Distinct unavailable spaces per IP per hour.")
	w.set("watches_per_ip", strconv.Itoa(c.Limits.WatchesPerIP), strconv.Itoa(def.Limits.WatchesPerIP), "Maximum concurrent watches per IP, even when rate_limits is false.")
	w.set("watches_total", strconv.Itoa(c.Limits.WatchesTotal), strconv.Itoa(def.Limits.WatchesTotal), "Maximum concurrent watches on this server.")
	w.set("ttl_min_seconds", strconv.Itoa(c.Limits.TTLMinSeconds), strconv.Itoa(def.Limits.TTLMinSeconds), "Minimum key TTL in whole seconds.")
	w.set("pulses_per_second", strconv.Itoa(c.Limits.PulsesPerSecond), strconv.Itoa(def.Limits.PulsesPerSecond), "Pulses per visitor per second, across spaces.")
	w.set("pulse_burst", strconv.Itoa(c.Limits.PulseBurst), strconv.Itoa(def.Limits.PulseBurst), "Visitor pulse bucket capacity.")
	w.set("pulses_per_ip_per_second", strconv.Itoa(c.Limits.PulsesPerIPPerSecond), strconv.Itoa(def.Limits.PulsesPerIPPerSecond), "Pulses per IP per second. Bucket capacity is twice this.")
	w.set("ttl_max_seconds", strconv.Itoa(c.Limits.TTLMaxSeconds), strconv.Itoa(def.Limits.TTLMaxSeconds), "Maximum key TTL in whole seconds, at most 10 years.")

	w.blank()
	w.comment("Pinned spaces are never cleaned up and can have their own limits.")
	w.comment("Leave a setting out to use the one from [limits].")
	if len(c.Spaces) == 0 {
		w.line("# [spaces.status-cedar-pilot-mango]")
		w.line(`# max_bytes = "100MB"`)
		w.line(`# max_value_size = "256KB"     # capped at 1MB`)
		w.line("# max_keys = 50000")
		w.line("# max_series_points = 500000")
	}
	pinned := make([]string, 0, len(c.Spaces))
	for name := range c.Spaces {
		pinned = append(pinned, name)
	}
	sort.Strings(pinned)
	for _, name := range pinned {
		p := c.Spaces[name]
		w.line("[spaces." + name + "]")
		if p.MaxBytes > 0 {
			w.line("max_bytes = " + size(p.MaxBytes))
		}
		if p.MaxValueSize > 0 {
			w.line("max_value_size = " + size(p.MaxValueSize))
		}
		if p.MaxKeys > 0 {
			w.line("max_keys = " + strconv.Itoa(p.MaxKeys))
		}
		if p.MaxSeriesPoints > 0 {
			w.line("max_series_points = " + strconv.Itoa(p.MaxSeriesPoints))
		}
	}
	return w.b.String()
}

type tomlWriter struct{ b strings.Builder }

func (w *tomlWriter) line(s string)    { w.b.WriteString(s + "\n") }
func (w *tomlWriter) comment(s string) { w.line("# " + s) }
func (w *tomlWriter) blank()           { w.line("") }

func (w *tomlWriter) section(name string) {
	if w.b.Len() > 0 {
		w.blank()
	}
	w.line("[" + name + "]")
}

func (w *tomlWriter) set(key, val, def, doc string) {
	w.comment(doc)
	if def != "" && val != def {
		w.line(fmt.Sprintf("%s = %s   # default: %s", key, val, def))
		return
	}
	w.line(key + " = " + val)
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func list(v []string) string {
	q := make([]string, len(v))
	for i, s := range v {
		q[i] = quote(s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// size writes n in the largest unit that keeps it whole, as "64KB".
func size(n Size) string {
	for _, u := range []struct {
		suf string
		m   Size
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}} {
		if n >= u.m && n%u.m == 0 {
			return quote(strconv.FormatInt(int64(n/u.m), 10) + u.suf)
		}
	}
	return quote(strconv.FormatInt(int64(n), 10) + "B")
}

// dur writes d in the largest unit that keeps it whole, as "180d" or "90s".
func dur(d Duration) string {
	t := time.Duration(d)
	for _, u := range []struct {
		suf string
		m   time.Duration
	}{{"d", 24 * time.Hour}, {"h", time.Hour}, {"m", time.Minute}, {"s", time.Second}} {
		if t >= u.m && t%u.m == 0 {
			return quote(strconv.FormatInt(int64(t/u.m), 10) + u.suf)
		}
	}
	return quote(t.String())
}
