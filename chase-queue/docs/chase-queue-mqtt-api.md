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
 "undo":"EA8XYZ","problems":["Club Log confirmations not loaded yet"],
 "contest":{"id":1,"name":"UKEIDXSSB","start_date":"2026-11-01","qsos":212}}
```

- `seq` echoes the candidates' `seq`. `ranked` is the list, in work order; a row bmrec has that is absent
  here is not listed (spotted, worked, skipped, handled).
- `activity` is `dx` or `contest`; `contest` is present in a contest. `tag` is `need` (DX) or `mult` (contest)
  on needed rows, absent otherwise. `undo` names what Undo would reverse (a callsign or `skip`).
- `problems` are what keeps the ranking from being right, worst first; absent when none.
- Rows from the DX cluster (`[cluster]`, chase-queue's own telnet login) carry `source:"cluster"`, a
  **negative** `row_id` (no bmrec row; stable while the spot lasts) and
  `spot:{freq_hz, spotter, comment, at}` of the latest spot. Only **needed** cluster stations are listed (at
  most 40), ranked by the same rules as the heard rows and mixed into the same order. A heard station that is
  on the cluster leaves the list and its cluster row stands for it. Skimmer spots (unless `skimmer = true`)
  and the operator's own spots are dropped; a spot lasts 30 minutes. `skip`/`handled`/`undo` work on cluster
  rows by their `row_id`. A consumer that joins rows to bmrec's board by `row_id` must treat `source:"cluster"`
  rows as having no board row.
- Each row with a call may carry two enrichments (both optional; consumers must not depend on them):
  - `info` — the QRZ.com callbook entry: `{source:"qrz", call, name, qth, state, country, grid, lat, lon,
    image, cq_zone, itu_zone}`. `call` is the entry's call (the home call of a portable form); `image` is a
    picture URL. Absent until looked up (the row republishes when the lookup lands), when QRZ does not have
    the call, or with no `[qrz]` login. Grid, lat/lon and image need a QRZ XML subscription. Street address,
    e-mail and birth date are never published.
  - `worked_before` — `{qsos, bands, modes, this_band, last}` from the station log (Log4OM, Club Log) and the
    stored DXLog contest logs, any mode, ever (contest logs are pruned with their instance after 30 days).
    A QSO two sources report counts once (same band and minute). `bands` is low to high; `this_band` says
    the row's own band is among them. Absent when the call was never worked.

```json
{"row_id":7,"call":"EA8XYZ","band":"20m","needed":true,"tag":"need",
 "info":{"source":"qrz","call":"EA8XYZ","name":"Juan Perez","qth":"Las Palmas","country":"Canary Islands",
         "grid":"IL28gc","lat":28.1,"lon":-15.4,"image":"https://cdn-xml.qrz.com/…","cq_zone":33,"itu_zone":36},
 "worked_before":{"qsos":2,"bands":["40m","15m"],"modes":["CW","FT8"],"this_band":false,"last":"2025-05-01T10:00:00Z"}}
```
- Republished on change and every 30 s.

## 4. `/cmd` (`shared/schema.CmdPayload`; `value` is a string)

| action | value | effect |
|---|---|---|
| `skip` | `{"row_id","band","call"}` | hide the station: contest until its call is confirmed, DX until 00:00 UTC |
| `handled` | `{"row_id","band","call","how"}` | contest QSY/spot mark; no-op in DX |
| `undo` | `""` | reverse the last skip/handled of this activity |
| `set_mode` | `"dx"` | leave contest mode |
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

## 6. Local HTTP (loopback, `[http] listen`)

`GET /healthz`; `GET /contest` (instances, definitions, prefills, status); `POST /contest/files`
`{"kind":"definition"|"prefill","name","text"}` (8 MiB). bmrec's contest setup proxies these.

## 7. Other inputs

A DX-cluster telnet node on `[cluster] address` (login `call`; reconnects with backoff, a node silent for 10
minutes is reconnected; "DX cluster not connected" in `problems` after a minute down); DXLog.net UDP XML on
`[contest] listen`; Log4OM `ADIF_MESSAGE` UDP on `[log4om] listen`; Club Log token API
(`/logs/{call}/dxcc-matrix`) and the public Most Wanted list; the QRZ.com XML callbook (`xmldata.qrz.com`,
login `[qrz] user` + `CHASE_QUEUE_QRZ_PASSWORD`; entries cached in SQLite for `refresh_days`, not-found for a day).
