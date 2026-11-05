package working

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"station-log/internal/working/callsign"
	"station-log/internal/working/n1mm"
	"station-log/internal/working/qrz"
)

// Kinds of listener, selecting the decoder.
const (
	KindN1MM   = "n1mm"
	KindLog4OM = "log4om"
)

// LivenessTick is how often the staleness watcher checks the feed.
const LivenessTick = 5 * time.Second

// Feed owns the working-station document. Listener goroutines call Handle
// (decode inline, queue the rest); everything that touches the document or
// publishes runs on the jobs worker via Enqueue, never on a UDP or paho
// goroutine.
type Feed struct {
	Resolver   Resolver
	StaleAfter time.Duration
	Log        *slog.Logger
	// Enqueue queues a closure on the jobs worker (shared/mqtt.Enqueue).
	Enqueue func(func())
	// Publish writes the retained document (jobs worker only).
	Publish func(payload []byte) error
	// OnChange is called (any goroutine) when the status shown in /state moved.
	OnChange func()
	Now      func() time.Time

	mu        sync.Mutex // clock + status
	lastHeard time.Time
	published bool // last device_online handed to the worker
	qrzAuth   bool // QRZ rejected the credentials, no success since

	// worker-only state
	cur      *Selected
	lastJSON []byte
	online   bool
	pubFail  bool
}

func (f *Feed) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Feed) touch() {
	f.mu.Lock()
	f.lastHeard = f.now()
	f.mu.Unlock()
}

// Live reports whether a logger was heard within StaleAfter; false until the
// first datagram since start.
func (f *Feed) Live() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.liveLocked()
}

func (f *Feed) liveLocked() bool {
	if f.lastHeard.IsZero() {
		return false
	}
	return f.now().Sub(f.lastHeard) <= f.StaleAfter
}

// QRZAuthFailed reports whether the last QRZ lookup was refused for credentials.
func (f *Feed) QRZAuthFailed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.qrzAuth
}

// Handle decodes one datagram of the given kind and queues the document
// update. Every datagram counts as logger liveness; every decodable one
// republishes with a fresh ts (no dedup on the selection path: the console
// re-brightens its marker off the ts). ctx bounds the QRZ work.
func (f *Feed) Handle(ctx context.Context, name, kind string, data []byte) {
	f.touch()
	res := f.Resolver
	if res.Now == nil {
		res.Now = f.now
	}
	switch kind {
	case KindN1MM:
		// contactinfo (QSO logged) is not a clear (the operator often stays on
		// the call) and RadioInfo is radio context the bus has elsewhere.
		root := n1mm.RootName(data)
		if got := n1mm.Classify(root); got != n1mm.KindLookupInfo {
			if got == n1mm.KindOther {
				f.Log.Debug("unrecognized datagram", "listener", name, "root", root, "raw", string(data))
			}
			return
		}
		li, err := n1mm.DecodeLookupInfo(data)
		if err != nil {
			f.Log.Warn("bad lookupinfo", "listener", name, "err", err)
			return
		}
		f.Log.Debug("lookupinfo", "listener", name, "call", li.Call, "reason", li.Reason, "az", li.Azimuth, "dist_km", li.DistanceKm)
		f.Enqueue(func() { f.apply(f.enrich(ctx, res.FromN1MM(li, name))) })
	case KindLog4OM:
		c, err := callsign.DecodeCallsign(data)
		if err != nil {
			f.Log.Debug("log4om datagram not decoded", "listener", name, "root", callsign.RootName(data), "err", err, "raw", string(data))
			return
		}
		f.Log.Debug("callsign", "listener", name, "call", c.Call)
		f.Enqueue(func() { f.apply(f.enrich(ctx, res.FromCallsign(c, name))) })
	}
}

// enrich runs the gap-fill on the jobs worker. A QRZ stall is bounded
// (3x the request timeout) and never blocks publication of the selection.
func (f *Feed) enrich(ctx context.Context, sel *Selected) *Selected {
	if sel == nil {
		return nil
	}
	if f.Resolver.QRZ != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 3*qrz.RequestTimeout)
		defer cancel()
	}
	sel, err := f.Resolver.Enrich(ctx, sel)
	switch {
	case err == nil:
		f.setQRZAuth(false)
	case errors.Is(err, qrz.ErrNotFound):
		f.Log.Debug("qrz lookup: call unknown", "call", sel.Call)
	case errors.Is(err, qrz.ErrAuth):
		if !f.QRZAuthFailed() {
			f.Log.Warn("qrz lookup: authentication failed; check the subscription, working.qrz.user and STATION_LOG_QRZ_PASSWORD")
		}
		f.setQRZAuth(true)
	default:
		f.Log.Warn("qrz lookup failed", "call", sel.Call, "err", err)
	}
	return sel
}

func (f *Feed) setQRZAuth(v bool) {
	f.mu.Lock()
	changed := f.qrzAuth != v
	f.qrzAuth = v
	f.mu.Unlock()
	if changed && f.OnChange != nil {
		f.OnChange()
	}
}

func (f *Feed) doc(sel *Selected, online bool) []byte {
	b, _ := json.Marshal(State{TS: f.now().UTC().Format(time.RFC3339), DeviceOnline: online, Selected: sel})
	return b
}

func (f *Feed) send(b []byte) {
	f.lastJSON = b
	if f.Publish == nil {
		return
	}
	if err := f.Publish(b); err != nil {
		if !f.pubFail {
			f.pubFail = true
			f.Log.Warn("working-station publish failed (the next datagram or reconnect re-announces it)", "err", err)
		}
		return
	}
	f.pubFail = false
}

// apply publishes a new selection (nil = cleared). Jobs worker only.
func (f *Feed) apply(sel *Selected) {
	f.cur = sel
	f.online = f.Live()
	f.mu.Lock()
	f.published = f.online
	f.mu.Unlock()
	f.send(f.doc(sel, f.online))
	if sel == nil {
		f.Log.Info("working station cleared")
		return
	}
	f.Log.Info("working station published", "call", sel.Call, "band", sel.Band, "mode", sel.Mode,
		"az", sel.Azimuth, "dist_km", sel.DistanceKm, "lat", sel.Lat, "lng", sel.Lng, "source", sel.Source)
}

// setOnline flips device_online for the staleness watcher; an unchanged
// value publishes nothing. Jobs worker only.
func (f *Feed) setOnline(live bool) {
	f.mu.Lock()
	f.published = live
	f.mu.Unlock()
	if live == f.online && f.lastJSON != nil {
		return
	}
	f.online = live
	f.send(f.doc(f.cur, live))
	f.Log.Info("working feed device_online", "online", live)
	if f.OnChange != nil {
		f.OnChange()
	}
}

// Birth (re)publishes the document on (re)connect: the last one verbatim, or
// device_online false when nothing was seen since start, which also replaces
// a stale retained document of a previous run. Safe from any goroutine.
func (f *Feed) Birth() {
	f.Enqueue(func() {
		if f.lastJSON == nil {
			f.online = false
			f.send(f.doc(nil, false))
			return
		}
		f.send(f.lastJSON)
	})
}

// Run is the staleness watcher: it queues a device_online flip when the UDP
// feed goes quiet or comes back. tick <= 0 means LivenessTick.
func (f *Feed) Run(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = LivenessTick
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.mu.Lock()
			live := f.liveLocked()
			differs := live != f.published
			f.mu.Unlock()
			if differs {
				f.Enqueue(func() { f.setOnline(live) })
			}
		}
	}
}
