// Command spid-ercm-rotator-bridge fronts the station's satellite-tracking
// rotator mount — SPID azimuth (Rot1Prog binary protocol) and GS-500 elevation
// via the ERC-M controller (GS-232B dialect) — as two canonical `rotator`
// slots (muehle/uhf/az-rotator, muehle/uhf/el-rotator) on the station MQTT
// bus, plus a rotctld TCP server (:4534) and a PstRotator UDP listener
// (:12041) fed from the same dispatch core. See the project CLAUDE.md and
// ../docs/plans/2026-09-12-001-feat-sat-ops-rotators-plan.md for details.
//
// Wiring (U5): both serial drivers (U2/U3) + the mount dispatch façade (U4)
// + the two-slot MQTT surface (internal/mqttslot). The rotctld/PstRotator
// listeners (U6/U7) attach at the marked seam in run() below.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"spid-ercm-rotator-bridge/internal/config"
	"spid-ercm-rotator-bridge/internal/ercm"
	"spid-ercm-rotator-bridge/internal/gs232"
	"spid-ercm-rotator-bridge/internal/mount"
	"spid-ercm-rotator-bridge/internal/mqttslot"
	"spid-ercm-rotator-bridge/internal/pstrotator"
	"spid-ercm-rotator-bridge/internal/rotctld"
	"spid-ercm-rotator-bridge/internal/spid"
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

// run is the service loop (U5 wiring):
//
//	serial drivers (spid / ercm)  →  mount façade (mount)  →  two-slot MQTT
//	surface (mqttslot), plus the driver poll loops and the per-slot /state
//	poll ticks. Every control path — the /cmd plane and, once U6/U7 land, the
//	rotctld TCP and PstRotator UDP listeners — funnels through the same
//
// façade, so all motion surfaces identically in both slots' /state (R4).
//
// An empty configured serial port selects the in-process mock device per axis
// (KTD7), so the whole stack runs bench- and CI-side without hardware.
func run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	azSlotCfg, err := cfg.Slot(config.AxisAZ)
	if err != nil {
		return err
	}
	elSlotCfg, err := cfg.Slot(config.AxisEL)
	if err != nil {
		return err
	}
	azLog := logger.With("slot", cfg.MQTT.Site+"/"+cfg.MQTT.Station+"/"+azSlotCfg.Slot)
	elLog := logger.With("slot", cfg.MQTT.Site+"/"+cfg.MQTT.Station+"/"+elSlotCfg.Slot)
	for _, s := range []struct {
		cfg config.SlotConfig
		log *slog.Logger
	}{{azSlotCfg, azLog}, {elSlotCfg, elLog}} {
		if s.cfg.Mock() {
			s.log.Info("axis configured", "axis", s.cfg.Axis, "mode", "mock")
		} else {
			s.log.Info("axis configured", "axis", s.cfg.Axis, "mode", "serial",
				"port", s.cfg.Serial.Port, "baud", s.cfg.Serial.Baud)
		}
	}

	// --- per-axis drivers (U2 SPID Rot1Prog az, U3 ERC-M GS-232B el) --------
	// Both satisfy mount.Controller as written (KTD12's narrow interface).
	// An empty serial port selects the in-process mock inside the driver (KTD7).
	azAxis, err := spid.New(azSlotCfg, cfg.Control, azLog)
	if err != nil {
		return err
	}
	elDrv := ercm.New(ercm.Config{
		Port:           elSlotCfg.Serial.Port,
		Baud:           elSlotCfg.Serial.Baud,
		PollInterval:   cfg.Control.PollIntervalDur,
		ReopenCooldown: cfg.Control.ReopenCooldownDur,
	}, elLog)

	// --- mount dispatch façade (U4) -----------------------------------------
	// The ONLY cross-axis semantics owner (atomic stop, refusal aggregation,
	// park): the MQTT /cmd path and the U6/U7 servers all consume it.
	m := mount.New(azAxis, elDrv, cfg.Control, logger)

	// Driver self-heal poll loops (KTD7): each driver owns its goroutine and
	// retries its by-id reopen indefinitely — a dead serial link degrades only
	// its own slot's device_online, never the process.
	go azAxis.RunPoll(ctx)
	go elDrv.Run(ctx)
	go m.Run(ctx) // per-axis dispatch workers (latest-wins, bounded stop epoch)

	// --- two-slot MQTT surface (U5, KTD2: one paho client per slot) ----------
	// Initial connect failure is fatal by design (§8.1 item 10): return the
	// error so main exits non-zero and systemd crash-loops the unit.
	azLimits, err := cfg.Control.Axis(config.AxisAZ)
	if err != nil {
		return err
	}
	elLimits, err := cfg.Control.Axis(config.AxisEL)
	if err != nil {
		return err
	}
	azSlot, err := mqttslot.New(ctx, mqttslot.Options{
		Broker:      cfg.MQTT.Broker,
		ClientID:    cfg.MQTT.ClientID,
		User:        cfg.MQTT.User,
		Password:    cfg.MQTT.Password,
		Site:        cfg.MQTT.Site,
		Station:     cfg.MQTT.Station,
		Slot:        azSlotCfg.Slot,
		Axis:        config.AxisAZ,
		Location:    cfg.MQTT.Location,
		Host:        cfg.Host,
		DeviceModel: azSlotCfg.DeviceModel,
		DeviceLink:  azSlotCfg.DeviceLink,
		// SPID's Rot1Prog has no firmware string to report — no callback, the
		// /meta firmware key is omitted.
		Limits:       azLimits,
		Mount:        m,
		Err:          azAxis.Err, // self-clears on link recovery inside the SPID driver
		PollInterval: cfg.Control.PollIntervalDur,
	}, azLog)
	if err != nil {
		return fmt.Errorf("az slot mqtt: %w", err)
	}
	elSlot, err := mqttslot.New(ctx, mqttslot.Options{
		Broker:      cfg.MQTT.Broker,
		ClientID:    cfg.MQTT.ClientID,
		User:        cfg.MQTT.User,
		Password:    cfg.MQTT.Password,
		Site:        cfg.MQTT.Site,
		Station:     cfg.MQTT.Station,
		Slot:        elSlotCfg.Slot,
		Axis:        config.AxisEL,
		Location:    cfg.MQTT.Location,
		Host:        cfg.Host,
		DeviceModel: elSlotCfg.DeviceModel,
		DeviceLink:  elSlotCfg.DeviceLink,
		// The ERC-M's rFMW string, read once per (re)open, folds into
		// /meta.device.firmware once it lands.
		Firmware: elDrv.Firmware,
		Limits:   elLimits,
		Mount:    m,
		// The ERC-M's LastError deliberately does NOT self-clear on recovery,
		// so gate it on the link state: /state.error reports live faults only.
		Err: func() string {
			if elDrv.Online() {
				return ""
			}
			return elDrv.LastError()
		},
		PollInterval: cfg.Control.PollIntervalDur,
	}, elLog)
	if err != nil {
		azSlot.Close() // do not leave the first slot's retained online dangling
		return fmt.Errorf("el slot mqtt: %w", err)
	}

	// Per-slot /state poll ticks (KTD14 cadence; R4: protocol-driven motion
	// surfaces here because the tick reads the façade every poll).
	go azSlot.Run()
	go elSlot.Run()

	// --- protocol servers (U6 rotctld :4534, U7 PstRotator :12041/UDP,
	// gs232 :4533/TCP — the PstRotator/N1MM legacy integration path) --------
	// All consume ONLY the mount façade m — the same pipeline /cmd feeds —
	// so a server-driven move surfaces in /state exactly like a bus-driven
	// one (R4). A failed listener is fatal for a headless bridge: exit
	// non-zero and let systemd restart (unlike pelcobridge2's TUI, which
	// must survive a lost listener).
	errCh := make(chan error, 3)
	rcSrv := rotctld.New(m,
		component+" · "+azSlotCfg.DeviceModel+"/"+elSlotCfg.DeviceModel,
		rotctld.LimitsFromControl(cfg.Control), logger)
	go func() {
		if err := rcSrv.ListenAndServe(ctx,
			net.JoinHostPort(cfg.Rotctld.Bind, strconv.Itoa(cfg.Rotctld.Port))); err != nil && ctx.Err() == nil {
			errCh <- fmt.Errorf("rotctld listen: %w", err)
		}
	}()
	pstSrv := pstrotator.New(cfg.PstRotator, m, logger)
	go func() {
		if err := pstSrv.Run(ctx); err != nil && ctx.Err() == nil {
			errCh <- fmt.Errorf("pstrotator listen: %w", err)
		}
	}()
	if cfg.GS232.Enabled {
		gsSrv := gs232.New(cfg.GS232, m, logger)
		go func() {
			if err := gsSrv.Run(ctx); err != nil && ctx.Err() == nil {
				errCh <- fmt.Errorf("gs232 listen: %w", err)
			}
		}()
	}

	select {
	case err := <-errCh:
		azSlot.Close() // do not leave retained online dangling after the fatal exit
		elSlot.Close()
		return err
	case <-ctx.Done():
	}

	// Clean shutdown (powerseq pattern): the LWT does not fire on a clean
	// exit, so both slots self-publish their retained offline /status before
	// disconnecting — no stale-online gap after `systemctl stop`.
	azSlot.Close()
	elSlot.Close()
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
