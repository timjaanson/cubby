package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A series key keeps every value written to it with POST {key}/series, each
// with a time in milliseconds. Its kv row holds the newest point, so reads,
// lists and watches treat it like any key. The points live in their own table
// and are only ever stored and returned, never computed on.

func errIsSeries(key string) *apiError {
	return &apiError{Status: 409, Code: "is_series", Extra: map[string]any{"key": key},
		Message: key + " is a series. Add points with POST " + key + "/series, or DELETE it first."}
}

func errNotSeries(key string) *apiError {
	return &apiError{Status: 409, Code: "not_series", Extra: map[string]any{"key": key},
		Message: key + " is a plain key, not a series. Read it with GET " + key + ", replace it with PUT."}
}

type pointIn struct {
	key    string
	value  []byte
	isJSON bool
}

type pointsReq struct {
	space, area string
	points      []pointIn // one per key, in request order
	t           int64     // 0: the server's clock
	writer      string
	spaceKey    string
	onCreate    func() error
}

type pointsResult struct {
	version int64 // highest version among the keys afterwards
	t       int64
	created bool // some key was new
	changed bool // some key got a new newest point
}

func (s *Store) addPoints(ctx context.Context, r pointsReq) (res pointsResult, err error) {
	vTotal, err := s.visitorTotal(r.writer)
	if err != nil {
		return res, err
	}
	sp, _, err := s.acquire(r.space, false, "")
	if err != nil {
		return res, err
	}
	if sp == nil {
		if s.spaceCount() >= s.cfg.Limits.MaxSpaces {
			return res, errServerFull
		}
		if r.onCreate != nil {
			if err := r.onCreate(); err != nil {
				return res, err
			}
		}
		if sp, _, err = s.acquire(r.space, true, ""); err != nil {
			return res, err
		}
	}
	defer s.release(sp)

	_, maxBytes, maxKeys := s.cfg.limitsFor(r.space)
	_, maxPoints := s.cfg.seriesLimitsFor(r.space)
	now := nowS()

	sp.wmu.Lock()
	defer sp.wmu.Unlock()
	tx, err := sp.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	m, err := readMeta(tx)
	if err != nil {
		return res, err
	}
	if m.keyHash != "" && r.area == "s" && (r.spaceKey == "" || subtle.ConstantTimeCompare([]byte(hashSecret(r.spaceKey)), []byte(m.keyHash)) != 1) {
		return res, errSpaceLocked
	}

	type oldRow struct {
		value   []byte
		version int64
		writer  string
		size    int64
		series  bool
		t, n    int64
		row     bool
	}
	olds := make([]oldRow, len(r.points))
	for i, p := range r.points {
		var o oldRow
		var t sql.NullInt64
		err := tx.QueryRow(`SELECT value, version, writer, size, series, t, n FROM kv WHERE area = ? AND key = ?`, r.area, p.key).
			Scan(&o.value, &o.version, &o.writer, &o.size, &o.series, &t, &o.n)
		switch {
		case err == nil:
			o.row, o.t = true, t.Int64
		case !errors.Is(err, sql.ErrNoRows):
			return res, err
		}
		if o.row && o.value != nil && !o.series {
			return res, errNotSeries(p.key)
		}
		olds[i] = o
	}

	// One time for every point in the request. Without ?t= the server's
	// clock, but always after each series' newest point, so points the
	// server times never collide.
	t := r.t
	if t == 0 {
		t = nowMs()
		for _, o := range olds {
			if o.row && o.value != nil {
				t = max(t, o.t+1)
			}
		}
	}

	seq := m.seq
	var spaceDelta, visitorDelta int64
	keysDelta := 0
	type usageMove struct {
		writer string
		delta  int64
	}
	var moves []usageMove

	for i, p := range r.points {
		o := olds[i]
		live := o.row && o.value != nil
		n := int64(0)
		if live {
			n = o.n
		}

		// A point at an existing time replaces it.
		var replaced int64
		err := tx.QueryRow(`SELECT length(key) + length(value) FROM points WHERE area = ? AND key = ? AND t = ?`, r.area, p.key, t).Scan(&replaced)
		switch {
		case err == nil:
		case errors.Is(err, sql.ErrNoRows):
			n++
		default:
			return res, err
		}
		if _, err := tx.Exec(`INSERT INTO points (area, key, t, value, is_json) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (area, key, t) DO UPDATE SET value = excluded.value, is_json = excluded.is_json`,
			r.area, p.key, t, p.value, p.isJSON); err != nil {
			return res, err
		}
		spaceDelta += int64(len(p.key)+len(p.value)) - replaced

		// Keep the newest maxPoints.
		if extra := n - int64(maxPoints); extra > 0 {
			var dropped int64
			if err := tx.QueryRow(`SELECT coalesce(sum(length(key) + length(value)), 0) FROM
				(SELECT key, value FROM points WHERE area = ? AND key = ? ORDER BY t LIMIT ?)`, r.area, p.key, extra).Scan(&dropped); err != nil {
				return res, err
			}
			if _, err := tx.Exec(`DELETE FROM points WHERE area = ? AND key = ? AND t IN
				(SELECT t FROM points WHERE area = ? AND key = ? ORDER BY t LIMIT ?)`, r.area, p.key, r.area, p.key, extra); err != nil {
				return res, err
			}
			spaceDelta -= dropped
			n -= extra
		}

		newest := !live || t >= o.t
		if !newest {
			// An older point: the key keeps its value and version.
			if _, err := tx.Exec(`UPDATE kv SET n = ? WHERE area = ? AND key = ?`, n, r.area, p.key); err != nil {
				return res, err
			}
			res.version = max(res.version, o.version)
			continue
		}

		seq++
		newSize := int64(len(p.key) + len(p.value))
		oldSize := int64(0)
		if o.row {
			oldSize = o.size
		}
		spaceDelta += newSize - oldSize
		if !live {
			keysDelta++
			res.created = true
		}
		// The series key counts towards the writer like a plain key of its
		// newest value. The points count towards the space only.
		if live {
			moves = append(moves, usageMove{o.writer, -oldSize})
			if o.writer == r.writer {
				visitorDelta -= oldSize
			}
		}
		moves = append(moves, usageMove{r.writer, newSize})
		visitorDelta += newSize

		if _, err := tx.Exec(`INSERT INTO kv (area, key, value, is_json, version, writer, size, updated_at, series, t, n)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
			ON CONFLICT (area, key) DO UPDATE SET value = excluded.value, is_json = excluded.is_json,
			version = excluded.version, writer = excluded.writer, size = excluded.size, updated_at = excluded.updated_at,
			series = 1, t = excluded.t, n = excluded.n`,
			r.area, p.key, p.value, p.isJSON, seq, r.writer, newSize, now, t, n); err != nil {
			return res, err
		}
		res.version = max(res.version, seq)
		res.changed = true
	}

	// Quotas apply to growth only. Trimming came first, so a full series at
	// a steady rate doesn't grow.
	if keysDelta > 0 && m.keys+keysDelta > maxKeys {
		return res, quotaErr("space_quota_exceeded", "The space has as many keys as it may hold.", int64(maxKeys), int64(m.keys))
	}
	if spaceDelta > 0 && m.bytes+spaceDelta > maxBytes {
		return res, quotaErr("space_quota_exceeded", "The space is full.", maxBytes, m.bytes)
	}
	if lim := int64(s.cfg.Limits.MaxVisitorBytes); visitorDelta > 0 && vTotal+visitorDelta > lim {
		return res, quotaErr("visitor_quota_exceeded", "You have stored as much as one visitor may.", lim, vTotal)
	}
	s.mu.Lock()
	total := s.total
	s.mu.Unlock()
	if spaceDelta > 0 && total+spaceDelta > int64(s.cfg.Limits.MaxTotalBytes) {
		return res, errServerFull
	}

	for _, mv := range moves {
		if _, err := tx.Exec(`INSERT INTO usage (writer, bytes) VALUES (?, max(0, ?)) ON CONFLICT (writer) DO UPDATE SET bytes = max(0, bytes + ?)`,
			mv.writer, mv.delta, mv.delta); err != nil {
			return res, err
		}
	}
	for k, v := range map[string]any{"seq": seq, "bytes": m.bytes + spaceDelta, "keys": m.keys + keysDelta, "last_active_at": now} {
		if err := setMeta(tx, k, v); err != nil {
			return res, err
		}
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	sp.metaActive = now

	s.mu.Lock()
	if st := s.stats[r.space]; st != nil {
		st.bytes += spaceDelta
		st.keys += keysDelta
		st.lastActive = now
	}
	s.total += spaceDelta
	s.mu.Unlock()
	for _, mv := range moves {
		s.adjustVisitor(mv.writer, mv.delta)
	}
	s.noteVisitorSpace(r.writer, r.space)
	res.t = t
	return res, nil
}

// readPoints returns points oldest first: with since, the first limit after
// it and the next since if there are more; without, the newest limit.
func (s *Store) readPoints(space, area, key string, since *int64, limit int) (points []Point, next *int64, version int64, err error) {
	sp, _, err := s.acquire(space, false, "")
	if err != nil {
		return nil, nil, 0, err
	}
	if sp == nil {
		return nil, nil, 0, errNotFound
	}
	defer s.release(sp)
	var series bool
	err = sp.db.QueryRow(`SELECT series FROM kv WHERE area = ? AND key = ? AND value IS NOT NULL`, area, key).Scan(&series)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, 0, errNotFound
	}
	if err != nil {
		return nil, nil, 0, err
	}
	if !series {
		return nil, nil, 0, errNotSeries(key)
	}
	s.touchSpace(sp)
	m, err := readMeta(sp.db)
	if err != nil {
		return nil, nil, 0, err
	}

	var rows *sql.Rows
	if since != nil {
		rows, err = sp.db.Query(`SELECT t, value, is_json FROM points WHERE area = ? AND key = ? AND t > ? ORDER BY t LIMIT ?`, area, key, *since, limit+1)
	} else {
		rows, err = sp.db.Query(`SELECT t, value, is_json FROM points WHERE area = ? AND key = ? ORDER BY t DESC LIMIT ?`, area, key, limit)
	}
	if err != nil {
		return nil, nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		p := Point{Area: area, Key: key}
		if err := rows.Scan(&p.T, &p.Value, &p.IsJSON); err != nil {
			return nil, nil, 0, err
		}
		points = append(points, p)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, err
	}
	if since == nil {
		for i, j := 0, len(points)-1; i < j; i, j = i+1, j-1 {
			points[i], points[j] = points[j], points[i]
		}
	} else if len(points) > limit {
		points = points[:limit]
		nt := points[limit-1].T
		next = &nt
	}
	return points, next, m.seq, nil
}

// ---------- HTTP ----------

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
func (s *Server) seriesAdd(c *reqCtx, t target) error {
	id, err := c.needVisitor()
	if err != nil {
		return err
	}
	if strings.HasPrefix(t.area, "u:") && t.area != "u:"+id {
		return &apiError{Status: 403, Code: "not_owner", Message: "Only that visitor can write under users/" + strings.TrimPrefix(t.area, "u:") + "/."}
	}
	var at int64
	if q := c.r.URL.Query(); q.Has("t") {
		n, ok := parseUint(q.Get("t"))
		if !ok || n < 1 {
			return badParam("t", "t is the point's time in milliseconds since 1970.")
		}
		if n > nowMs()+60_000 {
			return badParam("t", "t is more than 60 seconds in the future.")
		}
		at = n
	}

	maxPoint, _ := s.cfg.seriesLimitsFor(t.space)
	readLimit := maxPoint + 1
	if t.pattern {
		readLimit = maxBatchMembers*(maxPoint+160) + 2
	}
	body, err := io.ReadAll(io.LimitReader(c.r.Body, readLimit))
	if err != nil {
		return &apiError{Status: 400, Code: "invalid_encoding", Message: "Could not read the body."}
	}
	tooLarge := func(key string) error {
		e := &apiError{Status: 413, Code: "value_too_large", Message: "A point is at most " + strconv.FormatInt(maxPoint, 10) + " bytes.", Extra: map[string]any{"limit": maxPoint}}
		if key != "" {
			e.Message = "The point for " + key + " is too large. " + e.Message
			e.Extra["key"] = key
		}
		return e
	}
	if int64(len(body)) >= readLimit {
		if t.pattern {
			return &apiError{Status: 413, Code: "value_too_large", Message: "The body is too large for a batch.", Extra: map[string]any{"limit": readLimit - 1}}
		}
		return tooLarge("")
	}
	if len(body) == 0 {
		return &apiError{Status: 400, Code: "empty_value", Message: "Empty values aren't stored. A point needs a value."}
	}
	if !utf8.Valid(body) {
		return &apiError{Status: 400, Code: "invalid_encoding", Message: "Values must be UTF-8 text."}
	}
	isJSON := json.Valid(body)
	if isJSON && jsonDepth(body) > 64 {
		return &apiError{Status: 400, Code: "json_too_deep", Message: "JSON may be nested at most 64 levels."}
	}

	req := pointsReq{space: t.space, area: t.area, t: at, writer: id, spaceKey: c.r.Header.Get("Space-Key"),
		onCreate: func() error { return s.spaceRate(c, id) }}
	if t.pattern {
		if req.points, err = parseBatch(body, isJSON, t.key, maxPoint, tooLarge); err != nil {
			return err
		}
	} else {
		req.points = []pointIn{{key: t.key, value: body, isJSON: isJSON}}
	}

	if err := s.rate(s.lim.writesPerVisitor, id, 1, "rate_limited"); err != nil {
		return err
	}
	if err := s.rate(s.lim.writesPerIP, c.ip, 1, "rate_limited"); err != nil {
		return err
	}
	if err := s.rate(s.lim.writeBytesPerIP, c.ip, float64(len(body)+len(t.key)), "rate_limited"); err != nil {
		return err
	}

	res, err := s.store.addPoints(c.r.Context(), req)
	if err != nil {
		return err
	}
	if res.changed {
		s.notify.fire(t.space)
	}
	if t.pattern {
		keys := make([]string, len(req.points))
		for i, p := range req.points {
			keys[i] = p.key
		}
		c.json(200, map[string]any{"version": res.version, "t": res.t, "keys": keys})
		return nil
	}
	c.header().Set("ETag", etag(res.version))
	status := 200
	if res.created {
		status = 201
	}
	c.json(status, map[string]any{"version": res.version, "t": res.t})
	return nil
}

// parseBatch reads a JSON object into one point per member, keeping the
// members' order and their bytes exactly as sent.
func parseBatch(body []byte, isJSON bool, prefix string, maxPoint int64, tooLarge func(string) error) ([]pointIn, error) {
	notObject := &apiError{Status: 400, Code: "batch_needs_object",
		Message: "A batch body is a JSON object of 1 to 100 members, like {\"temp\": 21.4, \"rh\": 45}. Each member becomes a point in the series named by the prefix plus the member's name."}
	if !isJSON {
		return nil, notObject
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, notObject
	}
	seen := map[string]bool{}
	var out []pointIn
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
			return nil, &apiError{Status: 400, Code: "batch_needs_object", Message: "The member " + strconv.Quote(name) + " appears twice.", Extra: map[string]any{"key": key}}
		}
		seen[key] = true
		if !validKey(key) {
			return nil, &apiError{Status: 400, Code: "invalid_key", Extra: map[string]any{"key": key},
				Message: strconv.Quote(key) + " isn't a valid key. The prefix plus each member's name must be 1 to 128 of A-Z a-z 0-9 . _ : -, and not \"watch\" or \"series\"."}
		}
		if int64(len(raw)) > maxPoint {
			return nil, tooLarge(key)
		}
		out = append(out, pointIn{key: key, value: raw, isJSON: true})
		if len(out) > maxBatchMembers {
			return nil, notObject
		}
	}
	if len(out) == 0 {
		return nil, notObject
	}
	return out, nil
}

func (s *Server) seriesRead(c *reqCtx, t target) error {
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
	var since *int64
	if q.Has("since") {
		n, ok := parseUint(q.Get("since"))
		if !ok {
			return badParam("since", "since is a time in milliseconds. Points after it are returned, oldest first.")
		}
		since = &n
	}
	points, next, version, err := s.store.readPoints(t.space, t.area, t.key, since, limit)
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
	c.json(200, map[string]any{"key": t.key, "points": out, "version": version, "next": nx})
	return nil
}
