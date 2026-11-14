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

// Liveness-probe bounds. SmartSDR is on-change-only: a healthy but quiet band
// streams nothing for minutes, so frame silence must NOT be mistaken for a
// dead link. Instead, after probeInterval with no inbound traffic the read
// loop sends an eliciting `version` probe; if NOTHING arrives within
// probeTimeout of that eliciting write, the connection is provably dead.
// Worst-case frozen-conn detection ≈ probeInterval + probeTimeout (vs ~11 min
// for TCP keepalive, and forever before). Without this, a frozen TCP conn
// (radio power-pulled, LAN path silently dropped) blocked Run forever while
// the bridge's 5 s heartbeat kept laundering stale state as fresh.
const (
	DefaultProbeInterval = 10 * time.Second
	DefaultProbeTimeout  = 5 * time.Second

	// probeCmd elicits a reply from any SmartSDR firmware and changes nothing.
	probeCmd = "version"
)

// Handler receives parsed status frames. Implementations typically route
// by frame.Topic. It must not block; slow work should be queued.
type Handler func(Frame)

// Client is a SmartSDR TCP client.
type Client struct {
	addr    string // host:4992
	conn    net.Conn
	rd      *bufio.Reader
	handler Handler

	probeInterval time.Duration // idle gap before the liveness probe
	probeTimeout  time.Duration // wait for proof of life after the probe

	mu      sync.Mutex
	closed  bool
	writeMu sync.Mutex // serializes command writes (send) against concurrent Senders

	// Command sequence numbers. Every command goes out as C<seq>|<cmd> with a
	// fresh seq; the radio echoes it in R<seq>|<code>|<message>, so a reply
	// maps back to the command that caused it. pending holds the commands
	// still waiting for a reply (seqMu, not writeMu: the read loop takes it).
	seqMu        sync.Mutex
	seq          int
	pending      map[int]string
	replyHandler func(Reply)
}

// maxPending bounds the pending-command map. The radio replies to every
// command, so the map stays near empty; the bound only guards against a radio
// that stops replying while commands keep coming.
const maxPending = 256

// Reply is the radio's answer to one command: R<seq>|<code>|<message>. Code 0
// means the radio accepted the command; any other code is a SmartSDR error
// (hex on the wire, e.g. 0x50000016) and Message may carry its text.
type Reply struct {
	Seq     int
	Code    uint32
	Message string
	Command string // the command sent with Seq; "" if the client did not send it
}

// OK reports whether the radio accepted the command.
func (r Reply) OK() bool { return r.Code == 0 }

// parseReply decodes a reply frame (Handle = seq, Body = "<hex code>|<message>").
func parseReply(f Frame) (Reply, error) {
	seq, err := strconv.Atoi(f.Handle)
	if err != nil {
		return Reply{}, fmt.Errorf("flexradio: reply seq %q: %w", f.Handle, err)
	}
	codeStr, msg, _ := strings.Cut(f.Body, "|")
	code, err := strconv.ParseUint(strings.TrimSpace(codeStr), 16, 32)
	if err != nil {
		return Reply{}, fmt.Errorf("flexradio: reply code %q: %w", codeStr, err)
	}
	return Reply{Seq: seq, Code: uint32(code), Message: strings.TrimSpace(msg)}, nil
}

// Dial connects to the radio at host:4992.
func Dial(ctx context.Context, host string) (*Client, error) {
	var d net.Dialer
	addr := net.JoinHostPort(host, strconv.Itoa(CommandPort))
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	c := &Client{
		addr:    addr,
		conn:    conn,
		rd:      bufio.NewReader(conn),
		handler: func(Frame) {},
	}
	c.defaults()
	return c, nil
}

// newClientFromConn builds a client over an already-connected conn. Used
// by tests to inject an in-memory pipe.
func newClientFromConn(conn net.Conn) *Client {
	c := &Client{
		conn:    conn,
		rd:      bufio.NewReader(conn),
		handler: func(Frame) {},
	}
	c.defaults()
	return c
}

// defaults fills unset probe bounds (tests shrink them between construction
// and Run).
func (c *Client) defaults() {
	if c.probeInterval <= 0 {
		c.probeInterval = DefaultProbeInterval
	}
	if c.probeTimeout <= 0 {
		c.probeTimeout = DefaultProbeTimeout
	}
}

// SetReplyHandler installs the handler for command replies (see Reply). It
// receives every reply the read loop sees, except the replies Handshake
// consumes itself. Must be called before Handshake. It must not block.
func (c *Client) SetReplyHandler(h func(Reply)) {
	c.replyHandler = h
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
		"sub tx all",  // transmit status: rfpower/tunepower/max_power_level, for the power ceiling
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
	// *after* the awaited commands; its reply reaches the reply handler through
	// Run (a refusal is logged there). On a
	// v4 radio the DVK status stream then flows through HandleStatus("dvk").
	_, _ = c.send(ctx, "sub dvk all")
	// Mic-profile list: SmartSDR does not broadcast profiles via `sub radio all`;
	// the list is obtained by sending the one-shot `profile mic info` command
	// (undocumented, from FlexLib). Fire-and-forget like `sub dvk all`; the radio
	// replies with a `profile mic list=…` status frame handled in
	// HandleStatus("profile"). Best-effort — an older radio that rejects it just
	// leaves mic_profiles empty (the feature degrades gracefully).
	_, _ = c.send(ctx, "profile mic info")
	return info, nil
}

// sendAwaitReply writes a C<seq>|<cmd> command and reads lines until the
// R<seq>|... reply with the same seq arrives. Interleaved status frames (S|...)
// go to the handler and other replies to the reply handler, so none are lost
// during the handshake. Returns the reply body ("<code>|<message>").
func (c *Client) sendAwaitReply(ctx context.Context, cmd string) (string, error) {
	seq, err := c.send(ctx, cmd)
	if err != nil {
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
			if frame.Handle == strconv.Itoa(seq) {
				c.takePending(seq)
				return frame.Body, nil
			}
			c.dispatchReply(frame)
		case FrameStatus:
			// Dispatch interleaved status so it isn't lost.
			c.handler(frame)
		}
	}
}

// send writes a C<seq>|... command with a fresh seq and records it as pending.
// It does NOT wait for the reply; replies are surfaced through Run to the
// reply handler (or consumed by sendAwaitReply). writeMu serializes writes so
// concurrent Senders (e.g. the /cmd worker) do not interleave on the wire;
// reads in Run use a separate bufio.Reader and are independent of this lock.
func (c *Client) send(ctx context.Context, cmd string) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	seq := c.nextSeq(cmd)
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	_ = c.conn.SetWriteDeadline(deadline)
	defer c.conn.SetWriteDeadline(time.Time{})
	if _, err := fmt.Fprintf(c.conn, "C%d|%s\n", seq, cmd); err != nil {
		c.takePending(seq)
		return 0, err
	}
	return seq, nil
}

// nextSeq allocates the next command seq and records cmd as pending.
func (c *Client) nextSeq(cmd string) int {
	c.seqMu.Lock()
	defer c.seqMu.Unlock()
	c.seq++
	if c.pending == nil || len(c.pending) >= maxPending {
		c.pending = make(map[int]string)
	}
	c.pending[c.seq] = cmd
	return c.seq
}

// takePending removes and returns the command recorded for seq.
func (c *Client) takePending(seq int) string {
	c.seqMu.Lock()
	defer c.seqMu.Unlock()
	cmd := c.pending[seq]
	delete(c.pending, seq)
	return cmd
}

// dispatchReply matches a reply frame to its command and hands it to the
// reply handler. Replies to the liveness probe are dropped: they carry no
// information beyond "the wire is alive", which Run already noted.
func (c *Client) dispatchReply(f Frame) {
	r, err := parseReply(f)
	if err != nil {
		return
	}
	r.Command = c.takePending(r.Seq)
	if r.Command == probeCmd && r.OK() {
		return
	}
	if c.replyHandler != nil {
		c.replyHandler(r)
	}
}

// Send writes a SmartSDR command to the radio without waiting for the reply
// (fire-and-forget). Run delivers the reply to the reply handler, which logs a
// refused command; callers that need confirmation observe the radio's async
// status stream — the stationa fire-and-observe plane discipline. Safe to call
// concurrently with Run.
func (c *Client) Send(ctx context.Context, cmd string) error {
	_, err := c.send(ctx, cmd)
	return err
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
//
// SetRFPower / SetTunePower / SetMaxPowerLevel exist ONLY to enforce the
// power ceiling (set_rf_power_limit): the bridge calls them to lower a value
// that is over the limit, or to set/lift the radio-side max_power_level
// ceiling — never to raise output. The `transmit set` wire form is from
// FlexLib; the status fields were confirmed live on the FLEX-8400.
type Commander interface {
	DVKPlay(id int) error                           // dvk playback_start id=<id>
	DVKStop(id int) error                           // dvk playback_stop  id=<id>
	SetBand(panHandle string, bandNumber int) error // display pan s <handle> band=<n>
	SetMicProfile(name string) error                // profile mic load "<name>"
	SetSliceMode(slice int, mode string) error      // slice set <idx> mode=<MODE>
	SetSliceRxAnt(slice int, ant string) error      // slice set <idx> rxant=<PORT>
	TuneSlice(slice int, freqHz int64) error        // slice tune <idx> <MHz> autopan=1
	SetRFPower(level int) error                     // transmit set rfpower=<n>
	SetTunePower(level int) error                   // transmit set tunepower=<n>
	SetMaxPowerLevel(level int) error               // transmit set max_power_level=<n>
}

// SetRFPower sets the transmit RF power slider (0–100; watts on the FLEX-8400).
func (c *Client) SetRFPower(level int) error {
	return c.Send(context.Background(), fmt.Sprintf("transmit set rfpower=%d", level))
}

// SetTunePower sets the TUNE carrier power (0–100).
func (c *Client) SetTunePower(level int) error {
	return c.Send(context.Background(), fmt.Sprintf("transmit set tunepower=%d", level))
}

// SetMaxPowerLevel sets the radio-side transmit power ceiling (0–100).
func (c *Client) SetMaxPowerLevel(level int) error {
	return c.Send(context.Background(), fmt.Sprintf("transmit set max_power_level=%d", level))
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

// SetSliceMode sets the demodulation mode of slice index slice (SmartSDR
// `slice set <idx> mode=<MODE>`, from the SmartSDR TCPIP slice wiki). mode is
// the firmware mode string (CW, USB, LSB); the bridge maps the canonical
// vocabulary before calling. The result is observed on the slice status stream.
func (c *Client) SetSliceMode(slice int, mode string) error {
	return c.Send(context.Background(), fmt.Sprintf("slice set %d mode=%s", slice, mode))
}

// TuneSlice tunes slice index slice to freqHz (SmartSDR `slice tune <idx>
// <MHz> autopan=1`, the command a logger's spot click ends in). autopan=1 moves
// the panadapter along: without it the radio refuses a tune outside the pan's
// current span (seen live 2026-11-04: pan on 80 m, set_freq 14.074 MHz left
// the slice where it was). The result is observed on the slice status stream
// (RF_frequency=).
func (c *Client) TuneSlice(slice int, freqHz int64) error {
	return c.Send(context.Background(), fmt.Sprintf("slice tune %d %.6f autopan=1", slice, float64(freqHz)/1e6))
}

// SetSliceRxAnt selects the receive antenna port of slice index slice
// (SmartSDR `slice set <idx> rxant=<PORT>`; ports as in the slice's ant_list,
// e.g. ANT1, RX_A). Receive path only: txant is untouched. The result is
// observed on the slice status stream (rxant=).
func (c *Client) SetSliceRxAnt(slice int, ant string) error {
	return c.Send(context.Background(), fmt.Sprintf("slice set %d rxant=%s", slice, ant))
}

// Run blocks reading status lines and dispatching them to the handler.
// It returns when the connection is closed, the context is cancelled, or
// an unrecoverable read error occurs — including silence after an eliciting
// probe (the liveness watchdog; see the probe-bound constants).
//
// The probe runs on THIS goroutine: sendAwaitReply reads the same bufio
// reader as Run, so a watchdog calling it from another goroutine would race
// the read loop. Single reader, always.
func (c *Client) Run(ctx context.Context) error {
	// Wake the blocking read on ctx cancellation: an already-expired read
	// deadline unblocks it and the loop's error path returns ctx.Err().
	// (Without this, Run could block in ReadString long past cancellation —
	// pre-probe it blocked FOREVER on a silent conn.)
	go func() {
		<-ctx.Done()
		_ = c.conn.SetReadDeadline(time.Now())
	}()

	probing := false
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wait := c.probeInterval
		if probing {
			wait = c.probeTimeout
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(wait))
		line, err := c.rd.ReadString('\n')
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				if !probing {
					// Idle gap reached: elicit proof of life. Silence alone is
					// not death (on-change-only stream); the eliciting write is
					// what makes the NEXT silence meaningful.
					if _, perr := c.send(ctx, probeCmd); perr != nil {
						return fmt.Errorf("radio probe write: %w", perr)
					}
					probing = true
					continue
				}
				return fmt.Errorf("radio probe: no traffic within %s of the %s probe",
					c.probeTimeout, probeCmd)
			}
			if errors.Is(err, io.EOF) || isClosed(err) {
				return err
			}
			// Transient read error: keep going briefly, then surface.
			return err
		}
		probing = false // any inbound line — status or reply — proves the wire alive
		frame, err := ParseFrame(line)
		if err != nil {
			continue // malformed line; skip
		}
		if frame.Kind == FrameReply {
			c.dispatchReply(frame)
			continue
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
