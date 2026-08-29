// Command acom1200s-pa-bridge bridges an ACOM 600S/1200S linear amplifier to MQTT using
// the station integration model (slot muehle/hf/pa). It reads the amplifier's
// serial protocol over a USB-serial adapter, publishes a canonical PA state
// snapshot, and dispatches /cmd intent (set_mode, set_band, set_power) back to
// the amp. See README.md and docs/pa-mqtt-api.md for details.
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

	"acom1200s-pa-bridge/internal/acom"
	"acom1200s-pa-bridge/internal/bridge"
	"acom1200s-pa-bridge/internal/config"
)

func main() {
	fs := flag.NewFlagSet("acom1200s-pa-bridge", flag.ExitOnError)
	flags := config.RegisterFlags(fs)
	_ = fs.Parse(os.Args[1:])

	cfg, err := config.Load(flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "acom1200s-pa-bridge: load config: %v\n", err)
		os.Exit(2)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "acom1200s-pa-bridge: %v\n", err)
		os.Exit(2)
	}

	logger := newLogger(cfg.Log.Level)
	logger.Info("acom1200s-pa-bridge starting", "port", cfg.Serial.Port, "slot", cfg.MQTT.Slot)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg, flags.Debug, logger); err != nil {
		// A SIGTERM/SIGINT cancels the root context; that's a clean shutdown,
		// not a failure — exit 0 so `systemctl stop` doesn't report FAILURE.
		if errors.Is(err, context.Canceled) {
			logger.Info("acom1200s-pa-bridge stopped")
			return
		}
		logger.Error("acom1200s-pa-bridge exited", "err", err)
		os.Exit(1)
	}
	logger.Info("acom1200s-pa-bridge stopped")
}

func run(ctx context.Context, cfg config.Config, debug bool, log *slog.Logger) error {
	adapter := &slogAdapter{log}

	// Construct the publisher and bridge before connecting MQTT so the /cmd
	// handler is wired before OnConnect fires. pub.Client is assigned once the
	// paho client exists; bridge.New only needs the Publisher interface.
	pub := &bridge.PahoPublisher{}

	dev := acom.New(cfg.Serial.Port, cfg.Serial.AvgTimeMs, debug, adapter)

	b := bridge.New(bridge.Config{
		Site:               cfg.MQTT.Site,
		Station:            cfg.MQTT.Station,
		Slot:               cfg.MQTT.Slot,
		Location:           cfg.MQTT.Location,
		Host:               cfg.Host,
		DiscoveryPrefix:    cfg.MQTT.DiscoveryPrefix,
		AvailTopic:         availabilityTopic(cfg),
		PublishHADiscovery: cfg.MQTT.PublishHADiscovery,
		DeviceModel:        cfg.Device.Model,
		DeviceSerial:       cfg.Device.Serial,
		DeviceLink:         cfg.Device.Link,
		Commander:          dev, // the amplifier is the /cmd executor
	}, pub, adapter)

	// Wire the /cmd dispatch used by the OnConnect subscription callback.
	// Commands are funneled through a bounded channel to a single worker
	// goroutine, so the paho message handler never blocks (a set_band can walk
	// the amp for >1 s of serial writes) and commands serialize — no two
	// set_band walks interleave on the wire. See the stationa memory on paho
	// handlers: never do blocking work in the message callback.
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

	// 2. Run the serial connection loop until ctx is cancelled.
	return serialLoop(ctx, cfg, b, dev, log)
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
			slot = "pa"
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
			// Runs in paho's goroutine; the bridge only touches serial via the
			// Commander (acom.Device), which guards all writes under its mutex.
			cmdHandler(m.Payload())
		}); tok.Wait() && tok.Error() != nil {
			log.Warn("subscribe cmd failed", "err", tok.Error())
		}
	}
	opts.OnConnectionLost = func(_ pahomqtt.Client, err error) {
		log.Warn("MQTT connection lost", "err", err)
	}

	client := pahomqtt.NewClient(opts)
	// Context-aware connect: paho's Connect().Wait() blocks ignoring ctx, so a
	// SIGTERM while the broker is unreachable (or auth is failing) can't
	// interrupt it and systemd must SIGKILL after TimeoutStopSec. sharedmqtt.Connect
	// bridges the wait through a goroutine + select on ctx.Done (hit this bridge
	// live; see the stationa memory on paho connect).
	if err := sharedmqtt.Connect(ctx, client); err != nil {
		return nil, err
	}
	return client, nil
}

// serialLoop opens the serial port, runs the telemetry loop, and restarts on
// failure with exponential backoff until ctx is cancelled.
//
// The loop is a pure observer: it does not drive the amp's power state and
// touches no host control lines. When the amp is unpowered (its remote-on
// relay released by the hf/switch slot) the serial port goes silent and the
// loop backs off; once the amp is powered on again telemetry resumes and the
// loop reconnects. There is no "park" mode — the loop simply keeps retrying
// while the bridge process is up.
func serialLoop(ctx context.Context, cfg config.Config, b *bridge.Bridge, dev *acom.Device, log *slog.Logger) error {
	const maxBackoff = 60 * time.Second
	const initialBackoff = 2 * time.Second
	const healthyRunForReset = time.Minute
	backoff := initialBackoff

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		runStart := time.Now()
		runErr := runOnce(ctx, cfg, b, dev, log)
		ran := time.Since(runStart) // run duration only — excludes the backoff sleep below
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Warn("serial run ended", "err", runErr)
		b.SetDeviceOnline(false, fmt.Sprintf("serial: %v", runErr))

		// A run that lasted a while was a healthy episode; the escalation
		// belongs to whatever broke it, not to history. Without this reset the
		// backoff sits at maxBackoff forever after a few short-lived cycles
		// (e.g. the amp's PSU cycled on/off repeatedly), making every later
		// transient fault wait the full 60 s to reconnect. Applied BEFORE the
		// sleep so the reconnect following the healthy run is the fast one,
		// and measured on run duration so a persistently broken link still
		// escalates to (and holds) maxBackoff.
		if ran >= healthyRunForReset {
			backoff = initialBackoff
		} else {
			backoff = scaleBackoff(backoff, maxBackoff)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
}

// runOnce performs one open/run cycle against the amplifier.
func runOnce(ctx context.Context, cfg config.Config, b *bridge.Bridge, dev *acom.Device, log *slog.Logger) error {
	if err := dev.Open(); err != nil {
		return fmt.Errorf("open serial: %w", err)
	}
	defer dev.Close()
	log.Info("serial port opened", "port", cfg.Serial.Port)
	b.SetDeviceOnline(true, "")

	// Publish meta + (gated) discovery on each (re)connect cycle so late
	// subscribers get the birth certificate. /status online is handled by the
	// MQTT OnConnect.
	b.PublishMeta()
	b.PublishDiscovery()

	return dev.Run(ctx, b.HandleTelemetry)
}

// availabilityTopic returns the LWT topic: <site>/<station>/<slot>/status.
func availabilityTopic(cfg config.Config) string {
	slot := cfg.MQTT.Slot
	if slot == "" {
		slot = "pa"
	}
	return schema.StatusTopic(cfg.MQTT.Site, cfg.MQTT.Station, slot)
}

// cmdTopic returns the /cmd topic: <site>/<station>/<slot>/cmd.
func cmdTopic(cfg config.Config) string {
	slot := cfg.MQTT.Slot
	if slot == "" {
		slot = "pa"
	}
	return schema.CmdTopic(cfg.MQTT.Site, cfg.MQTT.Station, slot)
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
