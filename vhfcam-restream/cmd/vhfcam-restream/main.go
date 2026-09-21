// SPDX-License-Identifier: AGPL-3.0-or-later

// vhfcam-restream copies the shack VHF camera's RTSPS stream to YouTube Live
// via ffmpeg. It is not an MQTT slot — a plain supervised restreamer.
//
// SIGHUP re-reads the config and restarts the stream; that is how the sd/hd
// source profile is switched without a service restart.
package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"vhfcam-restream/internal/config"
	"vhfcam-restream/internal/overlay"
	"vhfcam-restream/internal/preview"
	"vhfcam-restream/internal/restream"
)

const componentName = "vhfcam-restream"

func main() {
	def := config.Default()
	configPath := flag.String("config", "/etc/vhfcam-restream/config.toml", "path to the TOML config file")
	flag.Parse()

	// Bootstrap logger (defaults only); rebuilt with the configured level below.
	logger := newLogger(def.LogLevel)

	cfg, err := config.Load(*configPath)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist) && !isFlagSet("config"):
		logger.Info("no config file at default path; running on defaults", "path", *configPath)
	default:
		logger.Error("cannot load config", "path", *configPath, "err", err)
		os.Exit(1)
	}
	cfg.ApplyEnv()
	if err := cfg.Validate(); err != nil {
		logger.Error("invalid config", "err", err)
		os.Exit(1)
	}
	logger = newLogger(cfg.LogLevel)

	// Shared, reloadable config view: the supervisor and the overlay both read
	// the freshest config from here (SIGHUP reload stores into it).
	curCfg := &atomic.Pointer[config.Config]{}
	curCfg.Store(&cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Overlay first: its textfiles must exist before ffmpeg ever inits the
	// drawtext filters. The writer always runs; overlay.enabled only gates the
	// ffmpeg -vf chain, so a SIGHUP toggle is safe.
	ov := overlay.New(func() config.OverlayConfig { return curCfg.Load().Overlay },
		logger.With("component", componentName, "subcomponent", "overlay"))
	if err := ov.Start(ctx); err != nil {
		logger.Error("overlay init failed", "err", err)
		os.Exit(1)
	}

	// Preview HTTP server: always on (cheap); with the preview sink disabled
	// the player page just reports "offline".
	pvSrv := preview.NewServer(func() config.PreviewConfig { return curCfg.Load().Preview },
		logger.With("component", componentName, "subcomponent", "preview"))
	go func() {
		if err := pvSrv.ListenAndServe(ctx); err != nil {
			logger.Error("preview http server", "err", err)
		}
	}()

	// Radio-audio source: bridges the icom9700-radio-bridge's UDP PCM into
	// the preview ffmpeg's TCP input, silence-filled at a 10 ms cadence so
	// the radio audio never stalls the pipeline (radio off = silence, not a
	// frozen preview).
	if cfg.Preview.RadioAudio != "" {
		srcLog := logger.With("component", componentName, "subcomponent", "radio-audio")
		go func() {
			// net.Listen wants the bare address; the scheme prefix is only
			// for the ffmpeg input URL.
			tcpAddr := strings.TrimPrefix(restream.RadioAudioInputURL(cfg.Preview.RadioAudio), "tcp://")
			if err := preview.StartAudioSource(ctx, cfg.Preview.RadioAudio, tcpAddr, srcLog); err != nil {
				srcLog.Error("radio audio source", "err", err)
			}
		}()
	}

	reload := func() (config.Config, error) {
		nc, err := config.Load(*configPath)
		if err != nil {
			return nc, err
		}
		nc.ApplyEnv()
		curCfg.Store(&nc)
		// The HLS muxer does not create directories; the preview sink needs
		// its output dir to exist before ffmpeg starts.
		if err := os.MkdirAll(nc.Preview.Dir, 0755); err != nil {
			logger.Warn("preview dir create failed", "dir", nc.Preview.Dir, "err", err)
		}
		return nc, nil
	}
	// Same, for the initial config.
	if err := os.MkdirAll(curCfg.Load().Preview.Dir, 0755); err != nil {
		logger.Error("preview dir create failed", "dir", curCfg.Load().Preview.Dir, "err", err)
		os.Exit(1)
	}
	sinkLog := logger.With("component", componentName)
	ytSup := restream.New(cfg, reload, sinkLog.With("sink", "youtube")).
		WithEnabled(func() bool { return curCfg.Load().YoutubeEnabled })
	pvSup := restream.New(cfg, reload, sinkLog.With("sink", "preview")).
		WithEnabled(func() bool { return curCfg.Load().Preview.Enabled }).
		WithArgsFn(restream.BuildPreviewArgs)

	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		for range sighup {
			logger.Info("SIGHUP — reloading config and restarting the streams")
			ytSup.Reload()
			pvSup.Reload()
		}
	}()

	// Radio-audio demand: while the preview sink runs with radio_audio set,
	// heartbeat audio_on to the radio bridge (the demand is TTL-bounded
	// there — the heartbeat is what keeps the radio's audio flowing); on
	// shutdown, release it. The MQTT connection comes from the overlay.
	audioDemand := func(on bool) {
		c := ov.Client()
		if c == nil || !curCfg.Load().Preview.Enabled || curCfg.Load().Preview.RadioAudio == "" {
			return
		}
		payload := `{"action":"audio_off","value":"off"}`
		if on {
			payload = `{"action":"audio_on","value":"on"}`
		}
		topic := curCfg.Load().Preview.RadioAudioCmdTopic
		if tok := c.Publish(topic, 0, false, payload); tok.Wait() && tok.Error() != nil {
			logger.Warn("radio audio demand publish failed", "topic", topic, "err", tok.Error())
		} else {
			logger.Info("radio audio demand published", "topic", topic, "on", on)
		}
	}
	go func() {
		// Let MQTT connect, then heartbeat at a third of the bridge's TTL.
		time.Sleep(3 * time.Second)
		audioDemand(true)
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				audioDemand(false)
				return
			case <-t.C:
				audioDemand(true)
			}
		}
	}()

	// Run both sinks until shutdown; a sink-level failure never takes the
	// other one down (each supervisor loops on its own).
	done := make(chan error, 2)
	go func() { done <- ytSup.Run(ctx) }()
	go func() { done <- pvSup.Run(ctx) }()
	if err := <-done; err != nil {
		logger.Error("supervisor terminated", "err", err)
		os.Exit(1)
	}
	logger.Info("shutdown complete")
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// isFlagSet reports whether the named flag was set explicitly on the command
// line (docs/conventions/config-and-secrets.md pattern).
func isFlagSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}
