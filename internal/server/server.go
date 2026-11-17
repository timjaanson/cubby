// Package server is the HTTP API under /v1, the /kv.js, /share.js and /health routes,
// rate limits, watches, and the admin handler on the Unix socket.
package server

import (
	"bytes"
	_ "embed"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/timjaanson/cubby/internal/access"
	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/config"
	"github.com/timjaanson/cubby/internal/names"
	"github.com/timjaanson/cubby/internal/store"
	"github.com/timjaanson/cubby/internal/version"
	"github.com/timjaanson/cubby/web"
)

type Server struct {
	cfg      *config.Config
	store    *store.Store
	lim      *limits
	watches  watchCounter
	notify   notifier
	log      *log.Logger
	live     liveSet
	pulses   *pulseHubs
	pairings pairingWaits
}

func New(cfg *config.Config, store *store.Store, logger *log.Logger) *Server {
	s := &Server{
		cfg: cfg, store: store, lim: newLimits(cfg), log: logger,
		watches: watchCounter{perIP: map[string]int{}, perIPLimit: cfg.Limits.WatchesPerIP, totalLimit: cfg.Limits.WatchesTotal},
		notify:  notifier{spaces: map[string]*spaceSignal{}},
		pulses:  newPulseHubs(),
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
		h.Set("Access-Control-Allow-Methods", "GET, HEAD, PUT, PATCH, DELETE, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, If-Match, If-None-Match")
		h.Set("Access-Control-Max-Age", "86400")
		sw.WriteHeader(204)
		return
	}

	switch r.URL.Path {
	case "/health":
		c.json(200, map[string]any{"ok": true, "version": version.Version})
		return
	case "/kv.js":
		h.Set("Content-Type", "text/javascript; charset=utf-8")
		h.Set("Cache-Control", "public, max-age=3600")
		h.Set("Access-Control-Allow-Origin", "*")
		http.ServeContent(sw, r, "kv.js", version.BuildTime, bytes.NewReader(web.KVJS))
		return
	case "/share.js":
		h.Set("Content-Type", "text/javascript; charset=utf-8")
		h.Set("Cache-Control", "public, max-age=3600")
		h.Set("Access-Control-Allow-Origin", "*")
		http.ServeContent(sw, r, "share.js", version.BuildTime, bytes.NewReader(web.ShareJS))
		return
	}

	if !strings.HasPrefix(r.URL.EscapedPath(), "/v1/") {
		c.fail(&apierr.Error{Status: 404, Code: "not_found", Message: "No such route."})
		return
	}
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Expose-Headers", "ETag, Retry-After, KV-Time, KV-By, KV-Expires, RateLimit, RateLimit-Policy")
	h.Set("Cache-Control", "no-store")

	in, err := s.signIn(r, credential(r))
	if err != nil {
		c.fail(err)
		return
	}
	c.visitor, c.device, c.apiKey, c.token, c.tokErr = in.visitor, in.device, in.apiKey, in.token, in.err

	if err := s.route(c); err != nil {
		c.fail(err)
	}
}

var noRoute = &apierr.Error{Status: 404, Code: "not_found", Message: "No such route."}

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
		case "limits", "time":
			if m != http.MethodGet && m != http.MethodHead {
				return methodErr("GET, HEAD")
			}
			if err := checkParams(c.r); err != nil {
				return err
			}
			if err := s.rate(s.lim.readsPerIP, c.ip, 1, "rate_limited"); err != nil {
				return err
			}
			if seg[0] == "time" {
				c.json(200, map[string]any{"t": store.NowMs()})
			} else {
				c.json(200, s.serverLimits())
			}
			return nil
		case "visitors":
			if m != http.MethodPost {
				return methodErr("POST")
			}
			if err := checkParams(c.r); err != nil {
				return err
			}
			return s.registerVisitor(c)
		case "pair":
			if m != http.MethodPost {
				return methodErrMsg("POST", "POST the pairing code here, as the body or as {\"code\": \"...\"}, signed by the new device's key.")
			}
			if err := checkParams(c.r); err != nil {
				return err
			}
			return s.pair(c)
		case "spaces":
			if m != http.MethodPost && m != http.MethodGet && m != http.MethodHead {
				return methodErr("GET, HEAD, POST")
			}
			if c.apiKey != nil {
				return apierr.APIKeyNotAllowed
			}
			if m != http.MethodPost {
				if err := checkParams(c.r, "limit", "after"); err != nil {
					return err
				}
				return s.listSpaces(c)
			}
			if err := checkParams(c.r); err != nil {
				return err
			}
			return s.createSpace(c)
		}
	}

	// A credential that was sent but refused is a 401 everywhere from here,
	// reads of open spaces too. Quietly reading as a stranger would hide an
	// expired token or a signed-out device behind no_space, and the client
	// would never learn to fix its clock or register again.
	if c.tokErr != nil {
		return c.tokErr
	}

	// /me and what's under it belong to the visitor, not to a space. Space
	// names are at least 16 characters, so it can't be one.
	if seg[0] == "me" {
		return s.meRoute(c, seg[1:])
	}

	// The shape of the path first, before anything about the space, so an
	// unknown route is the same bare 404 whatever the name.
	if len(seg) >= 2 {
		switch seg[1] {
		case "shared":
			if len(seg) < 3 {
				return noRoute
			}
		case "by", "to":
			if len(seg) < 4 {
				return noRoute
			}
		case "inbox":
			if len(seg) < 4 || seg[2] == "owners" && len(seg) < 5 {
				return noRoute
			}
		case "access", "join", "live", "batch":
			if len(seg) != 2 {
				return noRoute
			}
		case "invites", "members":
			if len(seg) > 3 {
				return noRoute
			}
		default:
			return noRoute
		}
	}
	space := seg[0]
	if !names.ValidSpace(space) {
		if len(seg) == 1 {
			return noRoute
		}
		return apierr.InvalidSpace(names.SuggestFor(space))
	}
	if c.apiKey != nil {
		if space != c.apiKey.Space {
			return apierr.APIKeySpace(c.apiKey.Space)
		}
		// Data and /access only: an API key never joins, invites, manages
		// or holds a live connection.
		if len(seg) == 1 || seg[1] == "join" || seg[1] == "invites" || seg[1] == "members" || seg[1] == "live" {
			return apierr.APIKeyNotAllowed
		}
	}
	if len(seg) == 2 && seg[1] == "live" {
		if c.live != nil {
			return noRoute
		}
		// Access is checked per message, not here: a page may connect
		// before it joins, and a grant can arrive or end mid-connection.
		return s.liveRoute(c, space)
	}
	rights, err := s.store.Rights(space, c.visitor)
	if err != nil {
		return err
	}
	if rights != nil && c.apiKey != nil {
		rights.Cap = &c.apiKey.Access
	}
	// A ban is said plainly, on every route, before anything else: the
	// visitor was let in once, so the space's existence is no secret.
	if rights != nil && rights.Banned {
		return apierr.Banned(rights.BannedUntil)
	}

	// join decides about misses itself: it only costs one when it fails.
	if len(seg) == 2 && seg[1] == "join" {
		if m != http.MethodPost {
			return methodErr("POST")
		}
		if err := checkParams(c.r); err != nil {
			return err
		}
		return s.join(c, space, rights)
	}

	// A space this request can't see, missing or private, answers the same
	// way and costs the same miss.
	visible := rights != nil && rights.Visible()
	if s.cfg.RateLimitsOn() {
		if ok, retry := s.lim.misses.check(c.ip, space, func() bool { return visible }); !ok {
			return apierr.RateLimited("miss_limit", retry)
		}
	}
	if !visible {
		return apierr.NoSpace
	}

	if len(seg) == 1 || seg[1] == "access" || seg[1] == "invites" || seg[1] == "members" {
		return s.spaceRoute(c, space, rights, seg[1:])
	}
	if seg[1] == "batch" {
		if c.collect != nil {
			return badBatch("A batch can't hold a batch.")
		}
		return s.batch(c, space)
	}
	t := store.Target{Space: space}
	op := access.Op{}
	var rest []string
	inbox := false // the recipient's side of to/: read and delete only
	toRcpt := ""   // the visitor a to/ write goes to, checked to exist
	badID := func(what string) error {
		return &apierr.Error{Status: 400, Code: "invalid_key", Message: what}
	}
	switch seg[1] {
	case "shared":
		t.Area, op.Area, rest = "s", "shared", seg[2:]
	case "by":
		switch id := seg[2]; {
		case id == "*":
			t.Across = store.AllBy
		case id == "me":
			me, err := c.needVisitor()
			if err != nil {
				return err
			}
			t.Area, t.Self = store.ByArea(me), true
		case names.ValidVisitor(id):
			t.Area = store.ByArea(id)
		default:
			return badID("Not a visitor ID. IDs look like u_k7m2x9q4p1r8t3w6, or use me for your own.")
		}
		op.Area, rest = "by", seg[3:]
	case "to":
		me, err := c.needVisitor()
		if err != nil {
			return err
		}
		op.Area, t.Self = "to", true
		switch rc := seg[2]; {
		case rc == "*":
			t.Across, t.Fixed = store.Sent, me
		case rc == "me" || rc == me:
			t.Area = store.ToArea(me, me)
		case rc == "owners":
			t.Area = store.ToArea(rc, me)
		case names.ValidVisitor(rc):
			t.Area, toRcpt = store.ToArea(rc, me), rc
		default:
			return badID("After to/ comes the recipient: a visitor ID, me, owners, or * to list what you sent.")
		}
		rest = seg[3:]
	case "inbox":
		me, err := c.needVisitor()
		if err != nil {
			return err
		}
		inbox = true
		op.Area, t.Self = "to", true
		rcpt, i := me, 2
		if seg[2] == "owners" {
			if !rights.Owner() {
				return apierr.OwnerOnly
			}
			rcpt, i = "owners", 3
		}
		switch from := seg[i]; {
		case from == "*":
			t.Across, t.Fixed = store.Inbox, rcpt
		case from == "me":
			t.Area = store.ToArea(rcpt, me)
		case names.ValidVisitor(from):
			t.Area = store.ToArea(rcpt, from)
		default:
			return badID("After inbox/ comes the sender: a visitor ID, me, or * for everyone. Mail to the owners is under inbox/owners/.")
		}
		rest = seg[i+1:]
	default:
		return noRoute
	}

	// /{target} or /{target}/{action}, nothing after the action.
	action := ""
	switch {
	case len(rest) == 2 && names.ActionWords[rest[1]]:
		action = rest[1]
	case len(rest) == 2:
		return &apierr.Error{Status: 400, Code: "invalid_key", Message: "After a key or pattern, a path can only end in /watch, /series or /pulse. Keys can't contain slashes."}
	case len(rest) != 1:
		return &apierr.Error{Status: 400, Code: "invalid_key", Message: "Keys can't contain slashes, and nothing comes after /watch, /series or /pulse."}
	}
	name := rest[0]
	switch {
	case names.ValidPattern(name):
		t.Pattern, t.Key = true, strings.TrimSuffix(name, "*")
	case names.ValidKey(name):
		t.Key = name
	default:
		return &apierr.Error{Status: 400, Code: "invalid_key",
			Message: "A key is 1 to 128 of A-Z a-z 0-9 . _ : - and can't be \".\", \"..\", \"watch\", \"series\" or \"pulse\". A pattern is a prefix followed by *."}
	}

	// What the method does, then whether this visitor may.
	type handler func(*reqCtx, store.Target) error
	var h handler
	isRead := m == http.MethodGet || m == http.MethodHead
	switch {
	case inbox && !isRead && !(m == http.MethodDelete && action == ""):
		return methodErrMsg("GET, HEAD, DELETE", "inbox/ is what others sent you: read it, or delete from it. To send, use to/{recipient}/.")
	case action == "series":
		if t.Many() {
			return &apierr.Error{Status: 400, Code: "invalid_key", Message: "Series can't be read or added across several authors or senders yet. Name one: by/{id}/{key}/series, inbox/{sender}/{key}/series."}
		}
		switch {
		case isRead:
			if t.Pattern {
				return &apierr.Error{Status: 400, Code: "invalid_key", Message: "Series are read one key at a time. To see the newest point of each, GET the pattern without /series."}
			}
			if err := checkParams(c.r, "since", "until", "order", "limit"); err != nil {
				return err
			}
			h = s.seriesRead
		case m == http.MethodPost:
			if err := checkParams(c.r, "t", "ttl", "ifBefore"); err != nil {
				return err
			}
			h = s.seriesAdd
		default:
			return methodErrMsg("GET, HEAD, POST", "A series takes POST to add points and GET to read them. To replace or delete the key, use the path without /series.")
		}
	case action == "pulse":
		switch {
		case isRead:
			if err := checkParams(c.r, "after", "timeout"); err != nil {
				return err
			}
			h = s.pulseRead
		case m == http.MethodPost:
			if t.Pattern || t.Many() {
				return &apierr.Error{Status: 400, Code: "invalid_key", Message: "A pulse goes to one key. Listen with a pattern, send to a key."}
			}
			if err := checkParams(c.r); err != nil {
				return err
			}
			h = s.pulseSend
		default:
			return methodErrMsg("GET, POST", "POST sends a pulse, GET listens. Pulses aren't stored, so there's nothing to replace or delete.")
		}
	case action == "watch":
		if m != http.MethodGet {
			return methodErr("GET")
		}
		if err := checkParams(c.r, "version", "timeout"); err != nil {
			return err
		}
		h = s.watch
	case t.Many() || t.Pattern:
		if !isRead {
			if m == http.MethodPost && t.Pattern && !t.Many() {
				return methodErrMsg("GET, HEAD", "To add a point to each of several series, POST to "+name+"/series.")
			}
			return methodErrMsg("GET, HEAD", "Patterns, by/*/, to/*/ and inbox/*/ are read-only. Write and delete one key at a time.")
		}
		if err := checkParams(c.r, "limit", "order", "after"); err != nil {
			return err
		}
		h = s.list
	case isRead:
		if err := checkParams(c.r); err != nil {
			return err
		}
		h = s.read
	case m == http.MethodPut || m == http.MethodDelete:
		params := []string{"ifVersion", "ifBefore"}
		if m == http.MethodPut {
			params = append(params, "ttl")
		}
		if err := checkParams(c.r, params...); err != nil {
			return err
		}
		h = s.write
	case m == http.MethodPost:
		return methodErrMsg("GET, HEAD, PUT, DELETE", "To add a point use "+t.Key+"/series. To replace the value use PUT.")
	default:
		return methodErr("GET, HEAD, PUT, DELETE")
	}

	op.Write = !isRead
	if op.Write {
		id, err := c.needVisitor()
		if err != nil {
			return err
		}
		if author, ok := strings.CutPrefix(t.Area, "b:"); ok && author != id {
			// Owners may delete anyone's entries, as moderation. Nobody
			// writes them.
			if !(rights.Owner() && m == http.MethodDelete) {
				return apierr.NotYours(author)
			}
		} else if !rights.Allows(op) {
			return apierr.NoAccess(op.Need())
		}
		// A to/ slot is sent to someone who exists. Deleting needs no check:
		// the recipient may be gone and the sender still cleaning up.
		if toRcpt != "" && m != http.MethodDelete && !s.store.VisitorExists(toRcpt) {
			return &apierr.Error{Status: 404, Code: "no_visitor", Message: "No visitor has the ID " + toRcpt + "."}
		}
	}
	c.op = op
	return h(c, t)
}
