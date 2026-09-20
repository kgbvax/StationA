// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config loads the vhfcam-restream TOML configuration.
//
// Convention (docs/conventions/config-and-secrets.md): the config file is
// seed-once, 0600 on the device, and carries the secrets (the RTSPS token in
// the source URL and the YouTube stream key). Load preserves fs.ErrNotExist so
// the caller can tell "no file at the default path" from "bad file".
package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the vhfcam-restream configuration.
type Config struct {
	// Source URLs (rtsps). Quality selects which one feeds the stream.
	SourceURL   string `toml:"source_url"`   // SD stream
	SourceHDURL string `toml:"source_hd_url"` // HD stream; empty -> sd only
	Quality     string `toml:"quality"`       // "sd" | "hd"; switched live via SIGHUP

	// YouTube RTMP ingest.
	YouTubeURL string `toml:"youtube_url"` // e.g. rtmp://a.rtmp.youtube.com/live2
	StreamKey  string `toml:"stream_key"`  // secret; VHFCAM_STREAM_KEY env overrides

	// ffmpeg invocation.
	FFmpegBin     string `toml:"ffmpeg_bin"`
	RTSPTransport string `toml:"rtsp_transport"` // tcp | udp
	VideoMap      string `toml:"video_map"`      // ffmpeg -map for video
	AudioMap      string `toml:"audio_map"`      // ffmpeg -map for audio (must select an FLV-compatible codec)
	VideoCodec    string `toml:"video_codec"`    // -c:v
	AudioCodec    string `toml:"audio_codec"`    // -c:a

	// Sinks. The service is disabled at boot by default (deploy.sh), so nothing
	// streams constantly; when started, these pick which sinks run. The
	// preview is deliberately independent of YouTube: the LAN preview must
	// work when the internet (or YouTube) is down.
	YoutubeEnabled bool `toml:"youtube_enabled"`

	// Supervision.
	StallTimeoutSec int `toml:"stall_timeout_s"` // no ffmpeg progress for this long -> kill + restart
	RestartMinSec   int `toml:"restart_min_s"`   // initial restart backoff
	RestartMaxSec   int `toml:"restart_max_s"`   // backoff cap
	StableRunSec    int `toml:"stable_run_s"`    // a run longer than this resets the backoff

	// Operational-data overlay (drawtext burn-in). The overlay writer always
	// runs (its textfiles must exist before ffmpeg inits the drawtext filters);
	// Enabled only gates whether ffmpeg gets the -vf chain.
	Overlay OverlayConfig `toml:"overlay"`

	// Local web-browser preview: a second ffmpeg writes HLS to tmpfs and a
	// built-in HTTP server serves it to LAN browsers.
	Preview PreviewConfig `toml:"preview"`

	LogLevel string `toml:"log_level"`
}

// PreviewConfig configures the local HLS preview sink.
type PreviewConfig struct {
	Enabled  bool   `toml:"enabled"`
	HTTPAddr string `toml:"http_addr"` // listen address for the built-in server
	Dir      string `toml:"dir"`       // HLS output (tmpfs on the device)
	HlsTimeS int    `toml:"hls_time_s"`
	ListSize int    `toml:"hls_list_size"`
}

// OverlayConfig configures the MQTT-fed drawtext overlay.
type OverlayConfig struct {
	Enabled bool `toml:"enabled"`

	MQTTBroker   string `toml:"mqtt_broker"`
	MQTTUser     string `toml:"mqtt_user"`
	MQTTPassword string `toml:"mqtt_password"` // secret; VHFCAM_MQTT_PASSWORD env overrides

	Site    string `toml:"site"`    // status/state plane prefix
	Station string `toml:"station"`
	Slot    string `toml:"slot"`

	TopicAZ    string `toml:"topic_az"`    // rotator state carrying az
	TopicEL    string `toml:"topic_el"`    // rotator state carrying el
	TopicRadio string `toml:"topic_radio"` // radio state carrying freq_hz/tx

	Dir         string  `toml:"dir"`         // drawtext textfile directory (tmpfs on the device)
	StaleAfterS float64 `toml:"stale_after_s"` // max age of a snapshot's own ts before the field renders --- (rotators are change-only publishers; liveness comes from /status + device_online, not republish cadence)
	RefreshS    float64 `toml:"refresh_s"` // writer cadence (seconds, may be fractional)

	FontFile string `toml:"fontfile"`
	FontSize int    `toml:"font_size"`
	Margin   int    `toml:"margin"`
}

// Default returns the built-in defaults (used when the config file does not
// exist at the default path, and as the base every file decodes into).
func Default() Config {
	return Config{
		Quality:         "sd",
		YoutubeEnabled:  true,
		YouTubeURL:      "rtmp://a.rtmp.youtube.com/live2",
		FFmpegBin:       "/usr/bin/ffmpeg",
		RTSPTransport:   "tcp",
		VideoMap:        "0:v:0",
		AudioMap:        "0:a:0",
		VideoCodec:      "copy",
		AudioCodec:      "aac",
		StallTimeoutSec: 60,
		RestartMinSec:   5,
		RestartMaxSec:   300,
		StableRunSec:    120,
		Preview: PreviewConfig{
			Enabled:  false,
			HTTPAddr: ":8083",
			Dir:      "/run/vhfcam-restream/preview",
			HlsTimeS: 2,
			ListSize: 6,
		},
		Overlay: OverlayConfig{
			Enabled:      false,
			MQTTBroker:   "tcp://192.168.1.50:1883",
			MQTTUser:     "hf",
			Site:         "muehle",
			Station:      "hf",
			Slot:         "vhfcam",
			TopicAZ:      "muehle/uhf/az-rotator/state",
			TopicEL:      "muehle/uhf/el-rotator/state",
			TopicRadio:   "muehle/uhf/radio/state",
			Dir:          "/run/vhfcam-restream/overlay",
			StaleAfterS:  3600,
			RefreshS:     0.5,
			FontFile:     "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
			FontSize:     28,
			Margin:       12,
		},
		LogLevel: "info",
	}
}

// Load reads path into Default() and validates. Missing files surface as
// *fs.PathError (errors.Is(err, fs.ErrNotExist)); malformed files and invalid
// values are errors too.
func Load(path string) (Config, error) {
	cfg := Default()
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return cfg, err
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// ApplyEnv applies environment overrides (EnvironmentFile pattern).
func (c *Config) ApplyEnv() {
	if v := os.Getenv("VHFCAM_STREAM_KEY"); v != "" {
		c.StreamKey = v
	}
	if v := os.Getenv("VHFCAM_MQTT_PASSWORD"); v != "" {
		c.Overlay.MQTTPassword = v
	}
}

// Validate checks required fields and value ranges.
func (c *Config) Validate() error {
	if c.SourceURL == "" && c.SourceHDURL == "" {
		return fmt.Errorf("config: source_url and source_hd_url are both empty")
	}
	switch c.Quality {
	case "":
		c.Quality = "sd"
	case "sd", "hd":
	default:
		return fmt.Errorf(`config: quality must be "sd" or "hd", got %q`, c.Quality)
	}
	if c.Quality == "hd" && c.SourceHDURL == "" {
		return fmt.Errorf(`config: quality is "hd" but source_hd_url is empty`)
	}
	if !strings.HasPrefix(c.YouTubeURL, "rtmp://") && !strings.HasPrefix(c.YouTubeURL, "rtmps://") {
		return fmt.Errorf("config: youtube_url must be an rtmp(s):// URL, got %q", c.YouTubeURL)
	}
	if c.StreamKey == "" {
		return fmt.Errorf("config: stream_key is empty (set it in the config or via VHFCAM_STREAM_KEY)")
	}
	if c.FFmpegBin == "" || c.RTSPTransport == "" || c.VideoMap == "" || c.AudioMap == "" ||
		c.VideoCodec == "" || c.AudioCodec == "" {
		return fmt.Errorf("config: ffmpeg_bin, rtsp_transport, video_map, audio_map, video_codec and audio_codec must all be set")
	}
	if c.StallTimeoutSec <= 0 || c.RestartMinSec <= 0 || c.RestartMaxSec < c.RestartMinSec || c.StableRunSec <= 0 {
		return fmt.Errorf("config: supervision timings invalid (need stall_timeout_s > 0, restart_min_s > 0, restart_max_s >= restart_min_s, stable_run_s > 0)")
	}
	if err := c.Overlay.validate(); err != nil {
		return err
	}
	if c.Preview.Enabled && (c.Preview.Dir == "" || c.Preview.HlsTimeS <= 0 || c.Preview.ListSize <= 0) {
		return fmt.Errorf("config: preview.dir, preview.hls_time_s and preview.hls_list_size must be set when enabled")
	}
	return nil
}

func (o *OverlayConfig) validate() error {
	if !o.Enabled {
		return nil
	}
	if o.MQTTBroker == "" {
		return fmt.Errorf("config: overlay.enabled but overlay.mqtt_broker is empty")
	}
	if o.Dir == "" || o.FontFile == "" {
		return fmt.Errorf("config: overlay.dir and overlay.fontfile must be set when enabled")
	}
	if o.StaleAfterS <= 0 || o.RefreshS <= 0 || o.FontSize <= 0 || o.Margin < 0 {
		return fmt.Errorf("config: overlay timings/sizes invalid")
	}
	return nil
}

// EffectiveSource returns the stream URL for the configured quality plus a
// profile label for logging. Falls back to the SD URL if HD was requested but
// is not configured (Validate already rejects that combination; this is the
// defensive path for programmatically-built configs).
func (c Config) EffectiveSource() (url, profile string) {
	if c.Quality == "hd" && c.SourceHDURL != "" {
		return c.SourceHDURL, "hd"
	}
	return c.SourceURL, "sd"
}
