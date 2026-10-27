package server

// A live connection is one WebSocket per space. A data message is an HTTP
// route in an envelope and runs through route() like any request, so access,
// limits and errors can't drift from HTTP. Watches are pushed instead of
// polled. Everything not about the data in the space stays HTTP only.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/timjaanson/cubby/internal/access"
	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/names"
	"github.com/timjaanson/cubby/internal/store"
)

const (
	liveAuthWaitDefault = 10 * time.Second // for the first message, without an Authorization header
	livePing            = 30 * time.Second // below the common 60 s proxy idle timeout
	livePingWait        = 15 * time.Second
	liveWriteWait       = 10 * time.Second // a client that can't take a frame this long is gone
	liveReadLimit       = 256 << 10        // a full series batch fits
	liveMaxWatches      = 20
)

// liveAuthWait is a variable so tests can shorten it.
var liveAuthWait = liveAuthWaitDefault

// Close codes besides the standard ones. 4000 + the HTTP status it matches.
const (
	closeAuthTimeout websocket.StatusCode = 4408
)

// Headers of an HTTP response that carry data. On a socket they go in the
// reply's headers object.
var liveHeaders = []string{"ETag", "KV-By", "KV-Time", "KV-Expires", "RateLimit", "RateLimit-Policy", "Retry-After", "Allow"}

type liveSet struct {
	mu    sync.Mutex
	conns map[*liveConn]struct{}
}

func (ls *liveSet) add(l *liveConn) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.conns == nil {
		ls.conns = map[*liveConn]struct{}{}
	}
	ls.conns[l] = struct{}{}
}

func (ls *liveSet) remove(l *liveConn) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	delete(ls.conns, l)
}

// CloseLive closes every live connection with 1001 going away, so clients
// reconnect at once. http.Server.Shutdown doesn't see hijacked connections.
func (s *Server) CloseLive() {
	s.live.mu.Lock()
	conns := make([]*liveConn, 0, len(s.live.conns))
	for l := range s.live.conns {
		conns = append(conns, l)
	}
	s.live.mu.Unlock()
	var wg sync.WaitGroup
	for _, l := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.conn.Close(websocket.StatusGoingAway, "Server restarting.")
			l.cancel()
		}()
	}
	wg.Wait()
}

type liveConn struct {
	s      *Server
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	ip     string
	addr   string
	space  string

	mu      sync.Mutex
	visitor string
	tokErr  *apierr.Error
	watches map[string]*liveWatch
}

type liveWatch struct {
	cancel context.CancelFunc
	me     bool // watches the caller's own me/ area
}

type liveIn struct {
	ID     json.RawMessage            `json:"id"`
	Op     string                     `json:"op"`
	Method string                     `json:"method"`
	Path   string                     `json:"path"`
	Query  map[string]json.RawMessage `json:"query"`
	Body   json.RawMessage            `json:"body"`
	Text   *string                    `json:"text"`
	Token  *string                    `json:"token"`
	Watch  json.RawMessage            `json:"watch"`
}

type liveOut struct {
	ID      json.RawMessage   `json:"id,omitempty"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
	Text    bool              `json:"text,omitempty"`
}

func (s *Server) liveRoute(c *reqCtx, space string) error {
	if c.r.Method != http.MethodGet {
		return methodErr("GET")
	}
	if c.r.URL.Query().Has("token") {
		return apierr.BadParam("token", "A live connection doesn't take a token in the URL. Send {\"op\":\"auth\",\"token\":\"vt_...\"} as the first message, or an Authorization header.")
	}
	if err := checkParams(c.r); err != nil {
		return err
	}
	if !strings.EqualFold(c.r.Header.Get("Upgrade"), "websocket") {
		return &apierr.Error{Status: 426, Code: "upgrade_required", Message: "This route is a WebSocket. Connect with a WebSocket client."}
	}
	// Only a header can have set these. Clients that send headers can read
	// the HTTP status, so a bad one fails before the upgrade.
	if c.tokErr != nil {
		return c.tokErr
	}
	// A live connection counts as one open watch, however many it carries.
	if !s.watches.acquire(c.ip) {
		return &apierr.Error{Status: 429, Code: "too_many_watches", Message: "Too many open watches and live connections from this address."}
	}
	defer s.watches.release(c.ip)

	sw := c.w.(*statusWriter)
	conn, err := websocket.Accept(sw.ResponseWriter, c.r, &websocket.AcceptOptions{
		// Pages on any origin may connect. Tokens, not cookies, carry
		// identity, so there's nothing for another site to borrow.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		sw.status = 400 // Accept wrote the response
		return nil
	}
	sw.status = 101

	// The request's context ends when the handler returns; the connection
	// gets its own.
	ctx, cancel := context.WithCancel(context.Background())
	l := &liveConn{s: s, conn: conn, ctx: ctx, cancel: cancel, ip: c.ip, space: space,
		addr: s.clientIP(c.r).String(), visitor: c.visitor, watches: map[string]*liveWatch{}}
	s.live.add(l)
	defer s.live.remove(l)
	l.run(c.visitor != "")
	c.visitor = l.currentVisitor() // for the access log line
	return nil
}

func (l *liveConn) currentVisitor() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.visitor
}

func (l *liveConn) run(settled bool) {
	defer func() {
		l.cancel()
		l.conn.Close(websocket.StatusNormalClosure, "")
	}()
	l.conn.SetReadLimit(liveReadLimit)
	go l.keepalive()
	var authTimer *time.Timer
	if !settled {
		authTimer = time.AfterFunc(liveAuthWait, func() {
			l.conn.Close(closeAuthTimeout, "Send a first message within 10 seconds, usually auth.")
			l.cancel()
		})
	}
	for {
		typ, data, err := l.conn.Read(l.ctx)
		if err != nil {
			return
		}
		if authTimer != nil {
			if !authTimer.Stop() {
				return // closed for silence just now
			}
			authTimer = nil
		}
		if typ != websocket.MessageText {
			l.conn.Close(websocket.StatusUnsupportedData, "Messages are JSON text.")
			return
		}
		l.handle(data)
	}
}

func (l *liveConn) keepalive() {
	t := time.NewTicker(livePing)
	defer t.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(l.ctx, livePingWait)
			err := l.conn.Ping(ctx)
			cancel()
			if err != nil {
				l.cancel()
				l.conn.CloseNow()
				return
			}
		}
	}
}

// encode writes the envelope by hand so body keeps its bytes: json.Marshal
// would compact a stored value, and values are never reformatted.
func (out liveOut) encode() []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	if out.ID != nil {
		b.WriteString(`"id":`)
		b.Write(out.ID)
		b.WriteByte(',')
	}
	b.WriteString(`"status":` + strconv.Itoa(out.Status))
	if len(out.Headers) > 0 {
		b.WriteString(`,"headers":` + jsonText(out.Headers))
	}
	if out.Body != nil {
		b.WriteString(`,"body":`)
		b.Write(out.Body)
	}
	if out.Text {
		b.WriteString(`,"text":true`)
	}
	b.WriteByte('}')
	return b.Bytes()
}

func (l *liveConn) send(out liveOut) error {
	b := out.encode()
	ctx, cancel := context.WithTimeout(l.ctx, liveWriteWait)
	defer cancel()
	if err := l.conn.Write(ctx, websocket.MessageText, b); err != nil {
		l.cancel()
		return err
	}
	return nil
}

func (l *liveConn) sendErr(id json.RawMessage, err error) {
	ae, body := apiErrBody(err)
	b, _ := json.Marshal(body)
	out := liveOut{ID: id, Status: ae.Status, Body: b}
	if ae.Status == 429 {
		out.Headers = map[string]string{"Retry-After": jsonText(ae.Extra["retryAfter"])}
	}
	l.send(out)
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func badMessage(msg string) *apierr.Error {
	return &apierr.Error{Status: 400, Code: "bad_message", Message: msg}
}

// validID: a request id is a JSON number or string, echoed back as sent.
func validID(id json.RawMessage) bool {
	if len(id) == 0 || len(id) > 100 {
		return false
	}
	switch id[0] {
	case '"', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return true
	}
	return false
}

// charge takes one read for a message that doesn't reach route. false:
// rate limited, and the reply is sent.
func (l *liveConn) charge(id json.RawMessage) bool {
	if err := l.s.rate(l.s.lim.readsPerIP, l.ip, 1, "rate_limited"); err != nil {
		l.sendErr(id, err)
		return false
	}
	return true
}

func (l *liveConn) handle(data []byte) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m liveIn
	err := dec.Decode(&m)
	if err == nil && dec.More() {
		err = errors.New("trailing data")
	}
	if err != nil {
		// Echo the id if it can be found, so the caller's request doesn't hang.
		var loose struct {
			ID json.RawMessage `json:"id"`
		}
		json.Unmarshal(data, &loose)
		if !validID(loose.ID) {
			loose.ID = nil
		}
		if !l.charge(loose.ID) {
			return
		}
		l.sendErr(loose.ID, badMessage("A message is one JSON object: {\"id\",\"method\",\"path\",\"query\",\"body\" or \"text\"} for data, or {\"op\":\"auth\"|\"ping\"|\"stop\"}. "+err.Error()))
		return
	}
	if m.ID != nil && !validID(m.ID) {
		l.sendErr(nil, badMessage("id is a number or a string."))
		return
	}

	// A data message pays what its HTTP request pays, inside route. Other
	// messages, and data messages refused before route, cost one read.
	if m.Method == "" && m.Op != "stop" && !l.charge(m.ID) {
		return
	}
	switch {
	case m.Op != "" && m.Method != "":
		l.sendErr(m.ID, badMessage("A message has either op or method, not both."))
	case m.Op == "auth":
		l.auth(m)
	case m.Op == "ping":
		l.send(liveOut{ID: m.ID, Status: 200, Body: json.RawMessage(`{}`)})
	case m.Op == "stop":
		l.stop(m)
	case m.Op != "":
		l.sendErr(m.ID, badMessage("op is auth, ping or stop."))
	case m.Method != "":
		l.request(m)
	default:
		l.sendErr(m.ID, badMessage("A message needs method (GET, PUT, DELETE, POST) or op (auth, ping, stop)."))
	}
}

// auth sets who the following messages come from. It can come at any time
// and as often as needed: a header and an auth message together are fine.
// A bad token behaves like a bad token on HTTP: reads go on without a
// visitor and writes answer invalid_token.
func (l *liveConn) auth(m liveIn) {
	tok := ""
	if m.Token != nil {
		tok = *m.Token
	}
	visitor := ""
	var tokErr *apierr.Error
	switch {
	case tok == "":
	case !names.VisitorTokenRe.MatchString(tok):
		tokErr = apierr.InvalidToken
	default:
		id, err := l.s.store.VisitorByToken(tok)
		if err != nil {
			l.sendErr(m.ID, err)
			return
		}
		if id == "" {
			tokErr = apierr.InvalidToken
		}
		visitor = id
	}

	l.mu.Lock()
	changed := visitor != l.visitor
	l.visitor, l.tokErr = visitor, tokErr
	var ended []string
	if changed {
		// me/ meant the previous visitor's area. Those watches end; the
		// client starts them again from their last version.
		for id, w := range l.watches {
			if w.me {
				w.cancel()
				delete(l.watches, id)
				ended = append(ended, id)
			}
		}
	}
	l.mu.Unlock()
	for _, id := range ended {
		l.sendErr(json.RawMessage(id), &apierr.Error{Status: 409, Code: "identity_changed",
			Message: "This connection authenticated as another visitor, so its me/ watches ended. Start them again from their last version."})
	}

	if tokErr != nil {
		l.sendErr(m.ID, tokErr)
		return
	}
	var who any
	if visitor != "" {
		who = visitor
	}
	l.send(liveOut{ID: m.ID, Status: 200, Body: json.RawMessage(jsonText(map[string]any{"id": who}))})
}

func (l *liveConn) stop(m liveIn) {
	if m.Watch == nil {
		l.sendErr(m.ID, badMessage("stop takes watch: the id of the watch request."))
		return
	}
	l.mu.Lock()
	if w := l.watches[string(m.Watch)]; w != nil {
		w.cancel()
		delete(l.watches, string(m.Watch))
	}
	l.mu.Unlock()
	if m.ID != nil {
		l.send(liveOut{ID: m.ID, Status: 200, Body: json.RawMessage(`{}`)})
	}
}

// recorder captures what a handler writes, for the reply envelope.
type recorder struct {
	h      http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.h }
func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = 200
	}
	return r.body.Write(b)
}
func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}

func (l *liveConn) request(m liveIn) {
	start := time.Now()
	if m.ID == nil {
		if l.charge(nil) {
			l.sendErr(nil, badMessage("A request needs an id. The reply carries the same id."))
		}
		return
	}
	var err error
	method := m.Method
	switch method {
	case http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPost:
	default:
		err = badMessage("method is GET, PUT, DELETE or POST.")
	}
	area, _, _ := strings.Cut(m.Path, "/")
	if err == nil && area != "shared" && area != "me" && area != "users" {
		err = badMessage("path starts with shared/, me/ or users/, as in /v1/{space}/{path}. Everything else about a space is HTTP only.")
	}
	if err == nil && strings.ContainsAny(m.Path, "?#") {
		err = badMessage("path holds no query string; parameters go in query.")
	}
	var body []byte
	if err == nil {
		takesBody := method == http.MethodPut || method == http.MethodPost
		switch {
		case m.Body != nil && m.Text != nil:
			err = badMessage("Give body (a JSON value) or text (stored as text), not both.")
		case !takesBody && (m.Body != nil || m.Text != nil):
			err = badMessage("Only PUT and POST take a body.")
		case m.Body != nil:
			body = m.Body
		case m.Text != nil:
			body = []byte(*m.Text)
		}
	}
	q := url.Values{}
	for k, raw := range m.Query {
		if err != nil {
			break
		}
		if k == "token" {
			err = apierr.BadParam("token", "On a live connection the token comes from auth, not from query.")
			break
		}
		var v string
		switch {
		case len(raw) > 0 && raw[0] == '"':
			json.Unmarshal(raw, &v)
		case len(raw) > 0 && (raw[0] == '-' || (raw[0] >= '0' && raw[0] <= '9') || raw[0] == 't' || raw[0] == 'f'):
			v = string(raw)
		default:
			err = apierr.BadParam(k, "Query values are strings, numbers or booleans.")
		}
		q.Set(k, v)
	}
	if err != nil {
		if l.charge(m.ID) {
			l.sendErr(m.ID, err)
		}
		return
	}

	target := "/v1/" + l.space + "/" + m.Path
	r, rerr := http.NewRequestWithContext(l.ctx, method, "http://live"+target+"?"+q.Encode(), bytes.NewReader(body))
	if rerr != nil {
		if l.charge(m.ID) {
			l.sendErr(m.ID, badMessage("path isn't a valid path."))
		}
		return
	}
	rec := &recorder{h: http.Header{}}
	l.mu.Lock()
	visitor, tokErr := l.visitor, l.tokErr
	l.mu.Unlock()
	c := &reqCtx{w: &statusWriter{ResponseWriter: rec, status: 200}, r: r, ip: l.ip, visitor: visitor, tokErr: tokErr, live: l, liveID: m.ID}
	if err := l.s.route(c); err != nil {
		c.fail(err)
	}
	if l.s.log != nil {
		status := rec.status
		if c.liveStarted {
			status = 101
		}
		l.s.log.Printf("WS %s %s %d %s %s %s", method, r.URL.EscapedPath(), status,
			time.Since(start).Round(time.Millisecond), l.addr, shortHash(visitor))
	}
	if c.liveStarted {
		return // the watch sends its own frames
	}
	out := liveOut{ID: m.ID, Status: rec.status}
	for _, h := range liveHeaders {
		if v := rec.h.Get(h); v != "" {
			if out.Headers == nil {
				out.Headers = map[string]string{}
			}
			out.Headers[h] = v
		}
	}
	if b := rec.body.Bytes(); len(b) > 0 {
		if strings.HasPrefix(rec.h.Get("Content-Type"), "text/plain") {
			out.Text = true
			out.Body = json.RawMessage(jsonText(string(b)))
		} else {
			out.Body = bytes.TrimRight(b, "\n")
		}
	}
	l.send(out)
}

func (l *liveConn) startWatch(c *reqCtx, t store.Target, from int64) error {
	key := string(c.liveID)
	l.mu.Lock()
	if l.watches[key] != nil {
		l.mu.Unlock()
		return &apierr.Error{Status: 400, Code: "id_in_use", Message: "A watch with this id is running. Stop it first, or use a new id."}
	}
	if len(l.watches) >= liveMaxWatches {
		l.mu.Unlock()
		return &apierr.Error{Status: 429, Code: "too_many_watches", Message: "At most 20 watches run on one live connection."}
	}
	ctx, cancel := context.WithCancel(l.ctx)
	w := &liveWatch{cancel: cancel, me: strings.HasPrefix(t.Area, "m:")}
	l.watches[key] = w
	l.mu.Unlock()
	c.liveStarted = true
	go l.runWatch(ctx, w, c.liveID, t, c.op, from)
	return nil
}

// startPulses runs a pulse listener on the connection. Like a watch it sends
// a first frame at once, then one whenever matching pulses arrive. It never
// waits to batch: on a socket each pulse goes out as soon as it's sent.
func (l *liveConn) startPulses(c *reqCtx, t store.Target, h *pulseHub, cursor int64, missed bool) error {
	key := string(c.liveID)
	l.mu.Lock()
	if l.watches[key] != nil {
		l.mu.Unlock()
		return &apierr.Error{Status: 400, Code: "id_in_use", Message: "A watch or listener with this id is running. Stop it first, or use a new id."}
	}
	if len(l.watches) >= liveMaxWatches {
		l.mu.Unlock()
		return &apierr.Error{Status: 429, Code: "too_many_watches", Message: "At most 20 watches and listeners run on one live connection."}
	}
	ctx, cancel := context.WithCancel(l.ctx)
	w := &liveWatch{cancel: cancel, me: strings.HasPrefix(t.Area, "m:")}
	l.watches[key] = w
	l.mu.Unlock()
	c.liveStarted = true
	go l.runPulses(ctx, w, c.liveID, t, c.op, h, cursor, missed)
	return nil
}

func (l *liveConn) runPulses(ctx context.Context, w *liveWatch, id json.RawMessage, t store.Target, op access.Op, h *pulseHub, cursor int64, missed bool) {
	defer l.endWatch(string(id), w)
	h.listen(1)
	defer h.listen(-1)
	match := pulseMatch(t)
	first := true
	for {
		sig := h.wait()
		out, next, lost := h.since(cursor, match)
		if len(out) > 0 || lost || missed || first {
			if !first && !l.allowed(t.Space, op) {
				if ctx.Err() == nil {
					l.sendErr(id, apierr.NoSpace)
				}
				return
			}
			if ctx.Err() != nil {
				return
			}
			body := map[string]any{"pulses": nonNil(out), "seq": next, "missed": lost || missed}
			if l.send(liveOut{ID: id, Status: 200, Body: json.RawMessage(jsonText(body))}) != nil {
				return
			}
			first, missed = false, false
		}
		cursor = next
		select {
		case <-sig:
		case <-ctx.Done():
			return
		}
	}
}

func (l *liveConn) endWatch(key string, w *liveWatch) {
	l.mu.Lock()
	if l.watches[key] == w {
		delete(l.watches, key)
	}
	l.mu.Unlock()
	w.cancel()
}

// runWatch pushes the same bodies an HTTP watch returns, one after another.
// The first goes out at once, even with no changes, so the client knows the
// watch is running and where it stands. A slow client loses nothing: each
// push carries everything since the last one, merged per key.
func (l *liveConn) runWatch(ctx context.Context, w *liveWatch, id json.RawMessage, t store.Target, op access.Op, from int64) {
	s := l.s
	defer l.endWatch(string(id), w)
	s.notify.join(t.Space)
	defer s.notify.leave(t.Space)
	s.store.Pin(t.Space)
	defer s.store.Unpin(t.Space)
	s.store.Touch(t.Space)

	fail := func(err error) {
		if ctx.Err() == nil {
			l.sendErr(id, err)
		}
	}
	first, woken := true, false
	for {
		sig := s.notify.subscribe(t.Space)
		changes, seq, err := s.store.Changes(t, from, 1000)
		if errors.Is(err, store.ErrVersionExpired) {
			fail(errVersionExpired)
			return
		}
		if err != nil {
			fail(err)
			return
		}
		if len(changes) > 0 || first {
			if woken && len(changes) > 0 && s.notify.burst(t.Space) {
				select {
				case <-time.After(50 * time.Millisecond):
				case <-ctx.Done():
					return
				}
				woken = false
				continue
			}
			// Rights can change while a watch runs: a grant ends, the
			// connection authenticates as someone else. Check as a new
			// request would.
			if !first && !l.allowed(t.Space, op) {
				fail(apierr.NoSpace)
				return
			}
			body, next := watchBatch(changes, seq, from)
			if ctx.Err() != nil {
				return
			}
			if l.send(liveOut{ID: id, Status: 200, Body: json.RawMessage(jsonText(body))}) != nil {
				return
			}
			from, first = next, false
			if more, _ := body["more"].(bool); more {
				continue
			}
		}
		select {
		case <-sig:
			woken = true
		case <-ctx.Done():
			return
		}
	}
}

func (l *liveConn) allowed(space string, op access.Op) bool {
	rights, err := l.s.store.Rights(space, l.currentVisitor())
	return err == nil && rights != nil && rights.Visible() && rights.Allows(op)
}
