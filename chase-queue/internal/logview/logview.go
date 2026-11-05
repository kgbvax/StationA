// Package logview is chase-queue's derived view of the station log: which
// calls were worked on which band, in which mode class, and what is
// confirmed. station-log owns the QSOs; this package loads the worked rows
// from its HTTP API (GET /v1/worked) at start, after every MQTT reconnect and
// periodically, and keeps them current from its <slot>/event messages, so
// ranking never waits on a remote call. It never writes to station-log.
package logview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"codeberg.org/kgbvax/stationa/shared/logging"

	"chase-queue/internal/award"
	"chase-queue/internal/band"
	"chase-queue/internal/bus"
	"chase-queue/internal/contest"
	"chase-queue/internal/store"
)

const (
	maxBody = 128 << 20 // a worked list of a large log stays far below this
	maxSeen = 50000     // event ids kept for dedupe before the map is cleared
)

// Config configures a View.
type Config struct {
	// URL is station-log's HTTP API base, e.g. http://127.0.0.1:8096.
	URL string
	// Own is the active callsign; "" folds every own call of the log.
	Own string
	// Reload is the period of the full reload (default 5 minutes).
	Reload time.Duration
	// Retry is the wait after a failed load (default 30 seconds).
	Retry  time.Duration
	Client *http.Client
}

type rowKey struct{ call, band, class string }

// row is one call+band+mode_class of station-log's worked list.
type row struct {
	mode      string
	at        time.Time
	confirmed bool
	dxcc      int
	state     string
}

type workedResp struct {
	Worked []struct {
		Call      string    `json:"call"`
		Band      string    `json:"band"`
		ModeClass string    `json:"mode_class"`
		Mode      string    `json:"mode"`
		At        time.Time `json:"at"`
		Confirmed bool      `json:"confirmed"`
		DXCC      int       `json:"dxcc"`
		State     string    `json:"state"`
	} `json:"worked"`
}

// Event is the payload of station-log's <slot>/event.
type Event struct {
	ID     string `json:"id"`
	Action string `json:"action"` // added | merged | deleted
	QSO    struct {
		OwnCall   string    `json:"own_call"`
		Call      string    `json:"call"`
		Band      string    `json:"band"`
		Mode      string    `json:"mode"`
		ModeClass string    `json:"mode_class"`
		At        time.Time `json:"at"`
		DXCC      int       `json:"dxcc"`
		State     string    `json:"state"`
		Updated   time.Time `json:"updated"`
		Confirmed bool      `json:"confirmed"`
	} `json:"qso"`
}

// View is the log view. Safe for concurrent use.
type View struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	rows     map[rowKey]row
	byNorm   map[string][]rowKey // normalized call -> rows
	seen     map[string]evKey
	loaded   bool
	problem  string
	loading  bool
	buffered []Event // events that arrived while a load was in flight
	reloadAt bool    // a deleted event asked for a fresh load

	kick chan struct{}
	snap chan struct{} // coalesces Changed
}

// New returns a view with nothing loaded.
func New(cfg Config, log *slog.Logger) *View {
	if cfg.Reload <= 0 {
		cfg.Reload = 5 * time.Minute
	}
	if cfg.Retry <= 0 {
		cfg.Retry = 30 * time.Second
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 60 * time.Second}
	}
	cfg.Own = strings.ToUpper(strings.TrimSpace(cfg.Own))
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	return &View{cfg: cfg, log: log, rows: map[rowKey]row{}, byNorm: map[string][]rowKey{}, seen: map[string]evKey{},
		kick: make(chan struct{}, 1), snap: make(chan struct{}, 1)}
}

// Run loads the view and keeps it fresh until ctx ends. One failure streak
// logs one warning; the last good view stays in use meanwhile.
func (v *View) Run(ctx context.Context) {
	var streak logging.Streak
	for {
		wait := v.cfg.Reload
		if err := v.Load(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			streak.Fail(v.log, "station-log not reachable; the log view is not current", "err", err)
			wait = v.cfg.Retry
		} else {
			streak.Reset()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-v.kick:
		}
	}
}

// Reload asks Run for a fresh load now (after an MQTT reconnect, when events
// were missed).
func (v *View) Reload() {
	select {
	case v.kick <- struct{}{}:
	default:
	}
}

// Load fetches the worked list and replaces the view with it, then replays
// the events that arrived meanwhile.
func (v *View) Load(ctx context.Context) error {
	v.mu.Lock()
	v.loading, v.buffered, v.reloadAt = true, nil, false
	v.mu.Unlock()
	rows, err := v.fetch(ctx)
	v.mu.Lock()
	v.loading = false
	if err != nil {
		v.buffered = nil
		v.problem = "station-log not reachable: " + err.Error()
		v.mu.Unlock()
		v.changed()
		return err
	}
	v.rows, v.byNorm, v.seen = rows, map[string][]rowKey{}, map[string]evKey{}
	for k := range rows {
		n := contest.NormalizeCall(k.call)
		v.byNorm[n] = append(v.byNorm[n], k)
	}
	first := !v.loaded
	recovered := v.problem != ""
	v.loaded, v.problem = true, ""
	buf := v.buffered
	v.buffered = nil
	for _, ev := range buf {
		v.applyLocked(ev)
	}
	again := v.reloadAt
	n := len(v.rows)
	v.mu.Unlock()
	if first || recovered {
		v.log.Info("station-log view loaded", "worked_rows", n)
	}
	v.changed()
	if again {
		v.Reload()
	}
	return nil
}

func (v *View) fetch(ctx context.Context) (map[rowKey]row, error) {
	u := v.cfg.URL + "/v1/worked"
	if v.cfg.Own != "" {
		u += "?own=" + url.QueryEscape(v.cfg.Own)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.cfg.Client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL is not part of the message
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("GET /v1/worked: %s", resp.Status)
	}
	var w workedResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&w); err != nil {
		return nil, fmt.Errorf("GET /v1/worked: %w", err)
	}
	out := make(map[rowKey]row, len(w.Worked))
	for _, r := range w.Worked {
		call := strings.ToUpper(strings.TrimSpace(r.Call))
		if call == "" || r.Band == "" {
			continue
		}
		out[rowKey{call, r.Band, r.ModeClass}] = row{mode: strings.ToUpper(r.Mode), at: r.At, confirmed: r.Confirmed, dxcc: r.DXCC, state: strings.ToUpper(strings.TrimSpace(r.State))}
	}
	return out, nil
}

// Apply takes one MQTT event payload. It only touches memory, so the MQTT
// handler may call it directly. A malformed event is ignored.
func (v *View) Apply(payload []byte) {
	var ev Event
	if err := json.Unmarshal(payload, &ev); err != nil || ev.QSO.Call == "" {
		return
	}
	v.mu.Lock()
	if v.loading {
		v.buffered = append(v.buffered, ev)
	}
	ch := v.applyLocked(ev)
	v.mu.Unlock()
	if ch {
		v.changed()
	}
}

// evKey identifies an event's payload for the redelivery check.
type evKey struct {
	action    string
	updated   int64
	confirmed bool
	dxcc      int
	state     string
	at        int64
	mode      string
}

// applyLocked folds one event into the rows and reports whether they changed.
func (v *View) applyLocked(ev Event) bool {
	q := ev.QSO
	if v.cfg.Own != "" && !strings.EqualFold(q.OwnCall, v.cfg.Own) {
		return false
	}
	if ev.Action == "deleted" {
		// the row aggregates other QSOs of the same call, band and class, so
		// it cannot be edited here: take a fresh load
		if v.loading {
			v.reloadAt = true
		} else {
			defer v.Reload()
		}
		return false
	}
	if ev.Action != "added" && ev.Action != "merged" {
		return false
	}
	if ev.ID != "" {
		// Updated has whole-second resolution, so one import's added and
		// merged events of a QSO share it: a repeat is an event with the
		// same payload, not the same timestamp.
		k := evKey{ev.Action, q.Updated.UnixNano(), q.Confirmed, q.DXCC, strings.ToUpper(strings.TrimSpace(q.State)), q.At.UnixNano(), q.Mode}
		if last, ok := v.seen[ev.ID]; ok && last == k {
			return false
		}
		if len(v.seen) >= maxSeen {
			clear(v.seen)
		}
		v.seen[ev.ID] = k
	}
	k := rowKey{strings.ToUpper(strings.TrimSpace(q.Call)), q.Band, q.ModeClass}
	cur, ok := v.rows[k]
	if !ok {
		n := contest.NormalizeCall(k.call)
		v.byNorm[n] = append(v.byNorm[n], k)
	}
	nw := cur
	nw.confirmed = cur.confirmed || q.Confirmed
	if q.DXCC != 0 {
		nw.dxcc = q.DXCC
	}
	if s := strings.ToUpper(strings.TrimSpace(q.State)); s != "" {
		nw.state = s
	}
	if !ok || q.At.After(cur.at) {
		nw.at = q.At
		if m := strings.ToUpper(q.Mode); m != "" {
			nw.mode = m
		}
	}
	if ok && nw == cur {
		return false
	}
	v.rows[k] = nw
	return true
}

func (v *View) changed() {
	select {
	case v.snap <- struct{}{}:
	default:
	}
}

// Loaded reports whether a load has succeeded at least once.
func (v *View) Loaded() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.loaded
}

// Problem is "" while the last load succeeded, else what is wrong.
func (v *View) Problem() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.problem != "" && v.loaded {
		return v.problem + " (using the last loaded log)"
	}
	return v.problem
}

// Worked is every call worked on a band, any mode, by normalized call.
func (v *View) Worked() map[store.WorkedKey]bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make(map[store.WorkedKey]bool, len(v.rows))
	for k := range v.rows {
		out[store.WorkedKey{Call: contest.NormalizeCall(k.call), Band: k.band}] = true
	}
	return out
}

// Confirmed is every DXCC entity (ADIF number) with a confirmed QSO on a band,
// any mode.
func (v *View) Confirmed() []store.Confirmation {
	v.mu.Lock()
	defer v.mu.Unlock()
	set := map[store.Confirmation]bool{}
	for k, r := range v.rows {
		if r.confirmed && r.dxcc != 0 {
			set[store.Confirmation{DXCC: r.dxcc, Band: k.band}] = true
		}
	}
	out := make([]store.Confirmation, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	return out
}

// Entries is the view as award entries: one per call, band and mode class,
// with the entity, state and confirmation station-log holds for it.
func (v *View) Entries() []award.Entry {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]award.Entry, 0, len(v.rows))
	for k, r := range v.rows {
		out = append(out, award.Entry{Call: contest.NormalizeCall(k.call), DXCC: r.dxcc, State: r.state, Band: k.band,
			ModeClass: k.class, Confirmed: r.confirmed})
	}
	return out
}

// Summary is the worked-before of a call under its normalized form, nil when
// it was never worked. QSOs counts the call+band+mode-class entries of the
// log (station-log folds repeats of those). ThisBand is the caller's to set.
func (v *View) Summary(call string) *bus.WorkedBefore {
	v.mu.Lock()
	defer v.mu.Unlock()
	keys := v.byNorm[contest.NormalizeCall(call)]
	if len(keys) == 0 {
		return nil
	}
	w := &bus.WorkedBefore{}
	for _, k := range keys {
		r := v.rows[k]
		w.QSOs++
		if k.band != "" && !slices.Contains(w.Bands, k.band) {
			w.Bands = append(w.Bands, k.band)
		}
		if r.mode != "" && !slices.Contains(w.Modes, r.mode) {
			w.Modes = append(w.Modes, r.mode)
		}
		if r.at.After(w.Last) {
			w.Last, w.LastBand = r.at, k.band
		}
	}
	slices.SortFunc(w.Bands, func(a, b string) int { return bandIndex(a) - bandIndex(b) })
	slices.Sort(w.Modes)
	return w
}

// Changes is signalled (coalesced) whenever the view changed.
func (v *View) Changes() <-chan struct{} { return v.snap }

func bandIndex(name string) int {
	for i, b := range band.Bands {
		if b.Name == name {
			return i
		}
	}
	return len(band.Bands)
}
