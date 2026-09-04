// Command hadiscovery is the central Home Assistant discovery consumer for the station
// bus. It subscribes to slot /meta announcements, reads each slot's consumer-neutral
// `expose` block, and renders HA MQTT discovery. It owns no device and writes no /cmd;
// it only reads /meta and publishes under the HA discovery tree. See
// docs/discovery-mqtt-api.md and ../docs/station-integration-model.md §3.1 / §9.
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

	"hadiscovery/internal/config"
	"hadiscovery/internal/engine"
	"hadiscovery/internal/mqtt"
)

func main() {
	// Logging convention (docs/conventions/logging.md): one root slog text logger on
	// stderr with a constant component attr, installed as the default so internal
	// packages log through it.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})).
		With("component", "hadiscovery")
	slog.SetDefault(logger)

	def := config.Default()
	configPath := flag.String("config", "/etc/hadiscovery/config.toml", "path to config TOML")
	broker := flag.String("broker", def.MQTT.Broker, "MQTT broker URL (overrides config)")
	flag.Parse()

	cfg := loadConfig(*configPath)
	if isFlagSet("broker") {
		cfg.MQTT.Broker = *broker
	}
	cfg.MQTT.DiscoveryPrefix = orDefault(cfg.MQTT.DiscoveryPrefix, "homeassistant")

	if err := cfg.Validate(); err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(2)
	}
	if cfg.MQTT.Broker == "" {
		slog.Error("no MQTT broker configured (set [mqtt].broker or -broker)")
		os.Exit(2)
	}

	eng := engine.NewEngine(cfg.MQTT.DiscoveryPrefix, cfg.Area)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client, err := mqtt.New(ctx, cfg, eng)
	if err != nil {
		slog.Error("mqtt connect", "err", err)
		os.Exit(1)
	}
	defer client.Close()

	slog.Info("running",
		"slot", cfg.MQTT.Site+"/"+cfg.MQTT.Station+"/"+cfg.MQTT.Slot,
		"filter", cfg.MQTT.MetaFilter, "prefix", cfg.MQTT.DiscoveryPrefix, "area", cfg.Area)
	<-ctx.Done()
	slog.Info("shutting down")
}

// loadConfig applies the config-and-secrets convention: a missing DEFAULT-path file is
// tolerable (run on defaults + flags); an explicitly-requested file that is missing or
// malformed is fatal.
func loadConfig(path string) config.Config {
	cfg, err := config.Load(path)
	if err == nil {
		return cfg
	}
	if errors.Is(err, fs.ErrNotExist) && !isFlagSet("config") {
		slog.Info("no config at default path; using defaults + flags", "path", path)
		cfg := config.Default()
		cfg.ApplyDerivedDefaults()
		return cfg
	}
	slog.Error("load config", "path", path, "err", err)
	os.Exit(2)
	return config.Config{} // unreachable
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

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
