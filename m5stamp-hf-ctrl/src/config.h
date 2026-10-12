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
#define ANT_SWITCH_SLOT "hf/ant-switch"  // subscribed for arm logic (antenna grounded / switching)
#define ANT_CTRL_SLOT "hf/ant-ctrl"      // subscribed for arm logic (Ultrabeam moving / tuned)
#define ANT_SELECT_SLOT "hf/antenna-select"  // subscribed for arm logic (switch move pending)
#define PA_SLOT "hf/pa"                  // subscribed for arm logic (PA fault / liveness)
#define TUNER_SLOT "hf/tuner"            // subscribed for arm logic (ATU tune cycle)

// Full topic bases (no leading slash — station convention).
#define SWITCH_BASE SITE "/" SWITCH_SLOT     // muehle/hf/switch
#define PA_ARM_BASE  SITE "/" PA_ARM_SLOT     // muehle/hf/pa-arm
#define RADIO_BASE    SITE "/" RADIO_SLOT      // muehle/hf/radio
#define ANT_SWITCH_BASE SITE "/" ANT_SWITCH_SLOT  // muehle/hf/ant-switch
#define ANT_CTRL_BASE   SITE "/" ANT_CTRL_SLOT    // muehle/hf/ant-ctrl
#define ANT_SELECT_BASE SITE "/" ANT_SELECT_SLOT  // muehle/hf/antenna-select
#define PA_BASE         SITE "/" PA_SLOT          // muehle/hf/pa
#define TUNER_BASE      SITE "/" TUNER_SLOT       // muehle/hf/tuner

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
// band_safe = the band the radio is ACTUALLY on (derived from radio/state
// freq_hz, not the band label — flexbridge holds the label at the old band for
// 750 ms after a band change) is one the PA may amplify. A band outside this set
// (or a frequency outside every band) ⇒ armed false.
//
// 60m is deliberately absent: it is a 15 W EIRP allocation in DL/OE, the ACOM
// bridge has no 60m band, and a 1.2 kW PA must never arm there.
static const char* const SAFE_BANDS[] = {
    "160m", "80m", "40m", "30m", "20m", "17m", "15m", "12m", "10m", "6m"
};
static const int SAFE_BANDS_COUNT = sizeof(SAFE_BANDS) / sizeof(SAFE_BANDS[0]);

// Band edges for deriving the band from freq_hz. MUST mirror flexbridge's
// canonicalBands (flexbridge/internal/flexradio/band.go, IARU R1 edges per
// docs/conventions/band-mode-reference.md): inclusive edges, plus the same
// 2 kHz exit hysteresis so the PLC and flexbridge never disagree at a band edge.
struct BandEdge { uint32_t lo, hi; const char* label; };
static const BandEdge BAND_EDGES[] = {
    {1800000, 2000000, "160m"},   {3500000, 4000000, "80m"},
    {5351500, 5366500, "60m"},    {7000000, 7300000, "40m"},
    {10100000, 10150000, "30m"},  {14000000, 14350000, "20m"},
    {18068000, 18168000, "17m"},  {21000000, 21450000, "15m"},
    {24890000, 24990000, "12m"},  {28000000, 29700000, "10m"},
    {50000000, 54000000, "6m"},
};
static const int BAND_EDGES_COUNT = sizeof(BAND_EDGES) / sizeof(BAND_EDGES[0]);
#define BAND_EDGE_HYSTERESIS_HZ 2000

// --- arm timing ---------------------------------------------------------------
// Closing is slow, opening is fast. Every gate must hold for ARM_CLOSE_DWELL_MS
// with the radio in RX before the relay closes (no flapping on bus jitter, never
// hot-key the PA under drive). Exception: the gates of an Ultrabeam move in
// flight (commanded / moving / settling / off-frequency during it) are timed
// windows themselves — they block closing but do not restart the dwell, so the
// relay closes on the pass the move's settle ends.
#define ARM_CLOSE_DWELL_MS 1000
// After the radio moves to a different ham band, hold the arm open while the
// rest of the station follows: antennaselect switches the port / retunes the
// Ultrabeam / pre-positions the ACOM (one broker hop .50→.178), ultrabridge's
// first 2 s motor poll, the ACOM band walk (~150 ms per step).
#define BAND_CHANGE_HOLDOFF_MS 3000

// --- Ultrabeam (hf/ant-ctrl) --------------------------------------------------
// The switch port the Ultrabeam is wired to. Ultrabeam gates apply only while
// ant-switch/state.selected == this port. port4 matches the live antennaselect
// wiring map (17m → target port4, followed by ant-ctrl); the station docs still
// say port 3 (known issue O14) — confirm on site before changing.
#define ULTRABEAM_PORT "port4"
// ultrabridge retunes when |requested kHz − current kHz| >= 25 (integer kHz,
// ultrabridge controller.go freqDeadbandKHz). The PLC uses the SAME arithmetic:
// a radio frequency that would make ultrabridge move the elements is "not tuned".
#define ULTRABEAM_DEADBAND_KHZ 25
// ultrabridge polls the motors every 2 s and publishes /state change-only, so a
// commanded move can be invisible for up to one poll. After any ant-ctrl/cmd
// that will move the elements, treat the antenna as moving for this long.
#define ULTRABEAM_CMD_BLIND_MS 2500
// Settle after an UNOBSERVED change — a freq/direction change reported without
// a moving:true having been seen, or the switch entering the Ultrabeam port:
// mechanical settle + one ultrabridge poll period (2 s), since a short move can
// fall entirely between two polls.
#define ULTRABEAM_SETTLE_MS 3000
// Settle after an OBSERVED stop (moving true→false): the elements are at rest
// 250 ms after the RCU-06 reports the motors stopped. An observed stop replaces
// any longer settle window still running.
#define ULTRABEAM_STOP_SETTLE_MS 250

// --- relay readback -------------------------------------------------------------
// relayGet() reads the AW9523 output-pin level (coil driver), not the contacts.
// An open relay reading closed is rewritten every pass; this many consecutive
// failures raise "relay mismatch" (forces the arm open), as do this many
// confirmed drops of a closed relay. It clears after RELAY_MISMATCH_CLEAR
// consistent passes.
#define RELAY_MISMATCH_TRIP  3
#define RELAY_MISMATCH_CLEAR 20
// A CLOSED relay reading back open is acted on only after this many
// consecutive passes (one failed I²C read must not unkey the PA mid-over).
// Confirmed drops accumulate toward RELAY_MISMATCH_TRIP and are forgiven only
// after RELAY_STABLE_PASSES (60 s) closed and consistent.
#define RELAY_DROP_CONFIRM   3
#define RELAY_STABLE_PASSES  1200

// --- MQTT ------------------------------------------------------------------
// PubSubClient default buffer is too small for retained /meta with an expose
// block; bump it before connect() (the LWT is set before connect). The buffer
// also bounds INBOUND messages: a larger one is dropped silently, and pa-arm
// then never sees hf/radio/state ("radio offline", arm blocked). That state
// carries the dynamic mic_profiles + dvk_memories lists — ~1.5 KB on
// 2026-10-06, which overflowed the previous 1024. The pa-arm connection carries
// every arm input, so it gets 8 KB; hf/switch only sees its own small /cmd.
#define MQTT_BUFFER_SIZE        4096
#define PA_ARM_MQTT_BUFFER_SIZE 8192
// PubSubClient handles ONE inbound packet per loop() call. On (re)subscribe the
// broker replays ~14 retained arm inputs at once, and while keyed the ACOM and
// ATR bridges publish per meter frame; drain up to this many per main-loop pass
// so radio/state never queues behind telemetry.
#define MQTT_DRAIN_MAX 16
#define MQTT_KEEPALIVE_S 30

// Bounded reconnect (defect D6): a silent broker must not stall the arm-logic
// loop. PubSubClient::connect blocks in two places — the TCP SYN wait (ESP32
// WiFiClient default: 3000 ms) and the CONNACK wait (PubSubClient default:
// 15 s). Both are shrunk here; SlotMqtt::loop() additionally rate-limits
// attempts so at most ONE slot dials per main-loop pass. Worst-case single-pass
// stall ≈ MQTT_CONNECT_TIMEOUT_MS + 1 s CONNACK floor, vs ~6 s (two unbounded
// TCP waits) / ~18 s (CONNACK case) before.
#define MQTT_CONNECT_TIMEOUT_MS   150  // TCP SYN wait per attempt, ms
#define MQTT_CONNACK_TIMEOUT_S    1    // CONNACK wait, whole seconds (PubSubClient floor)
#define MQTT_RECONNECT_BACKOFF_MS 1000 // min spacing between connect attempts (shared)
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