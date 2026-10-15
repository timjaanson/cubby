package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

func (c *client) update(f *flags) error {
	p, err := c.onePath(f, "update")
	if err != nil {
		return err
	}
	if p.isPattern() || p.area == "users/*" {
		return usageCli("update works on one key")
	}
	cmd := f.str("exec")
	for attempt := 0; attempt < 10; attempt++ {
		cur, _, ver, err := c.fetch(p)
		var ce *cliError
		if errors.As(err, &ce) && ce.err == "not_found" {
			cur, ver, err = nil, 0, nil
		}
		if err != nil {
			return err
		}
		var next []byte
		if cmd != "" {
			in := cur
			if in == nil {
				in = []byte("null")
			}
			e := exec.Command("sh", "-c", cmd)
			e.Stdin, e.Stderr = bytes.NewReader(in), os.Stderr
			out, err := e.Output()
			if err != nil {
				return &cliError{code: 5, err: "exec_failed", msg: fmt.Sprintf("%s: %v", cmd, err)}
			}
			next = bytes.TrimRight(out, "\n")
		} else {
			if next, err = editValue(cur); err != nil {
				return err
			}
		}
		if len(next) == 0 || bytes.Equal(next, cur) {
			if ver != 0 {
				fmt.Println(ver)
			}
			return nil
		}
		v, _, err := c.put(p, next, &ver)
		if errors.As(err, &ce) && ce.err == "version_conflict" {
			if cmd == "" {
				fmt.Fprintln(os.Stderr, "cubby: someone else changed it meanwhile, reopening with their version")
			}
			time.Sleep(time.Duration(50+attempt*20) * time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}
		fmt.Println(v)
		return nil
	}
	return &cliError{code: 2, err: "version_conflict", msg: "gave up after 10 conflicting writes"}
}

func editValue(cur []byte) ([]byte, error) {
	editor := firstNonEmpty(os.Getenv("VISUAL"), os.Getenv("EDITOR"), "vi")
	tmp, err := os.CreateTemp("", "cubby-*.txt")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	tmp.Write(cur)
	tmp.Close()
	e := exec.Command("sh", "-c", editor+` "$1"`, "sh", tmp.Name())
	e.Stdin, e.Stdout, e.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := e.Run(); err != nil {
		return nil, &cliError{code: 5, err: "editor_failed", msg: err.Error()}
	}
	b, err := os.ReadFile(tmp.Name())
	if err != nil {
		return nil, err
	}
	if !bytes.HasSuffix(cur, []byte("\n")) {
		b = bytes.TrimSuffix(b, []byte("\n"))
	}
	return b, nil
}
