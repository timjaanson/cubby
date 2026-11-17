package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/timjaanson/cubby/internal/auth"
	"github.com/timjaanson/cubby/internal/config"
	"github.com/timjaanson/cubby/internal/version"
)

var defaultServer = "http://localhost:" + strconv.Itoa(config.DefaultPort)

type client struct {
	server string
	// defaulted: nothing named a server, so it is the built-in localhost one.
	defaulted bool
	creds     *credentials
	dc        *deviceCreds // this CLI's device on server, nil until made
	signer    *auth.Signer // its key
	apiKey    string       // from CUBBY_API_KEY: used instead of a device, never saved
	envKey    bool         // the device came from CUBBY_CREDENTIALS: never saved
	tok       string       // the current token, minted from signer
	tokExp    time.Time
	skew      time.Duration // server time minus ours, learned from a refused token
	http      *http.Client
}

func newClient(f *flags) (*client, error) {
	var cfg cliConfig
	toml.DecodeFile(filepath.Join(configDir(), "config.toml"), &cfg)
	server := config.ServerURL(firstNonEmpty(f.str("server"), os.Getenv("CUBBY_SERVER"), cfg.Server, defaultServer))
	if u, err := url.Parse(server); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, &cliError{code: 64, msg: fmt.Sprintf("server %q isn't a URL like https://kv.example.com", server)}
	}
	c := &client{server: server, creds: loadCreds(), apiKey: os.Getenv("CUBBY_API_KEY"), http: &http.Client{Timeout: 70 * time.Second},
		defaulted: firstNonEmpty(f.str("server"), os.Getenv("CUBBY_SERVER"), cfg.Server) == ""}
	if env := os.Getenv("CUBBY_CREDENTIALS"); env != "" {
		d, s, err := readDeviceCreds([]byte(env), server)
		if err != nil {
			return nil, &cliError{code: 64, msg: "CUBBY_CREDENTIALS: " + err.Error()}
		}
		c.dc, c.signer, c.envKey = d, s, true
	} else if d := c.creds.Servers[server]; d != nil {
		if s, err := auth.SignerFromJWK(d.Key); err == nil {
			c.dc, c.signer = d, s
		} else {
			fmt.Fprintf(os.Stderr, "cubby: warning: ignoring the key for %s in %s: %v\n", server, credsPath(), err)
		}
	}
	return c, nil
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
	case "not_found", "no_space":
		return 1
	case "version_conflict", "too_late":
		return 2
	case "no_access", "owner_only", "not_yours", "invite_invalid", "missing_token", "unknown_key", "bad_token", "bad_signature", "token_expired", "token_not_yet_valid", "wrong_audience", "invalid_api_key", "api_key_not_allowed", "api_key_space", "banned":
		return 3
	case "rate_limited", "stored_limit", "space_limit", "too_many_watches", "visitor_quota_exceeded", "space_quota_exceeded", "server_full":
		return 4
	}
	if status >= 400 && status < 500 {
		return 5
	}
	return 6
}

// origin is the server's origin, which tokens are signed for.
func (c *client) origin() string {
	u, _ := url.Parse(c.server)
	return u.Scheme + "://" + strings.ToLower(u.Host)
}

// token is the credential to send: the API key, or a token signed by this
// device, renewed five minutes before it ends. "" before there's a device.
func (c *client) token() string {
	if c.apiKey != "" {
		return c.apiKey
	}
	if c.signer == nil {
		return ""
	}
	now := time.Now().Add(c.skew)
	if c.tok == "" || now.After(c.tokExp.Add(-5*time.Minute)) {
		c.tok = c.signer.TokenAt(c.origin(), now, time.Hour, false)
		c.tokExp = now.Add(time.Hour)
	}
	return c.tok
}

// ensureDevice makes and registers this CLI's device key if there's none
// yet.
func (c *client) ensureDevice() error {
	if c.apiKey != "" || c.signer != nil {
		return nil
	}
	s, err := auth.NewSigner()
	if err != nil {
		return err
	}
	return c.register(s)
}

// register makes s a new visitor's first device on the server and saves it.
func (c *client) register(s *auth.Signer) error {
	host, _ := os.Hostname()
	body, _ := json.Marshal(map[string]string{"label": strings.TrimSpace("cubby CLI on " + firstNonEmpty(host, runtime.GOOS))})
	h := http.Header{}
	h.Set("Authorization", "Bearer "+s.TokenAt(c.origin(), time.Now().Add(c.skew), 5*time.Minute, true))
	resp, b, err := c.raw("POST", "/v1/visitors", body, h)
	if err != nil {
		return err
	}
	if resp.StatusCode != 201 && resp.StatusCode != 200 {
		return apiErrFrom(resp, b)
	}
	var out struct{ ID, Device string }
	json.Unmarshal(b, &out)
	c.adopt(&deviceCreds{Server: c.server, Visitor: out.ID, Device: s.Key.ID, Key: s.PrivateJWK()}, s)
	return nil
}

// adopt makes d this CLI's device on the server, saved unless it came from
// CUBBY_CREDENTIALS.
func (c *client) adopt(d *deviceCreds, s *auth.Signer) {
	c.dc, c.signer, c.tok = d, s, ""
	if c.envKey {
		return
	}
	c.creds.Servers[c.server] = d
	if err := c.creds.save(); err != nil {
		fmt.Fprintln(os.Stderr, "cubby: warning: could not save the device key:", err)
	}
}

// learnSkew reads the server's clock, after a token was refused for its
// times.
func (c *client) learnSkew() {
	start := time.Now()
	resp, b, err := c.raw("GET", "/v1/time", nil, nil)
	if err != nil || resp.StatusCode != 200 {
		return
	}
	var out struct{ T int64 }
	if json.Unmarshal(b, &out) == nil && out.T > 0 {
		mid := start.Add(time.Since(start) / 2)
		c.skew = time.UnixMilli(out.T).Sub(mid)
		c.tok = ""
	}
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
	req.Header.Set("User-Agent", "cubby/"+version.Version)
	resp, err := c.http.Do(req)
	if err != nil {
		var op *net.OpError
		if c.defaulted && errors.As(err, &op) && op.Op == "dial" {
			return nil, nil, &cliError{code: 6, err: "network_error", msg: fmt.Sprintf(
				"can't connect to %s, the built-in default. Name your server with --server, CUBBY_SERVER or server = \"...\" in %s",
				c.server, filepath.Join(configDir(), "config.toml"))}
		}
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
}

// do sends a request with the token. A token refused for its times is
// signed again after reading the server's clock; a key the server no longer
// knows is registered again, as a new visitor. Short rate limits on reads
// are retried. Rejected writes go back to the caller.
func (c *client) do(method, path string, body []byte, o reqOpts) (*http.Response, []byte, error) {
	if o.needToken {
		if err := c.ensureDevice(); err != nil {
			return nil, nil, err
		}
	}
	retried := false
	for attempt := 0; ; attempt++ {
		h := http.Header{}
		if t := c.token(); t != "" {
			h.Set("Authorization", "Bearer "+t)
		}
		resp, b, err := c.raw(method, path, body, h)
		if err != nil {
			return nil, nil, err
		}
		if resp.StatusCode == 401 && !retried && c.apiKey == "" {
			var e struct{ Error string }
			json.Unmarshal(b, &e)
			switch {
			case e.Error == "token_expired" || e.Error == "token_not_yet_valid":
				retried = true
				c.learnSkew()
				continue
			case e.Error == "unknown_key" && c.signer != nil && !c.envKey:
				retried = true
				fmt.Fprintf(os.Stderr, "cubby: this CLI's device was signed out on %s; registering its key again as a new visitor\n", c.server)
				if err := c.register(c.signer); err != nil {
					return nil, nil, err
				}
				continue
			case e.Error == "missing_token" && c.signer == nil:
				retried = true
				if err := c.ensureDevice(); err != nil {
					return nil, nil, err
				}
				continue
			}
		}
		if (method == "GET" || method == "HEAD") && resp.StatusCode == 429 && attempt < 3 {
			if ra, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && ra <= 30 {
				time.Sleep(time.Duration(ra) * time.Second)
				continue
			}
		}
		return resp, b, nil
	}
}
