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

| Device family / interface | `<devtag>` | Example name |
|---|---|---|
| Flex SmartSDR (6000/8000 series, same API) | `flex` | `flex-radio-bridge` |
| Ultrabeam beam family (same RCU control) | `ultrabeam` | `ultrabeam-ant-ctrl-bridge` |
| ACOM serial amps (shared protocol) | `acom` | `acom-pa-bridge` |
| ATR-1000 / BTR-1000 / N7DDC ATU (shared binary WS) | `atr1k` | `atr1k-tuner-bridge` |
| AF6SA WRC controller | `wrc` | `wrc-rotator-bridge` |
| Pelco-D protocol | `pelcod` | `pelcod-rotator-bridge` |

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
- **Contract-first, model-agnostic bridges** — `antswitchbridge` fronts a generic 1:N switch
  by contract, not a specific product, so it stays function-named. If a second switch model
  appears later, split it then (e.g. `<devtag>-ant-switch-bridge`).

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
| `wrcrotorbridge` | `wrc-rotator-bridge` | device (legacy; fronts the WRC controller) |
| `pelcobridge` | `pelcod-rotator-bridge` | device (legacy; Pelco-D protocol family) |
| `antswitchbridge` | `ant-switch-bridge` (exception: contract-first) | exception |
| `antennaselect` | — | logic slot (exception) |
| `hadiscovery` | — | logic slot (exception) |
| _(new)_ `atr1k-tuner-bridge` | `atr1k-tuner-bridge` | device (convention; ATR-1000 / N7DDC family) |