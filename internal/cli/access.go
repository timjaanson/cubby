package cli

// Commands about who may do what in a space: joining, settings, access
// tokens, grants and the owner token.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/timjaanson/cubby/internal/names"
)

// call sends a JSON body (nil for none) with the visitor token if there is
// one, expects want, and decodes the answer into out.
func (c *client) call(method, path string, body any, want int, out any) error {
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	resp, rb, err := c.do(method, path, b, reqOpts{})
	if err != nil {
		return err
	}
	if resp.StatusCode != want && !(want == 200 && resp.StatusCode == 201) && !(want == 201 && resp.StatusCode == 200) {
		return apiErrFrom(resp, rb)
	}
	if out != nil {
		if err := json.Unmarshal(rb, out); err != nil {
			return &cliError{code: 6, err: "bad_response", msg: err.Error()}
		}
	}
	return nil
}

func spaceArg(s string) (string, error) {
	if !names.ValidSpace(s) {
		return "", &cliError{code: 5, err: "invalid_space", msg: fmt.Sprintf("%q isn't a valid space name. Try %s.", s, names.SuggestFor(s))}
	}
	return s, nil
}

// settingsFlags puts --public, or --read and --write, into a request body.
func settingsFlags(f *flags, body map[string]any) error {
	if f.has("public") {
		if f.has("read") || f.has("write") {
			return usageCli("give --public, or --read and --write, not both")
		}
		body["public"] = f.str("public")
		return nil
	}
	if f.has("read") {
		body["read"] = f.str("read")
	}
	if f.has("write") {
		w := f.str("write")
		if w == "anyone" || w == "granted" {
			body["write"] = w
		} else {
			m := map[string]string{}
			for _, part := range strings.Split(w, ",") {
				k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
				if !ok {
					return usageCli("--write takes anyone, granted, or a list like shared=granted,me=anyone")
				}
				m[k] = v
			}
			body["write"] = m
		}
	}
	return nil
}

// inviteToken takes a token, or a link with t=TOKEN in its fragment.
func inviteToken(s string) string {
	if strings.HasPrefix(s, "at_") || strings.HasPrefix(s, "ot_") {
		return s
	}
	if _, frag, ok := strings.Cut(s, "#"); ok {
		if q, err := url.ParseQuery(frag); err == nil && q.Get("t") != "" {
			return q.Get("t")
		}
	}
	return s
}

func fmtTime(ms *int64) string {
	if ms == nil {
		return "-"
	}
	return time.UnixMilli(*ms).Format("2006-01-02 15:04")
}

func (c *client) join(f *flags) error {
	if len(f.args) != 2 {
		return usageCli("join takes a space and a token or link")
	}
	space, err := spaceArg(f.args[0])
	if err != nil {
		return err
	}
	var out struct {
		ID, Token string
		Owner     bool
		Allow     []string
	}
	if err := c.call("POST", "/v1/"+space+"/join", map[string]string{"token": inviteToken(f.args[1])}, 200, &out); err != nil {
		return err
	}
	c.adopt(out.Token, out.ID)
	if out.Owner {
		fmt.Println("owner")
		return nil
	}
	for _, s := range out.Allow {
		fmt.Println(s)
	}
	return nil
}

type accessOut struct {
	ID       *string
	Owner    bool
	Settings settingsOut
	Grants   []struct {
		ID     string
		Owner  bool
		Allow  []string
		Source string
		Until  *int64
	}
}

type settingsOut struct {
	Public string
	Read   string
	Write  map[string]string
}

func (s settingsOut) print() {
	fmt.Printf("public\t%s\nread\t%s\nwrite.shared\t%s\nwrite.users\t%s\nwrite.me\t%s\n",
		s.Public, s.Read, s.Write["shared"], s.Write["users"], s.Write["me"])
}

func (c *client) access(f *flags) error {
	if len(f.args) != 1 {
		return usageCli("access takes a space")
	}
	space, err := spaceArg(f.args[0])
	if err != nil {
		return err
	}
	var out accessOut
	if err := c.call("GET", "/v1/"+space+"/access", nil, 200, &out); err != nil {
		return err
	}
	switch {
	case out.Owner:
		fmt.Println("owner")
	case len(out.Grants) > 0:
		fmt.Println("member")
	default:
		fmt.Println("visitor")
	}
	for _, g := range out.Grants {
		scopes := strings.Join(g.Allow, " ")
		if g.Owner {
			scopes = "owner"
		}
		fmt.Printf("%s\t%s\t%s\n", g.ID, fmtTime(g.Until), scopes)
	}
	return nil
}

func (c *client) settings(f *flags) error {
	if len(f.args) != 1 {
		return usageCli("settings takes a space")
	}
	space, err := spaceArg(f.args[0])
	if err != nil {
		return err
	}
	body := map[string]any{}
	if err := settingsFlags(f, body); err != nil {
		return err
	}
	if len(body) == 0 {
		var out accessOut
		if err := c.call("GET", "/v1/"+space+"/access", nil, 200, &out); err != nil {
			return err
		}
		out.Settings.print()
		return nil
	}
	var out struct{ Settings settingsOut }
	if err := c.call("PATCH", "/v1/"+space, body, 200, &out); err != nil {
		return err
	}
	out.Settings.print()
	return nil
}

func (c *client) drop(f *flags) error {
	if len(f.args) != 1 {
		return usageCli("drop takes a space")
	}
	space, err := spaceArg(f.args[0])
	if err != nil {
		return err
	}
	if !f.bool("yes") {
		fmt.Fprintf(os.Stderr, "This deletes %s and everything in it. Type the name to confirm: ", space)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(line) != space {
			return &cliError{code: 5, err: "not_confirmed", msg: "the name didn't match, nothing was deleted"}
		}
	}
	return c.call("DELETE", "/v1/"+space, nil, 200, nil)
}

func (c *client) ownerToken(f *flags) error {
	if len(f.args) != 1 {
		return usageCli("owner-token takes a space")
	}
	space, err := spaceArg(f.args[0])
	if err != nil {
		return err
	}
	var out struct{ Owner string }
	if err := c.call("POST", "/v1/"+space+"/owner-token", nil, 200, &out); err != nil {
		return err
	}
	fmt.Println(out.Owner)
	return nil
}

// limitFlags reads --uses and --until: "unlimited" and "never" are null.
func limitFlags(f *flags, body map[string]any) error {
	if f.has("uses") {
		if u := f.str("uses"); u == "unlimited" {
			body["uses"] = nil
		} else if n, err := strconv.Atoi(u); err == nil && n >= 0 {
			body["uses"] = n
		} else {
			return usageCli("--uses takes a number or unlimited")
		}
	}
	if f.has("until") {
		if u := f.str("until"); u == "never" {
			body["until"] = nil
		} else {
			body["until"] = u
		}
	}
	return nil
}

func allowBody(f *flags) any {
	l := f.list("allow")
	if len(l) == 1 && (l[0] == "member" || l[0] == "viewer") {
		return l[0]
	}
	return l
}

func (c *client) tokenCmd(f *flags) error {
	if len(f.args) < 2 {
		return usageCli("token new|ls|set|rm <space> ...")
	}
	sub := f.args[0]
	space, err := spaceArg(f.args[1])
	if err != nil {
		return err
	}
	base := "/v1/" + space + "/tokens"
	switch sub {
	case "new":
		if len(f.args) != 2 {
			return usageCli("token new takes a space")
		}
		body := map[string]any{}
		if f.has("allow") {
			body["allow"] = allowBody(f)
		}
		if f.has("label") {
			body["label"] = f.str("label")
		}
		if f.has("grant-for") {
			body["grantFor"] = f.str("grant-for")
		}
		if err := limitFlags(f, body); err != nil {
			return err
		}
		var out struct{ Token string }
		if err := c.call("POST", base, body, 201, &out); err != nil {
			return err
		}
		if l := f.str("link"); l != "" {
			sep := "#"
			if strings.Contains(l, "#") {
				sep = "&"
			}
			fmt.Println(l + sep + "t=" + out.Token)
			return nil
		}
		fmt.Println(out.Token)
		return nil
	case "ls":
		var out struct {
			Items []struct {
				ID, Label, State string
				Allow            []string
				UsesLeft, Until  *int64
				Grants           int
			}
		}
		if err := c.call("GET", base, nil, 200, &out); err != nil {
			return err
		}
		for _, t := range out.Items {
			uses := "-"
			if t.UsesLeft != nil {
				uses = strconv.FormatInt(*t.UsesLeft, 10)
			}
			label := t.Label
			if label == "" {
				label = "-"
			}
			fmt.Printf("%s\t%s\t%s\t%s\t%d\t%s\t%s\n", t.ID, t.State, uses, fmtTime(t.Until), t.Grants, label, strings.Join(t.Allow, " "))
		}
		return nil
	case "set":
		if len(f.args) != 3 {
			return usageCli("token set takes a space and a token ID")
		}
		body := map[string]any{}
		if err := limitFlags(f, body); err != nil {
			return err
		}
		if len(body) == 0 {
			return usageCli("token set needs --uses or --until")
		}
		return c.call("PATCH", base+"/"+url.PathEscape(f.args[2]), body, 200, nil)
	case "rm":
		if len(f.args) != 3 {
			return usageCli("token rm takes a space and a token ID")
		}
		return c.call("DELETE", base+"/"+url.PathEscape(f.args[2]), nil, 200, nil)
	}
	return usageCli("token new|ls|set|rm <space> ...")
}

func (c *client) grantCmd(f *flags) error {
	if len(f.args) < 2 {
		return usageCli("grant ls|add|rm <space> ...")
	}
	sub := f.args[0]
	space, err := spaceArg(f.args[1])
	if err != nil {
		return err
	}
	base := "/v1/" + space + "/grants"
	switch sub {
	case "ls":
		q := ""
		if f.has("user") {
			q = "?user=" + url.QueryEscape(f.str("user"))
		}
		var out struct {
			Items []struct {
				ID, User, Source string
				Owner            bool
				Allow            []string
				Until            *int64
			}
		}
		if err := c.call("GET", base+q, nil, 200, &out); err != nil {
			return err
		}
		for _, g := range out.Items {
			kind, scopes := "member", strings.Join(g.Allow, " ")
			if g.Owner {
				kind, scopes = "owner", "-"
			}
			fmt.Printf("%s\t%s\t%s\t%s\t%s\t%s\n", g.ID, g.User, kind, g.Source, fmtTime(g.Until), scopes)
		}
		return nil
	case "add":
		if len(f.args) != 3 {
			return usageCli("grant add takes a space and a visitor ID")
		}
		body := map[string]any{"user": f.args[2]}
		if f.bool("owner") {
			body["owner"] = true
		} else if f.has("allow") {
			body["allow"] = allowBody(f)
		}
		if f.has("until") {
			body["until"] = f.str("until")
		}
		var out struct{ ID string }
		if err := c.call("POST", base, body, 201, &out); err != nil {
			return err
		}
		fmt.Println(out.ID)
		return nil
	case "rm":
		switch {
		case f.has("user") && len(f.args) == 2:
			return c.call("DELETE", base+"?user="+url.QueryEscape(f.str("user")), nil, 200, nil)
		case !f.has("user") && len(f.args) == 3:
			return c.call("DELETE", base+"/"+url.PathEscape(f.args[2]), nil, 200, nil)
		}
		return usageCli("grant rm takes a space and a grant ID, or a space and --user ID")
	}
	return usageCli("grant ls|add|rm <space> ...")
}
