// Package log4om receives the QSOs Log4OM 2 logs: an outbound UDP connection
// of type ADIF_MESSAGE sends one ADIF record per saved QSO. The datagram text
// goes to the importer unchanged, so a Log4OM QSO takes the same merge path
// as a file import. Log4OM sends no edits or deletes and does not resend
// lost datagrams.
package log4om

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
)

// Source tags the QSOs this feeder stores.
const Source = "log4om"

// Listener binds the UDP port and hands each datagram's text to Handle.
type Listener struct {
	Addr      string // host:port
	AllowFrom []netip.Prefix
	Log       *slog.Logger
	Handle    func(ctx context.Context, text string)
	// Ready, if set, receives the bound address once listening.
	Ready chan<- net.Addr
}

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
	pc, err := net.ListenPacket("udp", l.Addr)
	if err != nil {
		return err
	}
	defer pc.Close()
	if l.Ready != nil {
		l.Ready <- pc.LocalAddr()
	}
	go func() { <-ctx.Done(); pc.Close() }()
	buf := make([]byte, 65536)
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
		l.Handle(ctx, string(buf[:n]))
	}
}
