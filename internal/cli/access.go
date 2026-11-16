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
	// Everything here acts as someone, except reading limits and the time.
	needToken := !strings.HasPrefix(path, "/v1/limits") && !strings.HasPrefix(path, "/v1/time")
	resp, rb, err := c.do(method, path, b, reqOpts{needToken: needToken})
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

// settingsFlags puts --anyone and --invite into a request body.
func settingsFlags(f *flags, body map[string]any) error {
	for _, k := range []string{"anyone", "invite"} {
		if f.has(k) {
			body[k] = f.str(k)
		}
	}
	return nil
}

// inviteArg takes an invite, or a link with invite=INVITE in its fragment.
func inviteArg(s string) string {
	if strings.HasPrefix(s, "inv_") {
		return s
	}
	if _, frag, ok := strings.Cut(s, "#"); ok {
		if q, err := url.ParseQuery(frag); err == nil && q.Get("invite") != "" {
			return q.Get("invite")
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
		return usageCli("join takes a space and an invite or link")
	}
	space, err := spaceArg(f.args[0])
	if err != nil {
		return err
	}
	var out struct{ ID, Access string }
	if err := c.call("POST", "/v1/"+space+"/join", map[string]string{"invite": inviteArg(f.args[1])}, 200, &out); err != nil {
		return err
	}
	fmt.Println(out.Access)
	return nil
}

type settingsOut struct{ Anyone, Invite string }

func (s settingsOut) print() { fmt.Printf("anyone\t%s\ninvite\t%s\n", s.Anyone, s.Invite) }

func (c *client) access(f *flags) error {
	if len(f.args) != 1 {
		return usageCli("access takes a space")
	}
	space, err := spaceArg(f.args[0])
	if err != nil {
		return err
	}
	var out struct{ Access string }
	if err := c.call("GET", "/v1/"+space+"/access", nil, 200, &out); err != nil {
		return err
	}
	fmt.Println(out.Access)
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
	var out struct{ Settings settingsOut }
	if len(body) == 0 {
		if err := c.call("GET", "/v1/"+space+"/access", nil, 200, &out); err != nil {
			return err
		}
	} else if err := c.call("PATCH", "/v1/"+space, body, 200, &out); err != nil {
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

func (c *client) inviteCmd(f *flags) error {
	if len(f.args) < 2 {
		return usageCli("invite new|ls|set|rm <space> ...")
	}
	sub := f.args[0]
	space, err := spaceArg(f.args[1])
	if err != nil {
		return err
	}
	base := "/v1/" + space + "/invites"
	switch sub {
	case "new":
		if len(f.args) != 2 {
			return usageCli("invite new takes a space")
		}
		body := map[string]any{}
		if f.has("access") {
			body["access"] = f.str("access")
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
		var out struct{ Invite string }
		if err := c.call("POST", base, body, 201, &out); err != nil {
			return err
		}
		if l := f.str("link"); l != "" {
			sep := "#"
			if strings.Contains(l, "#") {
				sep = "&"
			}
			fmt.Println(l + sep + "invite=" + out.Invite)
			return nil
		}
		fmt.Println(out.Invite)
		return nil
	case "ls":
		var out struct {
			Items []struct {
				ID, Label, State, Access string
				UsesLeft, Until          *int64
				Members                  int
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
			fmt.Printf("%s\t%s\t%s\t%s\t%d\t%s\t%s\n", t.ID, t.State, uses, fmtTime(t.Until), t.Members, label, t.Access)
		}
		return nil
	case "set":
		if len(f.args) != 3 {
			return usageCli("invite set takes a space and a token ID")
		}
		body := map[string]any{}
		if err := limitFlags(f, body); err != nil {
			return err
		}
		if len(body) == 0 {
			return usageCli("invite set needs --uses or --until")
		}
		return c.call("PATCH", base+"/"+url.PathEscape(f.args[2]), body, 200, nil)
	case "rm":
		if len(f.args) != 3 {
			return usageCli("invite rm takes a space and a token ID")
		}
		return c.call("DELETE", base+"/"+url.PathEscape(f.args[2]), nil, 200, nil)
	}
	return usageCli("invite new|ls|set|rm <space> ...")
}

func (c *client) memberCmd(f *flags) error {
	if len(f.args) < 2 {
		return usageCli("member ls|set|rm <space> ...")
	}
	sub := f.args[0]
	space, err := spaceArg(f.args[1])
	if err != nil {
		return err
	}
	base := "/v1/" + space + "/members"
	switch sub {
	case "ls":
		if len(f.args) != 2 {
			return usageCli("member ls takes a space")
		}
		var out struct {
			Items []struct {
				ID, Access string
				Since      int64
				Until      *int64
			}
		}
		if err := c.call("GET", base, nil, 200, &out); err != nil {
			return err
		}
		for _, m := range out.Items {
			since := m.Since
			fmt.Printf("%s\t%s\t%s\t%s\n", m.ID, m.Access, fmtTime(&since), fmtTime(m.Until))
		}
		return nil
	case "set":
		if len(f.args) != 4 {
			return usageCli("member set takes a space, a visitor ID and a level")
		}
		body := map[string]any{"access": f.args[3]}
		if f.has("until") {
			body["until"] = f.str("until")
		}
		return c.call("PUT", base+"/"+url.PathEscape(f.args[2]), body, 200, nil)
	case "rm":
		if len(f.args) != 3 {
			return usageCli("member rm takes a space and a visitor ID, or me")
		}
		return c.call("DELETE", base+"/"+url.PathEscape(f.args[2]), nil, 200, nil)
	}
	return usageCli("member ls|set|rm <space> ...")
}
