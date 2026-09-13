// Command spid-ercm-rotator-bridge fronts the station's satellite-tracking
// rotator mount — SPID azimuth (Rot1Prog binary protocol) and GS-500 elevation
// via the ERC-M controller (GS-232B dialect) — as two canonical `rotator`
// slots (muehle/uhf/az-rotator, muehle/uhf/el-rotator) on the station MQTT
// bus, plus a rotctld TCP server (:4534) and a PstRotator UDP listener
// (:12041) fed from the same dispatch core. See README.md and
// ../docs/plans/2026-09-12-001-feat-sat-ops-rotators-plan.md for details.
//
// This is the U1 scaffold: config parsing, startup logging and signal
// handling only. The serial drivers (U2/U3), dispatch core (U4), MQTT slot
// surface (U5) and protocol listeners (U6/U7) wire into run() in later units.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"spid-ercm-rotator-bridge/internal/config"
)

// component is the constant slog component attr and the service name —
// equal to the systemd unit / bridge name (docs/conventions/logging.md §2).
const component = "spid-ercm-rotator-bridge"

func main() {
	fs := flag.NewFlagSet(component, flag.ExitOnError)
	flags := config.RegisterFlags(fs)
	_ = fs.Parse(os.Args[1:])

	cfg, err := config.Load(flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: load config: %v\n", component, err)
		os.Exit(2)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", component, err)
		os.Exit(2)
	}

	logger := newLogger(cfg.Log.Level)
	logger.Info("starting",
		"broker", cfg.MQTT.Broker,
		"rotctld", fmt.Sprintf("%s:%d", cfg.Rotctld.Bind, cfg.Rotctld.Port),
		"pstrotator", fmt.Sprintf("%s:%d", cfg.PstRotator.Bind, cfg.PstRotator.Port))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg, logger); err != nil {
		// A SIGTERM/SIGINT cancels the root context; that's a clean shutdown,
		// not a failure — exit 0 so `systemctl stop` doesn't report FAILURE.
		if errors.Is(err, context.Canceled) {
			logger.Info("stopped")
			return
		}
		logger.Error("exited", "err", err)
		os.Exit(1)
	}
	logger.Info("stopped")
}

// run is the service loop. U1: it logs the configured mount once and waits for
// a shutdown signal. Later units wire in here — the per-axis serial drivers
// with their self-heal loops (U2/U3), the mount dispatch façade (U4), the
// two-slot MQTT surface (U5), and the rotctld/PstRotator listeners (U6/U7).
func run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	for _, s := range cfg.Slots {
		// One child logger per slot (logging convention §2) — each axis's
		// narrative must be separable in the journal.
		log := logger.With("slot", cfg.MQTT.Site+"/"+cfg.MQTT.Station+"/"+s.Slot)
		if s.Mock() {
			log.Info("axis configured", "axis", s.Axis, "mode", "mock")
		} else {
			log.Info("axis configured", "axis", s.Axis, "mode", "serial",
				"port", s.Serial.Port, "baud", s.Serial.Baud)
		}
	}
	logger.Info("scaffold build: no device behavior wired yet; waiting for shutdown signal")
	<-ctx.Done()
	return ctx.Err()
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv})
	// Convention §1 (docs/conventions/logging.md): one constant `component`
	// attr on the root logger; slot-specific context goes through child
	// loggers created per slot.
	return slog.New(h).With("component", component)
}
