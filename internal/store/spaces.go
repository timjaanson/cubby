package store

import (
	"errors"
	"os"
	"sort"
	"strings"

	"github.com/timjaanson/cubby/internal/access"
	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/names"
)

// CreateSpace makes a new empty space owned by the visitor. With name it
// takes exactly that name, which the caller has validated, and fails with
// space_exists if it's taken; otherwise it generates one from label. It
// returns the name and the owner token, which is stored only as a hash.
func (s *Store) CreateSpace(name, label string, settings access.Settings, owner string) (string, string, error) {
	if s.SpaceCount() >= s.cfg.Limits.MaxSpaces {
		return "", "", apierr.ServerFull
	}
	ownerToken := names.RandomToken("ot_")
	ns := &newSpace{settings: settings, ownerHash: names.HashSecret(ownerToken), owner: owner}
	fixed := name != ""
	for range 10 {
		if !fixed {
			name = names.NewSpaceName(label)
		}
		if s.SpaceExists(name) {
			if fixed {
				return "", "", apierr.SpaceExists(name)
			}
			continue
		}
		sp, created, err := s.acquire(name, ns)
		if err != nil {
			return "", "", err
		}
		s.Release(sp)
		if created {
			s.noteVisitorSpace(owner, name)
			return name, ownerToken, nil
		}
		if fixed {
			return "", "", apierr.SpaceExists(name) // lost a race with another request
		}
	}
	return "", "", errors.New("could not generate a free name")
}

// deleteSpace removes a space's file. Its counter is remembered, so a space
// created later under the same name continues above it.
func (s *Store) DeleteSpace(name string) error {
	for {
		sp, err := s.Acquire(name)
		if err != nil {
			return err
		}
		if sp == nil {
			return apierr.NoSpace
		}
		s.Release(sp)
		sp.gate.Lock()
		if sp.dead {
			sp.gate.Unlock()
			continue
		}
		m, _ := readMeta(sp.db)
		sp.dead = true
		sp.db.Close()
		s.mu.Lock()
		delete(s.open, name)
		if st := s.stats[name]; st != nil {
			s.total -= st.bytes
			delete(s.stats, name)
			delete(s.expiring, name)
		}
		s.mu.Unlock()
		path := s.spacePath(name)
		for _, suf := range []string{"", "-wal", "-shm"} {
			os.Remove(path + suf)
		}
		sp.gate.Unlock()

		s.srv.Exec(`INSERT INTO graveyard (name, seq) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET seq = max(seq, excluded.seq)`, name, m.seq)
		s.srv.Exec(`DELETE FROM visitor_spaces WHERE space = ?`, name)
		s.cacheMu.Lock()
		s.vbytes = map[string]int64{}
		for k := range s.vspaces {
			if strings.HasSuffix(k, " "+name) {
				delete(s.vspaces, k)
			}
		}
		s.cacheMu.Unlock()
		s.OnChange(name)
		return nil
	}
}

type SpaceRow struct {
	Name       string          `json:"name"`
	Settings   access.Settings `json:"settings"`
	Pinned     bool            `json:"pinned"`
	Bytes      int64           `json:"bytes"`
	Keys       int             `json:"keys"`
	LastActive int64           `json:"lastActive"`
}

func (s *Store) Spaces() []SpaceRow {
	s.Rescan()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SpaceRow, 0, len(s.stats))
	for n, st := range s.stats {
		out = append(out, SpaceRow{Name: n, Settings: st.settings, Pinned: s.cfg.Pinned(n), Bytes: st.bytes, Keys: st.keys, LastActive: st.lastActive})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
