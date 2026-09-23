# muehle/uhf/radio — MQTT wire contract

The IC-9700 bridge's four planes (station integration model §5). The
machine truth is the implementation (`internal/bridge`); this document is
the reader-friendly contract. Retention: `/meta`, `/state`, `/status` are
retained; `/cmd` is one-shot (KTD6).

**Receive-only posture (2026-09 pivot).** There is no LAN CI-V command
path: remote TX control (arm/PTT/tuning) was removed from the bridge and
the console. The LAN session exists solely to carry the demand-driven
:50003 RX audio stream; telemetry is read over the dedicated **serial
CI-V** port (read-only) and broadcast on change. If control is ever needed
again, it will be done via serial CI-V — not LAN.

## /state

Retained snapshot. Publishes ON CHANGE (telemetry or demand flip), with a
60 s freshness heartbeat for an unchanged snapshot (KTD14) — the ts is
never stale.

| Key | Type | Present | Meaning |
|---|---|---|---|
| `ts` | RFC3339 | always | publish stamp (on change + 60 s heartbeat) |
| `session_state` | enum | always | CAPTURE-session state: `idle` \| `connecting` \| `live` \| `error` — the idle-vs-fault discriminator for the audio session |
| `audio_demand` | bool | always | the audio_on demand is set (TTL-bounded; refreshed by the consumer's audio_on heartbeats) |
| `monitor` | bool | always | the serial CI-V telemetry reader is on |
| `device_online` | bool | always | **capture liveness** (R16, redefined 2026-09): `session_state == "live"` — `false` on healthy idle, NOT device reachability |
| `error` | string | on fault | last rejection/session fact (taxonomy below) |
| `radio_responding` | bool | monitor on | the radio answers serial CI-V — false while the monitor is on means the radio is deaf (standby) or the serial port is down. Clears after 2 consecutive unanswered poll cycles |
| `freq_hz` | int | monitor on ∧ responding | radio frequency (Hz) — transceive broadcast or read |
| `band` | enum | monitor on ∧ responding | `2m` \| `70cm` \| `23cm` |
| `mode` | enum | monitor on ∧ responding | `cw` \| `usb` \| `lsb` \| `am` \| `fm` \| `data` |
| `satellite` | bool | monitor on ∧ responding | radio satellite mode (read-only `16 5A`) |
| `s_meter` | int 0-255 | monitor on ∧ responding | S9 = 120 |
| `swr`, `alc` | int 0-255 | monitor on ∧ responding | meter values (polled, dedup'd — published only on change) |
| `tx_power` | int | monitor on ∧ responding | ONLY if the bench proves the CI-V read; the key never appears until then |

Removed 2026-09: `armed`, `tx`, `selected_vfo`, `main`, `sub` — no TX
state exists anymore.

### /state.error taxonomy (observed facts only)

- `radio: login refused …` — the radio rejected the RS-BA1 credentials
- `monitor unavailable: serial.device not configured` — monitor_* without a serial port
- `power_on not configured (serial.device empty)` — ditto for the wake
- `civserial: …` — serial port open/read failures surfaced verbatim
- `cmd payload too large` — FIXED string: the 4 KB size gate never echoes attacker bytes
- `stale cmd (age …, bound 30s)` — the ts gate
- `unknown cmd action "…"` — the action set is the five below; everything else rejects

## /cmd

One-shot (KTD6): QoS-0 subscription (no offline backlog), clear-after-
execute-or-reject (the empty-payload echo guard), ts gate (30 s, unstamped
tolerated), 4 KB size gate, rejections clipped to 200 runes.

Exactly five actions — no value arguments, the action name is the intent:

| Action | Notes |
|---|---|
| `audio_on` / `audio_off` | opens/closes the :50003 RX audio receive stream and publishes its PCM to `audio.publish_addr` (S16LE 48 kHz mono UDP); the demand is TTL-bounded (`audio.demand_ttl`) and refreshed by repeated `audio_on` — a dead consumer releases the session (KTD-2). The audio demand is the session's only hold source |
| `power_on` | blind serial CI-V wake frame `1A 05 02 01` (IC-9700 remote wake from standby; configurable via `serial.power_on_frame`); requires `serial.device` |
| `monitor_on` / `monitor_off` | toggles the serial CI-V telemetry reader (sticky — no TTL: the serial wire is dedicated to this process); requires `serial.device` |

## /meta

Retained birth certificate. `role: "radio"`, `capabilities: {bands: [2m,
70cm, 23cm], modes: [cw, usb, lsb, am, fm, data]}`. The expose is
**read-only and control-free**: freq_hz, mode, s_meter, swr, alc,
audio_demand, monitor, radio_responding, session_state, error. No `tx`,
no `armed`, no actions — nothing in HA can key or steer the radio.

## /status

Retained `online`/`offline` (LWT + clean-exit self-publish).

## Session model (consumer-facing summary)

The radio's single LAN session is on-demand (KTD-2): the bridge connects
only on an audio demand and disconnects after an idle timeout, leaving the
radio free for manual wfview use. Connect attempts are spaced (3 × 30 s)
and never storm (a login-busy refusal is a single attempt); a failed
series rejects the demand into `/state.error` and decays to idle. Every
reconnect is a full re-login. The audio session carries NO CI-V commands:
the CI-V data stream is not opened (NoCIVData); telemetry is serial-only.
