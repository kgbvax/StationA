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
//   - connects with a CLEAN session (PubSubClient 2.8 forces cleanSession=1) and
//     a retained LWT "offline" on <base>/status — so the broker forgets every
//     subscription on disconnect, and self-heal relies on retained replay only
//   - on (re)connect publishes "online" to <base>/status, re-subscribes
//     <base>/cmd (retained /cmd is replayed by the broker → self-heal, model §8)
//     and then runs the slot's onConnected hook IN THE SAME CALL, so extra
//     subscriptions (the pa-arm input feeds) can never be skipped by a missed
//     connect edge
//   - exposes publish(topic, retained, payload) and a per-slot callback for /cmd
#pragma once

#include <Arduino.h>
#include <PubSubClient.h>
#include <WiFiClient.h>

typedef void (*CmdCallback)(const char* topic, const uint8_t* payload, unsigned int len);
// ConnectedHook runs right after a successful (re)connect + /cmd subscribe.
// Returns false if any of its subscriptions failed — the slot then drops the
// connection so the next dial starts clean instead of running half-subscribed.
typedef bool (*ConnectedHook)();

// BoundedConnectClient bounds the TCP connect wait. PubSubClient dials through
// the 2-arg Client overloads; the ESP32 core's default is a 3 s select() wait
// against a silent host — with TWO slots dialing every pass that stalled the
// arm-logic loop ~6 s per round (observed ~16 s arm drop vs the documented
// ~10 s bound). The 3-arg overloads take milliseconds and have no read-side
// side effects (setTimeout() would also shrink SO_RCVTIMEO — not wanted).
class BoundedConnectClient : public WiFiClient {
   public:
    int connect(IPAddress ip, uint16_t port) override {
        return WiFiClient::connect(ip, port, (int32_t)MQTT_CONNECT_TIMEOUT_MS);
    }
    int connect(const char* host, uint16_t port) override {
        return WiFiClient::connect(host, port, (int32_t)MQTT_CONNECT_TIMEOUT_MS);
    }
};

class SlotMqtt {
   public:
    SlotMqtt(const char* base, const char* clientSuffix, CmdCallback onCmd,
             ConnectedHook onConnected, uint16_t bufferSize)
        : base_(base), suffix_(clientSuffix), onCmd_(onCmd), onConnected_(onConnected) {
        client_.setClient(net_);
        client_.setBufferSize(bufferSize);
        client_.setKeepAlive(MQTT_KEEPALIVE_S);
        client_.setSocketTimeout(MQTT_CONNACK_TIMEOUT_S);
        // LWT: retained "offline" on this slot's /status — fires on unexpected disconnect.
        client_.setServer(MQTT_HOST, MQTT_PORT);
        client_.setCallback(
            [this](char* topic, uint8_t* payload, unsigned int len) {
                this->dispatch(topic, payload, len);
            });
        willTopic_[0] = '\0';
        snprintf(willTopic_, sizeof(willTopic_), "%s/status", base);
    }

    // loop drives the MQTT state machine; call every main iteration. Handles
    // reconnect with a shared backoff: at most ONE slot dials per pass, so a
    // down broker can never stall the arm-logic loop on both connections in
    // the same iteration (defect D6). Returns true while connected.
    bool loop() {
        if (client_.connected()) {
            // PubSubClient reads at most one packet per loop(); drain a bounded
            // burst so a retained replay or keyed-PA telemetry cannot starve
            // radio/state (MQTT_DRAIN_MAX in config.h).
            for (int i = 0; i < MQTT_DRAIN_MAX; ++i) {
                if (!client_.loop()) break;
                if (net_.available() <= 0) break;
            }
            return client_.connected();
        }
        static unsigned long lastAttemptMs = 0;  // shared across ALL SlotMqtt instances
        unsigned long now = millis();
        if (now - lastAttemptMs < MQTT_RECONNECT_BACKOFF_MS) {
            return false;
        }
        lastAttemptMs = now;
        if (connect()) {
            onConnect();
        }
        return client_.connected();
    }

    bool connected() { return client_.connected(); }

    // drop tears the session down so the next loop() redials and re-runs the
    // connect hook. Used after a WiFi outage: ESP32's WiFiClient can still
    // report a short-blipped socket as connected, and then nothing would
    // re-subscribe (the arm inputs were invalidated during the outage).
    void drop() {
        client_.disconnect();
        net_.stop();
    }

    // publish a payload under <base>/<suffix>. Returns PubSubClient's result
    // (false = not written; QoS 0, so true is no delivery guarantee either).
    bool publish(const char* suffix, bool retained, const char* payload) {
        char topic[96];
        snprintf(topic, sizeof(topic), "%s/%s", base_, suffix);
        return client_.publish(topic, payload, retained);
    }
    void publish(const char* suffix, bool retained, const uint8_t* payload, unsigned int len) {
        char topic[96];
        snprintf(topic, sizeof(topic), "%s/%s", base_, suffix);
        client_.publish(topic, payload, len, retained);
    }

    bool subscribe(const char* suffix) {
        char topic[96];
        snprintf(topic, sizeof(topic), "%s/%s", base_, suffix);
        return client_.subscribe(topic);
    }

    // subscribe an arbitrary absolute topic (the pa-arm input feeds).
    bool subscribeAbs(const char* topic) { return client_.subscribe(topic); }

   private:
    const char* base_;
    const char* suffix_;
    CmdCallback onCmd_;
    ConnectedHook onConnected_;
    BoundedConnectClient net_;
    PubSubClient client_;
    char willTopic_[80];

    bool connect() {
        char clientId[40];
        snprintf(clientId, sizeof(clientId), "%s-%s", SITE, suffix_);
        return client_.connect(clientId, MQTT_USER, MQTT_PASSWORD,
                               willTopic_, 1, true, "offline");
    }

    // onConnect runs inside the same loop() call that dialed, so the connect
    // edge can never be missed (it used to be sampled by main once per pass: a
    // drop + redial inside one SlotMqtt::loop() skipped the feed re-subscribe
    // and left pa-arm on "radio feed stale" until the next observed drop).
    void onConnect() {
        publish("status", true, "online");
        // Replay retained /cmd (broker delivers the last retained command on
        // subscribe → self-heal); the hook then publishes meta/state and
        // subscribes any extra feeds.
        bool ok = subscribe("cmd");
        if (ok && onConnected_) ok = onConnected_();
        if (!ok) {
            // A clean disconnect suppresses the LWT — retract "online" ourselves.
            publish("status", true, "offline");
            client_.disconnect();  // redial clean on the next backoff tick
        }
    }

    void dispatch(char* topic, uint8_t* payload, unsigned int len) {
        if (onCmd_) onCmd_(topic, payload, len);
    }
};