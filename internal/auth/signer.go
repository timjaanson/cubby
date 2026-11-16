package auth

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math/big"
	"time"
)

// Signer holds a private device key and signs tokens with it. The CLI and
// tests use it; servers only verify.
type Signer struct {
	Key *Key
	ec  *ecdsa.PrivateKey
	ed  ed25519.PrivateKey
}

// NewSigner makes a fresh ES256 key, the default for every cubby client.
func NewSigner() (*Signer, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return signerEC(priv)
}

// NewEdSigner makes a fresh EdDSA key.
func NewEdSigner() (*Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return signerEd(priv)
}

func signerEC(priv *ecdsa.PrivateKey) (*Signer, error) {
	pub, _ := json.Marshal(jwk{Kty: "EC", Crv: "P-256", X: b64.EncodeToString(pad32(priv.X)), Y: b64.EncodeToString(pad32(priv.Y))})
	k, err := ParseKey(pub)
	if err != nil {
		return nil, err
	}
	return &Signer{Key: k, ec: priv}, nil
}

func signerEd(priv ed25519.PrivateKey) (*Signer, error) {
	pub, _ := json.Marshal(jwk{Kty: "OKP", Crv: "Ed25519", X: b64.EncodeToString(priv.Public().(ed25519.PublicKey))})
	k, err := ParseKey(pub)
	if err != nil {
		return nil, err
	}
	return &Signer{Key: k, ed: priv}, nil
}

// SignerFromJWK reads a private JWK, the "key" of a credentials file.
func SignerFromJWK(raw []byte) (*Signer, error) {
	var j jwk
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, errors.New("the key is not a JWK object")
	}
	d, err := b64.DecodeString(j.D)
	if err != nil || len(d) != 32 {
		return nil, errors.New("the key has no private part (\"d\")")
	}
	switch {
	case j.Kty == "EC" && j.Crv == "P-256":
		priv := &ecdsa.PrivateKey{D: new(big.Int).SetBytes(d)}
		priv.Curve = elliptic.P256()
		priv.X, priv.Y = priv.Curve.ScalarBaseMult(d)
		s, err := signerEC(priv)
		if err == nil && s.Key.JWK != canonOf(j) {
			return nil, errors.New("the key's private part doesn't match its public part")
		}
		return s, err
	case j.Kty == "OKP" && j.Crv == "Ed25519":
		s, err := signerEd(ed25519.NewKeyFromSeed(d))
		if err == nil && s.Key.JWK != canonOf(j) {
			return nil, errors.New("the key's private part doesn't match its public part")
		}
		return s, err
	}
	return nil, errors.New("keys are ES256 (P-256) or EdDSA (Ed25519)")
}

func canonOf(j jwk) string {
	if j.Kty == "EC" {
		return `{"crv":"P-256","kty":"EC","x":"` + j.X + `","y":"` + j.Y + `"}`
	}
	return `{"crv":"Ed25519","kty":"OKP","x":"` + j.X + `"}`
}

// PrivateJWK is the full key, for a credentials file.
func (s *Signer) PrivateJWK() json.RawMessage {
	var j jwk
	json.Unmarshal([]byte(s.Key.JWK), &j)
	if s.ec != nil {
		j.D = b64.EncodeToString(pad32(s.ec.D))
	} else {
		j.D = b64.EncodeToString(s.ed.Seed())
	}
	b, _ := json.Marshal(j)
	return b
}

// Token signs a token for the server at origin, valid for ttl from now.
// withKey puts the public key in the header, as registering a key needs.
func (s *Signer) Token(origin string, ttl time.Duration, withKey bool) string {
	return s.TokenAt(origin, time.Now(), ttl, withKey)
}

// TokenAt is Token issued at now, for a client that corrects its clock.
func (s *Signer) TokenAt(origin string, now time.Time, ttl time.Duration, withKey bool) string {
	h := map[string]any{"alg": s.Key.Alg, "typ": "JWT", "kid": s.Key.ID}
	if withKey {
		h["jwk"] = json.RawMessage(s.Key.JWK)
	}
	hb, _ := json.Marshal(h)
	pb, _ := json.Marshal(map[string]any{"aud": origin, "iat": now.Unix(), "exp": now.Add(ttl).Unix()})
	in := b64.EncodeToString(hb) + "." + b64.EncodeToString(pb)
	return in + "." + b64.EncodeToString(s.sign([]byte(in)))
}

func (s *Signer) sign(msg []byte) []byte {
	if s.ed != nil {
		return ed25519.Sign(s.ed, msg)
	}
	h := sha256.Sum256(msg)
	r, sv, err := ecdsa.Sign(rand.Reader, s.ec, h[:])
	if err != nil {
		panic(err)
	}
	return append(pad32(r), pad32(sv)...)
}

func pad32(n *big.Int) []byte {
	b := n.Bytes()
	if len(b) >= 32 {
		return b
	}
	return append(make([]byte, 32-len(b)), b...)
}
