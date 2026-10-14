package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const defaultServer = "http://localhost:8080"

// ---------- config and credentials ----------

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
		return filepath.Join(x, "tiny-kv")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "tiny-kv")
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
	buf.WriteString("# Written by tiny-kv. Tokens and space keys are secrets.\n")
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".credentials.toml.tmp")
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "credentials.toml"))
}

// ---------- client ----------

type client struct {
	server   string
	creds    *credentials
	sc       *serverCreds
	envToken string // from --token or TINY_KV_TOKEN, never saved
	spaceKey string // from --space-key or TINY_KV_SPACE_KEY
	http     *http.Client
}

func newClient(f *flags) *client {
	var cfg cliConfig
	toml.DecodeFile(filepath.Join(configDir(), "config.toml"), &cfg)
	server := firstNonEmpty(f.str("server"), os.Getenv("TINY_KV_SERVER"), cfg.Server, defaultServer)
	server = strings.TrimRight(server, "/")
	if !strings.Contains(server, "://") {
		server = "http://" + server
	}
	creds := loadCreds()
	sc := creds.Servers[server]
	if sc == nil {
		sc = &serverCreds{}
		creds.Servers[server] = sc
	}
	return &client{
		server:   server,
		creds:    creds,
		sc:       sc,
		envToken: firstNonEmpty(f.str("token"), os.Getenv("TINY_KV_TOKEN")),
		spaceKey: firstNonEmpty(f.str("space-key"), os.Getenv("TINY_KV_SPACE_KEY")),
		http:     &http.Client{Timeout: 70 * time.Second},
	}
}

// cliError carries an exit code and the message for stderr.
type cliError struct {
	code   int
	status int
	err    string
	msg    string
	body   map[string]any
}

func (e *cliError) Error() string { return e.err + ": " + e.msg }

func exitFor(code string, status int) int {
	switch code {
	case "not_found":
		return 1
	case "version_conflict":
		return 2
	case "space_locked", "not_owner", "missing_token", "invalid_token":
		return 3
	case "rate_limited", "space_limit", "too_many_watches", "visitor_quota_exceeded", "space_quota_exceeded", "server_full":
		return 4
	}
	if status >= 400 && status < 500 {
		return 5
	}
	return 6
}

func (c *client) token() string {
	if c.envToken != "" {
		return c.envToken
	}
	return c.sc.Token
}

// ensureToken gets and saves a token if there's none yet.
func (c *client) ensureToken() (string, error) {
	if t := c.token(); t != "" {
		return t, nil
	}
	return c.freshToken()
}

func (c *client) freshToken() (string, error) {
	resp, body, err := c.raw("POST", "/v1/anon", nil, nil)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 201 {
		return "", apiErrFrom(resp, body)
	}
	var out struct{ Token, ID string }
	if err := json.Unmarshal(body, &out); err != nil {
		return "", &cliError{code: 6, err: "bad_response", msg: "the server sent something that isn't a token"}
	}
	c.sc.Token, c.sc.ID = out.Token, out.ID
	if err := c.creds.save(); err != nil {
		fmt.Fprintln(os.Stderr, "tiny-kv: warning: could not save the token:", err)
	}
	return out.Token, nil
}

func apiErrFrom(resp *http.Response, body []byte) *cliError {
	var m map[string]any
	json.Unmarshal(body, &m)
	code, _ := m["error"].(string)
	msg, _ := m["message"].(string)
	if code == "" {
		code, msg = "http_"+strconv.Itoa(resp.StatusCode), strings.TrimSpace(string(body))
	}
	if s, ok := m["suggestion"].(string); ok {
		msg += " Try " + s + "."
	}
	return &cliError{code: exitFor(code, resp.StatusCode), status: resp.StatusCode, err: code, msg: msg, body: m}
}

func (c *client) raw(method, path string, body []byte, hdr http.Header) (*http.Response, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.server+path, rd)
	if err != nil {
		return nil, nil, &cliError{code: 5, err: "bad_path", msg: err.Error()}
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", "tiny-kv/"+version)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, &cliError{code: 6, err: "network_error", msg: err.Error()}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, &cliError{code: 6, err: "network_error", msg: err.Error()}
	}
	return resp, b, nil
}

type reqOpts struct {
	needToken bool
	spaceKey  string
}

// do sends a request with the token, handles 401 invalid_token by getting a
// fresh one once, and retries short rate limits.
func (c *client) do(method, path string, body []byte, o reqOpts) (*http.Response, []byte, error) {
	if o.needToken {
		if _, err := c.ensureToken(); err != nil {
			return nil, nil, err
		}
	}
	renewed := false
	for attempt := 0; ; attempt++ {
		h := http.Header{}
		if t := c.token(); t != "" {
			h.Set("Authorization", "Bearer "+t)
		}
		if o.spaceKey != "" {
			h.Set("Space-Key", o.spaceKey)
		}
		resp, b, err := c.raw(method, path, body, h)
		if err != nil {
			return nil, nil, err
		}
		if resp.StatusCode == 401 && !renewed && c.envToken == "" {
			var e struct{ Error string }
			json.Unmarshal(b, &e)
			if e.Error == "invalid_token" || (e.Error == "missing_token" && c.token() == "") {
				renewed = true
				if _, err := c.freshToken(); err != nil {
					return nil, nil, err
				}
				continue
			}
		}
		if resp.StatusCode == 429 && attempt < 3 {
			if ra, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && ra <= 30 {
				time.Sleep(time.Duration(ra) * time.Second)
				continue
			}
		}
		return resp, b, nil
	}
}

// ---------- paths ----------

type kvPath struct {
	space, area string // area is "shared", "me", "users/<id>" or "users/*"
	name        string // key or pattern
}

func (p kvPath) String() string { return p.space + "/" + p.area + "/" + p.name }
func (p kvPath) url() string    { return "/v1/" + p.String() }
func (p kvPath) isPattern() bool {
	return strings.HasSuffix(p.name, "*")
}

func (c *client) parsePath(s string) (kvPath, error) {
	s = strings.TrimPrefix(s, "/")
	s = strings.TrimPrefix(s, "v1/")
	seg := strings.Split(s, "/")
	bad := &cliError{code: 5, err: "bad_path", msg: fmt.Sprintf("%q isn't a path like space/shared/key, space/users/me/key or space/me/key", s)}
	if len(seg) < 3 {
		return kvPath{}, bad
	}
	p := kvPath{space: seg[0]}
	switch seg[1] {
	case "shared", "me":
		if len(seg) != 3 {
			return kvPath{}, bad
		}
		p.area, p.name = seg[1], seg[2]
	case "users":
		if len(seg) != 4 {
			return kvPath{}, bad
		}
		id := seg[2]
		if id == "me" {
			var err error
			if id, err = c.visitorID(); err != nil {
				return kvPath{}, err
			}
		}
		p.area, p.name = "users/"+id, seg[3]
	default:
		return kvPath{}, bad
	}
	if !validSpace(p.space) {
		return kvPath{}, &cliError{code: 5, err: "invalid_space", msg: fmt.Sprintf("%q isn't a valid space name. Try %s.", p.space, suggestFor(p.space))}
	}
	return p, nil
}

func (c *client) visitorID() (string, error) {
	if c.envToken == "" && c.sc.ID != "" && c.sc.Token != "" {
		return c.sc.ID, nil
	}
	resp, b, err := c.do("GET", "/v1/whoami", nil, reqOpts{needToken: true})
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", apiErrFrom(resp, b)
	}
	var out struct{ ID string }
	json.Unmarshal(b, &out)
	if c.envToken == "" {
		c.sc.ID = out.ID
		c.creds.save()
	}
	return out.ID, nil
}

// spaceKeyFor returns the key to send for writes to shared/.
func (c *client) spaceKeyFor(p kvPath) string {
	if p.area != "shared" {
		return ""
	}
	if c.spaceKey != "" {
		return c.spaceKey
	}
	return c.sc.SpaceKeys[p.space]
}

// ---------- commands ----------

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
	"update": {[]string{"exec"}, nil, "update <path> [--exec CMD]\n  Read, pipe through CMD (or open $EDITOR), write back with ifVersion, retry on conflict.\n  Example: tiny-kv update space/shared/count --exec 'jq \"(. // 0) + 1\"'"},
	"new":    {[]string{"label"}, []string{"locked", "no-save"}, "new [--label L] [--locked] [--no-save]\n  Create a space. Locked spaces print the key on a second line and save it."},
	"name":   {nil, nil, "name [label]\n  Print a new valid space name. No server needed."},
	"whoami": {nil, nil, "whoami\n  Print your visitor ID."},
	"token":  {nil, nil, "token\n  Print your token, for curl: curl -H \"Authorization: Bearer $(tiny-kv token)\" ..."},
	"url":    {nil, nil, "url <path>\n  Print the full URL for a path."},
}

func cmdHelp(cmd string) int {
	switch cmd {
	case "serve":
		fmt.Printf("serve [--listen :8080] [--data DIR] [--config FILE] [--quiet]\n  Run the server. Env: TINY_KV_LISTEN, TINY_KV_DATA, TINY_KV_CONFIG.\n  Data goes in %s by default, created if missing.\n", defaultDataDir())
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
	fmt.Println("\nCommon flags: --server URL (TINY_KV_SERVER), --token T (TINY_KV_TOKEN), --space-key K (TINY_KV_SPACE_KEY)")
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
	c := newClient(f)
	switch cmd {
	case "name":
		if len(f.args) > 1 {
			return usageErr(errors.New("name takes at most one label"))
		}
		label := ""
		if len(f.args) == 1 {
			label = f.args[0]
		}
		fmt.Println(newSpaceName(label))
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
		fmt.Fprintf(os.Stderr, "tiny-kv: %s: %s\n", ce.err, ce.msg)
		return ce.code
	}
	fmt.Fprintln(os.Stderr, "tiny-kv:", err)
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
	o := reqOpts{needToken: p.area == "me"}
	resp, b, err := c.do("GET", p.url(), nil, o)
	if err != nil {
		return nil, false, 0, err
	}
	if resp.StatusCode != 200 {
		return nil, false, 0, apiErrFrom(resp, b)
	}
	return b, isJSONResp(resp), parseETag(resp), nil
}

func (c *client) get(f *flags) error {
	p, err := c.onePath(f, "get")
	if err != nil {
		return err
	}
	if p.isPattern() || p.area == "users/*" {
		return usageCli("get reads one key. Use ls for patterns and users/*/")
	}
	v, isJSON, ver, err := c.fetch(p)
	if err != nil {
		return err
	}
	if f.bool("json") {
		val := json.RawMessage(v)
		if !isJSON {
			val, _ = json.Marshal(string(v))
		}
		out, _ := json.Marshal(map[string]any{"key": p.name, "value": val, "version": ver})
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
				fmt.Fprintln(os.Stderr, "tiny-kv: warning: could not save the space key:", err)
			}
		}
	}
	return nil
}

// ---------- watch ----------

func (c *client) watch(f *flags) error {
	p, err := c.onePath(f, "watch")
	if err != nil {
		return err
	}
	if f.bool("raw") && (p.isPattern() || p.area == "users/*") {
		return usageCli("--raw is for watching one key")
	}
	fromNow := f.str("from") == "now"
	var from int64
	haveFrom := !fromNow
	if f.has("from") && !fromNow {
		if from, err = strconv.ParseInt(f.str("from"), 10, 64); err != nil || from < 0 {
			return usageCli("--from takes a version or now")
		}
	}
	state := f.str("state")
	if state != "" {
		if b, err := os.ReadFile(state); err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
				from, haveFrom = n, true
			}
		}
	}

	known := map[string]bool{} // keys currently known to exist, for re-syncs
	resync := false
	var resyncSeen map[string]bool
	backoff := time.Second
	needToken := p.area == "me"

	for {
		u := p.url() + "/watch"
		if haveFrom {
			u += "?version=" + strconv.FormatInt(from, 10)
		}
		resp, b, err := c.do("GET", u, nil, reqOpts{needToken: needToken})
		if err != nil {
			fmt.Fprintf(os.Stderr, "tiny-kv: %v, retrying in %s\n", err, backoff)
			time.Sleep(backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		switch {
		case resp.StatusCode == 200:
			backoff = time.Second
		case resp.StatusCode == 410:
			if fromNow {
				fmt.Fprintln(os.Stderr, "tiny-kv: version expired, continuing from now")
				haveFrom = false
			} else {
				fmt.Fprintln(os.Stderr, "tiny-kv: version expired, reloading current state")
				from, haveFrom, resync, resyncSeen = 0, true, true, map[string]bool{}
			}
			continue
		case resp.StatusCode == 429:
			var e struct{ RetryAfter int }
			json.Unmarshal(b, &e)
			time.Sleep(time.Duration(max(e.RetryAfter, 1)) * time.Second)
			continue
		case resp.StatusCode >= 500:
			fmt.Fprintf(os.Stderr, "tiny-kv: server error %d, retrying in %s\n", resp.StatusCode, backoff)
			time.Sleep(backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		default:
			return apiErrFrom(resp, b)
		}

		var out struct {
			Changes []item
			Version int64
			More    bool
		}
		if err := json.Unmarshal(b, &out); err != nil {
			return &cliError{code: 6, err: "bad_response", msg: err.Error()}
		}
		changes := out.Changes
		if resync {
			for _, ch := range changes {
				resyncSeen[ch.User+"/"+ch.Key] = true
			}
			if !out.More {
				for k := range known {
					if !resyncSeen[k] {
						u, key, _ := strings.Cut(k, "/")
						changes = append(changes, item{Key: key, User: u, Deleted: true, Version: out.Version})
					}
				}
				resync = false
			}
		}
		for _, ch := range changes {
			id := ch.User + "/" + ch.Key
			if ch.Deleted {
				delete(known, id)
			} else {
				known[id] = true
			}
			c.emit(ch, f)
		}
		from, haveFrom = out.Version, true
		if state != "" {
			if err := writeState(state, from); err != nil {
				fmt.Fprintln(os.Stderr, "tiny-kv: warning: could not save state:", err)
			}
		}
		if f.bool("once") && len(changes) > 0 && !out.More {
			return nil
		}
	}
}

func writeState(path string, v int64) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(v, 10)+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// rawValue turns a list/watch value back into the stored bytes: JSON as is,
// text unquoted.
func rawValue(v json.RawMessage) []byte {
	var s string
	if len(v) > 0 && v[0] == '"' && json.Unmarshal(v, &s) == nil {
		return []byte(s)
	}
	return v
}

func (c *client) emit(ch item, f *flags) {
	if cmd := f.str("exec"); cmd != "" {
		e := exec.Command("sh", "-c", cmd)
		if !ch.Deleted {
			e.Stdin = bytes.NewReader(rawValue(ch.Value))
		}
		e.Stdout, e.Stderr = os.Stdout, os.Stderr
		del := "0"
		if ch.Deleted {
			del = "1"
		}
		e.Env = append(os.Environ(), "KV_KEY="+ch.Key, "KV_VERSION="+strconv.FormatInt(ch.Version, 10), "KV_DELETED="+del)
		if ch.User != "" {
			e.Env = append(e.Env, "KV_USER="+ch.User)
		}
		if err := e.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "tiny-kv: warning: command for %s failed: %v\n", ch.Key, err)
		}
		return
	}
	if f.bool("raw") {
		if !ch.Deleted {
			os.Stdout.Write(append(rawValue(ch.Value), '\n'))
		}
		return
	}
	printItem(ch, f.bool("json"))
}

// ---------- update ----------

func (c *client) update(f *flags) error {
	p, err := c.onePath(f, "update")
	if err != nil {
		return err
	}
	if p.isPattern() || p.area == "users/*" {
		return usageCli("update works on one key")
	}
	cmd := f.str("exec")
	for attempt := 0; attempt < 10; attempt++ {
		cur, _, ver, err := c.fetch(p)
		var ce *cliError
		if errors.As(err, &ce) && ce.err == "not_found" {
			cur, ver, err = nil, 0, nil
		}
		if err != nil {
			return err
		}
		var next []byte
		if cmd != "" {
			in := cur
			if in == nil {
				in = []byte("null")
			}
			e := exec.Command("sh", "-c", cmd)
			e.Stdin, e.Stderr = bytes.NewReader(in), os.Stderr
			out, err := e.Output()
			if err != nil {
				return &cliError{code: 5, err: "exec_failed", msg: fmt.Sprintf("%s: %v", cmd, err)}
			}
			next = bytes.TrimRight(out, "\n")
		} else {
			if next, err = editValue(cur); err != nil {
				return err
			}
		}
		if len(next) == 0 || bytes.Equal(next, cur) {
			if ver != 0 {
				fmt.Println(ver)
			}
			return nil
		}
		v, _, err := c.put(p, next, &ver)
		if errors.As(err, &ce) && ce.err == "version_conflict" {
			if cmd == "" {
				fmt.Fprintln(os.Stderr, "tiny-kv: someone else changed it meanwhile, reopening with their version")
			}
			time.Sleep(time.Duration(50+attempt*20) * time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}
		fmt.Println(v)
		return nil
	}
	return &cliError{code: 2, err: "version_conflict", msg: "gave up after 10 conflicting writes"}
}

func editValue(cur []byte) ([]byte, error) {
	editor := firstNonEmpty(os.Getenv("VISUAL"), os.Getenv("EDITOR"), "vi")
	tmp, err := os.CreateTemp("", "tiny-kv-*.txt")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	tmp.Write(cur)
	tmp.Close()
	e := exec.Command("sh", "-c", editor+` "$1"`, "sh", tmp.Name())
	e.Stdin, e.Stdout, e.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := e.Run(); err != nil {
		return nil, &cliError{code: 5, err: "editor_failed", msg: err.Error()}
	}
	b, err := os.ReadFile(tmp.Name())
	if err != nil {
		return nil, err
	}
	if !bytes.HasSuffix(cur, []byte("\n")) {
		b = bytes.TrimSuffix(b, []byte("\n"))
	}
	return b, nil
}
