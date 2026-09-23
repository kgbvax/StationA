// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultFailsValidateWithoutSources(t *testing.T) {
	var cfg = Default()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected Validate to fail with no source URLs")
	}
}

func TestLoadGoodFile(t *testing.T) {
	path := writeTemp(t, `
source_url    = "rtsps://cam:7441/SD?enableSrtp"
source_hd_url = "rtsps://cam:7441/HD?enableSrtp"
quality       = "hd"
youtube_url   = "rtmp://a.rtmp.youtube.com/live2"
stream_key    = "k0-key"
stall_timeout_s = 30
log_level     = "warn"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SourceURL != "rtsps://cam:7441/SD?enableSrtp" {
		t.Errorf("source_url = %q", cfg.SourceURL)
	}
	if cfg.Quality != "hd" {
		t.Errorf("quality = %q", cfg.Quality)
	}
	if cfg.FFmpegBin != "/usr/bin/ffmpeg" {
		t.Errorf("ffmpeg_bin default not preserved: %q", cfg.FFmpegBin)
	}
	url, profile := cfg.EffectiveSource()
	if url != cfg.SourceHDURL || profile != "hd" {
		t.Errorf("EffectiveSource = (%q, %q), want HD", url, profile)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want fs.ErrNotExist, got %v", err)
	}
}

func TestLoadMalformedFile(t *testing.T) {
	path := writeTemp(t, "this is not toml [[[")
	_, err := Load(path)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want parse error, got %v", err)
	}
}

func TestApplyEnvOverridesStreamKey(t *testing.T) {
	t.Setenv("VHFCAM_STREAM_KEY", "env-key")
	cfg := Default()
	cfg.StreamKey = "file-key"
	cfg.SourceURL = "rtsps://cam/stream"
	cfg.ApplyEnv()
	if cfg.StreamKey != "env-key" {
		t.Errorf("stream_key = %q, want env override", cfg.StreamKey)
	}
}

func TestValidateCases(t *testing.T) {
	base := func() Config {
		c := Default()
		c.SourceURL = "rtsps://cam/stream"
		c.StreamKey = "k"
		return c
	}

	c := base()
	if err := c.Validate(); err != nil {
		t.Errorf("baseline: %v", err)
	}

	sdSrc, sdProfile := c.EffectiveSource()
	if sdProfile != "sd" || sdSrc != "rtsps://cam/stream" {
		t.Errorf("EffectiveSource sd = (%q, %q)", sdSrc, sdProfile)
	}

	hdMissing := base()
	hdMissing.Quality = "hd"
	if err := hdMissing.Validate(); err == nil {
		t.Error("quality=hd without source_hd_url should fail")
	}

	hdOK := hdMissing
	hdOK.SourceHDURL = "rtsps://cam/hd"
	if err := hdOK.Validate(); err != nil {
		t.Errorf("quality=hd with url: %v", err)
	}

	badQuality := base()
	badQuality.Quality = "uhd"
	if err := badQuality.Validate(); err == nil {
		t.Error("quality=uhd should fail")
	}

	noKey := base()
	noKey.StreamKey = ""
	if err := noKey.Validate(); err == nil {
		t.Error("empty stream_key should fail")
	}

	badTimings := base()
	badTimings.RestartMaxSec = 1
	badTimings.RestartMinSec = 5
	if err := badTimings.Validate(); err == nil {
		t.Error("restart_max < restart_min should fail")
	}
}
