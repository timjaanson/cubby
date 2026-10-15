package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/names"
	"github.com/timjaanson/cubby/internal/store"
)

func (s *Server) anon(c *reqCtx) error {
	if err := s.rate(s.lim.anonPerIP, c.ip, 1, "rate_limited"); err != nil {
		return err
	}
	tok, id, err := s.store.NewVisitor()
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
	label, name := q.Get("label"), q.Get("name")
	if label != "" && name != "" {
		return apierr.BadParam("name", "Give a name or a label, not both.")
	}
	if label != "" && (!names.LabelRe.MatchString(label) || len(label) > 54) {
		return apierr.BadParam("label", "A label is lowercase letters, digits and single hyphens, at most 54 characters.")
	}
	if name != "" && !names.ValidSpace(name) {
		return apierr.InvalidSpace(names.SuggestFor(name))
	}
	if name != "" && s.store.SpaceExists(name) {
		return apierr.SpaceExists(name) // before the rate limit, so a taken name costs nothing
	}
	locked := false
	if q.Has("locked") {
		switch q.Get("locked") {
		case "", "1", "true":
			locked = true
		case "0", "false":
		default:
			return apierr.BadParam("locked", "locked takes no value, or true or false.")
		}
	}
	if err := s.spaceRate(c, id); err != nil {
		return err
	}
	var key string
	if name != "" {
		key, err = s.store.CreateNamedSpace(name, locked)
	} else {
		name, key, err = s.store.CreateSpace(label, locked)
	}
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

func valueHeaders(h http.Header, e *store.Entry) {
	if e.IsJSON {
		h.Set("Content-Type", "application/json; charset=utf-8")
	} else {
		h.Set("Content-Type", "text/plain; charset=utf-8")
	}
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
}

func etag(v int64) string { return `"` + strconv.FormatInt(v, 10) + `"` }

func (s *Server) read(c *reqCtx, t store.Target) error {
	if err := s.rate(s.lim.readsPerIP, c.ip, 1, "rate_limited"); err != nil {
		return err
	}
	e, err := s.store.Get(t.Space, t.Area, t.Key)
	if err != nil {
		return err
	}
	if e == nil {
		return apierr.NotFound
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
		return nil, apierr.BadParam("ifVersion", "ifVersion must be a version number, or 0 for \"only if it doesn't exist\".")
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

func (s *Server) write(c *reqCtx, t store.Target) error {
	id, err := c.needVisitor()
	if err != nil {
		return err
	}
	if strings.HasPrefix(t.Area, "u:") && t.Area != "u:"+id {
		return &apierr.Error{Status: 403, Code: "not_owner", Message: "Only that visitor can write under users/" + strings.TrimPrefix(t.Area, "u:") + "/."}
	}
	ifv, err := parseIfVersion(c.r)
	if err != nil {
		return err
	}
	req := store.WriteReq{Space: t.Space, Area: t.Area, Key: t.Key, Writer: id, IfVersion: ifv,
		SpaceKey: c.r.Header.Get("Space-Key"),
		OnCreate: func() error { return s.spaceRate(c, id) }}

	if c.r.Method == http.MethodPut {
		maxValue, _, _ := s.cfg.LimitsFor(t.Space)
		body, err := io.ReadAll(io.LimitReader(c.r.Body, maxValue+1))
		if err != nil {
			return &apierr.Error{Status: 400, Code: "invalid_encoding", Message: "Could not read the body."}
		}
		if int64(len(body)) > maxValue {
			return &apierr.Error{Status: 413, Code: "value_too_large", Message: "The value is too large.", Extra: map[string]any{"limit": maxValue}}
		}
		if len(body) == 0 {
			return &apierr.Error{Status: 400, Code: "empty_value", Message: "Empty values aren't stored. To remove a key, use DELETE."}
		}
		if !utf8.Valid(body) {
			return &apierr.Error{Status: 400, Code: "invalid_encoding", Message: "Values must be UTF-8 text."}
		}
		if json.Valid(body) {
			if jsonDepth(body) > 64 {
				return &apierr.Error{Status: 400, Code: "json_too_deep", Message: "JSON may be nested at most 64 levels."}
			}
			req.IsJSON = true
		}
		req.Value = body
	}

	if err := s.rate(s.lim.writesPerVisitor, id, 1, "rate_limited"); err != nil {
		return err
	}
	if err := s.rate(s.lim.writesPerIP, c.ip, 1, "rate_limited"); err != nil {
		return err
	}
	if err := s.rate(s.lim.writeBytesPerIP, c.ip, float64(len(req.Value)+len(t.Key)), "rate_limited"); err != nil {
		return err
	}

	res, err := s.store.Write(c.r.Context(), req)
	if err != nil {
		return err
	}
	if res.Changed {
		s.notify.fire(t.Space)
	}
	c.header().Set("ETag", etag(res.Version))
	status := 200
	if res.Created && c.r.Method == http.MethodPut {
		status = 201
	}
	c.json(status, map[string]any{"version": res.Version})
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

func toItem(e store.Entry) itemJSON {
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

func (s *Server) list(c *reqCtx, t store.Target) error {
	if err := s.rate(s.lim.readsPerIP, c.ip, 1, "rate_limited"); err != nil {
		return err
	}
	q := c.r.URL.Query()
	limit := 1000
	if q.Has("limit") {
		n, ok := parseUint(q.Get("limit"))
		if !ok || n < 1 || n > 1000 {
			return apierr.BadParam("limit", "limit is 1 to 1000.")
		}
		limit = int(n)
	}
	desc := false
	switch q.Get("order") {
	case "", "asc":
	case "desc":
		desc = true
	default:
		return apierr.BadParam("order", "order is asc or desc.")
	}
	after := q.Get("after")
	if after != "" {
		k, u, hasUser := strings.Cut(after, "/")
		if !names.KeyRe.MatchString(k) || (hasUser && (!t.AllUsers || !names.ValidVisitor(u))) {
			return apierr.BadParam("after", "after takes the next value from a previous list.")
		}
	}
	items, next, version, err := s.store.List(t, after, limit, desc)
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

func (s *Server) watch(c *reqCtx, t store.Target) error {
	if err := s.rate(s.lim.readsPerIP, c.ip, 1, "rate_limited"); err != nil {
		return err
	}
	q := c.r.URL.Query()
	timeout := 25 * time.Second
	if q.Has("timeout") {
		n, ok := parseUint(q.Get("timeout"))
		if !ok || n < 1 || n > 55 {
			return apierr.BadParam("timeout", "timeout is 1 to 55 seconds.")
		}
		timeout = time.Duration(n) * time.Second
	}
	var from int64
	if q.Has("version") {
		n, ok := parseUint(q.Get("version"))
		if !ok {
			return apierr.BadParam("version", "version is the version from your last list or watch response, or 0.")
		}
		from = n
	} else {
		seq, err := s.store.CurrentSeq(t.Space)
		if err != nil {
			return err
		}
		from = seq
	}

	if !s.watches.acquire(c.ip) {
		return &apierr.Error{Status: 429, Code: "too_many_watches", Message: "Too many open watches."}
	}
	defer s.watches.release(c.ip)
	s.notify.join(t.Space)
	defer s.notify.leave(t.Space)
	s.store.Pin(t.Space)
	defer s.store.Unpin(t.Space)
	s.store.Touch(t.Space)

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	woken := false
	for {
		sig := s.notify.subscribe(t.Space)
		changes, seq, err := s.store.Changes(t, from, 1000)
		if errors.Is(err, store.ErrVersionExpired) {
			return &apierr.Error{Status: 410, Code: "version_expired", Message: "That version is too old or from elsewhere. Start again from version=0."}
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
			seq, err := s.store.CurrentSeq(t.Space)
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
