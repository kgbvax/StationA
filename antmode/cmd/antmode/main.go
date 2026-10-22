// SPDX-License-Identifier: AGPL-3.0-or-later

// Command antmode is the station antenna-mode logic slot (muehle/hf/ant-mode).
// It owns the mode — normal, storm or maintenance — and publishes a retained
// lock that wrc-rotator-bridge and ultrabridge enforce (they park and retract
// themselves) and antennaselect, beamsteer and powerseq stand down for. Storm
// is entered by the operator, or automatically on an active DWD wind warning
// (BBK NINA) while the station is unmanned; the park heading comes from the
// DWD day wind forecast. See docs/ant-mode-mqtt-api.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"codeberg.org/kgbvax/stationa/shared/logging"

	"antmode/internal/config"
	"antmode/internal/dwd"
	"antmode/internal/engine"
	"antmode/internal/mqtt"
	"antmode/internal/nina"
	"antmode/internal/poll"
	"antmode/internal/store"
)

// userAgent identifies the public-API polls (NINA, DWD).
const userAgent = "stationa-antmode/1 (+https://codeberg.org/kgbvax/stationa)"

func main() {
	// Logging convention (docs/conventions/logging.md): slog text on stderr
	// with a constant component attr.
	logger := slog.New(logging.NewHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})).
		With("component", "antmode")
	slog.SetDefault(logger)

	def := config.Default()
	configPath := flag.String("config", "/etc/antmode/config.toml", "path to config TOML")
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

	slotLog := logger.With("slot", cfg.MQTT.Site+"/"+cfg.MQTT.Station+"/"+cfg.MQTT.Slot)
	st := store.File{Path: cfg.State.Path}
	loaded := loadState(st, slotLog)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := mqtt.New(ctx, cfg, st, loaded, slotLog)
	startPollers(ctx, cfg, client, slotLog)

	if err := client.Connect(ctx); err != nil {
		if ctx.Err() != nil {
			return // shut down while connecting
		}
		slog.Error("mqtt connect", "err", err)
		os.Exit(1)
	}
	defer client.Close()

	slotLog.Info("running", "auto_enter", cfg.Storm.AutoEnter, "power_borrow", cfg.Power.Borrow,
		"nina", cfg.NINA.ARS != "", "wind", cfg.Wind.DWDStation != "")
	<-ctx.Done()
	slog.Info("shutting down")
}

// loadState reads the state file. Missing → nil (the engine adopts a lock
// still retained on the broker); unreadable or corrupt → nil too, logged as
// an Error: the retained /state is the better source than a guess.
func loadState(st store.File, log *slog.Logger) *engine.Persisted {
	var p engine.Persisted
	err := st.Load(&p)
	switch {
	case err == nil:
		return &p
	case errors.Is(err, fs.ErrNotExist):
		log.Info("no state file; will adopt the retained /state if it is locked", "path", st.Path)
	default:
		log.Error("state file unreadable; will adopt the retained /state if it is locked", "path", st.Path, "err", err)
	}
	return nil
}

// startPollers runs the NINA and DWD polls; results reach the engine through
// the jobs queue.
func startPollers(ctx context.Context, cfg config.Config, client *mqtt.Client, log *slog.Logger) {
	hc := &http.Client{} // per-attempt deadlines come from the poller's context
	if cfg.NINA.ARS == "" {
		log.Warn("nina.ars is empty: no warning poll (warnings unknown, no storm suggestion or auto entry)")
	} else {
		codes := map[string]bool{}
		for _, c := range cfg.NINA.IICodes {
			codes[c] = true
		}
		nc := &nina.Client{BaseURL: cfg.NINA.BaseURL, UserAgent: userAgent, HTTP: hc, Codes: codes}
		go poll.Poller{
			Name: "nina", Log: log,
			Interval:   time.Duration(cfg.NINA.PollS) * time.Second,
			MinBackoff: 30 * time.Second,
			Timeout:    time.Duration(cfg.NINA.TimeoutS) * time.Second,
			Fn: func(actx context.Context) error {
				ws, err := nc.Fetch(actx, cfg.NINA.ARS)
				client.Do(func(e *engine.Engine) { e.NinaResult(ws, err) })
				if err == nil {
					log.Debug("nina poll", "qualifying", len(ws))
				}
				return err
			},
		}.Run(ctx)
	}
	if cfg.Wind.DWDStation == "" {
		log.Warn("wind.dwd_station is empty: no wind poll (storm parks at storm.fallback_az)")
		return
	}
	dc := &dwd.Client{BaseURL: cfg.Wind.BaseURL, UserAgent: userAgent, HTTP: hc}
	go poll.Poller{
		Name: "dwd wind", Log: log,
		Interval:   time.Duration(cfg.Wind.PollS) * time.Second,
		MinBackoff: 60 * time.Second,
		Timeout:    time.Duration(cfg.Wind.TimeoutS) * time.Second,
		Fn: func(actx context.Context) error {
			w, err := dc.Fetch(actx, cfg.Wind.DWDStation, time.Now())
			if err != nil {
				return fmt.Errorf("station %s: %w", cfg.Wind.DWDStation, err)
			}
			client.Do(func(e *engine.Engine) { e.WindResult(w, nil) })
			log.Debug("wind poll", "dir_deg", w.DirDeg, "gust_kmh", w.GustKmh, "day", w.Day)
			return nil
		},
	}.Run(ctx)
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
