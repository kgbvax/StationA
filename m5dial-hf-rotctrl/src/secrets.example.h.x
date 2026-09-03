// secrets.example.h — copy to src/secrets.h and fill in. src/secrets.h is gitignored.
//
// Embedded firmware (M5Stack Dial) uses its own gitignored secrets file, NOT
// the systemd EnvironmentFile convention (that is scoped to the Go services on
// shari). See ../docs/conventions/config-and-secrets.md.
//
//   cp src/secrets.example.h src/secrets.h
//   # then edit src/secrets.h
#pragma once

// WiFi — the station network the Dial joins.
#define WIFI_SSID "CHANGE_ME"
#define WIFI_PASSWORD "CHANGE_ME"

// MQTT broker — the shack broker on shari (192.168.1.139:1883); see
// ../mqtt-broker/README.md. The Dial is a remote device, so it uses shari's
// LAN address, not the 127.0.0.1 loopback the on-shari Go services use.
// Use the narrow "dial" account (read rotator state/status, write rotator
// cmd), NOT the broad "hf" account. See mqtt-broker/acl.conf.example.
#define MQTT_HOST "192.168.1.139"
#define MQTT_PORT 1883
#define MQTT_USER "dial"
#define MQTT_PASSWORD "CHANGE_ME"

// OTA update password. Required for network firmware uploads via espota —
// wireless OTA is wired into the first image, so only the very first flash
// needs USB.
#define OTA_PASSWORD "CHANGE_ME"

// Device identity (used for the ArduinoOTA hostname → m5dial-rotctrl-1.local
// and the mDNS name on the shack LAN).
#define DEVICE_MODEL "M5Stack Dial"
#define DEVICE_SERIAL "m5dial-rotctrl-1"