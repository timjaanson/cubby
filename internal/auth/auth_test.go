package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const origin = "https://cubby.example.com"

// sign makes a token by hand, so tests can break any part of it.
func sign(t *testing.T, s *Signer, header, payload map[string]any) string {
	t.Helper()
	hb, _ := json.Marshal(header)
	pb, _ := json.Marshal(payload)
	in := b64.EncodeToString(hb) + "." + b64.EncodeToString(pb)
	return in + "." + b64.EncodeToString(s.sign([]byte(in)))
}

func check(tok string, k *Key, now time.Time) error {
	t, err := Parse(tok)
	if err != nil {
		return err
	}
	return t.Verify(k, origin, now)
}

func code(err error) string {
	if err == nil {
		return ""
	}
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return err.Error()
}

func TestRoundTrip(t *testing.T) {
	for _, mk := range []func() (*Signer, error){NewSigner, NewEdSigner} {
		s, err := mk()
		if err != nil {
			t.Fatal(err)
		}
		tok := s.Token(origin, time.Hour, false)
		if !LooksLikeJWT(tok) {
			t.Fatalf("%s: not JWT-shaped: %s", s.Key.Alg, tok)
		}
		if err := check(tok, s.Key, time.Now()); err != nil {
			t.Fatalf("%s: %v", s.Key.Alg, err)
		}
		// The private JWK reads back as the same key.
		back, err := SignerFromJWK(s.PrivateJWK())
		if err != nil || back.Key.ID != s.Key.ID {
			t.Fatalf("%s: private JWK round trip: %v", s.Key.Alg, err)
		}
		if err := check(back.Token(origin, time.Minute, true), s.Key, time.Now()); err != nil {
			t.Fatalf("%s: token from reloaded key: %v", s.Key.Alg, err)
		}
	}
}

// RFC 7638 section 3.1 gives this thumbprint for its example RSA key; for
// EC and OKP the same rule applies. RFC 8037 appendix A.3 has an Ed25519 one.
func TestThumbprintRFC8037(t *testing.T) {
	k, err := ParseKey([]byte(`{"kty":"OKP","crv":"Ed25519","x":"11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo"}`))
	if err != nil {
		t.Fatal(err)
	}
	if k.ID != "kPrK_qmxVWaYVA9wwBF6Iuo3vVzz7TxHCTwXBygrS4k" {
		t.Fatalf("thumbprint %s", k.ID)
	}
}

func TestRejections(t *testing.T) {
	s, _ := NewSigner()
	ed, _ := NewEdSigner()
	now := time.Now()
	hdr := func() map[string]any { return map[string]any{"alg": "ES256", "typ": "JWT", "kid": s.Key.ID} }
	pay := func() map[string]any {
		return map[string]any{"aud": origin, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	}
	with := func(m map[string]any, k string, v any) map[string]any { m[k] = v; return m }

	// HS256 signed with the public key as the HMAC secret: the classic
	// algorithm confusion attack.
	hb, _ := json.Marshal(with(hdr(), "alg", "HS256"))
	pb, _ := json.Marshal(pay())
	in := b64.EncodeToString(hb) + "." + b64.EncodeToString(pb)
	mac := hmac.New(sha256.New, []byte(s.Key.JWK))
	mac.Write([]byte(in))
	hs256 := in + "." + b64.EncodeToString(mac.Sum(nil))

	good := sign(t, s, hdr(), pay())
	parts := strings.Split(good, ".")
	tampered := parts[0] + "." + b64.EncodeToString([]byte(`{"aud":"`+origin+`","iat":1,"exp":99999999999}`)) + "." + parts[2]

	cases := []struct {
		name, tok, want string
		key             *Key
	}{
		{"good", good, "", s.Key},
		{"alg none", strings.Join([]string{b64.EncodeToString([]byte(`{"alg":"none","kid":"x"}`)), parts[1], ""}, "."), "bad_signature", s.Key},
		{"HS256 with the public key", hs256, "bad_signature", s.Key},
		{"EdDSA header on an ES256 key", sign(t, s, with(hdr(), "alg", "EdDSA"), pay()), "bad_signature", s.Key},
		{"other key", sign(t, ed, map[string]any{"alg": "EdDSA", "kid": s.Key.ID}, pay()), "bad_signature", s.Key},
		{"tampered payload", tampered, "bad_signature", s.Key},
		{"expired", sign(t, s, hdr(), with(with(pay(), "iat", now.Add(-2*time.Hour).Unix()), "exp", now.Add(-time.Hour).Unix())), "token_expired", s.Key},
		{"expired within skew", sign(t, s, hdr(), with(with(pay(), "iat", now.Add(-time.Hour).Unix()), "exp", now.Add(-time.Minute).Unix())), "", s.Key},
		{"issued in the future", sign(t, s, hdr(), with(with(pay(), "iat", now.Add(10*time.Minute).Unix()), "exp", now.Add(20*time.Minute).Unix())), "token_not_yet_valid", s.Key},
		{"too long", sign(t, s, hdr(), with(pay(), "exp", now.Add(2*time.Hour).Unix())), "token_too_long", s.Key},
		{"exp before iat", sign(t, s, hdr(), with(pay(), "exp", now.Add(-time.Second).Unix())), "bad_token", s.Key},
		{"wrong audience", sign(t, s, hdr(), with(pay(), "aud", "https://evil.example")), "wrong_audience", s.Key},
		{"no audience", sign(t, s, hdr(), map[string]any{"iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}), "wrong_audience", s.Key},
		{"audience list", sign(t, s, hdr(), with(pay(), "aud", []string{"https://x", origin + "/"})), "", s.Key},
		{"no exp", sign(t, s, hdr(), map[string]any{"aud": origin, "iat": now.Unix()}), "bad_token", s.Key},
		{"fractional iat", sign(t, s, hdr(), with(pay(), "iat", 1.5)), "bad_token", s.Key},
		{"crit header", sign(t, s, with(hdr(), "crit", []string{"x"}), pay()), "bad_token", s.Key},
		{"padding", good + "==", "bad_token", s.Key},
		{"two parts", parts[0] + "." + parts[1], "bad_token", s.Key},
	}
	for _, c := range cases {
		if got := code(check(c.tok, c.key, now)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestParseKey(t *testing.T) {
	s, _ := NewSigner()
	if _, err := ParseKey(s.PrivateJWK()); code(err) != "bad_key" {
		t.Errorf("a private JWK must be refused, got %v", err)
	}
	for _, bad := range []string{
		`{"kty":"RSA","n":"x","e":"AQAB"}`,
		`{"kty":"EC","crv":"P-384","x":"x","y":"y"}`,
		`{"kty":"EC","crv":"P-256","x":"AAAA","y":"AAAA"}`,
		`{"kty":"EC","crv":"P-256","x":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","y":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`,
		`not json`,
	} {
		if _, err := ParseKey([]byte(bad)); code(err) != "bad_key" {
			t.Errorf("%s: got %v", bad, err)
		}
	}
	// Field order and extra members don't change the ID.
	var j map[string]any
	json.Unmarshal([]byte(s.Key.JWK), &j)
	j["alg"], j["use"] = "ES256", "sig"
	b, _ := json.Marshal(j)
	k, err := ParseKey(b)
	if err != nil || k.ID != s.Key.ID {
		t.Errorf("same key, different JSON: %v", err)
	}
}
