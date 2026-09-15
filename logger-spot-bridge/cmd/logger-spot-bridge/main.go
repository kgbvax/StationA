// Command logger-spot-bridge fronts the shack logging software (DXLog, Log4OM)
// as the canonical muehle/hf/spots slot: it listens on UDP next to the loggers
// for the operator-entered-callsign broadcasts (N1MM-family `lookupinfo`,
// Log4OM outbound CALLSIGN), resolves the selected station to a map position
// and a beam bearing, and publishes it as the retained /state `selected`
// record. The console renders it so the operator can judge whether — and where
// — to turn the beam. See README.md and docs/logger-spot-bridge-mqtt-api.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
	schema "codeberg.org/kgbvax/stationa/shared/schema"

	"logger-spot-bridge/internal/bridge"
	"logger-spot-bridge/internal/config"
	"logger-spot-bridge/internal/geo"
	"logger-spot-bridge/internal/log4om"
	"logger-spot-bridge/internal/n1mm"
)

func main() {
	fs := flag.NewFlagSet("logger-spot-bridge", flag.ExitOnError)
	flags := config.RegisterFlags(fs)
	_ = fs.Parse(os.Args[1:])

	cfg, err := config.Load(flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logger-spot-bridge: %v\n", err)
		os.Exit(2)
	}

	logger := newLogger(cfg.Log.Level)
	logger.Info("logger-spot-bridge starting",
		"listeners", len(cfg.Listeners),
		"slot", schema.SlotBase(cfg.MQTT.Site, cfg.Slot.Station, cfg.Slot.Slot),
		"broker", cfg.MQTT.Broker)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg, logger); err != nil {
		if errors.Is(err, context.Canceled) {
			logger.Info("logger-spot-bridge stopped")
			return
		}
		logger.Error("logger-spot-bridge exited", "err", err)
		os.Exit(1)
	}
	logger.Info("logger-spot-bridge stopped")
}

// app carries the shared wiring one bridge instance needs: the canonical slot
// state, the resolver, the MQTT write path, the UDP liveness clock and the
// jobs queue every goroutine funnels through.
type app struct {
	cfg        config.Config
	log        *slog.Logger
	b          *bridge.SlotBridge
	r          bridge.Resolver
	jobs       chan func()
	clock      *livenessClock
	pub        *pahoPublisher
	stateTopic string
}

// pahoPublisher is the MQTT write path. Set on every (re)connect; a nil
// client before the first connect makes Publish a no-op (state arriving
// before the broker is up is still recorded — the birth sequence sends it
// later).
type pahoPublisher struct {
	mu     sync.Mutex
	client pahomqtt.Client
	topic  string
	log    *slog.Logger
}

func (p *pahoPublisher) set(c pahomqtt.Client, topic string) {
	p.mu.Lock()
	p.client = c
	p.topic = topic
	p.mu.Unlock()
}

// Publish writes the retained /state snapshot with a bounded wait (REQ-RT-6):
// a dead broker surfaces as a logged error instead of stalling the worker.
// Errors are not retried — the retained snapshot is re-announced by the next
// change or the reconnect birth sequence.
func (p *pahoPublisher) Publish(payload []byte) {
	p.mu.Lock()
	c, topic := p.client, p.topic
	p.mu.Unlock()
	if c == nil {
		return
	}
	tok := c.Publish(topic, 1, true, payload)
	if !tok.WaitTimeout(10 * time.Second) {
		return // timed out; the next announce or reconnect re-arms the snapshot
	}
	if err := tok.Error(); err != nil {
		p.log.Warn("state publish failed", "err", err)
	}
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	topics := bridge.TopicsFor(cfg.MQTT.Site, cfg.Slot.Station, cfg.Slot.Slot)

	a := &app{cfg: cfg, log: log, stateTopic: topics.State}
	a.clock = &livenessClock{staleAfter: cfg.StaleAfter}
	a.pub = &pahoPublisher{log: log}
	a.b = bridge.New(bridge.Meta{
		Schema: "1.0",
		Role:   "bandmap",
		Host:   cfg.Host,
		Device: map[string]any{
			"name": "Shack logger (DXLog/Log4OM)",
			"link": "udp-broadcast",
		},
		Caps: map[string]any{
			"source":  cfg.Listeners[0].Name,
			"actions": []string{"selected"},
		},
	})

	if ll, ok := geo.LocatorToLatLng(cfg.StationLocator); ok {
		a.r = bridge.Resolver{Station: ll, HasStation: true}
	} else {
		// Config validation rejects a malformed locator; reaching this path
		// means none was configured. Bearing resolution then relies wholly on
		// logger-provided azimuth/distance.
		log.Warn("no station_locator configured — logger azimuth/distance will not resolve to map coordinates")
	}

	// Single worker goroutine for all slot-state mutation + publishing (the
	// UDP reader goroutines and paho callbacks must never publish inline —
	// REQ-RT-1..3; hadiscovery deadlocked live on a blocking publish in a
	// handler). Lifetime tied to a child ctx: run can return before the
	// parent ctx is cancelled, and cancelling slotCtx stops the worker cleanly.
	slotCtx, slotCancel := context.WithCancel(ctx)
	defer slotCancel()
	a.jobs = make(chan func(), 32)
	go sharedmqtt.RunJobs(slotCtx, a.jobs)

	// Liveness watcher: flip device_online when the UDP feed goes quiet or
	// comes back. Enqueued like everything else.
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	go func() {
		for {
			select {
			case <-slotCtx.Done():
				return
			case <-tick.C:
				live := a.clock.live()
				if live != a.clock.published {
					sharedmqtt.Enqueue(a.jobs, func() { a.setOnline(live) })
				}
			}
		}
	}()

	// UDP listeners: one goroutine per configured logger stream. Decode runs
	// inline on the listener goroutine (pure CPU); the state mutation +
	// publish is enqueued. A dead listener is a dead bridge — the failure is
	// surfaced through errCh so run returns it and the supervisor restarts.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	errCh := make(chan error, len(cfg.Listeners))
	for _, lc := range cfg.Listeners {
		lc := lc
		go func() {
			if err := a.serveUDP(runCtx, lc); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("udp listener failed", "listener", lc.Name, "err", err)
				errCh <- err
				runCancel()
			}
		}()
	}

	// MQTT client with the slot LWT, birth sequence on every (re)connect.
	opts := pahomqtt.NewClientOptions()
	opts.AddBroker(cfg.MQTT.Broker)
	clientID := cfg.MQTT.ClientID
	if clientID == "" {
		clientID = cfg.MQTT.Site + "-" + cfg.Slot.Station + "-" + cfg.Slot.Slot
	}
	opts.SetClientID(clientID)
	if cfg.MQTT.User != "" {
		opts.SetUsername(cfg.MQTT.User)
		opts.SetPassword(cfg.MQTT.Password)
	}
	opts.SetAutoReconnect(true)
	opts.SetCleanSession(false)
	opts.SetWill(topics.Status, "offline", 1, true)

	opts.OnConnect = func(c pahomqtt.Client) {
		log.Info("MQTT (re)connected", "broker", cfg.MQTT.Broker)
		a.pub.set(c, topics.State)
		// Non-blocking fire-and-forget is safe on paho's goroutine (the token
		// is not waited); this mirrors the shelly-power-bridge birth sequence.
		c.Publish(topics.Status, 1, true, []byte("online"))
		if metaPayload, err := a.b.MetaPayload(); err == nil {
			c.Publish(topics.Meta, 1, true, metaPayload)
		} else {
			log.Warn("meta marshal failed", "err", err)
		}
		sharedmqtt.Enqueue(a.jobs, a.birth)
	}
	opts.OnConnectionLost = func(_ pahomqtt.Client, err error) {
		log.Warn("MQTT connection lost", "err", err)
	}

	client := pahomqtt.NewClient(opts)
	if err := sharedmqtt.Connect(ctx, client); err != nil {
		return fmt.Errorf("mqtt connect: %w", err)
	}
	defer client.Disconnect(500)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// serveUDP binds one listener and decodes datagrams until ctx dies.
func (a *app) serveUDP(ctx context.Context, lc config.ListenerConfig) error {
	addr := fmt.Sprintf("0.0.0.0:%d", lc.Port)
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("listen udp %s: %w", addr, err)
	}
	defer pc.Close()
	a.log.Info("udp listener up", "listener", lc.Name, "kind", lc.Kind, "addr", addr)

	go func() {
		<-ctx.Done()
		pc.Close() // unblocks ReadFrom
	}()

	buf := make([]byte, 4096)
	for {
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read: %w", err)
		}
		// Copy: buf is reused by the next ReadFrom and the enqueued decode
		// result (and debug raw capture) must not alias it.
		data := append([]byte(nil), buf[:n]...)
		a.handleDatagram(lc, data)
	}
}

// handleDatagram decodes one datagram and enqueues the state update. Runs on
// the listener goroutine — decode inline (pure CPU), enqueue the rest.
func (a *app) handleDatagram(lc config.ListenerConfig, data []byte) {
	a.clock.touch()

	switch lc.Kind {
	case "n1mm":
		// Ignore non-lookupinfo roots: contactinfo (QSO logged) ends nothing
		// here — the operator often stays on the call — and RadioInfo is
		// radio context the bus already carries via muehle/hf/radio.
		root := n1mm.RootName(data)
		if got := n1mm.Classify(root); got != n1mm.KindLookupInfo {
			if got == n1mm.KindOther {
				a.log.Debug("unrecognized datagram", "listener", lc.Name, "root", root, "raw", string(data))
			}
			return
		}
		li, err := n1mm.DecodeLookupInfo(data)
		if err != nil {
			a.log.Warn("bad lookupinfo", "listener", lc.Name, "err", err)
			return
		}
		a.log.Debug("lookupinfo", "listener", lc.Name, "call", li.Call,
			"reason", li.Reason, "az", li.Azimuth, "dist_km", li.DistanceKm)
		sharedmqtt.Enqueue(a.jobs, func() { a.apply(a.r.FromN1MM(li, lc.Name)) })

	case "log4om":
		c, err := log4om.DecodeCallsign(data)
		if err != nil {
			// Tolerant decoder: log the raw datagram at debug so an unknown
			// shape can be pinned from the log alone (the format is
			// undocumented; first captures showed it is NOT XML).
			a.log.Debug("log4om datagram not decoded", "listener", lc.Name,
				"root", log4om.RootName(data), "err", err, "raw", string(data))
			return
		}
		a.log.Debug("callsign", "listener", lc.Name, "call", c.Call)
		sharedmqtt.Enqueue(a.jobs, func() { a.apply(a.r.FromLog4OM(c, lc.Name)) })
	}
}

// apply publishes a new selection (nil = cleared). Jobs worker only.
func (a *app) apply(sel *bridge.Selected) {
	payload, changed, err := a.b.SetSelected(sel, a.clock.live())
	if err != nil {
		a.log.Warn("state marshal failed", "err", err)
		return
	}
	if !changed {
		return
	}
	a.pub.Publish(payload)
	if sel == nil {
		a.log.Info("selection cleared")
	} else {
		a.log.Info("selection published", "call", sel.Call, "band", sel.Band,
			"mode", sel.Mode, "az", sel.Azimuth, "dist_km", sel.DistanceKm,
			"lat", sel.Lat, "lng", sel.Lng, "source", sel.Source)
	}
}

// setOnline flips device_online (staleness watcher path). Jobs worker only.
func (a *app) setOnline(live bool) {
	a.clock.published = live // recorded even if the state dedups: it IS applied
	payload, changed, err := a.b.SetDeviceOnline(live, decodeSelected(a.b.LastJSON()))
	if err != nil {
		a.log.Warn("state marshal failed", "err", err)
		return
	}
	if changed {
		a.pub.Publish(payload)
		a.log.Info("device_online", "online", live)
	}
}

// birth republishes the last retained snapshot on (re)connect — verbatim, so
// a broker restart does not rewrite history (Jobs worker only).
func (a *app) birth() {
	if raw := a.b.LastJSON(); raw != "" {
		a.pub.Publish([]byte(raw))
	}
}

func decodeSelected(raw string) *bridge.Selected {
	if raw == "" {
		return nil
	}
	var st bridge.State
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return nil
	}
	return st.Selected
}

// livenessClock tracks "UDP heard recently" with a mutex; listener goroutines
// touch it, the watcher and the jobs worker read it. `published` mirrors the
// last value fed to setOnline so the watcher enqueues only real flips.
type livenessClock struct {
	mu         sync.Mutex
	last       time.Time
	staleAfter time.Duration
	published  bool
}

func (l *livenessClock) touch() {
	l.mu.Lock()
	l.last = time.Now()
	l.mu.Unlock()
}

func (l *livenessClock) live() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last.IsZero() {
		return false // nothing heard since start: logger feed not proven alive
	}
	return time.Since(l.last) <= l.staleAfter
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
	// Convention §1 (docs/conventions/logging.md): one constant `component`
	// attr per service.
	return slog.New(h).With("component", "logger-spot-bridge")
}
