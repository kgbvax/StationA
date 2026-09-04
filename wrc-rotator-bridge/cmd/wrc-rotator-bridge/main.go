// Command wrc-rotator-bridge bridges the HF antenna rotator (Yaesu G-450DC
// steered via an AF6SA WRC controller) to MQTT using the station integration
// model (slot muehle/hf/rotator). It reads the WRC's WebSocket status stream,
// publishes a canonical rotator state snapshot, dispatches /cmd intent
// (set_az, stop, fwd, rev) back to the rotator, and optionally runs a GS-232B
// TCP server for legacy rotator-control software. See README.md and
// docs/wrc-rotator-bridge-mqtt-api.md for details.
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

	"wrc-rotator-bridge/internal/bridge"
	"wrc-rotator-bridge/internal/config"
	"wrc-rotator-bridge/internal/gs232"
	"wrc-rotator-bridge/internal/pstrotator"
	"wrc-rotator-bridge/internal/rotor"
)

func main() {
	fs := flag.NewFlagSet("wrc-rotator-bridge", flag.ExitOnError)
	flags := config.RegisterFlags(fs)
	_ = fs.Parse(os.Args[1:])

	cfg, err := config.Load(flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wrc-rotator-bridge: load config: %v\n", err)
		os.Exit(2)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "wrc-rotator-bridge: %v\n", err)
		os.Exit(2)
	}

	logger := newLogger(cfg.Log.Level)
	logger.Info("wrc-rotator-bridge starting", "wrc", cfg.Rotor.URL, "slot", cfg.MQTT.Slot)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg, flags.Debug, logger); err != nil {
		// A SIGTERM/SIGINT cancels the root context; that's a clean shutdown,
		// not a failure — exit 0 so `systemctl stop` doesn't report FAILURE.
		if errors.Is(err, context.Canceled) {
			logger.Info("wrc-rotator-bridge stopped")
			return
		}
		logger.Error("wrc-rotator-bridge exited", "err", err)
		os.Exit(1)
	}
	logger.Info("wrc-rotator-bridge stopped")
}

func run(ctx context.Context, cfg config.Config, debug bool, log *slog.Logger) error {
	adapter := &slogAdapter{log}

	// Construct the publisher and bridge before connecting MQTT so the /cmd
	// handler is wired before OnConnect fires. pub.Client is assigned once the
	// paho client exists; bridge.New only needs the Publisher interface.
	pub := &bridge.PahoPublisher{}

	dev := rotor.New(cfg.Rotor.URL, debug, adapter)

	b := bridge.New(bridge.Config{
		Site:        cfg.MQTT.Site,
		Station:     cfg.MQTT.Station,
		Slot:        cfg.MQTT.Slot,
		Location:    cfg.MQTT.Location,
		Host:        cfg.Host,
		DeviceModel: cfg.Device.Model,
		DeviceLink:  cfg.Device.Link,
		Commander:   dev, // the rotator is the /cmd executor
	}, pub, adapter)

	// Wire the /cmd dispatch used by the OnConnect subscription callback.
	// Commands are funneled through a bounded channel to a single worker
	// goroutine, so the paho message handler never blocks (a set_az writes to
	// the WRC WebSocket) and commands serialize — no two moves interleave on
	// the wire. See the stationa memory on paho handlers: never do blocking
	// work in the message callback.
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
	// Publish the retained /meta birth certificate on (re)connect. OnConnect
	// fires during the initial Connect() before pub.Client is wired, so publish
	// via the paho client the callback receives (PublishMetaVia), not via pub.
	metaPublisher = b.PublishMetaVia

	// 1. Connect MQTT with LWT and a /cmd subscription.
	mqttClient, err := connectMQTT(ctx, cfg, log)
	if err != nil {
		return fmt.Errorf("mqtt connect: %w", err)
	}
	defer mqttClient.Disconnect(500)
	pub.Client = mqttClient
	log.Info("MQTT connected", "broker", cfg.MQTT.Broker)

	// 2. Start the GS-232B inbound server (optional legacy control path). It
	// drives the same device the bridge does; resulting motion surfaces in
	// /state. Runs in its own goroutines; ctx closes the listener on shutdown.
	if cfg.GS232.Enabled {
		srv := gs232.New(cfg.GS232.Bind, cfg.GS232.Port, dev, log)
		go func() {
			if err := srv.Run(ctx); err != nil {
				log.Error("GS-232 server ended", "err", err)
			}
		}()
	}

	// 3. Start the PSTRotator UDP listener (optional legacy control path). It
	// drives the same device the bridge does; resulting motion surfaces in
	// /state. Runs in its own goroutine; ctx closes the socket on shutdown.
	if cfg.PSTRotator.Enabled {
		srv := pstrotator.New(cfg.PSTRotator.Bind, cfg.PSTRotator.Port, dev, log)
		go func() {
			if err := srv.Run(ctx); err != nil {
				log.Error("PSTRotator UDP server ended", "err", err)
			}
		}()
	}

	// 4. Run the WRC WebSocket connection loop until ctx is cancelled.
	return wsLoop(ctx, cfg, b, dev, log)
}

// cmdHandler is set by run() so the paho /cmd subscription callback (wired in
// connectMQTT's OnConnect, before the bridge reference is otherwise in scope)
// can dispatch into the bridge. Package var rather than a closure to keep
// connectMQTT's signature independent of bridge construction order.
var cmdHandler = func(payload []byte) {}

// metaPublisher is set by run() to b.PublishMetaVia so connectMQTT's OnConnect
// callback can publish the retained /meta birth certificate using the paho
// client it receives (before pub.Client is wired). Same package-var pattern as
// cmdHandler, for the same reason: OnConnect is wired inside connectMQTT,
// before the bridge reference is otherwise in scope there.
var metaPublisher = func(c pahomqtt.Client) {}

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
			slot = "rotator"
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
		// Publish the retained /meta birth certificate on every (re)connect so
		// hadiscovery (and any consumer) always sees a fresh certificate.
		metaPublisher(c)
		// /cmd is not retained; resubscribe on every reconnect.
		if tok := c.Subscribe(cmd, 1, func(_ pahomqtt.Client, m pahomqtt.Message) {
			// Runs in paho's goroutine; the bridge only touches the WRC via the
			// Commander (rotor.Device), which guards all writes under its mutex.
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
	// bridges the wait through a goroutine + select on ctx.Done (see the
	// stationa memory on paho connect).
	if err := sharedmqtt.Connect(ctx, client); err != nil {
		return nil, err
	}
	return client, nil
}

// wsLoop dials the WRC, runs the status read loop, and restarts on failure with
// exponential backoff until ctx is cancelled.
func wsLoop(ctx context.Context, cfg config.Config, b *bridge.Bridge, dev *rotor.Device, log *slog.Logger) error {
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
		log.Warn("WRC run ended", "err", runErr)
		b.SetDeviceOnline(false, fmt.Sprintf("wrc: %v", runErr))
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
		slot = "rotator"
	}
	return schema.StatusTopic(cfg.MQTT.Site, cfg.MQTT.Station, slot)
}

// cmdTopic returns the /cmd topic: <site>/<station>/<slot>/cmd.
func cmdTopic(cfg config.Config) string {
	slot := cfg.MQTT.Slot
	if slot == "" {
		slot = "rotator"
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
func (s *slogAdapter) Errorf(format string, args ...any) { s.l.Error(fmt.Sprintf(format, args...)) }
func (s *slogAdapter) Debugf(format string, args ...any) { s.l.Debug(fmt.Sprintf(format, args...)) }
