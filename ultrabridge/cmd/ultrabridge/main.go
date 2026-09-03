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
	"time"

	"ultrabridge/internal/config"
	mqttbridge "ultrabridge/internal/mqtt"
	"ultrabridge/internal/ub/service"
	"ultrabridge/internal/ub/transport"
	"ultrabridge/internal/web"
)

const defaultConfigPath = "/etc/ultrabridge/config.toml"

func main() {
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var dev transport.Client
	var err error
	if cfg.SerialPort == "" {
		dev = transport.NewMock()
	} else {
		dev, err = transport.OpenSerial(cfg.SerialPort, cfg.Baud)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	// Wrap the device so tx/rx exchanges can be streamed to the UI when the
	// debug toggle is on. The hub is shared with the web server.
	debugHub := web.NewDebugHub()
	dev = transport.NewTracing(dev, debugHub.Enabled, debugHub.Publish)

	ctrl := service.NewController(dev)
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
			fmt.Fprintf(os.Stderr, "mqtt connect failed: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "initial refresh failed: %v\n", err)
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

	fmt.Printf("ultrabridge listening on http://%s\n", cfg.HTTPAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, err)
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
	fmt.Fprintf(os.Stderr, "config: %v\n", err)
	os.Exit(1)
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
