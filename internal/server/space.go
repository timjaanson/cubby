package server

// Routes about a space rather than its data: creating it, joining it,
// asking what you may do, invites, members and the owner's settings.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
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
		Name     string          `json:"name"`
		Prefix   string          `json:"prefix"`
		Anyone   json.RawMessage `json:"anyone"`
		Inviters json.RawMessage `json:"inviters"`
	}
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	if in.Prefix != "" && in.Name != "" {
		return apierr.BadParam("name", "Give a name or a prefix, not both.")
	}
	if in.Prefix != "" && !names.ValidLabel(in.Prefix) {
		return apierr.BadParam("prefix", fmt.Sprintf("A prefix is lowercase letters, digits and single hyphens, at most %d characters.", names.MaxLabel))
	}
	if in.Name != "" && !names.ValidSpace(in.Name) {
		return apierr.InvalidSpace(names.SuggestFor(in.Name))
	}
	settings, err := access.ApplyInput(access.Default, in.Anyone, in.Inviters)
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
	name, err := s.store.CreateSpace(in.Name, in.Prefix, settings, id)
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
	c.json(200, map[string]any{"space": space, "id": id, "access": j.Access, "expires": j.Until, "settings": settings})
	return nil
}

// spaceRoute handles /v1/{space} and its access, invites and members
// routes. rest is the path after the space name.
func (s *Server) spaceRoute(c *reqCtx, space string, rights *access.Rights, rest []string) error {
	m := c.r.Method
	if len(rest) == 0 && (m == http.MethodGet || m == http.MethodHead) {
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
		var expires any
		switch {
		case rights.Banned && rights.BannedUntil != 0:
			expires = rights.BannedUntil
		case !rights.Banned && rights.Member > access.None && rights.Member >= rights.Settings.Anyone && rights.MemberUntil != 0:
			expires = rights.MemberUntil
		}
		c.json(200, map[string]any{"space": space, "id": id, "access": rights.Level(), "expires": expires,
			"canInvite": c.visitor != "" && rights.CanInvite(),
			"settings":  rights.Settings, "usage": usage, "limits": s.spaceLimits(space)})
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
		return methodErr("GET, HEAD, PATCH, DELETE")

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
		Anyone   json.RawMessage `json:"anyone"`
		Inviters json.RawMessage `json:"inviters"`
	}
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	st, err := access.ApplyInput(cur, in.Anyone, in.Inviters)
	if err != nil {
		return apierr.InvalidSettings(err.Error())
	}
	if err := s.store.SetSettings(space, st); err != nil {
		return err
	}
	c.json(200, map[string]any{"settings": st})
	return nil
}

// Defaults for a new invite: any number of uses for 7 days. An owner
// invite is always one use, for at most 7 days.
const (
	defaultInviteTTL  = 7 * 24 * time.Hour
	maxOwnerInviteTTL = 7 * 24 * time.Hour
)

func ownerInviteUses() error {
	return apierr.BadParam("uses", "An owner invite is for one visitor: uses is 1.")
}

func ownerInviteTTL() error {
	return apierr.BadParam("ttl", "An owner invite lasts at most 7 days.")
}

// ttlField reads an optional duration field from a body. set reports that
// the field was there; d is 0 for null (no end).
func ttlField(raw json.RawMessage, name string) (d time.Duration, set bool, err error) {
	if raw == nil {
		return 0, false, nil
	}
	if isNull(raw) {
		return 0, true, nil
	}
	d, err = access.TTL(raw)
	if err != nil {
		return 0, true, apierr.BadParam(name, name+": "+err.Error()+", or null for no end.")
	}
	return d, true, nil
}

func expiresIn(d time.Duration) *int64 {
	if d == 0 {
		return nil
	}
	ms := time.Now().Add(d).UnixMilli()
	return &ms
}

func (s *Server) mintInvite(c *reqCtx, space string, rights *access.Rights) error {
	var in struct {
		Label     string          `json:"label"`
		Access    json.RawMessage `json:"access"`
		Uses      json.RawMessage `json:"uses"`
		TTL       json.RawMessage `json:"ttl"`
		AccessTTL json.RawMessage `json:"accessTtl"`
	}
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	if utf8.RuneCountInString(in.Label) > 63 {
		return apierr.BadParam("label", "A label is at most 63 characters.")
	}
	level, err := s.giveLevel(in.Access, rights, true)
	if err != nil {
		return err
	}
	spec := store.InviteSpec{By: c.visitor, Label: in.Label, Access: level}

	owner := level == access.Owner
	if owner {
		one := int64(1)
		spec.Uses = &one
	}
	if owner && isNull(in.Uses) {
		return ownerInviteUses()
	}
	if in.Uses != nil && !isNull(in.Uses) {
		var uses int64
		if json.Unmarshal(in.Uses, &uses) != nil || uses < 1 {
			return apierr.BadParam("uses", "uses is how many visitors can redeem the invite, 1 or more, or null for no limit.")
		}
		spec.Uses = &uses
	}
	if owner && (spec.Uses == nil || *spec.Uses != 1) {
		return ownerInviteUses()
	}

	ttl, set, err := ttlField(in.TTL, "ttl")
	if err != nil {
		return err
	}
	if !set {
		ttl = defaultInviteTTL
	}
	if owner && (ttl == 0 || ttl > maxOwnerInviteTTL) {
		return ownerInviteTTL()
	}
	spec.Until = expiresIn(ttl)

	if grant, _, err := ttlField(in.AccessTTL, "accessTtl"); err != nil {
		return err
	} else if grant > 0 {
		ms := grant.Milliseconds()
		spec.GrantFor = &ms
	}

	info, token, err := s.store.CreateInvite(space, spec)
	if err != nil {
		return err
	}
	c.json(201, inviteOut{Invite: info, Token: token})
	return nil
}

type inviteOut struct {
	store.Invite
	Token string `json:"invite"`
}

// giveLevel reads the access a member hands out with an invite or an API
// key. Without one it's all, or the giver's own level if that's lower.
// Nobody gives more than they have.
func (s *Server) giveLevel(raw json.RawMessage, rights *access.Rights, ownerOK bool) (access.Level, error) {
	level := min(access.All, rights.Member)
	if raw == nil {
		return level, nil
	}
	allowed := []access.Level{access.Read, access.Post, access.All}
	if ownerOK {
		allowed = append(allowed, access.Owner)
	}
	level, err := access.ParseIn(raw, "access", allowed...)
	if err != nil {
		return 0, apierr.BadParam("access", err.Error())
	}
	if !rights.CanGive(level) {
		return 0, &apierr.Error{Status: 403, Code: "exceeds_rights",
			Message: fmt.Sprintf("You can give at most your own level, %q.", rights.Member.String())}
	}
	return level, nil
}

func (s *Server) patchInvite(c *reqCtx, space, id, by string) error {
	var in struct {
		Uses json.RawMessage `json:"uses"`
		TTL  json.RawMessage `json:"ttl"`
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
				return apierr.BadParam("uses", "uses is how many more visitors can redeem the invite, 0 to stop new ones, or null for no limit.")
			}
			ch.Uses = &n
		}
	}
	ttl, set, err := ttlField(in.TTL, "ttl")
	if err != nil {
		return err
	}
	if set {
		ch.SetUntil = true
		ch.Until = expiresIn(ttl)
	}
	// An owner invite stays one use within 7 days; uses 0 stops it.
	ch.Check = func(l access.Level) error {
		if l != access.Owner {
			return nil
		}
		if ch.SetUses && (ch.Uses == nil || *ch.Uses > 1) {
			return ownerInviteUses()
		}
		if set && (ttl == 0 || ttl > maxOwnerInviteTTL) {
			return ownerInviteTTL()
		}
		return nil
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
		TTL    json.RawMessage `json:"ttl"`
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
	ttl, _, err := ttlField(in.TTL, "ttl")
	if err != nil {
		return err
	}
	until := expiresIn(ttl)
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

// listSpaces handles GET /v1/spaces: the spaces the visitor belongs to,
// the most recently active first.
func (s *Server) listSpaces(c *reqCtx) error {
	visitor, err := c.needVisitor()
	if err != nil {
		return err
	}
	q := c.r.URL.Query()
	limit := 100
	if q.Has("limit") {
		n, ok := parseUint(q.Get("limit"))
		if !ok || n < 1 || n > 1000 {
			return apierr.BadParam("limit", "limit is 1 to 1000.")
		}
		limit = int(n)
	}
	var after *store.SpacesCursor
	if q.Has("after") {
		parts := strings.Split(q.Get("after"), ":")
		bad := apierr.BadParam("after", "after is the next value of the previous page.")
		if len(parts) != 3 || !names.ValidSpace(parts[2]) {
			return bad
		}
		active, ok1 := parseUint(parts[0])
		since, ok2 := parseUint(parts[1])
		if !ok1 || !ok2 {
			return bad
		}
		after = &store.SpacesCursor{Active: active, Since: since, Space: parts[2]}
	}
	if err := s.rate(s.lim.readsPerIP, c.ip, 1, "rate_limited"); err != nil {
		return err
	}
	items, next, err := s.store.ListSpaces(visitor, after, limit)
	if err != nil {
		return err
	}
	var nx any
	if next != nil {
		nx = strconv.FormatInt(next.Active, 10) + ":" + strconv.FormatInt(next.Since, 10) + ":" + next.Space
	}
	c.json(200, map[string]any{"items": items, "next": nx})
	return nil
}
