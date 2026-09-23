// Command icom9700-radio-bridge fronts the Icom IC-9700 as the canonical
// `radio` slot muehle/uhf/radio on the station bus. Receive-only posture
// (2026-09 pivot): the RS-BA1 LAN session carries the demand-driven
// receive-audio stream (:50003) — no LAN CI-V commands exist anymore. The
// radio's single LAN session is shared with manual wfview use by
// connecting only on demand (KTD-2). Telemetry is read over the dedicated
// serial CI-V port (internal/civserial) and broadcast on change.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"icom9700-radio-bridge/internal/bridge"
	"icom9700-radio-bridge/internal/civserial"
	"icom9700-radio-bridge/internal/config"
	"icom9700-radio-bridge/internal/radio"
)

func main() {
	fs := flag.NewFlagSet("icom9700-radio-bridge", flag.ExitOnError)
	flags := config.RegisterFlags(fs)
	_ = fs.Parse(os.Args[1:])

	cfg, err := config.Load(flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "icom9700-radio-bridge: load config: %v\n", err)
		os.Exit(2) // config errors exit 2, connect/run errors exit 1 (logging convention §3)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "icom9700-radio-bridge: invalid config: %v\n", err)
		os.Exit(2)
	}

	logger := newLogger(cfg.Log.Level).With("component", "icom9700-radio-bridge")
	slog.SetDefault(logger)
	// One slot (muehle/uhf/radio): stamp it as a child logger so every line
	// carries `slot` (logging convention §2).
	log := logger.With("slot", cfg.MQTT.Site+"/"+cfg.MQTT.Station+"/"+cfg.MQTT.Slot)
	log.Info("icom9700-radio-bridge starting",
		"radio_host", cfg.RadioHost, "broker", cfg.MQTT.Broker)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg, log); err != nil {
		log.Error("icom9700-radio-bridge exited", "err", err)
		os.Exit(1)
	}
	log.Info("icom9700-radio-bridge stopped")
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	// The audio PCM publisher (2026-09-21 preview sink): while the audio
	// demand is set, demodulated radio audio (S16LE 48 kHz mono) is
	// re-published as raw UDP datagrams to the preview host. Unconnected
	// socket + WriteTo on purpose: a connected UDP socket caches ICMP-derived
	// errors (ECONNREFUSED while the preview restarts) and would fail every
	// later write silently. The first failure is logged, the rest dropped
	// (a down preview host must not spam the log).
	var audioSink func([]byte)
	if cfg.Audio.PublishAddr != "" {
		raddr, err := net.ResolveUDPAddr("udp", cfg.Audio.PublishAddr)
		if err != nil {
			return fmt.Errorf("audio.publish_addr: %w", err)
		}
		conn, err := net.ListenUDP("udp", &net.UDPAddr{})
		if err != nil {
			return fmt.Errorf("audio publish bind: %w", err)
		}
		defer conn.Close()
		var firstErr atomic.Bool
		audioSink = func(pcm []byte) {
			if _, err := conn.WriteToUDP(pcm, raddr); err != nil && firstErr.CompareAndSwap(false, true) {
				log.Warn("audio publish write failed (further errors dropped)", "err", err)
			}
		}
	}

	// The on-demand radio capture-session manager (U4): Run owns the
	// lifecycle (idle/connecting/live/error); the bus drives it through
	// SetAudioDemand. It sits politely idle until an audio_on demand
	// connects it (KTD-2 — the radio stays free for wfview). No command
	// path exists: the LAN session carries the receive-audio stream only.
	mgr := radio.NewManager(radio.Config{
		Host:           cfg.RadioHost,
		Username:       cfg.CIV.Username,
		Password:       cfg.CIV.Password,
		IdleTimeout:    cfg.Session.IdleTimeoutDur,
		MaxAttempts:    cfg.Session.MaxAttempts,
		AttemptSpacing: cfg.Session.AttemptSpacingDur,
		AudioDemandTTL: cfg.Audio.DemandTTLDur,
		AudioSink:      audioSink,
		Logger:         log,
	})

	// The serial CI-V telemetry monitor (2026-09 pivot): read-only reads
	// and transceive broadcasts over the radio's USB serial port, on-change
	// updates to /state. nil when serial.device is empty — the monitor_*
	// and power_on cmds are then rejected with that fact.
	var monitor bridge.Monitor
	if cfg.Serial.Device != "" {
		m, err := civserial.New(civserial.Options{
			Device:        cfg.Serial.Device,
			Baud:          cfg.Serial.Baud,
			MeterInterval: cfg.Serial.MeterIntervalDur,
			PowerFrame:    cfg.Serial.PowerOnFrameBytes,
			Open:          civserial.SerialOpen(cfg.Serial.Device, cfg.Serial.Baud),
			Logger:        log,
		})
		if err != nil {
			return fmt.Errorf("serial monitor: %w", err)
		}
		monitor = &monitorAdapter{m: m}
	} else {
		log.Info("no serial.device configured: monitor and power_on cmds disabled")
	}

	// The four-plane MQTT surface (U5): /meta /state /status /cmd with the
	// station gate set. The initial MQTT connect is fatal (model §8.1
	// item 10) — systemd's Restart=on-failure crash-loops the unit until
	// the broker answers.
	br, err := bridge.New(bridge.Options{
		Broker:   cfg.MQTT.Broker,
		User:     cfg.MQTT.User,
		Password: cfg.MQTT.Password,
		Site:     cfg.MQTT.Site,
		Station:  cfg.MQTT.Station,
		Slot:     cfg.MQTT.Slot,
		Location: cfg.MQTT.Location,
		Host:     cfg.RadioHost,
		Manager:  mgr,
		Monitor:  monitor,
		Logger:   log,
	})
	if err != nil {
		return fmt.Errorf("bridge: %w", err)
	}
	defer br.Close()
	if err := br.Start(ctx); err != nil {
		return fmt.Errorf("mqtt connect: %w", err)
	}

	go func() {
		if err := mgr.Run(ctx); err != nil && ctx.Err() == nil {
			log.Warn("radio session loop ended", "err", err)
		}
	}()
	br.Run()
	return ctx.Err()
}

// monitorAdapter narrows civserial.Monitor onto the bridge's Monitor
// interface, translating the telemetry state (civserial owns its own
// type — the bridge must not import the serial implementation).
type monitorAdapter struct {
	m *civserial.Monitor
}

func (a *monitorAdapter) SetMonitor(on bool) error { return a.m.SetMonitor(on) }

func (a *monitorAdapter) Wake(ctx context.Context) error { return a.m.Wake(ctx) }

func (a *monitorAdapter) Snapshot() bridge.RadioState {
	s := a.m.Snapshot()
	return bridge.RadioState{
		Responding: s.Responding,
		FreqHz:     s.FreqHz,
		Band:       s.Band,
		Mode:       s.Mode,
		Satellite:  s.Satellite,
		SMeter:     s.SMeter,
		SWR:        s.SWR,
		ALC:        s.ALC,
	}
}

func (a *monitorAdapter) Updates() <-chan struct{} { return a.m.Updates() }

func (a *monitorAdapter) Close() { a.m.Close() }

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
	return slog.New(h)
}
