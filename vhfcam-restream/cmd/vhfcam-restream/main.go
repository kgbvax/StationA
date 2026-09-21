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
	"fmt"
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
	// the player page just reports "offline". The radio control buttons ride
	// the same server (POST /api/cmd/{action} -> MQTT), and the status
	// indicators poll GET /api/radio-status.
	var audioStatus preview.AudioSourceStatus
	pvSrv := preview.NewServer(func() config.PreviewConfig { return curCfg.Load().Preview },
		nil,
		logger.With("component", componentName, "subcomponent", "preview")).
		WithStatus(func() preview.RadioStatus {
			rl := ov.RadioLink()
			return preview.RadioStatus{
				RadioOnline:      rl.BridgeOnline && rl.DeviceOnline,
				SessionConnected: rl.AudioDemand && rl.SessionState == "live",
				AudioStream:      audioStatus.Alive(),
				RadioResponding:  rl.Responding,
			}
		})
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
			if err := preview.StartAudioSource(ctx, cfg.Preview.RadioAudio, tcpAddr, &audioStatus, srcLog); err != nil {
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
	// The web-page controls flip audioManualOn: a manual disconnect stops
	// the heartbeat until the page reconnects.
	audioDemandOn := &atomic.Bool{}
	audioDemandOn.Store(true)
	radioPublish := func(action string) error {
		c := ov.Client()
		if c == nil {
			return errors.New("mqtt not connected")
		}
		var payload string
		switch action {
		case "audio_on":
			payload = `{"action":"audio_on","value":"on"}`
		case "audio_off":
			payload = `{"action":"audio_off","value":"off"}`
		case "power_on":
			payload = `{"action":"power_on","value":"on"}`
		default:
			return fmt.Errorf("unknown radio action %q", action)
		}
		topic := curCfg.Load().Preview.RadioAudioCmdTopic
		if tok := c.Publish(topic, 0, false, payload); tok.Wait() && tok.Error() != nil {
			return tok.Error()
		}
		logger.Info("radio cmd published", "topic", topic, "action", action)
		return nil
	}
	pvSrv.WithCmd(func(action string) error {
		switch action {
		case "audio_on":
			audioDemandOn.Store(true)
		case "audio_off":
			audioDemandOn.Store(false)
		}
		return radioPublish(action)
	})
	audioDemand := func(on bool) {
		if !curCfg.Load().Preview.Enabled || curCfg.Load().Preview.RadioAudio == "" {
			return
		}
		if on && !audioDemandOn.Load() {
			return // operator disconnected via the page — heartbeat stays off
		}
		if err := radioPublish(map[bool]string{true: "audio_on", false: "audio_off"}[on]); err != nil {
			logger.Warn("radio audio demand publish failed", "err", err)
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
