# Prints tokens minted by Python PyJWT, one JSON object per line.
import json, sys, jwt
from jwt.algorithms import ECAlgorithm, OKPAlgorithm
from cryptography.hazmat.primitives.asymmetric import ec, ed25519
import base64, hashlib
aud, iat = sys.argv[1], int(sys.argv[2])
def b64(b): return base64.urlsafe_b64encode(b).rstrip(b"=").decode()
for alg, priv, algo in [("ES256", ec.generate_private_key(ec.SECP256R1()), ECAlgorithm), ("EdDSA", ed25519.Ed25519PrivateKey.generate(), OKPAlgorithm)]:
    jwk = json.loads(algo.to_jwk(priv.public_key()))
    jwk = {k: jwk[k] for k in (("crv", "kty", "x", "y") if alg == "ES256" else ("crv", "kty", "x"))}
    kid = b64(hashlib.sha256(json.dumps(jwk, separators=(",", ":"), sort_keys=True).encode()).digest())
    token = jwt.encode({"aud": aud, "iat": iat, "exp": iat + 3600}, priv, algorithm=alg, headers={"kid": kid, "typ": "JWT"})
    print(json.dumps({"tool": "pyjwt", "alg": alg, "jwk": jwk, "token": token}))
