package server

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"unicode/utf8"

	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/names"
	"github.com/timjaanson/cubby/internal/store"
)

type pointJSON struct {
	T     int64           `json:"t"`
	Value json.RawMessage `json:"value"`
}

func jsonValue(v []byte, isJSON bool) json.RawMessage {
	if isJSON {
		return json.RawMessage(v)
	}
	b, _ := json.Marshal(string(v))
	return b
}

const maxBatchMembers = 100

// seriesAdd handles POST {key}/series and POST {prefix}*/series.
func (s *Server) seriesAdd(c *reqCtx, t store.Target) error {
	id := c.visitor // route checked access
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
	if t.Pattern {
		readLimit = maxBatchMembers*(maxPoint+160) + 2
	}
	body, err := io.ReadAll(io.LimitReader(c.r.Body, readLimit))
	if err != nil {
		return &apierr.Error{Status: 400, Code: "invalid_encoding", Message: "Could not read the body."}
	}
	tooLarge := func(key string) error {
		e := &apierr.Error{Status: 413, Code: "value_too_large", Message: "A point is at most " + strconv.FormatInt(maxPoint, 10) + " bytes.", Extra: map[string]any{"limit": maxPoint}}
		if key != "" {
			e.Message = "The point for " + key + " is too large. " + e.Message
			e.Extra["key"] = key
		}
		return e
	}
	if int64(len(body)) >= readLimit {
		if t.Pattern {
			return &apierr.Error{Status: 413, Code: "value_too_large", Message: "The body is too large for a batch.", Extra: map[string]any{"limit": readLimit - 1}}
		}
		return tooLarge("")
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

	req := store.PointsReq{Space: t.Space, Area: t.Area, T: at, Writer: id}
	if t.Pattern {
		if req.Points, err = parseBatch(body, isJSON, t.Key, maxPoint, tooLarge); err != nil {
			return err
		}
	} else {
		req.Points = []store.PointIn{{Key: t.Key, Value: body, IsJSON: isJSON}}
	}

	if err := s.rate(s.lim.writesPerVisitor, id, 1, "rate_limited"); err != nil {
		return err
	}
	if err := s.rate(s.lim.writesPerIP, c.ip, 1, "rate_limited"); err != nil {
		return err
	}
	if err := s.rate(s.lim.writeBytesPerIP, c.ip, float64(len(body)+len(t.Key)), "rate_limited"); err != nil {
		return err
	}

	res, err := s.store.AddPoints(c.r.Context(), req)
	if err != nil {
		return err
	}
	if res.Changed {
		s.notify.fire(t.Space)
	}
	if t.Pattern {
		keys := make([]string, len(req.Points))
		for i, p := range req.Points {
			keys[i] = p.Key
		}
		c.json(200, map[string]any{"version": res.Version, "t": res.T, "keys": keys})
		return nil
	}
	c.header().Set("ETag", etag(res.Version))
	status := 200
	if res.Created {
		status = 201
	}
	c.json(status, map[string]any{"version": res.Version, "t": res.T})
	return nil
}

// parseBatch reads a JSON object into one point per member, keeping the
// members' order and their bytes exactly as sent.
func parseBatch(body []byte, isJSON bool, prefix string, maxPoint int64, tooLarge func(string) error) ([]store.PointIn, error) {
	notObject := &apierr.Error{Status: 400, Code: "batch_needs_object",
		Message: "A batch body is a JSON object of 1 to 100 members, like {\"temp\": 21.4, \"rh\": 45}. Each member becomes a point in the series named by the prefix plus the member's name."}
	if !isJSON {
		return nil, notObject
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, notObject
	}
	seen := map[string]bool{}
	var out []store.PointIn
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, notObject
		}
		name, _ := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, notObject
		}
		key := prefix + name
		if seen[key] {
			return nil, &apierr.Error{Status: 400, Code: "batch_needs_object", Message: "The member " + strconv.Quote(name) + " appears twice.", Extra: map[string]any{"key": key}}
		}
		seen[key] = true
		if !names.ValidKey(key) {
			return nil, &apierr.Error{Status: 400, Code: "invalid_key", Extra: map[string]any{"key": key},
				Message: strconv.Quote(key) + " isn't a valid key. The prefix plus each member's name must be 1 to 128 of A-Z a-z 0-9 . _ : -, and not \"watch\" or \"series\"."}
		}
		if int64(len(raw)) > maxPoint {
			return nil, tooLarge(key)
		}
		out = append(out, store.PointIn{Key: key, Value: raw, IsJSON: true})
		if len(out) > maxBatchMembers {
			return nil, notObject
		}
	}
	if len(out) == 0 {
		return nil, notObject
	}
	return out, nil
}

func (s *Server) seriesRead(c *reqCtx, t store.Target) error {
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
	var since *int64
	if q.Has("since") {
		n, ok := parseUint(q.Get("since"))
		if !ok {
			return apierr.BadParam("since", "since is a time in milliseconds. Points after it are returned, oldest first.")
		}
		since = &n
	}
	points, next, version, err := s.store.ReadPoints(t.Space, t.Area, t.Key, since, limit)
	if err != nil {
		return err
	}
	out := make([]pointJSON, 0, len(points))
	for _, p := range points {
		out = append(out, pointJSON{T: p.T, Value: jsonValue(p.Value, p.IsJSON)})
	}
	var nx any
	if next != nil {
		nx = *next
	}
	c.json(200, map[string]any{"key": t.Key, "points": out, "version": version, "next": nx})
	return nil
}
