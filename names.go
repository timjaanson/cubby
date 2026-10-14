package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"strings"
)

var (
	spaceRe   = regexp.MustCompile(`^([a-z0-9]+(-[a-z0-9]+)*-)?[a-z0-9]{8}$`)
	labelRe   = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	keyRe     = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	patternRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{0,127}\*$`)
	visitorRe = regexp.MustCompile(`^u_[a-z2-7]{16}$`)
	tokenRe   = regexp.MustCompile(`^kv_[A-Za-z0-9_-]{43}$`)
)

const alnum = "abcdefghijklmnopqrstuvwxyz0123456789"

// validSpace checks the space name rule: optional label, 8-char suffix with
// at least 2 digits and 2 letters, at most 63 characters.
func validSpace(name string) bool {
	if len(name) > 63 || !spaceRe.MatchString(name) {
		return false
	}
	suffix := name[len(name)-8:]
	digits, letters := 0, 0
	for _, c := range suffix {
		if c >= '0' && c <= '9' {
			digits++
		} else {
			letters++
		}
	}
	return digits >= 2 && letters >= 2
}

// validKey also rules out "." and "..", which browsers, fetch and curl
// collapse as path segments, so such a key could never be reached and would
// send the request somewhere else.
func validKey(k string) bool {
	return keyRe.MatchString(k) && k != "watch" && k != "." && k != ".."
}
func validPattern(p string) bool { return patternRe.MatchString(p) }
func validVisitor(id string) bool {
	return visitorRe.MatchString(id)
}

// randomSuffix returns 8 chars from a-z0-9 with at least 2 digits and 2 letters.
func randomSuffix() string {
	for {
		b := make([]byte, 8)
		rand.Read(b)
		out := make([]byte, 8)
		for i := range b {
			out[i] = alnum[int(b[i])%len(alnum)]
		}
		s := string(out)
		if validSpace(s) {
			return s
		}
	}
}

// newSpaceName builds a valid name from a label. Invalid label characters are
// dropped so it can also produce a suggestion for a bad name.
func newSpaceName(label string) string {
	label = cleanLabel(label)
	max := 63 - 9
	if len(label) > max {
		label = strings.Trim(label[:max], "-")
	}
	if label == "" {
		return randomSuffix()
	}
	return label + "-" + randomSuffix()
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

// suggestFor makes a valid name from an invalid one, keeping its readable part.
func suggestFor(bad string) string {
	return newSpaceName(bad)
}

func newVisitorID() string {
	b := make([]byte, 10)
	rand.Read(b)
	return "u_" + strings.ToLower(base32.StdEncoding.EncodeToString(b))
}

func randomToken(prefix string) string {
	b := make([]byte, 32)
	rand.Read(b)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func hashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// prefixRange turns a pattern prefix into [lo, hi) for key range scans.
// An empty prefix matches everything.
func prefixRange(prefix string) (string, string) {
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
