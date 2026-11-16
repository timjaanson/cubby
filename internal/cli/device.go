package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/timjaanson/cubby/internal/auth"
	"github.com/timjaanson/cubby/internal/names"
)

// deviceCmd: this CLI's visitor on other devices. pair makes a code for
// another device, accept uses one here, backup makes a key to keep.
func (c *client) deviceCmd(f *flags) error {
	if len(f.args) == 0 {
		return usageCli("device pair|accept|backup|ls|name|rm ...")
	}
	sub, args := f.args[0], f.args[1:]
	switch sub {
	case "pair":
		if len(args) != 0 {
			return usageCli("device pair takes no arguments")
		}
		body := map[string]any{}
		if f.has("ttl") {
			d, err := time.ParseDuration(f.str("ttl"))
			if err != nil {
				return usageCli("--ttl takes a duration like 10m or 90s")
			}
			body["ttl"] = int64(d.Seconds())
		}
		p, err := c.pairingCode(body)
		if err != nil {
			return err
		}
		fmt.Println(p.Display)
		if f.has("link") {
			fmt.Println(strings.TrimRight(f.str("link"), "#") + "#pair=" + p.Code)
		}
		if f.bool("no-wait") {
			return nil
		}
		fmt.Fprintf(os.Stderr, "On the other device: cubby device accept %s, or enter it in a page. Valid until %s. Waiting...\n",
			p.Display, time.UnixMilli(p.Expires).Format("15:04:05"))
		for {
			var st struct {
				State  string
				Device *struct{ ID, Label string }
			}
			if err := c.call("GET", "/v1/me/pairing-codes/"+p.ID+"?wait=55", nil, 200, &st); err != nil {
				return err
			}
			switch st.State {
			case "open":
				continue
			case "used":
				fmt.Fprintf(os.Stderr, "Paired %s %s.\n", st.Device.ID, firstNonEmpty(st.Device.Label, "(no label)"))
				return nil
			}
			return &cliError{code: 1, err: "pairing_code_" + st.State, msg: "nobody used the code"}
		}
	case "accept":
		if len(args) != 1 {
			return usageCli("device accept takes the code, or a link with #pair=CODE")
		}
		code := args[0]
		if _, frag, ok := strings.Cut(code, "#"); ok {
			for _, kv := range strings.Split(frag, "&") {
				if v, ok := strings.CutPrefix(kv, "pair="); ok {
					code = v
				}
			}
		}
		if names.NormalizePairingCode(code) == "" {
			return usageCli(fmt.Sprintf("%q isn't a pairing code. Codes look like K7M2X-9Q4PA.", args[0]))
		}
		if c.dc != nil && !f.bool("yes") && !f.bool("y") {
			who := firstNonEmpty(c.dc.Visitor, "a visitor")
			fmt.Fprintf(os.Stderr, "This CLI is %s on %s. Pairing makes it the other device's visitor instead, and %s is deleted with its data if no other device uses it. Continue? [y/N] ", who, c.server, who)
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
				return &cliError{code: 5, err: "not_confirmed", msg: "nothing changed"}
			}
		}
		s := c.signer
		if s == nil {
			var err error
			if s, err = auth.NewSigner(); err != nil {
				return err
			}
		}
		label := f.str("label")
		if !f.has("label") {
			host, _ := os.Hostname()
			label = strings.TrimSpace("cubby CLI on " + firstNonEmpty(host, runtime.GOOS))
		}
		out, err := c.pair(s, code, label)
		if err != nil {
			return err
		}
		c.adopt(&deviceCreds{Server: c.server, Visitor: out.ID, Device: s.Key.ID, Key: s.PrivateJWK()}, s)
		fmt.Println(out.ID)
		if out.Deleted && out.Previous != nil {
			fmt.Fprintf(os.Stderr, "%s was deleted.\n", *out.Previous)
		}
		return nil
	case "backup":
		if len(args) != 0 {
			return usageCli("device backup takes no arguments; it prints the key to stdout")
		}
		// A new key, paired to this visitor, printed and forgotten here.
		p, err := c.pairingCode(map[string]any{"ttl": 60})
		if err != nil {
			return err
		}
		s, err := auth.NewSigner()
		if err != nil {
			return err
		}
		out, err := c.pair(s, p.Code, firstNonEmpty(f.str("label"), "Backup key "+time.Now().Format("2006-01-02")))
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(deviceCreds{Server: c.server, Visitor: out.ID, Device: s.Key.ID, Key: s.PrivateJWK()}, "", "  ")
		fmt.Println(string(b))
		fmt.Fprintln(os.Stderr, "This is a backup key: a device of yours that can do everything you can. Keep it in a password manager.\nRestore with: cubby login FILE. Revoke with: cubby device rm "+s.Key.ID)
		return nil
	case "ls":
		var out struct {
			Items []struct {
				ID, Label         string
				Created, LastSeen int64
				Current           bool
			}
		}
		if err := c.call("GET", "/v1/me/devices", nil, 200, &out); err != nil {
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
		id := "current"
		if len(args) == 2 {
			id = args[1]
		}
		return c.call("PATCH", "/v1/me/devices/"+id, map[string]string{"label": args[0]}, 200, nil)
	case "rm":
		if f.bool("others") {
			if len(args) != 0 {
				return usageCli("device rm --others takes no device ID")
			}
			var out struct{ Removed []string }
			if err := c.call("DELETE", "/v1/me/devices?others=1", nil, 200, &out); err != nil {
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
		return c.call("DELETE", "/v1/me/devices/"+args[0], nil, 200, nil)
	}
	return usageCli("device pair|accept|backup|ls|name|rm ...")
}

type pairingCode struct {
	ID, Code, Display string
	Expires           int64
}

func (c *client) pairingCode(body map[string]any) (*pairingCode, error) {
	var p pairingCode
	if err := c.call("POST", "/v1/me/pairing-codes", body, 201, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

type pairedOut struct {
	ID, Device string
	Previous   *string
	Deleted    bool
}

// pair uses a pairing code, signed by s with its public key in the header.
func (c *client) pair(s *auth.Signer, code, label string) (*pairedOut, error) {
	b, _ := json.Marshal(map[string]string{"code": code, "label": label})
	h := http.Header{}
	h.Set("Authorization", "Bearer "+s.TokenAt(c.origin(), time.Now().Add(c.skew), 5*time.Minute, true))
	resp, rb, err := c.raw("POST", "/v1/pair", b, h)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 201 {
		return nil, apiErrFrom(resp, rb)
	}
	var out pairedOut
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, &cliError{code: 6, err: "bad_response", msg: err.Error()}
	}
	return &out, nil
}

// login makes credentials from a file (or - for stdin) this CLI's device on
// their server.
func (c *client) login(f *flags) error {
	if len(f.args) != 1 {
		return usageCli("login takes a credentials file, or - for stdin")
	}
	var b []byte
	var err error
	if f.args[0] == "-" {
		b, err = io.ReadAll(os.Stdin)
	} else {
		b, err = os.ReadFile(f.args[0])
	}
	if err != nil {
		return &cliError{code: 5, err: "bad_file", msg: err.Error()}
	}
	d, s, err := readDeviceCreds(b, c.server)
	if err != nil {
		return &cliError{code: 5, err: "bad_credentials", msg: err.Error()}
	}
	d.Server = c.server
	c.envKey = false
	c.adopt(d, s)
	id, err := c.visitorID()
	if err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

// apiKeyCmd manages API keys: static credentials for tools that can't sign
// tokens. A token is always the better choice where one works.
func (c *client) apiKeyCmd(f *flags) error {
	if len(f.args) == 0 {
		return usageCli("api-key new|ls|rm ...")
	}
	sub, args := f.args[0], f.args[1:]
	switch sub {
	case "new":
		if len(args) != 1 {
			return usageCli("api-key new takes a space")
		}
		space, err := spaceArg(args[0])
		if err != nil {
			return err
		}
		body := map[string]any{"space": space}
		if f.has("access") {
			body["access"] = f.str("access")
		}
		if f.has("label") {
			body["label"] = f.str("label")
		}
		if f.has("until") {
			if u := f.str("until"); u == "never" {
				body["until"] = nil
			} else {
				body["until"] = u
			}
		}
		var out struct {
			ID, Key, Warning string
			Until            *int64
		}
		if err := c.call("POST", "/v1/me/api-keys", body, 201, &out); err != nil {
			return err
		}
		fmt.Println(out.Key)
		if out.Warning != "" {
			fmt.Fprintln(os.Stderr, "Warning:", out.Warning)
		} else {
			fmt.Fprintf(os.Stderr, "%s, valid until %s. Shown once.\n", out.ID, fmtTime(out.Until))
		}
		return nil
	case "ls":
		var out struct {
			Items []struct {
				ID, Label, Space, Access string
				Until, LastUsed          *int64
			}
		}
		if err := c.call("GET", "/v1/me/api-keys", nil, 200, &out); err != nil {
			return err
		}
		for _, k := range out.Items {
			until := fmtTime(k.Until)
			if k.Until == nil {
				until = "never"
			}
			fmt.Printf("%s\t%s\t%s\t%s\t%s\t%s\n", k.ID, k.Space, k.Access, until, fmtTime(k.LastUsed), firstNonEmpty(k.Label, "-"))
		}
		return nil
	case "rm":
		if len(args) != 1 {
			return usageCli("api-key rm <id>")
		}
		return c.call("DELETE", "/v1/me/api-keys/"+args[0], nil, 200, nil)
	}
	return usageCli("api-key new|ls|rm ...")
}
