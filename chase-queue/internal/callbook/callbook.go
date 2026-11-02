// Package callbook adds callbook data (QRZ.com: name, QTH, locator,
// picture) to the callsigns on the list. Lookups run on one background
// goroutine, never on the publish path: Info answers from memory only and
// asks for a lookup when it has nothing; when a lookup lands, Changed is
// called so the publisher republishes. Results are cached in SQLite (found
// for refresh_days, not found for a day), so a restart costs no lookups.
package callbook

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"chase-queue/internal/bus"
	"chase-queue/internal/qrz"
	"chase-queue/internal/store"
)

const (
	notFoundTTL = 24 * time.Hour
	retryAfter  = 10 * time.Minute // a lookup that failed for a network reason
	authBackoff = time.Hour        // QRZ refused the login
	queueLen    = 64
)

var validCall = regexp.MustCompile(`^[A-Z0-9]{1,6}(/[A-Z0-9]{1,6}){0,2}$`)

// Looker is the callbook service (qrz.Client).
type Looker interface {
	Lookup(ctx context.Context, call string) (qrz.Record, error)
}

// Cache is where results are kept between restarts (store.Store).
type Cache interface {
	Callbook(call string) (store.CallbookEntry, bool, error)
	PutCallbook(store.CallbookEntry) error
}

// Config configures a Book.
type Config struct {
	Looker  Looker
	Cache   Cache
	TTL     time.Duration // how long a found entry is kept
	Changed func()        // a lookup landed: republish
	Now     func() time.Time
}

type entry struct {
	info    *bus.CallInfo // nil = not in the callbook
	fetched time.Time
	retry   time.Time // failed: not before
}

// Book is the in-memory callbook with its lookup worker.
type Book struct {
	cfg Config
	log *slog.Logger
	q   chan string

	mu      sync.Mutex
	mem     map[string]entry
	queued  map[string]bool
	authErr time.Time // last login refusal
	lastErr string
}

// New returns a Book; Run starts its worker.
func New(cfg Config, log *slog.Logger) *Book {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.TTL == 0 {
		cfg.TTL = 30 * 24 * time.Hour
	}
	return &Book{cfg: cfg, log: log, q: make(chan string, queueLen), mem: map[string]entry{}, queued: map[string]bool{}}
}

// Info returns what the callbook knows of call; with nothing fresh in
// memory it asks the worker for a lookup and returns nil.
func (b *Book) Info(call string) *bus.CallInfo {
	call = strings.ToUpper(strings.TrimSpace(call))
	if !validCall.MatchString(call) {
		return nil
	}
	now := b.cfg.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.mem[call]
	if ok && b.freshLocked(e, now) {
		return e.info
	}
	if ok && now.Before(e.retry) {
		return e.info
	}
	if !b.queued[call] {
		select {
		case b.q <- call:
			b.queued[call] = true
		default: // full: asked again on the next publish
		}
	}
	return e.info // a stale entry is still shown while it refreshes
}

func (b *Book) freshLocked(e entry, now time.Time) bool {
	if e.fetched.IsZero() {
		return false
	}
	ttl := b.cfg.TTL
	if e.info == nil {
		ttl = notFoundTTL
	}
	return now.Sub(e.fetched) < ttl
}

// Problem is what keeps the callbook from working ("" = fine).
func (b *Book) Problem() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.authErr.IsZero() && b.cfg.Now().Sub(b.authErr) < authBackoff {
		return "QRZ rejected the login: no callbook data"
	}
	return b.lastErr
}

// Run serves lookups until ctx ends.
func (b *Book) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case call := <-b.q:
			changed := b.resolve(ctx, call)
			b.mu.Lock()
			delete(b.queued, call)
			b.mu.Unlock()
			if changed && b.cfg.Changed != nil {
				b.cfg.Changed()
			}
		}
	}
}

// resolve fills the memory entry of call from the cache, else from QRZ. It
// reports whether the entry changed.
func (b *Book) resolve(ctx context.Context, call string) bool {
	now := b.cfg.Now()
	if b.cfg.Cache != nil {
		if c, ok, err := b.cfg.Cache.Callbook(call); err == nil && ok {
			e := entry{fetched: c.Fetched}
			if c.Found {
				var info bus.CallInfo
				if json.Unmarshal([]byte(c.Data), &info) == nil {
					e.info = &info
				}
			}
			b.mu.Lock()
			fresh := b.freshLocked(e, now)
			if _, had := b.mem[call]; fresh || !had {
				b.mem[call] = e // a stale entry is shown while QRZ is asked
			}
			b.mu.Unlock()
			if fresh {
				return true
			}
		}
	}
	b.mu.Lock()
	blocked := !b.authErr.IsZero() && now.Sub(b.authErr) < authBackoff
	b.mu.Unlock()
	if blocked {
		b.markRetry(call, b.authErr.Add(authBackoff))
		return false
	}
	info, err := b.lookup(ctx, call)
	switch {
	case err == nil || errors.Is(err, qrz.ErrNotFound):
		e := entry{info: info, fetched: now}
		b.mu.Lock()
		b.mem[call] = e
		b.lastErr = ""
		b.mu.Unlock()
		if b.cfg.Cache != nil {
			ce := store.CallbookEntry{Call: call, Found: info != nil, Fetched: now}
			if info != nil {
				d, _ := json.Marshal(info)
				ce.Data = string(d)
			}
			if err := b.cfg.Cache.PutCallbook(ce); err != nil {
				b.log.Warn("storing a callbook entry failed", "err", err)
			}
		}
		return true
	case errors.Is(err, qrz.ErrAuth):
		b.mu.Lock()
		first := b.authErr.IsZero() || now.Sub(b.authErr) >= authBackoff
		b.authErr = now
		b.mu.Unlock()
		if first {
			b.log.Error("QRZ rejected the login; retrying in an hour")
		}
		b.markRetry(call, now.Add(authBackoff))
		return true // the problem line changed
	default:
		if ctx.Err() != nil {
			return false
		}
		b.log.Warn("QRZ lookup failed", "call", call, "err", err)
		b.mu.Lock()
		b.lastErr = "QRZ lookups failing: " + err.Error()
		b.mu.Unlock()
		b.markRetry(call, now.Add(retryAfter))
		return true
	}
}

func (b *Book) markRetry(call string, until time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.mem[call]
	e.retry = until
	b.mem[call] = e
}

// lookup asks QRZ for call; a portable form the callbook does not have is
// retried under its home call (the longest part, the later one on a tie).
func (b *Book) lookup(ctx context.Context, call string) (*bus.CallInfo, error) {
	r, err := b.cfg.Looker.Lookup(ctx, call)
	if errors.Is(err, qrz.ErrNotFound) {
		if home := HomeCall(call); home != call {
			r, err = b.cfg.Looker.Lookup(ctx, home)
		}
	}
	if err != nil {
		return nil, err
	}
	info := &bus.CallInfo{Source: "qrz", Call: r.Call, Name: r.Name, QTH: r.City, State: r.State, Country: r.Country,
		Grid: r.Grid, Image: r.Image, CQZone: r.CQZone, ITUZone: r.ITUZone}
	if r.HasLL {
		lat, lon := r.Lat, r.Lon
		info.Lat, info.Lon = &lat, &lon
	}
	if info.Call == "" {
		info.Call = call
	}
	return info, nil
}

// HomeCall is the home callsign of a portable form: EA8/DL1ABC and
// DL1ABC/P are DL1ABC.
func HomeCall(call string) string {
	best := ""
	for p := range strings.SplitSeq(call, "/") {
		if len(p) >= len(best) {
			best = p
		}
	}
	if len(best) < 3 {
		return call
	}
	return best
}
