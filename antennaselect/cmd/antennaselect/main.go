// Command antennaselect runs the HF antenna-selection reconciler. It reads state from the
// station bus (radio, station activity, ant-switch, operator) and drives the ant-switch
// (and the controller band-follow) via the priority ladder. See docs/antenna-select-mqtt-api.md.
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

	"antennaselect/internal/config"
	"antennaselect/internal/mqtt"
	"antennaselect/internal/reconcile"
)

func main() {
	// Logging convention (docs/conventions/logging.md): one root slog text logger on
	// stderr with a constant component attr, installed as the default so internal
	// packages log through it.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})).
		With("component", "antennaselect")
	slog.SetDefault(logger)

	def := config.Default()
	configPath := flag.String("config", "/etc/antenna-select/config.toml", "path to config TOML")
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

	rec := reconcile.New(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client, err := mqtt.New(ctx, cfg, rec)
	if err != nil {
		slog.Error("mqtt connect", "err", err)
		os.Exit(1)
	}
	defer client.Close()

	slog.Info("running", "slot", cfg.MQTT.Site+"/"+cfg.MQTT.Station+"/"+cfg.MQTT.Slot)
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
		return config.Default()
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
