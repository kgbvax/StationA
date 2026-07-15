// Command shelly-power-bridge fronts one or more Shelly Gen2+ smart plugs that
// speak MQTT natively, translating their native topics into the station
// integration-model `power` slots. It runs one runtime goroutine per configured
// [[slot]] (Shelly), each with its own paho client + LWT, so a process death
// takes every fronted slot offline with no stale-online gap. See README.md and
// docs/shelly-power-bridge-mqtt-api.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
	schema "codeberg.org/kgbvax/stationa/shared/schema"

	"shelly-power-bridge/internal/bridge"
	"shelly-power-bridge/internal/config"
	"shelly-power-bridge/internal/shelly"
)

func main() {
	fs := flag.NewFlagSet("shelly-power-bridge", flag.ExitOnError)
	flags := config.RegisterFlags(fs)
	_ = fs.Parse(os.Args[1:])

	cfg, err := config.Load(flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "shelly-power-bridge: load config: %v\n", err)
		os.Exit(2)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "shelly-power-bridge: %v\n", err)
		os.Exit(2)
	}

	logger := newLogger(cfg.Log.Level)
	logger.Info("shelly-power-bridge starting", "slots", len(cfg.Slots), "site", cfg.MQTT.Site)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg, logger); err != nil {
		if errors.Is(err, context.Canceled) {
			logger.Info("shelly-power-bridge stopped")
			return
		}
		logger.Error("shelly-power-bridge exited", "err", err)
		os.Exit(1)
	}
	logger.Info("shelly-power-bridge stopped")
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	adapter := &slogAdapter{log}

	var wg sync.WaitGroup
	errCh := make(chan error, len(cfg.Slots))
	for _, sc := range cfg.Slots {
		sc := sc
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runSlot(ctx, cfg, sc, adapter, log); err != nil && !errors.Is(err, context.Canceled) {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		return err
	}
	return nil
}

// runSlot runs one Shelly → one canonical power slot. It owns a paho client
// (with the slot LWT), subscribes to the Shelly native status + heartbeat topics
// and the canonical /cmd, and translates between them. Returns ctx.Err() on
// shutdown.
func runSlot(ctx context.Context, cfg config.Config, sc config.SlotConfig, adapter *slogAdapter, log *slog.Logger) error {
	slotBase := schema.SlotBase(cfg.MQTT.Site, sc.Station, sc.Slot)
	avail := schema.StatusTopic(cfg.MQTT.Site, sc.Station, sc.Slot)
	cmdTopic := schema.CmdTopic(cfg.MQTT.Site, sc.Station, sc.Slot)
	nativeStatus := shelly.StatusTopic(sc.ShellyID)
	nativeOnline := sc.ShellyID + "/online"
	nativeRPC := shelly.RPCTopic(sc.ShellyID)

	pub := &bridge.PahoPublisher{}
	cmd := &shellyCommander{}

	b := bridge.New(bridge.Config{
		Site:            cfg.MQTT.Site,
		Station:         sc.Station,
		Slot:            sc.Slot,
		Location:        sc.Location,
		Host:            cfg.Host,
		DiscoveryPrefix: cfg.MQTT.DiscoveryPrefix,
		AvailTopic:      avail,
		DeviceModel:     sc.DeviceModel,
		DeviceSerial:    sc.DeviceSerial,
		FailSafe:        sc.FailSafe,
		Feeds:           sc.Feeds,
		Commander:       cmd,
	}, pub, adapter)

	// Single worker goroutine for all bridge state mutations + publishing, so
	// the paho message handlers (which run on paho's goroutine) never block on a
	// publish. See the stationa memory: paho handlers must not call blocking
	// Publish (hadiscovery deadlocked live).
	jobs := make(chan func(), 64)
	defer close(jobs)
	go sharedmqtt.RunJobs(ctx, jobs)

	// Heartbeat-driven device_online: the Shelly publishes "<id>/online"
	// "true" periodically and "false" on graceful disconnect. A watcher marks
	// the device offline if no heartbeat arrives within the staleness window.
	const heartbeatTimeout = 75 * time.Second
	lastHeartbeat := &heartbeatClock{}
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if lastHeartbeat.since() > heartbeatTimeout {
					sharedmqtt.Enqueue(jobs, func() { b.MarkDeviceOffline("shelly heartbeat lost") })
				}
			}
		}
	}()

	opts := pahomqtt.NewClientOptions()
	opts.AddBroker(cfg.MQTT.Broker)
	clientID := cfg.MQTT.ClientID
	if clientID == "" {
		clientID = cfg.MQTT.Site + "-" + sc.Station + "-" + sc.Slot
	}
	opts.SetClientID(clientID)
	if cfg.MQTT.User != "" {
		opts.SetUsername(cfg.MQTT.User)
		opts.SetPassword(cfg.MQTT.Password)
	}
	opts.SetAutoReconnect(true)
	opts.SetCleanSession(false)
	opts.SetWill(avail, "offline", 1, true)

	opts.OnConnect = func(c pahomqtt.Client) {
		log.Info("MQTT (re)connected", "slot", slotBase, "broker", cfg.MQTT.Broker)
		c.Publish(avail, 1, true, []byte("online"))
		pub.Client = c
		cmd.setClient(c, nativeRPC)
		b.SetConnected(true)
		b.PublishMeta()

		// Shelly native relay status → canonical power telemetry.
		if tok := c.Subscribe(nativeStatus, 1, func(_ pahomqtt.Client, m pahomqtt.Message) {
			_, power, err := shelly.ParseStatus(m.Payload())
			if err != nil {
				log.Warn("bad shelly status", "topic", m.Topic(), "err", err)
				return
			}
			sharedmqtt.Enqueue(jobs, func() { b.HandleTelemetry(power) })
		}); tok.Wait() && tok.Error() != nil {
			log.Warn("subscribe shelly status failed", "err", tok.Error())
		}

		// Shelly native heartbeat → device_online.
		if tok := c.Subscribe(nativeOnline, 1, func(_ pahomqtt.Client, m pahomqtt.Message) {
			if string(m.Payload()) == "true" {
				lastHeartbeat.touch()
				sharedmqtt.Enqueue(jobs, func() { b.HandleTelemetry("on") }) // refresh device_online true (no-op if power unchanged)
			} else {
				sharedmqtt.Enqueue(jobs, func() { b.MarkDeviceOffline("shelly online=false") })
			}
		}); tok.Wait() && tok.Error() != nil {
			log.Warn("subscribe shelly online failed", "err", tok.Error())
		}

		// Canonical /cmd (retained steady-state; the broker replays the last
		// command on every reconnect — self-heal, model §8).
		if tok := c.Subscribe(cmdTopic, 1, func(_ pahomqtt.Client, m pahomqtt.Message) {
			payload := append([]byte(nil), m.Payload()...) // copy; only valid during handler
			sharedmqtt.Enqueue(jobs, func() { b.HandleCommand(payload) })
		}); tok.Wait() && tok.Error() != nil {
			log.Warn("subscribe cmd failed", "err", tok.Error())
		}
	}
	opts.OnConnectionLost = func(_ pahomqtt.Client, err error) {
		log.Warn("MQTT connection lost", "slot", slotBase, "err", err)
		b.SetConnected(false)
	}

	client := pahomqtt.NewClient(opts)
	if err := sharedmqtt.Connect(ctx, client); err != nil {
		return fmt.Errorf("mqtt connect slot %s: %w", slotBase, err)
	}
	defer client.Disconnect(500)

	<-ctx.Done()
	return ctx.Err()
}

// shellyCommander implements bridge.Commander by publishing the Gen2+
// Switch.Set RPC over MQTT to the Shelly's "<id>/rpc" topic. Runs in the jobs
// worker (from HandleCommand), so blocking on the publish token is safe — it is
// never on a paho message-handler goroutine.
type shellyCommander struct {
	mu       sync.Mutex
	client   pahomqtt.Client
	rpcTopic string
}

func (s *shellyCommander) setClient(c pahomqtt.Client, rpcTopic string) {
	s.mu.Lock()
	s.client = c
	s.rpcTopic = rpcTopic
	s.mu.Unlock()
}

func (s *shellyCommander) SetPower(on bool) error {
	s.mu.Lock()
	c := s.client
	topic := s.rpcTopic
	s.mu.Unlock()
	if c == nil {
		return fmt.Errorf("mqtt client not connected")
	}
	tok := c.Publish(topic, 1, false, shelly.SwitchSet(on))
	tok.Wait()
	return tok.Error()
}

// heartbeatClock tracks the last Shelly "online" heartbeat; monotonic via a
// mutex-guarded time.Time. Safe for concurrent touch/since.
type heartbeatClock struct {
	mu   sync.Mutex
	last time.Time
}

func (h *heartbeatClock) touch() {
	h.mu.Lock()
	h.last = time.Now()
	h.mu.Unlock()
}

func (h *heartbeatClock) since() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.last.IsZero() {
		// No heartbeat yet since startup: count as stale so a never-heard Shelly
		// is reported offline rather than stuck online-default.
		return time.Hour
	}
	return time.Since(h.last)
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
