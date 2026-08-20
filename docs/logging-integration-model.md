# Logging & Operating Layer — Integration Model Extension

**Status: draft companion to `station-integration-model.md` (v0.8).** This document
proposes the additions that would become v0.9 but is kept as a **separate file** so the
core model stays clean while this layer is reviewed. Section references (§2, §7, §8,
Appendix A/C) point into `station-integration-model.md`. Nothing here changes an existing
document; the `submode` sub-field and the `<slot>/event` sub-topic are described here in
full so they can be folded into `band-mode-reference.md`, the schema template, and the
core model later if adopted.

The guiding idea is unchanged: *the live configuration is the documentation.* A logging
application, once bridged onto the bus, describes itself and publishes what it is doing —
QSOs, running score, spots — to a well-known address, addressed by name and role, never by
IP:port. Anyone who wants the log subscribes and reads.

---

## 1. Why — the problem this replaces

QSO logging, contest scoring, and DX spotting move today over **proprietary 1:1 UDP**
(N1MM+/DXLog XML, WSJT-X/JTDX binary QDataStream, Log4OM/fldigi). That transport is:

- **Best-effort / lossy.** A dropped datagram is a lost QSO or spot — no retry, no
  queueing, no late-join. In a contest that is lost points.
- **Operationally fragile.** Every app pair is wired by hand-entered IP:port. In a
  multi-op station operators re-plug machines and edit ports; the inventory drifts and
  breaks. Delivery is 1:1 unicast, so N consumers of one source need N wirings (O(N²)).
  The community already patches this with multicast + rebroadcast hacks (GridTracker,
  JTAlert port-forward, WSJT DX-Aggregator) — evidence of the pain, not a fix.

The station bus already solves these for RF and power: retained state for late-join, QoS-1
at-least-once, pub/sub fan-out, and self-describing `/meta` addressed by name. This
extension puts the log under the same discipline and defines a **UDP-adapter migration
path** so existing apps move onto the bus **without being modified**.

**Prior art on replacing the UDP.** The closest is **TCI** (Expert Electronics) — a
full-duplex WebSocket bus replacing CAT/UDP, radio-as-server, adopted by Log4OM, SunSDR and
others; it is radio-centric and stream-oriented, not a neutral station bus. **Multicast +
rebroadcast routers** are the community workaround for UDP being 1:1. **Hamlib `rigctld`**
(TCP) and **Wavelog/Cloudlog** (HTTP) cover control and logbook push. No MQTT-based
replacement of contest-logger UDP was found — this is largely new ground, but it is the
exact shape the station bus already handles.

---

## 2. Scope

- **Transport: MQTT for discrete data.** QSO / score / spot / state ride the existing
  Mosquitto bus. High-rate streams (IQ, audio, spectrum/waterfall, S-meter) stay
  out-of-band and out of scope, consistent with the VITA-49 deferral (core §12).
- **Observe-only.** Logging/score/spots and radio state flow *onto* the bus and *to*
  loggers. The logger→radio/rotator **write** path (QSY command, rotator bearing) is
  deferred; its `/cmd` topics already exist, so it is a clean later add (§11).
- **First adapters:** WSJT-X/JTDX ingest and Wavelog/Log4OM outbound. The neutral schema is
  kept general enough for contest loggers (N1MM/DXLog), which come next.

---

## 3. Addressing — the log follows the operator seat

The log is **not** shared site-level infrastructure like `host`/`power`. Several logs run
at once — one per operator PC — so the log sits at the model's **`position`** level
(`site/station/position/<slot>`, core §2), collapsing to the **station** when a station has
a single position:

- **Explicit position** (multi-op, or several single-ops sharing a site):
  `muehle/<station>/<position>/log` — e.g. `muehle/hf/op-a/log`.
- **Collapsed** (single position per station — current status quo, and how the rest of the
  model already collapses `position`): `muehle/<station>/log` — e.g. `muehle/hf/log`.

Multi/Multi maps to per-station logs (`muehle/hf/log`, `muehle/uhf/log`); multiple
single-ops map to per-position logs. Both reduce to **one log slot per operator seat**.
This is the sanctioned "`position` reappears when operators share a station" case from §2.

**The `host` of a log slot is that position's PC** (e.g. `shack-pc`), not `shari` — because
the ingest adapter runs next to the app it fronts (§8). That per-PC placement is *why* the
slot is position-scoped.

Slots per operator seat:

| Slot | Role | Plane shape |
|---|---|---|
| `…/log` | `qso-log` | QSO event source + retained rollup (incl. running score) |
| `…/spots` | `bandmap` | DX-spot event source (lower priority) |

Consumers of all logs use a wildcard (`muehle/+/log/event` collapsed, or
`muehle/+/+/log/event` with explicit positions). Recommend per-deployment consistency in
address depth so one wildcard suffices.

---

## 4. Planes — event vs state (the crux)

A QSO is a discrete **event**, not steady state. The core model has one precedent for
transient data: the deferred non-retained `<slot>/meter/<group>/<name>` sub-topic (§12).
QSO and spot events extend that precedent — a **non-retained event sub-topic** alongside
the four planes, never mixed into `/state`:

```
…/log/meta      retained       birth certificate (role qso-log, capabilities, expose)
…/log/status    retained       online | offline — Last Will
…/log/state     retained       rollup snapshot: qso_count, last_qso, session, score
…/log/event     NOT retained   one JSON message per new/edited/deleted QSO  (QoS 1)
```

- **`…/log/event`** — the live contact feed. Not retained (a retained "last QSO" would
  re-fire on every reconnect and misrepresent history), QoS 1 for at-least-once.
- **`…/log/state`** — the retained rollup a late joiner reads immediately: current QSO
  count, last QSO summary, running score. State discipline preserved — consumers couple to
  this, not to the fact an event was emitted.

**Reliability — the "why replace UDP" payoff.** The **logbook is the system of record**
(Wavelog / Log4OM / local ADIF), not the bus. The bus carries the event so consumers react.
Over UDP that is fire-and-forget to whoever happens to be listening. On the bus it is QoS-1
at-least-once with **broker queueing for offline persistent-session subscribers** plus a
retained rollup for late-join — strictly better than UDP, without pretending the bus is the
archive.

**Idempotency.** Every event carries a stable **`id`**. QoS-1 is at-least-once, so
consumers dedupe on `id`; `action: replace|delete` reference the original `id` (mirrors
N1MM's per-QSO GUID with ContactReplace / ContactDelete).

---

## 5. Canonical vocabulary additions

Proposed additions to core §4:

- **Roles:** `qso-log` (a running log application's QSO event + rollup source),
  `bandmap` (a DX-spot event source). Future: `scoreboard` (a derived aggregator that
  merges multiple positions' score — §11).
- **Capability keys:** `actions` (the QSO actions a source emits: `add`, `replace`,
  `delete`), `source` (the fronted app: `wsjtx`, `n1mm`, `log4om`, …), `submodes` (the
  digital submodes this source can report), `contest` (bool / contest id if contest-aware).

### 5.1 The `submode` sub-field (proposed addition to `band-mode-reference.md`)

Canonical `mode` stays exactly the six values (`cw`, `usb`, `lsb`, `am`, `fm`, `data`) —
all digital modes collapse to `data`. `band-mode-reference.md` already says: *"if a
component needs to distinguish specific digital modes, introduce a sub-field — do not
extend the canonical mode list."* This formalizes that sub-field for the logging layer:

- **`submode`** refines `data` with the **ADIF `SUBMODE` token, uppercase**: `FT8`, `FT4`,
  `RTTY`, `PSK31`, `JS8`, `WSPR`, `MFSK`, `OLIVIA`, … Present only when `mode == data`;
  omitted otherwise. Using ADIF tokens keeps the field interoperable with every logbook.
- `mode` remains the single canonical carrier-content value; `submode` is additive and
  optional. Consumers that do not care ignore it; contest scoring that needs RTTY-vs-FT8
  reads it.

---

## 6. Neutral schemas (ADIF's common core, normalized to station conventions)

ADIF is the logging lingua franca. The neutral record is ADIF's common fields normalized to
station rules — `freq_hz` integer Hz, canonical `mode` + `submode`, RFC 3339 UTC `ts` —
**plus a raw `adif` passthrough** so outbound logbook adapters keep full fidelity for fields
the neutral core does not model.

### 6.1 QSO event — `…/log/event` (not retained, QoS 1)

| field | type | notes |
|---|---|---|
| `id` | string | stable unique id (source GUID or synthesized); dedupe/replace/delete key |
| `action` | enum | `add` \| `replace` \| `delete` |
| `ts` | string | RFC 3339 UTC — the QSO time |
| `call` | string | worked station callsign |
| `freq_hz` | int | Hz (§4 core) |
| `band` | string | derived label (canonical table) |
| `mode` | enum | canonical: `cw`/`usb`/`lsb`/`am`/`fm`/`data` |
| `submode` | string | optional; ADIF `SUBMODE` uppercase, when `mode==data` (§5.1) |
| `rst_sent` / `rst_rcvd` | string | signal reports |
| `exchange_sent` / `exchange_rcvd` | string | contest exchange (free-form) |
| `gridsquare` | string | optional |
| `my_call` / `operator` | string | optional |
| `contest_id` | string | optional (e.g. `CQ-WW-DX-CW`) |
| `source` | string | producing app (`wsjtx`, `n1mm`, …) |
| `adif` | string | optional raw ADIF record — lossless passthrough |

```json
{
  "id": "3f2a9c…",
  "action": "add",
  "ts": "2026-07-16T14:03:12Z",
  "call": "DL1ABC",
  "freq_hz": 14074000,
  "band": "20m",
  "mode": "data",
  "submode": "FT8",
  "rst_sent": "-07",
  "rst_rcvd": "-12",
  "exchange_sent": "599 001",
  "exchange_rcvd": "599 014",
  "gridsquare": "JO31",
  "my_call": "DK0MU",
  "operator": "DL9XYZ",
  "contest_id": "CQ-WW-DX-CW",
  "source": "wsjtx",
  "adif": "<call:6>DL1ABC<band:3>20m<mode:3>FT8<qso_date:8>20260716<time_on:6>140312<eor>"
}
```

### 6.2 Log rollup + running score — `…/log/state` (retained)

```json
{
  "ts": "2026-07-16T14:03:12Z",
  "session_id": "2026-07-16",
  "qso_count": 142,
  "last_qso": { "call": "DL1ABC", "band": "20m", "mode": "data",
                "submode": "FT8", "freq_hz": 14074000, "ts": "2026-07-16T14:03:12Z" },
  "score": {
    "contest_id": "CQ-WW-DX-CW",
    "class": { "power": "high", "assisted": true, "mode": "cw", "bands": "all", "overlay": null },
    "qso_count": 142, "mult_count": 63, "points": 388, "score": 24444,
    "breakdown": [ { "band": "20m", "mode": "cw", "qsos": 80, "points": 220, "mults": 35 } ]
  }
}
```

`score` is omitted outside a contest. In networked Multi/Multi every position reports the
same aggregate score; a consumer reads any one, or a future `scoreboard` aggregator (§11)
merges them.

### 6.3 Spot event — `…/spots/event` (not retained, QoS 1)

```json
{
  "action": "add",
  "ts": "2026-07-16T14:05:00Z",
  "dxcall": "VK9XY",
  "freq_hz": 14023000,
  "band": "20m",
  "mode": "cw",
  "spotter": "RBN/DK0XX",
  "comment": "CQ",
  "source": "cluster",
  "status": "new"
}
```

Consumers build their own bandmap from the event stream. `source` ∈ `rbn|cluster|local`;
`status` (`new|dupe|mult|…`) and `mode` are optional.

---

## 7. Activity → bus mapping

| Activity | Bus mapping | Direction this pass |
|---|---|---|
| Log / update / delete contact | `…/log/event` + `…/log/state` rollup | ingest → bus → outbound logbook |
| QSY / radio state | **existing** `muehle/hf/radio/state` (freq_hz/band/mode/tx) | bus → logger (read) |
| Command radio (QSY) | **existing** `muehle/hf/radio/cmd` | deferred (observe-only, §11) |
| Control rotator | **existing** `muehle/hf/rotator/cmd`, `muehle/uhf/rotator/cmd` | deferred (observe-only, §11) |
| Publish score | `…/log/state.score` (retained) | ingest → bus → scoreboard |
| Spots / bandmap | `…/spots/event` (not retained) | cluster ↔ bus ↔ loggers |
| Callsign lookup (pre-QSO) | request/reply — awkward on MQTT | deferred (§11) |

QSY and rotator need **no new topics** — they reuse existing station slots. This pass only
*reads* `radio/state`; the write path is deferred.

---

## 8. Logger adapters — the migration mechanism

Adapters are edge translators, exactly analogous to device bridges (core §3, "adapters live
at the edge"), except the "device" is a software app on a socket and the adapter **runs on
the operator's PC next to the app it fronts**:

- **Ingest adapters** own the position's `qso-log` slot and translate an app's native
  protocol → the neutral event/state. First: **`wsjtx-log-bridge`** — runs on the position
  PC, listens on the WSJT-X UDP server port on localhost (binary QDataStream), decodes
  `QSO Logged` / `Logged ADIF` → `…/log/event` + `…/log/state`. `n1mm-log-bridge` (XML)
  follows the same shape.
- **Outbound consumers** are optional modules (core §9 pattern, like `hadiscovery`) and may
  run anywhere (e.g. `shari`): subscribe the log-event wildcard, push to a master logbook
  over HTTP. First: **`wavelog-consumer`** (Wavelog / Cloudlog API) and **`log4om-consumer`**
  (Log4OM UDP-inbound ADIF / HTTP).

**Migration.** Each app keeps speaking its native protocol to the *local* adapter on its PC
(fixed localhost port); the adapter puts it on the bus; other apps' consumers pull from the
bus. This turns the O(N²) point-to-point UDP mesh into O(N) hub-and-spoke, kills the
IP/port inventory (the bus address is fixed and self-describing), and adds
reliability / late-join / fan-out — **with no change to the apps**.

**Host/adapter placement** (proposed replacement for the core §7.3 `Logging | subscriber,
no slot | shari` row):

| Adapter / service | Fronts | Host |
|---|---|---|
| WSJT-X log bridge (`wsjtx-log-bridge`) | `muehle/<station>/log` (`qso-log`) | operator PC (e.g. `shack-pc`) |
| Wavelog consumer (`wavelog-consumer`) | consumer of `…/log/event`, no slot | `shari` |
| Log4OM consumer (`log4om-consumer`) | consumer of `…/log/event`, no slot | `shari` |

---

## 9. Transport rationale

Keep MQTT for discrete log data: retained state (late-join), QoS-1 at-least-once with
persistent-session queueing (reliability over UDP), pub/sub fan-out (no per-pair config),
self-describing `/meta` (no IP:port inventory), and one broker already running.

Explicitly **not** on the bus: high-rate IQ / audio / spectrum / waterfall / S-meter — same
reasoning as the VITA-49 deferral (core §12); that lane, if ever built, is a dedicated
stream transport (TCI / WebSocket / RTP), out of scope here.

Alternatives considered and declined: **NATS/JetStream** (better replay and request/reply,
but a second broker technology fragments the stack; persistent sessions + logbook-as-record
cover the durability need); **WebSocket/TCI** (radio-centric, not a neutral station bus);
**AMQP/RabbitMQ** (heavier than the need).

---

## 10. Conformance notes

New slots satisfy the core §8.1 adapter-conformance checklist unchanged, with two layer
specifics:

- The `…/log/event` sub-topic is the **non-retained event exception** (justified exactly as
  the deferred meter sub-topic, core §12) — it is *not* a fifth plane. The four planes
  (`/meta`, `/state`, `/status`, `/cmd`) keep their meaning; a read-only `qso-log` slot
  omits `/cmd` this pass (observe-only).
- Vocabulary rules hold: `freq_hz` integer Hz, `band` derived, `mode` canonical (never a raw
  firmware/ADIF mode string), `submode` refines `data` only (§5.1). `/state` is one full
  retained snapshot with an RFC 3339 `ts`; `/event` messages carry their own QSO `ts`.

An `expose` block (core §3.1, Appendix C) MAY be published so historians/dashboards can
render the log (e.g. `qso_count` as a `total_increasing` number, `score` numbers). It
carries no consumer vocabulary.

---

## 11. Deferred

- **Logger→radio/rotator write path** — map WSJT-X / N1MM QSY and rotor control onto the
  existing `radio/cmd` and `rotator/cmd` topics.
- **`scoreboard` aggregator** — a site/station-level derived slot merging multiple
  positions' `log/state.score` into one total (the one genuinely shared-level concern).
- **High-rate stream lane** (IQ / audio / spectrum) — dedicated transport, TCI as the domain
  reference; not MQTT.
- **Callsign lookup** request/reply (MQTT-5 response-topic pattern, or leave to the app).

---

## Appendix — worked example: the `log` slot on the wire

Concrete payloads for a collapsed single-position station, `muehle/hf/log`. `meta`,
`state`, `status` retained; `event` not retained. QoS 1 throughout.

`muehle/hf/log/meta` — retained
```json
{
  "schema": "1.0",
  "role": "qso-log",
  "location": "bauwagen",
  "host": "shack-pc",
  "capabilities": {
    "source": "wsjtx",
    "actions": ["add", "replace", "delete"],
    "contest": true,
    "submodes": ["FT8", "FT4", "JS8", "RTTY", "PSK31", "WSPR"]
  }
}
```

`muehle/hf/log/state` — retained (see §6.2 for the full shape)
```json
{
  "ts": "2026-07-16T14:03:12Z",
  "session_id": "2026-07-16",
  "qso_count": 142,
  "last_qso": { "call": "DL1ABC", "band": "20m", "mode": "data",
                "submode": "FT8", "freq_hz": 14074000, "ts": "2026-07-16T14:03:12Z" }
}
```

`muehle/hf/log/status` — retained, and the Last Will
```
online
```

`muehle/hf/log/event` — NOT retained (see §6.1 for the full field set)
```json
{ "id": "3f2a9c…", "action": "add", "ts": "2026-07-16T14:03:12Z",
  "call": "DL1ABC", "freq_hz": 14074000, "band": "20m",
  "mode": "data", "submode": "FT8", "source": "wsjtx" }
```

Encoding decisions worth copying: `id` is stable per QSO (dedupe + replace/delete key);
`freq_hz` is integer Hz; `mode` is canonical with `submode` refining `data`; the QSO time
is the event's own `ts`; `/state` is a full retained snapshot; `/event` is never retained.
