package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"

	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/store"
)

const maxBatchWrites = 100

// batchIn is one write of a batch: a live data message without id.
type batchIn struct {
	Method string                     `json:"method"`
	Path   string                     `json:"path"`
	Query  map[string]json.RawMessage `json:"query"`
	Body   json.RawMessage            `json:"body"`
	Text   *string                    `json:"text"`
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
		return methodErrMsg("POST", "POST a JSON array of writes: [{\"method\": \"PUT\", \"path\": \"shared/key\", \"body\": ...}, ...].")
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
	notArray := badBatch("A batch is a JSON array of 1 to " + strconv.Itoa(maxBatchWrites) + " writes, each {\"method\": \"PUT\", \"DELETE\" or \"POST\" (to {key}/series), \"path\", \"query\", \"body\" or \"text\"}.")
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
			return atIndex(badBatch("Each write is {\"method\", \"path\", \"query\", \"body\" or \"text\"}. "+err.Error()), i)
		}
		point := in.Method == http.MethodPost
		if !point && in.Method != http.MethodPut && in.Method != http.MethodDelete {
			return atIndex(badBatch("A batch holds PUT, DELETE, and POST of a series point."), i)
		}
		if point && !strings.HasSuffix(in.Path, "/series") {
			return atIndex(badBatch("In a batch, POST only adds a series point: the path ends in /series."), i)
		}
		if in.Path == "batch" {
			return atIndex(badBatch("A batch can't hold a batch."), i)
		}
		r, err := messageRequest(c.r.Context(), space, liveIn{Method: in.Method, Path: in.Path, Query: in.Query, Body: in.Body, Text: in.Text})
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
			return atIndex(badBatch("A batch holds PUT and DELETE of single keys, and points added to single series."), i)
		}
		w := reqs[before]
		k := w.Area + "\x00" + w.Key
		if j, dup := seen[k]; dup {
			// Points may share a key when each names its own time.
			p := reqs[j]
			if !w.Point || !p.Point || w.T == 0 || p.T == 0 || times[k][w.T] {
				return atIndex(badBatch("This key is already written by write "+strconv.Itoa(j)+". A batch writes each key once, except for points that each give a different t."), i)
			}
		} else {
			seen[k] = i
			times[k] = map[int64]bool{}
		}
		times[k][w.T] = true
		paths[i] = in.Path
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
