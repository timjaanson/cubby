package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/timjaanson/cubby/internal/access"
	"github.com/timjaanson/cubby/internal/apierr"
)

type reqCtx struct {
	w       http.ResponseWriter
	r       *http.Request
	ip      string // rate-limit key: the IPv4 address or the IPv6 /64
	visitor string // "" when no valid token was sent
	tokErr  *apierr.Error
	status  int

	op access.Op // what route checked, for watches that check again later

	// Set for a message on a live connection.
	live        *liveConn
	liveID      json.RawMessage
	liveStarted bool // a watch took over the reply
	will        willMode
}

// willMode is how a pulse request serves a last will.
type willMode int

const (
	willNone     willMode = iota
	willRegister          // check everything and charge, but don't send
	willFire              // send, without charging: registering paid for it
)

func (c *reqCtx) header() http.Header { return c.w.Header() }

func (c *reqCtx) json(status int, v any) {
	c.status = status
	c.header().Set("Content-Type", "application/json; charset=utf-8")
	c.w.WriteHeader(status)
	if c.r.Method == http.MethodHead {
		return
	}
	b, _ := json.Marshal(v)
	c.w.Write(append(b, '\n'))
}

func (c *reqCtx) fail(err error) {
	ae, body := apiErrBody(err)
	if ae.Status == 429 {
		c.header().Set("Retry-After", fmt.Sprint(ae.Extra["retryAfter"]))
	}
	if ae.Status == 405 {
		c.header().Set("Allow", fmt.Sprint(ae.Extra["allow"]))
	}
	c.json(ae.Status, body)
}

// apiErrBody turns any error into its API error and JSON body.
func apiErrBody(err error) (*apierr.Error, map[string]any) {
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		ae = &apierr.Error{Status: 500, Code: "internal", Message: "Internal server error."}
		log.Printf("internal error: %v", err)
	}
	body := map[string]any{"error": ae.Code, "message": ae.Message}
	for k, v := range ae.Extra {
		body[k] = v
	}
	return ae, body
}

// needVisitor returns the visitor or the right 401.
func (c *reqCtx) needVisitor() (string, error) {
	if c.visitor != "" {
		return c.visitor, nil
	}
	if c.tokErr != nil {
		return "", c.tokErr
	}
	return "", apierr.MissingToken
}

func (s *Server) clientIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, _ := netip.ParseAddr(host)
	addr = addr.Unmap()
	if !s.trusted(addr) {
		return addr
	}
	// Walk X-Forwarded-For from the right, skipping trusted proxies.
	xff := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(xff) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(xff[i]))
		if err != nil {
			break
		}
		addr = a.Unmap()
		if !s.trusted(addr) {
			break
		}
	}
	return addr
}

func (s *Server) trusted(a netip.Addr) bool {
	for _, p := range s.cfg.Trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func ipKey(a netip.Addr) string {
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

func (s *Server) rate(l *limiter, key string, n float64, code string) error {
	if !s.cfg.RateLimitsOn() {
		return nil
	}
	if ok, retry := l.take(key, n); !ok {
		return apierr.RateLimited(code, retry)
	}
	return nil
}

type statusWriter struct {
	http.ResponseWriter
	status int
	before func()
}

func (w *statusWriter) WriteHeader(code int) {
	if w.before != nil {
		w.before()
		w.before = nil
	}
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func shortHash(id string) string {
	if id == "" {
		return "-"
	}
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:4])
}

// checkParams rejects query parameters a route doesn't take, and any given
// twice. A typo like ifversion=0 would otherwise turn a conditional write into
// an unconditional one. token is accepted everywhere, and _ is ignored so
// cache-busting libraries work.
func checkParams(r *http.Request, allowed ...string) error {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return apierr.BadParam("query", "The query string is malformed.")
	}
	for k, vs := range q {
		if k == "token" || k == "_" {
			continue
		}
		known := false
		for _, a := range allowed {
			known = known || k == a
		}
		if !known {
			msg := fmt.Sprintf("Unknown parameter %q here.", k)
			if len(allowed) > 0 {
				msg += " This route takes: " + strings.Join(allowed, ", ") + "."
			}
			return apierr.BadParam(k, msg)
		}
		if len(vs) > 1 {
			return apierr.BadParam(k, fmt.Sprintf("Parameter %q is given more than once.", k))
		}
	}
	return nil
}

// parseUint accepts plain decimal digits only: no sign, no spaces, no hex,
// at most 16 digits (versions stay below 2^53).
func parseUint(s string) (int64, bool) {
	if s == "" || len(s) > 16 {
		return 0, false
	}
	var n int64
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int64(s[i]-'0')
	}
	return n, true
}

func methodErr(allow string) error {
	return methodErrMsg(allow, "Method not allowed here.")
}

func methodErrMsg(allow, msg string) error {
	return &apierr.Error{Status: 405, Code: "method_not_allowed", Message: msg, Extra: map[string]any{"allow": allow}}
}
