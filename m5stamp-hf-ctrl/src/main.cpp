// main.cpp — M5 Stamp PLC #1 firmware (m5stamp-hf-ctrl).
//
// A compound embedded node: one M5 Stamp PLC publishes TWO station
// integration-model slots that share device{model,serial} (model §3):
//
//   muehle/hf/switch   — PA + TRX remote-on relays (relays 3 & 4). Read-write.
//   muehle/hf/pa-arm   — the PA-enable arm relay (relay 1), with EMBEDDED arm
//                        logic (fail-safe-open, heartbeat-driven). /cmd only
//                        sets the `enabled` permit; `armed` is derived.
//
// The arm logic is the model's planned embedded safety node (§6, §11.3): the
// firmware subscribes hf/radio/state and hf/ant-switch/state and computes
//   armed = enabled ∧ radio_online ∧ ¬radio.tuning ∧ band_safe ∧ heartbeat ∧ antenna_ready
// driving relay 1 so that ANY failure (radio offline, tuning, unsafe band,
// heartbeat lost, antenna grounded, 13.8 V lost, PLC crash) drops the relay
// open → PA disabled.
//
// See docs/m5stamp-hf-ctrl-mqtt-api.md and
// ../stationa/docs/station-integration-model.md §7.1.

#include <Arduino.h>
#include <ArduinoJson.h>
#include <WiFi.h>
#include <ArduinoOTA.h>

#include "secrets.h"   // gitignored — see secrets.example.h
#include "config.h"
#include "relay.h"     // uses the library global M5StamPLC
#include "mqtt_slot.h"

// ---------------------------------------------------------------------------
// switch slot state (relays 3 & 4 → channels 2 & 3)
// ---------------------------------------------------------------------------
struct SwitchState {
    bool pa;   // PA remote-on relay position (read back)
    bool trx;  // TRX remote-on relay position (read back)
};
SwitchState switchState{false, false};

// Slot instances are defined later (before setup); forward-declare the pointers
// so command handlers can publish immediately after driving a relay.
extern SlotMqtt* gSwitch;
extern SlotMqtt* gPaArm;

// ---------------------------------------------------------------------------
// pa-arm slot state (relay 1 → channel 0) + arm logic inputs
// ---------------------------------------------------------------------------
bool enabled = false;        // software arm-permit (last /cmd set_enabled)
bool armed = false;          // derived actual relay state
// radio inputs from hf/radio/state:
bool radioOnline = false;
bool radioTuning = false;
String radioBand = "";       // canonical band name (e.g. "20m"), "" if unknown
unsigned long lastRadioStateMs = 0;  // heartbeat freshness
bool antennaReady = false;    // ant-switch/state.selected != "off" (an antenna is in circuit)

// Deduplication: publish immediately when enabled/armed/error change, otherwise
// only on heartbeat. lastPublishedEnabled/Armed/Error track the last snapshot we
// actually sent.
bool lastPublishedEnabled = false;
bool lastPublishedArmed = false;
String lastPublishedError = "";

// ---------------------------------------------------------------------------
// forward decls
// ---------------------------------------------------------------------------
static void publishSwitchMeta(SlotMqtt& s);
static void publishSwitchState(SlotMqtt& s);
static void publishSwitchCmd(const char* action, const char* value);
static void publishPaArmMeta(SlotMqtt& s);
static void publishPaArmState(SlotMqtt& s);
static void handleSwitchCmd(const char* topic, const uint8_t* payload, unsigned int len);
static void handlePaArmCmd(const char* topic, const uint8_t* payload, unsigned int len);
static void handleRadioState(const char* topic, const uint8_t* payload, unsigned int len);
static void handleAntSwitchState(const char* topic, const uint8_t* payload, unsigned int len);
static bool bandSafe(const String& band);
static bool recomputeArm(SlotMqtt& paArm);
static void uiInit();
static void uiRender();
static void handleButtons();

// ---------------------------------------------------------------------------
// JSON helpers
// ---------------------------------------------------------------------------
// Small static docs are fine within 1024-byte buffers (set in SlotMqtt).

static bool parseCmd(const uint8_t* payload, unsigned int len, String& action, String& value) {
    JsonDocument doc;
    DeserializationError err = deserializeJson(doc, payload, len);
    if (err) return false;
    action = doc["action"] | "";
    value = doc["value"] | "";
    return true;
}

// ---------------------------------------------------------------------------
// switch slot
// ---------------------------------------------------------------------------
static void applySwitchRelays() {
    relaySet(RELAY_PA_REMOTE, switchState.pa);
    relaySet(RELAY_TRX_REMOTE, switchState.trx);
    // read back
    switchState.pa = relayGet(RELAY_PA_REMOTE);
    switchState.trx = relayGet(RELAY_TRX_REMOTE);
}

static void handleSwitchCmd(const char* topic, const uint8_t* payload, unsigned int len) {
    String action, value;
    if (!parseCmd(payload, len, action, value)) return;
    if (action == "set_pa") {
        switchState.pa = (value == "on");
        applySwitchRelays();
    } else if (action == "set_trx") {
        switchState.trx = (value == "on");
        applySwitchRelays();
    } else {
        return;  // unknown action — drop
    }
    // Publish the readback state for this slot immediately after a command.
    if (gSwitch && gSwitch->connected()) publishSwitchState(*gSwitch);
}

// ---------------------------------------------------------------------------
// pa-arm slot
// ---------------------------------------------------------------------------
// The paArmSlot connection subscribes its own /cmd AND the radio + ant-switch
// /state feeds (for arm logic). All arrive through the same PubSubClient
// callback, so dispatch by topic suffix here: */cmd → set_enabled;
// */radio/state → radio inputs; */ant-switch/state → antenna_ready.
static void handlePaArmCmd(const char* topic, const uint8_t* payload, unsigned int len) {
    String t(topic);
    if (t.endsWith("/radio/state")) {
        handleRadioState(topic, payload, len);
        return;
    }
    if (t.endsWith("/ant-switch/state")) {
        handleAntSwitchState(topic, payload, len);
        return;
    }
    // /cmd path
    String action, value;
    if (!parseCmd(payload, len, action, value)) return;
    if (action == "set_enabled") {
        enabled = (value == "true");
        // armed recomputed in main loop; relay driven from armed.
    }
}

static void handleRadioState(const char* topic, const uint8_t* payload, unsigned int len) {
    JsonDocument doc;
    DeserializationError err = deserializeJson(doc, payload, len);
    if (err) return;
    // radio liveness: device_online true means the radio (flexbridge) is up.
    // tuning and band drive the arm logic.
    radioOnline = doc["device_online"] | false;
    radioTuning = doc["tuning"] | false;
    const char* b = doc["band"];
    radioBand = b ? String(b) : String("");
    lastRadioStateMs = millis();
}

static void handleAntSwitchState(const char* topic, const uint8_t* payload, unsigned int len) {
    JsonDocument doc;
    DeserializationError err = deserializeJson(doc, payload, len);
    if (err) return;
    // antennaReady = an antenna is actually in circuit (not the grounded "off"
    // position). Conservative: unknown/missing selected drops the arm.
    String sel = doc["selected"] | "";
    antennaReady = (sel.length() > 0 && sel != "off");
}

static bool bandSafe(const String& band) {
    if (band.length() == 0) return false;
    for (int i = 0; i < SAFE_BANDS_COUNT; ++i) {
        if (band == SAFE_BANDS[i]) return true;
    }
    return false;
}

// radioHeartbeatFresh reports whether a radio/state message arrived within
// RADIO_HEARTBEAT_MS (and at all). Shared by recomputeArm and currentPaArmError
// so the relay decision and the reported error can never disagree. With no
// fresh heartbeat the radio-derived inputs (device_online, tuning, band) are
// a frozen snapshot and must not be trusted (§10).
static bool radioHeartbeatFresh() {
    return lastRadioStateMs != 0 && (millis() - lastRadioStateMs) < RADIO_HEARTBEAT_MS;
}

// recomputeArm evaluates the arm condition and drives relay 1. Fail-safe-open:
// the relay is energized ONLY when every condition holds; any failure drops it
// open. Returns whether `armed` changed.
static bool recomputeArm(SlotMqtt& paArm) {
    bool newArmed = enabled && radioOnline && !radioTuning && bandSafe(radioBand)
                 && radioHeartbeatFresh() && antennaReady;
    if (newArmed != armed) {
        armed = newArmed;
        relaySet(RELAY_PA_ARM, armed);  // energize on arm, de-energize (open) on any drop
        return true;
    }
    return false;
}

// ---------------------------------------------------------------------------
// /meta + /state publishers
// ---------------------------------------------------------------------------
static void publishSwitchMeta(SlotMqtt& s) {
    JsonDocument doc;
    doc["schema"] = "1.0";
    doc["role"] = "switch";
    JsonObject dev = doc["device"].to<JsonObject>();
    dev["model"] = DEVICE_MODEL;
    dev["serial"] = DEVICE_SERIAL;
    doc["link"] = "wifi";
    doc["host"] = "embedded";
    JsonObject caps = doc["capabilities"].to<JsonObject>();
    JsonArray chans = caps["channels"].to<JsonArray>();
    chans.add("pa"); chans.add("trx");
    caps["exclusive"] = false;
    JsonObject kind = caps["kind"].to<JsonObject>();
    kind["pa"] = "remote_on";
    kind["trx"] = "remote_on";
    JsonObject rmap = caps["relay_map"].to<JsonObject>();
    rmap["pa"] = 3;
    rmap["trx"] = 4;
    JsonObject expose = doc["expose"].to<JsonObject>();
    JsonObject edev = expose["device"].to<JsonObject>();
    edev["name"] = DEVICE_MODEL;
    edev["model"] = DEVICE_MODEL;
    edev["manufacturer"] = "M5Stack";
    JsonArray fields = expose["fields"].to<JsonArray>();
    auto addField = [&](const char* key, const char* name) {
        JsonObject f = fields.add<JsonObject>();
        f["key"] = key;
        f["name"] = name;
        f["type"] = "enum";
        JsonArray opts = f["options"].to<JsonArray>();
        opts.add("on"); opts.add("off");
        f["writable"] = true;
        JsonObject cmd = f["command"].to<JsonObject>();
        cmd["action"] = String("set_") + key;
        cmd["value_key"] = "value";
        cmd["value_type"] = "string";
    };
    addField("pa", "PA remote-on");
    addField("trx", "TRX remote-on");
    String out;
    serializeJson(doc, out);
    s.publish("meta", true, out.c_str());
}

static void publishSwitchState(SlotMqtt& s) {
    JsonDocument doc;
    doc["ts"] = millis();  // uptime ms (no RTC; monotonic freshness marker)
    doc["pa"] = switchState.pa ? "on" : "off";
    doc["trx"] = switchState.trx ? "on" : "off";
    doc["device_online"] = true;  // the PLC is up if it's publishing
    String out;
    serializeJson(doc, out);
    s.publish("state", true, out.c_str());
}

static void publishSwitchCmd(const char* action, const char* value) {
    if (!gSwitch || !gSwitch->connected()) return;
    JsonDocument doc;
    doc["action"] = action;
    doc["value"] = value;
    String out;
    serializeJson(doc, out);
    gSwitch->publish("cmd", true, out.c_str());
}

static void publishPaArmMeta(SlotMqtt& s) {
    JsonDocument doc;
    doc["schema"] = "1.0";
    doc["role"] = "pa-arm";
    JsonObject dev = doc["device"].to<JsonObject>();
    dev["model"] = DEVICE_MODEL;  // SAME device as hf/switch (compound, §3)
    dev["serial"] = DEVICE_SERIAL;
    doc["link"] = "wifi";
    doc["host"] = "embedded";
    JsonObject caps = doc["capabilities"].to<JsonObject>();
    caps["fail_safe"] = "open";
    caps["heartbeat"] = true;
    caps["relay"] = 1;
    JsonObject expose = doc["expose"].to<JsonObject>();
    JsonObject edev = expose["device"].to<JsonObject>();
    edev["name"] = DEVICE_MODEL;
    edev["model"] = DEVICE_MODEL;
    edev["manufacturer"] = "M5Stack";
    JsonArray fields = expose["fields"].to<JsonArray>();
    {  // armed — read-only
        JsonObject f = fields.add<JsonObject>();
        f["key"] = "armed";
        f["name"] = "Armed";
        f["type"] = "boolean";
        f["writable"] = false;
    }
    {  // enabled — writable permit
        JsonObject f = fields.add<JsonObject>();
        f["key"] = "enabled";
        f["name"] = "Enabled (arm permit)";
        f["type"] = "boolean";
        f["writable"] = true;
        JsonObject cmd = f["command"].to<JsonObject>();
        cmd["action"] = "set_enabled";
        cmd["value_key"] = "value";
        cmd["value_type"] = "boolean";
    }
    String out;
    serializeJson(doc, out);
    s.publish("meta", true, out.c_str());
}

// currentPaArmError returns the human-readable reason the arm is blocked, or
// an empty string when there is no error. Heartbeat staleness is reported
// right after "radio offline": the tuning/band inputs below are only as fresh
// as the last radio/state message, so a stale feed outranks them — and it must
// be surfaced, because the arm drops on staleness alone while the radio inputs
// themselves may all still look healthy (this was a silent failure before;
// flexbridge now heartbeats /state, so staleness means a real feed problem).
static String currentPaArmError() {
    if (!radioOnline) return "radio offline";
    if (!radioHeartbeatFresh()) return "radio feed stale";
    if (radioTuning) return "radio tuning";
    if (!bandSafe(radioBand)) return "band not safe";
    if (!antennaReady) return "antenna grounded";
    return "";
}

static void publishPaArmState(SlotMqtt& s) {
    JsonDocument doc;
    doc["ts"] = millis();
    doc["enabled"] = enabled;
    doc["armed"] = armed;
    doc["device_online"] = true;
    String err = currentPaArmError();
    if (err.length() > 0) doc["error"] = err;
    String out;
    serializeJson(doc, out);
    s.publish("state", true, out.c_str());

    // Track what we just published so we can suppress duplicates on the heartbeat.
    lastPublishedEnabled = enabled;
    lastPublishedArmed = armed;
    lastPublishedError = err;
}

// ---------------------------------------------------------------------------
// local LCD UI — indicator lights + front-panel button toggles
// ---------------------------------------------------------------------------
// The M5 Stamp PLC has a 240x135 LCD and three buttons below it: A (left),
// B (middle), C (right).  We draw four indicator lights for the relay states
// and use B to toggle PA remote-on and C to toggle TRX remote-on.

static void uiInit() {
    M5StamPLC.setBacklight(true);
    auto& d = M5StamPLC.Display;
    // The LCD is mounted landscape on the PLC front panel. Rotation 1 gives a
    // normal, left-to-right landscape orientation.
    d.setRotation(1);
    d.fillScreen(0x000000);
    d.setTextSize(1);
    d.setTextDatum(middle_center);
}

static void drawSquareButton(int x, int y, int w, int h, bool on, const char* label) {
    auto& d = M5StamPLC.Display;
    uint16_t r = 6;
    uint32_t fillColor = on ? 0x00C000 : 0x404040;
    uint32_t borderColor = on ? 0x00FF00 : 0x808080;

    d.fillSmoothRoundRect(x, y, w, h, r, fillColor);
    d.drawRoundRect(x, y, w, h, r, borderColor);

    d.setTextColor(on ? 0xFFFFFF : 0xCCCCCC);
    d.setTextDatum(middle_center);
    d.setTextSize(2);
    d.drawString(label, x + w / 2, y + h / 2);
    d.setTextSize(1);  // restore default
}

static void drawStatusText() {
    auto& d = M5StamPLC.Display;

    // Header line with connection + test label.
    d.setTextDatum(top_left);
    d.setTextColor(0xFFFFFF);
    d.setTextSize(2);
    d.drawString("TEST", 4, 2);
    d.setTextSize(1);

    bool wifiUp = (WiFi.status() == WL_CONNECTED);
    String status = wifiUp ? (gSwitch && gSwitch->connected() ? "MQTT" : "WiFi") : "----";
    d.setTextDatum(top_right);
    d.setTextColor(wifiUp ? 0x00FF00 : 0xFF0000);
    d.setTextSize(2);
    d.drawString(status.c_str(), d.width() - 4, 2);
    d.setTextSize(1);
}

static void drawButtonLegend() {
    auto& d = M5StamPLC.Display;
    d.setTextDatum(bottom_center);
    d.setTextColor(0xAAAAAA);
    d.setTextSize(1);
    d.drawString("B:PA", d.width() / 2 - 55, d.height() - 2);
    d.drawString("C:TRX", d.width() / 2 + 55, d.height() - 2);
}

static void uiRender() {
    static unsigned long lastUiRender = 0;
    static bool firstRender = true;
    if (millis() - lastUiRender < UI_REFRESH_MS) return;
    lastUiRender = millis();

    auto& d = M5StamPLC.Display;

    // Full clear only on first draw; after that we repaint only the dynamic
    // regions to avoid the visible flicker from clearing the whole screen.
    if (firstRender) {
        d.fillScreen(0x000000);
        drawButtonLegend();
        firstRender = false;
    }

    // Header/status text is small and cheap; clear its bounding area and redraw.
    d.fillRect(0, 0, d.width(), 26, 0x000000);
    drawStatusText();

    // Four large square relay buttons across the middle of the screen.
    // Landscape 240x135: leave 28 px top for header, 18 px bottom for legend.
    int btnW = 48;
    int btnH = 48;
    int y = 32;
    int totalW = btnW * 4 + 8 * 3;  // 4 buttons + 8 px gaps
    int startX = (d.width() - totalW) / 2;
    int gap = 8;
    drawSquareButton(startX + 0 * (btnW + gap), y, btnW, btnH, relayGet(RELAY_PA_ARM), "ARM");
    drawSquareButton(startX + 1 * (btnW + gap), y, btnW, btnH, relayGet(RELAY_PA_REMOTE), "PA");
    drawSquareButton(startX + 2 * (btnW + gap), y, btnW, btnH, relayGet(RELAY_TRX_REMOTE), "TRX");
    drawSquareButton(startX + 3 * (btnW + gap), y, btnW, btnH, relayGet(RELAY_SPARE), "SP");
}

static void handleButtons() {
    static unsigned long lastBtnMs = 0;
    if (millis() - lastBtnMs < UI_BTN_DEBOUNCE_MS) return;

    // BtnB (middle) toggles PA remote power.
    if (M5StamPLC.BtnB.wasPressed()) {
        lastBtnMs = millis();
        switchState.pa = !switchState.pa;
        applySwitchRelays();
        publishSwitchCmd("set_pa", switchState.pa ? "on" : "off");
        if (gSwitch && gSwitch->connected()) publishSwitchState(*gSwitch);
    }
    // BtnC (rightmost) toggles TRX remote power.
    if (M5StamPLC.BtnC.wasPressed()) {
        lastBtnMs = millis();
        switchState.trx = !switchState.trx;
        applySwitchRelays();
        publishSwitchCmd("set_trx", switchState.trx ? "on" : "off");
        if (gSwitch && gSwitch->connected()) publishSwitchState(*gSwitch);
    }
}

// ---------------------------------------------------------------------------
// slot instances + connect bookkeeping
// ---------------------------------------------------------------------------
SlotMqtt* gSwitch = nullptr;
SlotMqtt* gPaArm = nullptr;
bool switchConnectedOnce = false;
bool paArmConnectedOnce = false;

void onSwitchConnect() {
    publishSwitchMeta(*gSwitch);
    publishSwitchState(*gSwitch);
    switchConnectedOnce = true;
}
void onPaArmConnect() {
    publishPaArmMeta(*gPaArm);
    publishPaArmState(*gPaArm);
    // (re)subscribe the radio + ant-switch state feeds for arm logic.
    gPaArm->subscribeAbs(RADIO_BASE "/state");
    gPaArm->subscribeAbs(ANT_SWITCH_BASE "/state");
    paArmConnectedOnce = true;
}

// ---------------------------------------------------------------------------
// WiFi
// ---------------------------------------------------------------------------
static void wifiConnect() {
    WiFi.mode(WIFI_STA);
    WiFi.setSleep(false);  // reliability over power on a mains-powered PLC
    WiFi.begin(WIFI_SSID, WIFI_PASSWORD);
    // Non-blocking: loop() waits for WiFiClass to connect.
}

// ---------------------------------------------------------------------------
// ArduinoOTA (network firmware updates)
// ---------------------------------------------------------------------------
static void otaInit() {
    ArduinoOTA.setHostname(DEVICE_SERIAL);
    ArduinoOTA.setPassword(OTA_PASSWORD);
    ArduinoOTA.onStart([]() {
        // Best-effort: drop the arm relay before rebooting into the update.
        relaySet(RELAY_PA_ARM, false);
    });
    ArduinoOTA.onError([](ota_error_t err) {
        // Errors are logged only; the main loop keeps running.
        (void)err;
    });
    ArduinoOTA.begin();
}

// ---------------------------------------------------------------------------
// setup / loop
// ---------------------------------------------------------------------------
SlotMqtt switchSlot(SWITCH_BASE, "switch", handleSwitchCmd);
SlotMqtt paArmSlot(PA_ARM_BASE, "pa-arm", handlePaArmCmd);

void setup() {
    relayInit();
    uiInit();
    wifiConnect();
    otaInit();
    gSwitch = &switchSlot;
    gPaArm = &paArmSlot;
    // Publish meta/states are driven from loop()'s connect-edge handler.
}

void loop() {
    // 1. Update M5Unified internals (button debouncing etc.). Do this every
    //    iteration so the front-panel buttons work even when WiFi/MQTT are down.
    M5StamPLC.update();

    // 2. Front-panel button toggles: B = PA remote-on, C = TRX remote-on.
    handleButtons();

    // 3. WiFi — wait/reconnect.
    static unsigned long lastWifiCheck = 0;
    if (WiFi.status() != WL_CONNECTED) {
        if (millis() - lastWifiCheck > 5000) {
            lastWifiCheck = millis();
            WiFi.reconnect();
        }
        uiRender();
        delay(LOOP_DELAY_MS);  // slow poll while waiting for WiFi
        return;
    }

    // 4. Drive both slot MQTT state machines.
    switchSlot.loop();
    paArmSlot.loop();

    // 4a. Service ArduinoOTA so network firmware updates work.
    ArduinoOTA.handle();

    // 5. Connect-edge bookkeeping (publish meta + initial state + resub radio).
    if (switchSlot.connected() && !switchConnectedOnce) onSwitchConnect();
    if (!switchSlot.connected()) switchConnectedOnce = false;
    if (paArmSlot.connected() && !paArmConnectedOnce) onPaArmConnect();
    if (!paArmSlot.connected()) paArmConnectedOnce = false;

    // 6. Arm logic: recompute every loop; publish /state immediately when
    //    enabled, armed, or the blocking error changes, otherwise only on the
    //    slow heartbeat. This prevents flooding the bus with identical
    //    snapshots while the PA sits idle in RX.
    static unsigned long lastArmPublish = 0;
    bool armedChanged = recomputeArm(paArmSlot);
    bool shouldPublish = false;
    if (paArmSlot.connected()) {
        String err = currentPaArmError();
        if (armedChanged ||
            enabled != lastPublishedEnabled ||
            armed != lastPublishedArmed ||
            err != lastPublishedError) {
            shouldPublish = true;
        } else if (millis() - lastArmPublish >= PA_ARM_HEARTBEAT_MS) {
            shouldPublish = true;
        }
        if (shouldPublish) {
            publishPaArmState(paArmSlot);
            lastArmPublish = millis();
        }
    }

    // 7. Local LCD indicator lights.
    uiRender();

    delay(LOOP_DELAY_MS);  // ~20 Hz poll; keeps CPU cool vs. a tight 10 ms loop
}