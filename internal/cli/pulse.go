package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// pulse sends one pulse: a value that isn't stored, for whoever is listening now.
func (c *client) pulse(f *flags) error {
	if len(f.args) < 1 || len(f.args) > 2 {
		return usageCli("pulse takes a path and an optional value")
	}
	p, err := c.parsePath(f.args[0])
	if err != nil {
		return err
	}
	if p.isPattern() || p.area == "users/*" {
		return usageCli("a pulse goes to one key; listen with a pattern using cubby pulses")
	}
	var value []byte
	if len(f.args) == 2 && f.args[1] != "-" {
		value = []byte(f.args[1])
	} else if value, err = io.ReadAll(os.Stdin); err != nil {
		return err
	}
	resp, b, err := c.do("POST", p.url()+"/pulse", value, reqOpts{needToken: true})
	if err != nil {
		return err
	}
	if resp.StatusCode != 202 {
		return apiErrFrom(resp, b)
	}
	return nil
}

type pulseItem struct {
	Key   string          `json:"key"`
	User  string          `json:"user,omitempty"`
	By    string          `json:"by"`
	Value json.RawMessage `json:"value"`
	T     int64           `json:"t"`
}

// pulses prints pulses as they arrive, from now on, until interrupted.
func (c *client) pulses(f *flags) error {
	p, err := c.onePath(f, "pulses")
	if err != nil {
		return err
	}
	backoff := time.Second
	after := ""
	for {
		u := p.url() + "/pulse"
		if after != "" {
			u += "?after=" + after
		}
		resp, b, err := c.do("GET", u, nil, reqOpts{needToken: p.area == "me"})
		if err != nil {
			fmt.Fprintf(os.Stderr, "cubby: %v, retrying in %s\n", err, backoff)
			time.Sleep(backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		switch {
		case resp.StatusCode == 200:
			backoff = time.Second
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
			Pulses []pulseItem
			Seq    int64
			Missed bool
		}
		if err := json.Unmarshal(b, &out); err != nil {
			return &cliError{code: 6, err: "bad_response", msg: err.Error()}
		}
		if out.Missed {
			fmt.Fprintln(os.Stderr, "cubby: some pulses were missed")
		}
		for _, pl := range out.Pulses {
			emitPulse(pl, f)
		}
		after = strconv.FormatInt(out.Seq, 10)
	}
}

func emitPulse(pl pulseItem, f *flags) {
	if cmd := f.str("exec"); cmd != "" {
		e := exec.Command("sh", "-c", cmd)
		e.Stdin = bytes.NewReader(rawValue(pl.Value))
		e.Stdout, e.Stderr = os.Stdout, os.Stderr
		e.Env = append(os.Environ(), "KV_KEY="+pl.Key, "KV_BY="+pl.By, "KV_TIME="+strconv.FormatInt(pl.T, 10))
		if pl.User != "" {
			e.Env = append(e.Env, "KV_USER="+pl.User)
		}
		if err := e.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "cubby: warning: command for %s failed: %v\n", pl.Key, err)
		}
		return
	}
	if f.bool("json") {
		b, _ := json.Marshal(pl)
		fmt.Println(string(b))
		return
	}
	if pl.User != "" {
		fmt.Printf("%s\t%s\t%s\n", pl.User, pl.Key, compactJSON(pl.Value))
	} else {
		fmt.Printf("%s\t%s\n", pl.Key, compactJSON(pl.Value))
	}
}
