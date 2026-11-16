package store

import (
	"database/sql"
	"errors"

	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/auth"
	"github.com/timjaanson/cubby/internal/names"
)

// Pairing code limits. A code is for one device, used once, alive for
// minutes.
const (
	PairingTTLDefault = 10 * 60 * 1000 // ms
	PairingTTLMax     = 60 * 60 * 1000
	PairingTTLMin     = 30 * 1000
	MaxOpenPairings   = 3
)

type Pairing struct {
	ID      string  `json:"id"`
	State   string  `json:"state"` // open, used, expired, cancelled
	Created int64   `json:"created"`
	Expires int64   `json:"expires"`
	Ended   *int64  `json:"ended"`
	Device  *Device `json:"device"` // the device it added, once used
}

func (s *Store) scanPairing(row *sql.Row, now int64) (*Pairing, error) {
	var l Pairing
	var dev sql.NullString
	err := row.Scan(&l.ID, &l.State, &l.Created, &l.Expires, &l.Ended, &dev)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if l.State == "open" && l.Expires <= now {
		l.State = "expired"
		end := l.Expires
		l.Ended = &end
	}
	if dev.Valid {
		var d Device
		err := s.srv.QueryRow(`SELECT id, label, created_at, last_seen_at, paired_by FROM devices WHERE id = ?`, dev.String).
			Scan(&d.ID, &d.Label, &d.Created, &d.LastSeen, &d.PairedBy)
		if err == nil {
			l.Device = &d
		} else {
			// Signed out since: still say which one it was.
			l.Device = &Device{ID: dev.String}
		}
	}
	return &l, nil
}

const pairingCols = `id, state, created_at, expires, ended_at, device`

// CreatePairing makes a one-use code that adds another device to this
// visitor. It returns the pairing and its code; only the hash is kept.
func (s *Store) CreatePairing(visitor, device string, ttl int64) (*Pairing, string, error) {
	now := NowMs()
	tx, err := s.srv.Begin()
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	var open int
	tx.QueryRow(`SELECT count(*) FROM pairing_codes WHERE visitor = ? AND state = 'open' AND expires > ?`, visitor, now).Scan(&open)
	if open >= MaxOpenPairings {
		return nil, "", &apierr.Error{Status: 409, Code: "too_many_pairing_codes",
			Message: "This visitor already has 3 open pairing codes. Cancel one, or wait for it to expire.", Extra: map[string]any{"limit": MaxOpenPairings}}
	}
	id, code := names.NewPairingID(), names.NewPairingCode()
	if _, err := tx.Exec(`INSERT INTO pairing_codes (id, code_hash, visitor, by_device, created_at, expires) VALUES (?, ?, ?, ?, ?, ?)`,
		id, names.HashSecret(code), visitor, device, now, now+ttl); err != nil {
		return nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}
	return &Pairing{ID: id, State: "open", Created: now, Expires: now + ttl}, code, nil
}

// GetPairing is one of a visitor's pairing codes, nil if there's no such
// one of theirs.
func (s *Store) GetPairing(visitor, id string) (*Pairing, error) {
	return s.scanPairing(s.srv.QueryRow(`SELECT `+pairingCols+` FROM pairing_codes WHERE id = ? AND visitor = ?`, id, visitor), NowMs())
}

// CancelPairing ends an open pairing code. Cancelling one that already
// ended changes nothing and returns it as it is.
func (s *Store) CancelPairing(visitor, id string) (*Pairing, error) {
	now := NowMs()
	if _, err := s.srv.Exec(`UPDATE pairing_codes SET state = 'cancelled', ended_at = ? WHERE id = ? AND visitor = ? AND state = 'open' AND expires > ?`, now, id, visitor, now); err != nil {
		return nil, err
	}
	return s.GetPairing(visitor, id)
}

// Paired is what pairing did.
type Paired struct {
	Visitor   string
	Device    string
	PairingID string
	Previous  string // the visitor this key was before, "" if it was new
	Deleted   bool   // Previous had no other device left and was deleted
}

// A code that doesn't exist is ErrPairingUnknown, so the caller counts it
// against the IP.
var (
	ErrPairingUnknown = &apierr.Error{Status: 404, Code: "bad_pairing_code", Message: "No such pairing code. Check it, or make a new one on the other device."}
	ErrSameVisitor    = &apierr.Error{Status: 409, Code: "same_visitor", Message: "This device is already that visitor. The code is still unused."}
)

func pairingEnded(state string) error {
	switch state {
	case "used":
		return &apierr.Error{Status: 410, Code: "pairing_code_used", Message: "This pairing code was already used. Make a new one on the other device."}
	case "cancelled":
		return &apierr.Error{Status: 410, Code: "pairing_code_cancelled", Message: "This pairing code was cancelled. Make a new one on the other device."}
	}
	return &apierr.Error{Status: 410, Code: "pairing_code_expired", Message: "This pairing code expired. Make a new one on the other device."}
}

// Pair adds key as a device of the code's visitor. A key that already was
// another visitor's device moves over; a previous visitor with no device
// left is deleted with everything it had. Pairing always wins: whoever holds
// the key has decided.
func (s *Store) Pair(code, label string, key *auth.Key) (*Paired, error) {
	now := NowMs()
	tx, err := s.srv.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id, visitor, by, state string
	var expires int64
	err = tx.QueryRow(`SELECT id, visitor, by_device, state, expires FROM pairing_codes WHERE code_hash = ?`, names.HashSecret(code)).
		Scan(&id, &visitor, &by, &state, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPairingUnknown
	}
	if err != nil {
		return nil, err
	}
	if state != "open" || expires <= now {
		return nil, pairingEnded(state)
	}
	var prev string
	tx.QueryRow(`SELECT visitor FROM devices WHERE id = ?`, key.ID).Scan(&prev)
	if prev == visitor {
		return nil, ErrSameVisitor
	}
	var exists int
	if tx.QueryRow(`SELECT 1 FROM visitors WHERE id = ?`, visitor).Scan(&exists) != nil {
		return nil, pairingEnded("expired") // the visitor went away meanwhile
	}
	r := &Paired{Visitor: visitor, Device: key.ID, PairingID: id, Previous: prev}
	left := 0
	if prev != "" {
		// The key leaves its old visitor, and what it minted there.
		if _, err := tx.Exec(`DELETE FROM devices WHERE id = ?`, key.ID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`DELETE FROM api_keys WHERE device = ?`, key.ID); err != nil {
			return nil, err
		}
		tx.QueryRow(`SELECT count(*) FROM devices WHERE visitor = ?`, prev).Scan(&left)
	}
	if err := insertDevice(tx, key, visitor, label, &by); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE pairing_codes SET state = 'used', ended_at = ?, device = ? WHERE id = ?`, now, key.ID, id); err != nil {
		return nil, err
	}
	tx.Exec(`UPDATE visitors SET last_seen_at = ? WHERE id = ?`, nowS(), visitor)
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if prev != "" && left == 0 {
		r.Deleted = true
		if err := s.PurgeVisitor(prev); err != nil {
			return r, err
		}
	}
	return r, nil
}

// DeviceVisitor says which visitor a device belongs to, "" if none.
func (s *Store) DeviceVisitor(id string) string {
	var v string
	s.srv.QueryRow(`SELECT visitor FROM devices WHERE id = ?`, id).Scan(&v)
	return v
}
