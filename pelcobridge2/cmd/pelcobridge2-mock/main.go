// pelcobridge2-mock serves a canned PTS-303Z/3050DZ head (internal/simhead)
// over a REAL serial port or TCP socket, for bench smoke tests without the
// hardware. The quirks the engine relies on — silence-required absolute sets,
// garbage readback while a motor runs — are all on by default, so a loopback
// exercise behaves like the bench link.
//
// Typical use with socat (macOS/Linux):
//
//	socat -d -d pty,raw,echo=0 pty,raw,echo=0     # prints both pty names
//	pelcobridge2-mock -pty /dev/ttys013
//	pelcobridge2 -port /dev/ttys014
//
// Or over TCP (Windows: no pty needed):
//
//	pelcobridge2-mock -listen :4001
//	pelcobridge2 -port tcp:127.0.0.1:4001
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"

	"pelcobridge2/internal/serialio"
	"pelcobridge2/internal/simhead"
)

func main() {
	var (
		pty       = flag.String("pty", "", "pty/serial port to serve the head on")
		listen    = flag.String("listen", "", "TCP address to serve the head on (alternative to -pty)")
		baud      = flag.Int("baud", 2400, "baud for the -pty port (ignored by ptys)")
		addr      = flag.Int("addr", 1, "head's Pelco address")
		pan       = flag.Float64("pan", 100, "initial pan in degrees")
		tilt      = flag.Float64("tilt", 30, "initial tilt in degrees")
		rateAz    = flag.Float64("rate-az", 0, "azimuth slew °/s (0 = simhead default)")
		rateEl    = flag.Float64("rate-el", 0, "elevation slew °/s (0 = simhead default)")
		honest    = flag.Bool("honest", false, "report true position while moving (disables the garbage quirk)")
		silence   = flag.Duration("silence", 0, "silence window for absolute sets (0 = simhead default)")
		listPorts = flag.Bool("list-ports", false, "enumerate serial ports (with USB identity) and exit")
	)
	flag.Parse()
	if *listPorts {
		ports, err := serialio.EnumeratePorts()
		if err != nil {
			fatal("list ports: %v", err)
		}
		serialio.WritePorts(os.Stdout, ports)
		return
	}
	if (*pty == "") == (*listen == "") {
		fatal("exactly one of -pty or -listen is required")
	}

	head := simhead.New(simhead.Options{
		Addr: byte(*addr), PanDeg: *pan, TiltDeg: *tilt,
		SilenceRequired: *silence, HonestReadback: *honest,
		RateAzDegPerS: *rateAz, RateElDegPerS: *rateEl,
	})

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if *listen != "" {
		serveTCP(ctx, head, *listen)
		return
	}
	servePTY(ctx, head, *pty, *baud)
}

func serveTCP(ctx context.Context, head *simhead.Head, addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fatal("listen %s: %v", addr, err)
	}
	fmt.Printf("mock head on tcp://%s (Ctrl-C to quit)\n", addr)
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "accept: %v\n", err)
			}
			return
		}
		// One head, one client at a time — like the real RS-485 line.
		go func() {
			fmt.Printf("client %s connected\n", conn.RemoteAddr())
			bridge(ctx, head, tcpLine{conn})
			fmt.Printf("client %s gone\n", conn.RemoteAddr())
		}()
	}
}

func servePTY(ctx context.Context, head *simhead.Head, path string, baud int) {
	port, err := serialio.OpenPort(path, baud)
	if err != nil {
		fatal("open %s: %v", path, err)
	}
	fmt.Printf("mock head on %s (Ctrl-C to quit)\n", path)
	go func() { <-ctx.Done(); _ = port.Close(); _ = head.Close() }()
	bridge(ctx, head, port)
}

// tcpLine adapts a net.Conn to serialio.Transport.
type tcpLine struct{ conn net.Conn }

func (t tcpLine) Read(p []byte) (int, error) { return t.conn.Read(p) }
func (t tcpLine) Write(b []byte) error       { _, err := t.conn.Write(b); return err }
func (t tcpLine) Close() error               { return t.conn.Close() }

// lineWriter adapts Transport-style writers (Write([]byte) error) to io.Writer
// for the copy loops.
type lineWriter struct{ t serialio.Transport }

func (w lineWriter) Write(b []byte) (int, error) {
	if err := w.t.Write(b); err != nil {
		return 0, err
	}
	return len(b), nil
}

// bridge copies head→line and line→head until either side dies. Read is
// io-shaped everywhere; only Write is Transport-shaped, hence lineWriter.
func bridge(ctx context.Context, head *simhead.Head, line serialio.Transport) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(lineWriter{head}, line); done <- struct{}{} }()
	go func() { _, _ = io.Copy(lineWriter{line}, head); done <- struct{}{} }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "pelcobridge2-mock: "+format+"\n", args...)
	os.Exit(1)
}
