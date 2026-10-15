// Command wordlist builds internal/names/words.txt, the 4096 words that
// generated space names are made of. It isn't part of the cubby binary.
//
// Usage: go run ./tools/wordlist SRC_DIR > internal/names/words.txt
//
// SRC_DIR holds these files, downloaded as is:
//
//	eff_large.txt  https://www.eff.org/files/2016/07/18/eff_large_wordlist.txt
//	os_long.txt    https://raw.githubusercontent.com/sts10/orchard-street-wordlists/main/lists/orchard-street-long.txt
//	bip39.txt      https://raw.githubusercontent.com/bitcoin/bips/master/bip-0039/english.txt
//	count_1w.txt   https://norvig.com/ngrams/count_1w.txt (word frequency)
//	ldnoobw.txt    https://raw.githubusercontent.com/LDNOOBW/List-of-Dirty-Naughty-Obscene-and-Otherwise-Bad-Words/master/en
//
// The rules: start from the union of the three curated lists, keep words of
// 3 to 6 letters a-z, drop stopwords (stopwords.txt), bad words, the hand
// list in exclude.txt, and words that are another common word with a prefix
// or suffix added (unsaved, dizziness, used). Of what's left, keep the 4096
// most frequent and print them sorted.
package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	want      = 4096
	vocabSize = 40000 // most frequent words that count as "common" for the affix rule
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./tools/wordlist SRC_DIR > internal/names/words.txt")
		os.Exit(2)
	}
	src := os.Args[1]
	here := "tools/wordlist"

	pool := map[string]bool{}
	for _, f := range []string{"eff_large.txt", "os_long.txt", "bip39.txt"} {
		for _, line := range lines(filepath.Join(src, f)) {
			if fs := strings.Fields(line); len(fs) > 0 {
				pool[strings.ToLower(fs[len(fs)-1])] = true
			}
		}
	}

	rank := map[string]int{}
	for i, line := range lines(filepath.Join(src, "count_1w.txt")) {
		w, _, _ := strings.Cut(line, "\t")
		if _, ok := rank[w]; !ok {
			rank[w] = i
		}
	}
	vocab := func(w string) bool { r, ok := rank[w]; return ok && r < vocabSize }

	drop := map[string]bool{}
	for _, line := range lines(filepath.Join(src, "ldnoobw.txt")) {
		drop[line] = true // whole lines: some entries are phrases of harmless words
	}
	for _, f := range []string{filepath.Join(here, "stopwords.txt"), filepath.Join(here, "exclude.txt")} {
		for _, line := range lines(f) {
			if strings.HasPrefix(line, "#") {
				continue
			}
			for _, w := range strings.Fields(line) {
				drop[w] = true
			}
		}
	}

	shape := regexp.MustCompile(`^[a-z]{3,6}$`)
	var out []string
	for w := range pool {
		if shape.MatchString(w) && !drop[w] && !derived(w, vocab) {
			out = append(out, w)
		}
	}
	freq := func(w string) int {
		if r, ok := rank[w]; ok {
			return r
		}
		return 1 << 30
	}
	slices.SortFunc(out, func(a, b string) int {
		if d := freq(a) - freq(b); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	if len(out) < want {
		fmt.Fprintf(os.Stderr, "only %d words left, need %d\n", len(out), want)
		os.Exit(1)
	}
	out = out[:want]
	slices.Sort(out)
	for _, w := range out {
		fmt.Println(w)
	}
}

var (
	prefixes = []string{"un", "re", "dis", "mis", "non", "pre", "over", "out"}
	suffixes = []string{"s", "es", "ies", "ed", "d", "ied", "ing", "ly", "ness", "able", "ible", "er", "ers", "est", "ful", "less", "ish", "ment", "ion"}
)

// derived reports whether w is a common word with a prefix or suffix added.
func derived(w string, vocab func(string) bool) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(w, p) && len(w)-len(p) >= 3 && vocab(w[len(p):]) {
			return true
		}
	}
	for _, s := range suffixes {
		if !strings.HasSuffix(w, s) {
			continue
		}
		b := w[:len(w)-len(s)]
		if len(b) < 3 {
			continue
		}
		var bases []string
		if s == "ies" || s == "ied" {
			bases = []string{b + "y"}
		} else {
			bases = []string{b, b + "e"}
			if len(b) > 3 && b[len(b)-1] == b[len(b)-2] {
				bases = append(bases, b[:len(b)-1]) // stopped -> stop
			}
		}
		for _, x := range bases {
			if x != w && vocab(x) {
				return true
			}
		}
	}
	return false
}

func lines(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			out = append(out, l)
		}
	}
	return out
}
