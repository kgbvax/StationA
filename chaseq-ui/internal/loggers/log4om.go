package loggers

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Log4OM is a Remote Control Interface v1.1 client. Log4OM answers on the
// inbound port + 1 as an IPv4 broadcast, so the reply socket binds udp4 on
// all interfaces (Windows Defender Firewall may need to allow it).
type Log4OM struct {
	Addr    string
	Timeout time.Duration
}

// NewLog4OM returns a client for addr (host:port of the inbound UDP).
func NewLog4OM(addr string, timeout time.Duration) *Log4OM {
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	return &Log4OM{Addr: addr, Timeout: timeout}
}

// Name implements Sender.
func (l *Log4OM) Name() string { return "Log4OM" }

// SendCall sends SetCallsign. Log4OM discards it when a call was already
// typed by hand; the spec gives no way to tell, so a Done=True reply is "ok".
func (l *Log4OM) SendCall(ctx context.Context, call string) Result {
	if !ValidCall(call) {
		return Result{Skipped, "no valid callsign"}
	}
	ra, err := net.ResolveUDPAddr("udp", l.Addr)
	if err != nil {
		return Result{Failed, "address: " + err.Error()}
	}
	var reply *net.UDPConn
	if ra.Port > 0 && ra.Port < 65535 {
		reply, _ = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: ra.Port + 1})
	}
	if reply != nil {
		defer reply.Close()
		_ = reply.SetReadDeadline(time.Now().Add(l.Timeout))
	}
	id := newGUID()
	msg := fmt.Sprintf("<RemoteControlRequest><MessageId>%s</MessageId><RemoteControlMessage>SetCallsign</RemoteControlMessage><Callsign>%s</Callsign></RemoteControlRequest>", id, call)
	conn, err := net.DialUDP("udp", nil, ra)
	if err != nil {
		return Result{Failed, err.Error()}
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(msg)); err != nil {
		return Result{Failed, err.Error()}
	}
	if reply == nil {
		return Result{NotConfirmed, "reply port " + strconv.Itoa(ra.Port+1) + " unavailable"}
	}
	buf := make([]byte, 4096)
	for {
		if ctx.Err() != nil {
			return Result{NotConfirmed, "cancelled"}
		}
		n, _, err := reply.ReadFromUDP(buf)
		if err != nil {
			return Result{NotConfirmed, "sent, no reply"}
		}
		r := string(buf[:n])
		if !strings.Contains(r, id) {
			continue // an unsolicited RadioInfo broadcast
		}
		if strings.Contains(strings.ToLower(r), "<done>true</done>") {
			return Result{OK, ""}
		}
		return Result{Failed, "Log4OM refused the call"}
	}
}

func newGUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
