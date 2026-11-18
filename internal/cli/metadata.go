package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"
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
		q.Set("ttl", f.str("ttl"))
	}
	return q, nil
}

func (c *client) limits(f *flags) error {
	if len(f.args) > 0 {
		return usageCli("limits takes no arguments. cubby info <space> shows a space's limits and usage")
	}
	var out map[string]any
	if err := c.call("GET", "/v1/limits", nil, 200, &out); err != nil {
		return err
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
