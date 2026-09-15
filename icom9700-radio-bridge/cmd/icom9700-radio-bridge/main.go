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
	"os"
	"os/signal"
	"syscall"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
	schema "codeberg.org/kgbvax/stationa/shared/schema"

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
	// Construct the bridge (and with it the on-demand radio session) BEFORE
	// connecting MQTT so the /cmd handler exists when OnConnect fires. The
	// session is on-demand (KTD-2/R2): the process itself never dials —
	// login attempts happen only cmd-driven, arm-driven or safety-driven,
	// so there is deliberately no connect/backoff loop here (the U1 loop is
	// gone; the session manager owns its retry series internally).
	br := bridge.New(ctx, bridgeOptions(cfg, log))
	defer br.Close()
	mgr := br.Manager()

	// /cmd dispatch: paho runs message handlers on its own goroutine, and a
	// radio command is a network round-trip on the CI-V stream, so the handler
	// must not call the manager inline. Funnel commands through the bridge's
	// bounded jobs channel to its single sharedmqtt.RunJobs worker: the
	// handler Enqueues non-blocking, the worker runs Execute serially (the
	// same worker serializes every /state publish — one writer to the slot
	// state). (See the stationa memory on paho handlers: never do blocking
	// work in the message callback.)

	// 1. Connect MQTT with LWT and a /cmd subscription. Fatal on failure:
	// never run the bridge with its MQTT plane silently dead (ultrabridge
	// convention, model §8.1 item 10) — systemd's Restart=on-failure
	// crash-loops the unit until the broker answers.
	mqttClient, err := connectMQTT(ctx, cfg, mgr, br, log)
	if err != nil {
		return fmt.Errorf("mqtt connect: %w", err)
	}
	defer mqttClient.Disconnect(500)
	log.Info("MQTT connected", "broker", cfg.MQTT.Broker)

	// 2. Run the telemetry/heartbeat tick until ctx is cancelled: poll reads
	// while a session is live (R6 reconciliation + KTD-8 meters), the dedup/
	// freshness heartbeat in every state (R6).
	return telemetryLoop(ctx, mgr, cfg.Radio.PollIntervalDur, log)
}

// bridgeOptions maps the loaded config onto the bridge wiring (a straight
// field copy; no bridge policy figure is decided here). The arm timeout is
// the session series bound plus handshake margin — the widest legitimate
// blocking SetArmed.
func bridgeOptions(cfg config.Config, log *slog.Logger) bridge.Options {
	return bridge.Options{
		Site:     cfg.MQTT.Site,
		Station:  cfg.MQTT.Station,
		Slot:     cfg.MQTT.Slot,
		Location: cfg.MQTT.Location,

		PollInterval: cfg.Radio.PollIntervalDur,
		ArmTimeout:   cfg.Session.AttemptSpacingDur*time.Duration(cfg.Session.MaxAttempts) + 30*time.Second,

		RadioHost:      cfg.RadioHost,
		Username:       cfg.CIV.Username,
		Password:       cfg.CIV.Password,
		IdleTimeout:    cfg.Session.IdleTimeoutDur,
		MaxAttempts:    cfg.Session.MaxAttempts,
		AttemptSpacing: cfg.Session.AttemptSpacingDur,
		ErrorDecay:     cfg.Session.ErrorDecayDur,
		TXWatchdog:     cfg.Session.TXWatchdogDur,

		// U6 SEAMS (KTD-4/KTD-5): installed inside bridge.New — the safety
		// core replaces the v1 arm gate (Options.ArmGate) and arms its TX
		// watchdog on every PTT-on (Options.OnPTTOn). The MQTT-plane-loss
		// rule (R4) is wired at OnConnectionLost below.
		Log: log,
	}
}

// connectMQTT establishes the MQTT connection with a Last Will that marks the
// bridge offline.
func connectMQTT(ctx context.Context, cfg config.Config, mgr radio.Manager, br *bridge.Bridge, log *slog.Logger) (pahomqtt.Client, error) {
	opts := pahomqtt.NewClientOptions()
	opts.AddBroker(cfg.MQTT.Broker)
	clientID := cfg.MQTT.ClientID
	if clientID == "" {
		// Client ID derives from the slot address (model §8) so a duplicate
		// connection is diagnosable on the broker.
		clientID = cfg.MQTT.Site + "-" + cfg.MQTT.Station + "-" + cfg.MQTT.Slot
	}
	opts.SetClientID(clientID)
	if cfg.MQTT.User != "" {
		opts.SetUsername(cfg.MQTT.User)
		opts.SetPassword(cfg.MQTT.Password)
	}
	// AutoReconnect only. Deliberately NO SetConnectRetry: a retrying initial
	// connect would hang inside paho until SIGTERM instead of failing fast —
	// the fatal-exit-on-first-connect contract (ultrabridge convention, model
	// §8.1 item 10) needs the error to reach run() so systemd can crash-loop
	// the unit until the broker answers.
	opts.SetAutoReconnect(true)

	avail := schema.StatusTopic(cfg.MQTT.Site, cfg.MQTT.Station, cfg.MQTT.Slot)
	cmd := schema.CmdTopic(cfg.MQTT.Site, cfg.MQTT.Station, cfg.MQTT.Slot)
	opts.SetWill(avail, "offline", 1, true)
	opts.OnConnect = func(c pahomqtt.Client) {
		c.Publish(avail, 1, true, []byte("online"))
		log.Info("MQTT (re)connected, published online LWT")
		// The bridge's connect ritual: retained /meta and a forced /state
		// restore (a broker wipe must not leave the slot stateless).
		br.OnMQTTConnect(c)
		// /cmd is one-shot class (R9/KTD-6): subscribe at QoS 0 so a
		// persistent-session backlog cannot replay stale commands over a
		// fresh connection, and resubscribe on every reconnect.
		if tok := c.Subscribe(cmd, 0, func(_ pahomqtt.Client, m pahomqtt.Message) {
			// paho reuses the message buffer after the handler returns; copy it.
			p := append([]byte(nil), m.Payload()...)
			sharedmqtt.Enqueue(br.Jobs(), func() {
				if err := mgr.Execute(ctx, p); err != nil {
					log.Warn("cmd execution failed", "err", err)
				}
			})
		}); tok.Wait() && tok.Error() != nil {
			log.Warn("subscribe cmd failed", "err", tok.Error())
		}
	}
	opts.OnConnectionLost = func(_ pahomqtt.Client, err error) {
		log.Warn("MQTT connection lost", "err", err)
		// U6 (R4/KTD-5): the loss-of-plane rule — unkey over the radio plane
		// (RequestPTTOff, UDP, independent of the lost broker link) and
		// disarm. Enqueue-only: the paho goroutine must not block; the worker
		// applies the rule and records the loss fact in /state.error.
		br.OnMQTTConnectionLost(err)
	}

	client := pahomqtt.NewClient(opts)
	// Context-aware connect: paho's Connect().Wait() blocks ignoring ctx, so
	// a SIGTERM while the broker is unreachable (or auth is failing) can't
	// interrupt the connect and systemd must SIGKILL after TimeoutStopSec.
	// sharedmqtt.Connect bridges the wait through a goroutine + select on
	// ctx.Done (acom hit this live; flexbridge was latent — stationa memory).
	if err := sharedmqtt.Connect(ctx, client); err != nil {
		return nil, err
	}
	return client, nil
}

// telemetryLoop ticks the bridge's telemetry read at the configured poll
// interval until ctx is cancelled. Poll is quiet while no session is held
// (on-demand model) and drives the freshness heartbeat in every state, so
// the retained /state ts never goes stale — including at healthy idle (R6).
func telemetryLoop(ctx context.Context, mgr radio.Manager, every time.Duration, log *slog.Logger) error {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := mgr.Poll(ctx); err != nil {
				// A telemetry failure never takes the loop down: the next
				// tick re-reads, and session-level faults surface through
				// the session's own state machine into /state.
				log.Warn("telemetry tick failed", "err", err)
			}
		}
	}
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
