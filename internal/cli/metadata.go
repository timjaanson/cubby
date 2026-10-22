package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/timjaanson/cubby/internal/config"
	"github.com/timjaanson/cubby/internal/names"
)

func writeFlags(f *flags) (url.Values, error) {
	q := url.Values{}
	if f.has("if-before") {
		n, err := strconv.ParseInt(f.str("if-before"), 10, 64)
		if err != nil || n < 0 {
			return nil, usageCli("--if-before takes server milliseconds since 1970")
		}
		q.Set("ifBefore", strconv.FormatInt(n, 10))
	}
	if f.has("ttl") {
		d, err := config.ParseDuration(f.str("ttl"))
		if err != nil || d <= 0 || d%time.Second != 0 {
			return nil, usageCli("--ttl takes a positive whole-second duration, like 30s or 1h")
		}
		q.Set("ttl", strconv.FormatInt(int64(d/time.Second), 10))
	}
	return q, nil
}

func (c *client) limits(f *flags) error {
	if len(f.args) > 1 {
		return usageCli("limits takes at most one space")
	}
	path := "/v1/limits"
	if len(f.args) == 1 {
		if !names.ValidSpace(f.args[0]) {
			return usageCli("limits takes a full space name")
		}
		path = "/v1/" + f.args[0] + "/access"
	}
	var out map[string]any
	if err := c.call("GET", path, nil, 200, &out); err != nil {
		return err
	}
	if len(f.args) > 0 {
		out = map[string]any{"limits": out["limits"], "usage": out["usage"]}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err == nil {
		fmt.Println(string(b))
	}
	return err
}

func (c *client) serverTime(f *flags) error {
	if len(f.args) != 0 {
		return usageCli("time takes no arguments")
	}
	best := time.Duration(1<<63 - 1)
	var offset float64
	for range 5 {
		sent := time.Now()
		var out struct{ T int64 }
		if err := c.call("GET", "/v1/time", nil, 200, &out); err != nil {
			return err
		}
		rtt := time.Since(sent)
		if rtt < best {
			best = rtt
			offset = float64(out.T) - float64(sent.UnixMilli()) - float64(rtt)/float64(time.Millisecond)/2
		}
	}
	fmt.Printf("offset_ms\t%.3f\nrtt_ms\t%.3f\n", offset, float64(best)/float64(time.Millisecond))
	return nil
}
