# CLAUDE.md — tgxl-tuner-bridge

tgxl-tuner-bridge bridges a **FlexRadio Tuner Genius XL** (TGXL, 1x3 / 3WAY variant, `192.168.1.145`)
to MQTT using the station integration model (slot `muehle/hf/tuner`). It replaced
`atr1k-tuner-bridge` (ATR-1000) on the same slot. It polls the tuner's line-based TCP API
(port 9010), publishes a canonical tuner state snapshot, and dispatches `/cmd`
(`set_inline`, `set_operate`, `set_antenna`, `tune`) back to the tuner. Read-write. It does not
drive TX itself, except that **`tune` makes the TGXL key the FLEX over the LAN** (see hazard).

Naming follows `<devtag>-<function>-bridge` (`../docs/conventions/naming.md`). Vendor docs:
`docs/TG.XL.API.Commands.pdf`, `docs/Tuner-Genius-XL-User-Guide.pdf`.

## Commands

```bash
go build ./cmd/tgxl-tuner-bridge
go test ./... -race            # no network/hardware needed (fake TGXL TCP server)
go vet ./... && gofmt -s -w .
./deploy.sh                    # cross-compile, ship, install as hardened systemd service (scmino)
```

Flags: `-config` (default `/etc/tgxl-tuner-bridge/config.toml`), `-log.level`, `-debug` (log TCP I/O).

## Architecture

TGXL TCP :9010 → `internal/tgxl` → `internal/bridge` → MQTT.

1. `cmd/tgxl-tuner-bridge/main.go` — flags, config, MQTT connect + LWT, bounded `/cmd` worker,
   reconnect loop with backoff (`devLoop`), UDP discovery when `tuner.address` is empty.
2. `internal/tgxl` — `protocol.go` (pure: framing, R/S/M/V parsing, unit conversions),
   `device.go` (dial, banner/AUTH, `info`, 250 ms `status` poll, seq-matched replies, state folding,
   settling/tune tracking), `discovery.go` (UDP 9010).
3. `internal/bridge` — `/meta` (role `tuner`), retained `/state` with change-dedup, `/cmd` dispatch.
4. `internal/config` — TOML + `TGXL_TUNER_BRIDGE_*` env.

Protocol: `C<seq>|cmd args\n` → `R<seq>|<code>|<msg>`; `status` answers `S<seq>|status k=v …`;
`M|text` are async messages (empty = cleared). The TGXL is polled, not streamed (no subscribe on :9010; the
radio-side meter relay lacks `peak` and stops RL in bypass, so it is not used). Poll rate is adaptive,
like AetherSDR: `tuner.poll_interval` (250 ms) while receiving, `tuner.tx_poll_interval` (50 ms) while
`pttA`/`pttB` is 1; one request in flight, a lost reply is abandoned after 1 s. A 3 s read
silence ends the run (dead link) and the loop reconnects; `/status` stays `online`, only
`/state.device_online` flips.

## Contract notes (consumers depend on these)

- `/state` always carries `settling` and `device_online` (m5stamp-hf-ctrl treats a missing
  `settling` as true and holds the PA arm open). `settling` is raised *before* the autotune carrier.
- `inline` = operate && !bypass. "Bypass" never uses STANDBY (standby disconnects all antennas).
- `set_inline` takes a real JSON bool (antennaselect `[tuner_follow]`, hf_console).
- `tune` accepts `"auto"`; the old ATR `"mem"`/`"full"` are accepted as aliases (hf_console
  tablets with an old APK).
- `fwd`/`swr` are converted from the API's dBm / return-loss-in-dB (`protocol.go`). **Units are from
  the PDF only; verify live with a known carrier** (`printf 'C1|info\nC2|status\n' | nc 192.168.1.145 9010`).
- `/cmd` is not retained.

## HAZARD: tune and the PA

`autotune` keys the FLEX through the TGXL (LAN PTT). The station has no PGXL, so nothing
puts the ACOM 1200S into standby during the tune. There is deliberately no bridge-side PA gate (owner
decision, 2026-11-06): the operator must have the PA in standby before any tune. Open question:
whether a TGXL tune also shows in flexbridge's `sub atu` `tuning` (which m5stamp gates on).

## Config and deploy

Password only via EnvironmentFile `/etc/tgxl-tuner-bridge/tgxl-tuner-bridge.env`
(`TGXL_TUNER_BRIDGE_MQTT_PASSWORD`). Seed-once `deploy.sh`; unit hardened like the other network-only
bridges (`PrivateDevices=true`, `AF_INET AF_INET6`). Target scmino. Config: `config.example.toml`.
Wire contract: `docs/tgxl-tuner-bridge-mqtt-api.md`. Shared docs: `../docs/`.
