# Canonical band and mode reference

This is the single authoritative table for HF/VHF band frequency ranges and canonical
mode names used across all stationa components. Components implement against this
reference; when it changes, each affected component needs a corresponding code update.

Edges are DL / IARU Region 1 allocations. Both edges are inclusive (the high edge is
the round kHz value, e.g. 20m runs 14,000,000–14,350,000 Hz).

---

## HF band frequency ranges

| Band | Low (Hz) | High (Hz) | Low (kHz) | High (kHz) |
|------|----------|-----------|-----------|------------|
| `160m` | 1,800,000 | 2,000,000 | 1800 | 2000 |
| `80m` | 3,500,000 | 4,000,000 | 3500 | 4000 |
| `60m` | 5,351,500 | 5,366,500 | 5351.5 | 5366.5 |
| `40m` | 7,000,000 | 7,300,000 | 7000 | 7300 |
| `30m` | 10,100,000 | 10,150,000 | 10100 | 10150 |
| `20m` | 14,000,000 | 14,350,000 | 14000 | 14350 |
| `17m` | 18,068,000 | 18,168,000 | 18068 | 18168 |
| `15m` | 21,000,000 | 21,450,000 | 21000 | 21450 |
| `12m` | 24,890,000 | 24,990,000 | 24890 | 24990 |
| `10m` | 28,000,000 | 29,700,000 | 28000 | 29700 |
| `6m` | 50,000,000 | 54,000,000 | 50000 | 54000 |

Frequencies outside the allocations resolve to a general-coverage / out-of-range
label, not a synthesized one:

- **HF general coverage** — a frequency in the 1.8–30 MHz HF range that is not inside
  any ham allocation (e.g. 9.9 MHz shortwave broadcast) → `gen`.
- **Out of range** — anything outside the allocations and outside the HF
  general-coverage range (VHF/UHF gaps, non-HF, zero/negative) → `unknown`.

`gen` is a recognized band label (accepted by band-validation helpers, case-insensitive).

---

## VHF/UHF band frequency ranges

| Band | Low (Hz) | High (Hz) |
|------|----------|-----------|
| `2m` | 144,000,000 | 146,000,000 |
| `70cm` | 430,000,000 | 440,000,000 |
| `23cm` | 1,240,000,000 | 1,300,000,000 |

23 cm is currently out of scope for automation.

---

## HF band centre frequencies (IARU R1)

Used by the antenna controller (ultrabridge) when jumping to a band by name.

| Band | Centre (Hz) | Centre (kHz) |
|------|------------|--------------|
| `20m` | 14,175,000 | 14175 |
| `17m` | 18,118,000 | 18118 |
| `15m` | 21,225,000 | 21225 |
| `12m` | 24,940,000 | 24940 |
| `10m` | 28,850,000 | 28850 |
| `6m` | 51,000,000 | 51000 |

---

## Canonical mode names

| Canonical name | Description | Firmware variants (normalise → canonical) |
|----------------|-------------|------------------------------------------|
| `cw` | CW | `CW`, `CW-U`, `CW-L`, `CW_U`, `CW_L` |
| `usb` | Upper sideband (voice) | `USB` |
| `lsb` | Lower sideband (voice) | `LSB` |
| `am` | AM | `AM`, `SAM` |
| `fm` | FM | `FM`, `DFM` |
| `data` | Digital data | `DIGU`, `DIGL`, `USB-D`, `LSB-D`, `FDV`, `RTTY`, `RTTY-U`, `RTTY-L`, `FT8`, `FT4`, `PSK31`, `WSPR`, `JS8` |

Digital-over-sideband firmware modes (`DIGU`, `DIGL`, `USB-D`, `LSB-D`) and digital
voice (`FDV`/FreeDV) normalize to `data` regardless of which sideband carries them:
canonical `mode` describes the content, not the RF carrier. This matters downstream —
e.g. a PA consumer derates for the continuous duty cycle of digital modes. (FT8/FT4/
PSK31 etc. are transmitted *in* DIGU/DIGL on most SDRs, so mapping DIGU to `usb` would
make digital operation indistinguishable from voice.)

**Normalization is the adapter's responsibility.** Consumers see only the canonical
names above. Adapters must publish a canonical mode or omit the field; they must never
publish a raw firmware mode string.

`data` is the generic catch-all for all digital modes. If a component needs to
distinguish between specific digital modes (e.g. for band plans or sequencing),
introduce a sub-field — do not extend the canonical mode list without updating all
adapters.

---

## Frequency unit convention

`freq_hz` is always an integer in **Hz**. This is the single source of truth on the
MQTT bus. Never publish kHz, MHz, or floating-point frequency values.

Components that use kHz internally (e.g. the RCU-06 controller in ultrabridge) multiply
by 1000 before publishing. Components that use Hz natively (e.g. SmartSDR in
flexbridge) publish directly.

`band` is always a derived label from the canonical table above, never a primary value
on `/state`. The canonical tuning intent is `set_freq_hz` — commanders set a frequency
and band falls out — which removes the class of bug where band and frequency disagree.
A radio *may* additionally accept a `set_band` `/cmd` input for native band-stacking: the
radio restores its own persisted per-band frequency, the bridge republishes that
`freq_hz`, and `band` is still derived from it (no band setpoint is stored). See the
radio slot in `station-integration-model.md`.
