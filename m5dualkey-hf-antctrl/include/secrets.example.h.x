// secrets.example.h.x — copy to include/secrets.h and fill in. include/secrets.h
// is gitignored. (The .x suffix keeps PlatformIO/IDE indexers from picking this
// template up as a real header — same pattern as m5dial-hf-rotctrl.)
//
//   cp include/secrets.example.h.x include/secrets.h
//
// Embedded firmware uses its own gitignored secrets file, NOT the systemd
// EnvironmentFile convention. See ../docs/conventions/config-and-secrets.md.
#pragma once

// WiFi — the station network.
#define SECRET_WIFI_SSID "CHANGE_ME"
#define SECRET_WIFI_PASSWORD "CHANGE_ME"

// MQTT broker — the one ultrabridge (muehle/hf/ant-ctrl) is on. The account
// needs read on muehle/hf/ant-ctrl/{state,status} and write on .../cmd.
#define SECRET_MQTT_HOST "192.168.1.50"
#define SECRET_MQTT_PORT 1883
#define SECRET_MQTT_USERNAME "CHANGE_ME"
#define SECRET_MQTT_PASSWORD "CHANGE_ME"
