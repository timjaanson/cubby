package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/timjaanson/cubby/internal/config"
	"github.com/timjaanson/cubby/internal/names"
)

func (c *client) add(f *flags) error {
	if len(f.args) < 1 {
		return usageCli("add takes a path, then a value or name=value pairs")
	}
	p, err := c.parsePath(f.args[0])
	if err != nil {
		return err
	}
	if strings.HasPrefix(p.area, "users/*") {
		return usageCli("add writes your own data. Use users/me/ instead of users/*/")
	}
	u := p.url() + "/series"
	if f.has("time") {
		n, err := strconv.ParseInt(f.str("time"), 10, 64)
		if err != nil || n < 1 {
			return usageCli("--time takes a time in milliseconds since 1970")
		}
		u += "?t=" + strconv.FormatInt(n, 10)
	}
	var body []byte
	switch {
	case p.isPattern() && len(f.args) > 1:
		prefix := strings.TrimSuffix(p.name, "*")
		var b bytes.Buffer
		b.WriteByte('{')
		for i, kv := range f.args[1:] {
			name, val, ok := strings.Cut(kv, "=")
			if !ok || name == "" {
				return usageCli(fmt.Sprintf("%q isn't name=value", kv))
			}
			if !names.ValidKey(prefix + name) {
				return &cliError{code: 5, err: "invalid_key", msg: fmt.Sprintf("%q isn't a valid key", prefix+name)}
			}
			if i > 0 {
				b.WriteByte(',')
			}
			nb, _ := json.Marshal(name)
			b.Write(nb)
			b.WriteByte(':')
			if json.Valid([]byte(val)) {
				b.WriteString(val)
			} else {
				vb, _ := json.Marshal(val)
				b.Write(vb)
			}
		}
		b.WriteByte('}')
		body = b.Bytes()
	case len(f.args) == 2 && f.args[1] != "-":
		body = []byte(f.args[1])
	case len(f.args) <= 2:
		if body, err = io.ReadAll(os.Stdin); err != nil {
			return err
		}
		if p.isPattern() {
			body = bytes.TrimSpace(body)
		}
	default:
		return usageCli("add takes one value for a key. Use a pattern like 'space/shared/env.*' with name=value pairs for several")
	}
	if body == nil {
		body = []byte{}
	}
	resp, b, err := c.do("POST", u, body, reqOpts{needToken: true})
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return apiErrFrom(resp, b)
	}
	var out struct{ T int64 }
	json.Unmarshal(b, &out)
	fmt.Println(out.T)
	return nil
}

// parseSince takes milliseconds, or a duration back from now like 90m, 24h or 7d.
func parseSince(s string, now time.Time) (int64, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n >= 0 {
		return n, nil
	}
	d, err := config.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, usageCli("--since takes a time in milliseconds, or a duration like 90m, 24h or 7d")
	}
	return now.Add(-d).UnixMilli(), nil
}

func (c *client) series(f *flags) error {
	p, err := c.onePath(f, "series")
	if err != nil {
		return err
	}
	if p.isPattern() || p.area == "users/*" {
		return usageCli("series reads one key")
	}
	limit := 0
	if f.has("limit") {
		if limit, err = strconv.Atoi(f.str("limit")); err != nil || limit < 1 {
			return usageCli("--limit takes a positive number")
		}
	}
	var since *int64
	if f.has("since") {
		n, err := parseSince(f.str("since"), time.Now())
		if err != nil {
			return err
		}
		since = &n
	} else if limit > 1000 {
		return usageCli("without --since, series prints at most the newest 1000 points. Use --since for more")
	}
	printed := 0
	for {
		q := url.Values{}
		page := 1000
		if limit > 0 {
			page = min(1000, limit-printed)
		}
		q.Set("limit", strconv.Itoa(page))
		if since != nil {
			q.Set("since", strconv.FormatInt(*since, 10))
		}
		resp, b, err := c.do("GET", p.url()+"/series?"+q.Encode(), nil, reqOpts{needToken: p.area == "me"})
		if err != nil {
			return err
		}
		if resp.StatusCode != 200 {
			return apiErrFrom(resp, b)
		}
		var out struct {
			Points []struct {
				T     int64           `json:"t"`
				Value json.RawMessage `json:"value"`
			}
			Next *int64
		}
		if err := json.Unmarshal(b, &out); err != nil {
			return &cliError{code: 6, err: "bad_response", msg: err.Error()}
		}
		for _, pt := range out.Points {
			if f.bool("json") {
				line, _ := json.Marshal(pt)
				fmt.Println(string(line))
			} else {
				fmt.Printf("%d\t%s\n", pt.T, compactJSON(pt.Value))
			}
			printed++
		}
		if out.Next == nil || (limit > 0 && printed >= limit) {
			return nil
		}
		since = out.Next
	}
}
