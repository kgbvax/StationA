# CLAUDE.md — vhfcam-restream

vhfcam-restream copies the shack VHF camera's (UniFi Protect) RTSPS stream to
YouTube Live over RTMP. It supervises **one `ffmpeg` process**: ffmpeg does the
streaming (RTSPS in, FLV out), the Go binary owns liveness — restart with
exponential backoff, kill on stall, SIGHUP-driven profile switch. Since
2026-09-19 it is also an MQTT **consumer** (and minimal slot): the
operational-data overlay subscribes to the uhf rotator/radio state snapshots
and burns AZ/EL/freq/TX into the video (see below).

## Why the args look the way they do

Both camera profiles (Medium 1024x576, HD 1920x1080, both @20fps) carry
**H.264 video + AAC and Opus audio tracks** — but at least one SDP session was
observed exposing **Opus only** (2026-09-19: `Stream map '0:a:m:aac' matches
no streams`), so a codec-selecting audio map is not safe. The mapping is
therefore `audio_map = "0:a:0"` (first audio track, whatever codec) +
`audio_codec = "aac"`: RTMP/FLV carries AAC only, and transcoding one mono/
stereo track to AAC costs nothing on the Pi. Video is always `-c copy`. Do
not "simplify" this back to `-c:a copy` — a bare copy breaks the moment the
track is Opus, which is what the default stream selection picks.

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

**The service installs DISABLED by default** (`ENABLED=false`): the unit is on
the device but not running — the Pi does not stream constantly. Bring it up ad
hoc on shari with `sudo systemctl enable --now vhfcam-restream`, stop it with
`sudo systemctl disable --now vhfcam-restream`, or deploy with `ENABLED=true`
to have deploys start it.

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

## Overlay (operational-data burn-in)

`[overlay] enabled = true` switches the video from `-c:v copy` to
`libx264 -preset superfast` + a `drawtext` chain (576p comfortable on the Pi 4,
load ~+0.8; keep the HD profile clean — 1080p software x264 is not budgeted).

- **File contract**: the writer (`internal/overlay`) renders
  `/run/vhfcam-restream/overlay/{az,el,freq,tx}.txt` at 2 Hz (tmpfs,
  `RuntimeDirectory` + `ReadWritePaths` in the unit). ffmpeg's drawtext reads
  them with `reload=1` every frame. The files MUST exist before ffmpeg inits
  drawtext — the writer runs unconditionally and seeds them at startup, which
  is what makes a SIGHUP toggle of `enabled` safe.
- **Data**: `muehle/uhf/az-rotator/state` (`az`), `muehle/uhf/el-rotator/state`
  (`el`), `muehle/uhf/radio/state` (`freq_hz`, `tx`) **plus each slot's
  `/status` LWT**. Freshness is two-layer (station model): a field renders only
  when our MQTT link is up, the source slot's `/status` is `online`, the
  snapshot says `device_online`, and the snapshot's own `ts` is younger than
  `stale_after_s` (default 3600 — the bridges are change-only publishers, so
  silence is normal and message-arrival time is *not* a staleness signal).
  Stale → `---`, never a frozen value. The red TX line just goes empty when
  not transmitting.
- **Planes**: publishes `muehle/hf/vhfcam/status` (retained LWT online/offline)
  and `/state` (`mqtt_connected`, per-field stale flags, `tx`). No `/meta` or
  `/cmd` yet. Broker is the **hassio** one (`tcp://192.168.1.50:1883`) — the
  shari-local mosquitto of the repo docs is not what is deployed (see
  memory `station-broker-hassio`).
- paho conventions apply: handlers Enqueue (never publish inline),
  ctx-aware connect via `shared/mqtt`.
- Later phases (planned, not built): phase 2 = Go-rendered overlay PNG piped
  to ffmpeg for a minimap + worked-station (from `muehle/hf/spots`) + sat
  subpoint via SGP4; phase 3 = RX/TX audio via an ALSA line tap (hardware).

## Sinks

The app runs **two independent sinks** (separate ffmpeg processes, each with
its own supervisor, restart backoff and stall watchdog):

1. **YouTube RTMP push** (`youtube_enabled`, default true) — the original sink.
2. **Local HLS preview** (`[preview] enabled`, default true) — a second ffmpeg
   writes `live.m3u8` + segments to `/run/vhfcam-restream/preview` (tmpfs) and
   the built-in HTTP server (`:8083`) serves a minimal player page at
   `http://<shari>:8083/`. **hls.js is vendored into the binary** (`go:embed`,
   Apache-2.0, see `internal/preview/assets/NOTICE`) so the page works with
   the internet down — the main reason a local preview exists. ~10 s behind
   live (`hls_time_s 2`, `hls_list_size 6`). No auth: LAN-only service, same
   posture as testui. The preview is deliberately a separate process so a
   YouTube/internet outage never blanks the LAN view; sink toggles apply via
   SIGHUP (the idle supervisor waits on the reload wake).
   **Radio audio** (`radio_audio`, e.g. ":45031"): replaces the camera's
   audio track with the IC-9700's demodulated audio. The bridge publishes
   S16LE/48 kHz/mono UDP datagrams; `internal/preview.StartAudioSource` binds
   the UDP address, silence-fills at 10 ms cadence and feeds the preview
   ffmpeg over loopback TCP (`-f s16le -i tcp://127.0.0.1:<port>`, mapped
   `1:a:0`) — radio off = silence, never a stalled preview. The bridge only
   streams on demand: this app heartbeats `audio_on` (20 s cadence) to
   `radio_audio_cmd_topic`; the bridge-side demand is TTL-bounded (60 s) so a
   dead preview releases the radio's session to manual wfview (KTD-2).

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
