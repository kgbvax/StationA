// Package app wires chase-queue together: the engine, the SQLite store, the
// history sources (DXLog, Log4OM, Club Log), the MQTT slot and the local HTTP
// API. All MQTT callbacks only enqueue work; one worker applies it, and one
// publisher turns the engine's state into the retained /state.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
	"codeberg.org/kgbvax/stationa/shared/schema"

	"chase-queue/internal/bus"
	"chase-queue/internal/callbook"
	"chase-queue/internal/calls"
	"chase-queue/internal/clublog"
	"chase-queue/internal/cluster"
	"chase-queue/internal/config"
	"chase-queue/internal/cty"
	"chase-queue/internal/dxlog"
	"chase-queue/internal/engine"
	"chase-queue/internal/log4om"
	"chase-queue/internal/qrz"
	"chase-queue/internal/store"
)

const heartbeat = 30 * time.Second

// Service is a running chase-queue.
type Service struct {
	cfg config.Config
	log *slog.Logger
	st  *store.Store
	eng *engine.Engine
	cty atomic.Pointer[cty.DB] // nil until cty.dat is there; replaced by a refresh
	now func() time.Time

	ranks *clublog.MostWanted
	dxl   *dxlog.Listener
	l4    *log4om.Listener
	cl    *clublog.Confirmations
	book  *callbook.Book  // nil without a QRZ login
	dxc   *cluster.Client // nil without a cluster login

	jobs  chan func() // /cmd work
	dirty chan struct{}

	candMu    sync.Mutex // the newest candidate state; older ones are dropped, never queued
	candRaw   []byte
	candSig   chan struct{}
	uploadSem chan struct{}
	clientMu  sync.Mutex

	switchMu      sync.Mutex
	mu            sync.Mutex // guards the fields below
	defName       string
	prefillN      int
	finderSeen    time.Time // when bmrec's candidates last arrived
	clState       string    // "", "loaded", "token rejected", "failed: ...", "no token"
	clLoaded      time.Time
	l4Seen        time.Time
	l4Err         string
	actID         atomic.Int64
	actName       atomic.Pointer[string]
	reloadPending atomic.Bool

	client   pahomqtt.Client
	topics   struct{ meta, state, status, cmd, finder, finderStatus string }
	lastPub  []byte      // publisher goroutine only
	forcePub atomic.Bool // set on (re)connect: publish even if unchanged
}

// New builds a service on an open store.
func New(cfg config.Config, st *store.Store, log *slog.Logger) *Service {
	s := &Service{cfg: cfg, log: log, st: st, now: time.Now,
		jobs: make(chan func(), 64), dirty: make(chan struct{}, 1), candSig: make(chan struct{}, 1), uploadSem: make(chan struct{}, 1)}
	db, err := cty.OpenContest(cfg.DataDir)
	if err != nil {
		log.Warn("cty.dat unusable; entities read as unknown (put cty.dat and cty.csv in data_dir)", "err", err)
	}
	s.cty.Store(db)
	s.ranks = clublog.New("", st, log)
	s.eng = engine.New(engine.Config{Store: st, CTY: db, Rank: s.ranks.Rank})
	site := cfg.MQTT.Site
	s.topics.meta = schema.MetaTopic(site, cfg.Slot.Station, cfg.Slot.Slot)
	s.topics.state = schema.StateTopic(site, cfg.Slot.Station, cfg.Slot.Slot)
	s.topics.status = schema.StatusTopic(site, cfg.Slot.Station, cfg.Slot.Slot)
	s.topics.cmd = schema.CmdTopic(site, cfg.Slot.Station, cfg.Slot.Slot)
	s.topics.finder = schema.StateTopic(site, cfg.Finder.Station, cfg.Finder.Slot)
	s.topics.finderStatus = schema.StatusTopic(site, cfg.Finder.Station, cfg.Finder.Slot)
	if cfg.Contest.Listen != "" {
		s.dxl = dxlog.NewListener(dxlog.ListenerConfig{Listen: cfg.Contest.Listen, AllowFrom: config.Prefixes(cfg.Contest.AllowFrom),
			Instance: func() int64 { return s.actID.Load() },
			Active: func() (string, bool) {
				p := s.actName.Load()
				if p == nil || s.actID.Load() == 0 {
					return "", false
				}
				return *p, true
			}}, log)
	}
	if cfg.Log4OM.Listen != "" {
		s.l4 = log4om.New(cfg.Log4OM.Listen, config.Prefixes(cfg.Log4OM.AllowFrom), log)
	}
	if cfg.Cluster.Address != "" {
		s.dxc = &cluster.Client{Addr: cfg.Cluster.Address, Call: cfg.Cluster.Call}
	}
	if cfg.QRZ.Enabled() {
		s.book = callbook.New(callbook.Config{
			Looker:  &qrz.Client{User: cfg.QRZ.User, Password: cfg.QRZ.Password, Agent: "chase-queue"},
			Cache:   st,
			TTL:     time.Duration(cfg.QRZ.RefreshDays) * 24 * time.Hour,
			Changed: s.poke,
		}, log)
	}
	if cfg.ClubLog.Token != "" {
		s.cl = &clublog.Confirmations{Token: cfg.ClubLog.Token, Call: cfg.ClubLog.Call, Confirmed: cfg.ClubLog.Confirmed}
	} else {
		s.clState = "no token"
	}
	return s
}

// poke asks the publisher for a fresh /state.
func (s *Service) poke() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

// Run serves until ctx ends.
func (s *Service) Run(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Join(s.cfg.DataDir, contestDirName), 0o750); err != nil {
		return err
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	spawn := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }

	// restore history, then start the sources
	if err := s.loadHistory(); err != nil {
		return err
	}
	s.resumeActive()
	go sharedmqtt.RunJobs(ctx, s.jobs)
	spawn(func() { s.ranks.Run(ctx) })
	spawn(func() { s.keep(ctx.Done()) })
	if s.dxl != nil {
		spawn(func() { _ = s.dxl.Run(ctx) })
		spawn(func() {
			dxlog.Write(ctx, s.dxl.Events(), dxlog.WriterConfig{Store: s.st,
				Contest: func() int64 { return s.actID.Load() }, Changed: s.reloadQSOs}, s.log)
		})
	}
	errCh := make(chan error, 2)
	if s.l4 != nil {
		spawn(func() {
			if err := s.l4.Run(ctx); err != nil {
				s.log.Error("Log4OM listener failed; worked stations are no longer tracked live", "err", err)
				s.setL4Error(err.Error())
				s.poke()
			}
		})
		spawn(func() { s.consumeLog4OM(ctx) })
	}
	if s.cl != nil {
		spawn(func() { s.refreshConfirmations(ctx) })
	}
	if s.book != nil {
		spawn(func() { s.book.Run(ctx) })
	}
	if s.dxc != nil {
		spawn(func() { s.dxc.Run(ctx, s.onClusterSpot) })
	}
	spawn(func() { s.refreshCTY(ctx) })
	spawn(func() { s.publisher(ctx) })
	spawn(func() { s.candidateWorker(ctx) })

	srv := &http.Server{Addr: s.cfg.HTTP.Listen, Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute, WriteTimeout: time.Minute}
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

	if err := s.connect(ctx); err != nil {
		return err
	}
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errCh:
	}
	s.shutdownBus()
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	return runErr
}

// loadHistory restores the worked log, confirmations and DX skips.
func (s *Service) loadHistory() error {
	w, err := s.st.WorkedSet()
	if err != nil {
		return err
	}
	s.eng.SetDXWorked(w)
	if cs, err := s.st.Confirmations(); err == nil && len(cs) > 0 {
		s.eng.SetConfirmations(cs)
		s.mu.Lock()
		s.clState = "stored"
		s.mu.Unlock()
	}
	if ks, err := s.st.DXSkips(s.now()); err == nil {
		s.eng.SetDXSkips(ks)
	}
	return nil
}

func (s *Service) consumeLog4OM(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case q := <-s.l4.QSOs():
			if err := s.st.AddLogQSOs([]store.LogQSO{q}); err != nil {
				s.log.Warn("storing a Log4OM QSO failed; it counts as worked until restart", "err", err)
			}
			s.eng.AddDXWorked(q.Call, q.Band)
			s.mu.Lock()
			s.l4Seen = s.now()
			s.mu.Unlock()
			s.poke()
		}
	}
}

// refreshConfirmations fetches Club Log's confirmations now and every
// refresh_minutes. A rejected token stops the retries.
func (s *Service) refreshConfirmations(ctx context.Context) {
	every := time.Duration(s.cfg.ClubLog.RefreshMinutes) * time.Minute
	for {
		wait := every
		cs, err := s.cl.Fetch(ctx)
		var rl *clublog.RateLimited
		switch {
		case err == nil:
			if err := s.st.ReplaceConfirmations(cs); err != nil {
				s.log.Warn("storing Club Log confirmations failed", "err", err)
			}
			s.eng.SetConfirmations(cs)
			s.mu.Lock()
			s.clState, s.clLoaded = "loaded", s.now()
			s.mu.Unlock()
			s.log.Info("club log confirmations loaded", "cells", len(cs))
		case errors.Is(err, clublog.ErrAuth):
			s.setCL("token rejected")
			s.log.Error("Club Log rejected the token; retrying in an hour")
			wait = time.Hour
		case errors.As(err, &rl):
			wait = rl.Wait
			s.setCL("failed: rate limited")
		default:
			if ctx.Err() != nil {
				return
			}
			s.setCL("failed")
			wait = 10 * time.Minute
			s.log.Warn("club log fetch failed", "err", err)
		}
		s.poke()
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// ctyRefresh is how often AD1C's country files are fetched.
const ctyRefresh = 24 * time.Hour

// refreshCTY keeps cty.dat and cty.csv current: now, then daily. New files
// take effect at once for DX chasing; a running contest instance keeps its
// model until it is activated again.
func (s *Service) refreshCTY(ctx context.Context) {
	f := cty.Fetcher{}
	for {
		wait := ctyRefresh
		changed, err := f.Refresh(ctx, s.cfg.DataDir)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("refreshing the country files failed; keeping the current ones", "err", err)
			wait = time.Hour
		case changed:
			db, err := cty.OpenContest(s.cfg.DataDir)
			if err != nil || db == nil {
				s.log.Warn("the refreshed country files did not load", "err", err)
				wait = time.Hour
				break
			}
			s.cty.Store(db)
			s.eng.SetCTY(db)
			s.log.Info("country files refreshed")
			s.poke()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (s *Service) setL4Error(v string) { s.mu.Lock(); s.l4Err = v; s.mu.Unlock() }

func (s *Service) setCL(v string) { s.mu.Lock(); s.clState = v; s.mu.Unlock() }

// contestStatus is the setup panel's status block (keys the bmrec UI reads).
func (s *Service) contestStatus() map[string]any {
	s.mu.Lock()
	st := map[string]any{"definition": s.defName, "prefill_entries": s.prefillN}
	s.mu.Unlock()
	if age, ok := s.ranks.Age(); ok {
		st["ranks_age_s"] = int(age / time.Second)
	}
	st["ranks_loaded"] = s.ranks.Loaded()
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cty.csv")); err != nil {
		st["cty_numbers_missing"] = true
	}
	if s.dxl != nil {
		st["listener"] = s.dxl.State()
	}
	return st
}

// problems lists what keeps the ranking from being right, worst first.
func (s *Service) problems(st bus.ChaseState) []string {
	var out []string
	s.mu.Lock()
	seen, cl := s.finderSeen, s.clState
	s.mu.Unlock()
	if seen.IsZero() || s.now().Sub(seen) > 2*time.Minute {
		out = append(out, "No candidates from bmrec")
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cty.csv")); err != nil || s.cty.Load() == nil {
		out = append(out, "cty.dat or cty.csv missing: entities cannot be told")
	}
	if st.Activity == bus.ActivityDX {
		switch cl {
		case "no token":
			out = append(out, "Club Log: no token configured")
		case "token rejected":
			out = append(out, "Club Log rejected the token")
		case "", "failed", "failed: rate limited":
			if !s.eng.ConfirmationsLoaded() {
				out = append(out, "Club Log confirmations not loaded yet")
			}
		}
		s.mu.Lock()
		l4Err := s.l4Err
		s.mu.Unlock()
		switch {
		case s.l4 == nil:
			out = append(out, "Log4OM feed not configured: worked stations are not tracked live")
		case l4Err != "":
			out = append(out, "Log4OM feed not listening: "+l4Err)
		}
	} else if s.dxl == nil {
		out = append(out, "No DXLog feed configured: the contest log is not visible")
	} else if cs := s.dxl.State(); !cs.Listening {
		out = append(out, "DXLog feed not listening: "+cs.LastError)
	} else if st.Contest != nil && st.Contest.QSOs == 0 {
		out = append(out, "No QSOs from DXLog yet")
	}
	if !s.ranks.Loaded() {
		out = append(out, "Club Log Most Wanted not loaded: no rarity order")
	}
	if s.dxc != nil {
		if cs := s.dxc.State(); !cs.Connected && s.now().Sub(cs.Since) > time.Minute {
			p := "DX cluster not connected"
			if cs.LastError != "" {
				p += ": " + cs.LastError
			}
			out = append(out, p)
		}
	}
	if s.book != nil {
		if p := s.book.Problem(); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// compute builds the state to publish.
func (s *Service) compute() bus.ChaseState {
	st := s.eng.Compute()
	s.enrich(&st)
	st.TS = s.now().UTC()
	st.Problems = s.problems(st)
	return st
}

// onClusterSpot takes one spot of the cluster login. Skimmer spots (unless
// configured) and the operator's own spots are dropped.
func (s *Service) onClusterSpot(sp cluster.Spot) {
	if sp.Skimmer() && !s.cfg.Cluster.Skimmer {
		return
	}
	if calls.SameStation(strings.TrimSuffix(sp.Spotter, "-#"), s.cfg.Cluster.Call) {
		return
	}
	s.eng.AddClusterSpot(engine.ClusterSpot{Call: sp.Call, Band: sp.Band, Spotter: sp.Spotter, Comment: sp.Comment, FreqHz: sp.FreqHz, At: sp.At})
	s.poke()
}

// enrich adds the callbook entry and the worked-before history to each
// listed call. Callbook data comes from memory only (a missing entry is
// looked up in the background and republished when it lands).
func (s *Service) enrich(st *bus.ChaseState) {
	var calls []string
	for _, r := range st.Ranked {
		if r.Call != "" {
			calls = append(calls, r.Call)
		}
	}
	worked, err := s.st.WorkedBefore(calls)
	if err != nil {
		s.log.Warn("reading worked-before failed", "err", err)
	}
	for i := range st.Ranked {
		r := &st.Ranked[i]
		if r.Call == "" {
			continue
		}
		if w := worked[r.Call]; w != nil {
			c := *w
			c.ThisBand = slices.Contains(c.Bands, r.Band)
			r.Worked = &c
		}
		if s.book != nil {
			r.Info = s.book.Info(r.Call)
		}
	}
}

// publisher publishes /state when something changed and every heartbeat.
func (s *Service) publisher(ctx context.Context) {
	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	publish := func(force bool) {
		st := s.compute()
		cmp := st
		cmp.TS = time.Time{}
		key, _ := json.Marshal(cmp)
		if s.forcePub.Swap(false) {
			force = true
		}
		if !force && string(key) == string(s.lastPub) {
			return
		}
		b, err := json.Marshal(st)
		if err != nil {
			s.log.Warn("state marshal failed", "err", err)
			return
		}
		if s.publishState(b) {
			s.lastPub = key
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.dirty:
			time.Sleep(100 * time.Millisecond) // coalesce bursts
			select {
			case <-s.dirty:
			default:
			}
			publish(false)
		case <-tick.C:
			publish(true)
		}
	}
}
