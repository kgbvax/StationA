// mqtt_client.h — lean MQTT client for a NON-slot consumer + /cmd stimulator.
//
// This is the Dial adaptation of m5stamp-hf-ctrl's SlotMqtt. SlotMqtt is
// slot-shaped: it connects with an LWT "offline" on <slot>/status and publishes
// "online" + /meta + /state on the connect edge — exactly what this device
// must NOT do (integration model §9 / PRD R1.6: a non-slot consumer has no MQTT
// presence of its own). What survives from SlotMqtt is the plumbing:
//   - PubSubClient buffer size set BEFORE connect (256 default is too small)
//   - keepalive, loop-driven reconnect, callback dispatch by absolute topic
// and what is new:
//   - NO will (3-arg connect), no status/meta/state publishes, no heartbeat
//   - a reconnect throttle so the face keeps rendering while the broker is down
//     (SlotMqtt retried every loop pass; the Dial renders at 30 Hz and the
//     blocking connect attempt would starve the needle)
//
// Known limitation, inherited from PubSubClient: publishes go out at QoS 0
// (the library cannot publish above QoS 0; only subscribe supports QoS 1).
// Consequence: a lost final set_az flush is possible on a flaky link. The face
// makes this visible — the TGT plaque shows the local target while /state's
// az/target_az lag — so the operator can nudge the knob again. Documented in
// docs/m5dial-hf-rotctrl-mqtt-api.md.
#pragma once

#include <Arduino.h>
#include <PubSubClient.h>
#include <WiFiClient.h>

typedef void (*MsgCallback)(const char* topic, const uint8_t* payload, unsigned int len);
typedef void (*ConnectCallback)();
typedef void (*DisconnectCallback)();

class DialMqtt {
   public:
    DialMqtt(const char* clientId, MsgCallback onMsg, ConnectCallback onConnect,
             DisconnectCallback onDisconnect)
        : clientId_(clientId), onMsg_(onMsg), onConnect_(onConnect),
          onDisconnect_(onDisconnect) {
        client_.setClient(net_);
        client_.setBufferSize(MQTT_BUFFER_SIZE);  // BEFORE connect
        client_.setKeepAlive(MQTT_KEEPALIVE_S);
        client_.setServer(MQTT_HOST, MQTT_PORT);
        // Bound connect block: PubSubClient's connect() calls the WiFiClient
        // connect synchronously, and against a silent host (shari powered off,
        // SYN unanswered) it blocks in select() for the core default 3000 ms —
        // freezing the whole loop (input, face, OTA) 3 s of every 5 s during
        // an outage. In this arduino-esp32 core WiFiClient::setTimeout takes
        // SECONDS (not ms): 1 caps each attempt at ~1 s.
        net_.setTimeout(1);
        client_.setCallback([this](char* topic, uint8_t* payload, unsigned int len) {
            if (this->onMsg_) this->onMsg_(topic, payload, len);
        });
    }

    // Drive the MQTT state machine every main-loop iteration. While connected
    // just pumps; while disconnected, throttled reconnect attempts (see
    // MQTT_RECONNECT_MS). Fires onConnect_ on the connected edge — subscribe
    // there: the retained /state + /status replay IS the boot-time feed.
    // Fires onDisconnect_ on the disconnect edge — main.cpp invalidates its
    // liveness knowledge there, so a reconnect can never trade on stale
    // pre-disconnect flags while the retained replay is still in flight.
    bool loop() {
        if (client_.connected()) {
            client_.loop();
            return true;
        }
        if (wasConnected_) {
            wasConnected_ = false;
            if (onDisconnect_) onDisconnect_();
        }
        unsigned long now = millis();
        if (now - lastAttemptMs_ < MQTT_RECONNECT_MS) return false;
        lastAttemptMs_ = now;
        // NO will: this device has no /status of its own (not a slot).
        if (client_.connect(clientId_, MQTT_USER, MQTT_PASSWORD)) {
            wasConnected_ = true;
            if (onConnect_) onConnect_();
            return true;
        }
        // Reason code on the serial monitor: -4 = TCP timeout (host down),
        // -5 = no network, 2 = bad client id, 4 = rejected credentials,
        // 5 = not authorized — turns a silent NO DATA face into a one-line
        // answer on the bench.
        Serial.printf("[mqtt] connect failed, rc=%d\n", client_.state());
        return false;
    }

    bool connected() { return client_.connected(); }

    // NOT retained — one-shot intent. PubSubClient publishes at QoS 0 (see
    // the file header); callers must treat /cmd as fire-and-observe.
    bool publish(const char* topic, const char* payload) {
        return client_.publish(topic, payload);
    }

    bool subscribe(const char* topic) { return client_.subscribe(topic, 1); }

   private:
    const char* clientId_;
    MsgCallback onMsg_;
    ConnectCallback onConnect_;
    DisconnectCallback onDisconnect_;
    WiFiClient net_;
    PubSubClient client_;
    bool wasConnected_ = false;
    unsigned long lastAttemptMs_ = 0;
};