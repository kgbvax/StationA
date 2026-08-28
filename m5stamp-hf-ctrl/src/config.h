// config.h — non-secret, compile-time configuration for the M5 Stamp PLC #1
// firmware (m5stamp-hf-ctrl).
//
// This firmware is a COMPOUND embedded node: one M5 Stamp PLC publishes TWO
// station integration-model slots that share the same device{model,serial}
// (model §3 compound-device pattern):
//
//   muehle/hf/switch   — remote-on relays (relays 3 & 4): PA remote-on, TRX remote-on
//   muehle/hf/pa-arm   — the PA-enable arm relay (relay 1), with embedded arm logic
//
// relay 2 is spare. See docs/m5stamp-hf-ctrl-mqtt-api.md and
// ../stationa/docs/station-integration-model.md §7.1.
#pragma once

// --- slot addressing (site/station/slot) -------------------------------------
// The site is "muehle"; both slots live under hf/.
#define SITE "muehle"

#define SWITCH_SLOT "hf/switch"
#define PA_ARM_SLOT "hf/pa-arm"
#define RADIO_SLOT "hf/radio"       // subscribed for arm logic (tuning / band / liveness)
#define ANT_SWITCH_SLOT "hf/ant-switch"  // subscribed for arm logic (antenna grounded?)

// Full topic bases (no leading slash — station convention).
#define SWITCH_BASE SITE "/" SWITCH_SLOT     // muehle/hf/switch
#define PA_ARM_BASE  SITE "/" PA_ARM_SLOT     // muehle/hf/pa-arm
#define RADIO_BASE    SITE "/" RADIO_SLOT      // muehle/hf/radio
#define ANT_SWITCH_BASE SITE "/" ANT_SWITCH_SLOT  // muehle/hf/ant-switch

// --- relay map (M5StamPLC relay channels are 0-indexed 0..3) -----------------
// Plan relay numbering is 1-based; subtract 1 for the library channel.
//   relay 1 = PA arm        → channel 0
//   relay 2 = spare        → channel 1   (unused for now)
//   relay 3 = PA remote-on → channel 2
//   relay 4 = TRX remote-on→ channel 3
#define RELAY_PA_ARM    0
#define RELAY_SPARE     1
#define RELAY_PA_REMOTE 2
#define RELAY_TRX_REMOTE 3

// --- pa-arm fail-safe semantics ---------------------------------------------
// fail_safe = "open": the arm relay is DE-ENERGIZED = OPEN = PA DISABLED.
// Driving the relay (energize) closes it → armed. Loss of 13.8 V (or the PLC
// crashing) drops the relay open → armed false. So `armed == true` requires the
// relay energized, and any heartbeat/safety failure de-energizes it. (§6.)

// --- heartbeat / staleness --------------------------------------------------
// The arm logic requires a recent radio heartbeat. The radio's /state carries a
// device_online + ts; if no /state is received within this window the arm
// drops (radio unreachable ⇒ armed false). Conservative: 10 s.
#define RADIO_HEARTBEAT_MS 10000

// --- band safety ------------------------------------------------------------
// band_safe = the radio's current band is one the PA (ACOM 1200S) can amplify.
// The ACOM 1200S covers 160m..6m (model §7.1 capabilities). A band outside this
// set (or unknown) ⇒ armed false. 6m is the upper edge.
static const char* const SAFE_BANDS[] = {
    "160m", "80m", "60m", "40m", "30m", "20m", "17m", "15m", "12m", "10m", "6m"
};
static const int SAFE_BANDS_COUNT = sizeof(SAFE_BANDS) / sizeof(SAFE_BANDS[0]);

// --- MQTT ------------------------------------------------------------------
// PubSubClient default buffer is too small for retained /meta with an expose
// block; bump it before connect() (the LWT is set before connect).
#define MQTT_BUFFER_SIZE 1024
#define MQTT_KEEPALIVE_S 30
// Two slots ⇒ two MQTT connections (each with its own LWT), mirroring the Go
// compound bridge's one-client-per-slot decision. MQTT 3.1.1 allows one Will
// per client, so one connection per slot is what makes a PLC crash fire both
// slots' /status wills at once (no stale-online gap).

// Re-publish cadence for retained /meta (birth cert) on a quiet bus — the
// broker retains it, so this is only a safety net, not the primary path.
#define META_REFRESH_MS 300000

// --- local display / buttons -----------------------------------------------
// The M5 Stamp PLC has a 240x135 LCD and three front-panel buttons (A/B/C).
// We render indicator lights for the relay states and use B/C as local toggles.
//
// UI_REFRESH_MS was lowered to keep the LCD cool: the display redraw is the
// most expensive operation in the loop, and a 1 Hz refresh is plenty for a
// relay-state dashboard. Buttons are sampled at the main loop cadence below.
#define UI_REFRESH_MS      1000  // display redraw cadence, ms
#define UI_BTN_DEBOUNCE_MS 150   // minimum time between button actions, ms

// --- pa-arm state publish cadence -------------------------------------------
// The arm slot publishes immediately when enabled/armed/error changes, and on a
// slow heartbeat when idle. 10 s matches the radio heartbeat window and keeps the
// retained /state fresh without flooding the bus.
#define PA_ARM_HEARTBEAT_MS 10000

// --- main loop cadence -------------------------------------------------------
// The loop polls WiFi/MQTT, buttons, and arm logic at ~20 Hz. This is more than
// enough for human-scale button toggles and MQTT keepalives, while letting the
// CPU/SoC idle between iterations (no tight spin).
#define LOOP_DELAY_MS      50