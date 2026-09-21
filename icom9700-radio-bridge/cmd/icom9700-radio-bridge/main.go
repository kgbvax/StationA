// Command icom9700-radio-bridge fronts the Icom IC-9700 as the canonical
// `radio` slot muehle/uhf/radio on the station bus, controlling it over CI-V
// via Icom's RS-BA1-style LAN protocol. The radio's single LAN session is
// shared with manual wfview use by connecting only on demand (KTD-2).
//
// U1 scaffold: the MQTT plane, /cmd dispatch wiring and the radio reconnect
// loop are real; the radio side is the internal/radio stub (U2-U5 land the
// transport, codec, session manager and bus surface). See the feature plan
// (../docs/plans/2026-09-14-001-feat-icom9700-radio-bridge-plan.md) and the
// protocol brief (docs/civ-research-brief.md in this module).
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
	// re-published as raw UDP datagrams to the preview host. UDP write
	// failures are contained to audio — the first one is logged, the rest
	// are dropped silently (a down preview host must not spam the log).
	var audioSink func([]byte)
	if cfg.Audio.PublishAddr != "" {
		raddr, err := net.ResolveUDPAddr("udp", cfg.Audio.PublishAddr)
		if err != nil {
			return fmt.Errorf("audio.publish_addr: %w", err)
		}
		conn, err := net.DialUDP("udp", nil, raddr)
		if err != nil {
			return fmt.Errorf("audio publish dial: %w", err)
		}
		defer conn.Close()
		var firstErr atomic.Bool
		audioSink = func(pcm []byte) {
			if _, err := conn.Write(pcm); err != nil && firstErr.CompareAndSwap(false, true) {
				log.Warn("audio publish write failed (further errors dropped)", "err", err)
			}
		}
	}

	// The on-demand radio session manager (U4): Run owns the lifecycle
	// (idle/connecting/live/error); the bus drives it through the bridge's
	// Execute/SetHold. It sits politely idle until a /cmd demand or the
	// armed hold connects it (KTD-2 — the radio stays free for wfview).
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

	// The four-plane MQTT surface (U5): /meta /state /status /cmd with the
	// station gate set. The initial MQTT connect is fatal (model §8.1
	// item 10) — systemd's Restart=on-failure crash-loops the unit until
	// the broker answers.
	br, err := bridge.New(bridge.Options{
		Broker:       cfg.MQTT.Broker,
		User:         cfg.MQTT.User,
		Password:     cfg.MQTT.Password,
		Site:         cfg.MQTT.Site,
		Station:      cfg.MQTT.Station,
		Slot:         cfg.MQTT.Slot,
		Location:     cfg.MQTT.Location,
		Host:         cfg.RadioHost,
		Manager:      mgr,
		PollInterval: cfg.Radio.PollIntervalDur,
		TXWatchdog:   cfg.Session.TXWatchdogDur,
		Logger:       log,
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
