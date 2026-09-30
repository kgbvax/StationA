// Command oscarwatch-sattrack-bridge fronts OscarWatch's "Satellite link"
// WebSocket as the canonical muehle/uhf/sat-track slot: it dials the tracker
// on the shack PC, follows the focused satellite (name, transponder, look
// angle, range, range rate, sunlit, radio-corrected frequencies), derives the
// sub-satellite point from the look angle and the station position, and
// publishes it all as one retained /state snapshot. Read-only: no /cmd.
// See README.md and docs/oscarwatch-sattrack-bridge-mqtt-api.md.
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
	"sync/atomic"
	"syscall"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
	schema "codeberg.org/kgbvax/stationa/shared/schema"

	"oscarwatch-sattrack-bridge/internal/bridge"
	"oscarwatch-sattrack-bridge/internal/config"
	"oscarwatch-sattrack-bridge/internal/geo"
	"oscarwatch-sattrack-bridge/internal/oscarwatch"
	"oscarwatch-sattrack-bridge/internal/source"
)

const component = "oscarwatch-sattrack-bridge"

func main() {
	fs := flag.NewFlagSet(component, flag.ExitOnError)
	flags := config.RegisterFlags(fs)
	check := fs.Bool("check", false, "validate the effective config, print it redacted, exit without touching MQTT or OscarWatch")
	_ = fs.Parse(os.Args[1:])

	cfg, err := config.Load(flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", component, err)
		os.Exit(2)
	}

	if *check {
		lines, err := config.Check(cfg, true)
		for _, l := range lines {
			fmt.Println(l)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: config check FAILED: %v\n", component, err)
			os.Exit(1)
		}
		fmt.Println("config check OK")
		return
	}

	root := newLogger(cfg.Log.Level)
	slog.SetDefault(root)
	slot := schema.SlotBase(cfg.MQTT.Site, cfg.Slot.Station, cfg.Slot.Slot)
	log := root.With("slot", slot)
	_, obsDesc := cfg.Observer()
	log.Info("starting", "source", cfg.Source.URL, "broker", cfg.MQTT.Broker, "station", obsDesc)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("exited", "err", err)
		os.Exit(1)
	}
	log.Info("stopped")
}

// app is the wiring of one bridge instance.
//
// Data flow: the WebSocket reader (wsLoop) replaces the bridge.Input under mu
// and wakes the state worker; the worker builds the snapshot from the NEWEST
// input and publishes it. Latest-wins by construction — a slow broker (the
// publish waits up to 10 s) cannot back up a queue of stale 1 Hz look angles,
// and a disconnect can never be dropped behind them. paho's OnConnect only
// sets a flag and wakes the worker (paho handlers must not publish blocking).
type app struct {
	cfg config.Config
	log *slog.Logger
	b   *bridge.SlotBridge
	obs *geo.Observer
	pub statePublisher

	mu    sync.Mutex
	in    bridge.Input
	birth bool          // republish the retained snapshot (MQTT reconnected)
	kick  chan struct{} // cap 1: wake the state worker

	prev          bridge.State // last published snapshot, for transition logs (worker only)
	warnedVersion atomic.Bool
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	topics := bridge.TopicsFor(cfg.MQTT.Site, cfg.Slot.Station, cfg.Slot.Slot)
	obs, _ := cfg.Observer()
	pub := &pahoPublisher{topic: topics.State, log: log}
	a := &app{
		cfg:  cfg,
		log:  log,
		b:    bridge.New(bridge.Meta(bridge.MetaOptions{Slot: cfg.Slot.Slot, Location: cfg.Location, Host: cfg.Host})),
		obs:  obs,
		pub:  pub,
		in:   bridge.Input{Err: "oscarwatch: not connected yet"},
		kick: make(chan struct{}, 1),
	}

	// The worker's lifetime is a child ctx so it stops before the MQTT
	// client disconnects below.
	workCtx, workCancel := context.WithCancel(ctx)
	defer workCancel()
	go a.stateWorker(workCtx)
	a.wake() // publish the initial device_online=false snapshot once connected

	src := source.New(cfg.Source.URL, cfg.Source.PingInterval, log)
	go a.wsLoop(workCtx, src)

	opts := pahomqtt.NewClientOptions()
	opts.AddBroker(cfg.MQTT.Broker)
	opts.SetClientID(cfg.MQTT.ClientID)
	if cfg.MQTT.User != "" {
		opts.SetUsername(cfg.MQTT.User)
		opts.SetPassword(cfg.MQTT.Password)
	}
	opts.SetAutoReconnect(true)
	opts.SetCleanSession(false)
	opts.SetWill(topics.Status, "offline", 1, true)
	opts.OnConnect = func(c pahomqtt.Client) {
		log.Info("MQTT (re)connected", "broker", cfg.MQTT.Broker)
		pub.set(c)
		// Fire-and-forget on paho's goroutine: tokens are not waited.
		c.Publish(topics.Status, 1, true, []byte("online"))
		if meta, err := a.b.MetaPayload(); err == nil {
			c.Publish(topics.Meta, 1, true, meta)
		} else {
			log.Error("meta marshal failed", "err", err)
		}
		a.requestBirth()
	}
	opts.OnConnectionLost = func(_ pahomqtt.Client, err error) {
		log.Warn("MQTT connection lost", "err", err)
	}

	client := pahomqtt.NewClient(opts)
	if err := sharedmqtt.Connect(ctx, client); err != nil {
		return fmt.Errorf("mqtt connect: %w", err)
	}

	<-ctx.Done()
	workCancel()
	// The LWT only fires on an unclean drop; a clean stop announces offline
	// itself (integration model §8.1 item 3).
	if client.IsConnectionOpen() {
		client.Publish(topics.Status, 1, true, []byte("offline")).WaitTimeout(2 * time.Second)
	}
	client.Disconnect(250)
	return ctx.Err()
}

// wsLoop keeps the OscarWatch link up: dial, read until it fails, back off,
// repeat. Backoff resets after every connection that came up, so a drop after
// hours of uptime retries in 2 s. Logging is edge-triggered: one Warn when the
// link is lost or first found unreachable, Debug while it stays down — the PC
// being off overnight must not flood `journalctl -p warning`.
func (a *app) wsLoop(ctx context.Context, src *source.Client) {
	const minBackoff, maxBackoff = 2 * time.Second, 60 * time.Second
	backoff := minBackoff
	reported := false // the current outage has been logged at Warn

	for {
		connected := false
		err := src.Run(ctx, func() {
			connected = true
			reported = false
			a.log.Info("OscarWatch connected", "url", src.URL)
			a.update(func(in *bridge.Input) { *in = bridge.Input{Online: true} })
		}, a.handleFrame)
		if ctx.Err() != nil {
			return
		}
		a.update(func(in *bridge.Input) { *in = bridge.Input{Err: "oscarwatch: " + err.Error()} })

		switch {
		case connected:
			a.log.Warn("OscarWatch link lost", "err", err)
			backoff = minBackoff
			reported = true
		case !reported:
			a.log.Warn("OscarWatch unreachable, retrying", "url", src.URL, "err", err)
			reported = true
		default:
			a.log.Debug("OscarWatch still unreachable", "err", err, "retry_in", backoff)
		}

		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(time.Duration(float64(backoff)*1.5), maxBackoff)
	}
}

// handleFrame routes one text frame. Runs on the WebSocket reader goroutine:
// decoding is pure CPU, the publish happens on the state worker.
func (a *app) handleFrame(data []byte) {
	h, err := oscarwatch.PeekHeader(data)
	if err != nil {
		a.log.Error("malformed frame dropped", "err", err, "len", len(data))
		a.log.Debug("malformed frame", "raw", string(data))
		return
	}
	switch oscarwatch.Classify(h.Type) {
	case oscarwatch.KindStatus:
		if h.Version != oscarwatch.ProtocolVersion && !a.warnedVersion.Swap(true) {
			a.log.Warn("unexpected Satellite-link protocol version, decoding as v1",
				"version", h.Version, "want", oscarwatch.ProtocolVersion)
		}
		st, err := oscarwatch.DecodeStatus(data)
		if err != nil {
			a.log.Error("malformed satelliteStatus dropped", "err", err)
			a.log.Debug("malformed satelliteStatus", "raw", string(data))
			return
		}
		a.update(func(in *bridge.Input) { in.Status = &st })
	case oscarwatch.KindQSO:
		a.log.Debug("logbook event ignored", "type", h.Type)
	default:
		a.log.Debug("unknown frame type ignored", "type", h.Type)
	}
}

// update mutates the input under the lock and wakes the worker. Status frames
// are replaced, never mutated in place, so the worker may keep the pointer.
func (a *app) update(f func(*bridge.Input)) {
	a.mu.Lock()
	f(&a.in)
	a.mu.Unlock()
	a.wake()
}

func (a *app) requestBirth() {
	a.mu.Lock()
	a.birth = true
	a.mu.Unlock()
	a.wake()
}

func (a *app) wake() {
	select {
	case a.kick <- struct{}{}:
	default: // already pending; the worker reads the newest input anyway
	}
}

func (a *app) stateWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.kick:
			a.flush()
		}
	}
}

// flush builds the snapshot from the newest input and publishes it if it
// changed; on a birth request an unchanged snapshot is republished verbatim.
func (a *app) flush() {
	a.mu.Lock()
	in, birth := a.in, a.birth
	a.birth = false
	a.mu.Unlock()

	st := bridge.Build(in, a.obs, time.Now())
	payload, changed, err := a.b.Update(st)
	if err != nil {
		a.log.Error("state marshal failed", "err", err)
		return
	}
	switch {
	case changed:
		a.pub.Publish(payload)
		a.logTransitions(st)
		a.prev = st
	case birth:
		if raw := a.b.LastJSON(); raw != "" {
			a.pub.Publish([]byte(raw))
		}
	}
}

// logTransitions reports focus changes and AOS/LOS at Info; the 1 Hz look
// angle updates are Debug.
func (a *app) logTransitions(st bridge.State) {
	name, prevName := deref(st.SatName), deref(a.prev.SatName)
	switch {
	case name != "" && name != prevName:
		a.log.Info("tracking satellite", "sat", name, "norad", deref(st.NoradID),
			"mode_type", deref(st.ModeType), "in_range", st.InRange)
	case name == "" && prevName != "":
		a.log.Info("no satellite tracked")
	case name != "" && st.InRange != a.prev.InRange:
		event := "LOS"
		if st.InRange {
			event = "AOS"
		}
		a.log.Info(event, "sat", name, "az", derefF(st.Az), "el", derefF(st.El))
	}
	a.log.Debug("state published", "sat", name, "az", derefF(st.Az), "el", derefF(st.El),
		"range_km", derefF(st.RangeKm), "sub_lat", derefF(st.SubLat), "sub_lng", derefF(st.SubLng))
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefF(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

// statePublisher writes the retained /state snapshot (a seam for tests).
type statePublisher interface {
	Publish(payload []byte)
}

// pahoPublisher is the /state write path. The client is set on every
// (re)connect; before the first connect Publish is a no-op (the snapshot is
// still recorded and the birth sequence sends it).
type pahoPublisher struct {
	mu     sync.Mutex
	client pahomqtt.Client
	topic  string
	log    *slog.Logger
}

func (p *pahoPublisher) set(c pahomqtt.Client) {
	p.mu.Lock()
	p.client = c
	p.mu.Unlock()
}

// Publish writes the retained snapshot with a bounded wait: a dead broker is
// a logged error, not a stalled worker. No retry — the next change or the
// reconnect birth re-sends the snapshot.
func (p *pahoPublisher) Publish(payload []byte) {
	p.mu.Lock()
	c := p.client
	p.mu.Unlock()
	if c == nil {
		return
	}
	tok := c.Publish(p.topic, 1, true, payload)
	if !tok.WaitTimeout(10 * time.Second) {
		p.log.Warn("state publish timed out")
		return
	}
	if err := tok.Error(); err != nil {
		p.log.Error("state publish failed", "err", err)
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
	return slog.New(h).With("component", component)
}
