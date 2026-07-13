# Future feature: CAT-emulator absolute band-follow for acom1200s-pa-bridge

**Status:** parked (2026-07-10). The amp currently band-follows via the proprietary
`0x09` next/prev walk; we accept the walk for now (RF-sense auto-banding trips on
the first TX of a new band, so the serial pre-positioning via antennaselect stays).
This document is the design to revisit when we want to eliminate the relay wear from
walking through intermediate bands.

## Context / motivation

acom1200s-pa-bridge changes the ACOM 1200S amp's band by **walking** next/prev steps over the
proprietary serial protocol (command `0x09`), because that protocol exposes only
relative band changes. Each intermediate band is a real LPF relay actuation — e.g.
20m→15m passes through 17m — so it wears relays. The amp already auto-bands by RF
sense, but that trips on the first TX of a new band, so antennaselect still
pre-positions it over serial (the walk).

This plan replaces the walk with an **absolute band-follow**: acom1200s-pa-bridge emulates a
Kenwood/Elecraft CAT radio on a **second serial port** wired to the amp's dedicated
CAT band-data jack. The amp band-follows via its own CAT logic (one absolute switch,
no walk, no first-TX trip). Band truth still comes from the MQTT bus — antennaselect
keeps emitting `{"action":"set_band","value":"15m"}` exactly as today; only
acom1200s-pa-bridge's *implementation* of `SetBand` changes.

This is the path the docs already anticipate: `band_source: "cat"` is a defined
model value (`../stationa/docs/station-integration-model.md:231-234`), and
`docs/pa-mqtt-api.md:238` says *"Band follows the radio via CAT in normal operation;
this command is for manual override."* The code never implemented it; this would.

**Why this shape:** keeping the bus contract unchanged (no new `set_freq` PA intent,
no `freq_hz` on the PA bus, no antennaselect change, no model soft-binding edit) is a
pure acom1200s-pa-bridge-internal swap. The amp only needs the *band* from CAT (the 1200S has
no per-frequency ATU memory), so an in-band midpoint frequency is sufficient — there
is no need to carry the radio's real `freq_hz` onto the PA slot.

## Decisions confirmed with the operator (2026-07-10)

- **CAT jack:** dedicated rear jack, separate from the PC/RS232 telemetry port. Feasibility gate passed.
- **Signal levels:** amp supports **both RS232 and TTL** (selectable via its CAT-type setting). Pick the level to match the adapter wired. **BCD band-data** is also available but **not chosen** — the serial Kenwood path is preferred. (BCD noted as a simpler future alternative below.)
- **CAT protocol:** **Elecraft K3 / Kenwood TS-480 ASCII** (Kenwood `IF;`/`FA;` poll-responder). The `catv` package is protocol-pluggable so this stays swappable.

## Hardware instructions

### What you need
1. **A 2nd USB-serial adapter** for shari, matching the level you set the amp to:
   - RS232 → a standard USB-RS232 cable (Prolific `067b:2303` or FTDI `0403:6001`). The existing Prolific is already in use for telemetry, so use a **different vendor/serial** (e.g. an FTDI USB-RS232) to get a distinct `/dev/serial/by-id`.
   - TTL → an FTDI TTL-232R-3V3 (or 5V) cable. **Do not** wire a full RS232 (±12V) adapter into a jack set to TTL — it can damage the amp. Set the amp's CAT type to match the adapter you use.
2. **A cable from the adapter to the amp's CAT jack.** Pinout comes from the **ACOM 1200S user manual** (rear-panel CAT connector drawing) — this repo only has the serial-protocol PDF, which does not document the CAT jack pins. Confirm TX/RX/GND (and CTS/RTS if the chosen mode needs them) before wiring. Kenwood CAT is 3-wire (TX/RX/GND), no hardware handshake — a simple TX/RX/GND crossover is enough once levels match.
3. **shari-side:** plug the 2nd adapter into the Pi, identify its stable symlink:
   ```bash
   ls -l /dev/serial/by-id/        # note the new adapter's by-id (must differ from the Prolific)
   ```
   Use that by-id as `[cat]` `port` (or `ACOM1200S_PA_BRIDGE_CAT_PORT`) — same convention as the telemetry port (`internal/config/config.go:79`).

### Configure the amp's CAT interface (one-time, front panel)
Set the amp's CAT interface (per the ACOM 1200S menu — or via proprietary `0x05` once the optional step below is done) to:
- **Command set:** Elecraft K3 / Kenwood TS-480
- **Interface type:** RS232 *or* TTL — **must match the adapter you wired**
- **Baud:** 9600 (default; Kenwood supports 9600/4800 — set both sides equal)
- **Radio address / controller address:** N/A for Kenwood (ASCII, no addressing)

### udev / permissions on shari
The systemd unit's `DeviceAllow=char-ttyUSB rw / char-ttyACM rw / char-tty rw`
(`deploy.sh:226-228`) and `SupplementaryGroups=dialout` already cover **any** tty, so
the 2nd adapter is auto-allowed under the sandbox — no unit change. The udev rule
(`deploy.sh:261-266`) currently pins the Prolific (`067b`) to `dialout`. If the 2nd
adapter is a different vendor (e.g. FTDI `0403`), either:
- broaden the rule to `SUBSYSTEM=="tty", SUBSYSTEMS=="usb", GROUP="dialout", MODE="0660"` (any USB-serial → dialout), or
- add a second `ATTRS{idVendor}=="0403"` line.

### Verification spike (on the bench, before trusting the integration)
Before final wiring, confirm two things empirically with a tiny standalone responder
(`cmd/spike-cat`, **not** in the main build) wired to the amp's CAT jack:
1. The amp **polls** (sends `IF;` or `FA;` periodically) → emulator = responder is correct. If it never polls and only reacts to unsolicited frames, switch `catv.Emulator.Run` to a publisher-on-ticker (single flagged design point in the `catv` package).
2. The amp's **band actually changes** when the responder's reported frequency crosses a band edge, and it follows on the first TX without tripping (the whole point).

The spike also de-risks the CAT-jack pinout and levels before committing a cable.

## Implementation outline

### 1. New package `internal/catv` — Kenwood CAT emulator
- `internal/catv/bands.go` — band→midpoint-Hz table (the amp only needs the band):
  ```go
  var bandMidpointHz = map[string]int64{
      "160m": 1_900_000, "80m": 3_750_000, "40m": 7_150_000, "30m": 10_125_000,
      "20m": 14_175_000, "17m": 18_110_000, "15m": 21_225_000, "12m": 24_940_000,
      "10m": 28_500_000, "6m": 52_000_000,
  }
  ```
  Reuse `acom.BandOptions` (`internal/acom/decoders.go:11`) for the canonical list.
- `internal/catv/protocol.go` — pluggable wire protocol (so Kenwood can be swapped later):
  ```go
  type Protocol interface {
      ReadPoll(r io.Reader) (Command, error)        // parse one inbound request (e.g. "IF;")
      FrequencyReply(freqHz int64) ([]byte, error)   // build the reply carrying freq
  }
  type CommandKind int
  const (KindUnknown CommandKind = iota; KindReadFreq; KindReadMode)
  type Command struct{ Kind CommandKind }
  ```
- `internal/catv/kenwood.go` — Kenwood ASCII `Protocol`. Reads until `;`; answers
  `IF;`, `FA;`, `FB;` defensively (the spike confirms which the amp sends). `FA` reply
  = `"FA" + 11-digit zero-padded Hz + ";"` (e.g. 14.175 MHz → `FA00014175000;`). `IF`
  reply = Kenwood IF frame with the 11-digit freq in the leading fields, others zeroed.
  Pure ASCII, no BCD — simpler than CI-V.
- `internal/catv/emulator.go` — owns the 2nd serial port + current freq:
  ```go
  type Emulator struct { portPath string; baud int; proto Protocol; log Logger
      mu sync.RWMutex; freqHz int64; port serial.Port }
  func New(portPath string, baud int, proto Protocol, log Logger) *Emulator
  func (e *Emulator) Open() error                  // go.bug.st/serial at baud, 8N1 (already a dep)
  func (e *Emulator) Run(ctx context.Context) error // poll-responder loop (spike confirms mode)
  func (e *Emulator) Close()
  func (e *Emulator) SetFrequency(freqHz int64)
  func (e *Emulator) SetBand(band string) error     // band→midpoint→SetFrequency; primary entry
  ```
  - `Run` delegates to `runOnce(ctx, rw io.ReadWriter)` so the loop is unit-testable
    with an in-memory pipe (no serial).
  - `Logger` is a local 3-method interface mirroring `acom.Logger` (`device.go:15-19`)
    so `catv` does not import `acom` (avoids a cycle; `acom` imports `catv`, not vice-versa).
  - **Spike-flagged point:** if the amp does not poll, add a `Publish(ctx)` ticker mode
    selected by a `mode` field. Only `Run`'s internals change; the public API is stable.

### 2. `internal/acom/device.go` — route SetBand to the emulator
Add an optional emulator via a small interface (keeps `acom` tests serial-free):
```go
type catBandSetter interface { SetBand(band string) error }
type Device struct { /* …existing… */ cat catBandSetter }   // nil = walk fallback
func (d *Device) SetCATEmulator(c catBandSetter)            // call before Open
```
In `SetBand` (`device.go:271-296`): if `d.cat != nil`, call `d.cat.SetBand(band)` and
return (absolute follow); else the **existing 0x09 walk code is kept verbatim as the
fallback** when `[cat]` is not configured. Zero-value `Device` (from `acom.New`)
behaves exactly as today — existing `device_test.go` / `bridge_test.go` stay green.

### 3. `internal/bridge/bridge.go` — dynamic `band_source`
`metaCapabilities.BandSource` is hardcoded `"rf_sense"` at `bridge.go:196`. Add
`BandSource string` to `bridge.Config` (`bridge.go:25-43`), use `b.cfg.BandSource` at
line 196 (empty → `"rf_sense"` default for test safety).

### 4. `internal/config/config.go` — new `[cat]` block
```go
type CATConfig struct {
    Enabled  bool   `toml:"enabled"`
    Port     string `toml:"port"`      // /dev/serial/by-id/…
    Protocol string `toml:"protocol"`  // "kenwood" (default) | "civ" (future)
    Baud     int    `toml:"baud"`       // 9600 for Kenwood; NOT the proprietary 9600
}
type Config struct { /* … */ CAT CATConfig `toml:"cat"` }
```
- `Defaults()`: `CAT{Enabled:false, Protocol:"kenwood", Baud:9600}`.
- `Validate()`: when `Enabled`, require `Port` + `Baud`; `Protocol` ∈ {`"","kenwood"`}.
- `applyEnv()`: `ACOM1200S_PA_BRIDGE_CAT_ENABLED`/`_PORT`/`_BAUD` (in the EnvironmentFile so
  TOML stays shareable) — mirrors the existing `ACOM1200S_PA_BRIDGE_SERIAL_PORT` pattern.

### 5. `cmd/acom1200s-pa-bridge/main.go` — construct + lifecycle
In `run()` after `acom.New` (`main.go:68`): if `cfg.CAT.Enabled`, build a
`catv.Emulator` (Kenwood protocol) and `dev.SetCATEmulator(catEmu)`. In `runOnce`
(`main.go:223-243`): open the CAT port and `go catEmu.Run(catCtx)` alongside the
telemetry loop, tied to the same (re)connect cycle so `PublishMeta` (which reports
`band_source`) runs after the CAT port is up. Set `bridge.Config.BandSource = "cat"`
when `cfg.CAT.Enabled`, else `"rf_sense"`.

### 6. Optional / secondary — program the amp's CAT type via proprietary `0x05`
The `0x05` command (PDF p.33) sets the amp's CAT type/set/baud. It is **not** modeled
in `internal/acom/protocol.go` (only `0x02`/`0x09` are). **Defer** unless the spike
shows the panel setting is onerous: the 0x05 frame layout must be confirmed from the
PDF and may not take effect at runtime. Front-panel config is a one-time action and
avoids a fragile runtime write. Mark optional; not on the critical path.

### 7. Tests (no hardware)
- `catv/bands_test.go`: every `BandOptions` midpoint falls inside its ITU band edges;
  `SetBand("11m")` errors, `SetBand("20m")` → `freqHz==14_175_000`.
- `catv/kenwood_test.go`: `FrequencyReply` builds exact `FA00014175000;` / `IF…;`
  frames; `ReadPoll` parses `IF;`/`FA;` and returns `KindUnknown` for garbage without
  panicking (via `strings.NewReader`, no port).
- `catv/emulator_test.go`: drive `runOnce` against an `io.Pipe` — feed `IF;`, assert
  the reply carries the current freq.
- `acom/device_test.go`: new `TestSetBandViaCAT` — attach a fake `catBandSetter`,
  assert no 0x09 frames written.
- `config/config_test.go`: `[cat]` disabled by default; enabled+no port → Validate
  errors; `ACOM1200S_PA_BRIDGE_CAT_PORT` override; baud default 9600.
- Existing `bridge_test.go` `fakeCommander` (`bridge_test.go:20-28`) and
  `TestCmdDispatchSetBand` (`bridge_test.go:286`) unchanged.

### 8. Docs
- `docs/pa-mqtt-api.md`: `band_source` now `cat` when `[cat]` enabled; reconcile the
  set_band section (lines ~231-239) to describe absolute-via-CAT vs walk-fallback.
- `../stationa/docs/conventions/deployment.md` serial addendum (after the udev
  section): two-adapter case — group/`DeviceAllow` already cover a 2nd tty, by-id
  must differ, and the udev-broaden vs second-rule choice from the hardware section.

## Verification (end-to-end, when implemented)
1. `go test ./...` and `go test ./... -race` green; `go vet`/`gofmt -s` clean.
2. Cross-compile (`GOOS=linux GOARCH=arm64 …`) and `./deploy.sh` to shari.
3. **With `[cat]` disabled:** behavior identical to today (walk fallback) — confirm
   no regression by changing the radio band and watching the 0x09 walk on `pa/state`.
4. **With `[cat] enabled` (after the spike):** change the FLEX band → antennaselect
   emits `set_band` → acom1200s-pa-bridge sets the emulator freq → amp follows absolutely (one
   switch, no intermediate bands on `pa/state`) → no first-TX trip on the new band.
   Confirm `pa/meta` reports `band_source: "cat"`.
5. **Fallback path:** unplug/disable the CAT adapter → `SetBand` falls back to the
   0x09 walk (or configure `cat.enabled=false`) and confirm the walk still works.

## Implementation sequence
1 `catv/bands.go`+test · 2 `catv/protocol.go`+`kenwood.go`+tests · 3 `catv/emulator.go`+test
· 4 `config` `[cat]`+tests · 5 `acom/device.go` routing · 6 `bridge` `BandSource` ·
7 `main.go` wiring · 8 docs · 9 (optional, post-spike) step 6.

## BCD alternative (not chosen)
The amp's CAT jack also exposes **BCD band data** (4 data lines + ground, band
1..10). Driving 4 GPIO lines on the Pi (or a small USB-GPIO/relay board) would be
even simpler than a serial emulator — no CAT framing at all, just set 4 bits to the
band index. The serial Kenwood path was chosen instead; BCD remains a
lower-complexity fallback if the Kenwood responder proves finicky on the bench.

## Not chosen: `set_freq` PA intent
Adding `set_freq` carrying the real `freq_hz` from antennaselect (which already has
`RadioFreqHz` in `Inputs` but withholds it from the PA today) would require a
contract change to `pa-mqtt-api.md`, a `pa.set_freq ← radio.freq_hz` model soft
binding, and an antennaselect change — all to carry the real frequency, which the
1200S doesn't need (no per-freq ATU memory). The in-band-midpoint approach buys the
same absolute band-follow with a pure acom1200s-pa-bridge-internal swap. Revisit only if the
amp gains per-frequency behavior.

## Critical files (when implemented)
- `internal/acom/device.go` — SetBand routing seam, `catBandSetter` iface, struct field
- `internal/catv/{bands,protocol,kenwood,emulator}.go` — new package
- `internal/config/config.go` — `CATConfig`, Validate, applyEnv
- `cmd/acom1200s-pa-bridge/main.go` — construction, lifecycle, BandSource wiring
- `internal/bridge/bridge.go` — `Config.BandSource`, line 196