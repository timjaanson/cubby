package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Where the server keeps its data when nobody says.
//
// An explicit directory (--data, CUBBY_DATA, data_dir in the config file,
// or systemd's STATE_DIRECTORY) is used as given, and a server that can't
// write it stops with a clear error. Without one, the server tries these in
// order:
//
//  1. the system directory, when running as root:
//     /var/lib/cubby on Linux, /Library/Application Support/cubby on macOS
//  2. the user's data directory: $XDG_DATA_HOME/cubby if that's an absolute
//     path, else ~/Library/Application Support/cubby on macOS,
//     %LOCALAPPDATA%\cubby on Windows, ~/.local/share/cubby elsewhere
//  3. ./cubby-data in the current directory, for users without a writable
//     home, such as service accounts and containers with a random UID
//
// A candidate that already holds data wins over everything else, so data is
// never left behind because, say, a home directory became writable later.
// Otherwise the first candidate that can actually be created and written is
// used. Nothing goes to a temporary directory, where data would vanish.

type dataDirCandidate struct {
	dir      string
	label    string
	fallback bool
}

func dataDirCandidates() []dataDirCandidate {
	var out []dataDirCandidate
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		switch runtime.GOOS {
		case "linux":
			out = append(out, dataDirCandidate{dir: "/var/lib/cubby", label: "system directory"})
		case "darwin":
			out = append(out, dataDirCandidate{dir: "/Library/Application Support/cubby", label: "system directory"})
		}
	}
	if d := userDataDir(); d != "" {
		out = append(out, dataDirCandidate{dir: d, label: "user data directory"})
	}
	if wd, err := os.Getwd(); err == nil {
		out = append(out, dataDirCandidate{dir: filepath.Join(wd, "cubby-data"), label: "current directory", fallback: true})
	}
	return out
}

func userDataDir() string {
	if x := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(x) && runtime.GOOS != "windows" {
		return filepath.Join(x, "cubby")
	}
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "cubby")
		}
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "/" || !filepath.IsAbs(home) {
		return "" // no usable home, as for many service accounts and containers
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "cubby")
	}
	return filepath.Join(home, ".local", "share", "cubby")
}

// explicitDataDir returns a directory the user chose, if any, and where it came from.
func explicitDataDir(flag, fromConfig string) (dir, source string) {
	switch {
	case flag != "":
		return flag, "--data"
	case os.Getenv("CUBBY_DATA") != "":
		return os.Getenv("CUBBY_DATA"), "CUBBY_DATA"
	case fromConfig != "":
		return fromConfig, "data_dir in the config file"
	case os.Getenv("STATE_DIRECTORY") != "":
		d, _, _ := strings.Cut(os.Getenv("STATE_DIRECTORY"), ":")
		return d, "systemd StateDirectory"
	}
	return "", ""
}

func hasData(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "server.db"))
	return err == nil
}

// writableDir creates dir if needed and proves a file can be written in it.
func writableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// resolveDataDir picks the data directory for the server. note explains a
// fallback choice, so the server can say why it didn't use the usual place.
func ResolveDataDir(flag, fromConfig string) (dir, note string, err error) {
	if d, src := explicitDataDir(flag, fromConfig); d != "" {
		abs, _ := filepath.Abs(d)
		if err := writableDir(abs); err != nil {
			return "", "", fmt.Errorf("can't use %s from %s: %w", abs, src, unwrapPathErr(err))
		}
		return abs, "", nil
	}
	cands := dataDirCandidates()
	for _, c := range cands {
		if hasData(c.dir) {
			if err := writableDir(c.dir); err != nil {
				return "", "", fmt.Errorf("existing data in %s isn't writable by this user: %w. Run as the user who owns it, or pass --data DIR", c.dir, unwrapPathErr(err))
			}
			return c.dir, "", nil
		}
	}
	var tried []string
	for i, c := range cands {
		err := writableDir(c.dir)
		if err == nil {
			if i > 0 {
				note = fmt.Sprintf("using %s because %s", c.label, strings.Join(tried, "; "))
			}
			return c.dir, note, nil
		}
		tried = append(tried, fmt.Sprintf("%s isn't writable (%v)", c.dir, unwrapPathErr(err)))
	}
	return "", "", fmt.Errorf("no writable place for data: %s. Pass --data DIR or set CUBBY_DATA", strings.Join(tried, "; "))
}

// findDataDir locates existing data without creating anything, for admin.
func FindDataDir(flag string) (string, error) {
	if d, _ := explicitDataDir(flag, ""); d != "" {
		return filepath.Abs(d)
	}
	var looked []string
	for _, c := range dataDirCandidates() {
		if hasData(c.dir) {
			return c.dir, nil
		}
		looked = append(looked, c.dir)
	}
	return "", fmt.Errorf("no cubby data found in %s. Start the server with 'cubby serve', or pass --data DIR", strings.Join(looked, ", "))
}

// PreviewDataDir says where a server would put its data, without creating it.
func PreviewDataDir() string {
	dir, how := DataDirPreview("", "")
	if how == "" {
		return dir
	}
	return dir + " (" + how + ")"
}

// DataDirPreview is ResolveDataDir without side effects: the directory a
// server started with these inputs would use, and why. dir is empty when
// nothing is writable.
func DataDirPreview(flag, fromConfig string) (dir, how string) {
	if d, src := explicitDataDir(flag, fromConfig); d != "" {
		abs, _ := filepath.Abs(d)
		return abs, "from " + src
	}
	cands := dataDirCandidates()
	for _, c := range cands {
		if hasData(c.dir) {
			return c.dir, "existing data"
		}
	}
	for _, c := range cands {
		if canCreate(c.dir) {
			return c.dir, ""
		}
	}
	return "", "nowhere writable, pass --data DIR"
}

// canCreate reports whether dir exists and is writable, or its nearest
// existing parent is, without changing anything.
func canCreate(dir string) bool {
	for d := dir; ; d = filepath.Dir(d) {
		if fi, err := os.Stat(d); err == nil {
			return fi.IsDir() && accessW(d)
		}
		if filepath.Dir(d) == d {
			return false
		}
	}
}

func unwrapPathErr(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", pe.Path, pe.Err)
	}
	return err
}
