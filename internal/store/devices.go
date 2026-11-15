package store

import (
	"database/sql"
	"errors"

	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/names"
)

// migrateServer brings server.db up to date. Version 1 moved each visitor's
// token into a device of its own; nothing else changes.
func migrateServer(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v >= 1 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'visitors'`).Scan(&n)
	if n == 0 {
		return nil // a new file: the schema makes everything
	}
	cols, err := tableCols(tx, "visitors")
	if err != nil {
		return err
	}
	if !cols["token_hash"] {
		return nil
	}
	if _, err := tx.Exec(serverSchema); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT id, token_hash, created_at, last_seen_at FROM visitors`)
	if err != nil {
		return err
	}
	type old struct {
		id, hash      string
		created, seen int64
	}
	var all []old
	for rows.Next() {
		var o old
		if err := rows.Scan(&o.id, &o.hash, &o.created, &o.seen); err != nil {
			rows.Close()
			return err
		}
		all = append(all, o)
	}
	rows.Close()
	for _, o := range all {
		if _, err := tx.Exec(`INSERT INTO devices (id, visitor, token_hash, created_at, last_seen_at) VALUES (?, ?, ?, ?, ?)`,
			names.NewDeviceID(), o.id, o.hash, o.created*1000, o.seen*1000); err != nil {
			return err
		}
	}
	// SQLite can't drop a UNIQUE column, so the table is rebuilt.
	for _, q := range []string{
		`CREATE TABLE visitors_new (id TEXT PRIMARY KEY, created_at INTEGER NOT NULL, last_seen_at INTEGER NOT NULL, stored INTEGER NOT NULL DEFAULT 0)`,
		`INSERT INTO visitors_new (id, created_at, last_seen_at, stored) SELECT id, created_at, last_seen_at, stored FROM visitors`,
		`DROP TABLE visitors`,
		`ALTER TABLE visitors_new RENAME TO visitors`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type Device struct {
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	Created  int64   `json:"created"`
	LastSeen int64   `json:"lastSeen"`
	LinkedBy *string `json:"linkedBy"`
	Current  bool    `json:"current"`
}

// Devices lists a visitor's devices, oldest first.
func (s *Store) Devices(visitor, current string) ([]Device, error) {
	rows, err := s.srv.Query(`SELECT id, label, created_at, last_seen_at, linked_by FROM devices WHERE visitor = ? ORDER BY created_at, id`, visitor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.Label, &d.Created, &d.LastSeen, &d.LinkedBy); err != nil {
			return nil, err
		}
		d.Current = d.ID == current
		out = append(out, d)
	}
	return out, rows.Err()
}

var noDevice = &apierr.Error{Status: 404, Code: "not_found", Message: "This visitor has no device with that ID."}

func (s *Store) RenameDevice(visitor, id, label string) error {
	res, err := s.srv.Exec(`UPDATE devices SET label = ? WHERE id = ? AND visitor = ?`, label, id, visitor)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return noDevice
	}
	return nil
}

// RemoveDevice signs one of a visitor's devices out: its token stops
// working. The last device can't go, since nothing could reach the visitor
// after it.
func (s *Store) RemoveDevice(visitor, id string) error {
	tx, err := s.srv.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n, mine int
	tx.QueryRow(`SELECT count(*), coalesce(sum(id = ?), 0) FROM devices WHERE visitor = ?`, id, visitor).Scan(&n, &mine)
	if mine == 0 {
		return noDevice
	}
	if n == 1 {
		return &apierr.Error{Status: 409, Code: "last_device", Message: "This is the visitor's only device. Signing it out would leave nothing that can act as this visitor."}
	}
	if _, err := tx.Exec(`DELETE FROM devices WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveOtherDevices signs out every device but keep, and returns their IDs.
func (s *Store) RemoveOtherDevices(visitor, keep string) ([]string, error) {
	tx, err := s.srv.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id FROM devices WHERE visitor = ? AND id <> ?`, visitor, keep)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if _, err := tx.Exec(`DELETE FROM devices WHERE visitor = ? AND id <> ?`, visitor, keep); err != nil {
		return nil, err
	}
	return ids, tx.Commit()
}

// Link limits. A link is for one device, used once, alive for minutes.
const (
	LinkTTLDefault = 10 * 60 * 1000 // ms
	LinkTTLMax     = 60 * 60 * 1000
	LinkTTLMin     = 30 * 1000
	MaxOpenLinks   = 3
)

type Link struct {
	ID      string  `json:"id"`
	State   string  `json:"state"` // open, used, expired, cancelled
	Created int64   `json:"created"`
	Expires int64   `json:"expires"`
	Ended   *int64  `json:"ended"`
	Device  *Device `json:"device"` // the device it made, once used
}

func (s *Store) scanLink(row *sql.Row, now int64) (*Link, error) {
	var l Link
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
		err := s.srv.QueryRow(`SELECT id, label, created_at, last_seen_at, linked_by FROM devices WHERE id = ?`, dev.String).
			Scan(&d.ID, &d.Label, &d.Created, &d.LastSeen, &d.LinkedBy)
		if err == nil {
			l.Device = &d
		} else {
			// Signed out since: still say which one it was.
			l.Device = &Device{ID: dev.String}
		}
	}
	return &l, nil
}

const linkCols = `id, state, created_at, expires, ended_at, device`

// CreateLink makes a one-use code that turns another device into this
// visitor. It returns the link and its code; only the hash is kept.
func (s *Store) CreateLink(visitor, device string, ttl int64) (*Link, string, error) {
	now := NowMs()
	tx, err := s.srv.Begin()
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	var open int
	tx.QueryRow(`SELECT count(*) FROM links WHERE visitor = ? AND state = 'open' AND expires > ?`, visitor, now).Scan(&open)
	if open >= MaxOpenLinks {
		return nil, "", &apierr.Error{Status: 409, Code: "too_many_links",
			Message: "This visitor already has 3 open links. Cancel one, or wait for it to expire.", Extra: map[string]any{"limit": MaxOpenLinks}}
	}
	id, code := names.NewLinkID(), names.NewLinkCode()
	if _, err := tx.Exec(`INSERT INTO links (id, code_hash, visitor, by_device, created_at, expires) VALUES (?, ?, ?, ?, ?, ?)`,
		id, names.HashSecret(code), visitor, device, now, now+ttl); err != nil {
		return nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}
	return &Link{ID: id, State: "open", Created: now, Expires: now + ttl}, code, nil
}

// GetLink is one of a visitor's links, nil if there's no such link of theirs.
func (s *Store) GetLink(visitor, id string) (*Link, error) {
	return s.scanLink(s.srv.QueryRow(`SELECT `+linkCols+` FROM links WHERE id = ? AND visitor = ?`, id, visitor), NowMs())
}

// CancelLink ends an open link. Cancelling one that already ended changes
// nothing and returns it as it is.
func (s *Store) CancelLink(visitor, id string) (*Link, error) {
	now := NowMs()
	if _, err := s.srv.Exec(`UPDATE links SET state = 'cancelled', ended_at = ? WHERE id = ? AND visitor = ? AND state = 'open' AND expires > ?`, now, id, visitor, now); err != nil {
		return nil, err
	}
	return s.GetLink(visitor, id)
}

// Redeemed is what redeeming a link gives the new device.
type Redeemed struct {
	Token    string
	Visitor  string
	Device   string
	LinkID   string
	Previous string // the visitor this device was before, "" if none
	Deleted  bool   // Previous had no other device left and was deleted
}

// LinkFailure is a code that exists but can't be used any more. Bad codes
// are nil, so the caller counts them against the IP.
var (
	ErrLinkUnknown = &apierr.Error{Status: 404, Code: "bad_link", Message: "No such link code. Check it, or make a new one on the other device."}
	ErrSameVisitor = &apierr.Error{Status: 409, Code: "same_visitor", Message: "This device is already that visitor. The code is still unused."}
)

func linkEnded(state string) error {
	switch state {
	case "used":
		return &apierr.Error{Status: 410, Code: "link_used", Message: "This link code was already used. Make a new one on the other device."}
	case "cancelled":
		return &apierr.Error{Status: 410, Code: "link_cancelled", Message: "This link code was cancelled. Make a new one on the other device."}
	}
	return &apierr.Error{Status: 410, Code: "link_expired", Message: "This link code expired. Make a new one on the other device."}
}

// RedeemLink makes a new device for the link's visitor. If the redeeming
// device already was a visitor (prevVisitor, prevDevice), that device leaves
// it; a previous visitor with no device left is deleted with everything it
// had. Redeeming always wins: whoever calls this has decided.
func (s *Store) RedeemLink(code, label, prevVisitor, prevDevice string) (*Redeemed, error) {
	now := NowMs()
	tx, err := s.srv.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id, visitor, by, state string
	var expires int64
	err = tx.QueryRow(`SELECT id, visitor, by_device, state, expires FROM links WHERE code_hash = ?`, names.HashSecret(code)).
		Scan(&id, &visitor, &by, &state, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrLinkUnknown
	}
	if err != nil {
		return nil, err
	}
	if state != "open" || expires <= now {
		return nil, linkEnded(state)
	}
	if prevVisitor == visitor {
		return nil, ErrSameVisitor
	}
	var exists int
	if tx.QueryRow(`SELECT 1 FROM visitors WHERE id = ?`, visitor).Scan(&exists) != nil {
		return nil, linkEnded("expired") // the visitor went away meanwhile
	}
	r := &Redeemed{Token: names.RandomToken("vt_"), Visitor: visitor, Device: names.NewDeviceID(), LinkID: id, Previous: prevVisitor}
	if err := insertDevice(tx, r.Device, visitor, r.Token, label, &by); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE links SET state = 'used', ended_at = ?, device = ? WHERE id = ?`, now, r.Device, id); err != nil {
		return nil, err
	}
	tx.Exec(`UPDATE visitors SET last_seen_at = ? WHERE id = ?`, nowS(), visitor)
	left := 0
	if prevDevice != "" {
		if _, err := tx.Exec(`DELETE FROM devices WHERE id = ?`, prevDevice); err != nil {
			return nil, err
		}
		tx.QueryRow(`SELECT count(*) FROM devices WHERE visitor = ?`, prevVisitor).Scan(&left)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if prevVisitor != "" && left == 0 {
		r.Deleted = true
		if err := s.PurgeVisitor(prevVisitor); err != nil {
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
