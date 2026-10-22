package server

// Routes about a space rather than its data: creating it, joining it,
// asking what you may do, and the owner's settings, tokens and grants.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/timjaanson/cubby/internal/access"
	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/names"
	"github.com/timjaanson/cubby/internal/store"
)

const maxJSONBody = 16 << 10

// decodeBody reads a JSON object into v. An empty body is an empty object.
// Unknown fields are refused, so {"use": 1} can't silently become the
// default of 10 uses.
func decodeBody(c *reqCtx, v any) error {
	body, err := io.ReadAll(io.LimitReader(c.r.Body, maxJSONBody+1))
	if err != nil {
		return apierr.BadParam("body", "Could not read the body.")
	}
	if len(body) > maxJSONBody {
		return apierr.BadParam("body", "The body is too large.")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if f, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			f = strings.Trim(f, `"`)
			return apierr.BadParam(f, fmt.Sprintf("Unknown field %q in the body.", f))
		}
		return apierr.BadParam("body", "The body must be a JSON object with the fields this route takes.")
	}
	if dec.More() {
		return apierr.BadParam("body", "The body must be one JSON object.")
	}
	return nil
}

func isNull(raw json.RawMessage) bool { return raw != nil && string(bytes.TrimSpace(raw)) == "null" }

func scopeErr(err error) error { return apierr.InvalidScope(capital(err.Error()) + ".") }

func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// identity returns the request's visitor, or creates one when no token was
// sent. created is the new token, "" when the visitor already existed.
func (s *Server) identity(c *reqCtx) (id, created string, err error) {
	if c.visitor != "" {
		return c.visitor, "", nil
	}
	if c.tokErr != nil {
		return "", "", c.tokErr
	}
	tok, id, err := s.newVisitor(c)
	return id, tok, err
}

func (s *Server) createSpace(c *reqCtx) error {
	var in struct {
		Name   string          `json:"name"`
		Label  string          `json:"label"`
		Public json.RawMessage `json:"public"`
		Read   json.RawMessage `json:"read"`
		Write  json.RawMessage `json:"write"`
	}
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	if in.Label != "" && in.Name != "" {
		return apierr.BadParam("name", "Give a name or a label, not both.")
	}
	if in.Label != "" && !names.ValidLabel(in.Label) {
		return apierr.BadParam("label", fmt.Sprintf("A label is lowercase letters, digits and single hyphens, at most %d characters.", names.MaxLabel))
	}
	if in.Name != "" && !names.ValidSpace(in.Name) {
		return apierr.InvalidSpace(names.SuggestFor(in.Name))
	}
	settings, err := access.ApplyInput(access.Private, in.Public, in.Read, in.Write)
	if err != nil {
		return apierr.InvalidSettings(err.Error())
	}
	if in.Name != "" && s.store.SpaceExists(in.Name) {
		return apierr.SpaceExists(in.Name) // before any limit, so a taken name costs nothing
	}
	if c.visitor != "" {
		if err := s.spaceRate(c, c.visitor); err != nil {
			return err
		}
	}
	id, newToken, err := s.identity(c)
	if err != nil {
		return err
	}
	if newToken != "" {
		if err := s.spaceRate(c, id); err != nil {
			return err
		}
	}
	name, ownerToken, err := s.store.CreateSpace(in.Name, in.Label, settings, id)
	if err != nil {
		return err
	}
	s.lim.misses.remember(c.ip, name, s.lim.misses.now()) // its creator can use it even while out of miss budget
	out := map[string]any{"space": name, "owner": ownerToken, "id": id, "settings": settings}
	if newToken != "" {
		out["token"] = newToken
	}
	c.json(201, out)
	return nil
}

// join redeems an access or owner token. Any failure answers invite_invalid,
// the same for a space that doesn't exist, and costs one miss.
func (s *Server) join(c *reqCtx, space string, rights *access.Rights) error {
	var in struct {
		Token string `json:"token"`
	}
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	if in.Token == "" {
		return apierr.BadParam("token", `join takes {"token": "at_..."} or an owner token, "ot_...".`)
	}
	if s.cfg.RateLimitsOn() {
		if ok, retry := s.lim.misses.allowed(c.ip, space); !ok {
			return apierr.RateLimited("miss_limit", retry)
		}
	}
	fail := func() error {
		if s.cfg.RateLimitsOn() {
			s.lim.misses.charge(c.ip)
		}
		return apierr.InviteInvalid
	}
	if rights == nil || !(names.AccessTokenRe.MatchString(in.Token) || names.OwnerTokenRe.MatchString(in.Token)) {
		return fail()
	}
	if c.visitor == "" && c.tokErr != nil {
		return c.tokErr
	}
	id, newToken := c.visitor, ""
	if id == "" {
		// A new visitor, created first so the grant has someone to belong
		// to, and removed again if the invite turns out to be no good.
		var err error
		if newToken, id, err = s.newVisitor(c); err != nil {
			return err
		}
	}
	g, err := s.store.Redeem(space, id, in.Token)
	if err != nil {
		if newToken != "" {
			s.store.DeleteVisitor(id)
		}
		if err == apierr.InviteInvalid {
			return fail()
		}
		return err
	}
	s.lim.misses.remember(c.ip, space, s.lim.misses.now())
	settings, err := s.store.Settings(space)
	if err != nil {
		return err
	}
	out := map[string]any{"space": space, "id": id, "owner": g.Owner, "allow": g.Allow, "until": g.Until, "settings": settings}
	status := 200
	if newToken != "" {
		out["token"], status = newToken, 201
	}
	c.json(status, out)
	return nil
}

func grantJSON(g access.Grant) map[string]any {
	var until any
	if g.Until != 0 {
		until = g.Until
	}
	allow := g.Allow
	if allow == nil {
		allow = []string{}
	}
	return map[string]any{"id": g.ID, "owner": g.Owner, "allow": allow, "source": g.Source, "until": until}
}

// spaceRoute handles /v1/{space} and its access, tokens, grants and
// owner-token routes. rest is the path after the space name.
func (s *Server) spaceRoute(c *reqCtx, space string, rights *access.Rights, rest []string) error {
	m := c.r.Method
	if len(rest) == 1 && rest[0] == "access" {
		if m != http.MethodGet && m != http.MethodHead {
			return methodErr("GET, HEAD")
		}
		if err := checkParams(c.r); err != nil {
			return err
		}
		var id any
		if c.visitor != "" {
			id = c.visitor
		}
		grants := []map[string]any{}
		for _, g := range rights.Grants {
			grants = append(grants, grantJSON(g))
		}
		c.json(200, map[string]any{"id": id, "owner": rights.Owner, "settings": rights.Settings, "grants": grants})
		return nil
	}

	// Everything else is for owners.
	if c.visitor == "" {
		if c.tokErr != nil {
			return c.tokErr
		}
		return apierr.MissingToken
	}
	if !rights.Owner {
		return apierr.OwnerOnly
	}

	switch {
	case len(rest) == 0:
		switch m {
		case http.MethodPatch:
			return s.patchSettings(c, space, rights.Settings)
		case http.MethodDelete:
			if err := checkParams(c.r); err != nil {
				return err
			}
			if err := s.store.DeleteSpace(space); err != nil {
				return err
			}
			c.json(200, map[string]any{})
			return nil
		}
		return methodErr("PATCH, DELETE")

	case rest[0] == "owner-token" && len(rest) == 1:
		if m != http.MethodPost {
			return methodErr("POST")
		}
		if err := checkParams(c.r); err != nil {
			return err
		}
		tok, err := s.store.RotateOwner(space, c.visitor)
		if err != nil {
			return err
		}
		c.json(200, map[string]any{"owner": tok})
		return nil

	case rest[0] == "tokens" && len(rest) == 1:
		switch m {
		case http.MethodPost:
			if err := checkParams(c.r); err != nil {
				return err
			}
			return s.mintToken(c, space)
		case http.MethodGet, http.MethodHead:
			if err := checkParams(c.r); err != nil {
				return err
			}
			items, err := s.store.ListTokens(space)
			if err != nil {
				return err
			}
			c.json(200, map[string]any{"items": items})
			return nil
		}
		return methodErr("GET, HEAD, POST")

	case rest[0] == "tokens" && len(rest) == 2:
		id := rest[1]
		if !names.ValidTokenID(id) {
			return &apierr.Error{Status: 404, Code: "not_found", Message: "No such access token in this space."}
		}
		if err := checkParams(c.r); err != nil {
			return err
		}
		switch m {
		case http.MethodPatch:
			return s.patchToken(c, space, id)
		case http.MethodDelete:
			if err := s.store.DeleteToken(space, id); err != nil {
				return err
			}
			c.json(200, map[string]any{})
			return nil
		}
		return methodErr("PATCH, DELETE")

	case rest[0] == "grants" && len(rest) == 1:
		switch m {
		case http.MethodGet, http.MethodHead:
			if err := checkParams(c.r, "user"); err != nil {
				return err
			}
			user := c.r.URL.Query().Get("user")
			if user != "" && !names.ValidVisitor(user) {
				return apierr.BadParam("user", "user is a visitor ID like u_k7m2x9q4p1r8t3w6.")
			}
			items, err := s.store.ListGrants(space, user)
			if err != nil {
				return err
			}
			c.json(200, map[string]any{"items": items})
			return nil
		case http.MethodPost:
			if err := checkParams(c.r); err != nil {
				return err
			}
			return s.addGrant(c, space)
		case http.MethodDelete:
			if err := checkParams(c.r, "user"); err != nil {
				return err
			}
			user := c.r.URL.Query().Get("user")
			if !names.ValidVisitor(user) {
				return apierr.BadParam("user", "To remove a visitor's grants, give ?user= with their ID. To remove one grant, DELETE grants/{id}.")
			}
			n, err := s.store.DeleteUserGrants(space, user)
			if err != nil {
				return err
			}
			c.json(200, map[string]any{"removed": n})
			return nil
		}
		return methodErr("GET, HEAD, POST, DELETE")

	case rest[0] == "grants" && len(rest) == 2:
		if m != http.MethodDelete {
			return methodErr("DELETE")
		}
		if err := checkParams(c.r); err != nil {
			return err
		}
		if !names.ValidGrantID(rest[1]) {
			return &apierr.Error{Status: 404, Code: "not_found", Message: "No such grant in this space."}
		}
		if err := s.store.DeleteGrant(space, rest[1]); err != nil {
			return err
		}
		c.json(200, map[string]any{})
		return nil
	}
	return noRoute
}

func (s *Server) patchSettings(c *reqCtx, space string, cur access.Settings) error {
	if err := checkParams(c.r); err != nil {
		return err
	}
	var in struct {
		Public json.RawMessage `json:"public"`
		Read   json.RawMessage `json:"read"`
		Write  json.RawMessage `json:"write"`
	}
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	st, err := access.ApplyInput(cur, in.Public, in.Read, in.Write)
	if err != nil {
		return apierr.InvalidSettings(err.Error())
	}
	if err := s.store.SetSettings(space, st); err != nil {
		return err
	}
	c.json(200, map[string]any{"settings": st})
	return nil
}

// Defaults for a new access token: a member, 10 uses, 7 days.
const (
	defaultTokenUses = 10
	defaultTokenTTL  = 7 * 24 * time.Hour
)

func (s *Server) mintToken(c *reqCtx, space string) error {
	var in struct {
		Label    string          `json:"label"`
		Allow    json.RawMessage `json:"allow"`
		Uses     json.RawMessage `json:"uses"`
		Until    json.RawMessage `json:"until"`
		GrantFor json.RawMessage `json:"grantFor"`
	}
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	if utf8.RuneCountInString(in.Label) > 63 {
		return apierr.BadParam("label", "A label is at most 63 characters.")
	}
	allow, err := access.ParseAllow(in.Allow)
	if err != nil {
		return scopeErr(err)
	}
	now := time.Now()
	spec := store.TokenSpec{Label: in.Label, Allow: allow}

	uses := int64(defaultTokenUses)
	spec.Uses = &uses
	switch {
	case isNull(in.Uses):
		spec.Uses = nil
	case in.Uses != nil:
		if json.Unmarshal(in.Uses, &uses) != nil || uses < 1 {
			return apierr.BadParam("uses", "uses is how many visitors can redeem the token, 1 or more, or null for no limit.")
		}
	}

	until := now.Add(defaultTokenTTL).UnixMilli()
	spec.Until = &until
	switch {
	case isNull(in.Until):
		spec.Until = nil
	case in.Until != nil:
		if until, err = access.ParseTime(in.Until, now); err != nil {
			return apierr.BadParam("until", "until: "+err.Error()+".")
		}
	}

	if in.GrantFor != nil && !isNull(in.GrantFor) {
		var d string
		dur, err := time.Duration(0), error(nil)
		if json.Unmarshal(in.GrantFor, &d) == nil {
			dur, err = access.ParseDuration(d)
		} else {
			err = fmt.Errorf("not a duration")
		}
		if err != nil || dur <= 0 {
			return apierr.BadParam("grantFor", `grantFor is a duration like "30d", or null for grants that last until revoked.`)
		}
		ms := dur.Milliseconds()
		spec.GrantFor = &ms
	}

	info, token, err := s.store.CreateToken(space, spec)
	if err != nil {
		return err
	}
	c.json(201, map[string]any{"id": info.ID, "token": token, "label": info.Label, "allow": info.Allow,
		"uses": info.UsesLeft, "until": info.Until, "grantFor": info.GrantFor})
	return nil
}

func (s *Server) patchToken(c *reqCtx, space, id string) error {
	var in struct {
		Uses  json.RawMessage `json:"uses"`
		Until json.RawMessage `json:"until"`
	}
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	var ch store.TokenChange
	if in.Uses != nil {
		ch.SetUses = true
		if !isNull(in.Uses) {
			var n int64
			if json.Unmarshal(in.Uses, &n) != nil || n < 0 {
				return apierr.BadParam("uses", "uses is how many more visitors can redeem the token, 0 to stop new ones, or null for no limit.")
			}
			ch.Uses = &n
		}
	}
	if in.Until != nil {
		ch.SetUntil = true
		if !isNull(in.Until) {
			ms, err := access.ParseTime(in.Until, time.Now())
			if err != nil {
				return apierr.BadParam("until", "until: "+err.Error()+".")
			}
			ch.Until = &ms
		}
	}
	info, err := s.store.UpdateToken(space, id, ch)
	if err != nil {
		return err
	}
	c.json(200, info)
	return nil
}

func (s *Server) addGrant(c *reqCtx, space string) error {
	var in struct {
		User  string          `json:"user"`
		Allow json.RawMessage `json:"allow"`
		Owner bool            `json:"owner"`
		Until json.RawMessage `json:"until"`
	}
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	if !names.ValidVisitor(in.User) {
		return apierr.BadParam("user", "user is a visitor ID like u_k7m2x9q4p1r8t3w6.")
	}
	if in.Owner && in.Allow != nil {
		return apierr.BadParam("allow", "An owner grant has every right. Give owner or allow, not both.")
	}
	allow, err := access.ParseAllow(in.Allow)
	if err != nil {
		return scopeErr(err)
	}
	var until *int64
	if in.Until != nil && !isNull(in.Until) {
		ms, err := access.ParseTime(in.Until, time.Now())
		if err != nil {
			return apierr.BadParam("until", "until: "+err.Error()+".")
		}
		until = &ms
	}
	g, err := s.store.AddGrant(space, in.User, in.Owner, allow, until)
	if err != nil {
		return err
	}
	c.json(201, g)
	return nil
}
