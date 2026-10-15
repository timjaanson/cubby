package names

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestValidSpace(t *testing.T) {
	for name, want := range map[string]bool{
		"cedar-pilot-mango-frost":  true,
		"poll-cedar-pilot-mango":   true,
		"abcdefghijklmnop":         true, // 16, any content
		"my-team-dashboard":        true,
		strings.Repeat("a", 63):    true,
		"abcdefghijklmno":          false, // 15
		strings.Repeat("a", 64):    false,
		"poll-k7m2x9q4":            false, // old format, too short now
		"Cedar-pilot-mango-frost":  false,
		"cedar--pilot-mango-frost": false,
		"-cedar-pilot-mango-frost": false,
		"cedar-pilot-mango-frost-": false,
		"cedar_pilot_mango_frost":  false,
		"cedar.pilot.mango.frost":  false,
		"":                         false,
	} {
		if ValidSpace(name) != want {
			t.Errorf("ValidSpace(%q) = %v", name, !want)
		}
	}
}

func TestWordList(t *testing.T) {
	if len(Words) != 4096 {
		t.Fatalf("%d words", len(Words))
	}
	re := regexp.MustCompile(`^[a-z]{3,6}$`)
	for i, w := range Words {
		if !re.MatchString(w) {
			t.Errorf("bad word %q", w)
		}
		if i > 0 && Words[i-1] >= w {
			t.Errorf("not sorted or duplicate at %q", w)
		}
	}
}

func TestRandomWords(t *testing.T) {
	const n = 50_000
	counts := make(map[string]int, len(Words))
	seen := map[string]bool{}
	for range n {
		s := RandomWords()
		ws := strings.Split(s, "-")
		if len(ws) != NameWords || !ValidSpace(s) {
			t.Fatalf("bad name %q", s)
		}
		for _, w := range ws {
			if _, ok := slices.BinarySearch(Words, w); !ok {
				t.Fatalf("%q not in the list", w)
			}
			counts[w]++
		}
		if seen[s] {
			t.Fatalf("repeat %q after %d names", s, len(seen))
		}
		seen[s] = true
	}
	// 200,000 draws over 4096 words: about 49 each, standard deviation 7.
	// Every word appears and none is far off; a biased or stuck generator fails.
	for _, w := range Words {
		if c := counts[w]; c < 15 || c > 95 {
			t.Errorf("word %q drawn %d times, want about 49", w, c)
		}
	}
}

func TestShortestDrawIsRedrawn(t *testing.T) {
	var short []string
	for _, w := range Words {
		if len(w) == 3 {
			short = append(short, w)
		}
	}
	if s := strings.Join(short[:4], "-"); len(s) >= MinSpace {
		t.Fatalf("four 3-letter words make %q, %d characters; the redraw rule is stale", s, len(s))
	}
	// 4 words of at least 3 letters plus a 4-letter one is always enough.
	if len(short[0]+short[1]+short[2])+4+3 < MinSpace {
		t.Fatal("names with one longer word can be too short")
	}
}

func TestNewSpaceName(t *testing.T) {
	for range 200 {
		n := NewSpaceName("My Poll!")
		if !ValidSpace(n) || !strings.HasPrefix(n, "my-poll-") || strings.Count(n, "-") != 1+NameWords {
			t.Fatalf("bad name %q", n)
		}
	}
	long := strings.Repeat("abcdefghi-", 10)
	for range 200 {
		n := NewSpaceName(long)
		if !ValidSpace(n) || len(n) > MaxSpace {
			t.Fatalf("long label gave %q (%d)", n, len(n))
		}
	}
	if n := NewSpaceName(""); !ValidSpace(n) || strings.Count(n, "-") != NameWords-1 {
		t.Fatalf("no label gave %q", n)
	}
	if n := NewSpaceName("!!!"); strings.Count(n, "-") != NameWords-1 {
		t.Fatalf("junk label gave %q", n)
	}
	if !ValidLabel(strings.Repeat("a", MaxLabel)) || ValidLabel(strings.Repeat("a", MaxLabel+1)) {
		t.Fatal("label length limit")
	}
	if n := SuggestFor("POLL--k7m2"); !ValidSpace(n) || !strings.HasPrefix(n, "poll-k7m2-") {
		t.Fatalf("suggestion %q", n)
	}
}

func TestSafeSpaceFile(t *testing.T) {
	for name, want := range map[string]bool{
		"a":                       true,
		"poll-k7m2x9q4":           true, // fails ValidSpace, still file-safe
		"cedar-pilot-mango-frost": true,
		strings.Repeat("a", 63):   true,
		strings.Repeat("a", 64):   false,
		"":                        false,
		"..":                      false,
		"a/b":                     false,
		"a.db":                    false,
		"Poll":                    false,
		"-a":                      false,
		"a--b":                    false,
	} {
		if SafeSpaceFile(name) != want {
			t.Errorf("SafeSpaceFile(%q) = %v", name, !want)
		}
	}
}
