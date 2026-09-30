# oscarwatch-sattrack-bridge

Publishes the satellite the station is tracking on the Mühle bus. The source is
OscarWatch's "Satellite link" WebSocket; the output is the retained slot
`muehle/uhf/sat-track`.

```
OscarWatch (BWPC)  ──ws://192.168.1.197:7373/──►  oscarwatch-sattrack-bridge (scmino)
   Satellite link v1                                  │
                                                      ▼
                                   muehle/uhf/sat-track/{meta,state,status}
```

The `/state` snapshot carries:
- `sat_name`, `norad_id`, `mode_type`
- `az`, `el`, `range_km`, `range_rate_km_s`, `sunlit`
- `in_range` and `tracking`
- the derived sub-satellite point `sub_lat`, `sub_lng`, `alt_km`
- `uplink_hz`/`downlink_hz` with canonical modes and bands, and `doppler`

With nothing tracked, or OscarWatch unreachable, the satellite keys are `null`. The full
contract is in [docs/oscarwatch-sattrack-bridge-mqtt-api.md](docs/oscarwatch-sattrack-bridge-mqtt-api.md).

## OscarWatch setup (one-time, on the shack PC)

Open Settings → Integrations → Satellite link:
- **Enable** the Satellite link WebSocket server; port **7373**.
- Turn on **Allow connections from local network**.
- Allow OscarWatch through the Windows firewall (private networks).
- Press Save.

## Build, test, deploy

```bash
go test ./... -race
MQTT_PASSWORD=... ./deploy.sh     # first deploy to scmino (seeds config + env once)
./deploy.sh                       # updates
```

Watch it:

```bash
ssh io@192.168.1.178 journalctl -u oscarwatch-sattrack-bridge -f
mosquitto_sub -h 192.168.1.50 -u hf -P "$MQTT_PASSWORD" -t 'muehle/uhf/sat-track/#' -v
```

Configuration: [config.example.toml](config.example.toml). Operational facts are in
[CLAUDE.md](CLAUDE.md).
