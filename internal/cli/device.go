package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/timjaanson/cubby/internal/names"
)

// deviceCmd: this CLI's visitor on other devices. link makes a code for
// another device, accept redeems one here.
func (c *client) deviceCmd(f *flags) error {
	if len(f.args) == 0 {
		return usageCli("device link|accept|ls|name|rm ...")
	}
	sub, args := f.args[0], f.args[1:]
	switch sub {
	case "link":
		if len(args) != 0 {
			return usageCli("device link takes no arguments")
		}
		body := map[string]any{}
		if f.has("ttl") {
			d, err := time.ParseDuration(f.str("ttl"))
			if err != nil {
				return usageCli("--ttl takes a duration like 10m or 90s")
			}
			body["ttl"] = int64(d.Seconds())
		}
		var l struct {
			ID, Code, Display string
			Expires           int64
		}
		if err := c.call("POST", "/v1/links", body, 201, &l); err != nil {
			return err
		}
		fmt.Println(l.Display)
		if f.has("link") {
			fmt.Println(strings.TrimRight(f.str("link"), "#") + "#link=" + l.Code)
		}
		if f.bool("no-wait") {
			return nil
		}
		fmt.Fprintf(os.Stderr, "On the other device: cubby device accept %s, or enter it in a page. Valid until %s. Waiting...\n",
			l.Display, time.UnixMilli(l.Expires).Format("15:04:05"))
		for {
			var st struct {
				State  string
				Device *struct{ ID, Label string }
			}
			if err := c.call("GET", "/v1/links/"+l.ID+"?wait=55", nil, 200, &st); err != nil {
				return err
			}
			switch st.State {
			case "open":
				continue
			case "used":
				label := st.Device.Label
				if label == "" {
					label = "(no label)"
				}
				fmt.Fprintf(os.Stderr, "Linked %s %s.\n", st.Device.ID, label)
				return nil
			}
			return &cliError{code: 1, err: "link_" + st.State, msg: "nobody used the code"}
		}
	case "accept":
		if len(args) != 1 {
			return usageCli("device accept takes the code, or a link with #link=CODE")
		}
		code := args[0]
		if _, frag, ok := strings.Cut(code, "#"); ok {
			for _, kv := range strings.Split(frag, "&") {
				if v, ok := strings.CutPrefix(kv, "link="); ok {
					code = v
				}
			}
		}
		if names.NormalizeLinkCode(code) == "" {
			return usageCli(fmt.Sprintf("%q isn't a link code. Codes look like K7M2X-9Q4PA.", args[0]))
		}
		cur := c.token()
		if cur != "" && c.envToken == "" && !f.bool("yes") && !f.bool("y") {
			fmt.Fprintf(os.Stderr, "This CLI is %s on %s. Linking makes it the other device's visitor instead, and %s is deleted with its data if no other device uses it. Continue? [y/N] ", firstNonEmpty(c.sc.ID, "a visitor"), c.server, firstNonEmpty(c.sc.ID, "it"))
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
				return &cliError{code: 5, err: "not_confirmed", msg: "nothing changed"}
			}
		}
		label := f.str("label")
		if !f.has("label") {
			host, _ := os.Hostname()
			label = strings.TrimSpace("cubby CLI on " + firstNonEmpty(host, runtime.GOOS))
		}
		b, _ := json.Marshal(map[string]string{"code": code, "label": label})
		h := http.Header{}
		if cur != "" {
			h.Set("Authorization", "Bearer "+cur)
		}
		resp, rb, err := c.raw("POST", "/v1/links/redeem", b, h)
		if err != nil {
			return err
		}
		if resp.StatusCode != 201 {
			return apiErrFrom(resp, rb)
		}
		var out struct {
			Token, ID, Device string
			Previous          *string
			Deleted           bool
		}
		if err := json.Unmarshal(rb, &out); err != nil {
			return &cliError{code: 6, err: "bad_response", msg: err.Error()}
		}
		if c.envToken != "" {
			// A token from --token or CUBBY_TOKEN isn't ours to replace.
			fmt.Println(out.Token)
			fmt.Fprintf(os.Stderr, "Linked as %s. The token above is the new one; the one you gave no longer works.\n", out.ID)
			return nil
		}
		c.adopt(out.Token, out.ID)
		fmt.Println(out.ID)
		if out.Deleted && out.Previous != nil {
			fmt.Fprintf(os.Stderr, "%s was deleted.\n", *out.Previous)
		}
		return nil
	case "ls":
		var out struct {
			Items []struct {
				ID, Label         string
				Created, LastSeen int64
				LinkedBy          *string
				Current           bool
			}
		}
		if err := c.call("GET", "/v1/devices", nil, 200, &out); err != nil {
			return err
		}
		for _, d := range out.Items {
			cur := ""
			if d.Current {
				cur = "*"
			}
			fmt.Printf("%s%s\t%s\t%s\t%s\n", d.ID, cur, time.UnixMilli(d.Created).Format("2006-01-02"),
				time.UnixMilli(d.LastSeen).Format("2006-01-02 15:04"), firstNonEmpty(d.Label, "-"))
		}
		return nil
	case "name":
		if len(args) < 1 || len(args) > 2 {
			return usageCli("device name <label> [device-id]")
		}
		id := "me"
		if len(args) == 2 {
			id = args[1]
		}
		return c.call("PATCH", "/v1/devices/"+id, map[string]string{"label": args[0]}, 200, nil)
	case "rm":
		if f.bool("others") {
			if len(args) != 0 {
				return usageCli("device rm --others takes no device ID")
			}
			var out struct{ Removed []string }
			if err := c.call("DELETE", "/v1/devices?others=1", nil, 200, &out); err != nil {
				return err
			}
			for _, id := range out.Removed {
				fmt.Println(id)
			}
			return nil
		}
		if len(args) != 1 {
			return usageCli("device rm <device-id> | --others")
		}
		return c.call("DELETE", "/v1/devices/"+args[0], nil, 200, nil)
	}
	return usageCli("device link|accept|ls|name|rm ...")
}
