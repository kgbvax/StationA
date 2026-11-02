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

DXLog.net UDP XML on `[contest] listen`; Log4OM `ADIF_MESSAGE` UDP on `[log4om] listen`; Club Log token API
(`/logs/{call}/dxcc-matrix`) and the public Most Wanted list.
