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
	"time"

	"github.com/timjaanson/cubby/internal/config"
	"github.com/timjaanson/cubby/internal/names"
)

var clientValueFlags = []string{"server"}

var commandFlags = map[string]struct {
	vals, bools []string
	help        string
}{
	"limits":   {nil, nil, "limits [space]\n  Print effective server limits, or the space limits and your usage, as JSON."},
	"time":     {nil, nil, "time\n  Print estimated server clock offset and shortest round trip in milliseconds."},
	"get":      {nil, []string{"json"}, "get <path> [--json]\n  Print the raw value. Exit 1 if it doesn't exist."},
	"set":      {[]string{"if-version", "ttl", "if-before"}, []string{"create"}, "set <path> [value|-] [--if-version N] [--create] [--ttl 30s] [--if-before MS]\n  Write from the argument, or from stdin when it's missing or -. Prints the new version."},
	"del":      {[]string{"if-version", "if-before"}, []string{"f", "force"}, "del <path> [--if-version N] [--if-before MS] [-f]\n  Delete. With -f a missing key isn't an error."},
	"ls":       {[]string{"limit"}, []string{"json", "desc"}, "ls <path or pattern> [--limit N] [--desc] [--json]\n  One line per item: key<TAB>value, or author<TAB>key<TAB>value under by/*/ (the sender\n  under inbox/*/, the recipient under to/*/)."},
	"watch":    {[]string{"from", "state", "exec"}, []string{"json", "once", "raw"}, "watch <path or pattern> [--from N|now] [--state FILE] [--exec CMD] [--once] [--raw] [--json]\n  Print current state, then changes. --exec runs CMD per change with the value on stdin\n  and KV_KEY, KV_VERSION, KV_DELETED, KV_BY (or KV_FROM and KV_TO) set. --state FILE resumes after restarts."},
	"add":      {[]string{"time", "ttl", "if-before"}, nil, "add <path> [value|-] [--time MS] [--ttl 14d] [--if-before MS]\n  Add a point to a series, from the argument or stdin. Prints the point's time. A point at\n  an existing time replaces it. --ttl removes the point that long after its time.\nadd <prefix*> name=value ... [--time MS] [--ttl D] [--if-before MS]\n  Add one point to each series prefix+name, all at one time. A value is JSON if it parses,\n  otherwise a string. Without name=value arguments, reads one JSON object from stdin."},
	"series":   {[]string{"since", "until", "limit"}, []string{"desc", "json"}, "series <path> [--since T] [--until T] [--desc] [--limit N] [--json]\n  Print a series' points oldest first, as t<TAB>value. T is milliseconds or a duration back\n  from now (90m, 24h, 7d); both bounds are exclusive. Without --since, the newest 1000.\n  --desc prints newest first and pages back to --since or the oldest. --json prints\n  NDJSON, one point per line: {\"t\", \"value\", \"expires\"?}."},
	"pulse":    {nil, nil, "pulse <path> [value|-]\n  Send a pulse: a value that isn't stored, delivered to whoever listens right now and\n  then gone. Nothing to read back later. For signals between running things."},
	"pulses":   {[]string{"exec"}, []string{"json"}, "pulses <path or pattern> [--exec CMD] [--json]\n  Print pulses as they arrive, from now on: key<TAB>value, or author<TAB>key<TAB>value under\n  by/*/. --exec runs CMD per pulse with the value on stdin and KV_KEY, KV_BY,\n  KV_TIME (and KV_FROM and KV_TO under to/ and inbox/) set."},
	"update":   {[]string{"exec"}, nil, "update <path> [--exec CMD]\n  Read, pipe through CMD (or open $EDITOR), write back with ifVersion, retry on conflict.\n  Example: cubby update space/shared/count --exec 'jq \"(. // 0) + 1\"'"},
	"new":      {[]string{"label", "anyone", "invite"}, nil, "new [label | name] [--anyone none|read|post|all] [--invite read|post|all|owner]\n  Create a space you own and print its name. Spaces are private unless --anyone gives\n  everyone a level:\n    read      anyone reads, invited people write (status pages, sensor readings)\n    post      anyone reads, writes their own by/ keys and sends to/ (polls, RSVPs)\n    all       anyone with the name reads and writes\n  --invite is the lowest member level that may invite others, post unless given.\n  With nothing, the name is four random words. A label gets the words added\n  (new poll -> poll-cedar-pilot-mango-frost); other text is made into a label first\n  (\"Home environment\" -> home-environment-...). A full name of 16+ characters is used\n  as is and fails if taken. Keep a backup key (cubby device backup) to get back in\n  from another machine."},
	"spaces":   {nil, []string{"json"}, "spaces [--json]\n  Every space you belong to (owner or invited), newest membership change first: name,\n  access, changed, members. The same on every paired device. --json prints each item\n  with since, until, via, lastActive, bytes, keys and settings too."},
	"batch":    {nil, nil, "batch <space> [file|-]\n  Write several keys all or nothing: a JSON array of up to 100 writes like\n  [{\"method\": \"PUT\", \"path\": \"shared/title\", \"body\": \"Trip\"},\n   {\"method\": \"DELETE\", \"path\": \"by/me/draft\", \"query\": {\"ifVersion\": 7}},\n   {\"method\": \"POST\", \"path\": \"shared/temp/series\", \"query\": {\"t\": 1794927600000, \"ttl\": 1209600}, \"body\": 21.4}]\n  from the file or stdin. One series may get several points if each gives its own t. text instead of body stores a string as text. Prints\n  path<TAB>version per write. If one fails, nothing is written and the error names it."},
	"name":     {nil, nil, "name [label]\n  Print a new space name: four random words, after the label if given. No server needed."},
	"whoami":   {nil, nil, "whoami\n  Print your visitor ID. cubby device ls shows this device (*) among yours."},
	"invite":   {[]string{"access", "uses", "until", "grant-for", "label", "link"}, nil, "invite new <space> [--access read|post|all|owner] [--uses N|unlimited] [--until T|never]\n           [--grant-for D] [--label L] [--link URL]\n  Mint an invite. Members at the space's invite level (post unless changed) and owners\n  can. Defaults: 10 uses, 7 days, and access all, or the inviter's own level if lower.\n  Nobody gives more than they have. Only owners mint --access owner, for one use within\n  7 days. --until takes 12h, 30d, 2027-01-31 or an RFC 3339\n  time. --link prints URL#invite=INVITE to send.\ninvite ls <space>\n  Your invites, or every invite for an owner. One line per invite: ID, state, uses left,\n  until, members, label, access.\ninvite set <space> <id> [--uses N|unlimited] [--until T|never]\n  Change how often and how long it can still be redeemed. --uses 0 stops new ones.\ninvite rm <space> <id>\n  Revoke an invite and every grant redeemed from it."},
	"api-key":  {[]string{"access", "until", "label"}, nil, "api-key new <space> [--access read|post|all] [--until 30d|T|never] [--label L]\n  Mint an API key: a static secret for tools that can't sign tokens, like webhooks. It\n  reads and writes in one space only, at most at your level (post unless given), and\n  lasts 30 days unless --until says otherwise. never makes it permanent, the weakest\n  option. Printed once. Send it as Authorization: Bearer cubby_... or ?token=cubby_...\napi-key ls\n  Your API keys: ID, space, access, until, last used, label.\napi-key rm <id>\n  Revoke an API key."},
	"login":    {nil, nil, "login <file | ->\n  Use a credentials file (from cubby device backup, or written by hand) as this CLI's\n  device on its server. - reads it from stdin. Prints the visitor ID."},
	"url":      {nil, nil, "url <path>\n  Print the full URL for a path."},
	"join":     {nil, nil, "join <space> <invite or link>\n  Redeem an invite (inv_...) for your visitor. A link like\n  https://example.com/page/#invite=inv_... works too. Prints your level now."},
	"access":   {nil, nil, "access <space>\n  Your level in a space: banned, none, read, post, all or owner."},
	"settings": {[]string{"anyone", "invite"}, nil, "settings <space> [--anyone none|read|post|all] [--invite read|post|all|owner]\n  Print a space's settings, or change them (owners). --anyone is the level every visitor\n  has without an invite. --invite is the lowest member level that may invite others."},
	"drop":     {nil, []string{"yes"}, "drop <space> [--yes]\n  Delete a space and everything in it (owners). Asks for the name again unless --yes."},
	"token":    {[]string{"ttl"}, nil, "token [--ttl 1h]\n  Print a token for this device, signed here and valid for --ttl (at most 1h, the\n  default). For curl: -H \"Authorization: Bearer $(cubby token)\"."},
	"member":   {[]string{"until"}, nil, "member ls <space>\n  The space's members, highest level first: ID, access, since. Owners also see until\n  or -, and banned visitors.\nmember set <space> <id> <level> [--until T]\n  Give a visitor exactly one level (owners): read, post, all or owner, replacing what\n  they had, or banned to keep them out and close their live connections. --until\n  takes 12h, 30d, 2027-01-31 or an RFC 3339 time.\nmember rm <space> <id|me>\n  Remove a member's grants and any ban (owners), or leave a space yourself with me."},
	"device":   {[]string{"ttl", "link", "label"}, []string{"no-wait", "others", "yes", "y"}, "device pair [--ttl 10m] [--link URL] [--no-wait]\n  Make a one-use code that adds another device to your visitor: same ID, every grant,\n  every space. Prints the code (K7M2X-9Q4PA), with --link also URL#pair=CODE for a page,\n  then waits until it's used. Valid 10 minutes unless --ttl says otherwise (30s to 1h).\ndevice accept <code or link> [--label L] [--yes]\n  Join the visitor that made the code. This CLI's current visitor is deleted with its\n  data if no other device uses it, so it asks first unless --yes. Prints the new ID.\ndevice backup [--label L]\n  Print a new backup key: credentials for one more device of yours, to keep in a\n  password manager. cubby login FILE restores it on any machine.\ndevice ls\n  Your devices: ID (* for this one), created, last seen, label.\ndevice name <label> [device-id]\n  Label this device, or another of yours.\ndevice rm <device-id>\ndevice rm --others\n  Sign a device out, or every device but this one. Their keys stop working."},
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
	fmt.Println("\nCommon flags: --server URL or port (CUBBY_SERVER). Env: CUBBY_CREDENTIALS (a credentials\nfile's content, for CI), CUBBY_API_KEY (use an API key instead of a device).")
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
	case "spaces":
		return c.spaces(f)
	case "batch":
		return c.batch(f)
	case "whoami":
		id, err := c.visitorID()
		if err == nil {
			fmt.Println(id)
		}
		return err
	case "token":
		return c.printToken(f)
	case "login":
		return c.login(f)
	case "api-key":
		return c.apiKeyCmd(f)
	case "join":
		return c.join(f)
	case "access":
		return c.access(f)
	case "settings":
		return c.settings(f)
	case "drop":
		return c.drop(f)
	case "invite":
		return c.inviteCmd(f)
	case "member":
		return c.memberCmd(f)
	case "device":
		return c.deviceCmd(f)
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
	resp, b, err := c.do("GET", p.url(), nil, reqOpts{needToken: p.needsToken()})
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
	if p.isPattern() || p.area == "by/*" {
		return usageCli("get reads one key. Use ls for patterns and by/*/")
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
	From    string          `json:"from,omitempty"`
	To      string          `json:"to,omitempty"`
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

// partyOf is the column that tells items apart on a path spanning several
// areas: the author across by/*/, the sender across inbox/*/, the
// recipient across to/*/. "" for a path in one area.
func partyOf(area, by, from, to string) string {
	switch {
	case area == "by/*":
		return by
	case area == "inbox/*" || area == "inbox/owners/*":
		return from
	case area == "to/*":
		return to
	}
	return ""
}

func (it item) party(area string) string { return partyOf(area, it.By, it.From, it.To) }

func printItem(it item, asJSON bool, area string) {
	if asJSON {
		b, _ := json.Marshal(it)
		fmt.Println(string(b))
		return
	}
	val := "-"
	if !it.Deleted {
		val = compactJSON(it.Value)
	}
	if pt := it.party(area); pt != "" {
		fmt.Printf("%s\t%s\t%s\n", pt, it.Key, val)
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
		resp, b, err := c.do("GET", p.url()+"?"+q.Encode(), nil, reqOpts{needToken: p.needsToken()})
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
			printItem(it, f.bool("json"), p.area)
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
	// Any text works as a label: "Home environment" becomes home-environment.
	if l, ok := body["label"].(string); ok {
		if body["label"] = names.Slug(l); body["label"] == "" {
			return &cliError{code: 5, err: "invalid_parameter", msg: fmt.Sprintf("%q has no letters or digits to make a label from", l)}
		}
	}
	if err := settingsFlags(f, body); err != nil {
		return err
	}
	var out struct{ Space string }
	if err := c.call("POST", "/v1/spaces", body, 201, &out); err != nil {
		return err
	}
	fmt.Println(out.Space)
	return nil
}

// printToken prints a token for curl, signed by this device.
func (c *client) printToken(f *flags) error {
	if len(f.args) != 0 {
		return usageCli("token takes no arguments")
	}
	ttl := time.Hour
	if f.has("ttl") {
		d, err := time.ParseDuration(f.str("ttl"))
		if err != nil || d <= 0 || d > time.Hour {
			return usageCli("--ttl is a duration up to 1h, like 10m")
		}
		ttl = d
	}
	if c.apiKey != "" {
		return usageCli("CUBBY_API_KEY is set: send the API key itself, there's no token to make")
	}
	if err := c.ensureDevice(); err != nil {
		return err
	}
	fmt.Println(c.signer.TokenAt(c.origin(), time.Now().Add(c.skew), ttl, false))
	return nil
}
