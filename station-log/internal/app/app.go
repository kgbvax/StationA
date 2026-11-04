// Package app wires station-log together: the store, the importer, the MQTT
// slot (/meta, /status, retained /state, the per-QSO event topic), the HTTP
// API and the optional Log4OM feeder. Event publishing runs on one jobs
// worker (shared/mqtt.RunJobs); no MQTT callback publishes.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
	"codeberg.org/kgbvax/stationa/shared/schema"

	"station-log/internal/config"
	"station-log/internal/httpapi"
	"station-log/internal/importer"
	"station-log/internal/log4om"
	"station-log/internal/store"
	"station-log/internal/watch"
)

// heartbeat is how often /state is re-evaluated, so an import done by the
// CLI (which cannot poke a running service) shows up too.
const heartbeat = 60 * time.Second

// Publisher is the MQTT publish path; the fake of the tests implements it.
type Publisher interface {
	Publish(topic string, qos byte, retained bool, payload []byte) error
}

// Service is a running station-log.
type Service struct {
	cfg config.Config
	log *slog.Logger
	st  *store.Store
	imp *importer.Importer
	now func() time.Time

	jobs  chan func() // event publishing, one worker
	dirty chan struct{}
	stop  chan struct{} // closed when Run ends: unblocks emitters

	pubMu sync.RWMutex
	pub   Publisher

	dropped  atomic.Int64 // events that could not be published
	l4Err    atomic.Pointer[string]
	lastPub  []byte // publisher goroutine only
	forcePub atomic.Bool

	watcher   *watch.Watcher
	ready     chan struct{} // closed once the MQTT link is up (first scan of the drop directory waits)
	readyOnce sync.Once

	topics struct{ meta, state, status, event string }
}

// New builds a service on an open store.
func New(cfg config.Config, st *store.Store, log *slog.Logger) *Service {
	s := &Service{cfg: cfg, log: log, st: st, now: time.Now,
		jobs: make(chan func(), 1024), dirty: make(chan struct{}, 1), stop: make(chan struct{})}
	site, station, slot := cfg.MQTT.Site, cfg.Slot.Station, cfg.Slot.Slot
	s.topics.meta = schema.MetaTopic(site, station, slot)
	s.topics.state = schema.StateTopic(site, station, slot)
	s.topics.status = schema.StatusTopic(site, station, slot)
	s.topics.event = schema.SiblingTopic(site, station, slot, "event")
	s.ready = make(chan struct{})
	s.imp = &importer.Importer{Store: st, Window: cfg.Window(), DefaultOwn: cfg.DefaultOwnCall, Sink: s.emit, Log: log}
	if cfg.Watch.Dir != "" {
		s.watcher = &watch.Watcher{Dir: cfg.Watch.Dir, Interval: time.Duration(cfg.Watch.IntervalSeconds) * time.Second,
			Source: cfg.Watch.Source, Store: st, Importer: s.imp, Log: log, OnChange: s.poke, Ready: s.ready}
	}
	return s
}

// Importer is the merge path (the CLI uses a bare importer on the same store).
func (s *Service) Importer() *importer.Importer { return s.imp }

// SetPublisher sets the publish path; Run sets the MQTT one.
func (s *Service) SetPublisher(p Publisher) {
	s.pubMu.Lock()
	s.pub = p
	s.pubMu.Unlock()
}

func (s *Service) publisher() Publisher {
	s.pubMu.RLock()
	defer s.pubMu.RUnlock()
	return s.pub
}

// poke asks the state publisher for a fresh /state.
func (s *Service) poke() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

// emit queues the events of a committed import and pokes /state. Blocking on a
// full queue is the back-pressure of a big import; it ends with the service.
func (s *Service) emit(events []store.Event) {
	for _, ev := range events {
		b, err := json.Marshal(ev)
		if err != nil {
			s.log.Error("event marshal failed", "err", err)
			continue
		}
		job := func() {
			p := s.publisher()
			if p == nil || p.Publish(s.topics.event, 1, false, b) != nil {
				if s.dropped.Add(1) == 1 {
					s.log.Warn("QSO event not published (broker offline); consumers must reload from the read API")
				}
			}
		}
		select {
		case s.jobs <- job:
		case <-s.stop:
			return
		}
	}
	s.poke()
}

// Handler is the HTTP API.
func (s *Service) Handler() http.Handler {
	return (&httpapi.API{Store: s.st, Importer: s.imp, AllowFrom: config.Prefixes(s.cfg.HTTP.AllowFrom), Window: s.cfg.Window(), Log: s.log}).Handler()
}

// State is the retained /state.
type State struct {
	TS         string               `json:"ts"`
	QSOCount   int                  `json:"qso_count"`
	OwnCalls   []string             `json:"own_calls"`
	LastImport *importer.LastImport `json:"last_import"`
	Watch      *watch.Status        `json:"watch,omitempty"`
	Problems   []string             `json:"problems"`
}

// BuildState reads the current state.
func (s *Service) BuildState() (State, error) {
	n, err := s.st.Count()
	if err != nil {
		return State{}, err
	}
	own, err := s.st.OwnCalls()
	if err != nil {
		return State{}, err
	}
	st := State{TS: s.now().UTC().Format(time.RFC3339), QSOCount: n, OwnCalls: own, Problems: []string{}}
	if l, ok := s.imp.Last(); ok {
		st.LastImport = &l
	}
	if s.watcher != nil {
		w := s.watcher.Status()
		st.Watch = &w
		if w.Error != "" {
			st.Problems = append(st.Problems, "ADIF drop directory: "+w.Error)
		}
	}
	if d := s.dropped.Load(); d > 0 {
		st.Problems = append(st.Problems, fmt.Sprintf("%d QSO events were not published (broker offline); consumers must reload from the read API", d))
	}
	if e := s.l4Err.Load(); e != nil {
		st.Problems = append(st.Problems, "Log4OM listener failed: "+*e)
	}
	if n == 0 {
		st.Problems = append(st.Problems, "the log is empty; import an ADIF file (-import-adif or POST /import)")
	}
	return st, nil
}

// publishState publishes the retained /state when it changed (ignoring ts) or force is set.
func (s *Service) publishState(force bool) {
	st, err := s.BuildState()
	if err != nil {
		s.log.Warn("state unavailable", "err", err)
		return
	}
	ts := st.TS
	st.TS = ""
	cmp, _ := json.Marshal(st)
	if !force && slices.Equal(cmp, s.lastPub) {
		return
	}
	st.TS = ts
	b, _ := json.Marshal(st)
	p := s.publisher()
	if p == nil {
		return
	}
	if err := p.Publish(s.topics.state, 1, true, b); err != nil {
		s.log.Warn("state publish failed", "err", err)
		return
	}
	s.lastPub = cmp
}

func (s *Service) statePublisher(ctx context.Context) {
	t := time.NewTicker(heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.dirty:
		case <-t.C:
		}
		s.publishState(s.forcePub.Swap(false))
	}
}

func (s *Service) metaPayload() ([]byte, error) {
	return json.Marshal(map[string]any{
		"schema": "1.0",
		"role":   "log-store",
		"host":   s.cfg.Host,
		"device": map[string]any{"name": "Station log"},
		"capabilities": map[string]any{
			"imports":    []string{"adif"},
			"feeders":    s.feeders(),
			"event":      s.topics.event,
			"events":     []string{store.ActionAdded, store.ActionMerged, store.ActionDeleted},
			"dup_window": s.cfg.DupWindow,
			"http_api":   s.cfg.HTTP.Listen,
			"api":        []string{"/v1/stats", "/v1/worked", "/v1/qsos", "/import"},
		},
	})
}

func (s *Service) feeders() []string {
	out := []string{}
	if s.cfg.Log4OM.Listen != "" {
		out = append(out, "log4om")
	}
	if s.cfg.Watch.Dir != "" {
		out = append(out, "adif-drop")
	}
	return out
}

// Run serves until ctx ends.
func (s *Service) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	spawn := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	spawn(func() { sharedmqtt.RunJobs(ctx, s.jobs) })
	spawn(func() { s.statePublisher(ctx) })

	errCh := make(chan error, 2)
	srv := &http.Server{Addr: s.cfg.HTTP.Listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 10 * time.Minute, WriteTimeout: 10 * time.Minute}
	spawn(func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	})
	spawn(func() {
		<-ctx.Done()
		sctx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = srv.Shutdown(sctx)
	})
	if s.cfg.Log4OM.Listen != "" {
		l := &log4om.Listener{Addr: s.cfg.Log4OM.Listen, AllowFrom: config.Prefixes(s.cfg.Log4OM.AllowFrom), Log: s.log,
			Handle: func(ctx context.Context, text string) {
				if _, err := s.imp.Import(ctx, text, importer.Options{Source: log4om.Source}); err != nil {
					s.log.Warn("storing a Log4OM QSO failed", "err", err)
				}
			}}
		spawn(func() {
			if err := l.Run(ctx); err != nil {
				s.log.Error("Log4OM listener failed; QSOs from Log4OM are no longer received live", "err", err)
				msg := err.Error()
				s.l4Err.Store(&msg)
				s.poke()
			}
		})
	}

	if s.watcher != nil {
		if _, err := os.Stat(s.cfg.Watch.Dir); err != nil {
			s.log.Warn("ADIF drop directory does not exist (yet); watching anyway", "dir", s.cfg.Watch.Dir, "err", err)
		}
		spawn(func() { s.watcher.Run(ctx) })
	}

	var runErr error
	if err := s.connect(ctx); err != nil {
		runErr = err
	} else {
		s.readyOnce.Do(func() { close(s.ready) })
		select {
		case <-ctx.Done():
		case runErr = <-errCh:
		}
		s.shutdownBus()
	}
	close(s.stop)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	return runErr
}

// pahoPub adapts a paho client; a publish that is not acknowledged within the
// timeout is an error.
type pahoPub struct{ c pahomqtt.Client }

func (p pahoPub) Publish(topic string, qos byte, retained bool, payload []byte) error {
	if !p.c.IsConnected() {
		return errors.New("mqtt not connected")
	}
	t := p.c.Publish(topic, qos, retained, payload)
	if !t.WaitTimeout(10 * time.Second) {
		return errors.New("mqtt publish timed out")
	}
	return t.Error()
}

// connect opens the MQTT link: LWT on /status, birth sequence on every
// (re)connect. station-log subscribes to nothing.
func (s *Service) connect(ctx context.Context) error {
	o := pahomqtt.NewClientOptions()
	o.AddBroker(s.cfg.MQTT.Broker)
	id := s.cfg.MQTT.ClientID
	if id == "" {
		id = s.cfg.MQTT.Site + "-" + s.cfg.Slot.Station + "-" + s.cfg.Slot.Slot
	}
	o.SetClientID(id)
	if s.cfg.MQTT.User != "" {
		o.SetUsername(s.cfg.MQTT.User)
		o.SetPassword(s.cfg.MQTT.Password)
	}
	o.SetAutoReconnect(true)
	o.SetCleanSession(true)
	o.SetWill(s.topics.status, "offline", 1, true)
	o.OnConnect = func(c pahomqtt.Client) {
		s.log.Info("MQTT (re)connected", "broker", s.cfg.MQTT.Broker)
		c.Publish(s.topics.status, 1, true, []byte("online"))
		if meta, err := s.metaPayload(); err == nil {
			c.Publish(s.topics.meta, 1, true, meta)
		}
		s.forcePub.Store(true)
		s.poke() // republish the retained state after a broker restart
	}
	o.OnConnectionLost = func(_ pahomqtt.Client, err error) { s.log.Warn("MQTT connection lost", "err", err) }
	c := pahomqtt.NewClient(o)
	s.SetPublisher(pahoPub{c})
	if err := sharedmqtt.Connect(ctx, c); err != nil {
		return fmt.Errorf("mqtt connect: %w", err)
	}
	return nil
}

func (s *Service) shutdownBus() {
	if p, ok := s.publisher().(pahoPub); ok && p.c.IsConnected() {
		t := p.c.Publish(s.topics.status, 1, true, []byte("offline"))
		t.WaitTimeout(2 * time.Second)
		p.c.Disconnect(500)
	}
}
