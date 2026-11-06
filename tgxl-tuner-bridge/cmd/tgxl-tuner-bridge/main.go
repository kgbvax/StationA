// Command tgxl-tuner-bridge bridges a FlexRadio Tuner Genius XL (1x3) to MQTT
// using the station integration model (slot muehle/hf/tuner). It polls the
// tuner's TCP API, publishes a canonical tuner state snapshot, and dispatches
// /cmd intent (set_inline, set_operate, set_antenna, tune) back to the tuner. See
// README.md and docs/tgxl-tuner-bridge-mqtt-api.md for details.
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

	"codeberg.org/kgbvax/stationa/shared/logging"
	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
	schema "codeberg.org/kgbvax/stationa/shared/schema"

	"tgxl-tuner-bridge/internal/bridge"
	"tgxl-tuner-bridge/internal/config"
	"tgxl-tuner-bridge/internal/tgxl"
)

func main() {
	fs := flag.NewFlagSet("tgxl-tuner-bridge", flag.ExitOnError)
	flags := config.RegisterFlags(fs)
	_ = fs.Parse(os.Args[1:])

	cfg, err := config.Load(flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tgxl-tuner-bridge: load config: %v\n", err)
		os.Exit(2)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "tgxl-tuner-bridge: %v\n", err)
		os.Exit(2)
	}

	// Logging convention §2: constant component attr on the root logger (also the
	// slog default, which device adapters log through) and the slot address.
	logger := newLogger(cfg.Log.Level).With("component", "tgxl-tuner-bridge",
		"slot", cfg.MQTT.Site+"/"+cfg.MQTT.Station+"/"+cfg.MQTT.Slot)
	slog.SetDefault(logger)
	logger.Info("tgxl-tuner-bridge starting", "tuner", cfg.Tuner.Address)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg, flags.Debug, logger); err != nil {
		// A SIGTERM/SIGINT cancels the root context; that's a clean shutdown,
		// not a failure — exit 0 so `systemctl stop` doesn't report FAILURE.
		if logging.CleanStop(ctx, err) {
			logger.Info("tgxl-tuner-bridge stopped")
			return
		}
		logger.Error("tgxl-tuner-bridge exited", "err", err)
		os.Exit(1)
	}
	logger.Info("tgxl-tuner-bridge stopped")
}

func run(ctx context.Context, cfg config.Config, debug bool, log *slog.Logger) error {
	adapter := &slogAdapter{log}

	// Construct the publisher and bridge before connecting MQTT so the /cmd
	// handler is wired before OnConnect fires. pub.Client is assigned once the
	// paho client exists; bridge.New only needs the Publisher interface.
	pub := &bridge.PahoPublisher{}

	poll, _ := cfg.PollDuration() // validated in main
	txPoll, _ := cfg.TxPollDuration()
	dev := tgxl.New(cfg.Tuner.Address, cfg.Tuner.AuthCode, poll, txPoll, debug, adapter)

	b := bridge.New(bridge.Config{
		Site:        cfg.MQTT.Site,
		Station:     cfg.MQTT.Station,
		Slot:        cfg.MQTT.Slot,
		Location:    cfg.MQTT.Location,
		Host:        cfg.Host,
		DeviceModel: cfg.Device.Model,
		DeviceLink:  cfg.Device.Link,
		Commander:   dev, // the tuner is the /cmd executor
	}, pub, adapter)
	dev.SetInfoHandler(b.SetDeviceInfo)

	// Wire the /cmd dispatch used by the OnConnect subscription callback.
	// Commands are funneled through a bounded channel to a single worker
	// goroutine, so the paho message handler never blocks (a set_inline/tune
	// writes to the TGXL) and commands serialize — no two commands
	// interleave on the wire. See the stationa memory on paho handlers: never
	// do blocking work in the message callback.
	cmdCh := make(chan []byte, 8)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case payload := <-cmdCh:
				b.HandleCommand(payload)
			}
		}
	}()
	cmdHandler = func(payload []byte) {
		select {
		case cmdCh <- payload:
		default:
			// Buffer full (command flooding): drop rather than block the paho
			// dispatch goroutine. The next /cmd re-arms once drained.
			log.Warn("/cmd queue full, dropping command")
		}
	}

	// 1. Connect MQTT with LWT and a /cmd subscription.
	mqttClient, err := connectMQTT(ctx, cfg, b, pub, log)
	if err != nil {
		return fmt.Errorf("mqtt connect: %w", err)
	}
	defer mqttClient.Disconnect(500)
	log.Info("MQTT connected", "broker", cfg.MQTT.Broker)

	// 2. Run the TGXL connection loop until ctx is cancelled.
	return devLoop(ctx, cfg, b, dev, log)
}

// cmdHandler is set by run() so the paho /cmd subscription callback (wired in
// connectMQTT's OnConnect, before the bridge reference is otherwise in scope)
// can dispatch into the bridge. Package var rather than a closure to keep
// connectMQTT's signature independent of bridge construction order.
var cmdHandler = func(payload []byte) {}

// connectMQTT establishes the MQTT connection with a Last Will that marks the
// bridge offline, and subscribes to the /cmd topic on (re)connect. The initial
// Connect is ctx-aware: paho's Connect()/Wait() blocks ignoring our context, so
// without this a SIGTERM while the broker is unreachable (or auth is failing)
// can't interrupt the connect and systemd must SIGKILL after TimeoutStopSec.
func connectMQTT(ctx context.Context, cfg config.Config, b *bridge.Bridge, pub *bridge.PahoPublisher, log *slog.Logger) (pahomqtt.Client, error) {
	opts := pahomqtt.NewClientOptions()
	opts.AddBroker(cfg.MQTT.Broker)
	clientID := cfg.MQTT.ClientID
	if clientID == "" {
		slot := cfg.MQTT.Slot
		if slot == "" {
			slot = "tuner"
		}
		clientID = cfg.MQTT.Site + "-" + cfg.MQTT.Station + "-" + slot
	}
	opts.SetClientID(clientID)
	if cfg.MQTT.User != "" {
		opts.SetUsername(cfg.MQTT.User)
		opts.SetPassword(cfg.MQTT.Password)
	}
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(5 * time.Second)

	avail := availabilityTopic(cfg)
	cmd := cmdTopic(cfg)
	opts.SetWill(avail, "offline", 1, true)
	opts.OnConnect = func(c pahomqtt.Client) {
		c.Publish(avail, 1, true, []byte("online"))
		log.Info("MQTT (re)connected, published online LWT")
		// /cmd is not retained; resubscribe on every reconnect.
		if tok := c.Subscribe(cmd, 1, func(_ pahomqtt.Client, m pahomqtt.Message) {
			// Runs in paho's goroutine; it only enqueues onto cmdCh (never blocks).
			cmdHandler(m.Payload())
		}); tok.Wait() && tok.Error() != nil {
			log.Warn("subscribe cmd failed", "err", tok.Error())
		}
		// Re-publish the retained birth certificate on reconnect so a fresh
		// broker (or a late subscriber) sees current identity/capabilities.
		b.PublishMeta()
		// Re-land the retained /state too (review S2): the broker may have
		// flushed it, and the bridge's change-only dedup would otherwise never
		// restore it while the tuner sits steady. No-ops before first telemetry.
		b.RepublishState()
	}
	opts.OnConnectionLost = func(_ pahomqtt.Client, err error) {
		log.Warn("MQTT connection lost", "err", err)
	}

	client := pahomqtt.NewClient(opts)
	pub.Client = client // set before Connect so OnConnect publishes are safe
	// Context-aware connect: paho's Connect().Wait() blocks ignoring ctx, so a
	// SIGTERM while the broker is unreachable (or auth is failing) can't
	// interrupt it and systemd must SIGKILL after TimeoutStopSec. sharedmqtt.Connect
	// bridges the wait through a goroutine + select on ctx.Done (see the
	// stationa memory on paho connect).
	if err := sharedmqtt.Connect(ctx, client); err != nil {
		return nil, err
	}
	return client, nil
}

// devLoop dials the TGXL, runs the poll/read loop, and restarts on failure
// with exponential backoff until ctx is cancelled.
func devLoop(ctx context.Context, cfg config.Config, b *bridge.Bridge, dev *tgxl.Device, log *slog.Logger) error {
	const maxBackoff = 60 * time.Second
	const healthyRun = time.Minute // a run that lived this long was a healthy link
	backoff := 2 * time.Second
	var lost logging.Streak // one Warn per outage, not per retry (TGXL powered off)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		start := time.Now()
		var runErr error
		if cfg.Tuner.Address == "" {
			var addr string
			addr, runErr = tgxl.Discover(ctx, 15*time.Second)
			if runErr == nil {
				dev.SetAddr(addr)
			}
		}
		if runErr == nil {
			runErr = dev.Run(ctx, b.HandleTelemetry)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(start) >= healthyRun {
			lost.Reset()
		}
		lost.Fail(log, "TGXL run ended", "err", runErr)
		b.SetDeviceOnline(false, fmt.Sprintf("tgxl: %v", runErr))
		if !sleepCtx(ctx, backoff) {
			return ctx.Err()
		}
		backoff = scaleBackoff(backoff, maxBackoff)
	}
}

// availabilityTopic returns the LWT topic: <site>/<station>/<slot>/status.
func availabilityTopic(cfg config.Config) string {
	slot := cfg.MQTT.Slot
	if slot == "" {
		slot = "tuner"
	}
	return schema.StatusTopic(cfg.MQTT.Site, cfg.MQTT.Station, slot)
}

// cmdTopic returns the /cmd topic: <site>/<station>/<slot>/cmd.
func cmdTopic(cfg config.Config) string {
	slot := cfg.MQTT.Slot
	if slot == "" {
		slot = "tuner"
	}
	return schema.CmdTopic(cfg.MQTT.Site, cfg.MQTT.Station, slot)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func scaleBackoff(cur, max time.Duration) time.Duration {
	next := time.Duration(float64(cur) * 1.5)
	if next > max {
		next = max
	}
	return next
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
	h := logging.NewHandler(os.Stderr, &slog.HandlerOptions{Level: lv})
	return slog.New(h)
}

type slogAdapter struct{ l *slog.Logger }

func (s *slogAdapter) Infof(format string, args ...any)  { s.l.Info(fmt.Sprintf(format, args...)) }
func (s *slogAdapter) Warnf(format string, args ...any)  { s.l.Warn(fmt.Sprintf(format, args...)) }
func (s *slogAdapter) Errorf(format string, args ...any) { s.l.Error(fmt.Sprintf(format, args...)) }
func (s *slogAdapter) Debugf(format string, args ...any) { s.l.Debug(fmt.Sprintf(format, args...)) }
