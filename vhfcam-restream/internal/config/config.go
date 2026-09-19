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

	// Supervision.
	StallTimeoutSec int `toml:"stall_timeout_s"` // no ffmpeg progress for this long -> kill + restart
	RestartMinSec   int `toml:"restart_min_s"`   // initial restart backoff
	RestartMaxSec   int `toml:"restart_max_s"`   // backoff cap
	StableRunSec    int `toml:"stable_run_s"`    // a run longer than this resets the backoff

	LogLevel string `toml:"log_level"`
}

// Default returns the built-in defaults (used when the config file does not
// exist at the default path, and as the base every file decodes into).
func Default() Config {
	return Config{
		Quality:         "sd",
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
		LogLevel:        "info",
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
