package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/timjaanson/cubby/internal/names"
)

type kvPath struct {
	space, area string // area is "shared", "me", "users/<id>" or "users/*"
	name        string // key or pattern
}

func (p kvPath) String() string { return p.space + "/" + p.area + "/" + p.name }

// url escapes every segment. The parser already allows only URL-safe
// characters, so this is a second guard against ?, # or % changing the target.
func (p kvPath) url() string {
	esc := func(seg string) string { return strings.ReplaceAll(url.PathEscape(seg), "%2A", "*") }
	segs := []string{esc(p.space)}
	for _, a := range strings.Split(p.area, "/") {
		segs = append(segs, esc(a))
	}
	return "/v1/" + strings.Join(append(segs, esc(p.name)), "/")
}
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
	if !names.ValidSpace(p.space) {
		return kvPath{}, &cliError{code: 5, err: "invalid_space", msg: fmt.Sprintf("%q isn't a valid space name. Try %s.", p.space, names.SuggestFor(p.space))}
	}
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
		if id != "me" && id != "*" && !names.ValidVisitor(id) {
			return kvPath{}, &cliError{code: 5, err: "invalid_key", msg: fmt.Sprintf("%q isn't a visitor ID. Use an ID like u_k7m2x9q4p1r8t3w6, me, or *.", id)}
		}
		p.area, p.name = "users/"+id, seg[3]
	default:
		return kvPath{}, bad
	}
	// Checked before anything is sent: a # or ? in a key would otherwise
	// silently address a different key.
	if !names.ValidKey(p.name) && !names.ValidPattern(p.name) {
		return kvPath{}, &cliError{code: 5, err: "invalid_key", msg: fmt.Sprintf("%q isn't a valid key. Keys are 1 to 128 of A-Z a-z 0-9 . _ : - and can't be \".\", \"..\", \"watch\" or \"series\". Patterns end in *.", p.name)}
	}
	if p.area == "users/me" {
		id, err := c.visitorID()
		if err != nil {
			return kvPath{}, err
		}
		p.area = "users/" + id
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
