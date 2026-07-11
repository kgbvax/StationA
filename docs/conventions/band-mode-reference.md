# Canonical band and mode reference

This is the single authoritative table for HF/VHF band frequency ranges and canonical
mode names used across all stationa components. Components implement against this
reference; when it changes, each affected component needs a corresponding code update.

Edges are DL / IARU Region 1 allocations.

---

## HF band frequency ranges

| Band | Low (Hz) | High (Hz) | Low (kHz) | High (kHz) |
|------|----------|-----------|-----------|------------|
| `160m` | 1,800,000 | 1,999,999 | 1800 | 1999 |
| `80m` | 3,500,000 | 3,999,999 | 3500 | 3999 |
| `60m` | 5,351,500 | 5,366,500 | 5351.5 | 5366.5 |
| `40m` | 7,000,000 | 7,299,999 | 7000 | 7299 |
| `30m` | 10,100,000 | 10,149,999 | 10100 | 10149 |
| `20m` | 14,000,000 | 14,349,999 | 14000 | 14349 |
| `17m` | 18,068,000 | 18,167,999 | 18068 | 18167 |
| `15m` | 21,000,000 | 21,449,999 | 21000 | 21449 |
| `12m` | 24,890,000 | 24,989,999 | 24890 | 24989 |
| `10m` | 28,000,000 | 29,699,999 | 28000 | 29699 |
| `6m` | 50,000,000 | 53,999,999 | 50000 | 53999 |

Outside these ranges, the band label is `band-N` where N is derived from the frequency
(implementation-specific fallback; do not rely on it downstream).

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

`band` is always a derived label from the canonical table above, never a primary value.
There is no `set_band` intent for radios — commanders set `freq_hz` and band falls out.
