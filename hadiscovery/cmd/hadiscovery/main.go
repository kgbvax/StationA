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
	"log"
	"os/signal"
	"syscall"

	"hadiscovery/internal/config"
	"hadiscovery/internal/engine"
	"hadiscovery/internal/mqtt"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("hadiscovery ")

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
		log.Fatalf("invalid configuration: %v", err)
	}
	if cfg.MQTT.Broker == "" {
		log.Fatal("no MQTT broker configured (set [mqtt].broker or -broker)")
	}

	eng := engine.NewEngine(cfg.MQTT.DiscoveryPrefix, cfg.Area)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client, err := mqtt.New(cfg, eng)
	if err != nil {
		log.Fatalf("mqtt connect: %v", err)
	}
	defer client.Close()

	log.Printf("running; slot=%s/%s/%s filter=%s prefix=%s area=%s",
		cfg.MQTT.Site, cfg.MQTT.Station, cfg.MQTT.Slot, cfg.MQTT.MetaFilter, cfg.MQTT.DiscoveryPrefix, cfg.Area)
	<-ctx.Done()
	log.Print("shutting down")
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
		log.Printf("no config at default path %s; using defaults + flags", path)
		cfg := config.Default()
		cfg.ApplyDerivedDefaults()
		return cfg
	}
	log.Fatalf("load config %s: %v", path, err)
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
