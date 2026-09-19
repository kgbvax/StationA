# CLAUDE.md — vhfcam-restream

vhfcam-restream copies the shack VHF camera's (UniFi Protect) RTSPS stream to
YouTube Live over RTMP. It supervises **one `ffmpeg` process**: ffmpeg does the
streaming (RTSPS in, FLV out), the Go binary owns liveness — restart with
exponential backoff, kill on stall, SIGHUP-driven profile switch. **Not an MQTT
slot** — no `/meta`, `/state`, `/cmd` planes; it is a plain media relay and is
deliberately outside the bus.

## Why the args look the way they do

Both camera profiles (SD 1024x576, HD 1920x1080, both @20fps) emit **H.264
video + two audio tracks (AAC mono, Opus stereo)**. RTMP/FLV can only carry
AAC, and ffmpeg's default stream selection would pick the 2-channel **Opus**
track and fail — so `audio_map = "0:a:m:aac"` pins the AAC track explicitly,
and both streams are `-c copy` (no transcoding on the Pi). Do not "simplify"
the maps away; a camera firmware change that reorders streams is exactly what
the codec-based map survives.

## Commands

```bash
go build ./cmd/vhfcam-restream   # local build
go test ./...                    # unit tests (fake ffmpeg via /bin/sh scripts)
go vet ./...
./deploy.sh                      # cross-compile + ship + systemd on shari
```

`deploy.sh` seeds the config on first deploy only; it takes `SOURCE_URL`,
`SOURCE_HD_URL`, `QUALITY`, `YT_URL`, `YT_STREAM_KEY`, `LOG_LEVEL` from the
environment (see the header comment).

## Config

`/etc/vhfcam-restream/config.toml` on shari (0600, seed-once; see
`../docs/conventions/config-and-secrets.md`). **It contains both secrets**: the
RTSPS path tokens (inside `source_url`/`source_hd_url`) and the YouTube stream
key (`stream_key`, env override `VHFCAM_STREAM_KEY`).

Profile switch (sd ↔ hd) without a service restart:

```bash
sudo -e /etc/vhfcam-restream/config.toml   # set quality = "hd"
sudo systemctl kill -s SIGHUP vhfcam-restream
```

SIGHUP re-reads the config and restarts ffmpeg. A failed reload (malformed
file) keeps the current config *and* the current stream running. `log_level`
changes are picked up on the next service restart only.

## Ops

```bash
ssh io@192.168.1.139 'journalctl -u vhfcam-restream -f'   # progress heartbeats every 60s
ssh io@192.168.1.139 'sudo systemctl status vhfcam-restream'
```

- A restart loop with "Could not find tag for codec" in the log means the
  camera started emitting a codec FLV can't carry (e.g. the cam was switched to
  H.265) — check with `ffprobe` and set `video_codec` accordingly.
- YouTube ingest state (stream health, "offline/online") is only visible in
  YT Studio; this service only guarantees it is pushing data.
- Known property: the ffmpeg command line carries the source token and stream
  key (`ps aux` on shari shows them, journald may echo them in some ffmpeg
  errors). Accepted for a single-operator Pi; flagged here so it is a decision,
  not a surprise.

## Station model and shared conventions

Shared documentation lives in `../docs/` (this component is a subdirectory of
the stationa monorepo). Config/secrets and deployment follow
`../docs/conventions/config-and-secrets.md` and `../docs/conventions/deployment.md`.
The logging convention (`../docs/conventions/logging.md`) applies: slog text
handler on stderr, constant `component` attr; ffmpeg stderr lines are logged
Info, or Warn when they look like errors.
