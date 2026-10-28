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

var clientValueFlags = []string{"server", "token"}

var commandFlags = map[string]struct {
	vals, bools []string
	help        string
}{
	"limits":      {nil, nil, "limits [space]\n  Print effective server limits, or the space limits and your usage, as JSON."},
	"time":        {nil, nil, "time\n  Print estimated server clock offset and shortest round trip in milliseconds."},
	"get":         {nil, []string{"json"}, "get <path> [--json]\n  Print the raw value. Exit 1 if it doesn't exist."},
	"set":         {[]string{"if-version", "ttl", "if-before"}, []string{"create"}, "set <path> [value|-] [--if-version N] [--create] [--ttl 30s] [--if-before MS]\n  Write from the argument, or from stdin when it's missing or -. Prints the new version."},
	"del":         {[]string{"if-version", "if-before"}, []string{"f", "force"}, "del <path> [--if-version N] [--if-before MS] [-f]\n  Delete. With -f a missing key isn't an error."},
	"ls":          {[]string{"limit"}, []string{"json", "desc"}, "ls <path or pattern> [--limit N] [--desc] [--json]\n  One line per item: key<TAB>value, or user<TAB>key<TAB>value under users/*/."},
	"watch":       {[]string{"from", "state", "exec"}, []string{"json", "once", "raw"}, "watch <path or pattern> [--from N|now] [--state FILE] [--exec CMD] [--once] [--raw] [--json]\n  Print current state, then changes. --exec runs CMD per change with the value on stdin\n  and KV_KEY, KV_VERSION, KV_DELETED, KV_USER set. --state FILE resumes after restarts."},
	"add":         {[]string{"time", "if-before"}, nil, "add <path> [value|-] [--time MS] [--if-before MS]\n  Add a point to a series, from the argument or stdin. Prints the point's time.\nadd <prefix*> name=value ... [--time MS] [--if-before MS]\n  Add one point to each series prefix+name, all at one time. A value is JSON if it parses,\n  otherwise a string. Without name=value arguments, reads one JSON object from stdin."},
	"series":      {[]string{"since", "limit"}, []string{"json"}, "series <path> [--since MS|24h|90m|7d] [--limit N] [--json]\n  Print a series' points oldest first, as t<TAB>value. Without --since, the newest 1000."},
	"pulse":       {nil, nil, "pulse <path> [value|-]\n  Send a pulse: a value that isn't stored, delivered to whoever listens right now and\n  then gone. Nothing to read back later. For signals between running things."},
	"pulses":      {[]string{"exec"}, []string{"json"}, "pulses <path or pattern> [--exec CMD] [--json]\n  Print pulses as they arrive, from now on: key<TAB>value, or user<TAB>key<TAB>value under\n  users/*/. --exec runs CMD per pulse with the value on stdin and KV_KEY, KV_BY,\n  KV_TIME, KV_USER set."},
	"update":      {[]string{"exec"}, nil, "update <path> [--exec CMD]\n  Read, pipe through CMD (or open $EDITOR), write back with ifVersion, retry on conflict.\n  Example: cubby update space/shared/count --exec 'jq \"(. // 0) + 1\"'"},
	"new":         {[]string{"label", "public", "read", "write", "invite"}, nil, "new [label | name] [--public none|read|respond|all] [--read R --write W] [--invite members|owners]\n  Create a space you own. Print its name, then its owner token: keep that one, it's\n  shown once and gets you back in from another machine. Spaces are private unless\n  --public says otherwise:\n    read      anyone reads, invited people write (status pages, sensor readings)\n    respond   anyone reads and adds their own users/ entry (polls, RSVPs)\n    all       anyone with the name reads and writes\n  With nothing, the name is four random words; a label gets the words added\n  (new poll -> poll-cedar-pilot-mango-frost); a full name of 16+ characters is used\n  as is and fails if taken."},
	"name":        {nil, nil, "name [label]\n  Print a new space name: four random words, after the label if given. No server needed."},
	"whoami":      {nil, []string{"show-token"}, "whoami [--show-token]\n  Print your visitor ID, or with --show-token your visitor token, for curl:\n  curl -H \"Authorization: Bearer $(cubby whoami --show-token)\" ..."},
	"url":         {nil, nil, "url <path>\n  Print the full URL for a path."},
	"join":        {nil, nil, "join <space> <token or link>\n  Redeem an invite (at_...) or an owner token (ot_...) for this CLI's visitor. A link\n  like https://example.com/page/#t=at_... works too. Prints what you may now do."},
	"access":      {nil, nil, "access <space>\n  What you may do in a space: owner, member or visitor, then one line per grant:\n  ID, until or -, scopes."},
	"settings":    {[]string{"public", "read", "write", "invite"}, nil, "settings <space> [--public none|read|respond|all | --read R --write W] [--invite members|owners]\n  Print a space's settings, or change them (owners). --write takes anyone, granted, or\n  a list like shared=granted,me=anyone. --invite says who may mint invites: members\n  (the default, each within their own rights) or owners only."},
	"drop":        {nil, []string{"yes"}, "drop <space> [--yes]\n  Delete a space and everything in it (owners). Asks for the name again unless --yes."},
	"owner-token": {nil, nil, "owner-token <space>\n  Rotate the space's owner token and print the new one (owners). Owners who joined\n  with the old one lose ownership; you keep yours."},
	"token":       {[]string{"allow", "uses", "until", "grant-for", "label", "link"}, nil, "token new <space> [--allow SCOPE ...] [--uses N|unlimited] [--until T|never] [--grant-for D]\n          [--label L] [--link URL]\n  Mint an access token: an invite. Owners and, unless the space says --invite owners,\n  members can. Defaults: 10 uses, 7 days, and a full member from an owner, or the\n  inviter's own rights from a member. A member can't give more than they hold. --allow takes scopes like shared/status.*:w, or member or viewer. --until\n  takes 12h, 30d, 2027-01-31 or an RFC 3339 time. --link prints URL#t=TOKEN to send.\ntoken ls <space>\n  Your tokens, or every token for an owner. One line per token: ID, state, uses left, until, grants, label, scopes.\ntoken set <space> <id> [--uses N|unlimited] [--until T|never]\n  Change how often and how long it can still be redeemed. --uses 0 stops new ones.\ntoken rm <space> <id>\n  Revoke a token and every grant redeemed from it."},
	"grant":       {[]string{"allow", "until", "user"}, []string{"owner"}, "grant ls <space> [--user ID]\n  One line per grant: ID, user, owner or member, source, until, scopes.\ngrant add <space> <user> [--allow SCOPE ...] [--until T] [--owner]\n  Give a visitor rights directly, with no token. Prints the grant ID.\ngrant rm <space> <grant-id>\ngrant rm <space> --user ID\n  Revoke one grant, or everything a visitor holds in the space."},
}

func cmdHelp(cmd string) int {
	switch cmd {
	case "serve":
		fmt.Printf(`serve [-p PORT | --listen ADDR] [--data DIR] [--config FILE] [--quiet]
  Run the server on port %d, on every interface.
  -p, --port 9000                  another port
  --listen 127.0.0.1:9000          one interface only (also takes a bare port)
  Env: CUBBY_LISTEN (port or address), CUBBY_DATA, CUBBY_CONFIG. Flags win over env,
  env wins over the config file. 'cubby config' shows the result.
  Data goes in %s, created if missing.
`, config.DefaultPort, config.PreviewDataDir())
		return 0
	case "admin":
		fmt.Print(adminUsage)
		return 0
	case "config":
		fmt.Print(configUsage)
		return 0
	}
	d, ok := commandFlags[cmd]
	if !ok {
		return usageErr(fmt.Errorf("unknown command %q", cmd))
	}
	fmt.Println(d.help)
	fmt.Println("\nCommon flags: --server URL or port (CUBBY_SERVER), --token T (CUBBY_TOKEN, a visitor token)")
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
	case "limits":
		return c.limits(f)
	case "time":
		return c.serverTime(f)
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
	case "pulse":
		return c.pulse(f)
	case "pulses":
		return c.pulses(f)
	case "add":
		return c.add(f)
	case "series":
		return c.series(f)
	case "new":
		return c.newSpace(f)
	case "whoami":
		if f.bool("show-token") {
			t, err := c.ensureToken()
			if err == nil {
				fmt.Println(t)
			}
			return err
		}
		id, err := c.visitorID()
		if err == nil {
			fmt.Println(id)
		}
		return err
	case "join":
		return c.join(f)
	case "access":
		return c.access(f)
	case "settings":
		return c.settings(f)
	case "drop":
		return c.drop(f)
	case "owner-token":
		return c.ownerToken(f)
	case "token":
		return c.tokenCmd(f)
	case "grant":
		return c.grantCmd(f)
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
	resp, b, err := c.fetchResponse(p)
	if err != nil {
		return nil, false, 0, err
	}
	return b, isJSONResp(resp), parseETag(resp), nil
}

func (c *client) fetchResponse(p kvPath) (*http.Response, []byte, error) {
	resp, b, err := c.do("GET", p.url(), nil, reqOpts{needToken: p.area == "me"})
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != 200 {
		return nil, nil, apiErrFrom(resp, b)
	}
	return resp, b, nil
}

func (c *client) get(f *flags) error {
	p, err := c.onePath(f, "get")
	if err != nil {
		return err
	}
	if p.isPattern() || p.area == "users/*" {
		return usageCli("get reads one key. Use ls for patterns and users/*/")
	}
	resp, v, err := c.fetchResponse(p)
	if err != nil {
		return err
	}
	if f.bool("json") {
		isJSON, ver := isJSONResp(resp), parseETag(resp)
		t, _ := strconv.ParseInt(resp.Header.Get("KV-Time"), 10, 64)
		expires, _ := strconv.ParseInt(resp.Header.Get("KV-Expires"), 10, 64)
		val := json.RawMessage(v)
		if !isJSON {
			val, _ = json.Marshal(string(v))
		}
		m := map[string]any{"key": p.name, "value": val, "version": ver}
		if t != 0 {
			m["t"] = t
		}
		if expires > 0 {
			m["expires"] = expires
		}
		if by := resp.Header.Get("KV-By"); by != "" {
			m["by"] = by
		}
		out, _ := json.Marshal(m)
		fmt.Println(string(out))
		return nil
	}
	os.Stdout.Write(v)
	return nil
}

func (c *client) put(p kvPath, value []byte, ifVersion *int64, extra ...url.Values) (int64, *http.Response, error) {
	u := p.url()
	q := url.Values{}
	if len(extra) > 0 {
		q = extra[0]
	}
	if ifVersion != nil {
		q.Set("ifVersion", strconv.FormatInt(*ifVersion, 10))
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	method := "PUT"
	if value == nil {
		method = "DELETE"
	}
	resp, b, err := c.do(method, u, value, reqOpts{needToken: true})
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
	q, err := writeFlags(f)
	if err != nil {
		return err
	}
	v, _, err := c.put(p, value, ifv, q)
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
	q, err := writeFlags(f)
	if err != nil {
		return err
	}
	v, _, err := c.put(p, nil, ifv, q)
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
	Expires int64           `json:"expires,omitempty"`
	Expired bool            `json:"expired,omitempty"`
	By      string          `json:"by,omitempty"`
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
	if len(f.args) > 1 {
		return usageCli("new takes one label or space name")
	}
	if len(f.args) == 1 && f.str("label") != "" {
		return usageCli("give the label as an argument or with --label, not both")
	}
	body := map[string]any{}
	switch {
	case f.str("label") != "":
		body["label"] = f.str("label")
	case len(f.args) == 1 && names.ValidSpace(f.args[0]):
		body["name"] = f.args[0] // already a full name: create exactly that
	case len(f.args) == 1:
		body["label"] = f.args[0]
	}
	if l, _ := body["label"].(string); l != "" && !names.ValidLabel(l) {
		return &cliError{code: 5, err: "invalid_parameter", msg: fmt.Sprintf("%q isn't a label or a space name. A label is lowercase letters, digits and single hyphens, at most %d characters", l, names.MaxLabel)}
	}
	if err := settingsFlags(f, body); err != nil {
		return err
	}
	var out struct{ Space, Owner, ID, Token string }
	if err := c.call("POST", "/v1/spaces", body, 201, &out); err != nil {
		return err
	}
	c.adopt(out.Token, out.ID)
	fmt.Println(out.Space)
	fmt.Println(out.Owner)
	return nil
}
