package working

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"codeberg.org/kgbvax/stationa/shared/logging"
)

// Bind retry backoff bounds (capped exponential).
const (
	DefaultMinBackoff = time.Second
	DefaultMaxBackoff = 30 * time.Second
	// maxRejectedSources bounds the per-source memory (UDP sources can be spoofed).
	maxRejectedSources = 1024
)

// Listener binds one UDP port and hands each allowed datagram (a copy) to Handle.
type Listener struct {
	Name      string // source name ("dxlog", "log4om")
	Kind      string // "n1mm" | "log4om"
	Addr      string // host:port
	AllowFrom []netip.Prefix
	Log       *slog.Logger
	Handle    func(ctx context.Context, l *Listener, data []byte)
	// Ready, if set, receives the bound address on every successful bind.
	Ready chan<- net.Addr
	// OnFail is called with the bind or read error before each retry;
	// OnUp after a successful bind (clears the failure). Any goroutine.
	OnFail func(err error)
	OnUp   func()
	// MinBackoff / MaxBackoff bound the retry delay; zero = defaults (1 s .. 30 s).
	MinBackoff, MaxBackoff time.Duration

	rejected atomic.Int64
	seenMu   sync.Mutex
	seen     map[netip.Addr]struct{}
}

// Rejected counts datagrams dropped for coming from a host outside allow_from.
func (l *Listener) Rejected() int64 { return l.rejected.Load() }

func (l *Listener) allowed(a netip.Addr) bool {
	for _, p := range l.AllowFrom {
		if p.Contains(a.Unmap()) {
			return true
		}
	}
	return false
}

// reject counts a refused datagram and logs the first one per source address.
func (l *Listener) reject(a netip.Addr) {
	l.rejected.Add(1)
	l.seenMu.Lock()
	if l.seen == nil {
		l.seen = map[netip.Addr]struct{}{}
	}
	_, dup := l.seen[a]
	if !dup && len(l.seen) < maxRejectedSources {
		l.seen[a] = struct{}{}
	}
	l.seenMu.Unlock()
	if !dup {
		l.Log.Warn("working-station datagram from a host outside allow_from (further ones from this host are counted, not logged)",
			"listener", l.Name, "from", a.String())
	}
}

// Run serves until ctx ends, retrying a failed bind or read with capped
// exponential backoff (one Warn per outage). It returns nil on ctx cancel.
func (l *Listener) Run(ctx context.Context) error {
	minB, maxB := l.MinBackoff, l.MaxBackoff
	if minB <= 0 {
		minB = DefaultMinBackoff
	}
	if maxB <= 0 {
		maxB = DefaultMaxBackoff
	}
	var streak logging.Streak
	backoff := minB
	for {
		healthy, err := l.serve(ctx, &streak)
		if ctx.Err() != nil {
			return nil
		}
		if healthy {
			backoff = minB
		}
		if l.OnFail != nil {
			l.OnFail(err)
		}
		streak.Fail(l.Log, "working-station listener failed; the logger feed is not received, retrying",
			"listener", l.Name, "addr", l.Addr, "err", err, "retry_in", backoff.String())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxB {
			backoff = maxB
		}
	}
}

// serve binds and reads until an error. healthy reports whether a datagram
// was read (the outage, if any, is over).
func (l *Listener) serve(ctx context.Context, streak *logging.Streak) (healthy bool, err error) {
	pc, err := net.ListenPacket("udp", l.Addr)
	if err != nil {
		return false, err
	}
	defer pc.Close()
	l.Log.Info("working-station listener up", "listener", l.Name, "kind", l.Kind, "addr", pc.LocalAddr().String())
	if l.OnUp != nil {
		l.OnUp()
	}
	if l.Ready != nil {
		select {
		case l.Ready <- pc.LocalAddr():
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			pc.Close()
		case <-stop:
		}
	}()
	buf := make([]byte, 65536)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return healthy, err
		}
		if !healthy {
			healthy = true
			streak.Reset()
		}
		ap, _ := netip.ParseAddrPort(from.String())
		if !l.allowed(ap.Addr()) {
			l.reject(ap.Addr().Unmap())
			continue
		}
		l.Handle(ctx, l, append([]byte(nil), buf[:n]...))
	}
}
