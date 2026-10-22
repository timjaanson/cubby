package server

// Admin commands are requests to the running server over a Unix socket in
// the data directory. The server does the work, exactly as for a client
// request, so watchers see deletions and nothing touches files behind its back.
// Only the server's OS user can open the socket, and that's the whole access
// check. There is no admin route over TCP.

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/timjaanson/cubby/internal/apierr"
	"github.com/timjaanson/cubby/internal/names"
	"github.com/timjaanson/cubby/internal/store"
	"github.com/timjaanson/cubby/internal/version"
)

type AdminStats struct {
	DataDir    string           `json:"dataDir"`
	Version    string           `json:"version"`
	Spaces     int              `json:"spaces"`
	MaxSpaces  int              `json:"maxSpaces"`
	Keys       int              `json:"keys"`
	Bytes      int64            `json:"bytes"`
	MaxBytes   int64            `json:"maxBytes"`
	Visitors   int              `json:"visitors"`
	Storing    int              `json:"storing"`
	Watches    int              `json:"watches"`
	Largest    []store.SpaceRow `json:"largest"`
	StartedAgo string           `json:"uptime"`
}

func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	fail := func(w http.ResponseWriter, r *http.Request, err error) {
		(&reqCtx{w: w, r: r}).fail(err)
	}
	ok := func(w http.ResponseWriter, r *http.Request, v any) {
		(&reqCtx{w: w, r: r}).json(200, v)
	}
	space := func(w http.ResponseWriter, r *http.Request) (string, bool) {
		name := r.PathValue("space")
		if !names.ValidSpace(name) {
			fail(w, r, &apierr.Error{Status: 400, Code: "invalid_space", Message: name + " isn't a valid space name."})
			return "", false
		}
		return name, true
	}

	mux.HandleFunc("GET /spaces", func(w http.ResponseWriter, r *http.Request) {
		ok(w, r, s.store.Spaces())
	})
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		rows := s.store.Spaces()
		st := AdminStats{DataDir: s.cfg.Server.DataDir, Version: version.Version, Spaces: len(rows), MaxSpaces: s.cfg.Limits.MaxSpaces,
			MaxBytes: int64(s.cfg.Limits.MaxTotalBytes), StartedAgo: time.Since(version.BuildTime).Round(time.Second).String()}
		for _, row := range rows {
			st.Keys += row.Keys
			st.Bytes += row.Bytes
		}
		st.Visitors, st.Storing = s.store.VisitorCounts()
		s.watches.mu.Lock()
		st.Watches = s.watches.total
		s.watches.mu.Unlock()
		sort.Slice(rows, func(i, j int) bool { return rows[i].Bytes > rows[j].Bytes })
		st.Largest = rows[:min(5, len(rows))]
		ok(w, r, st)
	})
	mux.HandleFunc("GET /spaces/{space}/export", func(w http.ResponseWriter, r *http.Request) {
		name, good := space(w, r)
		if !good {
			return
		}
		if !s.store.SpaceExists(name) {
			fail(w, r, apierr.NoSpace)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		err := s.store.Export(name, func(e store.Entry) error {
			area := "shared"
			switch {
			case strings.HasPrefix(e.Area, "u:"):
				area = "users/" + e.Area[2:]
			case strings.HasPrefix(e.Area, "m:"):
				area = "me:" + e.Area[2:]
			}
			it := toItem(e)
			line := map[string]any{"area": area, "key": e.Key, "value": it.Value, "version": e.Version, "writer": e.Writer}
			line["t"] = e.T
			if e.Expires > 0 {
				line["expires"] = e.Expires
			}
			if e.Series {
				line["series"], line["t"] = true, e.T
			}
			return enc.Encode(line)
		}, func(p store.Point) error {
			return enc.Encode(map[string]any{"area": adminArea(p.Area), "key": p.Key, "t": p.T, "value": jsonValue(p.Value, p.IsJSON)})
		})
		if err != nil {
			log.Printf("admin export %s: %v", name, err)
		}
	})
	mux.HandleFunc("DELETE /spaces/{space}", func(w http.ResponseWriter, r *http.Request) {
		name, good := space(w, r)
		if !good {
			return
		}
		if err := s.store.DeleteSpace(name); err != nil {
			fail(w, r, err)
			return
		}
		ok(w, r, map[string]any{"deleted": name})
	})
	// A new owner token, for an owner who lost theirs and every owner
	// identity. Owner grants from the old token go with it.
	mux.HandleFunc("POST /spaces/{space}/owner-token", func(w http.ResponseWriter, r *http.Request) {
		name, good := space(w, r)
		if !good {
			return
		}
		tok, err := s.store.RotateOwner(name, "")
		if err != nil {
			fail(w, r, err)
			return
		}
		ok(w, r, map[string]any{"space": name, "owner": tok})
	})
	mux.HandleFunc("POST /cleanup", func(w http.ResponseWriter, r *http.Request) {
		rep, err := s.Cleanup()
		if err != nil {
			fail(w, r, err)
			return
		}
		ok(w, r, rep)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fail(w, r, &apierr.Error{Status: 404, Code: "not_found", Message: "No such admin command."})
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		mux.ServeHTTP(sw, r)
		if s.log != nil {
			s.log.Printf("admin %s %s %d %s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
		}
	})
}

// adminArea turns a stored area into the form used in exports.
func adminArea(a string) string {
	switch {
	case strings.HasPrefix(a, "u:"):
		return "users/" + a[2:]
	case strings.HasPrefix(a, "m:"):
		return "me:" + a[2:]
	}
	return "shared"
}

func AdminSocketPath(dataDir string) string { return filepath.Join(dataDir, "admin.sock") }

// Unix socket paths are limited to about 104 bytes, which a deep data
// directory can exceed. Both sides change into the data directory and use the
// short relative name, so the length of the full path never matters.
const AdminSocketName = "admin.sock"

// listenAdmin opens the admin socket. It refuses if another server is
// already using the data directory.
func ListenAdmin(dataDir string) (net.Listener, error) {
	if err := os.Chdir(dataDir); err != nil {
		return nil, err
	}
	if c, err := net.DialTimeout("unix", AdminSocketName, time.Second); err == nil {
		c.Close()
		return nil, fmt.Errorf("another cubby server is already using %s", dataDir)
	}
	os.Remove(AdminSocketName) // stale, from a server that didn't shut down cleanly
	ln, err := net.Listen("unix", AdminSocketName)
	if err != nil {
		return nil, fmt.Errorf("admin socket in %s: %w", dataDir, err)
	}
	if err := os.Chmod(AdminSocketName, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}
