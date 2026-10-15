package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

//go:embed kv.js
var kvJS []byte

type apiError struct {
	Status  int
	Code    string
	Message string
	Extra   map[string]any
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

var (
	errNotFound    = &apiError{Status: 404, Code: "not_found", Message: "No such key."}
	errSpaceLocked = &apiError{Status: 403, Code: "space_locked", Message: "This space is locked. Writing shared/ needs the right Space-Key header."}
	errServerFull  = &apiError{Status: 507, Code: "server_full", Message: "The server is full."}
	errMissing     = &apiError{Status: 401, Code: "missing_token", Message: "This request needs a token. Get one with POST /v1/anon and send it as Authorization: Bearer <token>."}
	errInvalidTok  = &apiError{Status: 401, Code: "invalid_token", Message: "The token is unknown or expired. Get a new one with POST /v1/anon."}
)

func quotaErr(code, msg string, limit, used int64) *apiError {
	return &apiError{Status: 507, Code: code, Message: msg, Extra: map[string]any{"limit": limit, "used": used}}
}

func badParam(name, msg string) *apiError {
	return &apiError{Status: 400, Code: "invalid_parameter", Message: msg, Extra: map[string]any{"parameter": name}}
}

func rateErr(code string, retry time.Duration) *apiError {
	secs := int(math.Ceil(retry.Seconds()))
	if secs < 1 {
		secs = 1
	}
	msg := "Too many requests. Try again in a moment."
	if code == "space_limit" {
		msg = "Too many new spaces. Try again later."
	}
	return &apiError{Status: 429, Code: code, Message: msg, Extra: map[string]any{"retryAfter": secs}}
}

type Server struct {
	cfg     *Config
	store   *Store
	lim     *limits
	watches watchCounter
	notify  notifier
	log     *log.Logger
}

func newServer(cfg *Config, store *Store, logger *log.Logger) *Server {
	s := &Server{
		cfg: cfg, store: store, lim: newLimits(), log: logger,
		watches: watchCounter{perIP: map[string]int{}},
		notify:  notifier{spaces: map[string]*spaceSignal{}},
	}
	store.onChange = s.notify.fire
	return s
}

// ---------- notifier ----------

type notifier struct {
	mu     sync.Mutex
	spaces map[string]*spaceSignal
}

type spaceSignal struct {
	ch      chan struct{}
	waiters int
}

// subscribe returns a channel that closes on the next write to the space.
func (n *notifier) subscribe(space string) <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := n.spaces[space]
	if s == nil {
		s = &spaceSignal{ch: make(chan struct{})}
		n.spaces[space] = s
	}
	return s.ch
}

func (n *notifier) join(space string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := n.spaces[space]
	if s == nil {
		s = &spaceSignal{ch: make(chan struct{})}
		n.spaces[space] = s
	}
	s.waiters++
}

func (n *notifier) leave(space string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if s := n.spaces[space]; s != nil {
		if s.waiters--; s.waiters <= 0 {
			delete(n.spaces, space)
		}
	}
}

func (n *notifier) fire(space string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if s := n.spaces[space]; s != nil {
		close(s.ch)
		s.ch = make(chan struct{})
	}
}

// ---------- plumbing ----------

type reqCtx struct {
	w       http.ResponseWriter
	r       *http.Request
	ip      string // rate-limit key: the IPv4 address or the IPv6 /64
	visitor string // "" when no valid token was sent
	tokErr  *apiError
	status  int
}

func (c *reqCtx) header() http.Header { return c.w.Header() }

func (c *reqCtx) json(status int, v any) {
	c.status = status
	c.header().Set("Content-Type", "application/json; charset=utf-8")
	c.w.WriteHeader(status)
	if c.r.Method == http.MethodHead {
		return
	}
	b, _ := json.Marshal(v)
	c.w.Write(append(b, '\n'))
}

func (c *reqCtx) fail(err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		ae = &apiError{Status: 500, Code: "internal", Message: "Internal server error."}
		log.Printf("internal error: %v", err)
	}
	body := map[string]any{"error": ae.Code, "message": ae.Message}
	for k, v := range ae.Extra {
		body[k] = v
	}
	if ae.Status == 429 {
		c.header().Set("Retry-After", fmt.Sprint(ae.Extra["retryAfter"]))
	}
	if ae.Status == 405 {
		c.header().Set("Allow", fmt.Sprint(ae.Extra["allow"]))
	}
	c.json(ae.Status, body)
}

// needVisitor returns the visitor or the right 401.
func (c *reqCtx) needVisitor() (string, error) {
	if c.visitor != "" {
		return c.visitor, nil
	}
	if c.tokErr != nil {
		return "", c.tokErr
	}
	return "", errMissing
}

func (s *Server) clientIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, _ := netip.ParseAddr(host)
	addr = addr.Unmap()
	if !s.trusted(addr) {
		return addr
	}
	// Walk X-Forwarded-For from the right, skipping trusted proxies.
	xff := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(xff) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(xff[i]))
		if err != nil {
			break
		}
		addr = a.Unmap()
		if !s.trusted(addr) {
			break
		}
	}
	return addr
}

func (s *Server) trusted(a netip.Addr) bool {
	for _, p := range s.cfg.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func ipKey(a netip.Addr) string {
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

func (s *Server) rate(l *limiter, key string, n float64, code string) error {
	if !s.cfg.rateLimitsOn() {
		return nil
	}
	if ok, retry := l.take(key, n); !ok {
		return rateErr(code, retry)
	}
	return nil
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func shortHash(id string) string {
	if id == "" {
		return "-"
	}
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:4])
}

// ServeHTTP handles every route. Routing is by hand because spaces, areas and
// keys are all path segments with their own errors.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w, status: 200}
	addr := s.clientIP(r)
	c := &reqCtx{w: sw, r: r, ip: ipKey(addr)}

	defer func() {
		if s.log != nil {
			// Path only: query strings may carry a token.
			s.log.Printf("%s %s %d %s %s %s", r.Method, r.URL.EscapedPath(), sw.status,
				time.Since(start).Round(time.Millisecond), addr, shortHash(c.visitor))
		}
	}()

	h := w.Header()
	if r.Method == http.MethodOptions {
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, HEAD, PUT, DELETE, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization, Space-Key, Content-Type, If-Match, If-None-Match")
		h.Set("Access-Control-Max-Age", "86400")
		sw.WriteHeader(204)
		return
	}

	switch r.URL.Path {
	case "/health":
		c.json(200, map[string]any{"ok": true})
		return
	case "/kv.js":
		h.Set("Content-Type", "text/javascript; charset=utf-8")
		h.Set("Cache-Control", "public, max-age=3600")
		h.Set("Access-Control-Allow-Origin", "*")
		http.ServeContent(sw, r, "kv.js", buildTime, bytes.NewReader(kvJS))
		return
	case "/":
		h.Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(sw, "cubby %s\n\nAPI under /v1, JS client at /kv.js, health at /health.\n", version)
		return
	}

	if !strings.HasPrefix(r.URL.EscapedPath(), "/v1/") {
		c.fail(&apiError{Status: 404, Code: "not_found", Message: "No such route."})
		return
	}
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Expose-Headers", "ETag, Retry-After, KV-Time")
	h.Set("Cache-Control", "no-store")

	// Token: header wins over the query parameter.
	tok := ""
	if scheme, cred, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " "); ok && strings.EqualFold(scheme, "Bearer") {
		tok = strings.TrimSpace(cred)
	} else if t := r.URL.Query().Get("token"); t != "" {
		tok = t
	}
	if tok != "" && !tokenRe.MatchString(tok) {
		c.tokErr = errInvalidTok // malformed, no need to look it up
	} else if tok != "" {
		id, err := s.store.visitorByToken(tok)
		if err != nil {
			c.fail(err)
			return
		}
		if id == "" {
			c.tokErr = errInvalidTok
		}
		c.visitor = id
	}

	if err := s.route(c); err != nil {
		c.fail(err)
	}
}

func (s *Server) route(c *reqCtx) error {
	raw := strings.Split(strings.TrimPrefix(c.r.URL.EscapedPath(), "/v1/"), "/")
	seg := make([]string, len(raw))
	for i, p := range raw {
		u, err := url.PathUnescape(p)
		if err != nil {
			return &apiError{Status: 400, Code: "invalid_key", Message: "Bad escaping in the path."}
		}
		seg[i] = u
	}
	m := c.r.Method

	if len(seg) == 1 {
		switch seg[0] {
		case "anon":
			if m != http.MethodPost {
				return methodErr("POST")
			}
			if err := checkParams(c.r); err != nil {
				return err
			}
			return s.anon(c)
		case "whoami":
			if m != http.MethodGet && m != http.MethodHead {
				return methodErr("GET, HEAD")
			}
			if err := checkParams(c.r); err != nil {
				return err
			}
			id, err := c.needVisitor()
			if err != nil {
				return err
			}
			c.json(200, map[string]any{"id": id})
			return nil
		case "spaces":
			if m != http.MethodPost {
				return methodErr("POST")
			}
			if err := checkParams(c.r, "label", "locked"); err != nil {
				return err
			}
			return s.createSpace(c)
		}
	}
	if len(seg) < 3 {
		return &apiError{Status: 404, Code: "not_found", Message: "No such route. Paths look like /v1/{space}/shared/{key}."}
	}

	space := seg[0]
	if !validSpace(space) {
		return &apiError{Status: 400, Code: "invalid_space",
			Message: "A space name is an optional label plus 8 random characters with at least 2 digits and 2 letters, like poll-k7m2x9q4.",
			Extra:   map[string]any{"suggestion": suggestFor(space)}}
	}

	t := target{space: space}
	var rest []string
	switch seg[1] {
	case "shared":
		t.area, rest = "s", seg[2:]
	case "me":
		id, err := c.needVisitor()
		if err != nil {
			return err
		}
		t.area, rest = "m:"+id, seg[2:]
	case "users":
		if len(seg) < 4 {
			return &apiError{Status: 404, Code: "not_found", Message: "No such route. Paths look like /v1/{space}/users/{id}/{key}."}
		}
		if seg[2] == "*" {
			t.allUsers = true
		} else if validVisitor(seg[2]) {
			t.area = "u:" + seg[2]
		} else {
			return &apiError{Status: 400, Code: "invalid_key", Message: "Not a visitor ID. IDs look like u_k7m2x9q4p1r8t3w6."}
		}
		rest = seg[3:]
	default:
		return &apiError{Status: 404, Code: "not_found", Message: "No such area. Use shared, users/{id} or me."}
	}

	// /{target} or /{target}/{action}, nothing after the action.
	action := ""
	switch {
	case len(rest) == 2 && actionWords[rest[1]]:
		action = rest[1]
	case len(rest) == 2:
		return &apiError{Status: 400, Code: "invalid_key", Message: "After a key or pattern, a path can only end in /watch or /series. Keys can't contain slashes."}
	case len(rest) != 1:
		return &apiError{Status: 400, Code: "invalid_key", Message: "Keys can't contain slashes, and nothing comes after /watch or /series."}
	}
	name := rest[0]
	switch {
	case validPattern(name):
		t.pattern, t.key = true, strings.TrimSuffix(name, "*")
	case validKey(name):
		t.key = name
	default:
		return &apiError{Status: 400, Code: "invalid_key",
			Message: "A key is 1 to 128 of A-Z a-z 0-9 . _ : - and can't be \".\", \"..\", \"watch\" or \"series\". A pattern is a prefix followed by *."}
	}

	if action == "series" {
		if t.allUsers {
			return &apiError{Status: 400, Code: "invalid_key", Message: "Series can't be read or added across users/*/ yet. Use users/{id}/{key}/series."}
		}
		switch m {
		case http.MethodGet, http.MethodHead:
			if t.pattern {
				return &apiError{Status: 400, Code: "invalid_key", Message: "Series are read one key at a time. To see the newest point of each, GET the pattern without /series."}
			}
			if err := checkParams(c.r, "since", "limit"); err != nil {
				return err
			}
			return s.seriesRead(c, t)
		case http.MethodPost:
			if err := checkParams(c.r, "t"); err != nil {
				return err
			}
			return s.seriesAdd(c, t)
		}
		return methodErrMsg("GET, HEAD, POST", "A series takes POST to add points and GET to read them. To replace or delete the key, use the path without /series.")
	}
	if action == "watch" {
		if m != http.MethodGet {
			return methodErr("GET")
		}
		if err := checkParams(c.r, "version", "timeout"); err != nil {
			return err
		}
		return s.watch(c, t)
	}
	isRead := m == http.MethodGet || m == http.MethodHead
	if t.allUsers || t.pattern {
		if !isRead {
			if m == http.MethodPost && t.pattern && !t.allUsers {
				return methodErrMsg("GET, HEAD", "To add a point to each of several series, POST to "+name+"/series.")
			}
			return methodErrMsg("GET, HEAD", "Patterns and users/*/ are read-only. Write and delete one key at a time.")
		}
		if err := checkParams(c.r, "limit", "order", "after"); err != nil {
			return err
		}
		return s.list(c, t)
	}
	switch m {
	case http.MethodGet, http.MethodHead:
		if err := checkParams(c.r); err != nil {
			return err
		}
		return s.read(c, t)
	case http.MethodPut, http.MethodDelete:
		if err := checkParams(c.r, "ifVersion"); err != nil {
			return err
		}
		return s.write(c, t)
	case http.MethodPost:
		return methodErrMsg("GET, HEAD, PUT, DELETE", "To add a point use "+t.key+"/series. To replace the value use PUT.")
	}
	return methodErr("GET, HEAD, PUT, DELETE")
}

// checkParams rejects query parameters a route doesn't take, and any given
// twice. A typo like ifversion=0 would otherwise turn a conditional write into
// an unconditional one. token is accepted everywhere, and _ is ignored so
// cache-busting libraries work.
func checkParams(r *http.Request, allowed ...string) error {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return badParam("query", "The query string is malformed.")
	}
	for k, vs := range q {
		if k == "token" || k == "_" {
			continue
		}
		known := false
		for _, a := range allowed {
			known = known || k == a
		}
		if !known {
			msg := fmt.Sprintf("Unknown parameter %q here.", k)
			if len(allowed) > 0 {
				msg += " This route takes: " + strings.Join(allowed, ", ") + "."
			}
			return badParam(k, msg)
		}
		if len(vs) > 1 {
			return badParam(k, fmt.Sprintf("Parameter %q is given more than once.", k))
		}
	}
	return nil
}

// parseUint accepts plain decimal digits only: no sign, no spaces, no hex,
// at most 16 digits (versions stay below 2^53).
func parseUint(s string) (int64, bool) {
	if s == "" || len(s) > 16 {
		return 0, false
	}
	var n int64
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int64(s[i]-'0')
	}
	return n, true
}

func methodErr(allow string) error {
	return methodErrMsg(allow, "Method not allowed here.")
}

func methodErrMsg(allow, msg string) error {
	return &apiError{Status: 405, Code: "method_not_allowed", Message: msg, Extra: map[string]any{"allow": allow}}
}

// ---------- handlers ----------

func (s *Server) anon(c *reqCtx) error {
	if err := s.rate(s.lim.anonPerIP, c.ip, 1, "rate_limited"); err != nil {
		return err
	}
	tok, id, err := s.store.newVisitor()
	if err != nil {
		return err
	}
	c.visitor = id
	c.json(201, map[string]any{"token": tok, "id": id})
	return nil
}

func (s *Server) spaceRate(c *reqCtx, visitor string) error {
	if err := s.rate(s.lim.spacesPerVisitor, visitor, 1, "space_limit"); err != nil {
		return err
	}
	return s.rate(s.lim.spacesPerIP, c.ip, 1, "space_limit")
}

func (s *Server) createSpace(c *reqCtx) error {
	id, err := c.needVisitor()
	if err != nil {
		return err
	}
	q := c.r.URL.Query()
	label := q.Get("label")
	if label != "" && (!labelRe.MatchString(label) || len(label) > 54) {
		return badParam("label", "A label is lowercase letters, digits and single hyphens, at most 54 characters.")
	}
	locked := false
	if q.Has("locked") {
		switch q.Get("locked") {
		case "", "1", "true":
			locked = true
		case "0", "false":
		default:
			return badParam("locked", "locked takes no value, or true or false.")
		}
	}
	if err := s.spaceRate(c, id); err != nil {
		return err
	}
	name, key, err := s.store.createSpace(label, locked)
	if err != nil {
		return err
	}
	out := map[string]any{"space": name}
	if locked {
		out["key"] = key
	}
	c.json(201, out)
	return nil
}

func valueHeaders(h http.Header, e *Entry) {
	if e.IsJSON {
		h.Set("Content-Type", "application/json; charset=utf-8")
	} else {
		h.Set("Content-Type", "text/plain; charset=utf-8")
	}
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
}

func etag(v int64) string { return `"` + strconv.FormatInt(v, 10) + `"` }

func (s *Server) read(c *reqCtx, t target) error {
	if err := s.rate(s.lim.readsPerIP, c.ip, 1, "rate_limited"); err != nil {
		return err
	}
	e, err := s.store.get(t.space, t.area, t.key)
	if err != nil {
		return err
	}
	if e == nil {
		return errNotFound
	}
	h := c.header()
	h.Set("ETag", etag(e.Version))
	if e.Series {
		h.Set("KV-Time", strconv.FormatInt(e.T, 10))
	}
	if inm := c.r.Header.Get("If-None-Match"); inm != "" && strings.TrimPrefix(inm, "W/") == etag(e.Version) {
		c.status = 304
		c.w.WriteHeader(304)
		return nil
	}
	valueHeaders(h, e)
	h.Set("Content-Length", strconv.Itoa(len(e.Value)))
	c.w.WriteHeader(200)
	if c.r.Method != http.MethodHead {
		c.w.Write(e.Value)
	}
	return nil
}

func parseIfVersion(r *http.Request) (*int64, error) {
	raw := r.URL.Query().Get("ifVersion")
	if !r.URL.Query().Has("ifVersion") {
		im := strings.TrimSpace(r.Header.Get("If-Match"))
		if im == "" {
			return nil, nil
		}
		raw = strings.Trim(strings.TrimPrefix(im, "W/"), `"`)
	}
	n, ok := parseUint(raw)
	if !ok {
		return nil, badParam("ifVersion", "ifVersion must be a version number, or 0 for \"only if it doesn't exist\".")
	}
	return &n, nil
}

// jsonDepth returns the nesting depth of a valid JSON text.
func jsonDepth(b []byte) int {
	depth, max, inStr, esc := 0, 0, false, false
	for _, c := range b {
		switch {
		case inStr:
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '[' || c == '{':
			if depth++; depth > max {
				max = depth
			}
		case c == ']' || c == '}':
			depth--
		}
	}
	return max
}

func (s *Server) write(c *reqCtx, t target) error {
	id, err := c.needVisitor()
	if err != nil {
		return err
	}
	if strings.HasPrefix(t.area, "u:") && t.area != "u:"+id {
		return &apiError{Status: 403, Code: "not_owner", Message: "Only that visitor can write under users/" + strings.TrimPrefix(t.area, "u:") + "/."}
	}
	ifv, err := parseIfVersion(c.r)
	if err != nil {
		return err
	}
	req := writeReq{space: t.space, area: t.area, key: t.key, writer: id, ifVersion: ifv,
		spaceKey: c.r.Header.Get("Space-Key"),
		onCreate: func() error { return s.spaceRate(c, id) }}

	if c.r.Method == http.MethodPut {
		maxValue, _, _ := s.cfg.limitsFor(t.space)
		body, err := io.ReadAll(io.LimitReader(c.r.Body, maxValue+1))
		if err != nil {
			return &apiError{Status: 400, Code: "invalid_encoding", Message: "Could not read the body."}
		}
		if int64(len(body)) > maxValue {
			return &apiError{Status: 413, Code: "value_too_large", Message: "The value is too large.", Extra: map[string]any{"limit": maxValue}}
		}
		if len(body) == 0 {
			return &apiError{Status: 400, Code: "empty_value", Message: "Empty values aren't stored. To remove a key, use DELETE."}
		}
		if !utf8.Valid(body) {
			return &apiError{Status: 400, Code: "invalid_encoding", Message: "Values must be UTF-8 text."}
		}
		if json.Valid(body) {
			if jsonDepth(body) > 64 {
				return &apiError{Status: 400, Code: "json_too_deep", Message: "JSON may be nested at most 64 levels."}
			}
			req.isJSON = true
		}
		req.value = body
	}

	if err := s.rate(s.lim.writesPerVisitor, id, 1, "rate_limited"); err != nil {
		return err
	}
	if err := s.rate(s.lim.writesPerIP, c.ip, 1, "rate_limited"); err != nil {
		return err
	}
	if err := s.rate(s.lim.writeBytesPerIP, c.ip, float64(len(req.value)+len(t.key)), "rate_limited"); err != nil {
		return err
	}

	res, err := s.store.write(c.r.Context(), req)
	if err != nil {
		return err
	}
	if res.changed {
		s.notify.fire(t.space)
	}
	c.header().Set("ETag", etag(res.version))
	status := 200
	if res.created && c.r.Method == http.MethodPut {
		status = 201
	}
	c.json(status, map[string]any{"version": res.version})
	return nil
}

type itemJSON struct {
	Key     string          `json:"key"`
	Value   json.RawMessage `json:"value,omitempty"`
	Deleted bool            `json:"deleted,omitempty"`
	Version int64           `json:"version"`
	User    string          `json:"user,omitempty"`
	T       *int64          `json:"t,omitempty"`
}

func toItem(e Entry) itemJSON {
	it := itemJSON{Key: e.Key, Version: e.Version, User: e.User}
	if e.Series && e.Value != nil {
		t := e.T
		it.T = &t
	}
	switch {
	case e.Value == nil:
		it.Deleted = true
	case e.IsJSON:
		it.Value = json.RawMessage(e.Value)
	default:
		b, _ := json.Marshal(string(e.Value))
		it.Value = b
	}
	return it
}

func (s *Server) list(c *reqCtx, t target) error {
	if err := s.rate(s.lim.readsPerIP, c.ip, 1, "rate_limited"); err != nil {
		return err
	}
	q := c.r.URL.Query()
	limit := 1000
	if q.Has("limit") {
		n, ok := parseUint(q.Get("limit"))
		if !ok || n < 1 || n > 1000 {
			return badParam("limit", "limit is 1 to 1000.")
		}
		limit = int(n)
	}
	desc := false
	switch q.Get("order") {
	case "", "asc":
	case "desc":
		desc = true
	default:
		return badParam("order", "order is asc or desc.")
	}
	after := q.Get("after")
	if after != "" {
		k, u, hasUser := strings.Cut(after, "/")
		if !keyRe.MatchString(k) || (hasUser && (!t.allUsers || !validVisitor(u))) {
			return badParam("after", "after takes the next value from a previous list.")
		}
	}
	items, next, version, err := s.store.list(t, after, limit, desc)
	if err != nil {
		return err
	}
	out := make([]itemJSON, 0, len(items))
	for _, e := range items {
		out = append(out, toItem(e))
	}
	var nx any
	if next != "" {
		nx = next
	}
	c.json(200, map[string]any{"items": out, "version": version, "next": nx})
	return nil
}

func (s *Server) watch(c *reqCtx, t target) error {
	if err := s.rate(s.lim.readsPerIP, c.ip, 1, "rate_limited"); err != nil {
		return err
	}
	q := c.r.URL.Query()
	timeout := 25 * time.Second
	if q.Has("timeout") {
		n, ok := parseUint(q.Get("timeout"))
		if !ok || n < 1 || n > 55 {
			return badParam("timeout", "timeout is 1 to 55 seconds.")
		}
		timeout = time.Duration(n) * time.Second
	}
	var from int64
	if q.Has("version") {
		n, ok := parseUint(q.Get("version"))
		if !ok {
			return badParam("version", "version is the version from your last list or watch response, or 0.")
		}
		from = n
	} else {
		seq, err := s.store.currentSeq(t.space)
		if err != nil {
			return err
		}
		from = seq
	}

	if !s.watches.acquire(c.ip) {
		return &apiError{Status: 429, Code: "too_many_watches", Message: "Too many open watches."}
	}
	defer s.watches.release(c.ip)
	s.notify.join(t.space)
	defer s.notify.leave(t.space)
	s.store.pin(t.space)
	defer s.store.unpin(t.space)
	s.store.touch(t.space)

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	woken := false
	for {
		sig := s.notify.subscribe(t.space)
		changes, seq, err := s.store.changes(t, from, 1000)
		if errors.Is(err, errVersionExpired) {
			return &apiError{Status: 410, Code: "version_expired", Message: "That version is too old or from elsewhere. Start again from version=0."}
		}
		if err != nil {
			return err
		}
		if len(changes) > 0 || from == 0 {
			if woken && len(changes) > 0 {
				// A burst of writes goes out together.
				time.Sleep(50 * time.Millisecond)
				woken = false
				continue
			}
			more := len(changes) > 1000
			if more {
				changes = changes[:1000]
			}
			out := make([]itemJSON, 0, len(changes))
			for _, e := range changes {
				out = append(out, toItem(e))
			}
			v := seq
			if len(changes) > 0 {
				v = max(seq, changes[len(changes)-1].Version)
			}
			if more {
				v = changes[len(changes)-1].Version
			}
			c.json(200, map[string]any{"changes": out, "version": v, "more": more})
			return nil
		}
		select {
		case <-sig:
			woken = true
		case <-deadline.C:
			seq, err := s.store.currentSeq(t.space)
			if err != nil {
				return err
			}
			c.json(200, map[string]any{"changes": []any{}, "version": max(seq, from), "more": false})
			return nil
		case <-c.r.Context().Done():
			return nil
		}
	}
}
