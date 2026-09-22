# muehle/uhf/radio — MQTT wire contract

The IC-9700 bridge's four planes (station integration model §5). The
machine truth is the implementation (`internal/bridge`); this document is
the reader-friendly contract. Retention: `/meta`, `/state`, `/status` are
retained; `/cmd` is one-shot (KTD6 — a retained arm permit would defeat the
fail-disarm, R11).

## /state

Hybrid shape (R5): top-level active-TX fields PLUS `main`/`sub` detail —
one device is one slot and duplex truth stays in one snapshot. The
top-level fields mirror the TX VFO: **SUB while satellite mode is on**
(SUB is the uplink), MAIN otherwise (KTD-10 satellite semantics).

Per-state payload rules (R6, extended 2026-09-21): while `session_state !=
"live"` OR `radio_responding` is false, the radio-measured fields are OMITTED
(never zeroed, never frozen — a live-but-deaf session, e.g. the radio in
network standby, hides them exactly like an off-session); the stamped
bridge-held fields remain.

| Key | Type | Present | Meaning |
|---|---|---|---|
| `ts` | RFC3339 | always | publish stamp (freshness heartbeat: 60 poll ticks) |
| `session_state` | enum | always | `idle` \| `connecting` \| `live` \| `error` — the idle-vs-fault discriminator |
| `armed` | bool | always | the bridge-held TX permit (drops on session loss/restart, R11) |
| `audio_demand` | bool | always | the audio_on demand is set (TTL-bounded; refreshed by the consumer's audio_on heartbeats) |
| `device_online` | bool | always | **CI-V control-session liveness** (R16): `false` on healthy idle — NOT device reachability |
| `radio_responding` | bool | live | the radio answered CI-V on the latest poll — false while live means the radio is deaf (standby). NOT device_online, NOT reachability. Clears after 2 consecutive zero-answer polls |
| `selected_vfo` | `main`\|`sub` | after first read | bridge-held, survives off-session |
| `error` | string | on fault | last rejection/session fact (taxonomy below) |
| `freq_hz` | int | live ∧ responding | active-TX VFO frequency (Hz) |
| `band` | enum | live ∧ responding | `2m` \| `70cm` \| `23cm` |
| `mode` | enum | live ∧ responding | `cw` \| `usb` \| `lsb` \| `am` \| `fm` \| `data` |
| `tx` | `tx`\|`rx` | live | canonical string enum (R5) — stays published while deaf (safety mirror) |
| `satellite` | bool | live ∧ responding | radio satellite mode |
| `main` / `sub` | object | live ∧ responding | `{band, freq_hz, mode, data_mode, preamp, attenuator}` |
| `s_meter` | int 0-255 | live ∧ responding | S9 = 120 |
| `swr`, `alc` | int 0-255 | live ∧ responding | meter values (≤1 Hz, dedup'd, KTD-8) |
| `tx_power` | int | live | reserved (meter read lands with the bench pin) |

### /state.error taxonomy (observed facts only)

- `ptt rejected: not armed` — PTT without the arm permit (R10)
- `ptt rejected: session not live` — armed but no CI-V session (R10)
- `freq rejected: out of band for sub` — SUB has no 23cm (R7)
- `radio: login refused …` — the radio rejected the credentials
- `sat_mode rejected: tx is on or armed` — R10
- `ptt-off undeliverable: session unavailable` — a PTT-off could not reach the radio
- `cmd payload too large` — FIXED string: the 4 KB size gate never echoes attacker bytes
- `stale cmd (age …, bound 30s)` — the ts gate

## /cmd

One-shot (KTD6): QoS-0 subscription (no offline backlog), clear-after-
execute-or-reject (the empty-payload echo guard), ts gate (30 s, unstamped
tolerated), 4 KB size gate, rejections clipped to 200 runes.

Actions (value-key convention; `vfo` targets per-VFO actions — there is no
select-VFO action, R9):

| Action | Value | Notes |
|---|---|---|
| `set_freq` | Hz as string/number | per-VFO (`vfo` required); band-validated (SUB: 2m/70cm only) |
| `set_mode` | `cw\|usb\|lsb\|am\|fm` | per-VFO; DV/DD/data rejected as unsupported (R7) |
| `set_data` | `on`\|`off` | per-VFO data-mode modifier |
| `sat_mode` | `on`\|`off` | rejected while `tx=="tx"` or armed (R10) |
| `set_power` | 0-255 | RF output power, selected band's VFO (`14 0A` — NEVER the main-power `18 0x`) |
| `arm` / `disarm` | — | the TX permit; arm-while-idle is the on-demand session's connect trigger (R1/R14) |
| `ptt` | `on`\|`off` | R10 gate: `armed ∧ session_state=live` at send time; toggle, not hold-to-talk |
| `audio_on` / `audio_off` | — | opens/closes the :50003 RX audio receive stream and publishes its PCM to `audio.publish_addr` (S16LE 48 kHz mono UDP); the demand is TTL-bounded (`audio.demand_ttl`) and refreshed by repeated `audio_on` — a dead consumer releases the session (KTD-2) |
| `power_on` | — | blind untracked wake frame `1A 05 02 01` (IC-9700 remote wake from standby; frame configurable via `audio.power_on_frame`); needs a live session — handshake succeeds against a standby radio; refused while `tx=="tx"` |

## /meta

Retained birth certificate. `role: "radio"`, `capabilities: {bands: [2m,
70cm, 23cm], modes: [cw, usb, lsb, am, fm, data], bias_t: true,
satellite: true, vfos: [main, sub]}`. The expose is **read-only** (v1
posture, R8): freq (number/measurement), mode (enum, options_ref
capabilities.modes), tx (boolean, on "tx" / off "rx"), s_meter, armed,
session_state, error. No PTT/arm widgets in HA; an expose `actions[]` is
additive later without breaking anything, and the `armed ∧ live`
precondition is derivable from /state so future trackers are not blocked.

## /status

Retained `online`/`offline` (LWT + clean-exit self-publish).

## Session model (consumer-facing summary)

The radio's single LAN session is on-demand (KTD-2): the bridge connects
only on work (a cmd demand or the arm permit) and disconnects after an
idle timeout, leaving the radio free for manual wfview use. Connect
attempts are spaced (3 × 30 s) and never storm; a failed series rejects
the demand into `/state.error` and decays to idle. Every reconnect is a
full re-login.
