// Command testui is a workstation-side MQTT relay + schema-aware test UI for the
// stationa station bus. It connects to the broker as a passive consumer (subscribes
// <site>/#), holds the live slot tree in memory, serves a static browser UI, and
// proxies browser publish/clear requests back onto the bus with safety guards
// (site-prefix guard; /cmd is never retained — integration model §8).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"testui/internal/config"
	"testui/internal/mqtt"
	"testui/internal/web"
)

const defaultConfigPath = "config.toml"

func main() {
	def := config.Default()

	configPath := flag.String("config", defaultConfigPath, "Path to the TOML config file")
	httpAddr := flag.String("http", def.HTTPAddr, "HTTP listen address")
	site := flag.String("site", def.Site, "MQTT site prefix to subscribe and guard (<site>/#)")
	mqttBroker := flag.String("mqtt-broker", def.MQTT.Broker, "MQTT broker URL")
	mqttClientID := flag.String("mqtt-client-id", def.MQTT.ClientID, "MQTT client ID")
	mqttUser := flag.String("mqtt-user", def.MQTT.User, "MQTT username")
	flag.Parse()

	// Resolve effective config: defaults < config file < explicitly-set flags.
	cfg := loadConfig(*configPath, isFlagSet("config"))
	applyFlagOverrides(&cfg, map[string]string{
		"http":          *httpAddr,
		"site":          *site,
		"mqtt-broker":   *mqttBroker,
		"mqtt-client-id": *mqttClientID,
		"mqtt-user":     *mqttUser,
	})

	if cfg.MQTT.Broker == "" {
		fmt.Fprintln(os.Stderr, "no mqtt broker configured (set [mqtt].broker in config)")
		os.Exit(1)
	}
	// Normalize the site: strip leading/trailing slashes and reject empty. The publish
	// guard builds site+"/" as a prefix, so an empty or malformed site would silently
	// degrade it (empty -> prefix "/", fail-closed for legit topics but fail-open for
	// "/"-prefixed ones; trailing slash -> "site//" rejects everything).
	cfg.Site = strings.Trim(cfg.Site, "/")
	cfg.MQTT.Site = cfg.Site
	if cfg.Site == "" {
		fmt.Fprintln(os.Stderr, "site must be non-empty (set -site or [site] in config)")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	tree := web.NewTree()

	mqttClient, err := mqtt.New(
		cfg.MQTT.Broker, cfg.MQTT.ClientID,
		cfg.Site, cfg.MQTT.User, cfg.MQTT.Password,
		tree,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mqtt connect failed: %v\n", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: web.New(tree, mqttClient, cfg.Site).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		fmt.Println("\nshutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		mqttClient.Close()
	}()

	fmt.Printf("testui listening on http://%s  (broker %s, sub %s/#)\n",
		cfg.HTTPAddr, cfg.MQTT.Broker, cfg.Site)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// loadConfig returns the file-backed config, or defaults when the default config path
// is simply absent. A missing file is fatal only when -config was set explicitly; a
// malformed file is always fatal.
func loadConfig(path string, explicit bool) config.Config {
	cfg, err := config.Load(path)
	if err == nil {
		return cfg
	}
	if errors.Is(err, fs.ErrNotExist) && !explicit {
		return config.Default()
	}
	fmt.Fprintf(os.Stderr, "config: %v\n", err)
	os.Exit(1)
	return config.Default()
}

// applyFlagOverrides overlays onto cfg only the flags the user explicitly set, so
// flag > file > default precedence holds deterministically.
func applyFlagOverrides(cfg *config.Config, strs map[string]string) {
	if isFlagSet("http") {
		cfg.HTTPAddr = strs["http"]
	}
	if isFlagSet("site") {
		cfg.Site = strs["site"]
		// Keep the subscribe prefix and the MQTT site in sync when overridden via flag.
		cfg.MQTT.Site = strs["site"]
	}
	if isFlagSet("mqtt-broker") {
		cfg.MQTT.Broker = strs["mqtt-broker"]
	}
	if isFlagSet("mqtt-client-id") {
		cfg.MQTT.ClientID = strs["mqtt-client-id"]
	}
	if isFlagSet("mqtt-user") {
		cfg.MQTT.User = strs["mqtt-user"]
	}
}

// isFlagSet reports whether the named flag was explicitly provided.
func isFlagSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}