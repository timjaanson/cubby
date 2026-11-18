package server

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/names"
	"github.com/timjaanson/cubby/internal/store"
)

type pointJSON struct {
	Key     string          `json:"key,omitempty"` // in a read of several series
	T       int64           `json:"t"`
	Value   json.RawMessage `json:"value"`
	Expires int64           `json:"expires,omitempty"`
}

func jsonValue(v []byte, isJSON bool) json.RawMessage {
	if isJSON {
		return json.RawMessage(v)
	}
	b, _ := json.Marshal(string(v))
	return b
}

// maxSeriesLimit is the most points one series read returns.
const maxSeriesLimit = 10000

// seriesAdd handles POST {key}/series. Inside a batch it only collects the
// point.
func (s *Server) seriesAdd(c *reqCtx, t store.Target) error {
	id := c.visitor // route checked access
	if c.collect == nil {
		s.writeBudget(c)
	}
	before, err := parseIfBefore(c.r)
	if err != nil {
		return err
	}
	ttl, err := s.parseTTL(c.r)
	if err != nil {
		return err
	}
	var at int64
	if q := c.r.URL.Query(); q.Has("t") {
		n, ok := parseUint(q.Get("t"))
		if !ok || n < 1 {
			return apierr.BadParam("t", "t is the point's time in milliseconds since 1970.")
		}
		if n > store.NowMs()+60_000 {
			return apierr.BadParam("t", "t is more than 60 seconds in the future.")
		}
		at = n
	}

	maxPoint, _ := s.cfg.SeriesLimitsFor(t.Space)
	readLimit := maxPoint + 1
	body, err := io.ReadAll(io.LimitReader(c.r.Body, readLimit))
	if err != nil {
		return &apierr.Error{Status: 400, Code: "invalid_encoding", Message: "Could not read the body."}
	}
	if int64(len(body)) >= readLimit {
		return &apierr.Error{Status: 413, Code: "value_too_large", Message: "A point is at most " + strconv.FormatInt(maxPoint, 10) + " bytes.", Extra: map[string]any{"limit": maxPoint}}
	}
	if len(body) == 0 {
		return &apierr.Error{Status: 400, Code: "empty_value", Message: "Empty values aren't stored. A point needs a value."}
	}
	if !utf8.Valid(body) {
		return &apierr.Error{Status: 400, Code: "invalid_encoding", Message: "Values must be UTF-8 text."}
	}
	isJSON := json.Valid(body)
	if isJSON && jsonDepth(body) > 64 {
		return &apierr.Error{Status: 400, Code: "json_too_deep", Message: "JSON may be nested at most 64 levels."}
	}

	req := store.PointsReq{IfBefore: before, Space: t.Space, Area: t.Area, T: at, TTL: ttl, Writer: id,
		Points: []store.PointIn{{Key: t.Key, Value: body, IsJSON: isJSON}}}
	if c.collect != nil {
		*c.collect = append(*c.collect, store.WriteReq{Space: t.Space, Area: t.Area, Key: t.Key, Value: body, IsJSON: isJSON,
			Writer: id, IfBefore: before, Point: true, T: at, TTL: ttl})
		return nil
	}

	if err := s.rate(s.lim.writesPerVisitor, id, 1, "rate_limited"); err != nil {
		return err
	}
	if err := s.rate(s.lim.writesPerIP, c.ip, 1, "rate_limited"); err != nil {
		return err
	}
	if err := s.storedRoom(c.ip); err != nil {
		return err
	}

	res, err := s.store.AddPoints(c.r.Context(), req)
	if err != nil {
		return err
	}
	s.chargeStored(c.ip, res.Grown)
	if res.Changed {
		s.notify.fire(t.Space)
	}
	c.header().Set("ETag", etag(res.Version))
	status := 200
	if res.Created {
		status = 201
	}
	c.json(status, map[string]any{"version": res.Version, "t": res.T})
	return nil
}

func (s *Server) seriesRead(c *reqCtx, t store.Target) error {
	if err := s.rate(s.lim.readsPerIP, c.ip, 1, "rate_limited"); err != nil {
		return err
	}
	q := c.r.URL.Query()
	pq := store.PointsQuery{Limit: 1000, Desc: true}
	if q.Has("limit") {
		n, ok := parseUint(q.Get("limit"))
		if !ok || n < 1 || n > maxSeriesLimit {
			return apierr.BadParam("limit", "limit is 1 to 10000.")
		}
		pq.Limit = int(n)
	}
	// Newest first unless asked: things ordered by time start with the latest.
	switch q.Get("order") {
	case "", "desc":
	case "asc":
		pq.Desc = false
	default:
		return apierr.BadParam("order", "order is asc or desc.")
	}
	ms := func(name, what string) (*int64, error) {
		if !q.Has(name) {
			return nil, nil
		}
		n, ok := parseUint(q.Get(name))
		if !ok {
			return nil, apierr.BadParam(name, name+" is "+what)
		}
		return &n, nil
	}
	var err error
	if pq.Since, err = ms("since", "a time in milliseconds since 1970. Points at or after it are returned."); err != nil {
		return err
	}
	if pq.Before, err = ms("before", "a time in milliseconds since 1970. Points before it are returned."); err != nil {
		return err
	}
	if t.Pattern {
		return s.seriesReadPattern(c, t, pq)
	}
	if pq.After, err = ms("after", "the next value of the previous page."); err != nil {
		return err
	}
	points, next, version, err := s.store.ReadPoints(t.Space, t.Area, t.Key, pq)
	if err != nil {
		return err
	}
	out := make([]pointJSON, 0, len(points))
	for _, p := range points {
		out = append(out, pointJSON{T: p.T, Value: jsonValue(p.Value, p.IsJSON), Expires: p.Expires})
	}
	var nx any
	if next != nil {
		nx = *next
	}
	c.json(200, map[string]any{"items": out, "version": version, "next": nx})
	return nil
}

// seriesReadPattern reads every series under a prefix, merged by time; each
// point carries its key. next is "t:key", as ties at one t go by key.
func (s *Server) seriesReadPattern(c *reqCtx, t store.Target, pq store.PointsQuery) error {
	if a := c.r.URL.Query().Get("after"); c.r.URL.Query().Has("after") {
		ts, key, ok := strings.Cut(a, ":")
		n, okN := parseUint(ts)
		if !ok || !okN || !names.ValidKey(key) || !strings.HasPrefix(key, t.Key) {
			return apierr.BadParam("after", "after is the next value of the previous page.")
		}
		pq.After, pq.AfterKey = &n, key
	}
	points, next, version, err := s.store.ReadSeriesPrefix(t.Space, t.Area, t.Key, pq)
	if err != nil {
		return err
	}
	out := make([]pointJSON, 0, len(points))
	for _, p := range points {
		out = append(out, pointJSON{Key: p.Key, T: p.T, Value: jsonValue(p.Value, p.IsJSON), Expires: p.Expires})
	}
	var nx any
	if next != nil {
		nx = strconv.FormatInt(next.T, 10) + ":" + next.Key
	}
	c.json(200, map[string]any{"items": out, "version": version, "next": nx})
	return nil
}
