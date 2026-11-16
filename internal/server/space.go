package server

// Routes about a space rather than its data: creating it, joining it,
// asking what you may do, invites, members and the owner's settings.

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

func (s *Server) createSpace(c *reqCtx) error {
	var in struct {
		Name   string          `json:"name"`
		Label  string          `json:"label"`
		Anyone json.RawMessage `json:"anyone"`
		Invite json.RawMessage `json:"invite"`
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
	settings, err := access.ApplyInput(access.Default, in.Anyone, in.Invite)
	if err != nil {
		return apierr.InvalidSettings(err.Error())
	}
	if in.Name != "" && s.store.SpaceExists(in.Name) {
		return apierr.SpaceExists(in.Name) // before any limit, so a taken name costs nothing
	}
	id, err := c.needVisitor()
	if err != nil {
		return err
	}
	if err := s.spaceRate(c, id); err != nil {
		return err
	}
	name, err := s.store.CreateSpace(in.Name, in.Label, settings, id)
	if err != nil {
		return err
	}
	s.lim.misses.remember(c.ip, name, s.lim.misses.now()) // its creator can use it even while out of miss budget
	c.json(201, map[string]any{"space": name, "id": id, "settings": settings})
	return nil
}

// join redeems an invite. Any failure answers invite_invalid, the same for
// a space that doesn't exist, and costs one miss.
func (s *Server) join(c *reqCtx, space string, rights *access.Rights) error {
	var in struct {
		Invite string `json:"invite"`
	}
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	if in.Invite == "" {
		return apierr.BadParam("invite", `join takes {"invite": "inv_..."}.`)
	}
	id, err := c.needVisitor()
	if err != nil {
		return err
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
	if rights == nil || !names.InviteRe.MatchString(in.Invite) {
		return fail()
	}
	j, err := s.store.Redeem(space, id, in.Invite)
	if err != nil {
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
	c.json(200, map[string]any{"space": space, "id": id, "access": j.Access, "until": j.Until, "settings": settings})
	return nil
}

// spaceRoute handles /v1/{space} and its access, invites and members
// routes. rest is the path after the space name.
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
		if err := s.rate(s.lim.readsPerIP, c.ip, 1, "rate_limited"); err != nil {
			return err
		}
		usage, err := s.accessUsage(space, c.visitor)
		if err != nil {
			return err
		}
		c.json(200, map[string]any{"id": id, "access": rights.Level(), "canInvite": c.visitor != "" && rights.CanInvite(),
			"settings": rights.Settings, "usage": usage, "limits": s.spaceLimits(space)})
		return nil
	}

	// Everything else needs a visitor.
	if c.visitor == "" {
		if c.tokErr != nil {
			return c.tokErr
		}
		return apierr.MissingToken
	}
	owner := rights.Owner()
	route := ""
	if len(rest) > 0 {
		route = rest[0]
	}
	switch {
	case route == "invites":
		if !rights.CanInvite() {
			return apierr.OwnerOnly
		}
	case route == "members":
		// Decided per method below.
	case !owner:
		return apierr.OwnerOnly
	}
	// Members see and change only the invites they minted.
	by := ""
	if !owner {
		by = c.visitor
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

	case rest[0] == "invites" && len(rest) == 1:
		switch m {
		case http.MethodPost:
			if err := checkParams(c.r); err != nil {
				return err
			}
			return s.mintInvite(c, space, rights)
		case http.MethodGet, http.MethodHead:
			if err := checkParams(c.r); err != nil {
				return err
			}
			items, err := s.store.ListInvites(space, by)
			if err != nil {
				return err
			}
			c.json(200, map[string]any{"items": items})
			return nil
		}
		return methodErr("GET, HEAD, POST")

	case rest[0] == "invites" && len(rest) == 2:
		id := rest[1]
		if !names.ValidInviteID(id) {
			return &apierr.Error{Status: 404, Code: "not_found", Message: "No such invite in this space."}
		}
		if err := checkParams(c.r); err != nil {
			return err
		}
		switch m {
		case http.MethodPatch:
			return s.patchInvite(c, space, id, by)
		case http.MethodDelete:
			if err := s.store.DeleteInvite(space, id, by); err != nil {
				return err
			}
			c.json(200, map[string]any{})
			return nil
		}
		return methodErr("PATCH, DELETE")

	case rest[0] == "members" && len(rest) == 1:
		if m != http.MethodGet && m != http.MethodHead {
			return methodErr("GET, HEAD")
		}
		if err := checkParams(c.r); err != nil {
			return err
		}
		if rights.Member < access.Read {
			return notMember
		}
		items, err := s.store.Members(space, owner)
		if err != nil {
			return err
		}
		if owner {
			c.json(200, map[string]any{"items": items})
			return nil
		}
		// Members see who is in the space and at what level, nothing about
		// invites or timing.
		type seen struct {
			ID     string       `json:"id"`
			Access access.Level `json:"access"`
			Since  int64        `json:"since"`
		}
		out := make([]seen, len(items))
		for i, it := range items {
			out[i] = seen{it.ID, it.Access, it.Since}
		}
		c.json(200, map[string]any{"items": out})
		return nil

	case rest[0] == "members" && len(rest) == 2:
		user := rest[1]
		if user == "me" {
			user = c.visitor
		}
		if !names.ValidVisitor(user) {
			return apierr.BadParam("id", "A member is a visitor ID like u_k7m2x9q4p1r8t3w6, or me.")
		}
		if err := checkParams(c.r); err != nil {
			return err
		}
		switch m {
		case http.MethodPut:
			if !owner {
				return apierr.OwnerOnly
			}
			return s.setMember(c, space, user)
		case http.MethodDelete:
			// Owners remove anyone; everyone may leave.
			if !owner && user != c.visitor {
				return apierr.OwnerOnly
			}
			removed, err := s.store.RemoveMember(space, user)
			if err != nil {
				return err
			}
			c.json(200, map[string]any{"removed": removed})
			return nil
		}
		return methodErr("PUT, DELETE")
	}
	return noRoute
}

var notMember = &apierr.Error{Status: 403, Code: "not_member", Message: "Only members of this space can list its members."}

func (s *Server) patchSettings(c *reqCtx, space string, cur access.Settings) error {
	if err := checkParams(c.r); err != nil {
		return err
	}
	var in struct {
		Anyone json.RawMessage `json:"anyone"`
		Invite json.RawMessage `json:"invite"`
	}
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	st, err := access.ApplyInput(cur, in.Anyone, in.Invite)
	if err != nil {
		return apierr.InvalidSettings(err.Error())
	}
	if err := s.store.SetSettings(space, st); err != nil {
		return err
	}
	c.json(200, map[string]any{"settings": st})
	return nil
}

// Defaults for a new invite: 10 uses, 7 days.
const (
	defaultInviteUses = 10
	defaultInviteTTL  = 7 * 24 * time.Hour
)

func (s *Server) mintInvite(c *reqCtx, space string, rights *access.Rights) error {
	var in struct {
		Label    string          `json:"label"`
		Access   json.RawMessage `json:"access"`
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
	// The default is all, or the inviter's own level if that's lower.
	level := min(access.All, rights.Member)
	if in.Access != nil {
		var err error
		if level, err = access.ParseIn(in.Access, "access", access.Read, access.Post, access.All); err != nil {
			return apierr.BadParam("access", err.Error())
		}
		if !rights.CanGive(level) {
			return &apierr.Error{Status: 403, Code: "exceeds_rights",
				Message: fmt.Sprintf("An invite can give at most your own level, %q.", rights.Member.String())}
		}
	}
	now := time.Now()
	spec := store.InviteSpec{By: c.visitor, Label: in.Label, Access: level}

	uses := int64(defaultInviteUses)
	spec.Uses = &uses
	switch {
	case isNull(in.Uses):
		spec.Uses = nil
	case in.Uses != nil:
		if json.Unmarshal(in.Uses, &uses) != nil || uses < 1 {
			return apierr.BadParam("uses", "uses is how many visitors can redeem the invite, 1 or more, or null for no limit.")
		}
	}

	until := now.Add(defaultInviteTTL).UnixMilli()
	spec.Until = &until
	switch {
	case isNull(in.Until):
		spec.Until = nil
	case in.Until != nil:
		var err error
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

	info, token, err := s.store.CreateInvite(space, spec)
	if err != nil {
		return err
	}
	c.json(201, map[string]any{"id": info.ID, "invite": token, "label": info.Label, "access": info.Access,
		"uses": info.UsesLeft, "until": info.Until, "grantFor": info.GrantFor})
	return nil
}

func (s *Server) patchInvite(c *reqCtx, space, id, by string) error {
	var in struct {
		Uses  json.RawMessage `json:"uses"`
		Until json.RawMessage `json:"until"`
	}
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	var ch store.InviteChange
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
	info, err := s.store.UpdateInvite(space, id, by, ch)
	if err != nil {
		return err
	}
	c.json(200, info)
	return nil
}

// setMember gives a visitor exactly one level, replacing their grants, or
// bans them. A ban closes their live connections to the space.
func (s *Server) setMember(c *reqCtx, space, user string) error {
	var in struct {
		Access json.RawMessage `json:"access"`
		Until  json.RawMessage `json:"until"`
	}
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	level, err := access.ParseIn(in.Access, "access", access.Banned, access.Read, access.Post, access.All, access.Owner)
	if err != nil {
		return apierr.BadParam("access", err.Error())
	}
	if level == access.Banned && user == c.visitor {
		return apierr.BadParam("access", "You can't ban yourself.")
	}
	var until *int64
	if in.Until != nil && !isNull(in.Until) {
		ms, err := access.ParseTime(in.Until, time.Now())
		if err != nil {
			return apierr.BadParam("until", "until: "+err.Error()+".")
		}
		until = &ms
	}
	mem, err := s.store.SetMember(space, user, c.visitor, level, until)
	if err != nil {
		return err
	}
	if level == access.Banned {
		var u int64
		if until != nil {
			u = *until
		}
		s.closeBanned(space, user, u)
	}
	c.json(200, mem)
	return nil
}
