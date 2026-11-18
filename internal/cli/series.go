package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/timjaanson/cubby/internal/access"
)

func (c *client) add(f *flags) error {
	if len(f.args) < 1 || len(f.args) > 2 {
		return usageCli("add takes a path and a value (or - or nothing for stdin)")
	}
	p, err := c.parsePath(f.args[0])
	if err != nil {
		return err
	}
	if p.isPattern() || strings.HasSuffix(p.area, "/*") {
		return usageCli("add writes one series. For several at once use cubby batch")
	}
	q, err := writeFlags(f)
	if err != nil {
		return err
	}
	if f.has("t") {
		n, err := parseTime("t", f.str("t"), time.Now())
		if err != nil {
			return err
		}
		q.Set("t", strconv.FormatInt(n, 10))
	}
	u := p.url() + "/series"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var body []byte
	if len(f.args) == 2 && f.args[1] != "-" {
		body = []byte(f.args[1])
	} else if body, err = io.ReadAll(os.Stdin); err != nil {
		return err
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

// parseTime takes milliseconds since 1970, or a duration meaning that long
// ago (90m, 24h, 7d), for --since, --before and --t.
func parseTime(flag, s string, now time.Time) (int64, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n >= 0 {
		return n, nil
	}
	d, err := access.ParseDuration(s)
	if err != nil {
		return 0, usageCli("--" + flag + " takes a time in milliseconds since 1970, or a duration ago like 90m, 24h or 7d")
	}
	return now.Add(-d).UnixMilli(), nil
}

// seriesPage is the most points one series request returns.
const seriesPage = 10000

func (c *client) series(f *flags) error {
	p, err := c.onePath(f, "series")
	if err != nil {
		return err
	}
	if strings.HasSuffix(p.area, "/*") {
		return usageCli("series reads one area: name the author, sender or recipient")
	}
	limit, err := limitFlag(f)
	if err != nil {
		return err
	}
	q := url.Values{}
	now := time.Now()
	for _, name := range []string{"since", "before"} {
		if f.has(name) {
			n, err := parseTime(name, f.str(name), now)
			if err != nil {
				return err
			}
			q.Set(name, strconv.FormatInt(n, 10))
		}
	}
	if err := orderFlag(f, q); err != nil {
		return err
	}
	if f.has("after") {
		q.Set("after", f.str("after"))
	}
	printed := 0
	for {
		q.Set("limit", strconv.Itoa(seriesPage))
		if limit > 0 {
			q.Set("limit", strconv.Itoa(min(seriesPage, limit-printed)))
		}
		resp, b, err := c.do("GET", p.url()+"/series?"+q.Encode(), nil, reqOpts{needToken: p.needsToken()})
		if err != nil {
			return err
		}
		if resp.StatusCode != 200 {
			return apiErrFrom(resp, b)
		}
		var out struct {
			Items []struct {
				Key     string          `json:"key,omitempty"`
				T       int64           `json:"t"`
				Value   json.RawMessage `json:"value"`
				Expires int64           `json:"expires,omitempty"`
			}
			Next json.RawMessage // a t for one series, "t:key" for several
		}
		if err := json.Unmarshal(b, &out); err != nil {
			return &cliError{code: 6, err: "bad_response", msg: err.Error()}
		}
		for _, pt := range out.Items {
			if f.bool("json") {
				line, _ := json.Marshal(pt)
				fmt.Println(string(line))
			} else if pt.Key != "" {
				fmt.Printf("%s\t%d\t%s\n", pt.Key, pt.T, compactJSON(pt.Value))
			} else {
				fmt.Printf("%d\t%s\n", pt.T, compactJSON(pt.Value))
			}
			printed++
		}
		if len(out.Next) == 0 || string(out.Next) == "null" || (limit > 0 && printed >= limit) {
			return nil
		}
		next := strings.Trim(string(out.Next), `"`)
		q.Set("after", next)
	}
}

// limitFlag reads --limit: how many items to print in all, 0 for no limit.
func limitFlag(f *flags) (int, error) {
	if !f.has("limit") {
		return 0, nil
	}
	n, err := strconv.Atoi(f.str("limit"))
	if err != nil || n < 1 {
		return 0, usageCli("--limit takes a positive number")
	}
	return n, nil
}

// orderFlag reads --order asc|desc into q.
func orderFlag(f *flags, q url.Values) error {
	if !f.has("order") {
		return nil
	}
	switch o := f.str("order"); o {
	case "asc", "desc":
		q.Set("order", o)
		return nil
	}
	return usageCli("--order is asc or desc")
}
