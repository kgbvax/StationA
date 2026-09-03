// sim.h — the local rotator model for the SIM_MODE desk build.
//
// The simulation replaces the station side of the wire: no WiFi, no MQTT,
// no OTA — the Dial develops face and knob logic on the desk with nothing
// else powered. Fidelity is the whole point, so the model speaks the exact
// wire shapes through the exact production code:
//   - /cmd intake: set_az and stop enter the model the way they enter the
//     wrc-rotator-bridge, quirks included (set_az clamps to the mast range;
//     a stop halts past the measured azimuth, and the reported target keeps
//     the CANCELLED value after the halt — both are load-bearing for the
//     re-baseline logic in main.cpp).
//   - /state out: the model emits the same JSON the bridge publishes,
//     through the same parser and the same resync/beep/re-baseline paths,
//     with the same change-dedup discipline — a parked sim is silent, the
//     way a parked rotator is silent by design.
#pragma once

#if SIM_MODE

#include <Arduino.h>

class SimRotator {
   public:
    typedef void (*StateJsonCallback)(const char* json, unsigned int len);

    explicit SimRotator(StateJsonCallback onState) : onState_(onState) {}

    void begin() {
        // Boot the model mast mid-range, not at 0: scale 0 is the rotator's
        // hard CCW limit, and a model parked there leaves one whole knob
        // direction dead from power-on — both directions must be testable
        // before the first turn.
        az_ = SIM_BOOT_AZ;
        target_ = SIM_BOOT_AZ;
        moving_ = false;
        dirty_ = true;  // emit the initial parked state: the boot feed
        lastStepMs_ = millis();
    }

    // A set_az that changes nothing observable (same target, same motion)
    // emits nothing: the real bridge change-dedups on exactly that. A set_az
    // to the current position starts no motion (the real bridge behaves the
    // same) — and while parked it is not even a state change.
    void onSetAz(float az) {
        float newTarget = constrain(az, 0.0f, 450.0f);
        bool newMoving = fabsf(newTarget - az_) > 0.05f;
        if (newTarget == target_ && newMoving == moving_) return;
        target_ = newTarget;
        moving_ = newMoving;
        dirty_ = true;
    }

    // Braking coast: halt past the measured azimuth, clamped to the mast
    // range; the cancelled target stays reported. A stop while parked is no
    // state change at all — the parked, change-deduped real /state stays
    // silent, so the sim does too. The halt /state itself is DELAYED by
    // SIM_HALT_LATENCY_MS: on the wire the stop crosses the broker, the
    // bridge's /cmd queue and the WRC before the halt frame returns, and in
    // that window the operator can still detent — the press-vs-detent race
    // the stopResyncPending logic in main.cpp exists for. Compressing it to
    // zero would make that race untestable on the desk.
    void onStop() {
        if (!moving_) return;
        az_ = (target_ > az_) ? az_ + SIM_STOP_OVERSHOOT_DEG
                              : az_ - SIM_STOP_OVERSHOOT_DEG;
        az_ = constrain(az_, 0.0f, 450.0f);
        moving_ = false;
        dirty_ = true;
        haltAtMs_ = millis() + SIM_HALT_LATENCY_MS;
    }

    // Advance the mast and emit /state on the same edges the real one does:
    // continuously while moving, once at arrival, once per fresh command,
    // and never while parked. Called every main-loop pass.
    void step() {
        unsigned long now = millis();
        if (haltAtMs_ != 0) {
            if (now < haltAtMs_) {
                lastStepMs_ = now;  // no motion integration during the halt
                return;             // window; the wire is quiet there too
            }
            haltAtMs_ = 0;
            // The halt /state lands now: the parked path below flushes it.
            // A set_az that arrived inside the window re-armed motion, so
            // the moving flow resumes instead — the wire behaves the same
            // when a fresh command overtakes the braking.
        }
        if (!moving_) {
            lastStepMs_ = now;
            if (dirty_) {
                emitState();
                dirty_ = false;
            }
            return;
        }
        float dt = (now - lastStepMs_) * 0.001f;
        lastStepMs_ = now;
        float d = target_ - az_;
        float travel = SIM_ROTATOR_DEG_PER_SEC * dt;
        if (fabsf(d) <= travel) {
            az_ = target_;
            moving_ = false;  // arrival edge — the arrival beep path fires
        } else {
            az_ += (d > 0.0f) ? travel : -travel;
        }
        emitState();
        dirty_ = false;  // the moving state just went out
    }

   private:
    void emitState() {
        char json[96];
        // The bridge omits target_az when the WRC reports target 0 (the
        // field is omitempty and only set for a non-zero tdeg) — reproduce
        // that, so the null-target fallback in applyRotatorState runs on
        // the desk too, and a commanded park to 0 behaves as it will live.
        if (target_ == 0.0f) {
            snprintf(json, sizeof(json),
                     "{\"az\":%.1f,\"moving\":%s,\"device_online\":true}",
                     az_, moving_ ? "true" : "false");
        } else {
            snprintf(json, sizeof(json),
                     "{\"az\":%.1f,\"target_az\":%.1f,\"moving\":%s,"
                     "\"device_online\":true}",
                     az_, target_, moving_ ? "true" : "false");
        }
        if (onState_) onState_(json, (unsigned int)strlen(json));
    }

    StateJsonCallback onState_;
    float az_ = 0.0f;
    float target_ = 0.0f;
    bool moving_ = false;
    bool dirty_ = false;
    unsigned long lastStepMs_ = 0;
    unsigned long haltAtMs_ = 0;  // 0 = no halt pending; else the emit deadline
};

#endif  // SIM_MODE