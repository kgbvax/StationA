// arm.h — the pa-arm interlock: input feeds, gate evaluation, relay drive.
//
// relay 1 (PA arm, fail-safe-open) closes only when the operator permit
// (`enabled`, pa-arm/cmd set_enabled) is on AND every gate below holds. Inputs
// are the station's own bus snapshots, subscribed on the pa-arm connection:
//
//   hf/radio        /status + /state  liveness, heartbeat, tuning, tx, freq → band
//   hf/ant-switch   /status + /state  selected port, settled (relay travel)
//   hf/ant-ctrl     /status + /state  Ultrabeam device_online, moving, freq, direction
//   hf/ant-ctrl     /cmd              commanded moves, seen before ultrabridge's 2 s poll
//   hf/antenna-select /status + /state  reconciler target (a port move is pending)
//   hf/pa           /status + /state  ACOM liveness + fault
//   hf/tuner        /status + /state  ATR-1000 tune cycle (settling)
//
// Gates come in two classes (model §6: a slow arm permit ANDed with the fast
// hardware key line — not a fast software gate):
//
//   HAZARD    RF is (or is about to be) going somewhere unsafe RIGHT NOW: radio
//             link lost/stale, radio tuning, band not PA-safe, antenna grounded,
//             switch relays travelling, Ultrabeam moving / commanded / retracted
//             / off-frequency, PA fault, ATU tune cycle, relay mismatch.
//             → opens the relay immediately, even while transmitting.
//   READINESS the station has not finished following a change, or a supervising
//             feed is down: band-change hold-off, Ultrabeam settle, antenna move
//             pending, Ultrabeam / switch / PA offline.
//             → blocks closing; opens a closed relay only while the radio is in
//               RX (never hot-unkeys the PA under drive for bookkeeping).
//
// Closing needs the permit, every gate clear AND the radio in RX, all held
// continuously for ARM_CLOSE_DWELL_MS (a QSK/VOX gap between elements never
// closes the relay). The relay is readback-checked every loop pass: a lost write
// in the opening direction is rewritten at once, a closed relay that reads back
// open is treated as dropped and re-closes only through the dwell + RX path.
// `armed` reports the readback.
//
// Producers that publish change-only (ultrabridge, ant-switch, antennaselect,
// ACOM, ATR) get no age window — their liveness is the two-layer rule: the
// /status LWT AND /state.device_online. Only flexbridge heartbeats (5 s), so
// only the radio feed has a 10 s freshness window. Every input resets to the
// closed side when the pa-arm connection (or WiFi) drops, and on each
// (re)subscribe, so the arm can only close on post-reconnect data.
#pragma once

#include <Arduino.h>
#include <ArduinoJson.h>

#include "config.h"
#include "relay.h"

namespace arm {

// ---------------------------------------------------------------------------
// inputs — every field starts on the closed (blocking) side
// ---------------------------------------------------------------------------
struct Radio {
    bool statusOnline = false;
    bool stateSeen = false;      // a radio/state arrived since the last (re)subscribe
    bool deviceOnline = false;
    bool tuning = true;          // missing field ⇒ tuning
    bool rx = false;             // tx == "rx"; anything else ⇒ not RX
    uint32_t freqHz = 0;
    String band = "";            // derived from freqHz (flexbridge hysteresis), "" = none
    // Heartbeat: refreshed only by a NEW ts that is not the first message after a
    // subscribe — a retained replay (resubscribe, or the .50↔.178 bridge
    // reconnecting) repeats the old ts and must not revive a dead feed.
    String lastTs = "";          // survives reconnects
    uint8_t msgsSinceSub = 0;    // saturates at 2 (only "first after subscribe" matters)
    bool freshValid = false;
    unsigned long freshMs = 0;
};
struct AntSwitch {
    bool statusOnline = false;
    bool stateSeen = false;
    String selected = "";        // kept across reconnects (edge detection)
    bool settled = false;
};
struct AntCtrl {
    bool statusOnline = false;
    bool stateSeen = false;
    bool everSeen = false;
    bool deviceOnline = false;
    bool moving = true;          // missing field ⇒ moving
    uint32_t freqHz = 0;         // the RCU-06's current (target) frequency
    String direction = "";
    bool retracted = false;      // ultrabridge's latch (absent on older ultrabridge ⇒ false)
};
struct AntSelect {
    bool statusOnline = false;
    String target = "";          // "" = no opinion (manual mode / unresolvable)
};
struct Pa {
    bool statusOnline = false;
    bool stateSeen = false;
    bool deviceOnline = false;
    String fault = "";           // "" = no telemetry yet (acombridge open-time snapshot)
    String keyed = "";           // "" = no telemetry yet
};
struct Tuner {
    bool statusOnline = false;
    bool deviceOnline = false;
    bool settling = false;
};

Radio radio;
AntSwitch antSw;
AntCtrl ub;
AntSelect antSel;
Pa pa;
Tuner tuner;

// Edges / timers — local knowledge, survive reconnects.
String lastHamBand = "";
bool bandEdgeValid = false;
unsigned long bandEdgeMs = 0;
bool ubSettleValid = false;
unsigned long ubSettleMs = 0;     // settle hold-off starts here
bool ubCmdValid = false;
unsigned long ubCmdMs = 0;        // last ant-ctrl/cmd that will move the elements
bool ubRetracted = false;         // latched by a retract cmd; cleared by a later motion cmd

// Relay drive.
bool relayCmd = false;            // what we command relay 1 to be
bool relayReadback = false;       // what the expander reports (published as `armed`)
bool relayFault = false;
uint8_t relayMismatchCount = 0;  // open-direction: consecutive passes reading closed after a rewrite
uint8_t relayOkCount = 0;        // consecutive consistent passes (clears relayFault)
uint8_t relayDropPending = 0;    // consecutive passes a CLOSED relay read back open
uint8_t relayDropCount = 0;      // confirmed drops; reset after RELAY_STABLE_PASSES closed + consistent
uint16_t relayStablePasses = 0;
bool closeOkValid = false;        // permit ∧ gates clear ∧ RX, held since closeOkSinceMs
unsigned long closeOkSinceMs = 0;

// invalidate resets every input to unknown/closed. Idempotent: called on every
// pass while WiFi or the pa-arm connection is down, and on each (re)subscribe.
// Edge-detection memory (last ts, last band, last ub freq/direction, selected
// port, retract latch) is kept so a replay is not mistaken for a change.
inline void invalidate() {
    radio.statusOnline = false;
    radio.stateSeen = false;
    radio.deviceOnline = false;
    radio.tuning = true;
    radio.rx = false;
    radio.msgsSinceSub = 0;
    radio.freshValid = false;
    antSw.statusOnline = false;
    antSw.stateSeen = false;
    antSw.settled = false;
    ub.statusOnline = false;
    ub.stateSeen = false;
    ub.deviceOnline = false;
    ub.moving = true;
    antSel.statusOnline = false;
    antSel.target = "";
    pa.statusOnline = false;
    pa.stateSeen = false;
    pa.deviceOnline = false;
    pa.fault = "";
    pa.keyed = "";
    tuner.statusOnline = false;
    tuner.deviceOnline = false;
    tuner.settling = false;
}

// ---------------------------------------------------------------------------
// band derivation — mirrors flexbridge BandForFreqWithPrev
// ---------------------------------------------------------------------------
inline String bandForFreq(uint32_t hz, const String& prev) {
    for (int i = 0; i < BAND_EDGES_COUNT; ++i) {
        if (hz >= BAND_EDGES[i].lo && hz <= BAND_EDGES[i].hi) return String(BAND_EDGES[i].label);
    }
    // Exit hysteresis applies only into the HF general-coverage gap ("gen").
    if (prev.length() > 0 && hz >= 1800000 && hz <= 30000000) {
        for (int i = 0; i < BAND_EDGES_COUNT; ++i) {
            if (prev != BAND_EDGES[i].label) continue;
            uint32_t lo = BAND_EDGES[i].lo, hi = BAND_EDGES[i].hi;
            if (hz > hi && hz <= hi + BAND_EDGE_HYSTERESIS_HZ) return prev;
            if (hz < lo && hz + BAND_EDGE_HYSTERESIS_HZ >= lo) return prev;
        }
    }
    return String("");
}

inline bool bandSafe(const String& band) {
    if (band.length() == 0) return false;
    for (int i = 0; i < SAFE_BANDS_COUNT; ++i) {
        if (band == SAFE_BANDS[i]) return true;
    }
    return false;
}

inline uint32_t absDiff(uint32_t a, uint32_t b) { return a > b ? a - b : b - a; }

inline bool ultrabeamInCircuit() {
    return antSw.stateSeen && antSw.selected == ULTRABEAM_PORT;
}

// ---------------------------------------------------------------------------
// input handlers (called from the pa-arm PubSubClient callback)
// ---------------------------------------------------------------------------
inline bool payloadIs(const uint8_t* p, unsigned int len, const char* s) {
    size_t n = strlen(s);
    return len == n && memcmp(p, s, n) == 0;
}
inline bool statusOnline(const uint8_t* p, unsigned int len) { return payloadIs(p, len, "online"); }

inline void onRadioState(const uint8_t* payload, unsigned int len, unsigned long now) {
    static JsonDocument filter;
    static bool init = false;
    if (!init) {
        filter["device_online"] = true;
        filter["tuning"] = true;
        filter["tx"] = true;
        filter["freq_hz"] = true;
        filter["ts"] = true;
        init = true;
    }
    JsonDocument doc;
    if (deserializeJson(doc, payload, len, DeserializationOption::Filter(filter))) return;
    radio.stateSeen = true;
    radio.deviceOnline = doc["device_online"] | false;
    radio.tuning = doc["tuning"] | true;
    radio.rx = strcmp(doc["tx"] | "", "rx") == 0;
    // Any non-RX message restarts the close dwell, even if an "rx" follows in
    // the same loop pass (a QSK/VOX gap must never complete the dwell).
    if (!radio.rx) closeOkValid = false;
    radio.freqHz = doc["freq_hz"] | (uint32_t)0;
    String nb = bandForFreq(radio.freqHz, radio.band);
    radio.band = nb;
    if (nb.length() > 0 && nb != lastHamBand) {
        // A move to a DIFFERENT ham band (not an excursion out of and back into
        // the same one): hold the arm while the station follows.
        if (lastHamBand.length() > 0) {
            bandEdgeValid = true;
            bandEdgeMs = now;
        }
        lastHamBand = nb;
    }
    String ts = doc["ts"] | "";
    if (radio.msgsSinceSub < 2) radio.msgsSinceSub++;
    if (radio.msgsSinceSub >= 2 && ts.length() > 0 && ts != radio.lastTs) {
        radio.freshValid = true;
        radio.freshMs = now;
    }
    radio.lastTs = ts;
}

inline void onAntSwitchState(const uint8_t* payload, unsigned int len, unsigned long now) {
    JsonDocument doc;
    if (deserializeJson(doc, payload, len)) return;
    String sel = doc["selected"] | "";
    if (sel == ULTRABEAM_PORT && antSw.selected != ULTRABEAM_PORT) {
        // Switched onto the Ultrabeam: give antennaselect's follow + ultrabridge
        // one settle period before trusting its tuning.
        ubSettleValid = true;
        ubSettleMs = now;
    }
    antSw.selected = sel;
    antSw.settled = doc["settled"] | false;
    antSw.stateSeen = true;
}

inline void onAntCtrlState(const uint8_t* payload, unsigned int len, unsigned long now) {
    static JsonDocument filter;
    static bool init = false;
    if (!init) {
        filter["device_online"] = true;
        filter["moving"] = true;
        filter["freq_hz"] = true;
        filter["direction"] = true;
        filter["retracted"] = true;
        init = true;
    }
    JsonDocument doc;
    if (deserializeJson(doc, payload, len, DeserializationOption::Filter(filter))) return;
    bool moving = doc["moving"] | true;
    uint32_t freq = doc["freq_hz"] | (uint32_t)0;
    String dir = doc["direction"] | "";
    bool changed = !ub.everSeen || freq != ub.freqHz || dir != ub.direction;
    bool stopped = ub.stateSeen && ub.moving && !moving;
    if (changed || stopped) {
        ubSettleValid = true;
        ubSettleMs = now;
    }
    ub.deviceOnline = doc["device_online"] | false;
    ub.moving = moving;
    ub.freqHz = freq;
    ub.direction = dir;
    ub.retracted = doc["retracted"] | false;
    // ultrabridge is the authority on retraction once it has had a poll to
    // report it: a retracted:false after the blind window clears the PLC's own
    // cmd latch (re-extended from the web UI, or the retract never executed).
    // Only when the field is present — an older ultrabridge never reports it.
    if (ubRetracted && doc["retracted"].is<bool>() && !ub.retracted &&
        !(ubCmdValid && now - ubCmdMs < ULTRABEAM_CMD_BLIND_MS)) {
        ubRetracted = false;
    }
    ub.stateSeen = true;
    ub.everSeen = true;
}

// ultrabridge's band → centre frequency map (ultrabridge/internal/mqtt/client.go
// bandCenterKHz): a `band` cmd retunes to the centre, deadband permitting.
struct BandCentre { const char* band; uint32_t khz; };
static const BandCentre UB_BAND_CENTRES[] = {
    {"20m", 14175}, {"17m", 18118}, {"15m", 21225},
    {"12m", 24940}, {"10m", 28850}, {"6m", 51000},
};

// onAntCtrlCmd watches commands to the Ultrabeam so a move is known the moment
// it is requested — ultrabridge only reports motion at its next 2 s poll. It
// mirrors what ultrabridge actually sends to the RCU-06: frequency and band go
// through the 25 kHz deadband (so VFO steps never drop the arm), a direction
// command ALWAYS drives the motors (SetMode bypasses the deadband — it also
// re-extends a retracted beam), retract always moves.
inline void onAntCtrlCmd(const uint8_t* payload, unsigned int len, unsigned long now) {
    if (len == 0) return;  // ultrabridge's retained-clear marker
    JsonDocument doc;
    if (deserializeJson(doc, payload, len)) return;
    String action = doc["action"] | "";
    bool motion = false;
    if (action == "frequency") {
        uint32_t khz = (uint32_t)((doc["freq_hz"] | (uint32_t)0) / 1000);
        uint32_t cur = ub.freqHz / 1000;
        motion = cur == 0 || absDiff(khz, cur) >= ULTRABEAM_DEADBAND_KHZ;
    } else if (action == "direction" || action == "mode") {
        motion = true;
    } else if (action == "band") {
        String b = doc["value"] | "";
        b.trim();
        for (const BandCentre& c : UB_BAND_CENTRES) {
            if (b != c.band) continue;
            uint32_t cur = ub.freqHz / 1000;
            motion = cur == 0 || absDiff(c.khz, cur) >= ULTRABEAM_DEADBAND_KHZ;
        }
        // unknown band: ultrabridge drops it — no motion
    }
    if (action == "retract") {
        motion = true;
        ubRetracted = true;
    } else if (motion) {
        ubRetracted = false;  // a command that drives the elements back out
    }
    if (motion) {
        ubCmdValid = true;
        ubCmdMs = now;
        ubSettleValid = true;
        ubSettleMs = now;
    }
}

inline void onAntSelectState(const uint8_t* payload, unsigned int len) {
    JsonDocument doc;
    if (deserializeJson(doc, payload, len)) return;
    antSel.target = doc["target"] | "";
}

inline void onPaState(const uint8_t* payload, unsigned int len) {
    static JsonDocument filter;
    static bool init = false;
    if (!init) {
        filter["device_online"] = true;
        filter["fault"] = true;
        filter["keyed"] = true;
        init = true;
    }
    JsonDocument doc;
    if (deserializeJson(doc, payload, len, DeserializationOption::Filter(filter))) return;
    pa.deviceOnline = doc["device_online"] | false;
    pa.fault = doc["fault"] | "";
    pa.keyed = doc["keyed"] | "";
    pa.stateSeen = true;
}

inline void onTunerState(const uint8_t* payload, unsigned int len) {
    static JsonDocument filter;
    static bool init = false;
    if (!init) {
        filter["device_online"] = true;
        filter["settling"] = true;
        init = true;
    }
    JsonDocument doc;
    if (deserializeJson(doc, payload, len, DeserializationOption::Filter(filter))) return;
    tuner.deviceOnline = doc["device_online"] | false;
    tuner.settling = doc["settling"] | true;
}

// dispatch routes one inbound message on the pa-arm connection. Returns false
// if the topic is not an arm input (the caller then treats it as pa-arm/cmd).
inline bool dispatch(const char* topic, const uint8_t* payload, unsigned int len) {
    unsigned long now = millis();
    if (!strcmp(topic, RADIO_BASE "/state")) onRadioState(payload, len, now);
    else if (!strcmp(topic, RADIO_BASE "/status")) radio.statusOnline = statusOnline(payload, len);
    else if (!strcmp(topic, ANT_SWITCH_BASE "/state")) onAntSwitchState(payload, len, now);
    else if (!strcmp(topic, ANT_SWITCH_BASE "/status")) antSw.statusOnline = statusOnline(payload, len);
    else if (!strcmp(topic, ANT_CTRL_BASE "/state")) onAntCtrlState(payload, len, now);
    else if (!strcmp(topic, ANT_CTRL_BASE "/status")) ub.statusOnline = statusOnline(payload, len);
    else if (!strcmp(topic, ANT_CTRL_BASE "/cmd")) onAntCtrlCmd(payload, len, now);
    else if (!strcmp(topic, ANT_SELECT_BASE "/state")) onAntSelectState(payload, len);
    else if (!strcmp(topic, ANT_SELECT_BASE "/status")) antSel.statusOnline = statusOnline(payload, len);
    else if (!strcmp(topic, PA_BASE "/state")) onPaState(payload, len);
    else if (!strcmp(topic, PA_BASE "/status")) pa.statusOnline = statusOnline(payload, len);
    else if (!strcmp(topic, TUNER_BASE "/state")) onTunerState(payload, len);
    else if (!strcmp(topic, TUNER_BASE "/status")) tuner.statusOnline = statusOnline(payload, len);
    else return false;
    return true;
}

// The input topics, subscribed in this order on every (re)connect.
static const char* const INPUT_TOPICS[] = {
    RADIO_BASE "/status",      RADIO_BASE "/state",
    ANT_SWITCH_BASE "/status", ANT_SWITCH_BASE "/state",
    ANT_CTRL_BASE "/status",   ANT_CTRL_BASE "/state",   ANT_CTRL_BASE "/cmd",
    ANT_SELECT_BASE "/status", ANT_SELECT_BASE "/state",
    PA_BASE "/status",         PA_BASE "/state",
    TUNER_BASE "/status",      TUNER_BASE "/state",
};
static const int INPUT_TOPICS_COUNT = sizeof(INPUT_TOPICS) / sizeof(INPUT_TOPICS[0]);

// ---------------------------------------------------------------------------
// gate evaluation
// ---------------------------------------------------------------------------
struct Eval {
    const char* hazard = nullptr;  // first failing HAZARD gate
    const char* ready = nullptr;   // first failing READINESS gate
    const char* fault = nullptr;   // first failing gate that is a fault (→ /state.error)
};

inline bool radioFresh(unsigned long now) {
    return radio.freshValid && (now - radio.freshMs) < RADIO_HEARTBEAT_MS;
}

inline Eval evaluate(unsigned long now) {
    // Expire one-shot windows once elapsed, so a millis() wrap (~49.7 days) can
    // never re-enter them.
    if (ubCmdValid && now - ubCmdMs >= ULTRABEAM_CMD_BLIND_MS) ubCmdValid = false;
    if (ubSettleValid && now - ubSettleMs >= ULTRABEAM_SETTLE_MS) ubSettleValid = false;
    if (bandEdgeValid && now - bandEdgeMs >= BAND_CHANGE_HOLDOFF_MS) bandEdgeValid = false;
    if (radio.freshValid && now - radio.freshMs >= RADIO_HEARTBEAT_MS) radio.freshValid = false;

    Eval e;
    auto hazard = [&](const char* r, bool isFault) {
        if (!e.hazard) e.hazard = r;
        if (isFault && !e.fault) e.fault = r;
    };
    auto ready = [&](const char* r, bool isFault) {
        if (!e.ready) e.ready = r;
        if (isFault && !e.fault) e.fault = r;
    };

    if (relayFault) hazard("relay mismatch", true);

    // radio
    if (!radio.statusOnline) hazard("radio offline", true);
    else if (!radioFresh(now)) hazard("radio feed stale", true);
    else if (!radio.deviceOnline) hazard("radio offline", true);
    else {
        if (radio.tuning) hazard("radio tuning", false);
        if (!bandSafe(radio.band)) hazard("band not safe", false);
    }

    // antenna switch
    if (!antSw.statusOnline || !antSw.stateSeen) ready("antenna switch offline", true);
    else if (antSw.selected.length() == 0 || antSw.selected == "off") hazard("antenna grounded", false);
    else if (!antSw.settled) hazard("antenna switching", false);

    // Ultrabeam — only while it is the antenna in circuit
    if (ultrabeamInCircuit()) {
        if (ubRetracted || (ub.stateSeen && ub.retracted)) hazard("ultrabeam retracted", false);
        if (ubCmdValid && (now - ubCmdMs) < ULTRABEAM_CMD_BLIND_MS) hazard("ultrabeam commanded", false);
        if (!ub.statusOnline || !ub.stateSeen || !ub.deviceOnline) {
            ready("ultrabeam offline", true);
        } else {
            if (ub.moving) hazard("ultrabeam moving", false);
            uint32_t ubKhz = ub.freqHz / 1000;
            if (ubKhz == 0 || absDiff(radio.freqHz / 1000, ubKhz) >= ULTRABEAM_DEADBAND_KHZ) {
                hazard("ultrabeam not tuned", false);
            }
        }
        if (ubSettleValid && (now - ubSettleMs) < ULTRABEAM_SETTLE_MS) ready("ultrabeam settling", false);
    }

    // PA (ACOM) — offline only blocks closing (barefoot operation is unaffected:
    // the arm relay sits in the PA key line only)
    // Live telemetry only: acombridge publishes device_online:true with an empty
    // fault/keyed when it opens the port, before the amp has said anything.
    if (!pa.statusOnline || !pa.stateSeen || !pa.deviceOnline || pa.fault.length() == 0 ||
        pa.keyed.length() == 0) {
        ready("pa offline", false);
    }
    else if (pa.fault != "none") hazard("pa fault", true);

    // ATU tune cycle (the tuner is optional: only a live, tuning ATR blocks)
    if (tuner.statusOnline && tuner.deviceOnline && tuner.settling) hazard("tuner tuning", false);

    // station still following a change
    if (bandEdgeValid && (now - bandEdgeMs) < BAND_CHANGE_HOLDOFF_MS) ready("band change settling", false);
    if (antSel.statusOnline && antSel.target.length() > 0 && antSw.stateSeen &&
        antSel.target != antSw.selected) {
        ready("antenna not converged", false);
    }
    return e;
}

// ---------------------------------------------------------------------------
// relay drive
// ---------------------------------------------------------------------------
// recompute evaluates the gates and drives relay 1. Returns the evaluation so
// the caller can publish the block reason it was decided on.
inline Eval recompute(bool enabled, unsigned long now) {
    Eval e = evaluate(now);
    bool gatesOk = !e.hazard && !e.ready;
    bool closeOk = enabled && !relayFault && gatesOk && radio.stateSeen && radio.rx;
    if (!closeOk) {
        closeOkValid = false;
    } else if (!closeOkValid) {
        closeOkValid = true;
        closeOkSinceMs = now;
    }

    bool want;
    if (!enabled || relayFault) {
        want = false;
    } else if (relayCmd) {
        // closed: hazards open at once; readiness opens only in RX
        bool keyed = radio.stateSeen && !radio.rx;
        want = !(e.hazard || (e.ready && !keyed));
    } else {
        // open: close only after permit + gates + RX held for the whole dwell
        want = closeOkValid && (now - closeOkSinceMs) >= ARM_CLOSE_DWELL_MS;
    }
    // Command edges write the relay; the close edge is the only place it is
    // ever energized (dwell + RX verified above).
    if (want != relayCmd) relaySet(RELAY_PA_ARM, want);
    relayCmd = want;

    // Verify every pass.
    bool rb = relayGet(RELAY_PA_ARM);
    if (relayCmd && !rb) {
        // Commanded closed but reads open: an expander reset/brownout, an OTA
        // start that opened it directly — or just one failed I²C read (M5Unified
        // returns 0 on NACK). Confirm over RELAY_DROP_CONFIRM passes before
        // acting, writing nothing meanwhile: acting on a glitch would hot-unkey
        // the PA mid-over, and a relay that really dropped is open already.
        // A confirmed drop is never re-closed here (that would skip dwell + RX).
        relayStablePasses = 0;
        if (++relayDropPending >= RELAY_DROP_CONFIRM) {
            relayDropPending = 0;
            relayCmd = false;
            closeOkValid = false;
            relaySet(RELAY_PA_ARM, false);
            rb = relayGet(RELAY_PA_ARM);
            relayOkCount = 0;
            if (relayDropCount < 255) relayDropCount++;
            if (relayDropCount >= RELAY_MISMATCH_TRIP) relayFault = true;
        } else {
            rb = true;  // unconfirmed: report what is commanded
        }
    } else if (!relayCmd && rb) {
        // Commanded open but reads closed: rewrite the opening at once.
        relayDropPending = 0;
        relayStablePasses = 0;
        relaySet(RELAY_PA_ARM, false);
        rb = relayGet(RELAY_PA_ARM);
        if (rb) {
            relayOkCount = 0;
            if (relayMismatchCount < 255) relayMismatchCount++;
            if (relayMismatchCount >= RELAY_MISMATCH_TRIP) relayFault = true;
        }
    } else {
        relayDropPending = 0;
        relayMismatchCount = 0;
        if (relayFault && ++relayOkCount >= RELAY_MISMATCH_CLEAR) {
            relayOkCount = 0;
            relayFault = false;
        }
        // Drops are forgiven only after the relay has held closed for a while,
        // so a dead expander (every close drops) keeps re-tripping the fault.
        if (relayCmd && relayStablePasses < 65535 && ++relayStablePasses >= RELAY_STABLE_PASSES) {
            relayDropCount = 0;
        }
    }
    relayReadback = rb;
    return e;
}

// blockReason is why the arm is (or is about to go) open; "" when armed and clear.
inline const char* blockReason(const Eval& e, bool enabled) {
    if (e.hazard) return e.hazard;
    if (e.ready) return e.ready;
    if (!enabled) return "permit off";
    if (!relayReadback) return (radio.stateSeen && radio.rx) ? "settling" : "waiting for rx";
    return "";
}

}  // namespace arm
