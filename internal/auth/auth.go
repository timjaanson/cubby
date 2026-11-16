// Package auth reads device keys and the tokens they sign. A device key is
// a public JWK (RFC 7517), ES256 (P-256) or EdDSA (Ed25519). Its ID is the
// JWK thumbprint (RFC 7638). A token is a compact JWT (RFC 7519) the device
// signed itself:
//
//	header  {"alg": "ES256", "typ": "JWT", "kid": "<device id>"}
//	payload {"aud": "https://cubby.example.com", "iat": 1794787200, "exp": 1794790800}
//
// It knows nothing about HTTP or storage.
package auth

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"time"
)

const (
	// MaxLifetime is the longest a token may last: exp - iat.
	MaxLifetime = time.Hour
	// Skew is how far a client clock may be off.
	Skew = 5 * time.Minute
	// MaxTokenLen bounds what is parsed at all.
	MaxTokenLen = 4096
)

var b64 = base64.RawURLEncoding

// Key is a device's public key.
type Key struct {
	Alg string // "ES256" or "EdDSA"
	ID  string // the JWK thumbprint
	JWK string // the canonical public JWK, as stored
	ec  *ecdsa.PublicKey
	ed  ed25519.PublicKey
}

// Error is why a key or token was refused. Code is the API error code.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func fail(code, msg string) *Error { return &Error{Code: code, Message: msg} }

type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	D   string `json:"d"`
}

// ParseKey reads a public JWK. A key with its private part is refused, so a
// client that sends the wrong half finds out at once.
func ParseKey(raw []byte) (*Key, error) {
	var j jwk
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, fail("bad_key", "The key must be a JWK object, like {\"kty\":\"EC\",\"crv\":\"P-256\",\"x\":\"...\",\"y\":\"...\"}.")
	}
	if j.D != "" {
		return nil, fail("bad_key", "This JWK has its private part (\"d\"). Send only the public key; the private key never leaves the device.")
	}
	x, errX := b64.DecodeString(j.X)
	switch {
	case j.Kty == "EC" && j.Crv == "P-256":
		y, errY := b64.DecodeString(j.Y)
		if errX != nil || errY != nil || len(x) != 32 || len(y) != 32 {
			return nil, fail("bad_key", "A P-256 key has x and y of 32 bytes each, base64url without padding.")
		}
		point := append(append([]byte{4}, x...), y...)
		if _, err := ecdh.P256().NewPublicKey(point); err != nil {
			return nil, fail("bad_key", "This P-256 key is not a point on the curve.")
		}
		canon := `{"crv":"P-256","kty":"EC","x":"` + j.X + `","y":"` + j.Y + `"}`
		pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		return &Key{Alg: "ES256", ID: thumbprint(canon), JWK: canon, ec: pub}, nil
	case j.Kty == "OKP" && j.Crv == "Ed25519":
		if errX != nil || len(x) != ed25519.PublicKeySize {
			return nil, fail("bad_key", "An Ed25519 key has x of 32 bytes, base64url without padding.")
		}
		canon := `{"crv":"Ed25519","kty":"OKP","x":"` + j.X + `"}`
		return &Key{Alg: "EdDSA", ID: thumbprint(canon), JWK: canon, ed: ed25519.PublicKey(x)}, nil
	}
	return nil, fail("bad_key", "Keys are ES256 (kty EC, crv P-256) or EdDSA (kty OKP, crv Ed25519).")
}

// thumbprint is RFC 7638: SHA-256 of the canonical JWK, base64url.
func thumbprint(canon string) string {
	h := sha256.Sum256([]byte(canon))
	return b64.EncodeToString(h[:])
}

// Token is a parsed, not yet verified, JWT.
type Token struct {
	Alg string
	Kid string
	// JWK is the public key in the header, only when the token introduces
	// a new key.
	JWK      json.RawMessage
	Aud      []string
	IssuedAt time.Time
	Expires  time.Time
	signed   []byte
	sig      []byte
}

// LooksLikeJWT says whether s has the shape of a compact JWS.
func LooksLikeJWT(s string) bool {
	return len(s) <= MaxTokenLen && strings.Count(s, ".") == 2 && strings.HasPrefix(s, "eyJ")
}

// Parse reads a compact JWT without checking anything but its form.
func Parse(s string) (*Token, error) {
	bad := fail("bad_token", "This is not a cubby token. A token is a JWT: header.payload.signature, base64url.")
	if len(s) > MaxTokenLen {
		return nil, bad
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return nil, bad
	}
	hb, err1 := b64.DecodeString(parts[0])
	pb, err2 := b64.DecodeString(parts[1])
	sig, err3 := b64.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, fail("bad_token", "Each part of a token is base64url without padding.")
	}
	var h struct {
		Alg  string          `json:"alg"`
		Kid  string          `json:"kid"`
		JWK  json.RawMessage `json:"jwk"`
		Crit json.RawMessage `json:"crit"`
	}
	if err := json.Unmarshal(hb, &h); err != nil {
		return nil, fail("bad_token", "The token header is not a JSON object.")
	}
	if h.Crit != nil {
		return nil, fail("bad_token", "The token header has crit, which cubby doesn't support.")
	}
	var p struct {
		Aud json.RawMessage `json:"aud"`
		Iat json.Number     `json:"iat"`
		Exp json.Number     `json:"exp"`
	}
	dec := json.NewDecoder(bytes.NewReader(pb))
	dec.UseNumber()
	if err := dec.Decode(&p); err != nil {
		return nil, fail("bad_token", "The token payload is not a JSON object.")
	}
	t := &Token{Alg: h.Alg, Kid: h.Kid, signed: []byte(parts[0] + "." + parts[1]), sig: sig}
	if len(h.JWK) > 0 && string(h.JWK) != "null" {
		t.JWK = h.JWK
	}
	var one string
	if json.Unmarshal(p.Aud, &one) == nil {
		t.Aud = []string{one}
	} else if json.Unmarshal(p.Aud, &t.Aud) != nil {
		t.Aud = nil
	}
	iat, errI := p.Iat.Int64()
	exp, errE := p.Exp.Int64()
	if errI != nil || errE != nil {
		return nil, fail("bad_token", "The token needs iat and exp, whole seconds since 1970.")
	}
	t.IssuedAt, t.Expires = time.Unix(iat, 0), time.Unix(exp, 0)
	return t, nil
}

// Verify checks the token's signature with key, and its claims against the
// server's origin and the time. The key decides the algorithm: the header
// must name the same one, and is never used to pick it.
func (t *Token) Verify(k *Key, origin string, now time.Time) error {
	if t.Alg != k.Alg {
		return fail("bad_signature", "This key signs with "+k.Alg+"; the token header says alg "+quote(t.Alg)+".")
	}
	digest := sha256.Sum256(t.signed)
	ok := false
	switch k.Alg {
	case "ES256":
		// JOSE signatures are r||s, 32 bytes each, not DER.
		if len(t.sig) == 64 {
			r, s := new(big.Int).SetBytes(t.sig[:32]), new(big.Int).SetBytes(t.sig[32:])
			ok = ecdsa.Verify(k.ec, digest[:], r, s)
		}
	case "EdDSA":
		ok = ed25519.Verify(k.ed, t.signed, t.sig)
	}
	if !ok {
		msg := "The signature doesn't match this device's key."
		if k.Alg == "ES256" && len(t.sig) > 64 && t.sig[0] == 0x30 {
			msg = "An ES256 signature is 64 bytes, r then s. DER signatures (as openssl makes) must be converted."
		}
		return fail("bad_signature", msg)
	}
	return t.checkClaims(origin, now)
}

func (t *Token) checkClaims(origin string, now time.Time) error {
	if !t.forOrigin(origin) {
		return fail("wrong_audience", "This token is for "+quote(strings.Join(t.Aud, ", "))+". Its aud must be this server's origin, "+origin+".")
	}
	if t.Expires.Sub(t.IssuedAt) > MaxLifetime {
		return fail("token_too_long", "A token lasts at most an hour: exp - iat <= 3600.")
	}
	if !t.Expires.After(t.IssuedAt) {
		return fail("bad_token", "The token's exp must come after its iat.")
	}
	if t.IssuedAt.After(now.Add(Skew)) {
		return fail("token_not_yet_valid", "This token was issued in the future. Check the device clock, or take the time from GET /v1/time.")
	}
	if !now.Before(t.Expires.Add(Skew)) {
		return fail("token_expired", "This token expired. Sign a new one.")
	}
	return nil
}

func (t *Token) forOrigin(origin string) bool {
	for _, a := range t.Aud {
		if strings.TrimSuffix(a, "/") == origin {
			return true
		}
	}
	return false
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }
