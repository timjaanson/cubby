package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"

	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/store"
)

const maxBatchWrites = 100

// batchIn is one write of a batch, named by what it does, with the same
// option names as the single request: {"set": path, "value"}, {"delete":
// path}, {"add": path, "value", "t"} (a series point).
type batchIn struct {
	Set       *string         `json:"set"`
	Delete    *string         `json:"delete"`
	Add       *string         `json:"add"`
	Check     *string         `json:"check"`
	Value     json.RawMessage `json:"value"`
	Text      *string         `json:"text"`
	IfVersion json.RawMessage `json:"ifVersion"`
	IfBefore  json.RawMessage `json:"ifBefore"`
	TTL       json.RawMessage `json:"ttl"`
	T         json.RawMessage `json:"t"`
}

const batchShape = `Each write is {"set": path, "value"}, {"delete": path} or {"add": path, "value"} (a series point), with ifVersion, ifBefore, ttl and t as on a single request, and "text" instead of "value" for a plain-text value. {"check": path, "ifVersion"} writes nothing and fails the batch if the key has another version.`

// message turns a batch write into the request it stands for.
func (in batchIn) message() (liveIn, error) {
	var m liveIn
	n := 0
	for _, p := range []struct {
		path   *string
		method string
	}{{in.Set, http.MethodPut}, {in.Delete, http.MethodDelete}, {in.Add, http.MethodPost}, {in.Check, http.MethodGet}} {
		if p.path != nil {
			n++
			m.Method, m.Path = p.method, *p.path
		}
	}
	if n != 1 {
		return m, badBatch("A write has exactly one of set, delete, add or check. " + batchShape)
	}
	if strings.HasSuffix(m.Path, "/series") || strings.HasSuffix(m.Path, "/watch") || strings.HasSuffix(m.Path, "/pulse") {
		return m, badBatch("A path in a batch is a key, like shared/temp, with nothing after it.")
	}
	if m.Path == "batch" {
		return m, badBatch("A batch can't hold a batch.")
	}
	opt := func(name string, raw json.RawMessage, allowed bool) error {
		if raw == nil {
			return nil
		}
		if !allowed {
			op := map[string]string{http.MethodPut: "A set", http.MethodDelete: "A delete", http.MethodPost: "An add", http.MethodGet: "A check"}[m.Method]
			return badBatch(fmt.Sprintf("%s doesn't take %s.", op, name))
		}
		if m.Query == nil {
			m.Query = map[string]json.RawMessage{}
		}
		m.Query[name] = raw
		return nil
	}
	put, del, add, check := m.Method == http.MethodPut, m.Method == http.MethodDelete, m.Method == http.MethodPost, m.Method == http.MethodGet
	if check {
		if strings.Contains(m.Path, "*") {
			return m, badBatch("A check names one key, not a pattern.")
		}
		if in.IfVersion == nil {
			return m, badBatch("A check needs ifVersion: the version the key must still have, 0 for missing.")
		}
		if in.Value != nil || in.Text != nil {
			return m, badBatch("A check has no value.")
		}
	}
	for _, o := range []struct {
		name    string
		raw     json.RawMessage
		allowed bool
	}{{"ifVersion", in.IfVersion, put || del || check}, {"ifBefore", in.IfBefore, !check}, {"ttl", in.TTL, put || add}, {"t", in.T, add}} {
		if err := opt(o.name, o.raw, o.allowed); err != nil {
			return m, err
		}
	}
	if del && (in.Value != nil || in.Text != nil) {
		return m, badBatch("A delete has no value.")
	}
	if (put || add) && in.Value == nil && in.Text == nil {
		return m, badBatch("A set or add needs a value (or text).")
	}
	m.Body, m.Text = in.Value, in.Text
	if add {
		m.Path += "/series"
	}
	return m, nil
}

func badBatch(msg string) *apierr.Error {
	return &apierr.Error{Status: 400, Code: "bad_batch", Message: msg}
}

// atIndex adds the failing write's position to an API error.
func atIndex(err error, i int) error {
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		return err
	}
	cp := *ae
	cp.Extra = maps.Clone(ae.Extra)
	if cp.Extra == nil {
		cp.Extra = map[string]any{}
	}
	cp.Extra["index"] = i
	return &cp
}

// batch handles POST /v1/{space}/batch: up to 100 PUTs, DELETEs and series
// points (POST {key}/series), all or nothing. Each write goes through route as if it came alone, so it is
// checked exactly like a single request, then the store writes them in one
// transaction.
func (s *Server) batch(c *reqCtx, space string) error {
	if c.r.Method != http.MethodPost {
		return methodErrMsg("POST", `POST a JSON array of writes: [{"set": "shared/key", "value": ...}, ...].`)
	}
	if err := checkParams(c.r); err != nil {
		return err
	}
	if _, err := c.needVisitor(); err != nil {
		return err
	}
	s.writeBudget(c)
	limit := liveReadLimit(s.cfg)
	raw, err := io.ReadAll(io.LimitReader(c.r.Body, limit+1))
	if err != nil {
		return &apierr.Error{Status: 400, Code: "invalid_encoding", Message: "Could not read the body."}
	}
	if int64(len(raw)) > limit {
		return &apierr.Error{Status: 413, Code: "value_too_large", Message: "The batch is too large.", Extra: map[string]any{"limit": limit}}
	}
	notArray := badBatch("A batch is a JSON array of 1 to " + strconv.Itoa(maxBatchWrites) + " writes. " + batchShape)
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '[' {
		return notArray
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 {
		return notArray
	}
	if len(items) > maxBatchWrites {
		return badBatch("A batch holds at most " + strconv.Itoa(maxBatchWrites) + " writes.")
	}

	reqs := make([]store.WriteReq, 0, len(items))
	paths := make([]string, len(items))
	seen := map[string]int{}             // first write of each key
	times := map[string]map[int64]bool{} // point times per key
	for i, item := range items {
		dec := json.NewDecoder(bytes.NewReader(item))
		dec.DisallowUnknownFields()
		var in batchIn
		if err := dec.Decode(&in); err != nil {
			return atIndex(badBatch(batchShape+" "+err.Error()), i)
		}
		msg, err := in.message()
		if err != nil {
			return atIndex(err, i)
		}
		point := msg.Method == http.MethodPost
		r, err := messageRequest(c.r.Context(), space, msg)
		if err != nil {
			return atIndex(err, i)
		}
		sub := &reqCtx{w: &statusWriter{ResponseWriter: &recorder{h: http.Header{}}, status: 200}, r: r, ip: c.ip,
			visitor: c.visitor, device: c.device, apiKey: c.apiKey, token: c.token, collect: &reqs}
		before := len(reqs)
		if err := s.route(sub); err != nil {
			return atIndex(err, i)
		}
		if len(reqs) != before+1 || reqs[before].Point != point {
			return atIndex(badBatch("A batch holds sets, deletes and checks of single keys, and points added to single series."), i)
		}
		w := reqs[before]
		k := w.Area + "\x00" + w.Key
		if j, dup := seen[k]; dup {
			// Points may share a key when each names its own time.
			p := reqs[j]
			if !w.Point || !p.Point || w.T == 0 || p.T == 0 || times[k][w.T] {
				return atIndex(badBatch("This key is already in write "+strconv.Itoa(j)+". A batch names each key once (a write checks itself with ifVersion), except for points that each give a different t."), i)
			}
		} else {
			seen[k] = i
			times[k] = map[int64]bool{}
		}
		times[k][w.T] = true
		paths[i] = strings.TrimSuffix(msg.Path, "/series")
	}

	if err := s.rate(s.lim.writesPerVisitor, c.visitor, 1, "rate_limited"); err != nil {
		return err
	}
	if err := s.rate(s.lim.writesPerIP, c.ip, 1, "rate_limited"); err != nil {
		return err
	}
	for _, r := range reqs {
		if r.Value != nil {
			if err := s.storedRoom(c.ip); err != nil {
				return err
			}
			break
		}
	}

	res, err := s.store.WriteBatch(c.r.Context(), reqs)
	var be *store.BatchError
	if errors.As(err, &be) {
		return atIndex(be.Err, be.Index)
	}
	if err != nil {
		return err
	}
	type itemOut struct {
		Path    string `json:"path"`
		Version int64  `json:"version"`
		Created bool   `json:"created,omitempty"`
		T       int64  `json:"t,omitempty"`
	}
	out := make([]itemOut, len(res))
	var grown, version int64
	changed := false
	for i, r := range res {
		out[i] = itemOut{Path: paths[i], Version: r.Version, Created: r.Created && reqs[i].Value != nil}
		if reqs[i].Point {
			out[i].T = r.T
		}
		grown += r.Grown
		version = max(version, r.Version)
		changed = changed || r.Changed
	}
	s.chargeStored(c.ip, grown)
	if changed {
		s.notify.fire(space)
	}
	c.json(200, map[string]any{"version": version, "items": out})
	return nil
}
