// Package log4om receives the QSOs Log4OM 2 logs: an outbound UDP connection
// of type ADIF_MESSAGE sends one ADIF record per saved QSO. Log4OM sends no
// edits or deletes and does not resend lost datagrams, so a QSO heard here
// only means "worked"; confirmations come from Club Log.
package log4om

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"time"

	"chase-queue/internal/adif"
	"chase-queue/internal/band"
	"chase-queue/internal/contest"
	"chase-queue/internal/store"
)

// QSOFromRecord converts an ADIF record; ok is false without a call and a
// band (BAND, or FREQ in MHz).
func QSOFromRecord(r adif.Record, now time.Time) (store.LogQSO, bool) {
	call := contest.NormalizeCall(r["CALL"])
	b := strings.ToLower(strings.TrimSpace(r["BAND"]))
	if !band.Known(b) {
		b = ""
		if f, err := parseMHz(r["FREQ"]); err == nil {
			if bb, ok := band.Of(f * 1e6); ok {
				b = bb.Name
			}
		}
	}
	if call == "" || b == "" {
		return store.LogQSO{}, false
	}
	at := now
	if d, err := time.Parse("20060102150405", r["QSO_DATE"]+pad6(r["TIME_ON"])); err == nil {
		at = d
	}
	return store.LogQSO{Source: store.SourceLog4OM, Call: call, Band: b, Mode: strings.ToUpper(r["MODE"]), At: at.UTC()}, true
}

func pad6(s string) string {
	for len(s) < 6 {
		s += "0"
	}
	return s[:6]
}

func parseMHz(s string) (float64, error) {
	var f float64
	_, err := fmtSscan(strings.TrimSpace(s), &f)
	return f, err
}

// Listener binds the UDP port and sends each parsed QSO to out.
type Listener struct {
	Addr      string // host:port
	AllowFrom []netip.Prefix
	Log       *slog.Logger
	Now       func() time.Time
	out       chan store.LogQSO
}

// New returns a listener on addr (host:port) that accepts datagrams from allow.
func New(addr string, allow []netip.Prefix, log *slog.Logger) *Listener {
	return &Listener{Addr: addr, AllowFrom: allow, Log: log, out: make(chan store.LogQSO, 256)}
}

// QSOs is the channel of parsed QSOs.
func (l *Listener) QSOs() <-chan store.LogQSO { return l.out }

func (l *Listener) allowed(a netip.Addr) bool {
	for _, p := range l.AllowFrom {
		if p.Contains(a.Unmap()) {
			return true
		}
	}
	return false
}

// Run reads datagrams until ctx ends. A bind failure is returned.
func (l *Listener) Run(ctx context.Context) error {
	if l.Now == nil {
		l.Now = time.Now
	}
	pc, err := net.ListenPacket("udp", l.Addr)
	if err != nil {
		return err
	}
	defer pc.Close()
	go func() { <-ctx.Done(); pc.Close() }()
	buf := make([]byte, 16384)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		ap, _ := netip.ParseAddrPort(from.String())
		if !l.allowed(ap.Addr()) {
			l.Log.Warn("log4om datagram from a host outside allow_from", "from", ap.Addr().String())
			continue
		}
		for _, r := range adif.Parse(string(buf[:n])) {
			if q, ok := QSOFromRecord(r, l.Now()); ok {
				select {
				case l.out <- q:
				default:
					l.Log.Warn("log4om queue full: QSO dropped")
				}
			}
		}
	}
}
