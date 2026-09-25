// SPDX-License-Identifier: AGPL-3.0-or-later

// vhfcam-restream copies the shack VHF camera's RTSPS stream to YouTube Live
// via ffmpeg. It is not an MQTT slot — a plain supervised restreamer.
//
// SIGHUP re-reads the config and restarts the stream; that is how the sd/hd
// source profile is switched without a service restart.
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"vhfcam-restream/internal/config"
	"vhfcam-restream/internal/overlay"
	"vhfcam-restream/internal/preview"
	"vhfcam-restream/internal/recorder"
	"vhfcam-restream/internal/restream"
)

const componentName = "vhfcam-restream"

// The station dragon is embedded and dropped into the runtime dir at startup
// so the overlay filter can use it as the logo (config overlay.logo points
// there by default).
//
//go:embed dragon.png
var dragonPNG []byte

// installLogo materializes the embedded dragon at the configured logo path
// and clears the logo when that fails — a filter referencing a missing file
// would kill every ffmpeg run. Custom (non-default) paths are user-provided:
// they are kept as-is, but a missing file there also clears the logo.
func installLogo(c *config.Config, log *slog.Logger) {
	if c.Overlay.Logo == "" {
		return
	}
	if filepath.Base(c.Overlay.Logo) == "dragon.png" {
		if err := os.MkdirAll(filepath.Dir(c.Overlay.Logo), 0755); err == nil {
			err = os.WriteFile(c.Overlay.Logo, dragonPNG, 0644)
			if err == nil {
				return
			}
		}
		log.Warn("cannot materialize embedded dragon logo — disabling logo", "path", c.Overlay.Logo)
		c.Overlay.Logo = ""
		return
	}
	if _, err := os.Stat(c.Overlay.Logo); err != nil {
		log.Warn("logo file missing — disabling logo", "path", c.Overlay.Logo)
		c.Overlay.Logo = ""
	}
}

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
	installLogo(&cfg, logger)

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
	// The YouTube sink's runtime gate: the page's start/stop buttons flip it;
	// config youtube_enabled is the boot default.
	ytEnabled := &atomic.Bool{}
	ytEnabled.Store(cfg.YoutubeEnabled)
	// Radio-audio demand holders: the page's connect button and a running
	// recording. The radio audio flows while anyone holds it.
	demand := &preview.AudioDemand{}
	// Recorder: follows the preview's HLS; wired to the demand below.
	rec := recorder.New(func() config.Config { return *curCfg.Load() },
		logger.With("component", componentName, "subcomponent", "recorder")).
		WithFreq(ov.FreqHz)
	pvSrv := preview.NewServer(func() config.PreviewConfig { return curCfg.Load().Preview },
		nil,
		logger.With("component", componentName, "subcomponent", "preview")).
		WithStatus(func() preview.RadioStatus {
			rl := ov.RadioLink()
			st := preview.ComputeStatus(rl.BridgeOnline, rl.DeviceOnline, rl.Responding, audioStatus.Alive())
			st.Youtube = ytEnabled.Load()
			st.AudioHolders = demand.Holders()
			return st
		}).
		WithRecorder(rec)
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
		installLogo(&nc, logger)
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
		WithEnabled(func() bool { return ytEnabled.Load() })
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
	// Capture is OPT-IN — it starts OFF (2026-09): the heartbeat runs only
	// while a holder wants it (the page's connect button, or a recording).
	// The page's disconnect releases only the page's hold, so it never cuts
	// the audio out of a running recording.
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
			demand.Set(preview.HolderPage, true)
		case "audio_off":
			if _, after := demand.Set(preview.HolderPage, false); after {
				logger.Info("page released radio audio; still held", "holders", demand.Holders())
				return nil
			}
		case "yt_start":
			ytEnabled.Store(true)
			ytSup.Reload()
			return nil
		case "yt_stop":
			ytEnabled.Store(false)
			ytSup.Reload()
			return nil
		}
		return radioPublish(action)
	})
	audioDemand := func(on bool) {
		if !curCfg.Load().Preview.Enabled || curCfg.Load().Preview.RadioAudio == "" {
			return
		}
		if on && !demand.On() {
			return // nobody holds the audio — heartbeat stays off
		}
		if err := radioPublish(map[bool]string{true: "audio_on", false: "audio_off"}[on]); err != nil {
			logger.Warn("radio audio demand publish failed", "err", err)
		}
	}
	// A recording holds the radio audio for its whole duration; audio_on
	// goes out at once instead of waiting for the next heartbeat.
	rec.WithOnActive(func(active bool) {
		before, after := demand.Set(preview.HolderRecording, active)
		switch {
		case !before && after:
			audioDemand(true)
		case before && !after:
			audioDemand(false)
		}
	})
	go func() {
		// Let MQTT connect, then clear any stale demand from before the
		// restart (capture is opt-in — it must not outlive a restart).
		time.Sleep(3 * time.Second)
		audioDemand(false)
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

	// Recorder: finalizes leftovers first, then records on demand; on
	// shutdown it finalizes an active recording (bounded) before returning.
	recDone := make(chan struct{})
	go func() { _ = rec.Run(ctx); close(recDone) }()

	// Run both sinks until shutdown; a sink-level failure never takes the
	// other one down (each supervisor loops on its own).
	done := make(chan error, 2)
	go func() { done <- ytSup.Run(ctx) }()
	go func() { done <- pvSup.Run(ctx) }()
	if err := <-done; err != nil {
		logger.Error("supervisor terminated", "err", err)
		os.Exit(1)
	}
	<-recDone // the active recording is saved before the process exits
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
