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
	"time"

	"ultrabridge/internal/config"
	mqttbridge "ultrabridge/internal/mqtt"
	"ultrabridge/internal/ub/service"
	"ultrabridge/internal/ub/transport"
	"ultrabridge/internal/web"
)

const defaultConfigPath = "/etc/ultrabridge/config.toml"

func main() {
	// Root logger per ../docs/conventions/logging.md: log/slog text handler →
	// stderr, one constant `component` attr, installed as the default so every
	// package-level slog call carries it. The config schema has no [log] level
	// key, so the level stays at the Info default (do not invent a key here —
	// the schema is shared with the deployed 0600 file).
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})).With("component", "ultrabridge")
	slog.SetDefault(logger)

	// Defaults come from the config package so the file, flags, and built-in
	// defaults all agree on a single source of truth.
	def := config.Default()

	configPath := flag.String("config", defaultConfigPath, "Path to the TOML config file")
	httpAddr := flag.String("http", def.HTTPAddr, "HTTP listen address")
	port := flag.String("port", def.SerialPort, "Serial port path (leave empty to use mock)")
	baud := flag.Int("baud", def.Baud, "Serial baud rate")
	mqttBroker := flag.String("mqtt-broker", def.MQTT.Broker, "MQTT broker URL (optional)")
	mqttClientID := flag.String("mqtt-client-id", def.MQTT.ClientID, "MQTT client ID")
	mqttUser := flag.String("mqtt-user", def.MQTT.User, "MQTT username (optional)")
	mqttPassword := flag.String("mqtt-password", def.MQTT.Password, "MQTT password (optional)")
	flag.Parse()

	// Resolve effective config: defaults < config file < explicitly-set flags.
	cfg := loadConfig(*configPath, isFlagSet("config"))
	applyFlagOverrides(&cfg, map[string]string{
		"http": *httpAddr, "port": *port,
		"mqtt-broker": *mqttBroker, "mqtt-client-id": *mqttClientID,
		"mqtt-user": *mqttUser, "mqtt-password": *mqttPassword,
	}, *baud)

	// ultrabridge fronts exactly one slot (muehle/hf/ant-ctrl): stamp it as a
	// child logger so transport/service/mqtt lines all carry `slot` (logging
	// convention §2). The address needs site+station from config; when neither
	// is configured (bare mock runs) fall back to the component-only logger.
	slotLogger := logger
	if cfg.MQTT.Site != "" && cfg.MQTT.Station != "" {
		slotLogger = logger.With("slot", cfg.MQTT.Site+"/"+cfg.MQTT.Station+"/"+cfg.MQTT.Slot)
	}
	transport.SetLogger(slotLogger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	slog.Info("ultrabridge starting",
		"http_addr", cfg.HTTPAddr,
		"serial_port", cfg.SerialPort,
		"baud", cfg.Baud,
		"mqtt_broker", cfg.MQTT.Broker)

	var dev transport.Client
	var err error
	if cfg.SerialPort == "" {
		dev = transport.NewMock()
	} else {
		dev, err = transport.OpenSerial(cfg.SerialPort, cfg.Baud)
		if err != nil {
			slog.Error("serial open failed", "port", cfg.SerialPort, "err", err)
			os.Exit(1)
		}
	}
	// Wrap the device so tx/rx exchanges can be streamed to the UI when the
	// debug toggle is on. The hub is shared with the web server.
	debugHub := web.NewDebugHub()
	dev = transport.NewTracing(dev, debugHub.Enabled, debugHub.Publish)

	ctrl := service.NewController(dev)
	ctrl.SetLogger(slotLogger)
	ui := web.NewWithHub(ctrl, debugHub)
	var mqttClient *mqttbridge.Client
	if cfg.MQTT.Broker != "" {
		mqttClient, err = mqttbridge.New(
			ctx,
			cfg.MQTT.Broker, cfg.MQTT.ClientID,
			cfg.MQTT.Site, cfg.MQTT.Station, cfg.MQTT.Slot,
			cfg.MQTT.DiscoveryPrefix,
			cfg.Location, cfg.Host,
			cfg.MQTT.User, cfg.MQTT.Password,
			cfg.MQTT.PublishHADiscovery,
			ctrl,
		)
		if err != nil {
			// Fatal, deliberately: never run the bridge with its MQTT plane silently
			// dead. An initial-connect failure here (e.g. the broker briefly unreachable
			// in the boot storm) used to log "mqtt disabled" and keep running serial+web
			// only for the life of the process — ant-ctrl showed offline for ~18 h on
			// 2026-09-03 with no restart to rescue it. Exiting non-zero lets systemd's
			// Restart=on-failure / RestartSec=5 crash-loop the unit until the broker
			// answers (the same shape shelly-power-bridge/powerseq/antennaselect use).
			slog.Error("mqtt connect failed", "broker", cfg.MQTT.Broker, "err", err)
			os.Exit(1)
		}
		// Embedded HA discovery is gated off by default (model §9); hadiscovery
		// renders discovery from /meta.expose instead.
		mqttClient.PublishDiscoveryIfEnabled()
		mqttClient.BindCommands(ctx)
	}

	pollCtx, cancelPoll := context.WithCancel(ctx)
	defer cancelPoll()
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pollCtx.Done():
				return
			case <-ticker.C:
				_ = ctrl.Refresh(pollCtx)
				_ = ctrl.PollMotorStatus(pollCtx)
				ui.PublishStatus(ctrl.State())
				if mqttClient != nil {
					mqttClient.PublishState(ctrl.State())
				}
			}
		}
	}()

	if err := ctrl.Refresh(ctx); err != nil {
		// Warn, not Error: the bridge stays up and the 2 s poll loop keeps
		// retrying — degraded but recovering (logging convention §3).
		slog.Warn("initial refresh failed", "err", err)
	}
	ui.PublishStatus(ctrl.State())
	if mqttClient != nil {
		mqttClient.PublishState(ctrl.State())
	}

	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: ui.Routes(),
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if mqttClient != nil {
			mqttClient.Close()
		}
	}()

	slog.Info("http server listening", "addr", cfg.HTTPAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("http server failed", "addr", cfg.HTTPAddr, "err", err)
		os.Exit(1)
	}
}

// loadConfig returns the file-backed config, or defaults when the default
// config path is simply absent. A missing file is fatal only when -config was
// set explicitly (the operator asked for a file that must exist); a malformed
// file is always fatal.
func loadConfig(path string, explicit bool) config.Config {
	cfg, err := config.Load(path)
	if err == nil {
		return cfg
	}
	if errors.Is(err, fs.ErrNotExist) && !explicit {
		// Default path with no file present: run on defaults + flags.
		return config.Default()
	}
	slog.Error("config load failed", "path", path, "err", err)
	os.Exit(2) // config errors exit 2, connect/run errors exit 1 (logging convention §3)
	return config.Default() // unreachable
}

// applyFlagOverrides overlays onto cfg only the flags the user explicitly set,
// so that flag > file > default precedence holds deterministically (a flag set
// to its default value still wins over a file value).
func applyFlagOverrides(cfg *config.Config, strs map[string]string, baud int) {
	if isFlagSet("http") {
		cfg.HTTPAddr = strs["http"]
	}
	if isFlagSet("port") {
		cfg.SerialPort = strs["port"]
	}
	if isFlagSet("baud") {
		cfg.Baud = baud
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
	if isFlagSet("mqtt-password") {
		cfg.MQTT.Password = strs["mqtt-password"]
	}
}

// isFlagSet reports whether the named flag was explicitly provided on the
// command line (as opposed to carrying its default value).
func isFlagSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}
