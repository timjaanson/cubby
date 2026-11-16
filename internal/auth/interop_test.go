package auth

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestInterop verifies tokens minted by other libraries and tools: npm jose,
// Python PyJWT and an openssl shell script. tools/interop/gen.sh makes them.
func TestInterop(t *testing.T) {
	f, err := os.Open("testdata/interop.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	now := time.Unix(1794787200+60, 0)
	n := 0
	for sc := bufio.NewScanner(f); sc.Scan(); n++ {
		var c struct {
			Tool, Alg, Token string
			JWK              json.RawMessage
		}
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		name := c.Tool + " " + c.Alg
		key, err := ParseKey(c.JWK)
		if err != nil {
			t.Errorf("%s: key: %v", name, err)
			continue
		}
		tok, err := Parse(c.Token)
		if err != nil {
			t.Errorf("%s: parse: %v", name, err)
			continue
		}
		if tok.Kid != key.ID || key.Alg != c.Alg {
			t.Errorf("%s: kid %s, thumbprint %s, alg %s", name, tok.Kid, key.ID, key.Alg)
		}
		if err := tok.Verify(key, origin, now); err != nil {
			t.Errorf("%s: verify: %v", name, err)
		}
		if err := tok.Verify(key, origin, now.Add(2*time.Hour)); err == nil {
			t.Errorf("%s: accepted after expiry", name)
		}
	}
	if n < 5 {
		t.Errorf("only %d interop tokens", n)
	}
}
