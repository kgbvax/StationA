// secrets.example.h — copy to src/secrets.h and fill in. src/secrets.h is gitignored.
//
// Embedded firmware (M5 Stamp PLC) uses its own gitignored secrets file, NOT the
// systemd EnvironmentFile convention (that is scoped to the Go services on
// shari). See ../stationa/docs/conventions/config-and-secrets.md.
//
//   cp src/secrets.example.h src/secrets.h
//   # then edit src/secrets.h
#pragma once

// WiFi — the station network the PLC joins.
#define WIFI_SSID "CHANGE_ME"
#define WIFI_PASSWORD "CHANGE_ME"

// MQTT broker — the station broker at 192.168.1.50:1883 (model §2).
#define MQTT_HOST "192.168.1.50"
#define MQTT_PORT 1883
#define MQTT_USER "hf"
#define MQTT_PASSWORD "CHANGE_ME"

// OTA update password. Required for network firmware uploads via espota.
#define OTA_PASSWORD "CHANGE_ME"

// Device identity published in BOTH slots' /meta (compound device — the shared
// device{model,serial} is the compound-device relationship, model §3). Use the
// PLC's stable id (e.g. its MAC-derived name or a sticker).
#define DEVICE_MODEL "M5Stamp PLC"
#define DEVICE_SERIAL "m5stamp-plc-1"