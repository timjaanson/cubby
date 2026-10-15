package store

import (
	"errors"
	"os"
	"sort"
	"strings"

	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/names"
)

// createSpace makes a new empty space with a generated name. With locked it
// also returns the space key.
func (s *Store) CreateSpace(label string, locked bool) (name, key string, err error) {
	if s.SpaceCount() >= s.cfg.Limits.MaxSpaces {
		return "", "", apierr.ServerFull
	}
	for range 10 {
		name = names.NewSpaceName(label)
		if s.SpaceExists(name) {
			continue
		}
		kh := ""
		if locked {
			key = names.RandomToken("sk_")
			kh = names.HashSecret(key)
		}
		sp, created, err := s.Acquire(name, true, kh)
		if err != nil {
			return "", "", err
		}
		s.Release(sp)
		if created {
			return name, key, nil
		}
	}
	return "", "", errors.New("could not generate a free name")
}

// CreateNamedSpace creates a space under the exact name given, which the
// caller has validated. It fails with space_exists if the name is taken.
func (s *Store) CreateNamedSpace(name string, locked bool) (key string, err error) {
	if s.SpaceCount() >= s.cfg.Limits.MaxSpaces {
		return "", apierr.ServerFull
	}
	if s.SpaceExists(name) {
		return "", apierr.SpaceExists(name)
	}
	kh := ""
	if locked {
		key = names.RandomToken("sk_")
		kh = names.HashSecret(key)
	}
	sp, created, err := s.Acquire(name, true, kh)
	if err != nil {
		return "", err
	}
	s.Release(sp)
	if !created {
		return "", apierr.SpaceExists(name) // lost a race with another request
	}
	return key, nil
}

// rekey locks a space or replaces its key.
func (s *Store) Rekey(name string) (string, error) {
	sp, _, err := s.Acquire(name, false, "")
	if err != nil {
		return "", err
	}
	if sp == nil {
		return "", apierr.NoSpace
	}
	defer s.Release(sp)
	key := names.RandomToken("sk_")
	sp.wmu.Lock()
	err = setMeta(sp.db, "key_hash", names.HashSecret(key))
	sp.wmu.Unlock()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	if st := s.stats[name]; st != nil {
		st.locked = true
	}
	s.mu.Unlock()
	return key, nil
}

// deleteSpace removes a space's file. Its counter is remembered, so a space
// created later under the same name continues above it.
func (s *Store) DeleteSpace(name string) error {
	for {
		sp, _, err := s.Acquire(name, false, "")
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
	Name       string `json:"name"`
	Locked     bool   `json:"locked"`
	Pinned     bool   `json:"pinned"`
	Bytes      int64  `json:"bytes"`
	Keys       int    `json:"keys"`
	LastActive int64  `json:"lastActive"`
}

func (s *Store) Spaces() []SpaceRow {
	s.Rescan()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SpaceRow, 0, len(s.stats))
	for n, st := range s.stats {
		out = append(out, SpaceRow{Name: n, Locked: st.locked, Pinned: s.cfg.Pinned(n), Bytes: st.bytes, Keys: st.keys, LastActive: st.lastActive})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
