package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

const adminUsage = `tiny-kv admin <command> [--data DIR] [--config FILE]

  spaces            list spaces with size, keys and last activity
  export <space>    dump every key as JSON lines
  delete <space>    delete a space and everything in it
  rekey <space>     lock a space, or replace its key, and print the new key
  stats             totals, visitors and the largest spaces
  cleanup           run the hourly cleanup now

Admin works directly on the data directory. It's safe while the server runs.
`

func cmdAdmin(args []string) int {
	f, err := parseFlags(args, []string{"data", "config"}, nil)
	if err != nil {
		return usageErr(err)
	}
	if len(f.args) == 0 {
		fmt.Print(adminUsage)
		return 64
	}
	cfg, err := loadConfig(firstNonEmpty(f.str("config"), os.Getenv("TINY_KV_CONFIG")))
	if err != nil {
		fmt.Fprintln(os.Stderr, "tiny-kv:", err)
		return 64
	}
	if d := f.str("data"); d != "" {
		cfg.Server.DataDir = d
	}
	if _, err := os.Stat(cfg.Server.DataDir); err != nil {
		fmt.Fprintf(os.Stderr, "tiny-kv: no data in %s. Pass --data or set TINY_KV_DATA.\n", cfg.Server.DataDir)
		return 1
	}
	store, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tiny-kv:", err)
		return 6
	}
	defer store.Close()

	cmd, rest := f.args[0], f.args[1:]
	needSpace := func() (string, bool) {
		if len(rest) != 1 {
			fmt.Fprintf(os.Stderr, "tiny-kv: admin %s takes one space name\n", cmd)
			return "", false
		}
		return rest[0], true
	}
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "tiny-kv:", err)
		if strings.HasPrefix(err.Error(), "no space") {
			return 1
		}
		return 6
	}

	switch cmd {
	case "spaces":
		rows, err := store.spaces()
		if err != nil {
			return fail(err)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "SPACE\tKIND\tSIZE\tKEYS\tLAST ACTIVE")
		for _, r := range rows {
			kind := "open"
			if r.Locked {
				kind = "locked"
			}
			if cfg.pinned(r.Name) {
				kind += ",pinned"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", r.Name, kind, humanBytes(r.Bytes), r.Keys, time.Unix(r.LastActive, 0).Format("2006-01-02 15:04"))
		}
		tw.Flush()
	case "export":
		name, ok := needSpace()
		if !ok {
			return 64
		}
		rows, err := store.db.Query(`SELECT area, key, value, is_json, version, writer FROM kv WHERE space = ? AND value IS NOT NULL ORDER BY area, key`, name)
		if err != nil {
			return fail(err)
		}
		defer rows.Close()
		enc := json.NewEncoder(os.Stdout)
		for rows.Next() {
			var area, key, writer string
			var val []byte
			var isJSON bool
			var ver int64
			rows.Scan(&area, &key, &val, &isJSON, &ver, &writer)
			path := "shared"
			if strings.HasPrefix(area, "u:") {
				path = "users/" + area[2:]
			} else if strings.HasPrefix(area, "m:") {
				path = "me:" + area[2:]
			}
			e := toItem(Entry{Key: key, Value: val, IsJSON: isJSON, Version: ver})
			enc.Encode(map[string]any{"area": path, "key": e.Key, "value": e.Value, "version": ver, "writer": writer})
		}
	case "delete":
		name, ok := needSpace()
		if !ok {
			return 64
		}
		if err := store.deleteSpace(name); err != nil {
			return fail(err)
		}
		fmt.Println("deleted", name)
	case "rekey":
		name, ok := needSpace()
		if !ok {
			return 64
		}
		key, err := store.rekey(name)
		if err != nil {
			return fail(err)
		}
		fmt.Println(key)
	case "stats":
		var spaces, keys int
		var bytes int64
		var visitors, storing int
		store.db.QueryRow(`SELECT count(*), coalesce(sum(keys), 0), coalesce(sum(bytes), 0) FROM spaces`).Scan(&spaces, &keys, &bytes)
		store.db.QueryRow(`SELECT count(*), coalesce(sum(stored), 0) FROM visitors`).Scan(&visitors, &storing)
		fmt.Printf("data       %s\nspaces     %d of %d\nkeys       %d\nstored     %s of %s\nvisitors   %d (%d have stored something)\n",
			cfg.Server.DataDir, spaces, cfg.Limits.MaxSpaces, keys, humanBytes(bytes), humanBytes(int64(cfg.Limits.MaxTotalBytes)), visitors, storing)
		rows, _ := store.db.Query(`SELECT name, bytes, keys FROM spaces ORDER BY bytes DESC LIMIT 5`)
		if rows != nil {
			first := true
			for rows.Next() {
				var n string
				var b int64
				var k int
				rows.Scan(&n, &b, &k)
				if first {
					fmt.Println("largest:")
					first = false
				}
				fmt.Printf("  %-40s %8s %6d keys\n", n, humanBytes(b), k)
			}
			rows.Close()
		}
	case "cleanup":
		rep, err := store.cleanup()
		if err != nil {
			return fail(err)
		}
		fmt.Printf("removed %d visitors, %d tombstones, %d spaces\n", rep.Visitors, rep.Tombstones, rep.Spaces)
	default:
		fmt.Fprintf(os.Stderr, "tiny-kv: unknown admin command %q\n\n%s", cmd, adminUsage)
		return 64
	}
	return 0
}

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
