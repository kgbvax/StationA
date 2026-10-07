# CLAUDE.md — vhfcam-restream

vhfcam-restream copies the shack VHF camera's (UniFi Protect) RTSPS stream to
YouTube Live over RTMP. It supervises **one `ffmpeg` process**: ffmpeg does the
streaming (RTSPS in, FLV out), the Go binary owns liveness — restart with
exponential backoff, kill on stall, SIGHUP-driven profile switch. Since
2026-09-19 it is also an MQTT **consumer** (and minimal slot): the
operational-data overlay subscribes to the uhf rotator/radio state snapshots
and burns AZ/EL/freq/satellite into the video (see below).

## Host

Runs on **scmino** (`192.168.1.178`, Raspberry Pi CM5, Debian 12, 117 GB
NVMe), not shari — moved 2026-09-30 to take the x264 overlay encode and the
recordings off shari's SD card. Same arch (arm64), so `deploy.sh` is
unchanged apart from its default `SSH_HOST`. Two things point at this host
and move with it:

- **icom9700-radio-bridge** (still on shari) sends the radio PCM to
  `[audio] publish_addr = "192.168.1.178:45031"` in
  `/etc/icom9700-radio-bridge/config.toml` on shari.
- **hf_console** CAM tab: `defaultVhfcamBaseUrl` =
  `http://192.168.1.178:8083`; a tablet that saved the gear sheet keeps its
  own stored URL — edit it there.

Never run the service on both hosts at once: both would heartbeat
`audio_on`, fight over the `muehle/hf/vhfcam` LWT, and the bridge sends
audio to only one of them.

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
./deploy.sh                      # cross-compile + ship + systemd on scmino
```

`deploy.sh` seeds the config on first deploy only; it takes `SOURCE_URL`,
`SOURCE_HD_URL`, `QUALITY`, `YT_URL`, `YT_STREAM_KEY`, `LOG_LEVEL` from the
environment (see the header comment).

**The service starts at boot** (`ENABLED=true`, the default since 2026-10-04):
the console's CAM tab must work after a power cycle without a manual start.
Only the LAN preview runs by default — a fresh seed has `youtube_enabled =
false` (`YT_ENABLED`), so autostart never goes live on YouTube by itself.
Stop it with `sudo systemctl disable --now vhfcam-restream`, or deploy with
`ENABLED=false` to install it disabled and stopped.

## Config

`/etc/vhfcam-restream/config.toml` on scmino (0600, seed-once; see
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
`libx264 -preset superfast` + a `drawtext` chain (576p comfortable on a Pi 4,
load ~+0.8; keep the HD profile clean — 1080p software x264 is not budgeted).

- **File contract**: the writer (`internal/overlay`) renders
  `/run/vhfcam-restream/overlay/{az,el,freq,sat}.txt` at 2 Hz (tmpfs,
  `RuntimeDirectory` + `ReadWritePaths` in the unit). ffmpeg's drawtext reads
  them with `reload=1` every frame. The files MUST exist before ffmpeg inits
  drawtext — the writer runs unconditionally and seeds them at startup, which
  is what makes a SIGHUP toggle of `enabled` safe.
- **Data**: `muehle/uhf/az-rotator/state` (`az`), `muehle/uhf/el-rotator/state`
  (`el`), `muehle/uhf/radio/state` (`freq_hz`),
  `muehle/uhf/sat-track/state` (`tracking`, `sat_name`, `range_km`,
  `downlink_hz`, `uplink_hz` — from
  oscarwatch-sattrack-bridge) **plus each slot's
  `/status` LWT**. Freshness is two-layer (station model): a field renders only
  when our MQTT link is up, the source slot's `/status` is `online`, the
  snapshot says `device_online`, and the snapshot's own `ts` is younger than
  `stale_after_s` (default 3600 — the bridges are change-only publishers, so
  silence is normal and message-arrival time is *not* a staleness signal).
  Stale → `---`, never a frozen value. FREQ shows OscarWatch's
  radio-corrected `↓downlink ↑uplink` (MHz, 3 decimals) while a satellite is
  tracked, else the IC-9700's own `freq_hz`, else `FREQ ---` (user,
  2026-10-07); recordings are named after the same frequency
  (`Overlay.FreqHz`: the downlink while tracking). The SAT field
  (`SO-50 2100 km`, right-aligned, name capped at 8 chars so it cannot run
  into FREQ) is empty unless a satellite is tracked and the sat-track
  snapshot is fresh — no placeholder between passes. **No TX field** — the
  IC-9700 bridge is receive-only; the user dropped it 2026-10-07.
- **Layout** (user, 2026-10-07): the dragon logo stands in the bottom-left
  corner on the bottom edge (scaled into a 140 px box, its own PNG alpha —
  no colorkey, which destroyed the transparency); the bar text starts right
  of him (`x0 = 2·margin + 140`), AZ/EL/FREQ at x0 + {0, 4.6, 9.2}·fontsize,
  SAT right-aligned — measured on the 1024×576 SD profile (render the real
  filter on scmino) so `↓145.848 ↑435.302` and `TEVEL2-5 13802 km` still
  leave a gap.
- **Planes**: publishes `muehle/hf/vhfcam/status` (retained LWT online/offline)
  and `/state` (`mqtt_connected`, per-field stale flags). No `/meta` or
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
its own supervisor, restart backoff and stall watchdog), plus the recorder,
which copies the preview's segments (see Recording):

1. **YouTube RTMP push** (`youtube_enabled`, default true) — the original sink.
2. **Local HLS preview** (`[preview] enabled`, default true) — a second ffmpeg
   writes `live.m3u8` + segments to `/run/vhfcam-restream/preview` (tmpfs) and
   the built-in HTTP server (`:8083`) serves a minimal player page at
   `http://<scmino>:8083/`. **hls.js is vendored into the binary** (`go:embed`,
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
   `1:a:0` in both sinks — until 2026-09-25 a reset in
   `buildInputsAndOverlay` silently mapped the camera audio instead;
   `TestRadioAudioIsMapped` guards it) — radio off = silence, never a
   stalled preview. The bridge only
   streams on demand: this app heartbeats `audio_on` (20 s cadence) to
   `radio_audio_cmd_topic` — **opt-in, default OFF** (2026-09: capture starts
   only when the page's connect button is clicked; a restart clears the
   demand); the bridge-side demand is TTL-bounded (60 s) so a dead preview
   releases the radio's session to manual wfview (KTD-2).

## Recording

The preview page (and the hf_console CAM tab) can record what the preview
shows — overlay + radio audio — as MP4 (`internal/recorder`, `[record]`).

- **No extra encode, no ffmpeg while recording.** The recorder follows the
  preview's `live.m3u8` every second and appends each finished MPEG-TS
  segment byte-for-byte to `<record.dir>/.inprogress/<name>/partNNN.ts`
  (pre-roll: the last 3 segments ≈ what the page shows). On stop, one
  `ffmpeg -f concat … -c copy` remuxes the parts into `<name>.mp4`; if that
  fails the parts are kept as `.ts`. A preview ffmpeg restart (sequence runs
  backwards, or a copied segment name reappears with a new mtime) starts a
  new part; concat shifts the timestamps.
- **Crash-safe.** Parts are append-only TS; leftovers in `.inprogress` are
  finalized at the next startup (`stop_reason = recovered`). SIGTERM
  finalizes an active recording (20 s bound) before the process exits.
- **Limits.** Auto-stop after `max_minutes` (30) and when free space on the
  recordings filesystem drops below `min_free_gb` (8, checked every 5 s).
  A start needs `min_free_gb` plus room for a full-length recording twice
  (TS parts + MP4 during the remux, ≈1.2 GB at 30 min). The oldest finished
  recordings are deleted to stay under `max_total_gb` (5 ≈ eight 30-min
  recordings) — never to make free space. The cap must stay below what the
  floor leaves usable, or rotation never runs and starts get refused
  instead: the defaults were sized for shari's 29 GB SD card (15 GB free,
  2026-09-25). scmino's NVMe has ~108 GB free, so `max_total_gb` can be
  raised in the device config if more history is wanted.
- **Radio audio.** A recording holds the radio-audio demand (holder
  `recording`, `internal/preview/demand.go`); the page's disconnect only
  releases the page's hold, so it cannot cut audio out of a recording.
- **Names.** `vhfcam_<UTC start>[_<freq>MHz][_n].mp4`, e.g.
  `vhfcam_2026-09-25T1812Z_435.000MHz.mp4`; the frequency is the overlay's
  (`Overlay.FreqHz`, same freshness rule as the burned-in text).
- **API.** `POST /api/rec/start|stop` (409 busy / not recording, 507 low
  disk, 503 no preview or disabled), `GET /api/rec` (status + files),
  `GET|DELETE /api/rec/files/{name}` (strict name regex, regular files only,
  Range downloads). The `rec` block also rides `GET /api/radio-status`.
- Defaults come from `config.Default()`, so the seeded device config needs no
  edit; the unit's StateDirectory already allows the path.

## Ops

```bash
ssh io@192.168.1.178 'journalctl -u vhfcam-restream -f'   # progress heartbeats every 60s
ssh io@192.168.1.178 'sudo systemctl status vhfcam-restream'
```

- A restart loop with "Could not find tag for codec" in the log means the
  camera started emitting a codec FLV can't carry (e.g. the cam was switched to
  H.265) — check with `ffprobe` and set `video_codec` accordingly.
- YouTube ingest state (stream health, "offline/online") is only visible in
  YT Studio; this service only guarantees it is pushing data.
- Known property: the ffmpeg command line carries the source token and stream
  key (`ps aux` on scmino shows them, journald may echo them in some ffmpeg
  errors). Accepted for a single-operator Pi; flagged here so it is a decision,
  not a surprise.

## Station model and shared conventions

Shared documentation lives in `../docs/` (this component is a subdirectory of
the stationa monorepo). Config/secrets and deployment follow
`../docs/conventions/config-and-secrets.md` and `../docs/conventions/deployment.md`.
The logging convention (`../docs/conventions/logging.md`) applies: slog text
handler on stderr, constant `component` attr; ffmpeg stderr lines are logged
Info, or Warn when they look like errors.
