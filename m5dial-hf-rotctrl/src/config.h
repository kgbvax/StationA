// config.h — non-secret, compile-time configuration for the M5Stack Dial
// HF-rotator control head firmware (m5dial-hf-rotctrl).
//
// This device is NOT a slot (integration model §9 / PRD 02-interface-spec R1.6):
// it is a consumer + /cmd stimulator, like testui and hf_console. It has no
// /meta, /state or /status of its own and no LWT. Its entire bus footprint is
// two subscriptions (the rotator slot's /state and /status) and two publish
// shapes on the rotator slot's /cmd. Removing it changes nothing for the rest
// of the station.
//
// The rotator's /cmd argument key is "az", NOT the station-wide "value" — a
// pre-convention deviation declared in the slot's /meta.expose as
// value_key:"az" (see wrc-rotator-bridge). Do not "fix" it to "value".
// /cmd is NEVER retained (a stale replay can spin the antenna), and this
// firmware never replays a cached target across a reconnect.
#pragma once

// --- topics (site/station/slot — no leading slash, station convention) ------
#define SITE "muehle"
#define ROTATOR_SLOT "hf/rotator"
#define ROTATOR_STATE_TOPIC  SITE "/" ROTATOR_SLOT "/state"
#define ROTATOR_STATUS_TOPIC SITE "/" ROTATOR_SLOT "/status"
#define ROTATOR_CMD_TOPIC    SITE "/" ROTATOR_SLOT "/cmd"

// --- MQTT client -------------------------------------------------------------
// Client ID is a documented non-slot exception (interface-spec §1.1): it must
// not collide with any slot-derived bridge ID because this device is not a
// bridge and owns no slot.
#define MQTT_CLIENT_ID "dial-hf-rotctrl-1"
// PubSubClient's default 256-byte buffer is too small for the retained /state
// snapshot with room to spare; bump it BEFORE connect().
#define MQTT_BUFFER_SIZE 1024
#define MQTT_KEEPALIVE_S 30
// Reconnect attempt throttle: keeps the face rendering smoothly while the
// broker is unreachable instead of retrying every 20 ms loop pass.
#define MQTT_RECONNECT_MS 5000
// WiFi re-association throttle for the same reason.
#define WIFI_RECONNECT_MS 5000

// --- rotator range -------------------------------------------------------------
// The Yaesu G-450DC / WRC range is 0..450 degrees (450 = 360 + 90 overlap).
// The bridge does no range validation (the WRC's own limits are the real
// limits), so we clamp locally. Never wrap IN COMMAND SPACE: wrapping a
// target would command a full-circle spin. The display is a compass card —
// it wraps visually (az 390 renders at 30°) and flags the 360..450 pass
// with a "+360" hint (see face.h).
#define AZ_MIN 0.0f
#define AZ_MAX 450.0f

// --- knob -----------------------------------------------------------------------
// The Dial's encoder gives 64 pulses per revolution in 16 detents → 4 counts
// per detent. We step in whole detents and keep the remainder, so wobble at a
// detent boundary is absorbed instead of lost. ENCODER_INVERT flips the sign
// if CW reads negative on real hardware.
//
// One detent = one knob-flat = one compass step: the encoder has 16 detents
// per revolution, so 360/16 = 22.5° per detent — turning the knob moves the
// target exactly as far as the knob itself turned. Steps stay RELATIVE to
// whatever azimuth the rotator is at (no snap-to-grid). There is no coarse
// mode: one knob, one gesture set (turn = move, press = stop).
#define ENCODER_COUNTS_PER_DETENT 4
// Bench-verified 2026-09-01: with the pin order A=41/B=40 the raw counts are
// inverted on this hardware — a clockwise turn reads negative. Inverted so a
// clockwise turn steps the target UP (needle sweeps clockwise, through E).
#define ENCODER_INVERT 1
#define DETENT_DEG 22.5f   // one knob-flat = one compass step (360° / 16)
// Target re-sync window: /state target_az overrides the local target only
// after this much idle time after the last detent (console-initiated moves
// become the new baseline; while the operator is turning, incoming target_az
// is ignored so the two writers do not fight).
#define TARGET_RESYNC_IDLE_MS 2000

// --- /cmd publish coalescing ------------------------------------------------------
// The bridge's /cmd worker queue is 8 deep and silently DROPS on overflow, so
// the knob must not machine-gun set_az. First detent of a burst publishes
// immediately; while turning, at most one publish per PUBLISH_MIN_INTERVAL_MS
// with the latest target; a final flush PUBLISH_FLUSH_MS after the last detent.
#define PUBLISH_MIN_INTERVAL_MS 200
#define PUBLISH_FLUSH_MS 400

// --- face rendering -----------------------------------------------------------------
// Needle damping: displayAz converges on the real az by NEEDLE_K per 33 ms
// frame (time constant ~0.55 s: a 90° step settles in under 3 s, a ~3° lag at
// full slew — deliberately reads like a physical meter movement). Only the
// needle is damped; the target pointer jumps instantly (a target is a set
// point, not a measurement). Repaint is gated to FRAME_MS while converging and
// QUIESCENT_REFRESH_MS when parked, so an idle face costs ~1 repaint/s.
#define NEEDLE_K 0.06f
#define NEEDLE_DEADBAND_DEG 0.05f
#define FRAME_MS 33              // ~30 Hz while the needle is moving
#define QUIESCENT_REFRESH_MS 1000
#define LOOP_DELAY_MS 20

// --- buzzer ------------------------------------------------------------------------------
// Two functional beeps, both short: the needle arrives (moving true→false
// within ARRIVE_BEEP_TOLERANCE_DEG of the target, and not via our own stop),
// and the press = stop acknowledgment (published). Set BEEP_ENABLED to 0 to
// compile both out.
#define BEEP_ENABLED 1
#define BEEP_FREQ_HZ 2400
#define BEEP_ARRIVE_MS 30
#define BEEP_STOP_ACK_MS 30
#define ARRIVE_BEEP_TOLERANCE_DEG 1.5f

// --- simulation mode (desk-development build; never the deployed image) ------
// The m5dial-rotctrl1-sim env defines SIM_MODE=1; the normal envs leave it 0.
// The sim build replaces the station side of the wire with a local mast
// model (sim.h): no WiFi, no MQTT, no OTA. The face, knob, coalescer and
// re-baseline logic run the production code against the model, which
// reproduces the bridge's observable quirks (change-deduped /state, braking
// coast on stop, cancelled target still reported after a halt).
#ifndef SIM_MODE
#define SIM_MODE 0
#endif
#if SIM_MODE
// The G-450DC turns a full circle in about 63 s. The sim slews at the same
// rate, so needle damping, tracking lag and arrival timing behave as they
// will on the real mast. A 180-degree test turn takes ~32 s — the patience
// is part of the test. Tune faster only if iterating on face geometry.
#define SIM_ROTATOR_DEG_PER_SEC 5.7f
// A stop halts a little past the measured azimuth, like real mast braking.
// The press re-baseline path in main.cpp exists because of this coast; the
// sim must reproduce it.
#define SIM_STOP_OVERSHOOT_DEG 1.0f
// On the wire the halt /state returns only after the stop crosses the
// broker, the bridge's /cmd queue and the WRC (~100-500 ms). The sim holds
// the halt emit for this long, so the press-vs-detent race the
// stopResyncPending logic guards stays reproducible on the desk: a detent
// inside the window plays out exactly as it will against the live bridge.
#define SIM_HALT_LATENCY_MS 200
// Model-mast park position at boot: mid-range, away from both mechanical
// limits (0 is the hard CCW limit) — both knob directions live from power-on.
#define SIM_BOOT_AZ 180.0f
#endif