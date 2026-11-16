package store

import (
	"database/sql"
	"fmt"

	"github.com/timjaanson/cubby/internal/apierr"
)

// checkServerVersion refuses a server.db from before sign-in with keys.
func checkServerVersion(db *sql.DB, path string) error {
	var v, tables int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table'`).Scan(&tables)
	if tables > 0 && v != serverVersion {
		return fmt.Errorf("%s is format %d, from a cubby before v0.19. v0.19 changed sign-in to device keys and starts with an empty data directory: move the old one away", path, v)
	}
	return nil
}

type Device struct {
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	Created  int64   `json:"created"`
	LastSeen int64   `json:"lastSeen"`
	PairedBy *string `json:"pairedBy"`
	Current  bool    `json:"current"`
}

// Devices lists a visitor's devices, oldest first.
func (s *Store) Devices(visitor, current string) ([]Device, error) {
	rows, err := s.srv.Query(`SELECT id, label, created_at, last_seen_at, paired_by FROM devices WHERE visitor = ? ORDER BY created_at, id`, visitor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.Label, &d.Created, &d.LastSeen, &d.PairedBy); err != nil {
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

// RemoveDevice signs one of a visitor's devices out: its key and the API
// keys it minted stop working. The last device can't go, since nothing could reach the visitor
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
	if _, err := tx.Exec(`DELETE FROM api_keys WHERE device = ?`, id); err != nil {
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
	if _, err := tx.Exec(`DELETE FROM api_keys WHERE visitor = ? AND device <> ?`, visitor, keep); err != nil {
		return nil, err
	}
	return ids, tx.Commit()
}
