# tgxl-tuner-bridge

FlexRadio **Tuner Genius XL** (1x3) → MQTT bridge for the station slot `muehle/hf/tuner`.
Polls the TGXL TCP API (port 9010) and publishes SWR, forward power, operate/bypass, antenna,
tuning state; accepts `set_inline`, `set_operate`, `set_antenna` and `tune` on `/cmd`.

See `CLAUDE.md` (architecture, hazard), `docs/tgxl-tuner-bridge-mqtt-api.md` (wire contract),
`config.example.toml`, `deploy.sh`.
