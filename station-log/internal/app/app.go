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
	"net/url"
	"os"
	"slices"
	"strings"
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
	"station-log/internal/wavelog"
	"station-log/internal/working"
	"station-log/internal/working/geo"
	"station-log/internal/working/qrz"
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

	jobs  chan func() // QSO event publishing, one worker
	wjobs chan func() // working-feed closures, its own worker: neither side can starve the other
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

	syncer *wavelog.Syncer // nil while [wavelog] is off

	feed    *working.Feed // nil while [working] has no listener
	wlisten []*working.Listener
	workErr sync.Map     // listener name -> failure text
	workUp  atomic.Int32 // listeners bound

	topics struct{ meta, state, status, event, working string }
}

// New builds a service on an open store.
func New(cfg config.Config, st *store.Store, log *slog.Logger) *Service {
	s := &Service{cfg: cfg, log: log, st: st, now: time.Now,
		jobs: make(chan func(), 1024), wjobs: make(chan func(), 1024), dirty: make(chan struct{}, 1), stop: make(chan struct{})}
	site, station, slot := cfg.MQTT.Site, cfg.Slot.Station, cfg.Slot.Slot
	s.topics.meta = schema.MetaTopic(site, station, slot)
	s.topics.state = schema.StateTopic(site, station, slot)
	s.topics.status = schema.StatusTopic(site, station, slot)
	s.topics.event = schema.SiblingTopic(site, station, slot, "event")
	s.topics.working = schema.SiblingTopic(site, station, slot, "working")
	s.ready = make(chan struct{})
	s.imp = &importer.Importer{Store: st, Window: cfg.Window(), DefaultOwn: cfg.DefaultOwnCall, Sink: s.emit, Log: log}
	if cfg.Watch.Dir != "" {
		s.watcher = &watch.Watcher{Dir: cfg.Watch.Dir, Interval: time.Duration(cfg.Watch.IntervalSeconds) * time.Second,
			Source: cfg.Watch.Source, Store: st, Importer: s.imp, Log: log, OnChange: s.poke, Ready: s.ready}
	}
	if cfg.Working.Enabled() {
		s.feed = s.newFeed()
		for _, lc := range cfg.Working.Listeners {
			lc := lc
			s.wlisten = append(s.wlisten, &working.Listener{Name: lc.Name, Kind: lc.Kind, Addr: lc.Listen,
				AllowFrom: config.Prefixes(lc.AllowFrom), Log: log,
				Handle: func(ctx context.Context, l *working.Listener, data []byte) { s.feed.Handle(ctx, l.Name, l.Kind, data) },
				OnFail: func(err error) {
					if prev, ok := s.workErr.Load(lc.Name); !ok || prev != err.Error() {
						s.workErr.Store(lc.Name, err.Error())
						s.poke()
					}
				},
				OnUp: func() {
					if _, ok := s.workErr.LoadAndDelete(lc.Name); ok {
						s.poke()
					}
				}})
		}
	}
	if cfg.Wavelog.Enabled() {
		s.syncer = s.newSyncer()
	}
	return s
}

// newSyncer builds the Wavelog sync; a client that cannot be built (bad
// ca_file) leaves the sync off with a Warn, never fatal for the log.
func (s *Service) newSyncer() *wavelog.Syncer {
	w := s.cfg.Wavelog
	cl, err := wavelog.NewClient(w.URL, w.Key, w.CAFile)
	if err != nil {
		s.log.Error("Wavelog sync disabled", "err", err)
		return nil
	}
	u, _ := url.Parse(w.URL)
	if u.Scheme == "http" {
		s.log.Warn("Wavelog URL is plain http: the API key travels unencrypted", "host", u.Host)
	}
	c := wavelog.Config{Pull: w.Pull(), Push: w.Push(), URLHost: u.Host, URL: w.URL,
		Interval: time.Duration(w.IntervalSeconds) * time.Second, FullResync: time.Duration(w.FullResyncHours) * time.Hour,
		Batch: w.BatchSize, PullLimit: w.PullLimit, PushSources: w.PushSources, PushSince: w.PushSinceTime(), Repush: w.RepushChanges}
	for _, p := range w.Profiles {
		c.Profiles = append(c.Profiles, wavelog.Profile{OwnCall: strings.ToUpper(strings.TrimSpace(p.OwnCall)), StationID: strings.TrimSpace(p.StationProfileID)})
	}
	return &wavelog.Syncer{Cfg: c, Client: cl, Store: s.st, Importer: s.imp, Log: s.log, OnChange: s.poke, Ready: s.ready}
}

// newFeed builds the working-station feed: station locator, own-log grid
// lookup, optional QRZ. The feed's closures run on the jobs worker; the UDP
// handlers only decode and enqueue.
func (s *Service) newFeed() *working.Feed {
	w := s.cfg.Working
	r := working.Resolver{Grid: func(call string) (string, bool) {
		g, err := s.st.GridForCall(call)
		return g, err == nil && g != ""
	}}
	if ll, ok := geo.LocatorToLatLng(w.StationLocator); ok {
		r.Station, r.HasStation = ll, true
	} else {
		s.log.Warn("working.station_locator not set: logger azimuth/distance will not resolve to map coordinates")
	}
	if w.QRZ.User != "" {
		cache := qrz.OpenCache(s.cfg.QRZCachePath(), time.Duration(w.QRZ.CacheDays)*24*time.Hour,
			time.Duration(w.QRZ.NegativeMinutes)*time.Minute, 0)
		r.QRZ = qrz.NewService(qrz.New(w.QRZ.User, w.QRZ.Password), cache)
		s.log.Info("working feed: qrz lookup enabled", "user", w.QRZ.User, "cache", s.cfg.QRZCachePath())
	}
	return &working.Feed{Resolver: r, StaleAfter: w.Stale(), Log: s.log,
		Enqueue:  func(f func()) { sharedmqtt.Enqueue(s.wjobs, f) },
		OnChange: s.poke,
		Publish: func(b []byte) error {
			p := s.publisher()
			if p == nil {
				return errors.New("no publisher")
			}
			return p.Publish(s.topics.working, 1, true, b)
		}}
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
	if s.syncer != nil {
		for _, ev := range events {
			if ev.QSO.Source != wavelog.Source {
				s.syncer.Kick() // a logger delivered something: push soon
				break
			}
		}
	}
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
	Working    *WorkingStatus       `json:"working,omitempty"`
	Wavelog    *wavelog.Status      `json:"wavelog,omitempty"`
	Problems   []string             `json:"problems"`
}

// WorkingStatus is the working feed's part of /state. It carries no
// per-keystroke data (the keyed call lives only on the working topic), so it
// does not make /state churn.
type WorkingStatus struct {
	Listening []string `json:"listening"`
	Online    bool     `json:"online"`
	QRZ       bool     `json:"qrz"`
	// Rejected counts datagrams from hosts outside allow_from (logged once per source).
	Rejected int64 `json:"rejected,omitempty"`
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
	if s.feed != nil {
		ws := &WorkingStatus{Listening: []string{}, Online: s.feed.Live(), QRZ: s.cfg.Working.QRZ.User != ""}
		for _, l := range s.cfg.Working.Listeners {
			if _, failed := s.workErr.Load(l.Name); !failed {
				ws.Listening = append(ws.Listening, l.Name)
			}
		}
		for _, l := range s.wlisten {
			ws.Rejected += l.Rejected()
		}
		st.Working = ws
		s.workErr.Range(func(k, v any) bool {
			st.Problems = append(st.Problems, fmt.Sprintf("working-station listener %s failed: %s", k, v))
			return true
		})
		if s.feed.QRZAuthFailed() {
			st.Problems = append(st.Problems, "QRZ lookup rejected the credentials (working.qrz.user / STATION_LOG_QRZ_PASSWORD)")
		}
	}
	if s.syncer != nil {
		w := s.syncer.Status()
		st.Wavelog = &w
		if w.Error != "" {
			st.Problems = append(st.Problems, "Wavelog: "+w.Error)
		}
		if w.PushFailed > 0 {
			st.Problems = append(st.Problems, fmt.Sprintf("Wavelog: %d QSOs failed to push permanently", w.PushFailed))
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
	caps := map[string]any{
		"imports":    []string{"adif"},
		"feeders":    s.feeders(),
		"event":      s.topics.event,
		"events":     []string{store.ActionAdded, store.ActionMerged, store.ActionDeleted},
		"dup_window": s.cfg.DupWindow,
		"http_api":   s.cfg.HTTP.Listen,
		"api":        []string{"/v1/stats", "/v1/worked", "/v1/qsos", "/import"},
	}
	if s.feed != nil {
		caps["working"] = s.topics.working
	}
	if s.syncer != nil && s.cfg.Wavelog.Push() {
		caps["sync"] = []string{"wavelog"}
	}
	return json.Marshal(map[string]any{
		"schema":       "1.0",
		"role":         "log-store",
		"host":         s.cfg.Host,
		"device":       map[string]any{"name": "Station log"},
		"capabilities": caps,
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
	if s.feed != nil {
		out = append(out, "logger-udp")
	}
	if s.syncer != nil && s.cfg.Wavelog.Pull() {
		out = append(out, "wavelog-pull")
	}
	return out
}

// Run serves until ctx ends.
func (s *Service) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	spawn := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	spawn(func() { sharedmqtt.RunJobs(ctx, s.jobs) })
	if s.feed != nil {
		spawn(func() { sharedmqtt.RunJobs(ctx, s.wjobs) })
	}
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

	if s.feed != nil {
		s.runWorking(ctx, spawn)
	}

	if s.syncer != nil {
		spawn(func() { s.syncer.Run(ctx) })
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

// runWorking starts the staleness watcher and one goroutine per UDP
// listener. A listener that cannot bind is a problem in /state, never fatal
// for the QSO store or the other feeders; it retries with capped backoff and
// the problem clears once it binds.
func (s *Service) runWorking(ctx context.Context, spawn func(func())) {
	spawn(func() { s.feed.Run(ctx, working.LivenessTick) })
	for _, l := range s.wlisten {
		l := l
		spawn(func() { _ = l.Run(ctx) })
	}
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
		if s.feed != nil {
			s.feed.Birth() // queued: this callback never publishes the document itself
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
