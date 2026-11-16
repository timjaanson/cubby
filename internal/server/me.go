package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/timjaanson/cubby/internal/access"
	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/auth"
	"github.com/timjaanson/cubby/internal/names"
	"github.com/timjaanson/cubby/internal/store"
)

// The visitor's own things, none of them about a space: registering a key,
// /v1/me, devices, pairing codes and API keys.

const (
	maxDeviceLabel = 64
	maxPairingWait = 60 * time.Second
	apiKeyDefault  = 30 * 24 * time.Hour
)

// pairingWaits wakes status long-polls when a pairing code is used or
// cancelled.
type pairingWaits struct {
	mu sync.Mutex
	m  map[string]chan struct{}
}

func (w *pairingWaits) wait(id string) <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.m == nil {
		w.m = map[string]chan struct{}{}
	}
	ch := w.m[id]
	if ch == nil {
		ch = make(chan struct{})
		w.m[id] = ch
	}
	return ch
}

func (w *pairingWaits) fire(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ch := w.m[id]; ch != nil {
		close(ch)
		delete(w.m, id)
	}
}

// registerVisitor is POST /v1/visitors: a new visitor whose first device is
// the key that signed the request. Registering a key again returns its
// visitor.
func (s *Server) registerVisitor(c *reqCtx) error {
	if c.apiKey != nil {
		return apierr.APIKeyNotAllowed
	}
	var in struct {
		Label string `json:"label"`
	}
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	label, err := deviceLabel(in.Label)
	if err != nil {
		return err
	}
	key, err := s.newKey(c)
	if err != nil {
		return err
	}
	if err := s.rate(s.lim.visitorsPerIP, c.ip, 1, "rate_limited"); err != nil {
		return err
	}
	id, created, err := s.store.NewVisitor(key, label)
	if err != nil {
		return err
	}
	c.visitor, c.device = id, key.ID
	status := 200
	if created {
		status = 201
	}
	c.json(status, map[string]any{"id": id, "device": key.ID})
	return nil
}

// meRoute is /v1/me and what's under it. rest is the path after me.
func (s *Server) meRoute(c *reqCtx, rest []string) error {
	if c.apiKey != nil {
		return apierr.APIKeyNotAllowed
	}
	m := c.r.Method
	if len(rest) == 0 {
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
		c.json(200, map[string]any{"id": id, "device": c.device})
		return nil
	}
	id, err := c.needVisitor()
	if err != nil {
		return err
	}
	switch {
	case rest[0] == "devices" && len(rest) == 1:
		switch m {
		case http.MethodGet, http.MethodHead:
			if err := checkParams(c.r); err != nil {
				return err
			}
			return s.listDevices(c)
		case http.MethodDelete:
			if err := checkParams(c.r, "others"); err != nil {
				return err
			}
			if c.r.URL.Query().Get("others") != "1" {
				return apierr.BadParam("others", "DELETE /v1/me/devices signs out every other device and needs ?others=1. To sign out one, DELETE /v1/me/devices/{id}.")
			}
			return s.signOutOthers(c)
		}
		return methodErr("GET, HEAD, DELETE")
	case rest[0] == "devices" && len(rest) == 2:
		dev := rest[1]
		if dev == "current" {
			dev = c.device
		} else if !names.ValidDeviceID(dev) {
			return &apierr.Error{Status: 404, Code: "not_found", Message: "A device ID is its key's JWK thumbprint, 43 characters. Or use current for this one."}
		}
		switch m {
		case http.MethodPatch:
			if err := checkParams(c.r); err != nil {
				return err
			}
			var in struct {
				Label *string `json:"label"`
			}
			if err := decodeBody(c, &in); err != nil {
				return err
			}
			if in.Label == nil {
				return apierr.BadParam("label", `PATCH takes {"label": "Work laptop"}.`)
			}
			label, err := deviceLabel(*in.Label)
			if err != nil {
				return err
			}
			if err := s.store.RenameDevice(id, dev, label); err != nil {
				return err
			}
			return s.listDevices(c)
		case http.MethodDelete:
			if err := checkParams(c.r); err != nil {
				return err
			}
			if err := s.store.RemoveDevice(id, dev); err != nil {
				return err
			}
			s.signOutLive([]string{dev})
			c.json(200, map[string]any{"removed": []string{dev}})
			return nil
		}
		return methodErr("PATCH, DELETE")
	case rest[0] == "pairing-codes" && len(rest) == 1:
		if m != http.MethodPost {
			return methodErr("POST")
		}
		if err := checkParams(c.r); err != nil {
			return err
		}
		return s.createPairing(c)
	case rest[0] == "pairing-codes" && len(rest) == 2:
		if !names.ValidPairingID(rest[1]) {
			return &apierr.Error{Status: 404, Code: "not_found", Message: "No such pairing code. Their IDs look like pc_x4k2m7q9; the code itself goes to POST /v1/pair."}
		}
		switch m {
		case http.MethodGet, http.MethodHead:
			if err := checkParams(c.r, "wait"); err != nil {
				return err
			}
			return s.pairingStatus(c, rest[1])
		case http.MethodDelete:
			if err := checkParams(c.r); err != nil {
				return err
			}
			return s.cancelPairing(c, rest[1])
		}
		return methodErr("GET, HEAD, DELETE")
	case rest[0] == "api-keys" && len(rest) == 1:
		switch m {
		case http.MethodPost:
			if err := checkParams(c.r); err != nil {
				return err
			}
			return s.createAPIKey(c)
		case http.MethodGet, http.MethodHead:
			if err := checkParams(c.r); err != nil {
				return err
			}
			items, err := s.store.APIKeys(id)
			if err != nil {
				return err
			}
			c.json(200, map[string]any{"items": items})
			return nil
		}
		return methodErr("GET, HEAD, POST")
	case rest[0] == "api-keys" && len(rest) == 2:
		if m != http.MethodDelete {
			return methodErr("DELETE")
		}
		if err := checkParams(c.r); err != nil {
			return err
		}
		if !names.ValidAPIKeyID(rest[1]) {
			return &apierr.Error{Status: 404, Code: "not_found", Message: "This visitor has no API key with that ID. IDs look like ak_x4k2m7q9."}
		}
		if err := s.store.DeleteAPIKey(id, rest[1]); err != nil {
			return err
		}
		c.json(200, map[string]any{})
		return nil
	}
	return noRoute
}

func deviceLabel(l string) (string, error) {
	l = strings.TrimSpace(l)
	if !utf8.ValidString(l) || utf8.RuneCountInString(l) > maxDeviceLabel || strings.ContainsFunc(l, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", apierr.BadParam("label", "A label is up to 64 characters of plain text.")
	}
	return l, nil
}

func pairingJSON(l *store.Pairing) map[string]any {
	return map[string]any{"id": l.ID, "state": l.State, "created": l.Created, "expires": l.Expires, "ended": l.Ended, "device": l.Device}
}

func (s *Server) createPairing(c *reqCtx) error {
	var in struct {
		TTL *int64 `json:"ttl"`
	}
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	ttl := int64(store.PairingTTLDefault)
	if in.TTL != nil {
		ttl = *in.TTL * 1000
		if ttl < store.PairingTTLMin || ttl > store.PairingTTLMax {
			return apierr.BadParam("ttl", "ttl is 30 to 3600 seconds. The default is 600.")
		}
	}
	if err := s.rate(s.lim.linksPerVisitor, c.visitor, 1, "rate_limited"); err != nil {
		return err
	}
	l, code, err := s.store.CreatePairing(c.visitor, c.device, ttl)
	if err != nil {
		return err
	}
	out := pairingJSON(l)
	out["code"], out["display"] = code, names.DisplayPairingCode(code)
	c.json(201, out)
	return nil
}

func (s *Server) pairingStatus(c *reqCtx, pairingID string) error {
	if err := s.rate(s.lim.readsPerIP, c.ip, 1, "rate_limited"); err != nil {
		return err
	}
	var wait time.Duration
	if c.r.URL.Query().Has("wait") {
		n, ok := parseUint(c.r.URL.Query().Get("wait"))
		if !ok || n > int64(maxPairingWait/time.Second) {
			return apierr.BadParam("wait", "wait is 0 to 60 seconds.")
		}
		wait = time.Duration(n) * time.Second
	}
	ch := s.pairings.wait(pairingID) // before reading, so a change in between still wakes us
	l, err := s.store.GetPairing(c.visitor, pairingID)
	if err != nil {
		return err
	}
	if l == nil {
		return &apierr.Error{Status: 404, Code: "not_found", Message: "This visitor has no pairing code with that ID."}
	}
	if l.State == "open" && wait > 0 {
		if !s.watches.acquire(c.ip) {
			return apierr.RateLimited("too_many_watches", time.Second)
		}
		defer s.watches.release(c.ip)
		until := time.Until(time.UnixMilli(l.Expires))
		t := time.NewTimer(min(wait, until+10*time.Millisecond))
		defer t.Stop()
		select {
		case <-ch:
		case <-t.C:
		case <-c.r.Context().Done():
			return nil
		}
		if l, err = s.store.GetPairing(c.visitor, pairingID); err != nil {
			return err
		}
	}
	c.json(200, pairingJSON(l))
	return nil
}

func (s *Server) cancelPairing(c *reqCtx, pairingID string) error {
	l, err := s.store.CancelPairing(c.visitor, pairingID)
	if err != nil {
		return err
	}
	if l == nil {
		return &apierr.Error{Status: 404, Code: "not_found", Message: "This visitor has no pairing code with that ID."}
	}
	s.pairings.fire(pairingID)
	c.json(200, pairingJSON(l))
	return nil
}

// pair is POST /v1/pair: the device whose key signed the request becomes a
// device of the visitor that made the code. The code is a JSON object or the
// whole body, so curl -d CODE works. A new key comes in the token's jwk
// header; a key that already is a device leaves its old visitor.
func (s *Server) pair(c *reqCtx) error {
	if c.apiKey != nil {
		return apierr.APIKeyNotAllowed
	}
	body, err := io.ReadAll(io.LimitReader(c.r.Body, maxJSONBody+1))
	if err != nil || len(body) > maxJSONBody {
		return apierr.BadParam("body", "The body is the pairing code, or {\"code\": \"...\", \"label\": \"...\"}.")
	}
	var in struct {
		Code  string `json:"code"`
		Label string `json:"label"`
	}
	if t := bytes.TrimSpace(body); len(t) > 0 && t[0] == '{' {
		dec := json.NewDecoder(bytes.NewReader(t))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return apierr.BadParam("body", "The body is the pairing code, or {\"code\": \"...\", \"label\": \"...\"}.")
		}
	} else {
		in.Code = string(t)
	}
	label, err := deviceLabel(in.Label)
	if err != nil {
		return err
	}
	var key *auth.Key
	if c.token != nil && c.token.JWK != nil {
		if key, err = s.newKey(c); err != nil {
			return err
		}
	} else if c.device != "" {
		if _, key, err = s.store.DeviceKey(c.device); err != nil {
			return err
		}
	}
	if key == nil {
		if c.tokErr != nil {
			return c.tokErr
		}
		return apierr.TokenError("missing_token", "Sign this request with the key to add: a token with the public key in its header as jwk.")
	}
	if s.cfg.RateLimitsOn() {
		if ok, retry := s.lim.linkMissesPerIP.peek(c.ip, 1); !ok {
			return apierr.RateLimited("rate_limited", retry)
		}
	}
	miss := func() error {
		if s.cfg.RateLimitsOn() {
			s.lim.linkMissesPerIP.charge(c.ip, 1)
		}
		return store.ErrPairingUnknown
	}
	code := names.NormalizePairingCode(in.Code)
	if code == "" {
		if strings.TrimSpace(in.Code) == "" {
			return apierr.BadParam("code", "Send the pairing code as the body, or as {\"code\": \"...\"}.")
		}
		return miss()
	}
	r, err := s.store.Pair(code, label, key)
	if err == store.ErrPairingUnknown {
		return miss()
	}
	if err != nil && r == nil {
		return err
	}
	s.pairings.fire(r.PairingID)
	if r.Previous != "" {
		s.signOutLive([]string{r.Device})
	}
	c.visitor, c.device = r.Visitor, r.Device
	var prev any
	if r.Previous != "" {
		prev = r.Previous
	}
	c.json(201, map[string]any{"id": r.Visitor, "device": r.Device, "previous": prev, "deleted": r.Deleted})
	return nil
}

func (s *Server) listDevices(c *reqCtx) error {
	ds, err := s.store.Devices(c.visitor, c.device)
	if err != nil {
		return err
	}
	c.json(200, map[string]any{"id": c.visitor, "device": c.device, "items": ds})
	return nil
}

func (s *Server) signOutOthers(c *reqCtx) error {
	gone, err := s.store.RemoveOtherDevices(c.visitor, c.device)
	if err != nil {
		return err
	}
	s.signOutLive(gone)
	c.json(200, map[string]any{"removed": gone})
	return nil
}

// createAPIKey mints a static credential for one space. Its level defaults
// to post and is capped by the minter's; it can't be owner. It lasts 30
// days unless asked otherwise; no end at all takes an explicit null.
func (s *Server) createAPIKey(c *reqCtx) error {
	var in struct {
		Space  string          `json:"space"`
		Label  string          `json:"label"`
		Access json.RawMessage `json:"access"`
		Until  json.RawMessage `json:"until"`
	}
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	if in.Space == "" {
		return apierr.BadParam("space", "An API key is for one space: give its name as space.")
	}
	if !names.ValidSpace(in.Space) {
		return apierr.InvalidSpace(names.SuggestFor(in.Space))
	}
	label, err := deviceLabel(in.Label)
	if err != nil {
		return err
	}
	level := access.Post
	if in.Access != nil {
		if level, err = access.ParseIn(in.Access, "access", access.Read, access.Post, access.All); err != nil {
			return apierr.BadParam("access", err.Error()+" An API key can't be owner.")
		}
	}
	now := time.Now()
	until := now.Add(apiKeyDefault).UnixMilli()
	untilPtr := &until
	if in.Until != nil {
		if isNull(in.Until) {
			untilPtr = nil
		} else if until, err = access.ParseTime(in.Until, now); err != nil {
			return apierr.BadParam("until", err.Error())
		}
	}
	rights, err := s.store.Rights(in.Space, c.visitor)
	if err != nil {
		return err
	}
	if rights == nil || !rights.Visible() {
		return apierr.NoSpace
	}
	if rights.Level() < level {
		return &apierr.Error{Status: 403, Code: "exceeds_rights", Message: "An API key can't do more than you can: your level in this space is " + rights.Level().String() + "."}
	}
	k, secret, err := s.store.CreateAPIKey(c.visitor, c.device, in.Space, label, level, untilPtr)
	if err != nil {
		return err
	}
	out := map[string]any{"id": k.ID, "key": secret, "label": k.Label, "space": k.Space, "access": k.Access, "until": k.Until, "device": k.Device, "created": k.Created}
	if untilPtr == nil {
		out["warning"] = "This API key never expires. Anyone who sees it can use it until you delete it. A key with an end, or a token signed by a device, is safer."
	}
	c.json(201, out)
	return nil
}
