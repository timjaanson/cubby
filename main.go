// cubby is a self-hosted key-value store over plain HTTP. The same binary
// runs the server (cubby serve), administers its data (cubby admin) and
// acts as a command-line client (everything else).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// Set at build time with -ldflags "-X main.version=...".
var version = "dev"

var buildTime = time.Now()

const usage = `cubby: a key-value store over plain HTTP, server and client in one binary.

Server:
  cubby serve                     run the server on :8080 (see 'cubby help serve')
  cubby admin <command>           look after the server's data (spaces, export, delete, rekey, stats, cleanup)

Client (talks to $CUBBY_SERVER, default http://localhost:8080):
  cubby get <path>                print a value
  cubby set <path> [value|-]      write a value from the argument or stdin
  cubby del <path>                delete a value
  cubby ls <path>                 list a key or pattern, one tab-separated line per item
  cubby watch <path>              print changes as they happen
  cubby update <path>             read, change with --exec or $EDITOR, write back safely
  cubby add <path> [value]        add a point to a series ('space/shared/env.*' a=1 b=2 for several)
  cubby series <path>             print a series' points, oldest first
  cubby new [--label L] [--locked]  create a space on the server
  cubby name [label]              make up a valid space name, no server needed
  cubby whoami | token | url <path>

Paths mirror the URL after /v1/:
  poll-k7m2x9q4/shared/question     poll-k7m2x9q4/users/me/vote
  'poll-k7m2x9q4/shared/msg.*'      'poll-k7m2x9q4/users/*/vote'     poll-k7m2x9q4/me/draft

Run 'cubby help <command>' for flags, or see the README.
`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Print(usage)
		os.Exit(0)
	}
	cmd, rest := args[0], args[1:]
	var code int
	switch cmd {
	case "serve":
		code = cmdServe(rest)
	case "admin":
		code = cmdAdmin(rest)
	case "version", "--version", "-v":
		fmt.Println("cubby", version)
	case "help", "--help", "-h":
		if len(rest) > 0 {
			code = cmdHelp(rest[0])
		} else {
			fmt.Print(usage)
		}
	default:
		code = runClient(cmd, rest)
	}
	os.Exit(code)
}

func cmdServe(args []string) int {
	f, err := parseFlags(args, []string{"listen", "data", "config"}, []string{"quiet", "q"})
	if err == nil && len(f.args) > 0 {
		err = fmt.Errorf("serve takes no arguments, got %q", f.args[0])
	}
	if err != nil {
		return usageErr(err)
	}
	cfg, err := loadConfig(firstNonEmpty(f.str("config"), os.Getenv("CUBBY_CONFIG")))
	if err != nil {
		fmt.Fprintln(os.Stderr, "cubby:", err)
		return 64
	}
	if v := f.str("listen"); v != "" {
		cfg.Server.Listen = v
	}
	dir, note, err := resolveDataDir(f.str("data"), cfg.Server.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cubby:", err)
		return 6
	}
	cfg.Server.DataDir = dir
	if note != "" {
		log.Printf("note: %s", note)
	}
	store, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cubby:", err)
		return 6
	}
	defer store.Close()
	adminLn, err := listenAdmin(cfg.Server.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cubby:", err)
		return 6
	}

	var reqLog *log.Logger
	if !f.bool("quiet") && !f.bool("q") {
		reqLog = log.New(os.Stderr, "", log.LstdFlags)
	}
	srv := newServer(cfg, store, reqLog)
	hs := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	// Plain-text HTTP/2 too, so a proxy in front can multiplex watches.
	hs.Protocols = new(http.Protocols)
	hs.Protocols.SetHTTP1(true)
	hs.Protocols.SetUnencryptedHTTP2(true)

	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cubby:", err)
		return 6
	}
	log.Printf("cubby %s listening on %s, data in %s (%d spaces)", version, displayAddr(ln.Addr()), cfg.Server.DataDir, store.spaceCount())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				rep, err := store.cleanup()
				for _, l := range srv.lim.all() {
					l.prune()
				}
				if err != nil {
					log.Printf("cleanup: %v", err)
				} else if rep.Visitors+rep.Tombstones+rep.Spaces > 0 {
					log.Printf("cleanup: removed %d visitors, %d tombstones, %d spaces", rep.Visitors, rep.Tombstones, rep.Spaces)
				}
			}
		}
	}()

	admin := &http.Server{Handler: srv.adminHandler(), ReadHeaderTimeout: 10 * time.Second}
	go admin.Serve(adminLn)
	defer admin.Close()

	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "cubby:", err)
			return 6
		}
	case <-ctx.Done():
		log.Printf("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}
	return 0
}

func displayAddr(a net.Addr) string {
	s := a.String()
	if strings.HasPrefix(s, "[::]:") {
		return "http://localhost:" + strings.TrimPrefix(s, "[::]:")
	}
	if strings.HasPrefix(s, "0.0.0.0:") {
		return "http://localhost:" + strings.TrimPrefix(s, "0.0.0.0:")
	}
	return "http://" + s
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// ---------- flags ----------

// flags is a small parser that accepts flags anywhere on the line, so
// "cubby set path value --create" works as well as the other order.
type flags struct {
	vals map[string]string
	set  map[string]bool
	args []string
}

func parseFlags(in []string, withValue, boolean []string) (*flags, error) {
	f := &flags{vals: map[string]string{}, set: map[string]bool{}}
	isVal := map[string]bool{}
	for _, n := range withValue {
		isVal[n] = true
	}
	isBool := map[string]bool{}
	for _, n := range boolean {
		isBool[n] = true
	}
	for i := 0; i < len(in); i++ {
		a := in[i]
		if a == "--" {
			f.args = append(f.args, in[i+1:]...)
			break
		}
		// Values like -5 or -0.5 are arguments, not flags.
		if len(a) < 2 || a[0] != '-' || (a[1] >= '0' && a[1] <= '9') || a[1] == '.' {
			f.args = append(f.args, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		val, hasVal := "", false
		if k, v, ok := strings.Cut(name, "="); ok {
			name, val, hasVal = k, v, true
		}
		switch {
		case isVal[name]:
			if !hasVal {
				if i+1 >= len(in) {
					return nil, fmt.Errorf("flag --%s needs a value", name)
				}
				i++
				val = in[i]
			}
			f.vals[name] = val
			f.set[name] = true
		case isBool[name]:
			if hasVal {
				return nil, fmt.Errorf("flag --%s takes no value", name)
			}
			f.set[name] = true
		default:
			return nil, fmt.Errorf("unknown flag %s", a)
		}
	}
	return f, nil
}

func (f *flags) str(n string) string { return f.vals[n] }
func (f *flags) bool(n string) bool  { return f.set[n] }
func (f *flags) has(n string) bool   { return f.set[n] }

func usageErr(err error) int {
	fmt.Fprintf(os.Stderr, "cubby: %v\nRun 'cubby help' for usage.\n", err)
	return 64
}
