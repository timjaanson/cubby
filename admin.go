package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// Admin commands are requests to the running server over a Unix socket in
// the data directory. The server does the work, exactly as for a client
// request, so watchers see deletions and nothing touches files behind its back.
// Only the server's OS user can open the socket, and that's the whole access
// check. There is no admin route over TCP.

const adminUsage = `cubby admin <command> [--data DIR]

  spaces            list spaces with size, keys and last activity
  export <space>    dump every key, then every series point, as JSON lines
  delete <space>    delete a space and its file
  rekey <space>     lock a space, or replace its key, and print the new key
  stats             totals, visitors and the largest spaces
  cleanup           run the hourly cleanup now

Talks to the running server through admin.sock in its data directory, so run
it on the server machine as the same user. Pass --data if the server uses a
non-default data directory.
`

func adminSocketPath(dataDir string) string { return filepath.Join(dataDir, "admin.sock") }

// Unix socket paths are limited to about 104 bytes, which a deep data
// directory can exceed. Both sides change into the data directory and use the
// short relative name, so the length of the full path never matters.
const adminSocketName = "admin.sock"

// listenAdmin opens the admin socket. It refuses if another server is
// already using the data directory.
func listenAdmin(dataDir string) (net.Listener, error) {
	if err := os.Chdir(dataDir); err != nil {
		return nil, err
	}
	if c, err := net.DialTimeout("unix", adminSocketName, time.Second); err == nil {
		c.Close()
		return nil, fmt.Errorf("another cubby server is already using %s", dataDir)
	}
	os.Remove(adminSocketName) // stale, from a server that didn't shut down cleanly
	ln, err := net.Listen("unix", adminSocketName)
	if err != nil {
		return nil, fmt.Errorf("admin socket in %s: %w", dataDir, err)
	}
	if err := os.Chmod(adminSocketName, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// ---------- server side ----------

type adminStats struct {
	DataDir    string     `json:"dataDir"`
	Version    string     `json:"version"`
	Spaces     int        `json:"spaces"`
	MaxSpaces  int        `json:"maxSpaces"`
	Keys       int        `json:"keys"`
	Bytes      int64      `json:"bytes"`
	MaxBytes   int64      `json:"maxBytes"`
	Visitors   int        `json:"visitors"`
	Storing    int        `json:"storing"`
	Watches    int        `json:"watches"`
	Largest    []spaceRow `json:"largest"`
	StartedAgo string     `json:"uptime"`
}

func (s *Server) adminHandler() http.Handler {
	mux := http.NewServeMux()
	fail := func(w http.ResponseWriter, r *http.Request, err error) {
		(&reqCtx{w: w, r: r}).fail(err)
	}
	ok := func(w http.ResponseWriter, r *http.Request, v any) {
		(&reqCtx{w: w, r: r}).json(200, v)
	}
	space := func(w http.ResponseWriter, r *http.Request) (string, bool) {
		name := r.PathValue("space")
		if !validSpace(name) {
			fail(w, r, &apiError{Status: 400, Code: "invalid_space", Message: name + " isn't a valid space name."})
			return "", false
		}
		return name, true
	}

	mux.HandleFunc("GET /spaces", func(w http.ResponseWriter, r *http.Request) {
		ok(w, r, s.store.spaces())
	})
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		rows := s.store.spaces()
		st := adminStats{DataDir: s.cfg.Server.DataDir, Version: version, Spaces: len(rows), MaxSpaces: s.cfg.Limits.MaxSpaces,
			MaxBytes: int64(s.cfg.Limits.MaxTotalBytes), StartedAgo: time.Since(buildTime).Round(time.Second).String()}
		for _, row := range rows {
			st.Keys += row.Keys
			st.Bytes += row.Bytes
		}
		s.store.srv.QueryRow(`SELECT count(*), coalesce(sum(stored), 0) FROM visitors`).Scan(&st.Visitors, &st.Storing)
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
		if !s.store.spaceExists(name) {
			fail(w, r, errNoSpace)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		err := s.store.export(name, func(e Entry) error {
			area := "shared"
			switch {
			case strings.HasPrefix(e.Area, "u:"):
				area = "users/" + e.Area[2:]
			case strings.HasPrefix(e.Area, "m:"):
				area = "me:" + e.Area[2:]
			}
			it := toItem(e)
			line := map[string]any{"area": area, "key": e.Key, "value": it.Value, "version": e.Version, "writer": e.Writer}
			if e.Series {
				line["series"], line["t"] = true, e.T
			}
			return enc.Encode(line)
		}, func(p Point) error {
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
		if err := s.store.deleteSpace(name); err != nil {
			fail(w, r, err)
			return
		}
		ok(w, r, map[string]any{"deleted": name})
	})
	mux.HandleFunc("POST /spaces/{space}/rekey", func(w http.ResponseWriter, r *http.Request) {
		name, good := space(w, r)
		if !good {
			return
		}
		key, err := s.store.rekey(name)
		if err != nil {
			fail(w, r, err)
			return
		}
		ok(w, r, map[string]any{"space": name, "key": key})
	})
	mux.HandleFunc("POST /cleanup", func(w http.ResponseWriter, r *http.Request) {
		rep, err := s.store.cleanup()
		if err != nil {
			fail(w, r, err)
			return
		}
		ok(w, r, rep)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fail(w, r, &apiError{Status: 404, Code: "not_found", Message: "No such admin command."})
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

// ---------- client side ----------

func cmdAdmin(args []string) int {
	f, err := parseFlags(args, []string{"data"}, []string{"json"})
	if err != nil {
		return usageErr(err)
	}
	if len(f.args) == 0 {
		fmt.Print(adminUsage)
		return 64
	}
	dataDir, err := findDataDir(f.str("data"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "cubby: server_not_running:", err)
		return 6
	}
	sock := adminSocketPath(dataDir)
	if err := os.Chdir(dataDir); err != nil {
		fmt.Fprintf(os.Stderr, "cubby: server_not_running: can't open %s: %v\n", dataDir, err)
		return 6
	}
	hc := &http.Client{
		Timeout: 10 * time.Minute,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", adminSocketName)
		}},
	}
	call := func(method, path string) (*http.Response, error) {
		req, _ := http.NewRequest(method, "http://admin"+path, nil)
		resp, err := hc.Do(req)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "no such file") || strings.Contains(err.Error(), "connection refused") {
				return nil, &cliError{code: 6, err: "server_not_running",
					msg: fmt.Sprintf("no server is running with data in %s. Start it with 'cubby serve', or pass --data DIR.", dataDir)}
			}
			if strings.Contains(err.Error(), "permission denied") {
				return nil, &cliError{code: 3, err: "permission_denied", msg: "run admin as the same user as the server: " + sock}
			}
			return nil, &cliError{code: 6, err: "network_error", msg: err.Error()}
		}
		if resp.StatusCode >= 300 {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, apiErrFrom(resp, b)
		}
		return resp, nil
	}
	decode := func(resp *http.Response, v any) error {
		defer resp.Body.Close()
		if f.bool("json") {
			io.Copy(os.Stdout, resp.Body)
			return errJSONPrinted
		}
		return json.NewDecoder(resp.Body).Decode(v)
	}

	cmd, rest := f.args[0], f.args[1:]
	needSpace := func() (string, error) {
		if len(rest) != 1 {
			return "", &cliError{code: 64, msg: "admin " + cmd + " takes one space name"}
		}
		return rest[0], nil
	}

	err = func() error {
		switch cmd {
		case "spaces":
			resp, err := call("GET", "/spaces")
			if err != nil {
				return err
			}
			var rows []spaceRow
			if err := decode(resp, &rows); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SPACE\tKIND\tSIZE\tKEYS\tLAST ACTIVE")
			for _, r := range rows {
				kind := "open"
				if r.Locked {
					kind = "locked"
				}
				if r.Pinned {
					kind += ",pinned"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", r.Name, kind, humanBytes(r.Bytes), r.Keys, time.Unix(r.LastActive, 0).Format("2006-01-02 15:04"))
			}
			return tw.Flush()
		case "stats":
			resp, err := call("GET", "/stats")
			if err != nil {
				return err
			}
			var st adminStats
			if err := decode(resp, &st); err != nil {
				return err
			}
			fmt.Printf("server     %s, up %s\ndata       %s\nspaces     %d of %d\nkeys       %d\nstored     %s of %s\nvisitors   %d (%d have stored something)\nwatches    %d open\n",
				st.Version, st.StartedAgo, st.DataDir, st.Spaces, st.MaxSpaces, st.Keys, humanBytes(st.Bytes), humanBytes(st.MaxBytes), st.Visitors, st.Storing, st.Watches)
			if len(st.Largest) > 0 {
				fmt.Println("largest:")
				for _, r := range st.Largest {
					fmt.Printf("  %-40s %9s %6d keys\n", r.Name, humanBytes(r.Bytes), r.Keys)
				}
			}
			return nil
		case "export":
			name, err := needSpace()
			if err != nil {
				return err
			}
			resp, err := call("GET", "/spaces/"+name+"/export")
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			_, err = io.Copy(os.Stdout, resp.Body)
			return err
		case "delete":
			name, err := needSpace()
			if err != nil {
				return err
			}
			resp, err := call("DELETE", "/spaces/"+name)
			if err != nil {
				return err
			}
			resp.Body.Close()
			fmt.Println("deleted", name)
			return nil
		case "rekey":
			name, err := needSpace()
			if err != nil {
				return err
			}
			resp, err := call("POST", "/spaces/"+name+"/rekey")
			if err != nil {
				return err
			}
			var out struct{ Key string }
			if err := decode(resp, &out); err != nil {
				return err
			}
			fmt.Println(out.Key)
			return nil
		case "cleanup":
			resp, err := call("POST", "/cleanup")
			if err != nil {
				return err
			}
			var rep cleanupReport
			if err := decode(resp, &rep); err != nil {
				return err
			}
			fmt.Printf("removed %d visitors, %d tombstones, %d spaces\n", rep.Visitors, rep.Tombstones, rep.Spaces)
			return nil
		}
		return &cliError{code: 64, msg: fmt.Sprintf("unknown admin command %q", cmd)}
	}()

	var ce *cliError
	switch {
	case err == nil, errors.Is(err, errJSONPrinted):
		return 0
	case errors.As(err, &ce):
		if ce.code == 64 {
			fmt.Fprintf(os.Stderr, "cubby: %s\n\n%s", ce.msg, adminUsage)
			return 64
		}
		fmt.Fprintf(os.Stderr, "cubby: %s: %s\n", ce.err, ce.msg)
		return ce.code
	}
	fmt.Fprintln(os.Stderr, "cubby:", err)
	return 6
}

var errJSONPrinted = errors.New("printed as JSON")

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
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
