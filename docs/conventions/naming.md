# Bridge-naming convention

How the stationa component repos / binaries / systemd services are named.

The station integration model addresses every slot by a **canonical role**
(`radio`, `pa`, `tuner`, `rotator`, `ant-ctrl`…) — never a product name. The device is an
*attribute* of the slot, so swapping the box behind an address changes nothing downstream.
The bridge name carries the device identity; the slot address does not.

---

## Device bridges: `<devtag>-<function>-bridge`

A bridge that fronts one specific piece of hardware is named
`<devtag>-<function>-bridge`:

- `<devtag>` — the **device family / control interface**, not the narrowest model number.
  When a family shares one control API, use the family name — swapping the box within the
  family then changes nothing (the slot/device-agnostic principle applied to the bridge
  name). Use a specific model only when the interface is model-specific.
- `<function>` — the canonical slot role (`tuner`, `pa`, `rotator`, `radio`, `ant-ctrl`…).
- `-bridge` — the suffix shared by all hardware bridges.

> **Function-token hyphenation:** slot roles that themselves contain a hyphen (`ant-ctrl`,
> `ant-switch`) **collapse to a single token** when used as the `<function>` in a bridge
> name. Hyphens in the dir name are pure separators (they map 1:1 to the underscore
> transform in the env-overload prefix), so a multi-word role would otherwise create an
> ambiguous parse. Concretely: `ant-switch` ⇒ `antswitch`, `ant-ctrl` ⇒ `antctrl`. The
> *slot address* on the bus still uses the hyphenated form (`muehle/hf/ant-switch`) —
> that's the contract, not the bridge name. (Note: `ultrabeam-ant-ctrl-bridge` predates
> this rule; it is in the legacy table below and will be brought into line on its own
> rename.)

| Device family / interface | `<devtag>` | Example name |
|---|---|---|
| Flex SmartSDR (6000/8000 series, same API) | `flex` | `flex-radio-bridge` |
| Ultrabeam beam family (same RCU control) | `ultrabeam` | `ultrabeam-ant-ctrl-bridge` |
| ACOM serial amps (shared protocol) | `acom` | `acom-pa-bridge` |
| ATR-1000 / BTR-1000 / N7DDC ATU (shared binary WS) | `atr1k` | `atr1k-tuner-bridge` |
| AF6SA WRC controller | `wrc` | `wrc-rotator-bridge` |
| Pelco-D protocol | `pelcod` | `pelcod-rotator-bridge` |
| WaveShare relay-board family (ESPHome-managed relay expanders, e.g. PCA9554) | `waveshare_relay` | `waveshare_relay-antswitch-bridge` |

> **Deviation — `waveshare_relay`:** the `<devtag>` for this row uses an underscore
> rather than a hyphen (`waveshare_relay` vs. `waveshare-relay`). This is a deliberate
> exception: the rest of the convention is strictly hyphenated, and the `waveshare`
> family name is shared with other WaveShare boards that are not relay controllers.
> Underscoring the *function* suffix (`relay`) within the devtag keeps a future
> `waveshare-display-…` or `waveshare-io-…` family free to land under `waveshare-…`
> without colliding. The dir name, env overload prefix, systemd unit, and binary all
> follow from the devtag as usual.

### Derived names (must follow)

- **Env-overload prefix** — the dir name uppercased, hyphens → underscores:
  `atr1k-tuner-bridge` → `ATR1K_TUNER_BRIDGE_*`
  (e.g. `ATR1K_TUNER_BRIDGE_MQTT_PASSWORD`). The systemd `EnvironmentFile` matches.
- **systemd service, install dir, service user** — all equal the dir name
  (`atr1k-tuner-bridge`, `/opt/atr1k-tuner-bridge`, user `atr1k-tuner-bridge`).
- **Go module / binary** — equal the dir name (`module atr1k-tuner-bridge`,
  binary `atr1k-tuner-bridge`). Internal package dirs stay hyphen-free (`internal/tuner`,
  `internal/bridge`) per Go convention.

---

## Exceptions (keep descriptive, no device prefix)

- **Logic slots** with no device — `antennaselect` (the reconciler), `hadiscovery` (the HA
  discovery consumer). They are not device bridges.

---

## Legacy names (rename TODO, deferred)

The existing bridges predate this convention. They are **not force-renamed** in place —
renaming touches live systemd units, install dirs, deploy paths, and every doc reference,
so it is a deliberate, separate migration. New bridges adopt the convention starting with
`atr1k-tuner-bridge`. When the legacy rename happens, use the **family tag** (not the model
number) per the rule above.

| Current | Convention-compliant (target) | Kind |
|---|---|---|
| `flexbridge` | `flex-radio-bridge` | device (legacy; SmartSDR family) |
| `ultrabridge` | `ultrabeam-ant-ctrl-bridge` | device (legacy; Ultrabeam family) |
| `acombridge` | `acom1200s-pa-bridge` | device (legacy; ACOM serial family; **renamed model-specific by choice, deviating from the family-tag rule above**) |
| `wrcrotorbridge` | `wrc-rotator-bridge` | device (legacy; fronts the WRC controller; **renamed**) |
| `pelcobridge` | `pelcod-rotator-bridge` | device (legacy; Pelco-D protocol family) |
| `antennaselect` | — | logic slot (exception) |
| `hadiscovery` | — | logic slot (exception) |
| _(new)_ `atr1k-tuner-bridge` | `atr1k-tuner-bridge` | device (convention; ATR-1000 / N7DDC family) |
| _(renamed)_ `antswitchbridge` | `waveshare_relay-antswitch-bridge` | device (formerly contract-first exception; **renamed 2026-07** to follow the family-tag pattern; `_` in `waveshare_relay` is a recorded deviation, see §1) |