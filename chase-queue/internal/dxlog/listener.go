package dxlog

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Event is one parsed datagram on its way to the store.
type Event struct {
	Kind Kind
	QSO  QSO
	// Instance is the contest instance that was active when the datagram
	// arrived (0 when ListenerConfig.Instance is not set).
	Instance int64
}

// ListenerConfig configures a Listener.
type ListenerConfig struct {
	Listen    string         // host:port; required
	AllowFrom []netip.Prefix // senders accepted; empty accepts nothing
	// Active says whether contest mode is on and which contest name DXLog's
	// broadcasts must carry (compared case-insensitively). A datagram while
	// it reports off is dropped without a count.
	Active func() (name string, on bool)
	// Instance names the active contest instance, stamped on each event so a
	// batch buffered across a mode switch is not stored in the wrong one.
	Instance func() int64
	// RecvBuffer is the socket receive buffer to ask for; the kernel may
	// grant less (Linux caps it at net.core.rmem_max). Default 4 MiB.
	RecvBuffer int
	QueueLen   int           // events waiting for the writer; default 8192
	RetryMin   time.Duration // default 5s
	RetryMax   time.Duration // default 1m
}

// State is a snapshot for the UI.
type State struct {
	Listening        bool   `json:"listening"`
	LastError        string `json:"last_error"`
	AllowFrom        int    `json:"allow_from"`         // entries; 0 = nothing is accepted
	RecvBuffer       int    `json:"recv_buffer"`        // what the kernel granted, 0 = unknown
	RecvBufferWanted int    `json:"recv_buffer_wanted"` // what was asked for
	Received         int    `json:"received"`           // datagrams read
	Accepted         int    `json:"accepted"`           // QSO messages passed on
	Ignored          int    `json:"ignored"`            // other messages and parse failures
	DroppedHost      int    `json:"dropped_host"`       // sender not in allow_from
	DroppedContest   int    `json:"dropped_contest"`    // contestname differs from the active one
	DroppedQueue     int    `json:"dropped_queue"`      // the writer fell behind
	LastContestName  string `json:"last_contest_name"`  // the last name that did not match
	CompleteLogSeen  bool   `json:"complete_log_seen"`  // a contactreplace arrived since Reset
}

// Listener reads DXLog's UDP QSO broadcast.
type Listener struct {
	cfg ListenerConfig
	log *slog.Logger
	out chan Event

	mu sync.Mutex
	st State
}

// NewListener returns a Listener; Run starts it and Events delivers what it
// accepts.
func NewListener(cfg ListenerConfig, log *slog.Logger) *Listener {
	if cfg.RecvBuffer <= 0 {
		cfg.RecvBuffer = 4 << 20
	}
	if cfg.QueueLen <= 0 {
		cfg.QueueLen = 8192
	}
	if cfg.RetryMin <= 0 {
		cfg.RetryMin = 5 * time.Second
	}
	if cfg.RetryMax < cfg.RetryMin {
		cfg.RetryMax = max(time.Minute, cfg.RetryMin)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Listener{cfg: cfg, log: log.With("component", "dxlog"), out: make(chan Event, cfg.QueueLen),
		st: State{AllowFrom: len(cfg.AllowFrom), RecvBufferWanted: cfg.RecvBuffer}}
}

// Events delivers the accepted QSO messages. It is closed when Run returns.
func (l *Listener) Events() <-chan Event { return l.out }

// State returns a snapshot.
func (l *Listener) State() State {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.st
}

// ResetCompleteLog forgets that a complete-log broadcast arrived, for when
// contest mode is switched on.
func (l *Listener) ResetCompleteLog() {
	l.mu.Lock()
	l.st.CompleteLogSeen = false
	l.mu.Unlock()
}

func (l *Listener) bump(f func(*State)) {
	l.mu.Lock()
	f(&l.st)
	l.mu.Unlock()
}

func (l *Listener) allowed(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range l.cfg.AllowFrom {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Run listens until ctx is done. While the address cannot be bound it
// retries, with the error in State, so a listener configured on a LAN
// address comes up once the address is assigned.
func (l *Listener) Run(ctx context.Context) error {
	defer close(l.out)
	for ctx.Err() == nil {
		pc := l.bind(ctx)
		if pc == nil {
			return nil
		}
		l.serve(ctx, pc) // returns when ctx ends or the socket is broken; then bind again
	}
	return nil
}

// bind opens the socket, retrying with a backoff; nil when ctx ends first.
func (l *Listener) bind(ctx context.Context) *net.UDPConn {
	for wait := l.cfg.RetryMin; ; wait = min(2*wait, l.cfg.RetryMax) {
		var pc *net.UDPConn
		addr, err := net.ResolveUDPAddr("udp", l.cfg.Listen)
		if err == nil {
			pc, err = net.ListenUDP("udp", addr)
		}
		if err == nil {
			return pc
		}
		l.bump(func(s *State) { s.LastError = err.Error() })
		l.log.Warn("dxlog: cannot listen, retrying", "listen", l.cfg.Listen, "err", err, "in", wait)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// serve reads datagrams until ctx ends or reads keep failing (the interface
// went away): the socket is then closed so Run can bind a new one.
func (l *Listener) serve(ctx context.Context, pc *net.UDPConn) {
	_ = pc.SetReadBuffer(l.cfg.RecvBuffer)
	granted := recvBuffer(pc)
	l.bump(func(s *State) { s.Listening, s.LastError, s.RecvBuffer = true, "", granted })
	l.log.Info("dxlog listening", "addr", pc.LocalAddr().String(), "recv_buffer", granted, "wanted", l.cfg.RecvBuffer)
	if granted > 0 && granted < l.cfg.RecvBuffer/2 {
		l.log.Warn("dxlog: the kernel granted a smaller receive buffer than asked for; raise net.core.rmem_max for a lossless complete-log resend",
			"granted", granted, "wanted", l.cfg.RecvBuffer)
	}
	stop := context.AfterFunc(ctx, func() { pc.Close() })
	defer stop()
	defer pc.Close()
	defer l.bump(func(s *State) { s.Listening = false })

	buf := make([]byte, 64<<10)
	failures := 0
	for {
		n, from, err := pc.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			l.log.Warn("dxlog: read failed", "err", err)
			if failures++; failures >= 20 {
				l.bump(func(s *State) { s.LastError = err.Error() })
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		failures = 0
		l.handle(buf[:n], from.Addr())
	}
}

func (l *Listener) handle(b []byte, from netip.Addr) {
	l.bump(func(s *State) { s.Received++ })
	if !l.allowed(from) {
		l.bump(func(s *State) { s.DroppedHost++ })
		return
	}
	var want string
	if l.cfg.Active != nil {
		name, on := l.cfg.Active()
		if !on {
			return
		}
		want = name
	}
	kind, q, err := Parse(b)
	if err != nil || kind == KindIgnored {
		l.bump(func(s *State) { s.Ignored++ })
		return
	}
	if want != "" && !strings.EqualFold(q.ContestName, want) && !(kind == KindDelete && q.ContestName == "") {
		l.bump(func(s *State) { s.DroppedContest++; s.LastContestName = q.ContestName })
		return
	}
	ev := Event{Kind: kind, QSO: q}
	if l.cfg.Instance != nil {
		ev.Instance = l.cfg.Instance()
	}
	select {
	case l.out <- ev:
		l.bump(func(s *State) {
			s.Accepted++
			if kind == KindReplace {
				s.CompleteLogSeen = true
			}
		})
	default:
		l.bump(func(s *State) { s.DroppedQueue++ })
	}
}
