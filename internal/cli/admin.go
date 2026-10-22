package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/timjaanson/cubby/internal/config"
	"github.com/timjaanson/cubby/internal/server"
	"github.com/timjaanson/cubby/internal/store"
)

const adminUsage = `cubby admin <command> [--data DIR]

  spaces            list spaces with size, keys and last activity
  export <space>    dump every key, then every series point, as JSON lines
  delete <space>    delete a space and its file
  owner-token <space>
                    issue a new owner token, for an owner who lost theirs, and print it
  stats             totals, visitors and the largest spaces
  cleanup           run the hourly cleanup now

Talks to the running server through admin.sock in its data directory, so run
it on the server machine as the same user. Pass --data if the server uses a
non-default data directory.
`

func cmdAdmin(args []string) int {
	f, err := parseFlags(args, []string{"data"}, []string{"json"})
	if err != nil {
		return usageErr(err)
	}
	if len(f.args) == 0 {
		fmt.Print(adminUsage)
		return 64
	}
	dataDir, err := config.FindDataDir(f.str("data"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "cubby: server_not_running:", err)
		return 6
	}
	sock := server.AdminSocketPath(dataDir)
	if err := os.Chdir(dataDir); err != nil {
		fmt.Fprintf(os.Stderr, "cubby: server_not_running: can't open %s: %v\n", dataDir, err)
		return 6
	}
	hc := &http.Client{
		Timeout: 10 * time.Minute,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", server.AdminSocketName)
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
			var rows []store.SpaceRow
			if err := decode(resp, &rows); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SPACE\tREAD\tWRITE\tSIZE\tKEYS\tLAST ACTIVE")
			for _, r := range rows {
				st := r.Settings
				read, write := "granted", "granted"
				if st.Read {
					read = "anyone"
				}
				switch {
				case st.WriteShared && st.WriteUsers && st.WriteMe:
					write = "anyone"
				case st.WriteShared || st.WriteUsers || st.WriteMe:
					var parts []string
					for _, a := range []struct {
						n  string
						on bool
					}{{"shared", st.WriteShared}, {"users", st.WriteUsers}, {"me", st.WriteMe}} {
						v := "granted"
						if a.on {
							v = "anyone"
						}
						parts = append(parts, a.n+"="+v)
					}
					write = strings.Join(parts, ",")
				}
				name := r.Name
				if r.Pinned {
					name += " (pinned)"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", name, read, write, humanBytes(r.Bytes), r.Keys, time.Unix(r.LastActive, 0).Format("2006-01-02 15:04"))
			}
			return tw.Flush()
		case "stats":
			resp, err := call("GET", "/stats")
			if err != nil {
				return err
			}
			var st server.AdminStats
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
		case "owner-token":
			name, err := needSpace()
			if err != nil {
				return err
			}
			resp, err := call("POST", "/spaces/"+name+"/owner-token")
			if err != nil {
				return err
			}
			var out struct{ Owner string }
			if err := decode(resp, &out); err != nil {
				return err
			}
			fmt.Println(out.Owner)
			return nil
		case "cleanup":
			resp, err := call("POST", "/cleanup")
			if err != nil {
				return err
			}
			var rep store.CleanupReport
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
