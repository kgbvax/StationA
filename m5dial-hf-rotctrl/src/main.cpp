// main.cpp — M5Stack Dial HF-rotator control head (m5dial-hf-rotctrl).
//
// A physical analog control head for the station's HF rotator slot
// (muehle/hf/rotator, Yaesu G-450DC via the wrc-rotator-bridge on shari).
//
// Classification (integration model §9 / PRD 02-interface-spec R1.6): a
// consumer + /cmd stimulator, NOT a slot. No /meta, /state, /status of its
// own, no LWT, no heartbeat. It subscribes the rotator's /state + /status and
// publishes /cmd. Delete it and the station runs identically.
//
// Behavior:
//   - Face: fixed 0..360° compass card, damped needle (real meter sweep),
//     red target pointer, liveness ring/badge (two layers: bridge /status AND
//     /state.device_online — a parked rotator is silent by design, /state is
//     change-deduped upstream, so silence is never read as staleness). Azimuth
//     WRAPS on the display (az 390 renders at 30°); the command space stays
//     linear 0..450 — the WRC's real range — so the knob never commands a
//     spin, and a "+360" hint marks the overlap pass.
//   - Knob: each detent steps the local target by 22.5° — one knob-flat, one
//     compass step (the encoder has 16 detents per revolution) — and publishes
//     a set_az. The needle chases your hand. Detents are IGNORED while
//     liveness is down: queuing unobservable motion commands while blind is
//     worse than ignoring the input.
//   - Press: stop. Any hold duration; always safe, published whenever MQTT is
//     connected (and acknowledged by a short beep only when it publishes).
//   - Publishing is coalesced (the bridge's /cmd queue is 8 deep and drops
//     silently) and NEVER replayed across a reconnect (stale set_az replay
//     can spin the antenna).
//   - Wireless OTA is wired into this, the first image: after the single
//     USB first flash, every update is over the network (the SIM_MODE desk
//     build is the one exception — it has no radio, so USB is the only way
//     in and back out). An OTA start publishes stop first — the antenna
//     keeps moving through a Dial reboot otherwise, and the operator would
//     be blind while the head is down.
//
// Wire contract details: see docs/m5dial-hf-rotctrl-mqtt-api.md.
//
// SIM_MODE build (m5dial-rotctrl1-sim env): the station side of the wire is
// replaced by sim.h — a local mast model with no WiFi, MQTT or OTA. The
// model emits the same /state JSON through the same parser, and takes
// set_az/stop the way the bridge does, so all face/knob logic under test is
// the production code. The face shows a SIM label so a desk face is never
// mistaken for the live antenna.
#include <Arduino.h>
#include <ArduinoJson.h>
#include <ArduinoOTA.h>
#include <M5Dial.h>
#include <WiFi.h>

#include "config.h"
#include "secrets.h"

#include "face.h"
#include "knob.h"
#include "mqtt_client.h"
#include "sim.h"

// --- state ------------------------------------------------------------------

struct RotState {
    bool haveState = false;    // at least one /state received
    bool statusSeen = false;   // at least one /status received
    bool bridgeOnline = false; // /status == "online" (bridge process LWT)
    bool deviceOnline = false; // /state.device_online (WRC websocket link)
    bool moving = false;
    float az = 0.0f;
    float targetAz = 0.0f;     // bridge-reported target
};
static RotState rot;

static Face face;
static Knob knob;

// Forward declarations for the OTA callback.
static void publishStop();

// The /state and /status application paths — shared by the MQTT transport
// and the SIM_MODE model, so the sim exercises the production state machine.
static void applyRotatorState(const char* payload, unsigned int len);
static void applyStatus(const char* text);

#if SIM_MODE
static void onSimState(const char* json, unsigned int len);
static SimRotator sim(onSimState);
#else
static void onMqttConnect();
static void onMqttDisconnect();
static void onMqttMessage(const char* topic, const uint8_t* payload, unsigned int len);
static DialMqtt mqtt(MQTT_CLIENT_ID, onMqttMessage, onMqttConnect, onMqttDisconnect);
#endif

// Knob/publish state.
static float localTarget = 0.0f;  // where the operator last pointed
static bool targetValid = false;   // initialized from a real /state
static bool targetDirty = false;  // pending set_az (coalescer owns it)
static bool stopResyncPending = false;  // a stop was sent; the halt /state re-baselines
static unsigned long lastDetentMs = 0;
static unsigned long lastPublishMs = 0;
static unsigned long lastWifiRetryMs = 0;
static bool wifiScanned = false;  // one-shot scan dump while associating fails

// Face state.
static float displayAz = 0.0f;     // damped needle position
static bool faceDirty = true;
static unsigned long lastFrameMs = 0;
static unsigned long lastPaintMs = 0;

static char msgBuf[512];  // scratch for one inbound payload

// --- helpers -------------------------------------------------------------------

static void logf(const char* fmt, ...) {
    char line[96];
    va_list ap;
    va_start(ap, fmt);
    vsnprintf(line, sizeof(line), fmt, ap);
    va_end(ap);
    Serial.println(line);
}

#if BEEP_ENABLED
static void beep(uint32_t ms) { M5Dial.Speaker.tone(BEEP_FREQ_HZ, ms); }
#else
static void beep(uint32_t) {}
#endif

// The transport is up: MQTT in the live build, always in the SIM build.
static bool commUp() {
#if SIM_MODE
    return true;
#else
    return mqtt.connected();
#endif
}

// Full liveness: all three layers. statusSeen guards against trusting a
// stale retained "online" before the first /status replay.
static bool bridgeUp() {
    return commUp() && rot.statusSeen && rot.bridgeOnline;
}
static bool livenessOk() { return bridgeUp() && rot.haveState && rot.deviceOnline; }

// --- MQTT publish ------------------------------------------------------------------

// The rotator's declared value key is "az" (pre-convention deviation declared
// in the slot's /meta.expose; see wrc-rotator-bridge) — NOT "value".
// NOT retained: one-shot intent, QoS 0 (PubSubClient cannot publish above
// QoS 0; a lost final flush is visible as TGT-vs-needle mismatch on the face).
static void publishSetAz(float az) {
#if SIM_MODE
    // /cmd enters the local mast model instead of the broker.
    logf("[sim] set_az %.1f", az);
    sim.onSetAz(az);
    lastPublishMs = millis();  // the coalescer runs in sim too
#else
    JsonDocument doc;
    doc["action"] = "set_az";
    doc["az"] = az;
    char buf[64];
    serializeJson(doc, buf, sizeof(buf));
    if (mqtt.publish(ROTATOR_CMD_TOPIC, buf)) {
        lastPublishMs = millis();
    } else {
        logf("[mqtt] set_az publish failed");
    }
#endif
}

static void publishStop() {
#if SIM_MODE
    logf("[sim] stop");
    sim.onStop();
#else
    if (!mqtt.connected()) return;  // best-effort by design
    if (!mqtt.publish(ROTATOR_CMD_TOPIC, "{\"action\":\"stop\"}")) {
        logf("[mqtt] stop publish failed");
    }
#endif
}

// Coalesce knob-detent targets onto the bridge's 8-deep /cmd queue: first
// detent of a burst goes out immediately, while turning at most one publish
// per PUBLISH_MIN_INTERVAL_MS with the latest target, and a final flush
// PUBLISH_FLUSH_MS after the last detent. A pending target is DROPPED when
// the MQTT link goes down — never replay a cached set_az across a reconnect.
static void coalescePublish() {
    if (!targetDirty) return;
    if (!commUp()) {
        targetDirty = false;  // no replay across disconnects
        return;
    }
    unsigned long now = millis();
    bool burstActive = (now - lastDetentMs) < PUBLISH_FLUSH_MS;
    if (!burstActive) {
        publishSetAz(localTarget);  // final flush of the burst
        targetDirty = false;
        return;
    }
    if (now - lastPublishMs >= PUBLISH_MIN_INTERVAL_MS) {
        publishSetAz(localTarget);
    }
}

// --- MQTT callbacks (live build) / sim delivery (SIM_MODE build) ------------------

#if !SIM_MODE
static void onMqttConnect() {
    // Subscribing replays the retained /state + /status: the boot-time feed.
    mqtt.subscribe(ROTATOR_STATE_TOPIC);
    mqtt.subscribe(ROTATOR_STATUS_TOPIC);
    logf("[mqtt] connected, subscribed rotator state+status");
}

// Disconnect edge: drop ALL liveness knowledge. Without this, a reconnect
// trades on stale pre-disconnect flags (bridgeOnline/deviceOnline) until the
// retained replay lands — detents would be accepted against a possibly-dead
// slot. After this, the face shows NO DATA until the replay re-establishes
// /status + /state, mirroring the boot semantics.
static void onMqttDisconnect() {
    rot.statusSeen = false;
    rot.haveState = false;
    stopResyncPending = false;  // a stale stop must not hijack a later resync
    faceDirty = true;
}
#endif  // !SIM_MODE

static void applyStatus(const char* text) {
    rot.statusSeen = true;
    rot.bridgeOnline = (strcmp(text, "online") == 0);
    faceDirty = true;
}

// The /state application — the production state machine. Both transports
// end here: the MQTT handler from the bridge, the sim model from sim.h.
static void applyRotatorState(const char* payload, unsigned int len) {
    JsonDocument doc;
    if (deserializeJson(doc, payload, len) != DeserializationError::Ok) {
        logf("[state] bad /state JSON, ignored");
        return;
    }
    {
        bool wasMoving = rot.moving;
        rot.az = doc["az"] | rot.az;
        if (!doc["target_az"].isNull()) rot.targetAz = doc["target_az"].as<float>();
        rot.moving = doc["moving"] | false;
        rot.deviceOnline = doc["device_online"] | false;
        rot.haveState = true;
        faceDirty = true;

        // Target init/re-sync: first /state initializes the target; while the
        // operator is idle, the bridge-reported target wins (console-initiated
        // moves become the new baseline); while the operator is turning, the
        // incoming target is ignored so the two writers never fight.
        //
        // Stop exception: after a stop the HALT /state carries the true stop
        // point, and it must re-baseline the target even inside the turning
        // window (a press right after turning is the normal stop case). The
        // press only set a provisional target from the last-reported az; the
        // antenna coasts on past that. Uses az, NOT target_az — the WRC keeps
        // reporting the cancelled target as tdeg after a halt — so the idle
        // re-sync below must NOT overwrite it on the same message
        // (resyncedFromStop guards exactly that; otherwise a halt /state
        // landing after the turning window restores the cancelled target).
        bool resyncedFromStop = false;
        if (stopResyncPending && !rot.moving) {
            localTarget = rot.az;
            targetValid = true;
            stopResyncPending = false;
            resyncedFromStop = true;
        }
        bool turning = (millis() - lastDetentMs) < TARGET_RESYNC_IDLE_MS;
        if (!resyncedFromStop && (!targetValid || !turning)) {
            localTarget = doc["target_az"].isNull() ? rot.az : rot.targetAz;
            targetValid = true;
        }

        // The arrival beep: motion ended with the needle on target. NOT
        // sounded when this halt is our own stop landing (resyncedFromStop):
        // the re-baseline above just pulled the target onto the az, so the
        // check would otherwise claim "arrived" for a halt tens of degrees
        // short of the old target.
        if (wasMoving && !rot.moving && !resyncedFromStop &&
            fabsf(rot.az - localTarget) < ARRIVE_BEEP_TOLERANCE_DEG) {
            beep(BEEP_ARRIVE_MS);
        }
    }
}

#if SIM_MODE
// The simulated /state enters through the production parser — the sim
// tests the same state machine the bridge talks to.
static void onSimState(const char* json, unsigned int len) {
    applyRotatorState(json, len);
}
#else
// MQTT transport: dispatch by absolute topic into the shared apply paths.
static void onMqttMessage(const char* topic, const uint8_t* payload,
                           unsigned int len) {
    // PubSubClient payloads are not null-terminated; clamp+copy.
    unsigned int l = (len < sizeof(msgBuf) - 1) ? len : (unsigned int)(sizeof(msgBuf) - 1);
    memcpy(msgBuf, payload, l);
    msgBuf[l] = '\0';

    if (strcmp(topic, ROTATOR_STATUS_TOPIC) == 0) {
        applyStatus(msgBuf);
        return;
    }
    if (strcmp(topic, ROTATOR_STATE_TOPIC) == 0) {
        applyRotatorState(msgBuf, l);
    }
}
#endif  // !SIM_MODE

// --- knob ---------------------------------------------------------------------------

static void handleKnob() {
    int detents = knob.pollDetents();
    // Bench telemetry (like the wifi reason codes): with edge-driven
    // counting, one knob-flat is exactly ENCODER_COUNTS_PER_DETENT raw counts
    // — every flat, even while the face animates. If raw shows up but the
    // detent count lags, the divisor is wrong for this hardware; if raw
    // itself undercounts a flat, edges are being lost again.
    if (knob.lastRaw() != 0) {
        logf("[knob] raw=%d detents=%d", knob.lastRaw(), detents);
    }
    if (detents != 0) {
        lastDetentMs = millis();
        faceDirty = true;
        // Fresh operator input outranks any pending stop re-baseline: the
        // halt /state that arrives later must not clobber this new target.
        stopResyncPending = false;
        if (livenessOk()) {
            localTarget = constrain(localTarget + (float)detents * DETENT_DEG,
                                    AZ_MIN, AZ_MAX);
            targetDirty = true;  // coalescePublish owns the send
        } else {
            // Blind: do not queue motion commands we cannot observe.
            logf("[knob] detents ignored (link down)");
        }
    }

    if (knob.pollPress()) {
        // Flush any pending target FIRST, then stop, so the stop never
        // lands behind our own queued move. Then re-sync to where the
        // antenna actually is: the next detent steps from the halt point.
        // rot.az is only the PRE-halt azimuth (the antenna coasts on past
        // it), so this is provisional — the halt /state (moving:false)
        // carries the true stop point and re-baselines via
        // stopResyncPending in applyRotatorState.
        //
        // Press = the one safety gesture, so it gets the one acknowledgment:
        // the beep confirms the stop was PUBLISHED. With MQTT down nothing
        // is published and nothing changes on the face — a silent re-baseline
        // would drag the target pointer back while the antenna still runs
        // toward the old command.
        if (targetDirty && commUp()) {
            publishSetAz(localTarget);
            targetDirty = false;
        }
        if (commUp()) {
            publishStop();
            // Latch the re-baseline ONLY if motion was in flight: the halt
            // /state that consumes the latch only comes when something was
            // moving (a parked stop changes nothing on the wire — the
            // change-deduped bridge stays silent). A latch with no halt to
            // consume it would sit armed until some LATER arrival consumes
            // it and wrongly suppresses that arrival's beep.
            stopResyncPending = rot.moving;
            localTarget = rot.az;
            targetValid = rot.haveState;
            beep(BEEP_STOP_ACK_MS);
        } else {
            logf("[knob] press ignored (MQTT down)");
        }
        faceDirty = true;
    }
}

// --- face ---------------------------------------------------------------------------

static void handleFace() {
    unsigned long now = millis();
    bool needleSettling = fabsf(displayAz - rot.az) > NEEDLE_DEADBAND_DEG;

    if (!faceDirty && !needleSettling &&
        now - lastPaintMs < QUIESCENT_REFRESH_MS) {
        return;  // parked, nothing changed, freshly painted
    }
    if (now - lastFrameMs < FRAME_MS) return;  // frame-rate gate (~30 Hz)
    lastFrameMs = now;

    if (needleSettling) {
        // Exponential damping per frame — the meter-movement feel.
        displayAz += (rot.az - displayAz) * NEEDLE_K;
        if (fabsf(rot.az - displayAz) <= NEEDLE_DEADBAND_DEG) displayAz = rot.az;
    } else {
        displayAz = rot.az;  // snapped: never drift below the deadband
    }

    FaceModel m;
    m.az = rot.az;
    m.displayAz = displayAz;
    m.target = localTarget;
    m.moving = rot.moving;
    m.linkOk = livenessOk();
    m.bridgeUp = bridgeUp();
    m.haveState = rot.haveState;
    m.sim = SIM_MODE;
    face.render(m);
    faceDirty = false;
    lastPaintMs = now;
}

// --- wifi + OTA -----------------------------------------------------------------------

static void wifiConnect() {
    WiFi.mode(WIFI_STA);
    WiFi.setSleep(false);  // reliability over power: this runs on mains
    // The stack's own auto-reconnect retries association back-to-back, and
    // each attempt is a full active scan of all channels — with the AP out
    // of reach that is a 100% radio duty cycle and the device heats up.
    // Our throttled retry below (one attempt per WIFI_RECONNECT_MS) is the
    // only reconnect driver.
    WiFi.setAutoReconnect(false);
    // The drop reason turns a cycling status code into a one-line answer:
    // 201 = AP not found in scan, 15 = 4-way handshake timeout (wrong
    // password), 2 = auth expired, 202 = association failed.
    WiFi.onEvent([](WiFiEvent_t, WiFiEventInfo_t info) {
        logf("[wifi] dropped, reason=%d", info.wifi_sta_disconnected.reason);
    }, ARDUINO_EVENT_WIFI_STA_DISCONNECTED);
    WiFi.begin(WIFI_SSID, WIFI_PASSWORD);
    logf("[wifi] connecting to %s ...", WIFI_SSID);
}

static void otaInit() {
    // Wireless OTA is a first-class v1 feature: the very first flashed image
    // already carries the OTA listener, so only the USB first flash is wired.
    // (The m5stamp in the field runs pre-OTA firmware and its next update
    // needs physical USB — that lesson is why this exists on day one.)
    ArduinoOTA.setHostname(DEVICE_SERIAL);  // → m5dial-rotctrl-1.local
    ArduinoOTA.setPassword(OTA_PASSWORD);
    ArduinoOTA.onStart([]() {
        // Halt the rotator before the update: motion runs in the bridge/WRC,
        // independent of this head, and a reboot would leave the operator
        // blind while the antenna keeps moving. Best-effort — if MQTT is
        // down no stop goes out (operator rule: do not update during motion
        // with the link down).
        publishStop();
        logf("[ota] update starting: stop published");
    });
    ArduinoOTA.onEnd([]() { logf("[ota] update done, rebooting"); });
    ArduinoOTA.begin();
    logf("[ota] listening as %s.local", DEVICE_SERIAL);
}

// --- setup / loop --------------------------------------------------------------------------

void setup() {
    // HWCDC trap: M5Unified's default config has serial_baudrate = 0, so
    // M5.begin() never calls Serial.begin — and with ARDUINO_USB_MODE=1 the
    // core's boot-time auto-begin does not apply either (it is for the
    // soft-CDC path only). Without this explicit begin, HWCDC's TX ring
    // buffer stays NULL and every log write silently returns 0 — the ROM
    // banner is then the only thing a host ever sees. First in setup, so
    // every line below it reaches the monitor.
    Serial.begin(115200);
    // begin(enableEncoder): the M5Dial library leaves the encoder DISABLED by
    // default — passing true is load-bearing, or the knob does nothing.
    M5Dial.begin(true);
    M5Dial.Display.setBrightness(80);

    bool spriteOk = face.init(M5Dial.Display);
    logf("[dial] up, heap=%u, sprite=%s",
         (unsigned)ESP.getFreeHeap(), spriteOk ? "ok" : "FALLBACK direct");

    knob.begin();
#if SIM_MODE
    // The desk build boots self-contained: the mast model starts at az 0 and
    // the bridge is "online" by definition — the face goes green with no
    // network at all. The boot state feeds through the same emit/parse path
    // the live bridge uses.
    sim.begin();
    applyStatus("online");
    logf("[sim] up: local mast model, %.1f deg/s, no WiFi/MQTT/OTA",
         SIM_ROTATOR_DEG_PER_SEC);
#else
    wifiConnect();
    otaInit();
#endif
}

void loop() {
    M5Dial.update();  // first: input works even with WiFi/MQTT down

    handleKnob();        // press = stop works whenever comm is up
    coalescePublish();   // drops the pending target if comm just dropped
#if SIM_MODE
    sim.step();          // the station side of the wire, locally
#else
    unsigned long now = millis();
    if (WiFi.status() != WL_CONNECTED &&
        now - lastWifiRetryMs >= WIFI_RECONNECT_MS) {
        lastWifiRetryMs = now;
        // Status code turns a silent face diagnosis into a one-line answer:
        // 1 = SSID not in range, 4 = association failed, 6 = disconnected.
        logf("[wifi] not connected, status=%d (retrying)", WiFi.status());
        // One-shot dump after ~3 failed cycles: what the radio actually hears
        // from this spot answers "wrong SSID?" vs "out of range?" directly on
        // the monitor — no other tool needed on the bench.
        static int wifiFailCycles = 0;
        if (!wifiScanned && ++wifiFailCycles >= 3) {
            wifiScanned = true;
            // A scan while a connect attempt is in flight fails with -2
            // (radio busy) — cancel it first, then dwell longer per channel
            // than the default so weak beacons get a fair chance.
            WiFi.disconnect();
            delay(100);
            int n = WiFi.scanNetworks(false, false, false, 300);
            logf("[wifi] scan: %d networks visible", n);
            for (int i = 0; i < n; i++) {
                logf("[wifi]  ch%d %+ddBm %s", WiFi.channel(i), WiFi.RSSI(i),
                     WiFi.SSID(i).c_str());
            }
            WiFi.scanDelete();
        }
        WiFi.reconnect();
    }

    mqtt.loop();          // throttled reconnect while down
    ArduinoOTA.handle(); // network updates after the USB first flash
#endif
    handleFace();        // frame-gated repaint

    delay(LOOP_DELAY_MS);
}