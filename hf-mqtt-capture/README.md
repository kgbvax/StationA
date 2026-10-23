# hf-mqtt-capture

Passive MQTT traffic recorder for the HF station bus. Deployed on shari (`192.168.1.139`).

- Subscribes to `muehle/hf/#` (configurable `site`/`station`).
- Writes one line per message with an RFC3339Nano timestamp:
  ```
  2026-08-19T10:23:45.123Z muehle/hf/radio/state {"freq_hz":14175000,"band":"20m","tx":"rx"}
  ```
- Rotates log files hourly: `/var/log/hf-mqtt-capture/YYYY-MM-DD/HH.log` (UTC date and hour).
- Retains the last 72 hours of logs (configurable).
- Runs as a hardened systemd service.

## Deploy

```bash
cd /Users/ingomar.otter/dev/stationa/hf-mqtt-capture
./deploy.sh
```

The deploy script cross-compiles for Linux arm64, copies the binary to shari, and
installs a systemd unit. The MQTT password is seeded into `/etc/hf-mqtt-capture/config.toml`
(0600) from an existing station service env file on the target; subsequent deploys leave the
config untouched. deploy.sh looks for the acom1200s-pa-bridge, flexbridge, hadiscovery and
atr1k-tuner-bridge env files. Those services run on scmino since 2026-10-12, so on shari the
pull works only if their old env files remain; otherwise set the password on the device.

## Inspect captured traffic

```bash
ssh io@192.168.1.139
# Live tail of current hour (files are named in UTC)
sudo tail -F /var/log/hf-mqtt-capture/$(date -u +%Y-%m-%d)/$(date -u +%H).log
# Specific hour (UTC)
sudo cat /var/log/hf-mqtt-capture/2026-08-19/10.log
```

## Configuration

Edit `/etc/hf-mqtt-capture/config.toml` on shari:

```toml
broker   = "tcp://127.0.0.1:1883"   # deploy.sh seed default; shari runs no broker
user     = "hf"
password = ""
site     = "muehle"
station  = "hf"
log_dir  = "/var/log/hf-mqtt-capture"
retention_hours = 72
```

The seed default `tcp://127.0.0.1:1883` reaches nothing on shari (its mosquitto is
inactive), so the live config must name a remote broker, e.g. `tcp://192.168.1.50:1883`.
Which broker the deployed config uses is not recorded in the repo.

Then restart the service:

```bash
sudo systemctl restart hf-mqtt-capture
```

## License

Copyright © 2026 Ingomar Otter.

Licensed under the GNU Affero General Public License v3.0 or later
(SPDX: `AGPL-3.0-or-later`) — see [LICENSE](LICENSE).
