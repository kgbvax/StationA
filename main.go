// Command flex2mqtt observes a FlexRadio 6000-series radio and mirrors its
// state to MQTT for Home Assistant. See README.md for details.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	"flex2mqtt/internal/bridge"
	"flex2mqtt/internal/config"
	"flex2mqtt/internal/flexradio"
	"flex2mqtt/internal/ha"
)

func main() {
	fs := flag.NewFlagSet("flex2mqtt", flag.ExitOnError)
	flags := config.RegisterFlags(fs)
	_ = fs.Parse(os.Args[1:])

	cfg, err := config.Load(flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "flex2mqtt: load config: %v\n", err)
		os.Exit(2)
	}

	logger := newLogger(cfg.Log.Level)
	logger.Info("flex2mqtt starting")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("flex2mqtt exited", "err", err)
		os.Exit(1)
	}
	logger.Info("flex2mqtt stopped")
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	// 1. Connect MQTT with LWT.
	mqttClient, err := connectMQTT(ctx, cfg, log)
	if err != nil {
		return fmt.Errorf("mqtt connect: %w", err)
	}
	defer mqttClient.Disconnect(500)
	log.Info("MQTT connected", "broker", cfg.MQTT.Broker)

	pub := &bridge.PahoPublisher{Client: mqttClient}

	rates := map[flexradio.MeterGroup]time.Duration{
		flexradio.GroupTX:    cfg.Rates.Rate("tx"),
		flexradio.GroupAudio: cfg.Rates.Rate("audio"),
		flexradio.GroupRX:    cfg.Rates.Rate("rx"),
		flexradio.GroupHW:    cfg.Rates.Rate("hw"),
	}

	b := bridge.New(bridge.Config{
		Serial:          cfg.RadioSerial,
		StatePrefix:     cfg.MQTT.StatePrefix,
		DiscoveryPrefix: cfg.MQTT.DiscoveryPrefix,
		AvailTopic:      availabilityTopic(cfg),
		Rates:           rates,
	}, pub, &slogAdapter{log})

	// 2. Run the radio-connection loop until ctx is cancelled. Reconnects
	// internally on TCP drop or radio reboot.
	serial, err := radioLoop(ctx, cfg, b, pub, log)
	_ = serial
	return err
}

// connectMQTT establishes the MQTT connection with a Last Will that marks
// the bridge offline.
func connectMQTT(ctx context.Context, cfg config.Config, log *slog.Logger) (pahomqtt.Client, error) {
	opts := pahomqtt.NewClientOptions()
	opts.AddBroker(cfg.MQTT.Broker)
	clientID := cfg.MQTT.ClientID
	if clientID == "" {
		clientID = "flex2mqtt"
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
	opts.SetWill(avail, "offline", 1, true)
	opts.OnConnect = func(c pahomqtt.Client) {
		c.Publish(avail, 1, true, []byte("online"))
		log.Info("MQTT (re)connected, published online LWT")
	}
	opts.OnConnectionLost = func(c pahomqtt.Client, err error) {
		log.Warn("MQTT connection lost", "err", err)
	}

	client := pahomqtt.NewClient(opts)
	if tok := client.Connect(); tok.Wait() && tok.Error() != nil {
		return nil, tok.Error()
	}
	return client, nil
}

// availabilityTopic is the bridge's Last-Will-Testament topic.
func availabilityTopic(cfg config.Config) string {
	id := cfg.MQTT.ClientID
	if id == "" {
		id = "flex2mqtt"
	}
	return cfg.MQTT.StatePrefix + "/" + id + "/status"
}

// radioLoop connects to the radio (discovery or direct), runs the bridge,
// and reconnects on failure until ctx is cancelled.
//
// It returns only when ctx is cancelled; reconnects are internal.
func radioLoop(ctx context.Context, cfg config.Config, b *bridge.Bridge, pub *bridge.PahoPublisher, log *slog.Logger) (string, error) {
	const maxBackoff = 60 * time.Second
	backoff := 2 * time.Second

	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}

		// Resolve the radio host (discovery or direct).
		host, serial, err := resolveRadio(ctx, cfg, log)
		if err != nil {
			log.Warn("radio discovery failed", "err", err)
			if !sleepCtx(ctx, backoff) {
				return "", ctx.Err()
			}
			backoff = scaleBackoff(backoff, maxBackoff)
			continue
		}
		log.Info("radio resolved", "host", host, "serial", serial)
		b.SetDevice(ha.Device{Serial: serial, Model: radioModel(serial, cfg), Name: "FlexRadio " + radioModel(serial, cfg)})

		// Run the bridge against this radio until it disconnects.
		runErr := runOnce(ctx, cfg, host, b, log)

		// On ctx cancel: exit cleanly.
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

// resolveRadio returns the radio host (and serial) to connect to, via either
// the configured host or UDP discovery.
func resolveRadio(ctx context.Context, cfg config.Config, log *slog.Logger) (host, serial string, err error) {
	if cfg.RadioHost != "" {
		// Direct host: use it, serial is best-effort from the connection.
		if cfg.RadioSerial != "" {
			return cfg.RadioHost, cfg.RadioSerial, nil
		}
		// Discover just to get the serial for topic namespacing.
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
	// 1. Open the local UDP listener for VITA-49 meters.
	udpAddr := &net.UDPAddr{IP: net.IPv4zero, Port: cfg.RadioUDPPort}
	udpConn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		return fmt.Errorf("listen udp %d: %w", cfg.RadioUDPPort, err)
	}
	defer udpConn.Close()
	log.Info("UDP meter listener open", "port", cfg.RadioUDPPort)

	// 2. Connect TCP and handshake.
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	client, err := flexradio.Dial(cctx, host)
	if err != nil {
		return fmt.Errorf("dial radio: %w", err)
	}
	defer client.Close()
	// Unblock client.Run when ctx is cancelled. Without this, ReadString
	// holds the goroutine until SIGKILL (90 s systemd timeout).
	go func() { <-cctx.Done(); client.Close() }()
	log.Info("TCP connected to radio", "host", host)

	client.SetHandler(func(f flexradio.Frame) {
		switch f.Kind {
		case flexradio.FrameStatus:
			b.HandleStatus(f)
			// Lazily emit per-slice discovery when a slice first appears.
			if f.Topic == "slice" {
				b.MaybePublishSliceDiscovery()
			}
		case flexradio.FrameReply:
			b.HandleReply(f)
		}
	})

	info, err := client.Handshake(cctx, cfg.RadioUDPPort)
	if err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	if info.Serial != "" {
		b.SetDevice(ha.Device{
			Serial: info.Serial,
			Model:  info.Model,
			Name:   "FlexRadio " + info.Model,
		})
		log.Info("radio identified", "model", info.Model, "serial", info.Serial)
	}
	log.Info("handshake complete; observing")

	// 3. UDP meter reader goroutine.
	go udpReadLoop(cctx, udpConn, b, log)

	// 4. TCP status reader (blocks until disconnect).
	return client.Run(cctx)
}

// udpReadLoop reads VITA-49 datagrams and forwards them to the bridge.
func udpReadLoop(ctx context.Context, conn *net.UDPConn, b *bridge.Bridge, log *slog.Logger) {
	buf := make([]byte, 9000) // jumbo-safe; typical VITA datagrams are <1500
	for {
		if ctx.Err() != nil {
			return
		}
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue // loop back, check ctx
			}
			log.Warn("udp read error", "err", err)
			return
		}
		b.HandleMeterPacket(buf[:n])
	}
}

// sleepCtx sleeps for d, returning false if ctx is cancelled first.
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

// radioModel infers a model from the serial (FLEX-8400 etc.) when the
// discovery reply isn't available. Falls back to "FLEX-6000".
func radioModel(serial string, cfg config.Config) string {
	if cfg.RadioSerial != "" {
		// Heuristic: we only really know it's an 8400 from config intent.
		return "FLEX-8400"
	}
	return "FLEX-6000"
}

func defaultSerial(cfg config.Config) string {
	if cfg.RadioSerial != "" {
		return cfg.RadioSerial
	}
	return "flexradio"
}

// newLogger builds a slog.Logger at the configured level.
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

// slogAdapter adapts *slog.Logger to bridge.Logger.
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
