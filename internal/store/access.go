package store

import (
	"database/sql"
	"errors"
	"sort"
	"time"

	"github.com/timjaanson/cubby/internal/access"
	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/names"
)

// Limits on access rows per space.
const (
	MaxInvites = 1000
	MaxGrants  = 10_000
	// Tokens that can't be redeemed stay listed this long after they ended.
	deadInviteKeep = 30 * 24 * time.Hour
)

// accessCache is a space's settings, grants and bans, loaded once and
// dropped on any change to them, so the check per request needs no query.
type accessCache struct {
	settings access.Settings
	grants   map[string][]access.Grant // visitor -> grants, ended ones included
	used     map[string]int64          // visitor -> last_used_at written, seconds
	bans     map[string]int64          // visitor -> until in ms, 0 for no end; lapsed ones included
}

func (s *Store) loadAccess(sp *spaceDB) (*accessCache, error) {
	sp.accMu.Lock()
	defer sp.accMu.Unlock()
	if sp.acc != nil {
		return sp.acc, nil
	}
	m, err := readMeta(sp.db)
	if err != nil {
		return nil, err
	}
	c := &accessCache{settings: m.settings, grants: map[string][]access.Grant{}, used: map[string]int64{}, bans: map[string]int64{}}
	brows, err := sp.db.Query(`SELECT visitor, coalesce(until, 0) FROM bans`)
	if err != nil {
		return nil, err
	}
	for brows.Next() {
		var v string
		var until int64
		if err := brows.Scan(&v, &until); err != nil {
			brows.Close()
			return nil, err
		}
		c.bans[v] = until
	}
	brows.Close()
	rows, err := sp.db.Query(`SELECT id, visitor, access, source, coalesce(until, 0), coalesce(last_used_at, 0) FROM grants`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var g access.Grant
		var visitor, level string
		var lastUsed int64
		if err := rows.Scan(&g.ID, &visitor, &level, &g.Source, &g.Until, &lastUsed); err != nil {
			return nil, err
		}
		g.Level, _ = access.ParseLevel(level)
		c.grants[visitor] = append(c.grants[visitor], g)
		c.used[visitor] = max(c.used[visitor], lastUsed/1000)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sp.acc = c
	return c, nil
}

func (sp *spaceDB) dropAccess() {
	sp.accMu.Lock()
	sp.acc = nil
	sp.accMu.Unlock()
}

// Rights is what a visitor may do in a space. It returns nil if the space
// doesn't exist. visitor may be "".
func (s *Store) Rights(space, visitor string) (*access.Rights, error) {
	sp, err := s.Acquire(space)
	if err != nil || sp == nil {
		return nil, err
	}
	defer s.Release(sp)
	c, err := s.loadAccess(sp)
	if err != nil {
		return nil, err
	}
	r := &access.Rights{Settings: c.settings}
	if visitor == "" {
		return r, nil
	}
	now := time.Now()
	for _, g := range c.grants[visitor] {
		if g.Live(now) {
			r.Member = max(r.Member, g.Level)
		}
	}
	if until, ok := c.bans[visitor]; ok && r.Member != access.Owner && (until == 0 || until > now.UnixMilli()) {
		r.Banned, r.BannedUntil = true, until
		return r, nil
	}
	if r.Member > access.None {
		s.markUsed(sp, c, visitor, now)
	}
	return r, nil
}

// markUsed records that a visitor used their grants, at most once an hour.
func (s *Store) markUsed(sp *spaceDB, c *accessCache, visitor string, now time.Time) {
	sec := now.Unix()
	sp.accMu.Lock()
	stale := sec-c.used[visitor] >= 3600
	if stale {
		c.used[visitor] = sec
	}
	sp.accMu.Unlock()
	if stale {
		sp.wmu.Lock()
		sp.db.Exec(`UPDATE grants SET last_used_at = ? WHERE visitor = ?`, now.UnixMilli(), visitor)
		sp.wmu.Unlock()
	}
}

// withSpace runs fn on an existing space with its write lock held, in a
// transaction, and drops the access cache afterwards.
func (s *Store) withSpace(space string, fn func(tx *sql.Tx) error) error {
	sp, err := s.Acquire(space)
	if err != nil {
		return err
	}
	if sp == nil {
		return apierr.NoSpace
	}
	defer s.Release(sp)
	sp.wmu.Lock()
	defer sp.wmu.Unlock()
	tx, err := sp.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	sp.dropAccess()
	return nil
}

// InviteSpec is a new invite. nil pointers mean no limit.
type InviteSpec struct {
	By       string // the minting visitor
	Label    string
	Access   access.Level
	Uses     *int64
	Until    *int64 // ms
	GrantFor *int64 // ms
}

type Invite struct {
	ID             string       `json:"id"`
	Label          string       `json:"label"`
	Access         access.Level `json:"access"`
	UsesLeft       *int64       `json:"usesLeft"`
	Until          *int64       `json:"until"`
	GrantFor       *int64       `json:"grantFor"`
	State          string       `json:"state"`
	Members        int          `json:"members"`
	CreatedAt      int64        `json:"createdAt"`
	LastRedeemedAt *int64       `json:"lastRedeemedAt"`
	By             *string      `json:"by"`
}

// Member is one visitor's place in a space: their level from grants, or a
// ban. Until, Label, InvitedBy and LastUsedAt are for owners. Label and
// InvitedBy come from the invite that gave the level.
type Member struct {
	ID         string       `json:"id"`
	Access     access.Level `json:"access"`
	Since      int64        `json:"since"`
	Until      *int64       `json:"until"`
	Label      *string      `json:"label"`
	InvitedBy  *string      `json:"invitedBy"`
	LastUsedAt *int64       `json:"lastUsedAt"`
}

func nullable(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func inviteState(usesLeft, until, revoked sql.NullInt64, now int64) string {
	switch {
	case revoked.Valid:
		return "revoked"
	case usesLeft.Valid && usesLeft.Int64 <= 0:
		return "used_up"
	case until.Valid && until.Int64 <= now:
		return "expired"
	}
	return "active"
}

const inviteCols = `t.id, t.label, t.access, t.uses_left, t.until, t.grant_for, t.created_at, t.last_redeemed_at,
	(SELECT count(*) FROM grants g WHERE g.source = t.id), t.minted_by, t.revoked_at`

func scanInvite(sc interface{ Scan(...any) error }, now int64) (Invite, error) {
	var t Invite
	var level string
	var uses, until, grantFor, last, revoked sql.NullInt64
	var by sql.NullString
	if err := sc.Scan(&t.ID, &t.Label, &level, &uses, &until, &grantFor, &t.CreatedAt, &last, &t.Members, &by, &revoked); err != nil {
		return t, err
	}
	t.Access, _ = access.ParseLevel(level)
	t.UsesLeft, t.Until, t.GrantFor, t.LastRedeemedAt = nullable(uses), nullable(until), nullable(grantFor), nullable(last)
	if by.Valid {
		t.By = &by.String
	}
	t.State = inviteState(uses, until, revoked, now)
	return t, nil
}

func (s *Store) CreateInvite(space string, spec InviteSpec) (info Invite, token string, err error) {
	token = names.RandomToken("inv_")
	now := NowMs()
	err = s.withSpace(space, func(tx *sql.Tx) error {
		var n int
		tx.QueryRow(`SELECT count(*) FROM invites`).Scan(&n)
		if n >= MaxInvites {
			return apierr.Quota("space_quota_exceeded", "This space has as many invites as it may hold. Revoke old ones first.", MaxInvites, int64(n))
		}
		id := names.NewInviteID()
		if _, err := tx.Exec(`INSERT INTO invites (id, hash, label, access, uses_left, until, grant_for, created_at, minted_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, names.HashSecret(token), spec.Label, spec.Access.String(), spec.Uses, spec.Until, spec.GrantFor, now, spec.By); err != nil {
			return err
		}
		info, err = scanInvite(tx.QueryRow(`SELECT `+inviteCols+` FROM invites t WHERE id = ?`, id), now)
		return err
	})
	return info, token, err
}

// ListInvites lists a space's invites; with by, only those that visitor minted.
func (s *Store) ListInvites(space, by string) ([]Invite, error) {
	sp, err := s.Acquire(space)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, apierr.NoSpace
	}
	defer s.Release(sp)
	rows, err := sp.db.Query(`SELECT `+inviteCols+` FROM invites t WHERE ? = '' OR t.minted_by = ? ORDER BY created_at`, by, by)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := NowMs()
	out := []Invite{}
	for rows.Next() {
		t, err := scanInvite(rows, now)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// InviteChange changes how long and how often an invite can still be redeemed.
type InviteChange struct {
	SetUses  bool
	Uses     *int64 // nil: no limit
	SetUntil bool
	Until    *int64 // nil: no end
	// Check, if set, sees the invite's level before anything changes and
	// can refuse the change.
	Check func(access.Level) error
}

var inviteNotFound = &apierr.Error{Status: 404, Code: "not_found", Message: "No such invite in this space."}

// UpdateInvite changes an invite; with by, only one that visitor minted.
func (s *Store) UpdateInvite(space, id, by string, ch InviteChange) (info Invite, err error) {
	now := NowMs()
	err = s.withSpace(space, func(tx *sql.Tx) error {
		var exists int
		if tx.QueryRow(`SELECT 1 FROM invites WHERE id = ? AND (? = '' OR minted_by = ?)`, id, by, by).Scan(&exists) != nil {
			return inviteNotFound
		}
		if ch.Check != nil {
			var lv string
			if err := tx.QueryRow(`SELECT access FROM invites WHERE id = ?`, id).Scan(&lv); err != nil {
				return err
			}
			level, _ := access.ParseLevel(lv)
			if err := ch.Check(level); err != nil {
				return err
			}
		}
		if ch.SetUses {
			var ended any
			if ch.Uses != nil && *ch.Uses <= 0 {
				ended = now
			}
			if _, err := tx.Exec(`UPDATE invites SET uses_left = ?, ended_at = ? WHERE id = ?`, ch.Uses, ended, id); err != nil {
				return err
			}
		}
		if ch.SetUntil {
			if _, err := tx.Exec(`UPDATE invites SET until = ? WHERE id = ?`, ch.Until, id); err != nil {
				return err
			}
		}
		info, err = scanInvite(tx.QueryRow(`SELECT `+inviteCols+` FROM invites t WHERE id = ?`, id), now)
		return err
	})
	return info, err
}

// DeleteInvite revokes an invite and every grant redeemed from it. With by,
// only an invite that visitor minted.
func (s *Store) DeleteInvite(space, id, by string) error {
	return s.withSpace(space, func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM invites WHERE id = ? AND (? = '' OR minted_by = ?)`, id, by, by)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return inviteNotFound
		}
		_, err = tx.Exec(`DELETE FROM grants WHERE source = ?`, id)
		return err
	})
}

func (s *Store) VisitorExists(id string) bool {
	var one int
	return s.srv.QueryRow(`SELECT 1 FROM visitors WHERE id = ?`, id).Scan(&one) == nil
}

func countGrants(tx *sql.Tx) error {
	var n int64
	tx.QueryRow(`SELECT count(*) FROM grants`).Scan(&n)
	if n >= MaxGrants {
		return apierr.Quota("space_quota_exceeded", "This space has as many members as it may hold.", MaxGrants, n)
	}
	return nil
}

// Joined is what redeeming an invite gave: the visitor's level in the space
// now, and when the grant from this invite ends.
type Joined struct {
	Access access.Level
	Until  *int64
}

// Redeem turns an invite into a grant for visitor. Every
// failure is InviteInvalid, so the answer never tells which part was wrong.
// Redeeming again uses nothing.
func (s *Store) Redeem(space, visitor, token string) (j Joined, err error) {
	now := NowMs()
	added, fresh := false, false
	hash := names.HashSecret(token)
	err = s.withSpace(space, func(tx *sql.Tx) error {
		source, level := "", access.None
		var gUntil *int64
		{
			var lv string
			var uses, until, grantFor, revoked sql.NullInt64
			var by sql.NullString
			err := tx.QueryRow(`SELECT id, access, uses_left, until, grant_for, revoked_at, minted_by FROM invites WHERE hash = ?`, hash).
				Scan(&source, &lv, &uses, &until, &grantFor, &revoked, &by)
			if errors.Is(err, sql.ErrNoRows) {
				return apierr.InviteInvalid
			}
			if err != nil {
				return err
			}
			level, _ = access.ParseLevel(lv)
			var have int
			if tx.QueryRow(`SELECT 1 FROM grants WHERE visitor = ? AND source = ?`, visitor, source).Scan(&have) == nil {
				source = "" // already redeemed: nothing to add
			} else {
				if inviteState(uses, until, revoked, now) != "active" {
					return apierr.InviteInvalid
				}
				// An invite works only while its minter could still mint it.
				r, err := rightsTx(tx, by.String, now)
				if err != nil {
					return err
				}
				if !r.CanInvite() || !r.CanGive(level) {
					return apierr.InviteInvalid
				}
				if grantFor.Valid {
					u := now + grantFor.Int64
					gUntil = &u
				}
				if uses.Valid {
					left := uses.Int64 - 1
					var ended any
					if left <= 0 {
						ended = now
					}
					if _, err := tx.Exec(`UPDATE invites SET uses_left = ?, ended_at = ? WHERE id = ?`, left, ended, source); err != nil {
						return err
					}
				}
				if _, err := tx.Exec(`UPDATE invites SET last_redeemed_at = ? WHERE id = ?`, now, source); err != nil {
					return err
				}
			}
		}
		if source != "" {
			var have int
			if tx.QueryRow(`SELECT 1 FROM grants WHERE visitor = ? AND source = ?`, visitor, source).Scan(&have) != nil {
				if err := countGrants(tx); err != nil {
					return err
				}
				fresh = !hasGrant(tx, visitor, now)
				if _, err := tx.Exec(`INSERT INTO grants (id, visitor, access, source, until, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
					names.NewGrantID(), visitor, level.String(), source, gUntil, now); err != nil {
					return err
				}
				added = true
			}
		}
		r, err := rightsTx(tx, visitor, now)
		if err != nil {
			return err
		}
		j = Joined{Access: r.Member, Until: gUntil}
		return nil
	})
	if err == nil && added {
		s.noteMember(visitor, space, now, fresh)
	} else if err == nil {
		s.noteVisitorSpace(visitor, space)
	}
	return j, err
}

// Members lists a space's members, highest level first, then by when they
// joined. With bans, banned visitors are included as level banned.
func (s *Store) Members(space string, bans bool) ([]Member, error) {
	sp, err := s.Acquire(space)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, apierr.NoSpace
	}
	defer s.Release(sp)
	now := NowMs()
	rows, err := sp.db.Query(`SELECT g.visitor, g.access, g.until, g.created_at, g.last_used_at, t.minted_by, t.label
		FROM grants g LEFT JOIN invites t ON t.id = g.source
		WHERE g.until IS NULL OR g.until > ? ORDER BY g.created_at`, now)
	if err != nil {
		return nil, err
	}
	byID := map[string]*Member{}
	var order []string
	for rows.Next() {
		var id, level string
		var until, last sql.NullInt64
		var created int64
		var inviter, label sql.NullString
		if err := rows.Scan(&id, &level, &until, &created, &last, &inviter, &label); err != nil {
			rows.Close()
			return nil, err
		}
		l, _ := access.ParseLevel(level)
		from := func(m *Member) {
			m.InvitedBy, m.Label = nil, nil
			if inviter.Valid {
				m.InvitedBy = &inviter.String
			}
			if label.Valid && label.String != "" {
				m.Label = &label.String
			}
		}
		m := byID[id]
		if m == nil {
			m = &Member{ID: id, Access: l, Since: created, Until: nullable(until)}
			from(m)
			byID[id] = m
			order = append(order, id)
		} else {
			if l > m.Access {
				m.Access = l
				from(m)
			}
			// The membership lasts as long as its longest grant.
			if m.Until != nil && (!until.Valid || until.Int64 > *m.Until) {
				m.Until = nullable(until)
			}
		}
		if last.Valid && (m.LastUsedAt == nil || last.Int64 > *m.LastUsedAt) {
			m.LastUsedAt = nullable(last)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []Member{}
	if bans {
		brows, err := sp.db.Query(`SELECT visitor, until, created_at FROM bans WHERE until IS NULL OR until > ? ORDER BY created_at`, now)
		if err != nil {
			return nil, err
		}
		for brows.Next() {
			var m Member
			var until sql.NullInt64
			if err := brows.Scan(&m.ID, &until, &m.Since); err != nil {
				brows.Close()
				return nil, err
			}
			m.Access, m.Until = access.Banned, nullable(until)
			out = append(out, m)
		}
		brows.Close()
	}
	for _, id := range order {
		out = append(out, *byID[id])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Access > out[j].Access })
	return out, nil
}

var errNoVisitor = &apierr.Error{Status: 404, Code: "no_visitor", Message: "No visitor on this server has that ID."}

// SetMember gives a visitor exactly one level in a space, replacing every
// grant they had. Banned removes their grants and keeps them out until the
// ban ends or an owner sets another level.
func (s *Store) SetMember(space, user, by string, level access.Level, until *int64) (m Member, err error) {
	if !s.VisitorExists(user) {
		return m, errNoVisitor
	}
	now := NowMs()
	fresh := false
	err = s.withSpace(space, func(tx *sql.Tx) error {
		fresh = !hasGrant(tx, user, now)
		if _, err := tx.Exec(`DELETE FROM grants WHERE visitor = ?`, user); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM bans WHERE visitor = ?`, user); err != nil {
			return err
		}
		if level == access.Banned {
			if _, err := tx.Exec(`INSERT INTO bans (visitor, until, by, created_at) VALUES (?, ?, ?, ?)`, user, until, by, now); err != nil {
				return err
			}
		} else {
			if err := countGrants(tx); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO grants (id, visitor, access, source, until, created_at) VALUES (?, ?, ?, 'direct', ?, ?)`,
				names.NewGrantID(), user, level.String(), until, now); err != nil {
				return err
			}
		}
		m = Member{ID: user, Access: level, Since: now, Until: until}
		return revokeUngivable(tx)
	})
	if err == nil && level != access.Banned {
		s.noteMember(user, space, now, fresh)
	}
	return m, err
}

// RemoveMember removes a visitor's grants and any ban, so they are a
// stranger again. It reports whether there was anything to remove.
func (s *Store) RemoveMember(space, user string) (removed bool, err error) {
	err = s.withSpace(space, func(tx *sql.Tx) error {
		for _, table := range []string{"grants", "bans"} {
			res, err := tx.Exec(`DELETE FROM `+table+` WHERE visitor = ?`, user)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				removed = true
			}
		}
		return revokeUngivable(tx)
	})
	return removed, err
}

// rightsTx is a visitor's live rights, read inside a transaction.
func rightsTx(tx *sql.Tx, visitor string, now int64) (*access.Rights, error) {
	m, err := readMeta(tx)
	if err != nil {
		return nil, err
	}
	r := &access.Rights{Settings: m.settings}
	rows, err := tx.Query(`SELECT access FROM grants WHERE visitor = ? AND (until IS NULL OR until > ?)`, visitor, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var level string
		if err := rows.Scan(&level); err != nil {
			return nil, err
		}
		l, _ := access.ParseLevel(level)
		r.Member = max(r.Member, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if r.Member != access.Owner {
		var until int64
		err := tx.QueryRow(`SELECT coalesce(until, 0) FROM bans WHERE visitor = ? AND (until IS NULL OR until > ?)`, visitor, now).Scan(&until)
		if err == nil {
			r.Banned, r.BannedUntil = true, until
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	return r, nil
}

// revokeUngivable ends the open invites whose minter could no longer mint
// them: their level dropped, or the space raised its invite level. Grants
// already redeemed from them stay.
func revokeUngivable(tx *sql.Tx) error {
	now := NowMs()
	rows, err := tx.Query(`SELECT id, minted_by, access FROM invites WHERE revoked_at IS NULL
		AND (uses_left IS NULL OR uses_left > 0) AND (until IS NULL OR until > ?)`, now)
	if err != nil {
		return err
	}
	type tok struct{ id, by, level string }
	var open []tok
	for rows.Next() {
		var t tok
		if err := rows.Scan(&t.id, &t.by, &t.level); err != nil {
			rows.Close()
			return err
		}
		open = append(open, t)
	}
	rows.Close()
	for _, t := range open {
		r, err := rightsTx(tx, t.by, now)
		if err != nil {
			return err
		}
		l, _ := access.ParseLevel(t.level)
		if r.CanInvite() && r.CanGive(l) {
			continue
		}
		if _, err := tx.Exec(`UPDATE invites SET revoked_at = ? WHERE id = ?`, now, t.id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SetSettings(space string, st access.Settings) error {
	err := s.withSpace(space, func(tx *sql.Tx) error {
		if err := setMeta(tx, "settings", st.Encode()); err != nil {
			return err
		}
		return revokeUngivable(tx)
	})
	if err == nil {
		s.mu.Lock()
		if x := s.stats[space]; x != nil {
			x.settings = st
		}
		s.mu.Unlock()
	}
	return err
}

func (s *Store) Settings(space string) (access.Settings, error) {
	r, err := s.Rights(space, "")
	if err != nil {
		return access.Default, err
	}
	if r == nil {
		return access.Default, apierr.NoSpace
	}
	return r.Settings, nil
}

// pruneAccess removes grants past their end, and invites that can't be
// redeemed, have no grants left, and ended over 30 days ago.
func (s *Store) pruneAccess(space string) error {
	now := NowMs()
	cut := now - deadInviteKeep.Milliseconds()
	err := s.withSpace(space, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM grants WHERE until IS NOT NULL AND until <= ?`, now); err != nil {
			return err
		}
		if err := revokeUngivable(tx); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM invites WHERE NOT EXISTS (SELECT 1 FROM grants g WHERE g.source = invites.id)
			AND ((uses_left IS NOT NULL AND uses_left <= 0 AND coalesce(ended_at, 0) < ?) OR (until IS NOT NULL AND until < ?)
			OR (revoked_at IS NOT NULL AND revoked_at < ?))`, cut, cut, cut)
		return err
	})
	if err == apierr.NoSpace {
		return nil // deleted meanwhile
	}
	return err
}
