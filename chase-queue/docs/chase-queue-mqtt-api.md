# MQTT schema — chase-queue

chase-queue implements the `hf/chase` logic slot (role `chase-queue`) of the station integration model
(`../../docs/station-integration-model.md`). It reads bmrec's `hf/cqfinder` slot. Derived from
`internal/bus/bus.go` and `internal/app/mqtt.go`.

## 1. Connection

| Property | Value |
|---|---|
| Protocol | MQTT 3.1.1, `tcp://127.0.0.1:1883` (the station broker on scmino) |
| Authentication | `[mqtt] user` + `CHASE_QUEUE_MQTT_PASSWORD` when the broker requires it |
| Clean session | **Yes**. The candidates are retained and replay on subscribe; `/cmd` is subscribed at QoS 0 |
| Last will | `<slot>/status` = `offline`, retained |
| Client ID | `muehle-hf-chase` (`mqtt.client_id`) |

## 2. Topics

`<site>/<station>/<slot>` = `muehle/hf/chase`. Input: `muehle/hf/cqfinder/state` (bmrec).

| Topic | Direction | Retained | Payload |
|---|---|---|---|
| `muehle/hf/chase/status` | out | yes | `online` \| `offline` |
| `muehle/hf/chase/meta` | out | yes | `{schema, role:"chase-queue", host, device, capabilities}` |
| `muehle/hf/chase/state` | out | yes | the ranking (§3) |
| `muehle/hf/chase/cmd` | in | no | `{"action", "value"}` (§4) |
| `muehle/hf/cqfinder/state` | in | yes (bmrec) | the candidates (§5) |

## 3. `/state`

```json
{"ts":"…","seq":42,"activity":"dx","ranked":[{"row_id":7,"call":"EA8XYZ","band":"20m","needed":true,"tag":"need"}],
 "undo":"EA8XYZ","problems":["Club Log confirmations not loaded yet"]}
```

In a contest, `activity` is `contest`, `tag` is `mult`, and a `contest` block is added:

```json
{"activity":"contest","contest":{"id":1,"name":"UKEIDXSSB","start_date":"2026-11-01","qsos":212},"ranked":[…]}
```

- `seq` echoes the candidates' `seq`. `ranked` is the list, in work order; a row bmrec has that is absent
  here is not listed (spotted, skipped, handled). A station worked on the row's band **is** listed (below).
- Order: needed (new multiplier / entity) -> unknown -> unworked, not needed -> **worked on this band**
  (`worked_before.this_band`; in DX mode worked in the station log or a stored contest log, in a contest a dupe under the contest's
  rule; `needed:false`, no `tag`) -> candidates without a call.
- Every row is self-contained: `source` is always set (`bmrec` for a row bmrec hears, `cluster` below),
  `freq_hz` is the frequency in Hz (the bmrec candidate's rounded, or the latest cluster spot's; absent when
  unknown) and `mode` is one of `cw`, `usb`, `lsb`, `am`, `fm`, `data`, absent when unclear. chase-queue
  derives the mode: a bmrec row (bmrec hears only SSB) by the sideband rule (LSB below 10 MHz except 60 m,
  else USB); a cluster row from its comment (`CW`; `FT8`, `FT4`, `RTTY`, `PSK*`, `JT65`, `JS8`, `DIGI`, ... ->
  `data`; `SSB` -> sideband rule; `USB`/`LSB`/`AM`/`FM` literal), else from the IARU Region 1 band-plan
  segment at the frequency (CW segments -> `cw`; narrow digital segments and 3 kHz around the standard
  FT8/FT4 dial frequencies -> `data`; phone segments -> sideband rule). A consumer needs no `cqfinder` topic.
- `activity` is `dx` or `contest`; `contest` is present in a contest. `tag` is `mult` when the contest
  award needs the station, `need` when another award does, absent on rows that are not needed. `undo` names what Undo would reverse (a callsign or `skip`).
- **`needs`** (omitted when empty) says for which award a needed row is needed: one entry per slot of an
  enabled goal that the station would fill and that is not yet satisfied, goal priority first:
  `{"award","key","band"?,"mode"?,"status","atno"?}`. `award` is the definition's code (`dxcc`, `WAS-20-CW`, `contest`);
  `key` the slot's name (DXCC: the entity's primary prefix; WAS: the state; contest: the multiplier);
  `band` is absent when the award counts over all bands; `mode` (`cw`, `phone`, `data`) is present only when the
  award slices or filters by mode class; `status` is `unworked` or `worked-unconfirmed` (the slot as the log
  holds it; a confirmed slot is not a need); `atno: true` marks an all-time new one: the key (for DXCC the
  entity) is worked in no slice at all, not even unconfirmed, so it is more than a new band slot. `needed` is true exactly when `needs` is not empty; `tag` and
  `needed` stay as the derived summary. A slot that cannot be told (the station's state or entity is not known,
  no log loaded) is never listed and never counts as "not needed": it keeps the station in the unknown tier.
  A row on the worked-on-this-band tier has no `needs`. 40 rows with 3 needs each are about 13 KB of `/state`.
- **Ranking within the needed tier**: the goal priority of the first award that needs the station (higher
  first), then the number of awards that need it, then the new slots (an entity confirmed on no band, or a
  contest station's new multiplier slots), the Club Log Most Wanted rank, and the first-seen time. With
  one goal this is the old order exactly.
- **Buddies** (`[[buddy]]`: `call`, `note`, `boost`): a row whose call is a buddy (the same call or a portable form of
  it, matched like bmrec's spotted check) carries `buddy:true` and, with a note, `buddy_note`. A buddy is listed even
  when no goal needs it (cluster rows too): a **not-needed buddy sits in its own tier just below the needed block**
  (before unknown and unworked; a buddy already worked on the band stays in the worked tier), higher `boost` first.
  Inside the needed tier a buddy ranks **before** the other stations of the same goal priority (and a higher `boost`
  first among buddies); a higher goal priority still wins. Absent on other rows.
- `bmrec_unqualified` (integer, absent when 0): bmrec rows the `[bmrec] gate` kept off the list (section 5). Not
  a problem by itself.
- `problems` are what keeps the ranking from being right, worst first; absent when none. Cluster entries read
  `DX cluster <name> not connected[: error]` (one per `[[cluster]]`, after a minute down); with the `qualified`
  gate and no `qualified` field on bmrec's rows: `bmrec sends no qualified flag: ...`.
- `/meta.capabilities.history` is `"station-log"` when the log view comes from station-log, else `"local"`
  (additive; no wire field of the rows changed: `row_id`, `call`, `band`, `needed`, `tag` mean what they did).
- Rows from the DX clusters (`[[cluster]]`, chase-queue's own telnet logins) carry `source:"cluster"` (the
  value stays `cluster` whatever the cluster), `cluster_name` (the `name` of the cluster whose spot the row
  stands for), a **negative** `row_id` (no bmrec row; stable while the spot lasts) and
  `spot:{freq_hz, spotter, comment, at, rbn?}` of the latest spot; `rbn:true` marks a skimmer spot (spotter
  `...-#`, only from a cluster with `skimmer = true`). The same call on the same band spotted on several clusters
  is one row: the freshest spot wins (its `cluster_name`, `rbn`, frequency). Only **needed** cluster stations
  and **buddies** are listed (at most 40 needed, buddies do not count), ranked by the same rules as the heard
  rows and mixed into the same order. A heard station on
  chase-queue's own cluster login leaves the list and its cluster row stands for it. A spot that only bmrec
  relays (`spotted`, e.g. its own post, which chase-queue's login drops as the operator's) hides a heard station
  that is not needed, never a needed one. Skimmer spots (unless `skimmer = true`)
  and the operator's own spots are dropped; a spot lasts 30 minutes. `skip`/`handled`/`undo` work on cluster
  rows by their `row_id`. A consumer that joins rows to bmrec's board by `row_id` must treat `source:"cluster"`
  rows as having no board row.
- Each row with a call may carry two enrichments (both optional; consumers must not depend on them):
  - `info` — the QRZ.com callbook entry: `{source:"qrz", call, name, qth, state, country, grid, lat, lon,
    image, cq_zone, itu_zone}`. `call` is the entry's call (the home call of a portable form); `image` is a
    picture URL. Absent until looked up (the row republishes when the lookup lands), when QRZ does not have
    the call, or with no `[qrz]` login. Grid, lat/lon and image need a QRZ XML subscription. Street address,
    e-mail and birth date are never published.
  - `worked_before` — `{qsos, bands, modes, this_band, last}`. With `[station_log]` configured and loaded it
    comes from station-log's log view (`qsos` = call+band+mode_class entries of the log; `modes` as logged);
    otherwise from the local station log (Log4OM, Club Log) and the
    stored DXLog contest logs, any mode, ever (contest logs are pruned with their instance after 30 days).
    A QSO two sources report counts once (same band and minute). `bands` is low to high; `this_band` is true
    exactly when the row sits in the worked-on-this-band tier above (the engine's verdict). In DX mode that is the same history as `bands`;
    in a contest it is the contest's dupe rule, so a band in `bands` can show `this_band` false there. Portable
    and SSID forms (EA8XYZ/P) are looked up under their normalized call too. Absent when the call was never worked. Without `[station_log]` only QSOs since chase-queue
    started are known.

```json
{"row_id":7,"call":"EA8XYZ","band":"20m","needed":true,"tag":"need","source":"bmrec","freq_hz":14205000,"mode":"usb",
 "info":{"source":"qrz","call":"EA8XYZ","name":"Juan Perez","qth":"Las Palmas","country":"Canary Islands",
         "grid":"IL28gc","lat":28.1,"lon":-15.4,"image":"https://cdn-xml.qrz.com/…","cq_zone":33,"itu_zone":36},
 "worked_before":{"qsos":2,"bands":["40m","15m"],"modes":["CW","FT8"],"this_band":false,"last":"2025-05-01T10:00:00Z"}}
```
- Republished on change and every 30 s.

### Awards and goals

An **award** is a definition: a key, a slice and a confirmation rule. The service reads `*.toml` files from
`[awards] dir` (default `<data_dir>/awards`) once at start; a bad file, a goal naming an unknown award or a
`kind = "reference"` award stops the start with the reason. DXCC per band is built in (a file may replace it
once). A file holds `[[award]]` tables:

| key | meaning |
|---|---|
| `code` | unique name goals use (letters, digits, `_ . + -`; case-insensitive; `contest` is reserved) |
| `name` | display name |
| `kind` | `dxcc` (key: the entity) or `field` (key: the station's `STATE`; from the log, else QRZ.com). `reference` (SOTA, WWFF, POTA) is deferred and refused |
| `rule` | `confirmed` (default; a slot is open until a QSL, LoTW, eQSL or Club Log confirmation) or `worked` |
| `field` | `STATE` (kind field) |
| `dxcc` | ADIF numbers the field award covers (WAS: 6, 110, 291); empty = every entity |
| `items` | allowed keys (kind field); empty = any |
| `bands` | restrict to these bands (`20m`); empty = every band |
| `modes` | restrict to mode classes `cw`, `phone`, `data`; empty = any |
| `per_band` | default true: a slot per band; false counts a key once over all bands |
| `per_mode` | a slot per mode class |
| `[[award.config]]` | named variants (`name`, `bands`, `modes`); each becomes the award `<code>-<name>` |

A slot is satisfied under the rule by the station-log view (worked rows by entity, state, band, mode class) and,
for a DXCC award without a mode filter, Club Log's matrix. A station whose band or mode class lies outside the
award, or whose entity or state the award does not cover, is not part of it.

**Goals** (`[[goal]]`: `award`, `priority`, `enabled`) pick the awards that make a station needed. DXCC is a goal
unless a goal turns it off; the running contest is the award `contest` (its multiplier slots; priority 100 by
default, above DXCC 10) while an instance runs; any other award is a goal only when a `[[goal]]` enables it
(priority 50 by default). `activity`, `contest`, `tag`, `undo` and the `/cmd` actions are unchanged; `activity`
is derived (`contest` while an instance runs). `/meta.capabilities.awards` lists the loaded award codes.

`chase-queue -import-award-xml FILE` prints a definition file for a Log4OM award export (a `QSOFIELDS` award
with its small item list and `AwardConfigurations`; bands and modes carry over, continent and group filters
are dropped with a `# note`). A `REFERENCE` award exits with status 3 and a "deferred" message, without reading
its item list.

## 4. `/cmd` (`shared/schema.CmdPayload`; `value` is a string)

| action | value | effect |
|---|---|---|
| `skip` | `{"row_id","band","call"}` | hide the station: contest until its call is confirmed, DX until 00:00 UTC |
| `handled` | `{"row_id","band","call","how"}` | contest QSY/spot mark (`how` defaults to `qsy`); no-op in DX |
| `undo` | `""` | reverse the last skip/handled of this activity |
| `set_mode` | any (not read; send `"dx"`) | leave contest mode |
| `switch_instance` | `{"instance"}` or `{"new":true,"definition_id","prefill_id","start_date"}` | contest mode on |
| `delete_instance` | instance id | delete a stored instance (not the running one) |

An action on a row no longer listed is logged and ignored.

## 5. Candidates (bmrec → `cqfinder/state`)

```json
{"seq":42,"ts":"…","rows":[{"row_id":7,"call":"EA8XYZ","call_source":"confirmed","call_trusted":true,
 "band":"20m","freq_hz":14231500,"first_heard":"…","last_heard":"…"}],
 "spotted":[{"call":"DL1ABC","band":"20m","at":"…","source":"cluster"}]}
```

`spotted` carries the human cluster spots and bmrec's own posts of the last 30 minutes.

**Qualification gate (`[bmrec] gate`).** `off`: every open row is a station (the former behaviour, also call-less
candidates). `trusted` (default): only rows with a non-empty `call` and `call_trusted:true` (or `call_source`
`confirmed`/`owner`). `qualified`: trusted rows that also carry `"qualified":true`. Rows kept out are not listed
and are counted in `/state.bmrec_unqualified`. Default `trusted` trades completeness for precision: a freshly
heard CQ whose call is only `inferred` (transcript voting) or `spotted`, and every call-less candidate, stays off
the list until the answering station reads the call back or the operator types it in bmrec; `gate = "off"`
brings the old list back. The `trusted` gate only vouches for the callsign, not that the row is a CQ.

**Spec for bmrec: the `qualified` field** (optional, additive; chase-queue ignores it unless `gate = "qualified"`):
each row of `cqfinder/state.rows` gets `"qualified": true|false`. `true` when the operator qualified the row on
the HF CQ page (the board's `ActConfirm`: `OwnerLabel = "cq"`, `DecidedBy = "owner"`) **or** the row's detection is
confident (bmrec's own threshold, e.g. `State = confirmed` or an LLM/phrase decision above its cut); `false` for
everything else that passes bmrec's `Eligible` (rhythm-only `probable` rows). A row the operator marked `not_cq`
stays unpublished as today. **Send the field on every row, `false` included** (no `omitempty`): chase-queue reads an
absent field as "this bmrec does not send it" and, with `gate = "qualified"`, lists nothing and reports the
problem `bmrec sends no qualified flag` (an older bmrec never passes by accident). A state with no rows says
nothing either way.

## 6. Local HTTP (loopback, `[http] listen`)

`GET /healthz`; `GET /contest` (instances, definitions, prefills, status); `POST /contest/files`
`{"kind":"definition"|"prefill","name","text"}` (8 MiB). bmrec's contest setup proxies these.

## 7. Other inputs

DX-cluster telnet nodes, one `[[cluster]]` each (`name`, `address`, `call`, `skimmer`, `enabled`; each has its own
login and reconnects with backoff, a node silent for 10 minutes is reconnected; `DX cluster <name> not
connected` in `problems` after a minute down; the former single `[cluster]` table still loads as one entry
named `cluster`). Bands: 160-10 m (no 30 m), 6 m, 2 m and 70 cm (the Club Log matrix keys `6`, `2`, `70`); the
mode of a VHF/UHF row is absent unless the comment names one or the frequency is an FT8/FT4 dial. Note that the
built-in DXCC award has no band limit, so almost every entity spotted on 2 m or 70 cm counts as needed: restrict
`dxcc` with `bands` in an awards file if that floods the list; DXLog.net UDP XML on
`[contest] listen`; Log4OM `ADIF_MESSAGE` UDP on `[log4om] listen` (not started when `[station_log]` is set);
station-log (`[station_log] url`, default `http://127.0.0.1:8096`: `GET /v1/worked?own=<own_call>` at start, on
every MQTT reconnect and every `reload_minutes`, plus the QoS 1 events on `muehle/hf/station-log/event`;
`added`/`merged` are applied in place, `deleted` triggers a reload; unreachable = `problems` entry
`station-log not reachable: ...`, the last good view stays in use); Club token API
(`/logs/{call}/dxcc-matrix`) and the public Most Wanted list; AD1C's country files (`country-files.com/bigcty/`, at start and daily); the QRZ.com XML callbook (`xmldata.qrz.com`,
login `[qrz] user` + `CHASE_QUEUE_QRZ_PASSWORD`; entries cached in SQLite for `refresh_days`, not-found for a day).
