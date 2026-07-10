// Command antennaselect runs the HF antenna-selection reconciler. It reads state from the
// station bus (radio, station activity, ant-switch, operator) and drives the ant-switch
// (and the controller band-follow) via the priority ladder. See docs/antenna-select-mqtt-api.md.
package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"log"
	"os/signal"
	"syscall"

	"antennaselect/internal/config"
	"antennaselect/internal/mqtt"
	"antennaselect/internal/reconcile"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("antenna-select ")

	def := config.Default()
	configPath := flag.String("config", "/etc/antenna-select/config.toml", "path to config TOML")
	broker := flag.String("broker", def.MQTT.Broker, "MQTT broker URL (overrides config)")
	flag.Parse()

	cfg := loadConfig(*configPath)
	if isFlagSet("broker") {
		cfg.MQTT.Broker = *broker
	}

	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}
	if cfg.MQTT.Broker == "" {
		log.Fatal("no MQTT broker configured (set [mqtt].broker or -broker)")
	}

	rec := reconcile.New(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client, err := mqtt.New(cfg, rec)
	if err != nil {
		log.Fatalf("mqtt connect: %v", err)
	}
	defer client.Close()

	log.Printf("running; slot=%s/%s/%s", cfg.MQTT.Site, cfg.MQTT.Station, cfg.MQTT.Slot)
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
		return config.Default()
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
