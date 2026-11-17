#include "Cubby.h"
#include <NetworkClientSecure.h>
#include <HTTPClient.h>
#include <Preferences.h>
#include <esp_random.h>
#include <mbedtls/ecp.h>
#include <mbedtls/ecdsa.h>
#include <mbedtls/sha256.h>
#include <mbedtls/base64.h>
#include <mbedtls/bignum.h>

static const char *NS = "cubby";
static NetworkClientSecure tls;
static HTTPClient http;

static int hwRandom(void *, unsigned char *out, size_t len) { esp_fill_random(out, len); return 0; }

static String b64url(const uint8_t *data, size_t len) {
  size_t olen = 0;
  String out;
  unsigned char *buf = (unsigned char *)malloc(len * 4 / 3 + 8);
  if (!buf) return out;
  if (mbedtls_base64_encode(buf, len * 4 / 3 + 8, &olen, data, len) == 0) {
    out.reserve(olen);
    for (size_t i = 0; i < olen; ++i) {
      char c = buf[i];
      if (c == '=') break;
      out += c == '+' ? '-' : c == '/' ? '_' : c;
    }
  }
  free(buf);
  return out;
}
static String b64url(const String &s) { return b64url((const uint8_t *)s.c_str(), s.length()); }

// The value of "name": "..." or "name": 123 in a flat JSON answer. Good enough for cubby's replies.
static String jsonField(const String &body, const char *name) {
  String key = String("\"") + name + "\":";
  int i = body.indexOf(key);
  if (i < 0) return String();
  i += key.length();
  while (i < (int)body.length() && body[i] == ' ') ++i;
  if (i < (int)body.length() && body[i] == '"') {
    int j = body.indexOf('"', i + 1);
    return j < 0 ? String() : body.substring(i + 1, j);
  }
  int j = i;
  while (j < (int)body.length() && (isdigit(body[j]) || body[j] == '-')) ++j;
  return body.substring(i, j);
}

void Cubby::begin(const char *origin, const char *rootsPem, const char *deviceLabel, int64_t (*nowMs)()) {
  base = origin; roots = rootsPem; label = deviceLabel; clock = nowMs;
  tls.setCACert(roots);
  http.setReuse(true);
  http.setTimeout(15000);
  http.setConnectTimeout(10000);
  Preferences p; p.begin(NS, true);
  visitor = p.getString("visitor", "");
  spaceName = p.getString("space", "");
  p.end();
  loadKey();
}

bool Cubby::loadKey() {
  Preferences p; p.begin(NS, true);
  keyLoaded = p.getBytes("d", priv, sizeof(priv)) == sizeof(priv);
  p.end();
  return keyLoaded && deriveJwk();
}

bool Cubby::makeKey() {
  mbedtls_ecp_group grp; mbedtls_mpi d; mbedtls_ecp_point q;
  mbedtls_ecp_group_init(&grp); mbedtls_mpi_init(&d); mbedtls_ecp_point_init(&q);
  bool ok = mbedtls_ecp_group_load(&grp, MBEDTLS_ECP_DP_SECP256R1) == 0 &&
            mbedtls_ecp_gen_keypair(&grp, &d, &q, hwRandom, nullptr) == 0 &&
            mbedtls_mpi_write_binary(&d, priv, 32) == 0;
  mbedtls_ecp_point_free(&q); mbedtls_mpi_free(&d); mbedtls_ecp_group_free(&grp);
  if (!ok) return false;
  Preferences p; p.begin(NS, false);
  p.putBytes("d", priv, sizeof(priv));
  p.remove("visitor");
  p.end();
  visitor = "";
  keyLoaded = true;
  return deriveJwk();
}

// Public point from the private scalar, the JWK, and its RFC 7638 thumbprint as the key ID.
bool Cubby::deriveJwk() {
  mbedtls_ecp_group grp; mbedtls_mpi d; mbedtls_ecp_point q;
  mbedtls_ecp_group_init(&grp); mbedtls_mpi_init(&d); mbedtls_ecp_point_init(&q);
  uint8_t x[32], y[32];
  bool ok = mbedtls_ecp_group_load(&grp, MBEDTLS_ECP_DP_SECP256R1) == 0 &&
            mbedtls_mpi_read_binary(&d, priv, 32) == 0 &&
            mbedtls_ecp_mul(&grp, &q, &d, &grp.G, hwRandom, nullptr) == 0 &&
            mbedtls_mpi_write_binary(&q.MBEDTLS_PRIVATE(X), x, 32) == 0 &&
            mbedtls_mpi_write_binary(&q.MBEDTLS_PRIVATE(Y), y, 32) == 0;
  mbedtls_ecp_point_free(&q); mbedtls_mpi_free(&d); mbedtls_ecp_group_free(&grp);
  if (!ok) { keyLoaded = false; return false; }
  jwkJson = "{\"crv\":\"P-256\",\"kty\":\"EC\",\"x\":\"" + b64url(x, 32) + "\",\"y\":\"" + b64url(y, 32) + "\"}";
  uint8_t h[32];
  mbedtls_sha256((const uint8_t *)jwkJson.c_str(), jwkJson.length(), h, 0);
  kid = b64url(h, 32);
  token = ""; tokenExp = 0;
  return true;
}

int64_t Cubby::nowSeconds() {
  int64_t ms = clock ? clock() : 0;
  if (ms <= 0) return 0;
  return (ms + clockOffset) / 1000;
}

String Cubby::sign(bool withJwk) {
  int64_t now = nowSeconds();
  if (!keyLoaded || now <= 0) return String();
  if (!withJwk && token.length() && now < tokenExp - 600) return token;
  String header = "{\"alg\":\"ES256\",\"typ\":\"JWT\",\"kid\":\"" + kid + "\"";
  if (withJwk) header += ",\"jwk\":" + jwkJson;
  header += "}";
  int64_t iat = now - 30, exp = iat + 3600; // 30 s back absorbs a little clock skew
  String payload = "{\"aud\":\"" + base + "\",\"iat\":" + String((long long)iat) + ",\"exp\":" + String((long long)exp) + "}";
  String input = b64url(header) + "." + b64url(payload);
  uint8_t h[32], sig[64];
  mbedtls_sha256((const uint8_t *)input.c_str(), input.length(), h, 0);
  mbedtls_ecp_group grp; mbedtls_mpi d, r, s;
  mbedtls_ecp_group_init(&grp); mbedtls_mpi_init(&d); mbedtls_mpi_init(&r); mbedtls_mpi_init(&s);
  // Deterministic ECDSA (RFC 6979); the signature is raw r || s, as JOSE wants, not DER.
  bool ok = mbedtls_ecp_group_load(&grp, MBEDTLS_ECP_DP_SECP256R1) == 0 &&
            mbedtls_mpi_read_binary(&d, priv, 32) == 0 &&
            mbedtls_ecdsa_sign_det_ext(&grp, &r, &s, &d, h, 32, MBEDTLS_MD_SHA256, hwRandom, nullptr) == 0 &&
            mbedtls_mpi_write_binary(&r, sig, 32) == 0 && mbedtls_mpi_write_binary(&s, sig + 32, 32) == 0;
  mbedtls_mpi_free(&s); mbedtls_mpi_free(&r); mbedtls_mpi_free(&d); mbedtls_ecp_group_free(&grp);
  if (!ok) return String();
  String jwt = input + "." + b64url(sig, 64);
  if (!withJwk) { token = jwt; tokenExp = exp; }
  return jwt;
}

void Cubby::learnServerTime(const String &body) {
  String st = jsonField(body, "serverTime");
  int64_t server = st.length() ? atoll(st.c_str()) : 0;
  int64_t local = clock ? clock() : 0;
  if (server > 0 && local > 0) { clockOffset = server - local; token = ""; tokenExp = 0; }
}

CubbyResult Cubby::send(const char *method, const String &url, const String &body, const String &auth) {
  CubbyResult r;
  if (!http.begin(tls, url)) { r.status = -1; r.error = "bad_url"; return r; }
  static const char *keep[] = {"Retry-After"};
  http.collectHeaders(keep, 1);
  if (auth.length()) http.addHeader("Authorization", "Bearer " + auth);
  if (body.length()) http.addHeader("Content-Type", "application/json");
  r.status = http.sendRequest(method, (uint8_t *)body.c_str(), body.length());
  if (r.status > 0) {
    r.body = http.getString();
    if (!r.ok()) {
      r.error = jsonField(r.body, "error");
      String idx = jsonField(r.body, "index");
      if (idx.length()) r.index = idx.toInt();
      r.retryAfter = http.header("Retry-After").toInt();
    }
  } else {
    r.error = "transport";
    http.end(); tls.stop(); // drop a broken keep-alive connection
    return r;
  }
  http.end();
  return r;
}

CubbyResult Cubby::registerDevice() {
  CubbyResult r;
  if (!keyLoaded && !makeKey()) { r.error = "no_key"; return r; }
  if (nowSeconds() <= 0) { r.error = "no_clock"; return r; }
  String jwt = sign(true);
  String body = "{\"label\":\"" + label + "\"}";
  r = send("POST", base + "/v1/visitors", body, jwt);
  if (r.error == "token_expired" || r.error == "token_not_yet_valid") { learnServerTime(r.body); r = send("POST", base + "/v1/visitors", body, sign(true)); }
  if (r.ok()) {
    visitor = jsonField(r.body, "id");
    Preferences p; p.begin(NS, false); p.putString("visitor", visitor); p.end();
  }
  return r;
}

CubbyResult Cubby::join(const String &space, const String &inviteOrLink) {
  String invite = inviteOrLink;
  int at = invite.indexOf("invite=");
  if (at >= 0) invite = invite.substring(at + 7);
  int amp = invite.indexOf('&'); if (amp >= 0) invite = invite.substring(0, amp);
  CubbyResult r = request("POST", "/v1/" + space + "/join", "{\"invite\":\"" + invite + "\"}");
  if (r.ok()) {
    spaceName = space;
    Preferences p; p.begin(NS, false); p.putString("space", space); p.end();
  }
  return r;
}

void Cubby::leave() {
  spaceName = "";
  Preferences p; p.begin(NS, false); p.remove("space"); p.end();
}

void Cubby::resetIdentity() {
  Preferences p; p.begin(NS, false); p.clear(); p.end();
  memset(priv, 0, sizeof(priv));
  keyLoaded = false; visitor = ""; spaceName = ""; kid = ""; jwkJson = ""; token = ""; tokenExp = 0;
}

CubbyResult Cubby::request(const char *method, const String &path, const String &body) {
  CubbyResult r;
  if (!registered()) { r.error = "not_registered"; return r; }
  String url = path.startsWith("/v1/") ? base + path : base + "/v1/" + spaceName + "/" + path;
  if (!path.startsWith("/v1/") && !joined()) { r.error = "no_space"; return r; }
  String jwt = sign(false);
  if (!jwt.length()) { r.error = "no_clock"; return r; }
  r = send(method, url, body, jwt);
  if (r.error == "token_expired" || r.error == "token_not_yet_valid") {
    // The board's clock is off from the server's: adopt the server's time for tokens and retry once.
    learnServerTime(r.body);
    r = send(method, url, body, sign(false));
  }
  return r;
}
