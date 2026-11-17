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
	// The two secrets the server hands out: an invite to a space, and an
	// API key for tools that can't sign tokens.
	InviteRe = regexp.MustCompile(`^inv_[A-Za-z0-9_-]{43}$`)
	APIKeyRe = regexp.MustCompile(`^cubby_[A-Za-z0-9_-]{43}$`)
	// A device ID is its key's JWK thumbprint: SHA-256, base64url.
	deviceIDRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	inviteIDRe  = regexp.MustCompile(`^i_[a-z2-7]{8}$`)
	pairingIDRe = regexp.MustCompile(`^pc_[a-z2-7]{8}$`)
	apiKeyIDRe  = regexp.MustCompile(`^ak_[a-z2-7]{8}$`)
)

const (
	MinSpace = 16 // shortest space name the API accepts
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

// ValidSpace is the API's rule for space names: 16 to 63 lowercase letters,
// digits and single hyphens, not starting or ending with a hyphen. It checks
// length and characters only; a hand-picked name is as hard to guess as
// whoever picked it made it.
func ValidSpace(name string) bool {
	return len(name) >= MinSpace && len(name) <= MaxSpace && spaceRe.MatchString(name)
}

// ValidLabel checks a label: the same characters as a space name, at most
// MaxLabel long.
func ValidLabel(l string) bool {
	return len(l) <= MaxLabel && LabelRe.MatchString(l)
}

// ValidKey also rules out "." and "..", which browsers, fetch and curl
// collapse as path segments, so such a key could never be reached and would
// send the request somewhere else.
func ValidKey(k string) bool {
	return KeyRe.MatchString(k) && !ActionWords[k] && k != "." && k != ".."
}

// actionWords can follow a key in a path, so they can't be keys themselves.
var ActionWords = map[string]bool{"watch": true, "series": true, "pulse": true}

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
	label = Slug(label)
	if label == "" {
		return RandomWords()
	}
	return label + "-" + RandomWords()
}

// Slug makes a label from any text: lowercase, each run of other characters
// one hyphen, trimmed, at most MaxLabel. "" if nothing is left.
func Slug(s string) string {
	l := cleanLabel(s)
	if len(l) > MaxLabel {
		l = strings.Trim(l[:MaxLabel], "-")
	}
	return l
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

func shortID(prefix string) string {
	b := make([]byte, 5)
	rand.Read(b)
	return prefix + strings.ToLower(base32.StdEncoding.EncodeToString(b))
}

// NewInviteID and NewGrantID name invites and grants within a space.
func NewInviteID() string { return shortID("i_") }
func NewGrantID() string  { return shortID("g_") }

// NewPairingID names a pairing code, NewAPIKeyID an API key. Both are
// server-wide, and their tables' primary keys catch the rare repeat.
func NewPairingID() string { return shortID("pc_") }
func NewAPIKeyID() string  { return shortID("ak_") }

func ValidDeviceID(id string) bool  { return deviceIDRe.MatchString(id) }
func ValidInviteID(id string) bool  { return inviteIDRe.MatchString(id) }
func ValidPairingID(id string) bool { return pairingIDRe.MatchString(id) }
func ValidAPIKeyID(id string) bool  { return apiKeyIDRe.MatchString(id) }

// Pairing codes are 10 characters of Crockford's base32, 50 bits: short enough
// to type, used once, and alive for minutes. The server limits failed
// redeems per IP, which is what makes 50 bits enough.
const pairingAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const PairingCodeLen = 10

// NewPairingCode returns a code in its canonical form, 10 characters.
func NewPairingCode() string {
	var b [8]byte
	rand.Read(b[:])
	var n uint64
	for _, c := range b {
		n = n<<8 | uint64(c)
	}
	out := make([]byte, PairingCodeLen)
	for i := range out {
		out[i] = pairingAlphabet[n&31]
		n >>= 5
	}
	return string(out)
}

// NormalizePairingCode reads a code as people type it: any case, with
// dashes or spaces, O for 0 and I or L for 1. It returns the canonical form,
// or "" if it can't be a code.
func NormalizePairingCode(in string) string {
	s := strings.TrimSpace(in)
	var b strings.Builder
	for _, c := range strings.ToUpper(s) {
		switch {
		case c == '-' || c == ' ':
			continue
		case c == 'O':
			c = '0'
		case c == 'I' || c == 'L':
			c = '1'
		}
		if !strings.ContainsRune(pairingAlphabet, c) {
			return ""
		}
		b.WriteRune(c)
	}
	if b.Len() != PairingCodeLen {
		return ""
	}
	return b.String()
}

// DisplayPairingCode is the code for reading and typing: K7M2X-9Q4PA.
func DisplayPairingCode(c string) string {
	if len(c) != PairingCodeLen {
		return c
	}
	return c[:5] + "-" + c[5:]
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
