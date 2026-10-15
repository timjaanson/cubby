package cli

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

	"github.com/timjaanson/cubby/internal/config"
	"github.com/timjaanson/cubby/internal/server"
	"github.com/timjaanson/cubby/internal/store"
	"github.com/timjaanson/cubby/internal/version"
)

func cmdServe(args []string) int {
	f, err := parseFlags(args, []string{"listen", "port", "p", "data", "config"}, []string{"quiet", "q"})
	if err == nil && len(f.args) > 0 {
		err = fmt.Errorf("serve takes no arguments, got %q", f.args[0])
	}
	if err != nil {
		return usageErr(err)
	}
	cfg, err := config.Load(firstNonEmpty(f.str("config"), os.Getenv("CUBBY_CONFIG")))
	if err != nil {
		fmt.Fprintln(os.Stderr, "cubby:", err)
		return 64
	}
	// Precedence: flag, then CUBBY_LISTEN, then the config file, then the default.
	listen := firstNonEmpty(f.str("listen"), f.str("port"), f.str("p"))
	if n := len(nonEmpty(f.str("listen"), f.str("port"), f.str("p"))); n > 1 {
		return usageErr(fmt.Errorf("give one of --listen, --port or -p, not %d", n))
	}
	if listen == "" {
		listen = os.Getenv("CUBBY_LISTEN")
	}
	if listen != "" {
		addr, err := config.ListenAddr(listen)
		if err != nil {
			return usageErr(err)
		}
		cfg.Server.Listen = addr
	}
	dir, note, err := config.ResolveDataDir(f.str("data"), cfg.Server.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cubby:", err)
		return 6
	}
	cfg.Server.DataDir = dir
	if note != "" {
		log.Printf("note: %s", note)
	}
	store, err := store.Open(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cubby:", err)
		return 6
	}
	defer store.Close()
	adminLn, err := server.ListenAdmin(cfg.Server.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cubby:", err)
		return 6
	}

	var reqLog *log.Logger
	if !f.bool("quiet") && !f.bool("q") {
		reqLog = log.New(os.Stderr, "", log.LstdFlags)
	}
	srv := server.New(cfg, store, reqLog)
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
	log.Printf("cubby %s listening on %s, data in %s (%d spaces)", version.Version, displayAddr(ln.Addr()), cfg.Server.DataDir, store.SpaceCount())

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
				rep, err := srv.Cleanup()
				if err != nil {
					log.Printf("cleanup: %v", err)
				} else if rep.Visitors+rep.Tombstones+rep.Spaces > 0 {
					log.Printf("cleanup: removed %d visitors, %d tombstones, %d spaces", rep.Visitors, rep.Tombstones, rep.Spaces)
				}
			}
		}
	}()

	admin := &http.Server{Handler: srv.AdminHandler(), ReadHeaderTimeout: 10 * time.Second}
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

func nonEmpty(v ...string) []string {
	var out []string
	for _, s := range v {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
