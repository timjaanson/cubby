// Package server is the HTTP API under /v1, the /kv.js and /health routes,
// rate limits, watches, and the admin handler on the Unix socket.
package server

import (
	"bytes"
	_ "embed"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/config"
	"github.com/timjaanson/cubby/internal/names"
	"github.com/timjaanson/cubby/internal/store"
	"github.com/timjaanson/cubby/internal/version"
	"github.com/timjaanson/cubby/web"
)

type Server struct {
	cfg     *config.Config
	store   *store.Store
	lim     *limits
	watches watchCounter
	notify  notifier
	log     *log.Logger
}

func New(cfg *config.Config, store *store.Store, logger *log.Logger) *Server {
	s := &Server{
		cfg: cfg, store: store, lim: newLimits(), log: logger,
		watches: watchCounter{perIP: map[string]int{}},
		notify:  notifier{spaces: map[string]*spaceSignal{}},
	}
	store.OnChange = s.notify.fire
	return s
}

// Cleanup runs the store's cleanup and forgets idle rate-limit buckets. The
// serve command calls it hourly; admin cleanup calls it on demand.
func (s *Server) Cleanup() (store.CleanupReport, error) {
	rep, err := s.store.Cleanup()
	for _, l := range s.lim.all() {
		l.prune()
	}
	s.lim.misses.prune()
	return rep, err
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
		http.ServeContent(sw, r, "kv.js", version.BuildTime, bytes.NewReader(web.KVJS))
		return
	case "/":
		h.Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(sw, "cubby %s\n\nAPI under /v1, JS client at /kv.js, health at /health.\n", version.Version)
		return
	}

	if !strings.HasPrefix(r.URL.EscapedPath(), "/v1/") {
		c.fail(&apierr.Error{Status: 404, Code: "not_found", Message: "No such route."})
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
	if tok != "" && !names.TokenRe.MatchString(tok) {
		c.tokErr = apierr.InvalidToken // malformed, no need to look it up
	} else if tok != "" {
		id, err := s.store.VisitorByToken(tok)
		if err != nil {
			c.fail(err)
			return
		}
		if id == "" {
			c.tokErr = apierr.InvalidToken
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
			return &apierr.Error{Status: 400, Code: "invalid_key", Message: "Bad escaping in the path."}
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
			if err := checkParams(c.r, "name", "label", "locked"); err != nil {
				return err
			}
			return s.createSpace(c)
		}
	}
	if len(seg) < 3 {
		return &apierr.Error{Status: 404, Code: "not_found", Message: "No such route. Paths look like /v1/{space}/shared/{key}."}
	}

	space := seg[0]
	if !names.ValidSpace(space) {
		return apierr.InvalidSpace(names.SuggestFor(space))
	}
	if s.cfg.RateLimitsOn() {
		if ok, retry := s.lim.misses.check(c.ip, space, func() bool { return s.store.SpaceExists(space) }); !ok {
			return apierr.RateLimited("miss_limit", retry)
		}
	}

	t := store.Target{Space: space}
	var rest []string
	switch seg[1] {
	case "shared":
		t.Area, rest = "s", seg[2:]
	case "me":
		id, err := c.needVisitor()
		if err != nil {
			return err
		}
		t.Area, rest = "m:"+id, seg[2:]
	case "users":
		if len(seg) < 4 {
			return &apierr.Error{Status: 404, Code: "not_found", Message: "No such route. Paths look like /v1/{space}/users/{id}/{key}."}
		}
		if seg[2] == "*" {
			t.AllUsers = true
		} else if names.ValidVisitor(seg[2]) {
			t.Area = "u:" + seg[2]
		} else {
			return &apierr.Error{Status: 400, Code: "invalid_key", Message: "Not a visitor ID. IDs look like u_k7m2x9q4p1r8t3w6."}
		}
		rest = seg[3:]
	default:
		return &apierr.Error{Status: 404, Code: "not_found", Message: "No such area. Use shared, users/{id} or me."}
	}

	// /{target} or /{target}/{action}, nothing after the action.
	action := ""
	switch {
	case len(rest) == 2 && names.ActionWords[rest[1]]:
		action = rest[1]
	case len(rest) == 2:
		return &apierr.Error{Status: 400, Code: "invalid_key", Message: "After a key or pattern, a path can only end in /watch or /series. Keys can't contain slashes."}
	case len(rest) != 1:
		return &apierr.Error{Status: 400, Code: "invalid_key", Message: "Keys can't contain slashes, and nothing comes after /watch or /series."}
	}
	name := rest[0]
	switch {
	case names.ValidPattern(name):
		t.Pattern, t.Key = true, strings.TrimSuffix(name, "*")
	case names.ValidKey(name):
		t.Key = name
	default:
		return &apierr.Error{Status: 400, Code: "invalid_key",
			Message: "A key is 1 to 128 of A-Z a-z 0-9 . _ : - and can't be \".\", \"..\", \"watch\" or \"series\". A pattern is a prefix followed by *."}
	}

	if action == "series" {
		if t.AllUsers {
			return &apierr.Error{Status: 400, Code: "invalid_key", Message: "Series can't be read or added across users/*/ yet. Use users/{id}/{key}/series."}
		}
		switch m {
		case http.MethodGet, http.MethodHead:
			if t.Pattern {
				return &apierr.Error{Status: 400, Code: "invalid_key", Message: "Series are read one key at a time. To see the newest point of each, GET the pattern without /series."}
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
	if t.AllUsers || t.Pattern {
		if !isRead {
			if m == http.MethodPost && t.Pattern && !t.AllUsers {
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
		return methodErrMsg("GET, HEAD, PUT, DELETE", "To add a point use "+t.Key+"/series. To replace the value use PUT.")
	}
	return methodErr("GET, HEAD, PUT, DELETE")
}
