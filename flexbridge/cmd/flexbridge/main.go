// Command flexbridge observes a FlexRadio 6000-series radio and mirrors its
// state to MQTT using the station integration model. See README.md for details.
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

	"flexbridge/internal/bridge"
	"flexbridge/internal/config"
	"flexbridge/internal/flexradio"
	"flexbridge/internal/ha"
)

func main() {
	fs := flag.NewFlagSet("flexbridge", flag.ExitOnError)
	flags := config.RegisterFlags(fs)
	_ = fs.Parse(os.Args[1:])

	cfg, err := config.Load(flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "flexbridge: load config: %v\n", err)
		os.Exit(2)
	}
	if cfg.MQTT.Site == "" || cfg.MQTT.Station == "" {
		// Station-model addressing is mandatory (model §2/§8.1): without site
		// and station the slot topics would be malformed.
		fmt.Fprintln(os.Stderr, "flexbridge: mqtt site and station must be configured for station-model addressing")
		os.Exit(2)
	}

	logger := newLogger(cfg.Log.Level)
	logger.Info("flexbridge starting")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("flexbridge exited", "err", err)
		os.Exit(1)
	}
	logger.Info("flexbridge stopped")
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	// Construct the publisher and bridge BEFORE connecting MQTT so the /cmd
	// handler is wired before OnConnect fires (OnConnect subscribes to /cmd and
	// dispatches into the bridge). pub.Client is assigned once the paho client
	// exists.
	pub := &bridge.PahoPublisher{}
	b := bridge.New(bridge.Config{
		Site:               cfg.MQTT.Site,
		Station:            cfg.MQTT.Station,
		Slot:               cfg.MQTT.Slot,
		Location:           cfg.MQTT.Location,
		DiscoveryPrefix:    cfg.MQTT.DiscoveryPrefix,
		AvailTopic:         availabilityTopic(cfg),
		PublishHADiscovery: cfg.MQTT.PublishHADiscovery,
	}, pub, &slogAdapter{log})

	// /cmd dispatch: paho runs message handlers on its own goroutine, and a DVK
	// command writes to the radio TCP socket (a blocking write with a deadline),
	// so the handler must not call the bridge inline. Funnel commands through a
	// bounded channel to a single sharedmqtt.RunJobs worker: the handler Enqueues
	// non-blocking, the worker runs HandleCommand serially. See the stationa
	// memory on paho handlers: never do blocking work in the message callback.
	jobs := make(chan func(), 32)
	go sharedmqtt.RunJobs(ctx, jobs)
	cmdHandler = func(payload []byte) {
		// paho reuses the message buffer after the handler returns; copy it.
		p := append([]byte(nil), payload...)
		sharedmqtt.Enqueue(jobs, func() { b.HandleCommand(p) })
	}

	// 1. Connect MQTT with LWT and a /cmd subscription.
	mqttClient, err := connectMQTT(ctx, cfg, log)
	if err != nil {
		return fmt.Errorf("mqtt connect: %w", err)
	}
	defer mqttClient.Disconnect(500)
	pub.Client = mqttClient
	log.Info("MQTT connected", "broker", cfg.MQTT.Broker)

	// 2. Run the radio-connection loop until ctx is cancelled.
	_, err = radioLoop(ctx, cfg, b, pub, log)
	return err
}

// cmdHandler is set by run() so the paho /cmd subscription callback (wired in
// connectMQTT's OnConnect, before the bridge reference is otherwise in scope)
// can dispatch into the bridge. Package var rather than a closure to keep
// connectMQTT's signature independent of bridge construction order.
var cmdHandler = func(payload []byte) {}

// connectMQTT establishes the MQTT connection with a Last Will that marks
// the bridge offline.
func connectMQTT(ctx context.Context, cfg config.Config, log *slog.Logger) (pahomqtt.Client, error) {
	opts := pahomqtt.NewClientOptions()
	opts.AddBroker(cfg.MQTT.Broker)
	clientID := cfg.MQTT.ClientID
	if clientID == "" {
		// Client ID derives from the slot address (model §8) so a duplicate
		// connection is diagnosable on the broker.
		slot := cfg.MQTT.Slot
		if slot == "" {
			slot = "radio"
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
		// /cmd is not retained (DVK is one-shot); resubscribe on every reconnect.
		if tok := c.Subscribe(cmd, 1, func(_ pahomqtt.Client, m pahomqtt.Message) {
			cmdHandler(m.Payload())
		}); tok.Wait() && tok.Error() != nil {
			log.Warn("subscribe cmd failed", "err", tok.Error())
		}
	}
	opts.OnConnectionLost = func(c pahomqtt.Client, err error) {
		log.Warn("MQTT connection lost", "err", err)
	}

	client := pahomqtt.NewClient(opts)
	// Context-aware connect: paho's Connect().Wait() blocks ignoring ctx, so a
	// SIGTERM while the broker is unreachable (or auth is failing) can't
	// interrupt the connect and systemd must SIGKILL after TimeoutStopSec.
	// sharedmqtt.Connect bridges the wait through a goroutine + select on
	// ctx.Done (flexbridge was latent for this; see the stationa memory).
	if err := sharedmqtt.Connect(ctx, client); err != nil {
		return nil, err
	}
	return client, nil
}

// availabilityTopic returns the LWT topic: <site>/<station>/<slot>/status.
// Site and station are validated non-empty at startup.
func availabilityTopic(cfg config.Config) string {
	slot := cfg.MQTT.Slot
	if slot == "" {
		slot = "radio"
	}
	return schema.StatusTopic(cfg.MQTT.Site, cfg.MQTT.Station, slot)
}

// cmdTopic returns the /cmd topic: <site>/<station>/<slot>/cmd.
func cmdTopic(cfg config.Config) string {
	slot := cfg.MQTT.Slot
	if slot == "" {
		slot = "radio"
	}
	return schema.CmdTopic(cfg.MQTT.Site, cfg.MQTT.Station, slot)
}

// radioLoop connects to the radio, runs the bridge, and reconnects on
// failure until ctx is cancelled.
func radioLoop(ctx context.Context, cfg config.Config, b *bridge.Bridge, pub *bridge.PahoPublisher, log *slog.Logger) (string, error) {
	const maxBackoff = 60 * time.Second
	backoff := 2 * time.Second

	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}

		host, serial, err := resolveRadio(ctx, cfg, log)
		if err != nil {
			log.Warn("radio discovery failed", "err", err)
			if !sleepCtx(ctx, backoff) {
				return "", ctx.Err()
			}
			backoff = scaleBackoff(backoff, maxBackoff)
			continue
		}
		// NOTE: do NOT call SetDevice here. The serial returned by resolveRadio
		// may be a placeholder ("flexradio") when discovery failed but a host is
		// configured — publishing /meta now would clobber the real retained
		// birth certificate with a bogus FLEX-6000/flexradio identity on every
		// retry while the radio is off. SetDevice is called only from runOnce
		// after a successful handshake, so /meta is (re)written solely with the
		// radio's real identity (or left untouched when the radio is unreachable).
		log.Info("radio resolved", "host", host, "serial", serial)

		runErr := runOnce(ctx, cfg, host, b, log)

		if ctx.Err() != nil {
			return serial, ctx.Err()
		}
		log.Warn("radio connection lost", "err", runErr)
		b.Reset()
		if !sleepCtx(ctx, backoff) {
			return serial, ctx.Err()
		}
		backoff = scaleBackoff(backoff, maxBackoff)
	}
}

// resolveRadio returns the radio host (and serial) to connect to.
func resolveRadio(ctx context.Context, cfg config.Config, log *slog.Logger) (host, serial string, err error) {
	if cfg.RadioHost != "" {
		if cfg.RadioSerial != "" {
			return cfg.RadioHost, cfg.RadioSerial, nil
		}
		dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if d, derr := flexradio.Discover(dctx, ""); derr == nil && d != nil {
			return cfg.RadioHost, d.Serial, nil
		}
		return cfg.RadioHost, defaultSerial(cfg), nil
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	d, err := flexradio.Discover(dctx, cfg.RadioSerial)
	if err != nil {
		return "", "", err
	}
	if d.IP == nil {
		return "", "", fmt.Errorf("discovery reply had no IP")
	}
	return d.IP.String(), d.Serial, nil
}

// runOnce performs one full connect/handshake/run cycle against the radio.
func runOnce(ctx context.Context, cfg config.Config, host string, b *bridge.Bridge, log *slog.Logger) error {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	client, err := flexradio.Dial(cctx, host)
	if err != nil {
		return fmt.Errorf("dial radio: %w", err)
	}
	defer client.Close()
	go func() { <-cctx.Done(); client.Close() }()
	log.Info("TCP connected to radio", "host", host)

	client.SetHandler(func(f flexradio.Frame) {
		if f.Kind == flexradio.FrameStatus {
			b.HandleStatus(f)
		}
	})

	info, err := client.Handshake(cctx)
	if err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	if info.Serial != "" {
		b.SetDevice(ha.Device{
			Serial:   info.Serial,
			Model:    info.Model,
			Name:     "FlexRadio " + info.Model,
			Firmware: info.Firmware,
		})
		log.Info("radio identified", "model", info.Model, "serial", info.Serial, "firmware", info.Firmware)
	}
	// The *Client implements flexradio.Commander (DVKPlay/DVKStop). Install it
	// so /cmd DVK intent reaches the radio; Reset() clears it on disconnect.
	b.SetCommander(client)
	log.Info("handshake complete; observing")

	return client.Run(cctx)
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

func defaultSerial(cfg config.Config) string {
	if cfg.RadioSerial != "" {
		return cfg.RadioSerial
	}
	return "flexradio"
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

func (s *slogAdapter) Infof(format string, args ...any) {
	s.l.Info(fmt.Sprintf(format, args...))
}
func (s *slogAdapter) Warnf(format string, args ...any) {
	s.l.Warn(fmt.Sprintf(format, args...))
}
func (s *slogAdapter) Debugf(format string, args ...any) {
	s.l.Debug(fmt.Sprintf(format, args...))
}
