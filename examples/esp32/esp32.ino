// Joins a cubby space once, then writes a value every minute.
// Fill in the five settings, then build for any ESP32 with Arduino core 3.x.
// The invite is used on the first boot only; the key, visitor and space stay in NVS.
#include <WiFi.h>
#include <time.h>
#include "Cubby.h"
#include "LeRoots.h"

const char *WIFI_SSID = "your-wifi";
const char *WIFI_PASS = "your-password";
const char *SERVER = "https://cubby.example.com"; // the server's public origin, no trailing slash
const char *SPACE = "your-space-name";
const char *INVITE = "inv_...";                   // an invite, or a link with #invite=

Cubby cubby;

int64_t nowMs() {
  time_t t = time(nullptr);
  return t > 1700000000 ? (int64_t)t * 1000 : 0; // 0 until NTP has the time
}

void setup() {
  Serial.begin(115200);
  WiFi.begin(WIFI_SSID, WIFI_PASS);
  while (WiFi.status() != WL_CONNECTED) delay(250);
  configTime(0, 0, "pool.ntp.org");
  while (!nowMs()) delay(250);

  cubby.begin(SERVER, LE_ROOTS, "esp32 example", nowMs);
  if (!cubby.registered()) {
    CubbyResult r = cubby.registerDevice();
    Serial.printf("register: %d %s %s\n", r.status, r.error.c_str(), cubby.visitorId().c_str());
  }
  if (!cubby.joined()) {
    CubbyResult r = cubby.join(SPACE, INVITE);
    Serial.printf("join: %d %s\n", r.status, r.body.c_str());
  }
}

void loop() {
  // PUT replaces the value. For a time series use POST "shared/uptime/series" instead.
  String value = String(millis() / 1000);
  CubbyResult r = cubby.request("PUT", "shared/uptime", value);
  Serial.printf("PUT shared/uptime %s: %d %s\n", value.c_str(), r.status, r.error.c_str());
  delay(60000);
}
