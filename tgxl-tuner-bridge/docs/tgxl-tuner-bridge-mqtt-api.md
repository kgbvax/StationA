# MQTT schema — tgxl-tuner-bridge

Implements the `tuner` slot (`muehle/hf/tuner`) of the station integration model
(`../../docs/station-integration-model.md` §7.1) for a FlexRadio Tuner Genius XL, 1x3 variant.
Replaced the ATR-1000 bridge. Authoritative for the on-the-wire contract;
derived from `internal/bridge/bridge.go` and `internal/tgxl/`.

## 1. Topics

`<site>/<station>/<slot>/{meta,state,status,cmd}` — defaults `muehle/hf/tuner`. MQTT 3.1.1,
user `hf`, client ID `muehle-hf-tuner`.

| Suffix | Retained | Direction | Purpose |
|--------|----------|-----------|---------|
| `/meta` | yes | bridge → bus | birth certificate, republished on every (re)connect and when `info` arrives |
| `/state` | yes | bridge → bus | JSON snapshot, change-deduped |
| `/status` | yes | broker LWT | `online` / `offline` (the **bridge**, not the TGXL) |
| `/cmd` | **no** | bus → bridge | one-shot intent |

## 2. `/meta`

```json
{
  "schema": "1.0", "role": "tuner",
  "device": { "model": "Tuner Genius XL", "serial": "210387-1", "firmware": "1.1.8" },
  "link": "ethernet", "location": "bauwagen", "host": "scmino",
  "capabilities": { "inline": true, "tune_modes": ["auto"], "antennas": 3 },
  "expose": { "...": "fields swr, fwd, inline*, operate*, antenna*, settling, ptt, fault, message, device_online; action tune" }
}
```
`*` = writable (`set_inline` / `set_operate` bool, `set_antenna` number 1–3). `serial` / `firmware` appear once
`info` has been read.

## 3. `/state`

```json
{ "ts": "2026-11-06T10:30:00Z", "inline": true, "operate": true, "antenna": 2,
  "swr": 1.22, "fwd": 10, "ptt": false, "settling": false,
  "relay_c1": 1, "relay_l": 2, "relay_c2": 3, "device_online": true }
```

| Field | Meaning |
|-------|---------|
| `inline` | in the RF path: operate **and** not bypassed |
| `operate` | OPERATE (true) / STANDBY (false; standby disconnects all antennas) |
| `antenna` | selected antenna 1..3 (1x3 variant) |
| `swr` | ratio, from the API's return-loss dB (`Γ=10^(-RL/20)`, `(1+Γ)/(1-Γ)`, clamped 1..99) |
| `fwd` | forward watts, from dBm (`10^((dBm-30)/10)`, < 1 W reads 0) |
| `ptt` | PTT asserted on the tuner |
| `settling` | an autotune is in progress (raised before the carrier; PLC #1 gates the PA arm on it) |
| `fault` | `tune timeout` (tuning > 20 s) or `<cmd>: code N` (non-zero TGXL reply); cleared by the next successful command / tune |
| `message` | TGXL warning/info text (`M|` line); omitted when cleared |
| `relay_c1/l/c2` | matching-network relay positions 0–255 (diagnostic) |
| `device_online`, `error` | the TGXL link (distinct from `/status`) |

No `freq_hz`/band/mode: those are radio concerns. `l_uh`/`c_pf` (ATR-1000) no longer exist.

## 4. `/cmd`

All arguments ride under `value`.

| Action | value | TGXL command |
|--------|-------|--------------|
| `set_inline` | bool | true: `operate set=1` (only if in standby) then `bypass set=0`; false: `bypass set=1` |
| `set_operate` | bool | `operate set=1|0` |
| `set_antenna` | 1..3 | `activate ant=N` |
| `tune` | `"auto"` (`"mem"`/`"full"` accepted as aliases) | `autotune` |

Unknown actions / bad values are logged and ignored.

**Hazard:** `tune` makes the TGXL key the FLEX over the LAN. The station has no PGXL, so nothing
protects the ACOM 1200S; it must be in standby. No bridge-side gate.

## 5. TGXL protocol summary

Polled adaptively: `tuner.poll_interval` 250 ms RX, `tuner.tx_poll_interval` 50 ms while PTT is asserted.

TCP 9010; banner `V<fw> [AUTH]`; `C<seq>|cmd args\n` → `R<seq>|<code>|<msg>`; `status` →
`S<seq>|status fwd= peak= max= swr= pttA= … antA= state= active= tuning= bypass= relayC1= relayL= relayC2=`;
`M|<text>`. UDP 9010 broadcast `TunerGenius ip= v= serial= nickname=`. Full detail:
`TG.XL.API.Commands.pdf`.
