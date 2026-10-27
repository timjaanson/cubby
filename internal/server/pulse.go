package server

// Pulses are values that are never stored. A pulse goes to whoever listens
// at that moment and is gone: no version, no disk, no replay beyond a short
// in-memory window that lets HTTP listeners and reconnecting sockets catch
// up. They're for things that change many times a second or that matter as
// events, not as state: cursors, drags, reactions, game inputs.
//
// Sending must stay cheap however many arrive, so a send only appends to the
// space's window and wakes listeners. Listeners each read the window from
// their own cursor; a slow one falls behind and is told it missed some,
// without ever slowing a sender or another listener.

import (
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/store"
)

const (
	pulseWindow      = 10 * time.Second // how long a pulse can still be picked up
	pulseWindowCount = 1024             // per space
	pulseWindowBytes = 256 << 10        // per space
	pulseBatchMax    = 256              // pulses per response; older ones count as missed
	pulseHTTPBatch   = 50 * time.Millisecond
)

type pulse struct {
	seq    int64
	area   string // "s", "m:{id}" or "u:{id}", as store.Target.Area
	key    string
	by     string
	value  []byte
	isJSON bool
	t      int64
	at     time.Time
}

type pulseJSON struct {
	Key   string          `json:"key"`
	User  string          `json:"user,omitempty"`
	By    string          `json:"by"`
	Value json.RawMessage `json:"value"`
	T     int64           `json:"t"`
}

func (p *pulse) json() pulseJSON {
	out := pulseJSON{Key: p.key, By: p.by, Value: jsonValue(p.value, p.isJSON), T: p.t}
	if strings.HasPrefix(p.area, "u:") {
		out.User = p.area[2:]
	}
	return out
}

type pulseHub struct {
	mu        sync.Mutex
	window    []pulse // oldest first
	bytes     int
	dropped   int64 // seq of the newest pulse that left the window
	sig       chan struct{}
	listeners int
	active    time.Time
}

type pulseHubs struct {
	seq       atomic.Int64 // server-wide; starts at the start time in µs, so it keeps rising across restarts
	mu        sync.Mutex
	hubs      map[string]*pulseHub
	lastSweep time.Time
}

func newPulseHubs() *pulseHubs {
	p := &pulseHubs{hubs: map[string]*pulseHub{}}
	p.seq.Store(time.Now().UnixMicro())
	return p
}

// hub returns the space's hub, creating it. Idle hubs are swept here, so
// nothing needs a timer.
func (p *pulseHubs) hub(space string) *pulseHub {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if now.Sub(p.lastSweep) > pulseWindow {
		p.lastSweep = now
		for name, h := range p.hubs {
			h.mu.Lock()
			idle := h.listeners == 0 && now.Sub(h.active) > pulseWindow
			h.mu.Unlock()
			if idle {
				delete(p.hubs, name)
			}
		}
	}
	h := p.hubs[space]
	if h == nil {
		h = &pulseHub{sig: make(chan struct{}), active: now}
		p.hubs[space] = h
	}
	return h
}

func (p *pulseHubs) send(space string, pl pulse) {
	h := p.hub(space)
	h.mu.Lock()
	// Numbered under the hub's lock, so the window is always in seq order
	// and a listener's cursor never skips or repeats a pulse.
	pl.seq = p.seq.Add(1)
	h.window = append(h.window, pl)
	h.bytes += len(pl.value)
	h.trim(pl.at)
	h.active = pl.at
	close(h.sig)
	h.sig = make(chan struct{})
	h.mu.Unlock()
}

// trim drops what's too old or over the window's size. Caller holds mu.
func (h *pulseHub) trim(now time.Time) {
	n := 0
	for n < len(h.window) && (now.Sub(h.window[n].at) > pulseWindow || len(h.window)-n > pulseWindowCount || h.bytes > pulseWindowBytes) {
		h.bytes -= len(h.window[n].value)
		h.dropped = h.window[n].seq
		n++
	}
	if n > 0 {
		h.window = append(h.window[:0:0], h.window[n:]...)
	}
}

// since returns the pulses after cursor that match, at most pulseBatchMax
// (the newest), the cursor to continue from, and whether any were lost: they
// left the window before this listener got to them, or didn't fit.
func (h *pulseHub) since(cursor int64, match func(*pulse) bool) (out []pulseJSON, next int64, missed bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.trim(time.Now())
	missed = cursor < h.dropped
	next = cursor
	// The window is in seq order, so skip what this listener has seen.
	start := sort.Search(len(h.window), func(i int) bool { return h.window[i].seq > cursor })
	for i := start; i < len(h.window); i++ {
		p := &h.window[i]
		next = p.seq
		if match(p) {
			out = append(out, p.json())
		}
	}
	if len(out) > pulseBatchMax {
		out = out[len(out)-pulseBatchMax:]
		missed = true
	}
	return out, next, missed
}

func (h *pulseHub) wait() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sig
}

func (h *pulseHub) listen(delta int) {
	h.mu.Lock()
	h.listeners += delta
	h.active = time.Now()
	h.mu.Unlock()
}

// pulseMatch selects the pulses a target covers: one key or a prefix, in one
// area or across users/*/.
func pulseMatch(t store.Target) func(*pulse) bool {
	return func(p *pulse) bool {
		if t.AllUsers {
			if !strings.HasPrefix(p.area, "u:") {
				return false
			}
		} else if p.area != t.Area {
			return false
		}
		if t.Pattern {
			return strings.HasPrefix(p.key, t.Key)
		}
		return p.key == t.Key
	}
}

// pulseSend handles POST {key}/pulse. route checked access; the visitor is known.
func (s *Server) pulseSend(c *reqCtx, t store.Target) error {
	maxSize := int64(s.cfg.Limits.MaxPulseSize)
	body, err := io.ReadAll(io.LimitReader(c.r.Body, maxSize+1))
	if err != nil {
		return &apierr.Error{Status: 400, Code: "invalid_encoding", Message: "Could not read the body."}
	}
	if int64(len(body)) > maxSize {
		return &apierr.Error{Status: 413, Code: "value_too_large", Message: "A pulse is at most " + sizeText(maxSize) + ".", Extra: map[string]any{"limit": maxSize}}
	}
	if len(body) == 0 {
		return &apierr.Error{Status: 400, Code: "empty_value", Message: "A pulse needs a value."}
	}
	if !utf8.Valid(body) {
		return &apierr.Error{Status: 400, Code: "invalid_encoding", Message: "Values must be UTF-8 text."}
	}
	isJSON := json.Valid(body)
	if isJSON && jsonDepth(body) > 64 {
		return &apierr.Error{Status: 400, Code: "json_too_deep", Message: "JSON may be nested at most 64 levels."}
	}
	if err := s.rate(s.lim.pulsesPerVisitor, c.visitor, 1, "rate_limited"); err != nil {
		return err
	}
	if err := s.rate(s.lim.pulsesPerIP, c.ip, 1, "rate_limited"); err != nil {
		return err
	}
	now := time.Now()
	ms := now.UnixMilli()
	s.pulses.send(t.Space, pulse{area: t.Area, key: t.Key, by: c.visitor, value: body, isJSON: isJSON, t: ms, at: now})
	c.json(202, map[string]any{"t": ms})
	return nil
}

func sizeText(n int64) string {
	if n%1024 == 0 {
		return strconv.FormatInt(n/1024, 10) + " KB"
	}
	return strconv.FormatInt(n, 10) + " bytes"
}

// pulseRead handles GET {key or pattern}/pulse: a long poll over HTTP, a
// running listener on a live connection.
func (s *Server) pulseRead(c *reqCtx, t store.Target) error {
	if err := s.rate(s.lim.readsPerIP, c.ip, 1, "rate_limited"); err != nil {
		return err
	}
	q := c.r.URL.Query()
	timeout := 25 * time.Second
	if q.Has("timeout") {
		if c.live != nil {
			return apierr.BadParam("timeout", "Listening on a live connection runs until stopped, so it takes no timeout.")
		}
		n, ok := parseUint(q.Get("timeout"))
		if !ok || n < 1 || n > 55 {
			return apierr.BadParam("timeout", "timeout is 1 to 55 seconds.")
		}
		timeout = time.Duration(n) * time.Second
	}
	now := s.pulses.seq.Load()
	cursor, missed := now, false
	if q.Has("after") {
		n, ok := parseUint(q.Get("after"))
		if !ok {
			return apierr.BadParam("after", "after is the seq from your last pulse response.")
		}
		cursor = n
		if cursor > now {
			// From before a restart with a clock that went back, or made up.
			cursor, missed = now, true
		}
	}
	h := s.pulses.hub(t.Space)
	if c.live != nil {
		return c.live.startPulses(c, t, h, cursor, missed)
	}

	if !s.watches.acquire(c.ip) {
		return &apierr.Error{Status: 429, Code: "too_many_watches", Message: "Too many open watches."}
	}
	defer s.watches.release(c.ip)
	h.listen(1)
	defer h.listen(-1)
	match := pulseMatch(t)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	woken := false
	for {
		sig := h.wait()
		out, next, lost := h.since(cursor, match)
		if len(out) > 0 || lost || missed {
			if woken {
				// More are probably on the way; take them in this response.
				woken = false
				select {
				case <-time.After(pulseHTTPBatch):
					continue
				case <-c.r.Context().Done():
					return nil
				}
			}
			c.json(200, map[string]any{"pulses": nonNil(out), "seq": next, "missed": lost || missed})
			return nil
		}
		cursor = next
		select {
		case <-sig:
			woken = true
		case <-deadline.C:
			c.json(200, map[string]any{"pulses": []pulseJSON{}, "seq": cursor, "missed": false})
			return nil
		case <-c.r.Context().Done():
			return nil
		}
	}
}

func nonNil(p []pulseJSON) []pulseJSON {
	if p == nil {
		return []pulseJSON{}
	}
	return p
}
