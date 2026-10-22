package store

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/timjaanson/cubby/internal/access"
	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/names"
)

// Limits on access rows per space.
const (
	MaxTokens = 1000
	MaxGrants = 10_000
	// Tokens that can't be redeemed stay listed this long after they ended.
	deadTokenKeep = 30 * 24 * time.Hour
)

// accessCache is a space's settings and grants, loaded once and dropped on
// any change to them, so the check per request needs no query.
type accessCache struct {
	settings access.Settings
	grants   map[string][]access.Grant // visitor -> grants, ended ones included
	used     map[string]int64          // visitor -> last_used_at written, seconds
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
	c := &accessCache{settings: m.settings, grants: map[string][]access.Grant{}, used: map[string]int64{}}
	rows, err := sp.db.Query(`SELECT id, visitor, owner, allow, source, until, coalesce(last_used_at, 0) FROM grants`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var g access.Grant
		var visitor, allow string
		var until sql.NullInt64
		var lastUsed int64
		if err := rows.Scan(&g.ID, &visitor, &g.Owner, &allow, &g.Source, &until, &lastUsed); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(allow), &g.Allow)
		g.Scopes = access.ParseScopes(g.Allow)
		g.Until = until.Int64
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
		if !g.Live(now) {
			continue
		}
		if g.Owner {
			r.Owner = true
		}
		r.Grants = append(r.Grants, g)
	}
	if len(r.Grants) > 0 {
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

// TokenSpec is a new access token. nil pointers mean no limit.
type TokenSpec struct {
	Label    string
	Allow    []string
	Uses     *int64
	Until    *int64 // ms
	GrantFor *int64 // ms
}

type TokenInfo struct {
	ID             string   `json:"id"`
	Label          string   `json:"label"`
	Allow          []string `json:"allow"`
	UsesLeft       *int64   `json:"usesLeft"`
	Until          *int64   `json:"until"`
	GrantFor       *int64   `json:"grantFor"`
	State          string   `json:"state"`
	Grants         int      `json:"grants"`
	CreatedAt      int64    `json:"createdAt"`
	LastRedeemedAt *int64   `json:"lastRedeemedAt"`
}

type GrantInfo struct {
	ID         string   `json:"id"`
	User       string   `json:"user"`
	Owner      bool     `json:"owner"`
	Allow      []string `json:"allow"`
	Source     string   `json:"source"`
	Until      *int64   `json:"until"`
	CreatedAt  int64    `json:"createdAt"`
	LastUsedAt *int64   `json:"lastUsedAt"`
}

func nullable(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func tokenState(usesLeft, until sql.NullInt64, now int64) string {
	switch {
	case usesLeft.Valid && usesLeft.Int64 <= 0:
		return "used_up"
	case until.Valid && until.Int64 <= now:
		return "expired"
	}
	return "active"
}

const tokenCols = `t.id, t.label, t.allow, t.uses_left, t.until, t.grant_for, t.created_at, t.last_redeemed_at,
	(SELECT count(*) FROM grants g WHERE g.source = t.id)`

func scanToken(sc interface{ Scan(...any) error }, now int64) (TokenInfo, error) {
	var t TokenInfo
	var allow string
	var uses, until, grantFor, last sql.NullInt64
	if err := sc.Scan(&t.ID, &t.Label, &allow, &uses, &until, &grantFor, &t.CreatedAt, &last, &t.Grants); err != nil {
		return t, err
	}
	json.Unmarshal([]byte(allow), &t.Allow)
	t.UsesLeft, t.Until, t.GrantFor, t.LastRedeemedAt = nullable(uses), nullable(until), nullable(grantFor), nullable(last)
	t.State = tokenState(uses, until, now)
	return t, nil
}

func (s *Store) CreateToken(space string, spec TokenSpec) (info TokenInfo, token string, err error) {
	token = names.RandomToken("at_")
	now := NowMs()
	err = s.withSpace(space, func(tx *sql.Tx) error {
		var n int
		tx.QueryRow(`SELECT count(*) FROM tokens`).Scan(&n)
		if n >= MaxTokens {
			return apierr.Quota("space_quota_exceeded", "This space has as many access tokens as it may hold. Revoke old ones first.", MaxTokens, int64(n))
		}
		allow, _ := json.Marshal(spec.Allow)
		id := names.NewTokenID()
		if _, err := tx.Exec(`INSERT INTO tokens (id, hash, label, allow, uses_left, until, grant_for, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, names.HashSecret(token), spec.Label, string(allow), spec.Uses, spec.Until, spec.GrantFor, now); err != nil {
			return err
		}
		info, err = scanToken(tx.QueryRow(`SELECT `+tokenCols+` FROM tokens t WHERE id = ?`, id), now)
		return err
	})
	return info, token, err
}

func (s *Store) ListTokens(space string) ([]TokenInfo, error) {
	sp, err := s.Acquire(space)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, apierr.NoSpace
	}
	defer s.Release(sp)
	rows, err := sp.db.Query(`SELECT ` + tokenCols + ` FROM tokens t ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := NowMs()
	out := []TokenInfo{}
	for rows.Next() {
		t, err := scanToken(rows, now)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TokenChange changes how long and how often a token can still be redeemed.
type TokenChange struct {
	SetUses  bool
	Uses     *int64 // nil: no limit
	SetUntil bool
	Until    *int64 // nil: no end
}

var tokenNotFound = &apierr.Error{Status: 404, Code: "not_found", Message: "No such access token in this space."}

func (s *Store) UpdateToken(space, id string, ch TokenChange) (info TokenInfo, err error) {
	now := NowMs()
	err = s.withSpace(space, func(tx *sql.Tx) error {
		var exists int
		if tx.QueryRow(`SELECT 1 FROM tokens WHERE id = ?`, id).Scan(&exists) != nil {
			return tokenNotFound
		}
		if ch.SetUses {
			var ended any
			if ch.Uses != nil && *ch.Uses <= 0 {
				ended = now
			}
			if _, err := tx.Exec(`UPDATE tokens SET uses_left = ?, ended_at = ? WHERE id = ?`, ch.Uses, ended, id); err != nil {
				return err
			}
		}
		if ch.SetUntil {
			if _, err := tx.Exec(`UPDATE tokens SET until = ? WHERE id = ?`, ch.Until, id); err != nil {
				return err
			}
		}
		info, err = scanToken(tx.QueryRow(`SELECT `+tokenCols+` FROM tokens t WHERE id = ?`, id), now)
		return err
	})
	return info, err
}

// DeleteToken revokes a token and every grant redeemed from it.
func (s *Store) DeleteToken(space, id string) error {
	return s.withSpace(space, func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM tokens WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return tokenNotFound
		}
		_, err = tx.Exec(`DELETE FROM grants WHERE source = ?`, id)
		return err
	})
}

const grantCols = `id, visitor, owner, allow, source, until, created_at, last_used_at`

func scanGrant(sc interface{ Scan(...any) error }) (GrantInfo, error) {
	var g GrantInfo
	var allow string
	var until, last sql.NullInt64
	if err := sc.Scan(&g.ID, &g.User, &g.Owner, &allow, &g.Source, &until, &g.CreatedAt, &last); err != nil {
		return g, err
	}
	json.Unmarshal([]byte(allow), &g.Allow)
	if g.Allow == nil {
		g.Allow = []string{}
	}
	g.Until, g.LastUsedAt = nullable(until), nullable(last)
	return g, nil
}

// Redeem turns an access or owner token into a grant for visitor. Every
// failure is InviteInvalid, so the answer never tells which part was wrong.
// Redeeming again returns the existing grant and uses nothing.
func (s *Store) Redeem(space, visitor, token string) (g GrantInfo, err error) {
	now := NowMs()
	hash := names.HashSecret(token)
	err = s.withSpace(space, func(tx *sql.Tx) error {
		existing := func(source string) bool {
			got, err := scanGrant(tx.QueryRow(`SELECT `+grantCols+` FROM grants WHERE visitor = ? AND source = ?`, visitor, source))
			if err != nil {
				return false
			}
			g = got
			return true
		}
		count := func() error {
			var n int64
			tx.QueryRow(`SELECT count(*) FROM grants`).Scan(&n)
			if n >= MaxGrants {
				return apierr.Quota("space_quota_exceeded", "This space has as many grants as it may hold.", MaxGrants, n)
			}
			return nil
		}

		if names.OwnerTokenRe.MatchString(token) {
			m, err := readMeta(tx)
			if err != nil {
				return err
			}
			if m.ownerHash == "" || subtle.ConstantTimeCompare([]byte(hash), []byte(m.ownerHash)) != 1 {
				return apierr.InviteInvalid
			}
			if existing("owner-token") {
				return nil
			}
			if err := count(); err != nil {
				return err
			}
			id := names.NewGrantID()
			if _, err := tx.Exec(`INSERT INTO grants (id, visitor, owner, allow, source, created_at) VALUES (?, ?, 1, '[]', 'owner-token', ?)`, id, visitor, now); err != nil {
				return err
			}
			g, err = scanGrant(tx.QueryRow(`SELECT `+grantCols+` FROM grants WHERE id = ?`, id))
			return err
		}

		var tid, allow string
		var uses, until, grantFor sql.NullInt64
		err := tx.QueryRow(`SELECT id, allow, uses_left, until, grant_for FROM tokens WHERE hash = ?`, hash).Scan(&tid, &allow, &uses, &until, &grantFor)
		if errors.Is(err, sql.ErrNoRows) {
			return apierr.InviteInvalid
		}
		if err != nil {
			return err
		}
		if existing(tid) {
			return nil
		}
		if tokenState(uses, until, now) != "active" {
			return apierr.InviteInvalid
		}
		if err := count(); err != nil {
			return err
		}
		var gUntil any
		if grantFor.Valid {
			gUntil = now + grantFor.Int64
		}
		id := names.NewGrantID()
		if _, err := tx.Exec(`INSERT INTO grants (id, visitor, owner, allow, source, until, created_at) VALUES (?, ?, 0, ?, ?, ?, ?)`,
			id, visitor, allow, tid, gUntil, now); err != nil {
			return err
		}
		if uses.Valid {
			left := uses.Int64 - 1
			var ended any
			if left <= 0 {
				ended = now
			}
			if _, err := tx.Exec(`UPDATE tokens SET uses_left = ?, ended_at = ? WHERE id = ?`, left, ended, tid); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE tokens SET last_redeemed_at = ? WHERE id = ?`, now, tid); err != nil {
			return err
		}
		g, err = scanGrant(tx.QueryRow(`SELECT `+grantCols+` FROM grants WHERE id = ?`, id))
		return err
	})
	if err == nil {
		s.noteVisitorSpace(visitor, space)
	}
	return g, err
}

func (s *Store) ListGrants(space, user string) ([]GrantInfo, error) {
	sp, err := s.Acquire(space)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, apierr.NoSpace
	}
	defer s.Release(sp)
	q, args := `SELECT `+grantCols+` FROM grants ORDER BY created_at`, []any{}
	if user != "" {
		q, args = `SELECT `+grantCols+` FROM grants WHERE visitor = ? ORDER BY created_at`, []any{user}
	}
	rows, err := sp.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GrantInfo{}
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GrantsOf is a visitor's grants in a space, ended ones left out.
func (s *Store) GrantsOf(space, visitor string) ([]GrantInfo, error) {
	all, err := s.ListGrants(space, visitor)
	if err != nil {
		return nil, err
	}
	now := NowMs()
	out := all[:0]
	for _, g := range all {
		if g.Until == nil || *g.Until > now {
			out = append(out, g)
		}
	}
	return out, nil
}

func (s *Store) VisitorExists(id string) bool {
	var one int
	return s.srv.QueryRow(`SELECT 1 FROM visitors WHERE id = ?`, id).Scan(&one) == nil
}

// AddGrant gives a visitor rights directly, with no token.
func (s *Store) AddGrant(space, user string, owner bool, allow []string, until *int64) (g GrantInfo, err error) {
	if !s.VisitorExists(user) {
		return g, &apierr.Error{Status: 404, Code: "not_found", Message: "No such visitor on this server."}
	}
	now := NowMs()
	err = s.withSpace(space, func(tx *sql.Tx) error {
		var n int64
		tx.QueryRow(`SELECT count(*) FROM grants`).Scan(&n)
		if n >= MaxGrants {
			return apierr.Quota("space_quota_exceeded", "This space has as many grants as it may hold.", MaxGrants, n)
		}
		if owner {
			allow = []string{}
		}
		a, _ := json.Marshal(allow)
		id := names.NewGrantID()
		if _, err := tx.Exec(`INSERT INTO grants (id, visitor, owner, allow, source, until, created_at) VALUES (?, ?, ?, ?, 'direct', ?, ?)`,
			id, user, owner, string(a), until, now); err != nil {
			return err
		}
		g, err = scanGrant(tx.QueryRow(`SELECT `+grantCols+` FROM grants WHERE id = ?`, id))
		return err
	})
	if err == nil {
		s.noteVisitorSpace(user, space)
	}
	return g, err
}

func (s *Store) DeleteGrant(space, id string) error {
	return s.withSpace(space, func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM grants WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return &apierr.Error{Status: 404, Code: "not_found", Message: "No such grant in this space."}
		}
		return nil
	})
}

// DeleteUserGrants removes everything a visitor holds in a space.
func (s *Store) DeleteUserGrants(space, user string) (int64, error) {
	var n int64
	err := s.withSpace(space, func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM grants WHERE visitor = ?`, user)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}

func (s *Store) SetSettings(space string, st access.Settings) error {
	err := s.withSpace(space, func(tx *sql.Tx) error {
		return setMeta(tx, "settings", st.Encode())
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
		return access.Private, err
	}
	if r == nil {
		return access.Private, apierr.NoSpace
	}
	return r.Settings, nil
}

// RotateOwner replaces the owner token and removes owner grants that came
// from the old one, except keep's. It returns the new token.
func (s *Store) RotateOwner(space, keep string) (string, error) {
	token := names.RandomToken("ot_")
	err := s.withSpace(space, func(tx *sql.Tx) error {
		if err := setMeta(tx, "owner_hash", names.HashSecret(token)); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM grants WHERE source = 'owner-token' AND visitor <> ?`, keep)
		return err
	})
	return token, err
}

// pruneAccess removes grants past their end, and tokens that can't be
// redeemed, have no grants left, and ended over 30 days ago.
func (s *Store) pruneAccess(space string) error {
	now := NowMs()
	cut := now - deadTokenKeep.Milliseconds()
	err := s.withSpace(space, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM grants WHERE until IS NOT NULL AND until <= ?`, now); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM tokens WHERE NOT EXISTS (SELECT 1 FROM grants g WHERE g.source = tokens.id)
			AND ((uses_left IS NOT NULL AND uses_left <= 0 AND coalesce(ended_at, 0) < ?) OR (until IS NOT NULL AND until < ?))`, cut, cut)
		return err
	})
	if err == apierr.NoSpace {
		return nil // deleted meanwhile
	}
	return err
}
