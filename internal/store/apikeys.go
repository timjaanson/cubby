package store

import (
	"database/sql"
	"errors"

	"github.com/timjaanson/cubby/internal/access"
	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/names"
)

// APIKey is a static credential for tools that can't sign tokens: webhooks,
// no-code tools. It acts as its visitor in one space, at most at its level.
type APIKey struct {
	ID       string       `json:"id"`
	Label    string       `json:"label"`
	Space    string       `json:"space"`
	Access   access.Level `json:"access"`
	Device   string       `json:"device"` // the device that minted it
	Created  int64        `json:"created"`
	Until    *int64       `json:"until"`
	LastUsed *int64       `json:"lastUsed"`
	Visitor  string       `json:"-"`
}

const MaxAPIKeys = 50 // per visitor

const apiKeyCols = `id, label, space, access, device, created_at, until, last_used_at, visitor`

func scanAPIKey(sc interface{ Scan(...any) error }) (*APIKey, error) {
	var k APIKey
	var level string
	var until, last sql.NullInt64
	if err := sc.Scan(&k.ID, &k.Label, &k.Space, &level, &k.Device, &k.Created, &until, &last, &k.Visitor); err != nil {
		return nil, err
	}
	k.Access, _ = access.ParseLevel(level)
	k.Until, k.LastUsed = nullable(until), nullable(last)
	return &k, nil
}

// CreateAPIKey mints a key for visitor, made by device. It returns the key
// and its secret; only the hash is kept.
func (s *Store) CreateAPIKey(visitor, device, space, label string, level access.Level, until *int64) (*APIKey, string, error) {
	secret := names.RandomToken("cubby_")
	now := NowMs()
	tx, err := s.srv.Begin()
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	var n int64
	tx.QueryRow(`SELECT count(*) FROM api_keys WHERE visitor = ?`, visitor).Scan(&n)
	if n >= MaxAPIKeys {
		return nil, "", apierr.Quota("api_key_limit", "This visitor has as many API keys as it may hold. Delete old ones first.", MaxAPIKeys, n)
	}
	id := names.NewAPIKeyID()
	if _, err := tx.Exec(`INSERT INTO api_keys (id, hash, visitor, device, space, access, label, created_at, until) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, names.HashSecret(secret), visitor, device, space, level.String(), label, now, until); err != nil {
		return nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}
	return &APIKey{ID: id, Label: label, Space: space, Access: level, Device: device, Created: now, Until: until, Visitor: visitor}, secret, nil
}

// APIKeyBySecret finds a live API key, nil for an unknown or expired one.
// It marks the key used, at most once a minute.
func (s *Store) APIKeyBySecret(secret string) (*APIKey, error) {
	k, err := scanAPIKey(s.srv.QueryRow(`SELECT `+apiKeyCols+` FROM api_keys WHERE hash = ?`, names.HashSecret(secret)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now := NowMs()
	if k.Until != nil && *k.Until <= now {
		return nil, nil
	}
	if k.LastUsed == nil || now-*k.LastUsed > 60_000 {
		s.srv.Exec(`UPDATE api_keys SET last_used_at = ? WHERE id = ?`, now, k.ID)
	}
	s.touch(k.Visitor, k.Device)
	return k, nil
}

// APIKeys lists a visitor's API keys, newest first.
func (s *Store) APIKeys(visitor string) ([]*APIKey, error) {
	rows, err := s.srv.Query(`SELECT `+apiKeyCols+` FROM api_keys WHERE visitor = ? ORDER BY created_at DESC, id`, visitor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*APIKey{}
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// DeleteAPIKey revokes one of a visitor's API keys.
func (s *Store) DeleteAPIKey(visitor, id string) error {
	res, err := s.srv.Exec(`DELETE FROM api_keys WHERE id = ? AND visitor = ?`, id, visitor)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &apierr.Error{Status: 404, Code: "not_found", Message: "This visitor has no API key with that ID."}
	}
	return nil
}

// pruneAPIKeys deletes keys a day past their end, and keys of spaces that
// are gone.
func (s *Store) pruneAPIKeys() {
	s.srv.Exec(`DELETE FROM api_keys WHERE until IS NOT NULL AND until < ?`, NowMs()-24*3600*1000)
}

// dropSpaceAPIKeys deletes every API key for a deleted space.
func (s *Store) dropSpaceAPIKeys(space string) {
	s.srv.Exec(`DELETE FROM api_keys WHERE space = ?`, space)
}
