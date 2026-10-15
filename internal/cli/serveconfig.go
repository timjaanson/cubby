package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/timjaanson/cubby/internal/config"
)

// serveFlags are the flags that shape a server's config, shared by serve
// and config so both resolve settings the same way.
var serveFlags = []string{"listen", "port", "p", "data", "config"}

// serveConfig builds the config a server would run with: defaults, then the
// config file, then environment variables, then flags. It doesn't touch the
// data directory. sources lists what was read, for display.
func serveConfig(f *flags) (cfg *config.Config, sources []string, err error) {
	file := f.str("config")
	switch {
	case file != "":
		sources = append(sources, "--config "+file)
	case os.Getenv("CUBBY_CONFIG") != "":
		file = os.Getenv("CUBBY_CONFIG")
		sources = append(sources, "CUBBY_CONFIG="+file)
	}
	cfg, err = config.Load(file)
	if err != nil {
		return nil, nil, err
	}

	given := nonEmpty(f.str("listen"), f.str("port"), f.str("p"))
	if len(given) > 1 {
		return nil, nil, badUsage{fmt.Errorf("give one of --listen, --port or -p, not %d", len(given))}
	}
	listen := ""
	switch {
	case len(given) == 1:
		listen = given[0]
		sources = append(sources, "--listen/--port "+listen)
	case os.Getenv("CUBBY_LISTEN") != "":
		listen = os.Getenv("CUBBY_LISTEN")
		sources = append(sources, "CUBBY_LISTEN="+listen)
	}
	if listen != "" {
		addr, err := config.ListenAddr(listen)
		if err != nil {
			return nil, nil, badUsage{err}
		}
		cfg.Server.Listen = addr
	}

	switch {
	case f.str("data") != "":
		sources = append(sources, "--data "+f.str("data"))
	case os.Getenv("CUBBY_DATA") != "":
		sources = append(sources, "CUBBY_DATA="+os.Getenv("CUBBY_DATA"))
	}
	return cfg, sources, nil
}

// badUsage marks an error in what the user typed, as opposed to a broken
// config file, so it gets the "run cubby help" hint.
type badUsage struct{ error }

// configErr prints a serveConfig error and returns the exit code.
func configErr(err error) int {
	if _, ok := err.(badUsage); ok {
		return usageErr(err)
	}
	fmt.Fprintln(os.Stderr, "cubby:", err)
	return 64
}

const configUsage = `cubby config [--defaults] [--config FILE] [-p PORT | --listen ADDR] [--data DIR]

Print the server config as a TOML file, with a comment on every setting.

By default it shows what 'cubby serve' would use with the same flags and
environment: the built-in defaults, then the config file, then CUBBY_* env
vars, then flags. Settings that differ from the default say so.

  --defaults    print the built-in defaults only, ignoring env and files

Start a config file with:
  cubby config --defaults > cubby.toml
  cubby serve --config cubby.toml
`

func cmdConfig(args []string) int {
	f, err := parseFlags(args, serveFlags, []string{"defaults"})
	if err == nil && len(f.args) > 0 {
		err = fmt.Errorf("config takes no arguments, got %q", f.args[0])
	}
	if err != nil {
		return usageErr(err)
	}
	def := config.Default()
	if f.bool("defaults") {
		if len(f.vals) > 0 {
			return usageErr(fmt.Errorf("--defaults ignores other settings, so don't combine them"))
		}
		fmt.Print("# cubby server config: the built-in defaults.\n")
		fmt.Print("# Every setting is optional. Delete what you don't change, then run:\n")
		fmt.Print("#   cubby serve --config FILE    (or CUBBY_CONFIG=FILE cubby serve)\n\n")
		fmt.Print(def.TOML(def, "On this machine that would be "+config.PreviewDataDir()+"."))
		return 0
	}

	cfg, sources, err := serveConfig(f)
	if err != nil {
		return configErr(err)
	}
	dir, how := config.DataDirPreview(f.str("data"), cfg.Server.DataDir)
	note := "cubby serve would use " + dir + "."
	if dir == "" {
		note = "cubby serve would find " + how + "."
	} else if how == "existing data" {
		note = "cubby serve would use " + dir + ", where data already is."
	}
	if strings.HasPrefix(how, "from ") && dir != "" {
		cfg.Server.DataDir = dir // explicit, so print it as a setting
	} else if cfg.Server.DataDir != "" {
		cfg.Server.DataDir, _ = filepath.Abs(cfg.Server.DataDir)
	}

	fmt.Print("# cubby server config: what 'cubby serve' would use with these inputs.\n")
	if len(sources) == 0 {
		fmt.Print("# Read from: nothing, so these are the built-in defaults.\n")
	} else {
		fmt.Printf("# Read from: %s.\n", strings.Join(sources, ", "))
	}
	fmt.Print("# Every setting is optional. Save to a file and run: cubby serve --config FILE\n\n")
	fmt.Print(cfg.TOML(def, note))
	return 0
}
