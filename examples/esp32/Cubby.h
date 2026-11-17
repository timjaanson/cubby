#pragma once
// Minimal cubby client for ESP32 (Arduino core 3.x): the device is its own visitor with an ES256
// (P-256) key in NVS. It signs its own 1-hour JWTs with mbedtls (key ID = RFC 7638 thumbprint,
// signature raw r || s), registers once, joins one space with an invite, then sends HTTP
// requests. A token the server refuses for clock skew is signed again on the server's time,
// once. No other retries: the caller resends from its own buffer.
#include <Arduino.h>

struct CubbyResult {
  int status = 0;          // HTTP status; < 0 for transport errors (HTTPClient codes)
  String error;            // cubby error code ("" when ok), or a local one
  String body;             // response body
  int index = -1;          // failing write of a batch, from the error
  uint32_t retryAfter = 0; // seconds, from a 429
  bool ok() const { return status >= 200 && status < 300; }
};

class Cubby {
 public:
  // origin: "https://cubby.example.com". rootsPem: the CA certificates to trust.
  // nowMs: the board's UTC clock in ms (0 when unknown). Loads key and state from NVS.
  void begin(const char *origin, const char *rootsPem, const char *label, int64_t (*nowMs)());

  bool hasKey() const { return keyLoaded; }
  bool registered() const { return visitor.length() > 0; }
  bool joined() const { return spaceName.length() > 0; }
  const String &deviceId() const { return kid; }
  const String &visitorId() const { return visitor; }
  const String &space() const { return spaceName; }
  const String &origin() const { return base; }
  int64_t serverOffsetMs() const { return clockOffset; } // added to nowMs() for tokens

  // Creates the key if there is none (call with the radio on, for true randomness),
  // then POST /v1/visitors. Idempotent.
  CubbyResult registerDevice();
  // POST /v1/{space}/join with an invite (inv_... or a link containing #invite=).
  CubbyResult join(const String &space, const String &invite);
  void leave();          // forgets the space locally
  void resetIdentity();  // deletes the key and visitor: the next register is a new visitor

  // A data request in the joined space: path is relative to /v1/{space}/ (like "batch",
  // "shared/k", "inbox/*/cmd.*"). Absolute paths start with /v1/.
  CubbyResult request(const char *method, const String &path, const String &body = String());
  CubbyResult batch(const String &json) { return request("POST", "batch", json); }


 private:
  String base, label, kid, visitor, spaceName, jwkJson, token;
  const char *roots = nullptr;
  int64_t (*clock)() = nullptr;
  int64_t clockOffset = 0, tokenExp = 0;
  bool keyLoaded = false;
  uint8_t priv[32] = {};

  bool loadKey();
  bool makeKey();
  bool deriveJwk();
  String sign(bool withJwk);
  int64_t nowSeconds();
  CubbyResult send(const char *method, const String &url, const String &body, const String &auth);
  void learnServerTime(const String &body);
};
