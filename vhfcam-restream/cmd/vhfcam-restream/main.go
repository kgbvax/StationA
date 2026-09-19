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
	"sync/atomic"
	"syscall"

	"vhfcam-restream/internal/config"
	"vhfcam-restream/internal/overlay"
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

	sup := restream.New(cfg, func() (config.Config, error) {
		nc, err := config.Load(*configPath)
		if err != nil {
			return nc, err
		}
		nc.ApplyEnv()
		curCfg.Store(&nc)
		return nc, nil
	}, logger.With("component", componentName))

	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		for range sighup {
			logger.Info("SIGHUP — reloading config and restarting the stream")
			sup.Reload()
		}
	}()

	if err := sup.Run(ctx); err != nil {
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
