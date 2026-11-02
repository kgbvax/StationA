// Package cluster is a DX-cluster telnet client (DXSpider, CC-Cluster, AR-
// Cluster): it logs in with the operator's call, reads the "DX de" lines and
// hands each HF spot to the caller. It reconnects on its own; State says
// whether it is connected and why not.
package cluster

import (
	"bufio"
	"context"
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"chase-queue/internal/band"
)

const (
	loginWait   = 15 * time.Second // send the call anyway when no prompt shows
	idleTimeout = 10 * time.Minute // a silent node is reconnected
	backoffMin  = 5 * time.Second
	backoffMax  = 2 * time.Minute
)

// Spot is one DX-cluster spot.
type Spot struct {
	Spotter string
	Call    string
	FreqHz  float64
	Band    string
	Comment string
	At      time.Time // when it arrived
}

// dxLine is "DX de SP5XYZ:     14025.0  EA8XYZ       CW 599 tnx       1234Z JO32".
var dxLine = regexp.MustCompile(`^DX de\s+([A-Za-z0-9/#\-]+):?\s*([0-9]+(?:\.[0-9]+)?)\s+([A-Za-z0-9/]+)\s*(.*)$`)

var tail = regexp.MustCompile(`\s*\d{4}Z(\s+[A-Ra-r]{2}\d{2})?\s*$`)

// Parse reads one cluster line; ok is false for anything but an HF spot.
func Parse(line string, now time.Time) (Spot, bool) {
	m := dxLine.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return Spot{}, false
	}
	khz, err := strconv.ParseFloat(m[2], 64)
	if err != nil {
		return Spot{}, false
	}
	b, ok := band.Of(khz * 1000)
	if !ok {
		return Spot{}, false
	}
	return Spot{Spotter: strings.ToUpper(m[1]), Call: strings.ToUpper(m[3]), FreqHz: khz * 1000, Band: b.Name,
		Comment: strings.TrimSpace(tail.ReplaceAllString(m[4], "")), At: now.UTC()}, true
}

// Skimmer reports whether a spot comes from a CW/RTTY skimmer (spotter "-#").
func (s Spot) Skimmer() bool { return strings.HasSuffix(s.Spotter, "-#") }

// State is the connection state.
type State struct {
	Connected bool
	Since     time.Time // connected since, or disconnected since
	LastError string
}

// Client is one cluster login.
type Client struct {
	Addr string // host:port
	Call string // login
	Dial func(ctx context.Context, addr string) (net.Conn, error)
	Now  func() time.Time

	mu    sync.Mutex
	state State
}

// State reports the connection state.
func (c *Client) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *Client) set(connected bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Connected != connected || c.state.Since.IsZero() {
		c.state.Since = c.now()
	}
	c.state.Connected = connected
	if err != nil {
		c.state.LastError = err.Error()
	} else if connected {
		c.state.LastError = ""
	}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Run connects, logs in and calls spot for every HF spot until ctx ends,
// reconnecting with backoff.
func (c *Client) Run(ctx context.Context, spot func(Spot)) {
	c.set(false, nil) // disconnected since now
	wait := backoffMin
	for ctx.Err() == nil {
		start := c.now()
		err := c.session(ctx, spot)
		if ctx.Err() != nil {
			return
		}
		c.set(false, err)
		if c.now().Sub(start) > backoffMax {
			wait = backoffMin // the session held a while: start over
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, backoffMax)
	}
}

func (c *Client) session(ctx context.Context, spot func(Spot)) error {
	dial := c.Dial
	if dial == nil {
		dial = func(ctx context.Context, addr string) (net.Conn, error) {
			d := net.Dialer{Timeout: 15 * time.Second, KeepAlive: time.Minute}
			return d.DialContext(ctx, "tcp", addr)
		}
	}
	conn, err := dial(ctx, c.Addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	r := bufio.NewReader(conn)
	if err := c.login(conn, r); err != nil {
		return err
	}
	c.set(true, nil)
	for {
		conn.SetReadDeadline(time.Now().Add(idleTimeout))
		line, err := r.ReadString('\n')
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return errors.New("no data for 10 minutes")
			}
			return err
		}
		if s, ok := Parse(strings.TrimRight(line, "\r\n"), c.now()); ok {
			spot(s)
		}
	}
}

// login waits for the node's call prompt (which has no newline) and sends
// the call; with no prompt within loginWait it sends the call anyway.
func (c *Client) login(conn net.Conn, r *bufio.Reader) error {
	var seen strings.Builder
	deadline := time.Now().Add(loginWait)
	buf := make([]byte, 512)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(deadline)
		n, err := r.Read(buf)
		seen.Write(buf[:n])
		low := strings.ToLower(seen.String())
		if strings.Contains(low, "login") || strings.Contains(low, "call:") || strings.Contains(low, "callsign") || strings.Contains(low, "your call") {
			break
		}
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				break
			}
			return err
		}
		if seen.Len() > 64<<10 {
			break
		}
	}
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := conn.Write([]byte(c.Call + "\r\n"))
	return err
}
