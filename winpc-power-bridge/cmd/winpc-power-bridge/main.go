// Command winpc-power-bridge runs on the Windows shack PC (bwpc) and publishes
// the station integration-model slot muehle/power/shack-pc. It reports that
// the PC is awake plus the user-idle time, and on /cmd {"action":"sleep"} it
// puts the PC to sleep (S3) if nobody has used keyboard or mouse for
// min_user_idle_minutes. powerseq wakes it again with Wake-on-LAN.
//
// It runs as a hidden logon Task Scheduler task in the interactive user
// session (GetLastInputInfo only sees that session), not as a service. See
// CLAUDE.md and docs/winpc-power-bridge-mqtt-api.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	"codeberg.org/kgbvax/stationa/shared/logging"
	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
	schema "codeberg.org/kgbvax/stationa/shared/schema"

	"winpc-power-bridge/internal/bridge"
	"winpc-power-bridge/internal/config"
	"winpc-power-bridge/internal/winpower"
)

const component = "winpc-power-bridge"

const (
	// publishTimeout bounds every token Wait, so a stalled broker cannot park
	// the jobs worker (review S1b pattern).
	publishTimeout = 10 * time.Second
	// subTimeout bounds the OnConnect subscribe Wait (review S1f).
	subTimeout = 10 * time.Second
	// offlineTimeout bounds the explicit "offline" publish before suspend.
	offlineTimeout = 5 * time.Second
	// disconnectQuiesceMs lets paho flush in-flight packets on Disconnect.
	disconnectQuiesceMs = 500
	// logRotateBytes: a log file above this size is moved to <file>.1 at start.
	logRotateBytes = 5 << 20
)

func main() {
	fs := flag.NewFlagSet(component, flag.ExitOnError)
	flags := config.RegisterFlags(fs)
	_ = fs.Parse(os.Args[1:])

	cfg, err := config.Load(flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: load config: %v\n", component, err)
		os.Exit(2)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", component, err)
		os.Exit(2)
	}

	logger, closeLog, err := newLogger(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: open log file: %v\n", component, err)
		os.Exit(2)
	}
	defer closeLog()
	slog.SetDefault(logger)

	slotBase := schema.SlotBase(cfg.MQTT.Site, cfg.MQTT.Station, cfg.MQTT.Slot)
	log := logger.With("slot", slotBase)
	log.Info(component+" starting", "config", cfg.Path(), "broker", cfg.MQTT.Broker,
		"min_user_idle_minutes", cfg.Sleep.MinUserIdleMinutes)
	if cfg.MQTT.Password == "" && cfg.MQTT.User != "" {
		log.Warn("no MQTT password: set " + config.EnvPrefix + "MQTT_PASSWORD or [mqtt] password")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg, log); err != nil && !logging.CleanStop(ctx, err) {
		log.Error(component+" exited", "err", err)
		closeLog()
		os.Exit(1)
	}
	log.Info(component + " stopped")
}

// run owns the jobs worker, the bridge and the MQTT session until ctx ends.
func run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	jobs := make(chan func(), 64)
	go sharedmqtt.RunJobs(ctx, jobs)

	s := &session{cfg: cfg, log: log, jobs: jobs}
	s.statusTopic = schema.StatusTopic(cfg.MQTT.Site, cfg.MQTT.Station, cfg.MQTT.Slot)
	s.cmdTopic = schema.CmdTopic(cfg.MQTT.Site, cfg.MQTT.Station, cfg.MQTT.Slot)

	s.b = bridge.New(bridge.Config{
		Site:         cfg.MQTT.Site,
		Station:      cfg.MQTT.Station,
		Slot:         cfg.MQTT.Slot,
		Location:     cfg.Location,
		Host:         cfg.Host,
		DeviceModel:  cfg.Device.Model,
		DeviceSerial: cfg.Device.Serial,
		Link:         cfg.Device.Link,
		MinUserIdle:  time.Duration(cfg.Sleep.MinUserIdleMinutes) * time.Minute,
	}, s, winpower.UserIdle, func() error { return s.sleep(ctx) }, log)

	if err := s.connect(ctx); err != nil {
		return err
	}
	defer s.disconnect()

	tick := time.NewTicker(time.Duration(cfg.State.IntervalS) * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			sharedmqtt.Enqueue(jobs, s.b.PublishState)
		}
	}
}

// session owns the current paho client. A paho client is not reused after
// Disconnect (paho warns that reuse may panic), so every connect — at start
// and after each resume from sleep — builds a fresh client from the same
// options. The bridge publishes through the session (bridge.Publisher), so it
// never holds a stale client.
type session struct {
	cfg         config.Config
	log         *slog.Logger
	jobs        chan func()
	b           *bridge.Bridge
	statusTopic string
	cmdTopic    string

	mu     sync.Mutex
	client pahomqtt.Client
}

// Publish implements bridge.Publisher on the current client (QoS 1).
func (s *session) Publish(topic string, retained bool, payload []byte) error {
	c := s.current()
	if c == nil || !c.IsConnected() {
		return bridge.ErrNotConnected
	}
	tok := c.Publish(topic, 1, retained, payload)
	if !tok.WaitTimeout(publishTimeout) {
		return fmt.Errorf("publish %s: timed out after %s", topic, publishTimeout)
	}
	return tok.Error()
}

func (s *session) current() pahomqtt.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

func (s *session) setClient(c pahomqtt.Client) {
	s.mu.Lock()
	s.client = c
	s.mu.Unlock()
}

// connect builds a fresh client and connects it. ConnectRetry keeps the
// connect token pending until the broker is reachable (network still coming
// up at logon or right after resume); sharedmqtt.Connect makes the wait
// ctx-aware.
func (s *session) connect(ctx context.Context) error {
	c := pahomqtt.NewClient(s.options())
	s.setClient(c)
	if err := sharedmqtt.Connect(ctx, c); err != nil {
		s.setClient(nil)
		return fmt.Errorf("mqtt connect: %w", err)
	}
	return nil
}

// disconnect publishes "offline" and closes the current client cleanly. A
// clean DISCONNECT suppresses the LWT, so the explicit "offline" is required.
func (s *session) disconnect() {
	c := s.current()
	if c == nil {
		return
	}
	if c.IsConnected() {
		tok := c.Publish(s.statusTopic, 1, true, []byte("offline"))
		if !tok.WaitTimeout(offlineTimeout) || tok.Error() != nil {
			s.log.Warn("publish offline before disconnect failed", "err", tok.Error())
		}
	}
	c.Disconnect(disconnectQuiesceMs)
	s.setClient(nil)
}

// sleep is the bridge's SuspendFunc. It runs on the jobs worker after the
// guard passed: check that the machine can suspend (refuse without a /status
// flap if not), go offline on the bus, disconnect, suspend, and — once
// SetSuspendState returns on resume — reconnect with a fresh client. The
// OnConnect of the new client republishes online + /meta + /state.
func (s *session) sleep(ctx context.Context) error {
	if err := winpower.CanSuspend(); err != nil {
		return err
	}
	s.disconnect()
	suspendErr := winpower.Suspend()
	if suspendErr != nil {
		s.log.Error("suspend failed; reconnecting", "err", suspendErr)
	}
	if err := s.connect(ctx); err != nil {
		return errors.Join(suspendErr, err)
	}
	return suspendErr
}

func (s *session) options() *pahomqtt.ClientOptions {
	cfg := s.cfg.MQTT
	opts := pahomqtt.NewClientOptions()
	opts.AddBroker(cfg.Broker)
	opts.SetClientID(cfg.ClientID)
	if cfg.User != "" {
		opts.SetUsername(cfg.User)
		opts.SetPassword(cfg.Password)
	}
	// Clean session: /cmd is subscribed at QoS 0 and nothing may be queued for
	// this client while the PC sleeps — a sleep command must never be
	// delivered late, after a wake.
	opts.SetCleanSession(true)
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(5 * time.Second)
	opts.SetMaxReconnectInterval(30 * time.Second)
	opts.SetKeepAlive(30 * time.Second)
	opts.SetWill(s.statusTopic, "offline", 1, true)

	opts.OnConnect = func(c pahomqtt.Client) {
		s.log.Info("MQTT (re)connected", "broker", cfg.Broker)
		c.Publish(s.statusTopic, 1, true, []byte("online")) // no Wait on the OnConnect goroutine
		sharedmqtt.Enqueue(s.jobs, func() {
			s.b.PublishMeta()
			s.b.PublishState()
		})
		// /cmd at QoS 0: one-shot, never queued. Retained replays are dropped
		// in bridge.HandleCommand (m.Retained()).
		tok := c.Subscribe(s.cmdTopic, 0, func(_ pahomqtt.Client, m pahomqtt.Message) {
			retained := m.Retained()
			payload := append([]byte(nil), m.Payload()...) // only valid during the handler
			sharedmqtt.Enqueue(s.jobs, func() { s.b.HandleCommand(retained, payload) })
		})
		if !tok.WaitTimeout(subTimeout) || tok.Error() != nil {
			s.log.Error("subscribe cmd failed", "topic", s.cmdTopic, "err", tok.Error())
		}
	}
	opts.OnConnectionLost = func(_ pahomqtt.Client, err error) {
		s.log.Warn("MQTT connection lost", "err", err)
	}
	return opts
}

// newLogger builds the root slog logger (logging convention §1: text handler,
// constant component attr). Windows exception (CLAUDE.md): with [log] file set
// the output goes to that file only — the hidden GUI-subsystem process has no
// usable stderr. A file above logRotateBytes is moved to <file>.1 at start.
func newLogger(cfg config.Config) (*slog.Logger, func(), error) {
	var lv slog.Level
	switch cfg.Log.Level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	var w io.Writer = os.Stderr
	closeFn := func() {}
	if path := cfg.LogFilePath(); path != "" {
		if fi, err := os.Stat(path); err == nil && fi.Size() > logRotateBytes {
			_ = os.Rename(path, path+".1")
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, nil, err
		}
		w = f
		var once sync.Once
		closeFn = func() { once.Do(func() { _ = f.Close() }) }
	}
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: lv})
	return slog.New(h).With("component", component), closeFn, nil
}
