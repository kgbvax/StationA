// Package source is the WebSocket client for OscarWatch's Satellite-link
// server. It dials, hands every text frame to the caller, and returns the
// error that ended the connection so the caller's reconnect loop can back off.
// It never writes data frames — the protocol is server-push only.
//
// Dead-link detection is by WebSocket ping, not by data silence: OscarWatch
// documents change-driven sends, so between passes (nothing focused, `** NO
// SATELLITE **`) the socket may legitimately go quiet. (The live server also
// re-sends the idle frame about once a second — observed 2026-09-30 — but that
// is undocumented, so liveness must not depend on it.) A ping every
// PingInterval with a read deadline of 2.5× that, extended by every pong and
// every frame, catches a dead PC or a pulled cable. OscarWatch answers pings
// (verified live 2026-09-30).
package source

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

const (
	handshakeTimeout = 10 * time.Second
	controlTimeout   = 5 * time.Second
	// DefaultReadLimit bounds one frame. A satelliteStatus frame is under
	// 1 KiB and a qsoLogged frame a few KiB; anything near the limit is not
	// OscarWatch talking.
	DefaultReadLimit = 64 << 10
)

// Client dials one Satellite-link URL. Fields are set once at construction
// (tests shrink the timings) and read-only afterwards.
type Client struct {
	URL string
	// PingInterval is the keepalive cadence; 0 disables pings and the read
	// deadline (the link is then only dropped by TCP errors).
	PingInterval time.Duration
	// PongWait is the read deadline armed after every pong or frame;
	// New sets it to 2.5 × PingInterval.
	PongWait  time.Duration
	ReadLimit int64
	Log       *slog.Logger
}

// New builds a client with the default pong wait and read limit.
func New(url string, pingInterval time.Duration, log *slog.Logger) *Client {
	return &Client{
		URL:          url,
		PingInterval: pingInterval,
		PongWait:     pingInterval * 5 / 2,
		ReadLimit:    DefaultReadLimit,
		Log:          log,
	}
}

// Run dials the server, calls onConnect once the handshake succeeds, then
// calls onFrame for every text frame (on this goroutine, in order) until the
// connection fails or ctx is cancelled. It always returns a non-nil error:
// ctx.Err() on cancellation, otherwise the failure that ended the run.
func (c *Client) Run(ctx context.Context, onConnect func(), onFrame func([]byte)) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: handshakeTimeout,
		NetDialContext:   (&net.Dialer{Timeout: handshakeTimeout, KeepAlive: 30 * time.Second}).DialContext,
	}
	conn, resp, err := dialer.DialContext(ctx, c.URL, http.Header{})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("dial %s: %w", c.URL, err)
	}
	defer conn.Close()
	conn.SetReadLimit(c.ReadLimit)

	// Close the conn when ctx dies so a blocked ReadMessage returns. The stop
	// channel ends the watcher when Run returns for any other reason — a bare
	// <-ctx.Done() would strand one goroutine per reconnect (atr1k review S3).
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()

	if c.PingInterval > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(c.PongWait))
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(c.PongWait))
		})
		go c.pinger(conn, stop)
	}

	if onConnect != nil {
		onConnect()
	}

	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read: %w", err)
		}
		if c.PingInterval > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(c.PongWait))
		}
		if mt != websocket.TextMessage {
			c.Log.Debug("ignoring non-text frame", "type", mt, "len", len(data))
			continue
		}
		onFrame(data)
	}
}

// pinger sends a ping every PingInterval until stop closes. WriteControl is
// safe to call concurrently with the reader (gorilla's documented exception).
// A failed ping is not fatal here: the missing pong expires the read deadline
// and the reader returns the error.
func (c *Client) pinger(conn *websocket.Conn, stop <-chan struct{}) {
	t := time.NewTicker(c.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(controlTimeout)); err != nil {
				c.Log.Debug("ping failed", "err", err)
			}
		}
	}
}
