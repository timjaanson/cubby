package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/timjaanson/cubby/internal/config"
	"github.com/timjaanson/cubby/internal/names"
)

var clientValueFlags = []string{"server", "token", "space-key"}

var commandFlags = map[string]struct {
	vals, bools []string
	help        string
}{
	"get":    {nil, []string{"json"}, "get <path> [--json]\n  Print the raw value. Exit 1 if it doesn't exist."},
	"set":    {[]string{"if-version"}, []string{"create"}, "set <path> [value|-] [--if-version N] [--create]\n  Write from the argument, or from stdin when it's missing or -. Prints the new version."},
	"del":    {[]string{"if-version"}, []string{"f", "force"}, "del <path> [--if-version N] [-f]\n  Delete. With -f a missing key isn't an error."},
	"ls":     {[]string{"limit"}, []string{"json", "desc"}, "ls <path or pattern> [--limit N] [--desc] [--json]\n  One line per item: key<TAB>value, or user<TAB>key<TAB>value under users/*/."},
	"watch":  {[]string{"from", "state", "exec"}, []string{"json", "once", "raw"}, "watch <path or pattern> [--from N|now] [--state FILE] [--exec CMD] [--once] [--raw] [--json]\n  Print current state, then changes. --exec runs CMD per change with the value on stdin\n  and KV_KEY, KV_VERSION, KV_DELETED, KV_USER set. --state FILE resumes after restarts."},
	"add":    {[]string{"time"}, nil, "add <path> [value|-] [--time MS]\n  Add a point to a series, from the argument or stdin. Prints the point's time.\nadd <prefix*> name=value ... [--time MS]\n  Add one point to each series prefix+name, all at one time. A value is JSON if it parses,\n  otherwise a string. Without name=value arguments, reads one JSON object from stdin."},
	"series": {[]string{"since", "limit"}, []string{"json"}, "series <path> [--since MS|24h|90m|7d] [--limit N] [--json]\n  Print a series' points oldest first, as t<TAB>value. Without --since, the newest 1000."},
	"update": {[]string{"exec"}, nil, "update <path> [--exec CMD]\n  Read, pipe through CMD (or open $EDITOR), write back with ifVersion, retry on conflict.\n  Example: cubby update space/shared/count --exec 'jq \"(. // 0) + 1\"'"},
	"new":    {[]string{"label"}, []string{"locked", "no-save"}, "new [--label L] [--locked] [--no-save]\n  Create a space. Locked spaces print the key on a second line and save it."},
	"name":   {nil, nil, "name [label]\n  Print a new valid space name. No server needed."},
	"whoami": {nil, nil, "whoami\n  Print your visitor ID."},
	"token":  {nil, nil, "token\n  Print your token, for curl: curl -H \"Authorization: Bearer $(cubby token)\" ..."},
	"url":    {nil, nil, "url <path>\n  Print the full URL for a path."},
}

func cmdHelp(cmd string) int {
	switch cmd {
	case "serve":
		fmt.Printf("serve [--listen :8080] [--data DIR] [--config FILE] [--quiet]\n  Run the server. Env: CUBBY_LISTEN, CUBBY_DATA, CUBBY_CONFIG.\n  Data goes in %s, created if missing.\n", config.PreviewDataDir())
		return 0
	case "admin":
		fmt.Print(adminUsage)
		return 0
	}
	d, ok := commandFlags[cmd]
	if !ok {
		return usageErr(fmt.Errorf("unknown command %q", cmd))
	}
	fmt.Println(d.help)
	fmt.Println("\nCommon flags: --server URL (CUBBY_SERVER), --token T (CUBBY_TOKEN), --space-key K (CUBBY_SPACE_KEY)")
	return 0
}

func runClient(cmd string, args []string) int {
	d, ok := commandFlags[cmd]
	if !ok {
		return usageErr(fmt.Errorf("unknown command %q", cmd))
	}
	f, err := parseFlags(args, append(d.vals, clientValueFlags...), d.bools)
	if err != nil {
		return usageErr(err)
	}
	c, err := newClient(f)
	if err != nil {
		var ce *cliError
		errors.As(err, &ce)
		return usageErr(errors.New(ce.msg))
	}
	switch cmd {
	case "name":
		if len(f.args) > 1 {
			return usageErr(errors.New("name takes at most one label"))
		}
		label := ""
		if len(f.args) == 1 {
			label = f.args[0]
		}
		fmt.Println(names.NewSpaceName(label))
		return 0
	}
	err = c.run(cmd, f)
	if err == nil {
		return 0
	}
	var ce *cliError
	if errors.As(err, &ce) {
		if ce.code == 64 {
			return usageErr(errors.New(ce.msg))
		}
		fmt.Fprintf(os.Stderr, "cubby: %s: %s\n", ce.err, ce.msg)
		return ce.code
	}
	fmt.Fprintln(os.Stderr, "cubby:", err)
	return 6
}

func usageCli(msg string) error { return &cliError{code: 64, msg: msg} }

func (c *client) onePath(f *flags, cmd string) (kvPath, error) {
	if len(f.args) != 1 {
		return kvPath{}, usageCli(cmd + " takes one path")
	}
	return c.parsePath(f.args[0])
}

func (c *client) run(cmd string, f *flags) error {
	switch cmd {
	case "get":
		return c.get(f)
	case "set":
		return c.set(f)
	case "del":
		return c.del(f)
	case "ls":
		return c.ls(f)
	case "watch":
		return c.watch(f)
	case "update":
		return c.update(f)
	case "add":
		return c.add(f)
	case "series":
		return c.series(f)
	case "new":
		return c.newSpace(f)
	case "whoami":
		id, err := c.visitorID()
		if err == nil {
			fmt.Println(id)
		}
		return err
	case "token":
		t, err := c.ensureToken()
		if err == nil {
			fmt.Println(t)
		}
		return err
	case "url":
		p, err := c.onePath(f, "url")
		if err == nil {
			fmt.Println(c.server + p.url())
		}
		return err
	}
	return usageCli("unknown command " + cmd)
}

func parseETag(resp *http.Response) int64 {
	n, _ := strconv.ParseInt(strings.Trim(resp.Header.Get("ETag"), `"`), 10, 64)
	return n
}

func isJSONResp(resp *http.Response) bool {
	return strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json")
}

func (c *client) fetch(p kvPath) (value []byte, isJSON bool, ver int64, err error) {
	value, isJSON, ver, _, err = c.fetchT(p)
	return
}

// fetchT also returns the newest point's time for a series key, else 0.
func (c *client) fetchT(p kvPath) (value []byte, isJSON bool, ver, t int64, err error) {
	o := reqOpts{needToken: p.area == "me"}
	resp, b, err := c.do("GET", p.url(), nil, o)
	if err != nil {
		return nil, false, 0, 0, err
	}
	if resp.StatusCode != 200 {
		return nil, false, 0, 0, apiErrFrom(resp, b)
	}
	t, _ = strconv.ParseInt(resp.Header.Get("KV-Time"), 10, 64)
	return b, isJSONResp(resp), parseETag(resp), t, nil
}

func (c *client) get(f *flags) error {
	p, err := c.onePath(f, "get")
	if err != nil {
		return err
	}
	if p.isPattern() || p.area == "users/*" {
		return usageCli("get reads one key. Use ls for patterns and users/*/")
	}
	v, isJSON, ver, t, err := c.fetchT(p)
	if err != nil {
		return err
	}
	if f.bool("json") {
		val := json.RawMessage(v)
		if !isJSON {
			val, _ = json.Marshal(string(v))
		}
		m := map[string]any{"key": p.name, "value": val, "version": ver}
		if t != 0 {
			m["t"] = t
		}
		out, _ := json.Marshal(m)
		fmt.Println(string(out))
		return nil
	}
	os.Stdout.Write(v)
	return nil
}

func (c *client) put(p kvPath, value []byte, ifVersion *int64) (int64, *http.Response, error) {
	u := p.url()
	if ifVersion != nil {
		u += "?ifVersion=" + strconv.FormatInt(*ifVersion, 10)
	}
	method := "PUT"
	if value == nil {
		method = "DELETE"
	}
	resp, b, err := c.do(method, u, value, reqOpts{needToken: true, spaceKey: c.spaceKeyFor(p)})
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode >= 300 {
		return 0, resp, apiErrFrom(resp, b)
	}
	var out struct{ Version int64 }
	json.Unmarshal(b, &out)
	return out.Version, resp, nil
}

func ifVersionFlag(f *flags) (*int64, error) {
	if f.bool("create") {
		z := int64(0)
		return &z, nil
	}
	if !f.has("if-version") {
		return nil, nil
	}
	n, err := strconv.ParseInt(f.str("if-version"), 10, 64)
	if err != nil || n < 0 {
		return nil, usageCli("--if-version takes a version number")
	}
	return &n, nil
}

func (c *client) set(f *flags) error {
	if len(f.args) < 1 || len(f.args) > 2 {
		return usageCli("set takes a path and an optional value")
	}
	p, err := c.parsePath(f.args[0])
	if err != nil {
		return err
	}
	var value []byte
	if len(f.args) == 2 && f.args[1] != "-" {
		value = []byte(f.args[1])
	} else {
		if value, err = io.ReadAll(os.Stdin); err != nil {
			return err
		}
	}
	ifv, err := ifVersionFlag(f)
	if err != nil {
		return err
	}
	if value == nil {
		value = []byte{}
	}
	v, _, err := c.put(p, value, ifv)
	if err != nil {
		return err
	}
	fmt.Println(v)
	return nil
}

func (c *client) del(f *flags) error {
	p, err := c.onePath(f, "del")
	if err != nil {
		return err
	}
	ifv, err := ifVersionFlag(f)
	if err != nil {
		return err
	}
	v, _, err := c.put(p, nil, ifv)
	var ce *cliError
	if errors.As(err, &ce) && ce.err == "not_found" && (f.bool("f") || f.bool("force")) {
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Println(v)
	return nil
}

type item struct {
	Key     string          `json:"key"`
	Value   json.RawMessage `json:"value,omitempty"`
	Deleted bool            `json:"deleted,omitempty"`
	Version int64           `json:"version"`
	User    string          `json:"user,omitempty"`
	T       int64           `json:"t,omitempty"`
}

func compactJSON(v json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, v) != nil {
		return string(v)
	}
	return b.String()
}

func printItem(it item, asJSON bool) {
	if asJSON {
		b, _ := json.Marshal(it)
		fmt.Println(string(b))
		return
	}
	val := "-"
	if !it.Deleted {
		val = compactJSON(it.Value)
	}
	if it.User != "" {
		fmt.Printf("%s\t%s\t%s\n", it.User, it.Key, val)
	} else {
		fmt.Printf("%s\t%s\n", it.Key, val)
	}
}

func (c *client) ls(f *flags) error {
	p, err := c.onePath(f, "ls")
	if err != nil {
		return err
	}
	limit := 0
	if f.has("limit") {
		if limit, err = strconv.Atoi(f.str("limit")); err != nil || limit < 1 {
			return usageCli("--limit takes a positive number")
		}
	}
	after, printed := "", 0
	for {
		q := url.Values{}
		page := 1000
		if limit > 0 {
			page = min(1000, limit-printed)
		}
		q.Set("limit", strconv.Itoa(page))
		if f.bool("desc") {
			q.Set("order", "desc")
		}
		if after != "" {
			q.Set("after", after)
		}
		resp, b, err := c.do("GET", p.url()+"?"+q.Encode(), nil, reqOpts{needToken: p.area == "me"})
		if err != nil {
			return err
		}
		if resp.StatusCode != 200 {
			return apiErrFrom(resp, b)
		}
		var out struct {
			Items []item
			Next  *string
		}
		if err := json.Unmarshal(b, &out); err != nil {
			return &cliError{code: 6, err: "bad_response", msg: err.Error()}
		}
		for _, it := range out.Items {
			printItem(it, f.bool("json"))
			printed++
		}
		if out.Next == nil || (limit > 0 && printed >= limit) {
			return nil
		}
		after = *out.Next
	}
}

func (c *client) newSpace(f *flags) error {
	if len(f.args) != 0 {
		return usageCli("new takes no arguments, use --label")
	}
	q := url.Values{}
	if l := f.str("label"); l != "" {
		if !names.LabelRe.MatchString(l) || len(l) > 54 {
			return &cliError{code: 5, err: "invalid_parameter", msg: "a label is lowercase letters, digits and single hyphens, at most 54 characters"}
		}
		q.Set("label", l)
	}
	u := "/v1/spaces"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	if f.bool("locked") {
		if len(q) > 0 {
			u += "&locked"
		} else {
			u += "?locked"
		}
	}
	resp, b, err := c.do("POST", u, nil, reqOpts{needToken: true})
	if err != nil {
		return err
	}
	if resp.StatusCode != 201 {
		return apiErrFrom(resp, b)
	}
	var out struct{ Space, Key string }
	json.Unmarshal(b, &out)
	fmt.Println(out.Space)
	if out.Key != "" {
		fmt.Println(out.Key)
		if !f.bool("no-save") {
			if c.sc.SpaceKeys == nil {
				c.sc.SpaceKeys = map[string]string{}
			}
			c.sc.SpaceKeys[out.Space] = out.Key
			if err := c.creds.save(); err != nil {
				fmt.Fprintln(os.Stderr, "cubby: warning: could not save the space key:", err)
			}
		}
	}
	return nil
}
