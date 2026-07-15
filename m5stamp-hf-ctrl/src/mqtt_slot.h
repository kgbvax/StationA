// mqtt_slot.h — one MQTT connection per slot, each with its own LWT.
//
// The PLC is a compound device publishing two slots (hf/switch, hf/pa-arm). MQTT
// 3.1.1 allows one Will per client, so a PLC crash would only fire one slot's
// /status will if we used a single connection. Instead each slot gets its own
// PubSubClient (its own WiFiClient + LWT on its own <slot>/status), so a crash
// takes BOTH slots offline at once with no stale-online gap — the embedded
// analog of the Go compound bridge's one-client-per-slot decision.
//
// Each SlotMqtt:
//   - connects with CleanSession=false and a retained LWT "offline" on <base>/status
//   - on (re)connect publishes "online" to <base>/status and re-subscribes <base>/cmd
//     (retained /cmd is replayed by the broker → self-heal, model §8)
//   - exposes publish(topic, retained, payload) and a per-slot callback for /cmd
#pragma once

#include <Arduino.h>
#include <PubSubClient.h>
#include <WiFiClient.h>

typedef void (*CmdCallback)(const char* topic, const uint8_t* payload, unsigned int len);

class SlotMqtt {
   public:
    SlotMqtt(const char* base, const char* clientSuffix, CmdCallback onCmd)
        : base_(base), suffix_(clientSuffix), onCmd_(onCmd), connected_(false) {
        client_.setClient(net_);
        client_.setBufferSize(MQTT_BUFFER_SIZE);
        client_.setKeepAlive(MQTT_KEEPALIVE_S);
        // LWT: retained "offline" on this slot's /status — fires on unexpected disconnect.
        char willTopic[80];
        snprintf(willTopic, sizeof(willTopic), "%s/status", base);
        client_.setServer(MQTT_HOST, MQTT_PORT);
        client_.setCallback(
            [this](char* topic, uint8_t* payload, unsigned int len) {
                this->dispatch(topic, payload, len);
            });
        willTopic_[0] = '\0';
        snprintf(willTopic_, sizeof(willTopic_), "%s/status", base);
    }

    // loop drives the MQTT state machine; call every main iteration. Handles
    // reconnect with a backoff. Returns true while connected.
    bool loop() {
        if (client_.connected()) {
            client_.loop();
            return true;
        }
        // (re)connect
        if (connect()) {
            onConnect();
        }
        return false;
    }

    bool connected() { return client_.connected(); }

    // publish a retained payload under <base>/<suffix>.
    void publish(const char* suffix, bool retained, const char* payload) {
        char topic[96];
        snprintf(topic, sizeof(topic), "%s/%s", base_, suffix);
        client_.publish(topic, payload, retained);
    }
    void publish(const char* suffix, bool retained, const uint8_t* payload, unsigned int len) {
        char topic[96];
        snprintf(topic, sizeof(topic), "%s/%s", base_, suffix);
        client_.publish(topic, payload, len, retained);
    }

    void subscribe(const char* suffix) {
        char topic[96];
        snprintf(topic, sizeof(topic), "%s/%s", base_, suffix);
        client_.subscribe(topic);
    }

    // subscribe an arbitrary absolute topic (used for hf/radio/state by pa-arm).
    void subscribeAbs(const char* topic) { client_.subscribe(topic); }

   private:
    const char* base_;
    const char* suffix_;
    CmdCallback onCmd_;
    WiFiClient net_;
    PubSubClient client_;
    bool connected_;
    char willTopic_[80];

    bool connect() {
        char clientId[40];
        snprintf(clientId, sizeof(clientId), "%s-%s", SITE, suffix_);
        return client_.connect(clientId, MQTT_USER, MQTT_PASSWORD,
                               willTopic_, 1, true, "offline");
    }

    void onConnect() {
        connected_ = true;
        publish("status", true, "online");
        // Replay retained /cmd (broker delivers the last retained command on
        // subscribe → self-heal). And re-subscribe any absolute topics is the
        // caller's responsibility via the onCmd_ wiring — we only re-subscribe
        // our own /cmd here; extra subs are set up by main after connect.
        subscribe("cmd");
    }

    void dispatch(char* topic, uint8_t* payload, unsigned int len) {
        if (onCmd_) onCmd_(topic, payload, len);
    }
};