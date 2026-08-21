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
//  1. Send `version` to probe the connection and confirm the protocol.
//  2. Send the subscription commands (slice/radio/interlock/atu).
//  3. Query the radio identity via `info`.
//
// After the handshake the read loop pushes every status frame to Handler.

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

	mu      sync.Mutex
	closed  bool
	handle  int        // command handle counter (we always use 1)
	writeMu sync.Mutex // serializes command writes (send) against concurrent Senders
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

// Handshake probes the connection, subscribes to status topics, and queries
// the radio identity. Must be called exactly once after Dial, before Run.
//
// It returns the radio's self-reported model and serial. The RadioInfo fields
// may be empty if the radio did not supply them (non-fatal).
func (c *Client) Handshake(ctx context.Context) (RadioInfo, error) {
	if _, err := c.sendAwaitReply(ctx, "version"); err != nil {
		return RadioInfo{}, fmt.Errorf("version exchange: %w", err)
	}

	subs := []string{
		"sub slice all",
		"sub radio all",
		"sub interlock all",
		"sub atu all",
		"sub pan all", // panadapter status: pan stream ids (handles) + band, for band changes
	}
	for _, s := range subs {
		if _, err := c.sendAwaitReply(ctx, s); err != nil {
			return RadioInfo{}, fmt.Errorf("subscribe %q: %w", s, err)
		}
	}

	var info RadioInfo
	if reply, err := c.sendAwaitReply(ctx, "info"); err == nil {
		info = parseInfoReply(reply)
	}

	// DVK (SmartSDR v4+, SmartSDR+ license). Subscribe best-effort: a v3 radio
	// or an unlicensed radio may reject `sub dvk all`, and that must not break
	// the handshake — DVK is an optional capability. Sent fire-and-forget
	// *after* the awaited commands so its reply is consumed by Run (as a dropped
	// FrameReply) rather than misattributed to a following sendAwaitReply. On a
	// v4 radio the DVK status stream then flows through HandleStatus("dvk").
	_ = c.send(ctx, "sub dvk all")
	// Mic-profile list: SmartSDR does not broadcast profiles via `sub radio all`;
	// the list is obtained by sending the one-shot `profile mic info` command
	// (undocumented, from FlexLib). Fire-and-forget like `sub dvk all`; the radio
	// replies with a `profile mic list=…` status frame handled in
	// HandleStatus("profile"). Best-effort — an older radio that rejects it just
	// leaves mic_profiles empty (the feature degrades gracefully).
	_ = c.send(ctx, "profile mic info")
	return info, nil
}

// sendAwaitReply writes a C1|<cmd> command and reads lines until the matching
// R1|<seq>|... reply arrives. Any interleaved asynchronous status frames
// (S|...) are dispatched to the handler so none are lost during the
// handshake. Returns the reply body (the part after "R1|<seq>|").
//
// Sequence numbers: SmartSDR replies echo a per-command sequence number in
// the second field. We always use sequence 0 (we send one command at a time
// during the handshake), so we match the first R1| reply we see.
func (c *Client) sendAwaitReply(ctx context.Context, cmd string) (string, error) {
	if err := c.send(ctx, cmd); err != nil {
		return "", err
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	_ = c.conn.SetReadDeadline(deadline)
	defer c.conn.SetReadDeadline(time.Time{})

	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		line, err := c.rd.ReadString('\n')
		if err != nil {
			return "", err
		}
		frame, err := ParseFrame(line)
		if err != nil {
			continue
		}
		switch frame.Kind {
		case FrameReply:
			// R1|<seq>|<body>. We sent C1|..., so the matching reply is R1|...
			return frame.Body, nil
		case FrameStatus:
			// Dispatch interleaved status so it isn't lost.
			c.handler(frame)
		}
	}
}

// send writes a C1|... command. It does NOT wait for the R1 reply; replies
// are surfaced through Run as FrameReply (or consumed by sendAwaitReply).
// writeMu serializes writes so concurrent Senders (e.g. the /cmd worker) do
// not interleave on the wire; reads in Run use a separate bufio.Reader and are
// independent of this lock.
func (c *Client) send(ctx context.Context, cmd string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	_ = c.conn.SetWriteDeadline(deadline)
	defer c.conn.SetWriteDeadline(time.Time{})
	_, err := fmt.Fprintf(c.conn, "C%d|%s\n", c.handle, cmd)
	return err
}

// Send writes a SmartSDR command to the radio without waiting for the reply
// (fire-and-forget). The reply, if any, is delivered through Run as a
// FrameReply (the bridge handler drops FrameReply); callers that need
// confirmation observe the radio's async status stream instead — the stationa
// fire-and-observe plane discipline. Safe to call concurrently with Run.
func (c *Client) Send(ctx context.Context, cmd string) error {
	return c.send(ctx, cmd)
}

// Commander is the radio control surface the bridge drives from /cmd. *Client
// implements it; tests use a fake. Methods send fire-and-forget SmartSDR
// commands and rely on the status stream for confirmation.
//
// DVK (Digital Voice Keyer) is a SmartSDR v4+ / SmartSDR+ feature: 12 voice
// memories (ids 1-12). playback_start plays a memory AND keys the transmitter
// (no separate xmit needed); playback_stop stops and unkeys. The wire strings
// were originally third-party-confirmed (AetherSDR vs FLEX-8600 fw 4.2.18) and
// are now confirmed against the live FLEX-8400 on shari; the official SmartSDR
// API wiki does not document the `dvk` command family.
//
// SetBand drives SmartSDR's native band-stacking: `display pan s <handle>
// band=<wavelength>` changes the panadapter's band and the radio restores the
// last-used frequency/mode for that band. The wire command and band-number
// (wavelength-in-meters) form are from the SmartSDR TCPIP display-pan wiki,
// confirmed live on shari the same way the DVK commands were.
//
// SetMicProfile drives SmartSDR's native mic profiles: `profile mic load
// "<name>"` selects a profile. This is from the SmartSDR TCPIP profile wiki.
// The available profile LIST is obtained by sending the one-shot `profile mic
// info` command (undocumented but used by FlexLib/AetherSDR; confirmed live on
// shari) — sent directly in the handshake (see Client.Handshake), the radio
// replies with `profile mic list=<name>^<name>^…` status frames parsed by
// ParseProfile. SmartSDR does NOT report an active mic profile, so the bridge
// tracks the active name client-side as "last loaded via SetMicProfile".
// (`profile mic save "<name>"` is obsolete on SmartSDR v4+ — the radio returns
// malformed — so there is no SaveMicProfile here; profile creation/editing now
// uses a file-transfer mechanism, out of scope.)
type Commander interface {
	DVKPlay(id int) error                           // dvk playback_start id=<id>
	DVKStop(id int) error                           // dvk playback_stop  id=<id>
	SetBand(panHandle string, bandNumber int) error // display pan s <handle> band=<n>
	SetMicProfile(name string) error                // profile mic load "<name>"
}

// DVKPlay triggers playback of DVK memory id (1-12) and keys the transmitter.
func (c *Client) DVKPlay(id int) error {
	return c.Send(context.Background(), fmt.Sprintf("dvk playback_start id=%d", id))
}

// DVKStop stops playback of DVK memory id and unkeys the transmitter.
func (c *Client) DVKStop(id int) error {
	return c.Send(context.Background(), fmt.Sprintf("dvk playback_stop id=%d", id))
}

// SetBand changes the band on panadapter panHandle to bandNumber using SmartSDR
// native band-stacking: the radio restores the last-used frequency/mode for
// that band. bandNumber is the wavelength-in-meters (e.g. 20 for 20m). The
// result is observed on the slice/pan status stream, not via the reply.
func (c *Client) SetBand(panHandle string, bandNumber int) error {
	return c.Send(context.Background(), fmt.Sprintf("display pan s %s band=%d", panHandle, bandNumber))
}

// SetMicProfile makes name the current mic profile (SmartSDR `profile mic load
// "<name>"`). The result is observed on the profile status stream, not via the
// reply. name is wrapped in double quotes on the wire; the bridge validates it
// (no embedded quotes/control chars) before calling.
func (c *Client) SetMicProfile(name string) error {
	return c.Send(context.Background(), fmt.Sprintf("profile mic load \"%s\"", name))
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

// RadioInfo holds the model, serial, and firmware version reported by the
// radio's "info" command.
type RadioInfo struct {
	Model    string // e.g. "FLEX-8400"
	Serial   string // e.g. "1126-1213-8400-3564"
	Firmware string // e.g. "3.8.19"
}

// parseInfoReply parses the comma-separated key="value" info reply body.
// Example: model="FLEX-8400",chassis_serial="1126-1213-8400-3564",...
func parseInfoReply(reply string) RadioInfo {
	// Body from sendAwaitReply is "<seq>|<content>"; strip the seq prefix.
	if i := strings.IndexByte(reply, '|'); i >= 0 {
		reply = reply[i+1:]
	}
	var ri RadioInfo
	for _, tok := range strings.Split(reply, ",") {
		eq := strings.IndexByte(tok, '=')
		if eq <= 0 {
			continue
		}
		k := strings.TrimSpace(tok[:eq])
		v := strings.Trim(tok[eq+1:], `"`)
		switch k {
		case "model":
			ri.Model = v
		case "chassis_serial":
			ri.Serial = v
		case "firmware_version":
			ri.Firmware = v
		}
	}
	return ri
}

func isClosed(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	return strings.Contains(err.Error(), "use of closed network connection")
}
