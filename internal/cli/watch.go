package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func (c *client) watch(f *flags) error {
	p, err := c.onePath(f, "watch")
	if err != nil {
		return err
	}
	if f.bool("raw") && (p.isPattern() || p.area == "users/*") {
		return usageCli("--raw is for watching one key")
	}
	fromNow := f.str("from") == "now"
	var from int64
	haveFrom := !fromNow
	if f.has("from") && !fromNow {
		if from, err = strconv.ParseInt(f.str("from"), 10, 64); err != nil || from < 0 {
			return usageCli("--from takes a version or now")
		}
	}
	state := f.str("state")
	if state != "" {
		if b, err := os.ReadFile(state); err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
				from, haveFrom = n, true
			}
		}
	}

	known := map[string]bool{} // keys currently known to exist, for re-syncs
	resync := false
	var resyncSeen map[string]bool
	backoff := time.Second
	needToken := p.area == "me"

	for {
		u := p.url() + "/watch"
		if haveFrom {
			u += "?version=" + strconv.FormatInt(from, 10)
		}
		resp, b, err := c.do("GET", u, nil, reqOpts{needToken: needToken})
		if err != nil {
			fmt.Fprintf(os.Stderr, "cubby: %v, retrying in %s\n", err, backoff)
			time.Sleep(backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		switch {
		case resp.StatusCode == 200:
			backoff = time.Second
		case resp.StatusCode == 410:
			if fromNow {
				fmt.Fprintln(os.Stderr, "cubby: version expired, continuing from now")
				haveFrom = false
			} else {
				fmt.Fprintln(os.Stderr, "cubby: version expired, reloading current state")
				from, haveFrom, resync, resyncSeen = 0, true, true, map[string]bool{}
			}
			continue
		case resp.StatusCode == 429:
			var e struct{ RetryAfter int }
			json.Unmarshal(b, &e)
			time.Sleep(time.Duration(max(e.RetryAfter, 1)) * time.Second)
			continue
		case resp.StatusCode >= 500:
			fmt.Fprintf(os.Stderr, "cubby: server error %d, retrying in %s\n", resp.StatusCode, backoff)
			time.Sleep(backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		default:
			return apiErrFrom(resp, b)
		}

		var out struct {
			Changes []item
			Version int64
			More    bool
		}
		if err := json.Unmarshal(b, &out); err != nil {
			return &cliError{code: 6, err: "bad_response", msg: err.Error()}
		}
		changes := out.Changes
		if resync {
			for _, ch := range changes {
				resyncSeen[ch.User+"/"+ch.Key] = true
			}
			if !out.More {
				for k := range known {
					if !resyncSeen[k] {
						u, key, _ := strings.Cut(k, "/")
						changes = append(changes, item{Key: key, User: u, Deleted: true, Version: out.Version})
					}
				}
				resync = false
			}
		}
		for _, ch := range changes {
			id := ch.User + "/" + ch.Key
			if ch.Deleted {
				delete(known, id)
			} else {
				known[id] = true
			}
			c.emit(ch, f)
		}
		from, haveFrom = out.Version, true
		if state != "" {
			if err := writeState(state, from); err != nil {
				fmt.Fprintln(os.Stderr, "cubby: warning: could not save state:", err)
			}
		}
		if f.bool("once") && len(changes) > 0 && !out.More {
			return nil
		}
	}
}

func writeState(path string, v int64) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(v, 10)+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// rawValue turns a list/watch value back into the stored bytes: JSON as is,
// text unquoted.
func rawValue(v json.RawMessage) []byte {
	var s string
	if len(v) > 0 && v[0] == '"' && json.Unmarshal(v, &s) == nil {
		return []byte(s)
	}
	return v
}

func (c *client) emit(ch item, f *flags) {
	if cmd := f.str("exec"); cmd != "" {
		e := exec.Command("sh", "-c", cmd)
		if !ch.Deleted {
			e.Stdin = bytes.NewReader(rawValue(ch.Value))
		}
		e.Stdout, e.Stderr = os.Stdout, os.Stderr
		del := "0"
		if ch.Deleted {
			del = "1"
		}
		e.Env = append(os.Environ(), "KV_KEY="+ch.Key, "KV_VERSION="+strconv.FormatInt(ch.Version, 10), "KV_DELETED="+del)
		if ch.User != "" {
			e.Env = append(e.Env, "KV_USER="+ch.User)
		}
		if ch.T != 0 {
			e.Env = append(e.Env, "KV_TIME="+strconv.FormatInt(ch.T, 10))
		}
		if err := e.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "cubby: warning: command for %s failed: %v\n", ch.Key, err)
		}
		return
	}
	if f.bool("raw") {
		if !ch.Deleted {
			os.Stdout.Write(append(rawValue(ch.Value), '\n'))
		}
		return
	}
	printItem(ch, f.bool("json"))
}
