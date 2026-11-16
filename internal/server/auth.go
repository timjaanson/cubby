package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/auth"
	"github.com/timjaanson/cubby/internal/names"
	"github.com/timjaanson/cubby/internal/store"
)

// Sign-in. Every request may carry one credential, in the Authorization
// header or ?token=:
//
//   - a token: a JWT signed by a device key, valid at most an hour
//   - an API key, cubby_..., for tools that can't sign; one space only
//
// Nothing else is accepted, and nothing permanent is needed by default.

// signedIn is who a credential belongs to.
type signedIn struct {
	visitor, device string
	apiKey          *store.APIKey
	token           *auth.Token   // the parsed token, also when its key is unknown
	err             *apierr.Error // why the credential was refused
}

// origin is the server's public origin, which tokens must name as aud.
func (s *Server) origin(r *http.Request) string {
	if s.cfg.Server.PublicURL != "" {
		return s.cfg.Server.PublicURL
	}
	scheme, host := "http", r.Host
	if r.TLS != nil {
		scheme = "https"
	}
	if s.trusted(remoteAddr(r)) {
		if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
			scheme = p
		}
		if h := r.Header.Get("X-Forwarded-Host"); h != "" {
			host, _, _ = strings.Cut(h, ",")
			host = strings.TrimSpace(host)
		}
	}
	return scheme + "://" + strings.ToLower(host)
}

// credential is the request's token or API key: the header wins over ?token=.
func credential(r *http.Request) string {
	if scheme, cred, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " "); ok && strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(cred)
	}
	return r.URL.Query().Get("token")
}

// signIn checks a credential. Only storage failures are errors; a refused
// credential is in the result's err.
func (s *Server) signIn(r *http.Request, cred string) (signedIn, error) {
	var in signedIn
	switch {
	case cred == "":
		return in, nil
	case names.APIKeyRe.MatchString(cred):
		k, err := s.store.APIKeyBySecret(cred)
		if err != nil {
			return in, err
		}
		if k == nil {
			in.err = apierr.InvalidAPIKey
			return in, nil
		}
		in.visitor, in.device, in.apiKey = k.Visitor, k.Device, k
		return in, nil
	case !auth.LooksLikeJWT(cred):
		in.err = apierr.TokenError("bad_token", "This is neither a token (a JWT, eyJ...) nor an API key (cubby_...).")
		return in, nil
	}
	t, err := auth.Parse(cred)
	if err != nil {
		in.err = refused(err)
		return in, nil
	}
	in.token = t
	if t.Kid == "" {
		if t.JWK == nil {
			in.err = apierr.TokenError("bad_token", "The token header needs kid, the device ID: the JWK thumbprint of its key.")
		}
		return in, nil // a new key; only /visitors and /pair take it
	}
	visitor, key, err := s.store.DeviceKey(t.Kid)
	if err != nil {
		return in, err
	}
	if key == nil {
		in.err = apierr.UnknownKey
		return in, nil
	}
	if err := t.Verify(key, s.origin(r), time.Now()); err != nil {
		in.err = refused(err)
		return in, nil
	}
	in.visitor, in.device = visitor, key.ID
	return in, nil
}

func refused(err error) *apierr.Error {
	if e, ok := err.(*auth.Error); ok {
		return apierr.TokenError(e.Code, e.Message)
	}
	return apierr.TokenError("bad_token", err.Error())
}

// newKey is the key a token introduces in its jwk header, after checking
// the token is signed by it. Registering and pairing take one.
func (s *Server) newKey(c *reqCtx) (*auth.Key, error) {
	t := c.token
	if t == nil {
		if c.tokErr != nil {
			return nil, c.tokErr
		}
		return nil, apierr.TokenError("missing_token", "Send a token signed by the new key, with the public key in its header as jwk.")
	}
	if t.JWK == nil {
		return nil, apierr.TokenError("bad_token", "Put the new public key in the token header as jwk, the way ACME does.")
	}
	key, err := auth.ParseKey(t.JWK)
	if err != nil {
		e := err.(*auth.Error)
		return nil, &apierr.Error{Status: 400, Code: e.Code, Message: e.Message}
	}
	if t.Kid != "" && t.Kid != key.ID {
		return nil, apierr.TokenError("bad_token", "kid must be the key's JWK thumbprint, "+key.ID+".")
	}
	if err := t.Verify(key, s.origin(c.r), time.Now()); err != nil {
		return nil, refused(err)
	}
	return key, nil
}

// wwwAuthenticate is RFC 6750's header for a 401.
func wwwAuthenticate(e *apierr.Error) string {
	if e.Code == "missing_token" {
		return `Bearer realm="cubby"`
	}
	desc := strings.NewReplacer(`"`, `'`, `\`, ``).Replace(e.Message)
	return `Bearer realm="cubby", error="invalid_token", error_description="` + desc + `"`
}
