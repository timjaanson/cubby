package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/timjaanson/cubby/internal/version"
)

const defaultServer = "http://localhost:8080"

type client struct {
	server   string
	creds    *credentials
	sc       *serverCreds
	envToken string // from --token or CUBBY_TOKEN, never saved
	spaceKey string // from --space-key or CUBBY_SPACE_KEY
	http     *http.Client
}

func newClient(f *flags) (*client, error) {
	var cfg cliConfig
	toml.DecodeFile(filepath.Join(configDir(), "config.toml"), &cfg)
	server := firstNonEmpty(f.str("server"), os.Getenv("CUBBY_SERVER"), cfg.Server, defaultServer)
	if !strings.Contains(server, "://") {
		server = "http://" + server
	}
	if u, err := url.Parse(server); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, &cliError{code: 64, msg: fmt.Sprintf("server %q isn't a URL like https://kv.example.com", server)}
	}
	server = strings.TrimRight(server, "/")
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
		envToken: firstNonEmpty(f.str("token"), os.Getenv("CUBBY_TOKEN")),
		spaceKey: firstNonEmpty(f.str("space-key"), os.Getenv("CUBBY_SPACE_KEY")),
		http:     &http.Client{Timeout: 70 * time.Second},
	}, nil
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
		fmt.Fprintln(os.Stderr, "cubby: warning: could not save the token:", err)
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
	req.Header.Set("User-Agent", "cubby/"+version.Version)
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
