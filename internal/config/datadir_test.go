package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// isolate gives each case its own home and working directory, with no
// data directory set in the environment.
func isolate(t *testing.T) (home, work string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission cases need a non-root user")
	}
	root := t.TempDir()
	home, work = filepath.Join(root, "home"), filepath.Join(root, "work")
	os.MkdirAll(home, 0o755)
	os.MkdirAll(work, 0o755)
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("CUBBY_DATA", "")
	t.Setenv("STATE_DIRECTORY", "")
	t.Chdir(work)
	return home, work
}

func userDir(home string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "cubby")
	}
	return filepath.Join(home, ".local", "share", "cubby")
}

func readOnly(t *testing.T, dir string) {
	os.Chmod(dir, 0o500)
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
}

func TestDataDirNormalUser(t *testing.T) {
	home, _ := isolate(t)
	dir, note, err := ResolveDataDir("", "")
	if err != nil || dir != userDir(home) || note != "" {
		t.Fatalf("got %q %q %v", dir, note, err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("not created private: %v %v", fi, err)
	}
}

func TestDataDirNoUsableHome(t *testing.T) {
	_, work := isolate(t)
	t.Setenv("HOME", "/")
	dir, _, err := ResolveDataDir("", "")
	if err != nil || dir != filepath.Join(work, "cubby-data") {
		t.Fatalf("got %q %v", dir, err)
	}
}

func TestDataDirReadOnlyHome(t *testing.T) {
	home, work := isolate(t)
	readOnly(t, home)
	dir, note, err := ResolveDataDir("", "")
	if err != nil || dir != filepath.Join(work, "cubby-data") || !strings.Contains(note, "isn't writable") {
		t.Fatalf("got %q %q %v", dir, note, err)
	}
	// Admin finds the same place without being told.
	os.WriteFile(filepath.Join(dir, "server.db"), nil, 0o600)
	if found, err := FindDataDir(""); err != nil || found != dir {
		t.Fatalf("findDataDir: %q %v", found, err)
	}
}

func TestDataDirExistingDataWins(t *testing.T) {
	home, work := isolate(t)
	fallback := filepath.Join(work, "cubby-data")
	os.MkdirAll(fallback, 0o700)
	os.WriteFile(filepath.Join(fallback, "server.db"), nil, 0o600)
	dir, _, err := ResolveDataDir("", "")
	if err != nil || dir != fallback {
		t.Fatalf("got %q %v", dir, err)
	}
	if _, err := os.Stat(userDir(home)); err == nil {
		t.Fatal("created an empty user directory next to existing data")
	}
}

func TestDataDirExplicitNoFallback(t *testing.T) {
	_, work := isolate(t)
	locked := filepath.Join(work, "locked")
	os.MkdirAll(locked, 0o755)
	readOnly(t, locked)
	for _, set := range []func(){
		func() {},
		func() { t.Setenv("CUBBY_DATA", filepath.Join(locked, "d")) },
	} {
		set()
		flag := ""
		if os.Getenv("CUBBY_DATA") == "" {
			flag = filepath.Join(locked, "d")
		}
		_, _, err := ResolveDataDir(flag, "")
		if err == nil || !strings.Contains(err.Error(), "can't use") {
			t.Fatalf("explicit unwritable dir: %v", err)
		}
	}
	// Relative explicit paths become absolute.
	t.Setenv("CUBBY_DATA", "")
	if dir, _, err := ResolveDataDir("rel", ""); err != nil || dir != filepath.Join(work, "rel") {
		t.Fatalf("relative: %q %v", dir, err)
	}
	if dir, _, _ := ResolveDataDir("", "fromcfg"); dir != filepath.Join(work, "fromcfg") {
		t.Fatalf("config data_dir: %q", dir)
	}
}

func TestDataDirNothingWritable(t *testing.T) {
	home, work := isolate(t)
	readOnly(t, home)
	readOnly(t, work)
	_, _, err := ResolveDataDir("", "")
	if err == nil || !strings.Contains(err.Error(), "no writable place") || !strings.Contains(err.Error(), "--data") {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(PreviewDataDir(), "nowhere writable") {
		t.Fatalf("preview: %s", PreviewDataDir())
	}
}

func TestDataDirXDG(t *testing.T) {
	home, _ := isolate(t)
	xdg := filepath.Join(home, "xdg")
	t.Setenv("XDG_DATA_HOME", xdg)
	if dir, _, _ := ResolveDataDir("", ""); dir != filepath.Join(xdg, "cubby") {
		t.Fatalf("absolute XDG_DATA_HOME: %q", dir)
	}
	t.Setenv("XDG_DATA_HOME", "relative/xdg")
	if dir, _, _ := ResolveDataDir("", ""); dir != userDir(home) {
		t.Fatalf("relative XDG_DATA_HOME must be ignored: %q", dir)
	}
}

func TestDataDirSystemd(t *testing.T) {
	_, work := isolate(t)
	t.Setenv("STATE_DIRECTORY", filepath.Join(work, "state")+":"+filepath.Join(work, "other"))
	if dir, _, err := ResolveDataDir("", ""); err != nil || dir != filepath.Join(work, "state") {
		t.Fatalf("got %q %v", dir, err)
	}
}
