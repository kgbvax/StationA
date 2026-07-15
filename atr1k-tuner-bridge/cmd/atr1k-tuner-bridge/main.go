// Command atr1k-tuner-bridge bridges the ATR-1000 ATU (BTR-1000 / N7DDC family)
// to MQTT using the station integration model (slot muehle/hf/tuner). It reads
// the tuner's binary WebSocket status stream, publishes a canonical tuner state
// snapshot, and dispatches /cmd intent (set_inline, tune) back to the tuner. See
// README.md and docs/atr1k-tuner-bridge-mqtt-api.md for details.
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
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
	schema "codeberg.org/kgbvax/stationa/shared/schema"

	"atr1k-tuner-bridge/internal/bridge"
	"atr1k-tuner-bridge/internal/config"
	"atr1k-tuner-bridge/internal/tuner"
)

func main() {
	fs := flag.NewFlagSet("atr1k-tuner-bridge", flag.ExitOnError)
	flags := config.RegisterFlags(fs)
	_ = fs.Parse(os.Args[1:])

	cfg, err := config.Load(flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "atr1k-tuner-bridge: load config: %v\n", err)
		os.Exit(2)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "atr1k-tuner-bridge: %v\n", err)
		os.Exit(2)
	}

	logger := newLogger(cfg.Log.Level)
	logger.Info("atr1k-tuner-bridge starting", "tuner", cfg.Tuner.URL, "slot", cfg.MQTT.Slot)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg, flags.Debug, logger); err != nil {
		// A SIGTERM/SIGINT cancels the root context; that's a clean shutdown,
		// not a failure — exit 0 so `systemctl stop` doesn't report FAILURE.
		if errors.Is(err, context.Canceled) {
			logger.Info("atr1k-tuner-bridge stopped")
			return
		}
		logger.Error("atr1k-tuner-bridge exited", "err", err)
		os.Exit(1)
	}
	logger.Info("atr1k-tuner-bridge stopped")
}

func run(ctx context.Context, cfg config.Config, debug bool, log *slog.Logger) error {
	adapter := &slogAdapter{log}

	// Construct the publisher and bridge before connecting MQTT so the /cmd
	// handler is wired before OnConnect fires. pub.Client is assigned once the
	// paho client exists; bridge.New only needs the Publisher interface.
	pub := &bridge.PahoPublisher{}

	dev := tuner.New(cfg.Tuner.URL, debug, adapter)

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

	// Wire the /cmd dispatch used by the OnConnect subscription callback.
	// Commands are funneled through a bounded channel to a single worker
	// goroutine, so the paho message handler never blocks (a set_inline/tune
	// writes to the ATR WebSocket) and commands serialize — no two commands
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
	mqttClient, err := connectMQTT(ctx, cfg, log)
	if err != nil {
		return fmt.Errorf("mqtt connect: %w", err)
	}
	defer mqttClient.Disconnect(500)
	pub.Client = mqttClient
	log.Info("MQTT connected", "broker", cfg.MQTT.Broker)

	// 2. Run the ATR-1000 WebSocket connection loop until ctx is cancelled.
	return wsLoop(ctx, cfg, b, dev, log)
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
func connectMQTT(ctx context.Context, cfg config.Config, log *slog.Logger) (pahomqtt.Client, error) {
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
			// Runs in paho's goroutine; the bridge only touches the ATR via the
			// Commander (tuner.Device), which guards all writes under its mutex.
			cmdHandler(m.Payload())
		}); tok.Wait() && tok.Error() != nil {
			log.Warn("subscribe cmd failed", "err", tok.Error())
		}
		// Re-publish the retained birth certificate on reconnect so a fresh
		// broker (or a late subscriber) sees current identity/capabilities.
		publishMetaOnReconnect(c, cfg)
	}
	opts.OnConnectionLost = func(_ pahomqtt.Client, err error) {
		log.Warn("MQTT connection lost", "err", err)
	}

	client := pahomqtt.NewClient(opts)
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

// publishMetaOnReconnect re-publishes the retained /meta birth certificate on
// every (re)connect so the broker repopulates it after a flush. It builds a
// one-shot bridge bound to the live paho client just for PublishMeta.
func publishMetaOnReconnect(c pahomqtt.Client, cfg config.Config) {
	pub := &bridge.PahoPublisher{Client: c}
	b := bridge.New(bridge.Config{
		Site:        cfg.MQTT.Site,
		Station:     cfg.MQTT.Station,
		Slot:        cfg.MQTT.Slot,
		Location:    cfg.MQTT.Location,
		Host:        cfg.Host,
		DeviceModel: cfg.Device.Model,
		DeviceLink:  cfg.Device.Link,
	}, pub, &slogAdapter{slog.Default()})
	b.PublishMeta()
}

// wsLoop dials the ATR-1000, runs the status read loop, and restarts on failure
// with exponential backoff until ctx is cancelled.
func wsLoop(ctx context.Context, cfg config.Config, b *bridge.Bridge, dev *tuner.Device, log *slog.Logger) error {
	const maxBackoff = 60 * time.Second
	backoff := 2 * time.Second

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		runErr := dev.Run(ctx, b.HandleTelemetry)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Warn("ATR run ended", "err", runErr)
		b.SetDeviceOnline(false, fmt.Sprintf("atr1k: %v", runErr))
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
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv})
	return slog.New(h)
}

type slogAdapter struct{ l *slog.Logger }

func (s *slogAdapter) Infof(format string, args ...any)  { s.l.Info(fmt.Sprintf(format, args...)) }
func (s *slogAdapter) Warnf(format string, args ...any)  { s.l.Warn(fmt.Sprintf(format, args...)) }
func (s *slogAdapter) Debugf(format string, args ...any) { s.l.Debug(fmt.Sprintf(format, args...)) }
