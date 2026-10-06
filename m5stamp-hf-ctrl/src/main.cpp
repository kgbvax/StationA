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
// firmware subscribes the radio, antenna switch, Ultrabeam controller, antenna
// reconciler, PA and tuner feeds and computes (arm.h)
//   armed = enabled ∧ all gates clear (held ARM_CLOSE_DWELL_MS, closed in RX)
// driving relay 1 so that any hazard (radio lost/stale/tuning, unsafe band,
// antenna grounded or switching, Ultrabeam moving/off-frequency, PA fault, ATU
// tuning, 13.8 V lost, PLC crash) drops the relay open → PA disabled, and a band
// change or Ultrabeam move holds it open until the station has followed.
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
#include "arm.h"       // pa-arm interlock: inputs, gates, relay drive

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
// pa-arm slot state (relay 1 → channel 0). Inputs + gates live in arm.h.
// ---------------------------------------------------------------------------
bool enabled = false;        // software arm-permit (last /cmd set_enabled)
String cmdError = "";        // last rejected /cmd value (D5: never a silent disarm)
// A front-panel permit toggle (BtnA) stays the authoritative intent until the
// PLC sees its own retained echo come back: until then it is re-published (on
// reconnect, and every PERMIT_REPUBLISH_MS), and an inbound set_enabled with a
// different value — the broker's stale retained intent — is not applied (D8,
// for the permit).
bool permitEchoPending = false;
bool permitIntent = false;
unsigned long permitPublishMs = 0;
#define PERMIT_REPUBLISH_MS 2000
arm::Eval armEval;           // the evaluation relay 1 was last driven from

// Deduplication: publish immediately when enabled/armed/error/block change,
// otherwise only on heartbeat. lastPublished* track the last snapshot sent.
bool lastPublishedEnabled = false;
bool lastPublishedArmed = false;
String lastPublishedError = "";
String lastPublishedBlock = "";
String lastPublishedCmdError = "";

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
static bool onSwitchConnected();
static bool onPaArmConnected();
static void uiInit();
static void uiRender();
static void handleButtons();

// ---------------------------------------------------------------------------
// JSON helpers
// ---------------------------------------------------------------------------
// parseBoolish reads a /cmd `value` that may arrive as a JSON bool or as one of
// two case-insensitive strings (onWord/offWord). Returns false — and leaves
// `out` untouched — for anything else, so a mistyped value is rejected loudly
// instead of being applied as the safe-side-but-wrong "off" (defect D5).
static bool parseBoolish(JsonVariantConst v, const char* onWord, const char* offWord, bool& out) {
    if (v.is<bool>()) {
        out = v.as<bool>();
        return true;
    }
    if (!v.is<const char*>()) return false;
    String s = v.as<const char*>();
    s.trim();
    s.toLowerCase();
    if (s == onWord) { out = true; return true; }
    if (s == offWord) { out = false; return true; }
    return false;
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
    JsonDocument doc;
    if (deserializeJson(doc, payload, len)) return;
    String action = doc["action"] | "";
    bool on;
    if (action == "set_pa") {
        if (!parseBoolish(doc["value"], "on", "off", on)) return;  // bad value — drop, relay unchanged
        switchState.pa = on;
        applySwitchRelays();
    } else if (action == "set_trx") {
        if (!parseBoolish(doc["value"], "on", "off", on)) return;
        switchState.trx = on;
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
// The paArmSlot connection subscribes its own /cmd AND the arm input feeds
// (arm.h INPUT_TOPICS). All arrive through the same PubSubClient callback:
// arm::dispatch takes the input topics (exact match), anything else is our /cmd.
static void handlePaArmCmd(const char* topic, const uint8_t* payload, unsigned int len) {
    if (arm::dispatch(topic, payload, len)) return;
    if (strcmp(topic, PA_ARM_BASE "/cmd") != 0) return;
    JsonDocument doc;
    if (deserializeJson(doc, payload, len)) return;  // unparseable — silent drop (R6.2)
    String action = doc["action"] | "";
    if (action != "set_enabled") return;            // unknown action — silent drop
    bool v;
    if (parseBoolish(doc["value"], "true", "false", v)) {
        if (permitEchoPending) {
            if (v != permitIntent) return;  // stale retained intent — the button press wins
            permitEchoPending = false;      // our own echo: the broker holds the new intent
        }
        enabled = v;  // armed recomputed in main loop; relay driven from the gates
        cmdError = "";
    } else {
        cmdError = "set_enabled: bad value";  // permit unchanged, reported in /state
    }
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

// publishPaArmPermit writes the local permit as the retained pa-arm/cmd intent
// (the same wire form powerseq and the console use). We are subscribed to our
// own /cmd, so the echo comes back and is re-applied — idempotent.
static void publishPaArmPermit() {
    permitEchoPending = true;  // cleared only by the echo (handlePaArmCmd)
    permitIntent = enabled;
    permitPublishMs = millis();
    if (!gPaArm || !gPaArm->connected()) return;  // re-sent from the connect hook
    JsonDocument doc;
    doc["action"] = "set_enabled";
    doc["value"] = enabled ? "true" : "false";
    String out;
    serializeJson(doc, out);
    gPaArm->publish("cmd", true, out.c_str());
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
    JsonArray inputs = caps["inputs"].to<JsonArray>();
    inputs.add(RADIO_SLOT); inputs.add(ANT_SWITCH_SLOT); inputs.add(ANT_CTRL_SLOT);
    inputs.add(ANT_SELECT_SLOT); inputs.add(PA_SLOT); inputs.add(TUNER_SLOT);
    caps["ultrabeam_port"] = ULTRABEAM_PORT;
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
        // Wire form is a STRING ("true"/"false"); a JSON bool is accepted too
        // (parseBoolish), anything else is rejected into /state.cmd_error.
        // Must match the API doc §set_enabled; consumers key on this descriptor.
        cmd["value_type"] = "string";
    }
    {  // block — read-only interlock reason
        JsonObject f = fields.add<JsonObject>();
        f["key"] = "block";
        f["name"] = "Arm blocked by";
        f["type"] = "string";
        f["writable"] = false;
    }
    String out;
    serializeJson(doc, out);
    s.publish("meta", true, out.c_str());
}

// /state carries:
//   enabled    the operator permit
//   armed      relay 1 readback
//   error      a FAULT among the gates (feed lost/stale, relay mismatch, PA
//              fault, Ultrabeam controller offline while in circuit) —
//              independent of `enabled` (D7: tells the operator why an arm would
//              be blocked). Normal transitions are NOT errors (console FAULTS).
//   block      why the arm is open / about to open, including normal
//              transitions ("band change settling", "ultrabeam moving",
//              "permit off", "waiting for rx"); absent when armed and clear
//   cmd_error  the last rejected set_enabled value, until a valid one arrives
static void publishPaArmState(SlotMqtt& s) {
    JsonDocument doc;
    doc["ts"] = millis();
    doc["enabled"] = enabled;
    doc["armed"] = arm::relayReadback;
    doc["device_online"] = true;
    String err = armEval.fault ? armEval.fault : "";
    String block = arm::blockReason(armEval, enabled);
    if (err.length() > 0) doc["error"] = err;
    if (block.length() > 0) doc["block"] = block;
    if (cmdError.length() > 0) doc["cmd_error"] = cmdError;
    String out;
    serializeJson(doc, out);
    s.publish("state", true, out.c_str());

    // Track what we just published so we can suppress duplicates on the heartbeat.
    lastPublishedEnabled = enabled;
    lastPublishedArmed = arm::relayReadback;
    lastPublishedError = err;
    lastPublishedBlock = block;
    lastPublishedCmdError = cmdError;
}

// ---------------------------------------------------------------------------
// local LCD UI — indicator lights + front-panel button toggles
// ---------------------------------------------------------------------------
// The M5 Stamp PLC has a 240x135 LCD and three buttons below it: A (left),
// B (middle), C (right).  We draw four indicator lights for the relay states
// and use A to toggle the arm permit, B to toggle PA remote-on and C to toggle
// TRX remote-on.

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

    // Header line: arm status (ARMED / PERMIT off / BLOCKED) + link state.
    d.setTextDatum(top_left);
    d.setTextSize(2);
    if (arm::relayReadback) {
        d.setTextColor(0x00FF00);
        d.drawString("ARMED", 4, 2);
    } else if (!enabled) {
        d.setTextColor(0xAAAAAA);
        d.drawString("PERMIT", 4, 2);
    } else {
        d.setTextColor(0xFF4040);
        d.drawString("BLOCKED", 4, 2);
    }
    d.setTextSize(1);

    bool wifiUp = (WiFi.status() == WL_CONNECTED);
    String status = wifiUp ? (gPaArm && gPaArm->connected() ? "MQTT" : "WiFi") : "----";
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
    d.drawString("A:ARM", d.width() / 2 - 80, d.height() - 2);
    d.drawString("B:PA", d.width() / 2, d.height() - 2);
    d.drawString("C:TRX", d.width() / 2 + 80, d.height() - 2);
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

    // Arm block reason under the buttons, so the cause is readable at the PLC.
    d.fillRect(0, y + btnH + 4, d.width(), 14, 0x000000);
    d.setTextDatum(top_center);
    d.setTextSize(1);
    d.setTextColor(0xFFFF80);
    d.drawString(arm::blockReason(armEval, enabled), d.width() / 2, y + btnH + 6);
}

static void handleButtons() {
    static unsigned long lastBtnMs = 0;
    if (millis() - lastBtnMs < UI_BTN_DEBOUNCE_MS) return;

    // BtnA (leftmost) toggles the arm PERMIT — manual arm / disarm at the PLC.
    // It never forces relay 1: arming still needs every gate clear, the dwell
    // and RX (arm.h); disarming opens the relay on the next pass.
    if (M5StamPLC.BtnA.wasPressed()) {
        lastBtnMs = millis();
        enabled = !enabled;
        cmdError = "";
        publishPaArmPermit();
    }

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

// Connect hooks run inside SlotMqtt::onConnect (same call as the dial), after
// /status online + /cmd subscribe — the R8.1 reconnect order.
static bool onSwitchConnected() {
    publishSwitchMeta(*gSwitch);
    publishSwitchState(*gSwitch);
    return true;
}
static bool onPaArmConnected() {
    // Every input back to unknown/closed BEFORE the retained replay lands: the
    // arm can only close on data received over this connection.
    arm::invalidate();
    if (permitEchoPending) publishPaArmPermit();  // un-echoed BtnA press wins over the stale retained intent
    publishPaArmMeta(*gPaArm);
    publishPaArmState(*gPaArm);
    bool ok = true;
    for (int i = 0; i < arm::INPUT_TOPICS_COUNT; ++i) {
        ok = gPaArm->subscribeAbs(arm::INPUT_TOPICS[i]) && ok;
    }
    return ok;
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
        // Best-effort: drop the arm relay before rebooting into the update. Also
        // clear the commanded state, so a FAILED update that returns to loop()
        // re-arms only through the dwell + RX path.
        relaySet(RELAY_PA_ARM, false);
        arm::relayCmd = false;
        arm::closeOkValid = false;
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
SlotMqtt switchSlot(SWITCH_BASE, "switch", handleSwitchCmd, onSwitchConnected, MQTT_BUFFER_SIZE);
SlotMqtt paArmSlot(PA_ARM_BASE, "pa-arm", handlePaArmCmd, onPaArmConnected, PA_ARM_MQTT_BUFFER_SIZE);

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

    // 3. WiFi + MQTT. The arm logic below runs on EVERY pass, link or not
    //    (D1: it used to be skipped while WiFi was down, freezing relay 1).
    static unsigned long lastWifiCheck = 0;
    static bool linkLost = false;
    bool wifiUp = (WiFi.status() == WL_CONNECTED);
    if (wifiUp) {
        if (linkLost) {
            // The inputs were invalidated during the outage; force a fresh
            // session so the connect hook re-subscribes and the broker replays.
            paArmSlot.drop();
            switchSlot.drop();
            linkLost = false;
        }
        // pa-arm first: the safety slot wins the shared reconnect backoff.
        paArmSlot.loop();
        switchSlot.loop();
        ArduinoOTA.handle();
    } else {
        linkLost = true;
        if (millis() - lastWifiCheck > 5000) {
            lastWifiCheck = millis();
            WiFi.reconnect();
        }
    }
    // A button permit change not yet echoed back: re-send it.
    if (permitEchoPending && paArmSlot.connected() &&
        millis() - permitPublishMs >= PERMIT_REPUBLISH_MS) {
        publishPaArmPermit();
    }

    // 4. No link ⇒ no supervision: every arm input drops to unknown/closed, so
    //    relay 1 opens on this pass instead of holding its last position.
    if (!wifiUp || !paArmSlot.connected()) arm::invalidate();

    // 5. Arm logic: recompute + drive relay 1 every pass; publish /state
    //    immediately when enabled, armed, error, block or cmd_error change,
    //    otherwise only on the slow heartbeat.
    static unsigned long lastArmPublish = 0;
    armEval = arm::recompute(enabled, millis());
    if (paArmSlot.connected()) {
        String err = armEval.fault ? armEval.fault : "";
        String block = arm::blockReason(armEval, enabled);
        if (enabled != lastPublishedEnabled ||
            arm::relayReadback != lastPublishedArmed ||
            err != lastPublishedError ||
            block != lastPublishedBlock ||
            cmdError != lastPublishedCmdError ||
            millis() - lastArmPublish >= PA_ARM_HEARTBEAT_MS) {
            publishPaArmState(paArmSlot);
            lastArmPublish = millis();
        }
    }

    // 6. Local LCD indicator lights.
    uiRender();

    delay(LOOP_DELAY_MS);  // ~20 Hz poll; keeps CPU cool vs. a tight 10 ms loop
}