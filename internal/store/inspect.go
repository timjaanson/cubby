package store

import "github.com/timjaanson/cubby/internal/apierr"

// Read-only views into the store's caches and counters, for admin stats and
// tests. Nothing here changes stored data.

// VisitorCounts returns how many visitors exist and how many store anything.
func (s *Store) VisitorCounts() (visitors, storing int) {
	s.srv.QueryRow(`SELECT count(*), coalesce(sum(stored), 0) FROM visitors`).Scan(&visitors, &storing)
	return
}

// SpaceBytes is the cached byte count of a space, 0 if it isn't loaded.
func (s *Store) SpaceBytes(name string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.stats[name]; st != nil {
		return st.bytes
	}
	return 0
}

// ForgetVisitorBytes drops a visitor's cached byte total so the next check
// recounts it from the space files. An empty id drops every visitor.
func (s *Store) ForgetVisitorBytes(id string) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if id == "" {
		s.vbytes = map[string]int64{}
		return
	}
	delete(s.vbytes, id)
}

// OpenSpaces is how many space files are open right now.
func (s *Store) OpenSpaces() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.open)
}

// PointCount is how many series points a space holds across all keys.
func (s *Store) PointCount(space string) (int, error) {
	sp, err := s.Acquire(space)
	if err != nil || sp == nil {
		return 0, err
	}
	defer s.Release(sp)
	var n int
	err = sp.db.QueryRow(`SELECT count(*) FROM points`).Scan(&n)
	return n, err
}

// Usage returns space totals and the caller's stored bytes across all spaces.
// No values or other visitors' totals are disclosed.
func (s *Store) Usage(space, id string) (bytes int64, keys int, visitor int64, err error) {
	sp, err := s.Acquire(space)
	if err != nil {
		return
	}
	if sp == nil {
		return 0, 0, 0, apierr.NoSpace
	}
	if err = s.expire(sp); err != nil {
		s.Release(sp)
		return
	}
	m, e := readMeta(sp.db)
	s.Release(sp)
	if e != nil {
		return 0, 0, 0, e
	}
	if id != "" {
		visitor, err = s.VisitorTotal(id)
	}
	return m.bytes, m.keys, visitor, err
}
