# hf-mqtt-capture

Passive MQTT traffic recorder for the HF station bus on shari.

- Subscribes to `muehle/hf/#` (configurable `site`/`station`).
- Writes one line per message with an RFC3339Nano timestamp:
  ```
  2026-08-19T10:23:45.123Z muehle/hf/radio/state {"freq_hz":14175000,"band":"20m","tx":"rx"}
  ```
- Rotates log files hourly: `/var/log/hf-mqtt-capture/YYYY-MM-DD/HH.log`.
- Retains the last 72 hours of logs (configurable).
- Runs as a hardened systemd service.

## Deploy

```bash
cd /Users/ingomar.otter/dev/stationa/hf-mqtt-capture
./deploy.sh
```

The deploy script cross-compiles for Linux arm64, copies the binary to shari, and
installs a systemd unit. The MQTT password is seeded into `/etc/hf-mqtt-capture/config.toml`
(0600) from an existing station service env file on shari; subsequent deploys leave the
config untouched.

## Inspect captured traffic

```bash
ssh io@192.168.1.139
# Live tail of current hour
sudo tail -F /var/log/hf-mqtt-capture/$(date +%Y-%m-%d)/$(date +%H).log
# Specific hour
sudo cat /var/log/hf-mqtt-capture/2026-08-19/10.log
```

## Configuration

Edit `/etc/hf-mqtt-capture/config.toml` on shari:

```toml
broker   = "tcp://192.168.1.50:1883"
user     = "hf"
password = ""
site     = "muehle"
station  = "hf"
log_dir  = "/var/log/hf-mqtt-capture"
retention_hours = 72
```

Then restart the service:

```bash
sudo systemctl restart hf-mqtt-capture
```
