// Package names validates space names, keys, patterns and visitor IDs, and
// makes the random names, tokens and hashes the rest of the code uses.
package names

import (
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"strings"
)

var (
	spaceRe   = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	LabelRe   = spaceRe
	KeyRe     = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	patternRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{0,127}\*$`)
	visitorRe = regexp.MustCompile(`^u_[a-z2-7]{16}$`)
	TokenRe   = regexp.MustCompile(`^kv_[A-Za-z0-9_-]{43}$`)
)

const (
	MinSpace = 16 // shortest space name the server accepts
	MaxSpace = 63
	// MaxLabel leaves room for "-" and four words of up to 6 letters.
	MaxLabel = MaxSpace - 1 - (4*6 + 3)
	// NameWords is how many words a generated name has, 12 bits each.
	NameWords = 4
)

//go:embed words.txt
var wordsTxt string

// Words is the 4096-word list generated names are drawn from, built by
// tools/wordlist.
var Words = strings.Fields(wordsTxt)

func init() {
	if len(Words) != 4096 {
		panic("names: words.txt must have 4096 words")
	}
}

// ValidSpace checks the space name rule: 16 to 63 lowercase letters, digits
// and single hyphens, not starting or ending with a hyphen. The length is all
// it checks; a name is as hard to guess as whoever picked it made it.
func ValidSpace(name string) bool {
	return len(name) >= MinSpace && len(name) <= MaxSpace && spaceRe.MatchString(name)
}

// ValidLabel checks a label: the same characters as a space name, at most
// MaxLabel long.
func ValidLabel(l string) bool {
	return len(l) <= MaxLabel && LabelRe.MatchString(l)
}

// validKey also rules out "." and "..", which browsers, fetch and curl
// collapse as path segments, so such a key could never be reached and would
// send the request somewhere else.
func ValidKey(k string) bool {
	return KeyRe.MatchString(k) && !ActionWords[k] && k != "." && k != ".."
}

// actionWords can follow a key in a path, so they can't be keys themselves.
var ActionWords = map[string]bool{"watch": true, "series": true}

func ValidPattern(p string) bool { return patternRe.MatchString(p) }

func ValidVisitor(id string) bool {
	return visitorRe.MatchString(id)
}

// RandomWords returns NameWords words joined by hyphens, 48 random bits.
// 4096 words is exactly 12 bits, so taking 12 bits per word has no bias.
// Four 3-letter words would be 15 characters, one short of a valid name, so
// that rare draw (about 1 in 75,000) is redrawn.
func RandomWords() string {
	for {
		var b [6]byte
		rand.Read(b[:])
		var n uint64
		for _, c := range b {
			n = n<<8 | uint64(c)
		}
		out := make([]string, NameWords)
		for i := range out {
			out[i] = Words[n&0xfff]
			n >>= 12
		}
		if s := strings.Join(out, "-"); len(s) >= MinSpace {
			return s
		}
	}
}

// NewSpaceName builds a valid name from a label: the label, a hyphen and four
// random words. Invalid label characters are dropped so it can also make a
// suggestion from a bad name.
func NewSpaceName(label string) string {
	label = cleanLabel(label)
	if len(label) > MaxLabel {
		label = strings.Trim(label[:MaxLabel], "-")
	}
	if label == "" {
		return RandomWords()
	}
	return label + "-" + RandomWords()
}

func cleanLabel(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	dash := false
	for _, c := range s {
		switch {
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			b.WriteRune(c)
			dash = false
		default:
			if b.Len() > 0 && !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// SuggestFor makes a valid name from an invalid one, keeping its readable part.
func SuggestFor(bad string) string {
	return NewSpaceName(bad)
}

func NewVisitorID() string {
	b := make([]byte, 10)
	rand.Read(b)
	return "u_" + strings.ToLower(base32.StdEncoding.EncodeToString(b))
}

func RandomToken(prefix string) string {
	b := make([]byte, 32)
	rand.Read(b)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func HashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// prefixRange turns a pattern prefix into [lo, hi) for key range scans.
// An empty prefix matches everything.
func PrefixRange(prefix string) (string, string) {
	if prefix == "" {
		return "", "\xff"
	}
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return prefix, string(b[:i+1])
		}
	}
	return prefix, "\xff"
}
