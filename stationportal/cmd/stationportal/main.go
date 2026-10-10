// Command stationportal serves the station landing page: links to every key
// service (probed server-side), the live slot/hardware inventory from the bus
// (/meta, /status, /state.device_online) and the static software, host and
// passive-resource inventory. It is a passive bus consumer — no slot, no LWT,
// no publishes.
package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"stationportal/internal/bus"
	"stationportal/internal/config"
	"stationportal/internal/inventory"
	"stationportal/internal/probe"
	"stationportal/internal/web"

	"codeberg.org/kgbvax/stationa/shared/logging"
)

func main() {
	configPath := flag.String("config", "/etc/stationportal/config.toml", "Path to the TOML config file")
	httpAddr := flag.String("http", "", "HTTP listen address (overrides the config)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Error("load config", "path", *configPath, "err", err)
		os.Exit(2)
	}
	if *httpAddr != "" {
		cfg.HTTPAddr = *httpAddr
	}

	// Logging convention (docs/conventions/logging.md): slog text on stderr,
	// constant component attr.
	level := slog.LevelInfo
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	log := slog.New(logging.NewHandler(os.Stderr, &slog.HandlerOptions{Level: level})).
		With("component", "stationportal")
	slog.SetDefault(log)
	if err != nil {
		log.Info("no config file — using defaults", "path", *configPath)
	}

	inv, err := inventory.Load(cfg.Inventory)
	if err != nil {
		log.Error("load inventory", "path", cfg.Inventory, "err", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	site := strings.Trim(cfg.Site, "/")
	store := bus.NewStore(site)
	go bus.Run(ctx, bus.Options{
		Broker: cfg.MQTT.Broker, ClientID: cfg.MQTT.ClientID,
		User: cfg.MQTT.User, Password: cfg.MQTT.Password, Site: site,
	}, store, log.With("subcomponent", "bus"))

	prober := probe.New(inv.Links, cfg.ProbeTimeout())
	go prober.Run(ctx, cfg.ProbeInterval())

	host, _ := os.Hostname()
	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: (&web.Server{
			Inv: inv, Store: store, Probes: prober,
			Broker: cfg.MQTT.Broker, Host: host, Revision: revision(),
			RefreshS: int(cfg.ProbeInterval().Seconds()), Log: log,
		}).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info("stationportal listening", "http", cfg.HTTPAddr, "broker", cfg.MQTT.Broker,
		"links", len(inv.Links), "slots", len(inv.Slots), "software", len(inv.Software))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("http server failed", "err", err)
		os.Exit(1)
	}
	log.Info("stationportal stopped")
}

// revision returns the VCS revision stamped into the binary ("+dirty" when
// built from a modified tree), or "" when unavailable.
func revision() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev, dirty string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "+dirty"
			}
		}
	}
	if rev == "" {
		return ""
	}
	return rev + dirty
}
