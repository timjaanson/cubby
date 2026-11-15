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

	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/names"
	"github.com/timjaanson/cubby/internal/store"
)

// Device links: one visitor on several devices. A device makes a one-use
// code, another device redeems it and becomes the same visitor. Nothing
// here is about a space.

const (
	maxDeviceLabel = 64
	maxLinkWait    = 60 * time.Second
)

// linkWaits wakes status long-polls when a link is used or cancelled.
type linkWaits struct {
	mu sync.Mutex
	m  map[string]chan struct{}
}

func (w *linkWaits) wait(id string) <-chan struct{} {
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

func (w *linkWaits) fire(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ch := w.m[id]; ch != nil {
		close(ch)
		delete(w.m, id)
	}
}

func (s *Server) identityRoute(c *reqCtx, seg []string) error {
	m := c.r.Method
	switch {
	case seg[0] == "links" && len(seg) == 1:
		if m != http.MethodPost {
			return methodErr("POST")
		}
		if err := checkParams(c.r); err != nil {
			return err
		}
		return s.createLink(c)
	case seg[0] == "links" && len(seg) == 2 && seg[1] == "redeem":
		if m != http.MethodPost {
			return methodErrMsg("POST", "POST the code here, as the body or as {\"code\": \"...\"}.")
		}
		if err := checkParams(c.r); err != nil {
			return err
		}
		return s.redeemLink(c)
	case seg[0] == "links" && len(seg) == 2:
		if !names.ValidLinkID(seg[1]) {
			return &apierr.Error{Status: 404, Code: "not_found", Message: "No such link. Link IDs look like l_x4k2m7q9; the code itself is redeemed with POST /v1/links/redeem."}
		}
		switch m {
		case http.MethodGet, http.MethodHead:
			if err := checkParams(c.r, "wait"); err != nil {
				return err
			}
			return s.linkStatus(c, seg[1])
		case http.MethodDelete:
			if err := checkParams(c.r); err != nil {
				return err
			}
			return s.cancelLink(c, seg[1])
		}
		return methodErr("GET, HEAD, DELETE")
	case seg[0] == "devices" && len(seg) == 1:
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
				return apierr.BadParam("others", "DELETE /v1/devices signs out every other device and needs ?others=1. To sign out one, DELETE /v1/devices/{id}.")
			}
			return s.signOutOthers(c)
		}
		return methodErr("GET, HEAD, DELETE")
	case seg[0] == "devices" && len(seg) == 2:
		id, err := c.needVisitor()
		if err != nil {
			return err
		}
		dev := seg[1]
		if dev == "me" {
			dev = c.device
		} else if !names.ValidDeviceID(dev) {
			return &apierr.Error{Status: 404, Code: "not_found", Message: "Device IDs look like d_k7m2x9q4, or use me for this one."}
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
	}
	return noRoute
}

func deviceLabel(l string) (string, error) {
	l = strings.TrimSpace(l)
	if !utf8.ValidString(l) || utf8.RuneCountInString(l) > maxDeviceLabel || strings.ContainsFunc(l, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", apierr.BadParam("label", "A device label is up to 64 characters of plain text.")
	}
	return l, nil
}

func linkJSON(l *store.Link) map[string]any {
	return map[string]any{"id": l.ID, "state": l.State, "created": l.Created, "expires": l.Expires, "ended": l.Ended, "device": l.Device}
}

func (s *Server) createLink(c *reqCtx) error {
	id, err := c.needVisitor()
	if err != nil {
		return err
	}
	var in struct {
		TTL *int64 `json:"ttl"`
	}
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	ttl := int64(store.LinkTTLDefault)
	if in.TTL != nil {
		ttl = *in.TTL * 1000
		if ttl < store.LinkTTLMin || ttl > store.LinkTTLMax {
			return apierr.BadParam("ttl", "ttl is 30 to 3600 seconds. The default is 600.")
		}
	}
	if err := s.rate(s.lim.linksPerVisitor, id, 1, "rate_limited"); err != nil {
		return err
	}
	l, code, err := s.store.CreateLink(id, c.device, ttl)
	if err != nil {
		return err
	}
	out := linkJSON(l)
	out["code"], out["display"] = code, names.DisplayLinkCode(code)
	c.json(201, out)
	return nil
}

func (s *Server) linkStatus(c *reqCtx, linkID string) error {
	id, err := c.needVisitor()
	if err != nil {
		return err
	}
	if err := s.rate(s.lim.readsPerIP, c.ip, 1, "rate_limited"); err != nil {
		return err
	}
	var wait time.Duration
	if c.r.URL.Query().Has("wait") {
		n, ok := parseUint(c.r.URL.Query().Get("wait"))
		if !ok || n > int64(maxLinkWait/time.Second) {
			return apierr.BadParam("wait", "wait is 0 to 60 seconds.")
		}
		wait = time.Duration(n) * time.Second
	}
	ch := s.links.wait(linkID) // before reading, so a change in between still wakes us
	l, err := s.store.GetLink(id, linkID)
	if err != nil {
		return err
	}
	if l == nil {
		return &apierr.Error{Status: 404, Code: "not_found", Message: "This visitor has no link with that ID."}
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
		if l, err = s.store.GetLink(id, linkID); err != nil {
			return err
		}
	}
	c.json(200, linkJSON(l))
	return nil
}

func (s *Server) cancelLink(c *reqCtx, linkID string) error {
	id, err := c.needVisitor()
	if err != nil {
		return err
	}
	l, err := s.store.CancelLink(id, linkID)
	if err != nil {
		return err
	}
	if l == nil {
		return &apierr.Error{Status: 404, Code: "not_found", Message: "This visitor has no link with that ID."}
	}
	s.links.fire(linkID)
	c.json(200, linkJSON(l))
	return nil
}

// redeemLink takes the code as a JSON object or as the whole body, so
// curl -d CODE works.
func (s *Server) redeemLink(c *reqCtx) error {
	body, err := io.ReadAll(io.LimitReader(c.r.Body, maxJSONBody+1))
	if err != nil || len(body) > maxJSONBody {
		return apierr.BadParam("body", "The body is the link code, or {\"code\": \"...\", \"label\": \"...\"}.")
	}
	var in struct {
		Code  string `json:"code"`
		Label string `json:"label"`
	}
	if t := bytes.TrimSpace(body); len(t) > 0 && t[0] == '{' {
		dec := json.NewDecoder(bytes.NewReader(t))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return apierr.BadParam("body", "The body is the link code, or {\"code\": \"...\", \"label\": \"...\"}.")
		}
	} else {
		in.Code = string(t)
	}
	label, err := deviceLabel(in.Label)
	if err != nil {
		return err
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
		return store.ErrLinkUnknown
	}
	code := names.NormalizeLinkCode(in.Code)
	if code == "" {
		if strings.TrimSpace(in.Code) == "" {
			return apierr.BadParam("code", "Send the link code as the body, or as {\"code\": \"...\"}.")
		}
		return miss()
	}
	// An unknown or expired token is ignored: the device simply has no
	// identity to leave.
	r, err := s.store.RedeemLink(code, label, c.visitor, c.device)
	if err == store.ErrLinkUnknown {
		return miss()
	}
	if err != nil && r == nil {
		return err
	}
	s.links.fire(r.LinkID)
	if c.device != "" {
		s.signOutLive([]string{c.device})
	}
	c.visitor, c.device = r.Visitor, r.Device
	var prev any
	if r.Previous != "" {
		prev = r.Previous
	}
	c.json(201, map[string]any{"token": r.Token, "id": r.Visitor, "device": r.Device, "previous": prev, "deleted": r.Deleted})
	return nil
}

func (s *Server) listDevices(c *reqCtx) error {
	id, err := c.needVisitor()
	if err != nil {
		return err
	}
	ds, err := s.store.Devices(id, c.device)
	if err != nil {
		return err
	}
	c.json(200, map[string]any{"id": id, "device": c.device, "items": ds})
	return nil
}

func (s *Server) signOutOthers(c *reqCtx) error {
	id, err := c.needVisitor()
	if err != nil {
		return err
	}
	gone, err := s.store.RemoveOtherDevices(id, c.device)
	if err != nil {
		return err
	}
	s.signOutLive(gone)
	c.json(200, map[string]any{"removed": gone})
	return nil
}
