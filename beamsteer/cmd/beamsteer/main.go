// SPDX-License-Identifier: AGPL-3.0-or-later

// Command beamsteer is the HF smart-rotation logic slot (muehle/hf/beam-steer).
// It emulates PstRotator's UDP interface for the contest logger; each rotate
// request is turned into the cheapest way to put an Ultrabeam lobe on the
// station — a 180° direction flip when the station is behind, otherwise a
// rotation to the lobe that needs the least travel. With smart rotation off
// it passes requests straight to the rotator. See docs/beam-steer-mqtt-api.md.
package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"codeberg.org/kgbvax/stationa/shared/pstrotator"

	"beamsteer/internal/config"
	"beamsteer/internal/mqtt"
)

func main() {
	// Logging convention (docs/conventions/logging.md): slog text on stderr
	// with a constant component attr.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})).
		With("component", "beamsteer")
	slog.SetDefault(logger)

	def := config.Default()
	configPath := flag.String("config", "/etc/beamsteer/config.toml", "path to config TOML")
	broker := flag.String("broker", def.MQTT.Broker, "MQTT broker URL (overrides config)")
	flag.Parse()

	cfg := loadConfig(*configPath)
	if isFlagSet("broker") {
		cfg.MQTT.Broker = *broker
	}
	if err := cfg.Validate(); err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(2)
	}
	if cfg.MQTT.Broker == "" {
		slog.Error("no MQTT broker configured (set [mqtt].broker or -broker)")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slotLog := logger.With("slot", cfg.MQTT.Site+"/"+cfg.MQTT.Station+"/"+cfg.MQTT.Slot)
	client, err := mqtt.New(ctx, cfg, slotLog)
	if err != nil {
		slog.Error("mqtt connect", "err", err)
		os.Exit(1)
	}
	defer client.Close()

	srv := &pstrotator.Server{
		Bind:  cfg.PstRotator.Bind,
		Port:  cfg.PstRotator.Port,
		H:     client.PstRotatorHandler(),
		Reply: pstrotator.ReplyFormat(cfg.PstRotator.Reply),
		Log:   slotLog,
	}
	udpErr := make(chan error, 1)
	go func() { udpErr <- srv.Run(ctx) }()

	slotLog.Info("running", "pstrotator_port", cfg.PstRotator.Port)
	select {
	case <-ctx.Done():
		slog.Info("shutting down")
	case err := <-udpErr:
		if err != nil {
			slog.Error("pstrotator listener", "err", err)
			client.Close()
			os.Exit(1)
		}
	}
}

// loadConfig: a missing DEFAULT-path file runs on defaults + flags; an
// explicitly requested file that is missing or malformed is fatal.
func loadConfig(path string) config.Config {
	cfg, err := config.Load(path)
	if err == nil {
		return cfg
	}
	if errors.Is(err, fs.ErrNotExist) && !isFlagSet("config") {
		slog.Info("no config at default path; using defaults + flags", "path", path)
		return config.Default()
	}
	slog.Error("load config", "path", path, "err", err)
	os.Exit(2)
	return config.Config{}
}

func isFlagSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}
