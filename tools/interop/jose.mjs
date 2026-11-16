// Prints tokens minted by npm jose, one JSON object per line.
import { SignJWT, generateKeyPair, exportJWK, calculateJwkThumbprint } from "jose"
const [aud, iat] = [process.argv[2], Number(process.argv[3])]
for (const alg of ["ES256", "EdDSA"]) {
  const { publicKey, privateKey } = await generateKeyPair(alg, alg === "EdDSA" ? { crv: "Ed25519" } : {})
  const jwk = await exportJWK(publicKey)
  const kid = await calculateJwkThumbprint(jwk)
  const token = await new SignJWT({}).setProtectedHeader({ alg, typ: "JWT", kid })
    .setAudience(aud).setIssuedAt(iat).setExpirationTime(iat + 3600).sign(privateKey)
  console.log(JSON.stringify({ tool: "jose", alg, jwk, token }))
}
