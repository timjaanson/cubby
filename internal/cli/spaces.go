package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
)

// spaces prints every space this visitor belongs to, most recently active
// first, following the pages up to --limit.
func (c *client) spaces(f *flags) error {
	if len(f.args) != 0 {
		return usageCli("spaces takes no arguments")
	}
	limit, err := limitFlag(f)
	if err != nil {
		return err
	}
	q := url.Values{}
	printed := 0
	for {
		if limit > 0 {
			q.Set("limit", strconv.Itoa(min(1000, limit-printed)))
		}
		path := "/v1/spaces"
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		var out struct {
			Items []json.RawMessage
			Next  *string
		}
		if err := c.call("GET", path, nil, 200, &out); err != nil {
			return err
		}
		for _, raw := range out.Items {
			printed++
			if f.bool("json") {
				fmt.Println(string(raw))
				continue
			}
			var it struct {
				Space, Access string
				LastActive    int64
				Members       int
			}
			json.Unmarshal(raw, &it)
			fmt.Printf("%s\t%s\t%s\t%d\n", it.Space, it.Access, fmtTime(&it.LastActive), it.Members)
		}
		if out.Next == nil || limit > 0 && printed >= limit {
			return nil
		}
		q.Set("after", *out.Next)
	}
}

// batch posts a JSON array of writes, all or nothing.
func (c *client) batch(f *flags) error {
	if len(f.args) < 1 || len(f.args) > 2 {
		return usageCli("batch takes a space and a file, or - or nothing for stdin")
	}
	space, err := spaceArg(f.args[0])
	if err != nil {
		return err
	}
	var body []byte
	if len(f.args) == 1 || f.args[1] == "-" {
		body, err = io.ReadAll(os.Stdin)
	} else {
		body, err = os.ReadFile(f.args[1])
	}
	if err != nil {
		return &cliError{code: 5, err: "bad_input", msg: err.Error()}
	}
	resp, rb, err := c.do("POST", "/v1/"+space+"/batch", body, reqOpts{needToken: true})
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		e := apiErrFrom(resp, rb)
		if i, ok := e.body["index"].(float64); ok {
			e.msg += fmt.Sprintf(" (write %d; nothing was written)", int(i))
		}
		return e
	}
	var out struct {
		Items []struct {
			Path    string
			Version int64
		}
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return &cliError{code: 6, err: "bad_response", msg: err.Error()}
	}
	for _, it := range out.Items {
		fmt.Printf("%s\t%d\n", it.Path, it.Version)
	}
	return nil
}
