package flexradio

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SmartSDR TCP/IP API client (port 4992).
//
// The client performs a one-time handshake:
//
//  1. Read the radio's version banner (an "S|version ..." line).
//  2. Send "C1|client udpport <port>" to tell the radio where to stream
//     VITA-49 meters.
//  3. Send the subscription commands (slice/radio/interlock/atu/meter).
//  4. Optionally request the meter list and slice list (one-shot) so the
//     meter index map and initial state are populated immediately instead
//     of waiting for the first async status push.
//
// After the handshake the read loop pushes every status frame to Handler.
// Nothing is read on a timer after this — all updates arrive as async
// status lines or as VITA-49 UDP datagrams (handled by the bridge's UDP
// goroutine, not here).
//
// The client owns the TCP connection only. Meter UDP packets are read by
// the bridge from its own UDP listener; this client just configures the
// radio to send them.

// CommandPort is the SmartSDR TCP API port.
const CommandPort = 4992

// Handler receives parsed status frames. Implementations typically route
// by frame.Topic. It must not block; slow work should be queued.
type Handler func(Frame)

// Client is a SmartSDR TCP client.
type Client struct {
	addr    string // host:4992
	conn    net.Conn
	rd      *bufio.Reader
	handler Handler

	mu     sync.Mutex
	closed bool
	handle int // command handle counter (we always use 1)
}

// Dial connects to the radio at host:4992.
func Dial(ctx context.Context, host string) (*Client, error) {
	var d net.Dialer
	addr := net.JoinHostPort(host, strconv.Itoa(CommandPort))
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return &Client{
		addr:    addr,
		conn:    conn,
		rd:      bufio.NewReader(conn),
		handler: func(Frame) {},
		handle:  1,
	}, nil
}

// newClientFromConn builds a client over an already-connected conn. Used
// by tests to inject an in-memory pipe.
func newClientFromConn(conn net.Conn) *Client {
	return &Client{
		conn:    conn,
		rd:      bufio.NewReader(conn),
		handler: func(Frame) {},
		handle:  1,
	}
}

// SetHandler installs the status handler. Must be called before Run.
func (c *Client) SetHandler(h Handler) {
	if h == nil {
		h = func(Frame) {}
	}
	c.handler = h
}

// Close terminates the connection.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	return c.conn.Close()
}

// Handshake performs the version-banner read, sets the UDP port for the
// meter stream, and subscribes to the status topics. It must be called
// exactly once after Dial, before Run.
func (c *Client) Handshake(ctx context.Context, udpPort int) error {
	// 1. Read the radio's version banner line. The radio sends this
	//    unsolicited on connect: "S<handle>|version v3.x.x ...".
	if err := c.readVersionBanner(ctx); err != nil {
		return fmt.Errorf("version banner: %w", err)
	}

	// 2. Tell the radio which UDP port to stream meters to.
	if err := c.send(ctx, fmt.Sprintf("client udpport %d", udpPort)); err != nil {
		return fmt.Errorf("set udpport: %w", err)
	}

	// 3. Subscribe to all the status topics we publish.
	subs := []string{
		"sub slice all",
		"sub radio all",
		"sub interlock all",
		"sub atu all",
		"sub meter all",
	}
	for _, s := range subs {
		if err := c.send(ctx, s); err != nil {
			return fmt.Errorf("subscribe: %w", err)
		}
	}

	// 4. Request the current meter list (one-shot) so the meter index map
	//    is populated without waiting for async "meter N" pushes.
	if err := c.send(ctx, "meter list"); err != nil {
		// Non-fatal: we'll get meters via async status.
		_ = err
	}
	return nil
}

// readVersionBanner reads lines until it sees the version status line or
// times out. Replies to other incidental lines are ignored.
func (c *Client) readVersionBanner(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(10 * time.Second)
	}
	_ = c.conn.SetReadDeadline(deadline)
	defer c.conn.SetReadDeadline(time.Time{})

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line, err := c.rd.ReadString('\n')
		if err != nil {
			return err
		}
		frame, err := ParseFrame(line)
		if err != nil {
			continue
		}
		if frame.Kind == FrameStatus && (frame.Topic == "version" || strings.HasPrefix(frame.Body, "version")) {
			return nil
		}
	}
}

// send writes a C1|... command. It does NOT wait for the R1 reply; replies
// are surfaced through Run as FrameReply and most commands don't need the
// ack. Callers that need the reply should use sendAwait.
func (c *Client) send(ctx context.Context, cmd string) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	_ = c.conn.SetWriteDeadline(deadline)
	defer c.conn.SetWriteDeadline(time.Time{})
	_, err := fmt.Fprintf(c.conn, "C%d|%s\n", c.handle, cmd)
	return err
}

// Run blocks reading status lines and dispatching them to the handler.
// It returns when the connection is closed, the context is cancelled, or
// an unrecoverable read error occurs.
func (c *Client) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, err := c.rd.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) || isClosed(err) {
				return err
			}
			// Transient read error: keep going briefly, then surface.
			return err
		}
		frame, err := ParseFrame(line)
		if err != nil {
			continue // malformed line; skip
		}
		c.handler(frame)
	}
}

// RemoteAddr returns the radio address.
func (c *Client) RemoteAddr() string { return c.addr }

func isClosed(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	return strings.Contains(err.Error(), "use of closed network connection")
}
