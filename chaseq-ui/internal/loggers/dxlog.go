package loggers

import (
	"context"
	"net"
	"time"
)

// DXLog talks to the ChaseqCallsign.cs script running inside DXLog.net: one
// "CALL <callsign>" datagram to a loopback port, answered "OK".
type DXLog struct {
	Addr    string
	Timeout time.Duration
}

// NewDXLog returns a sender for the script listening at addr.
func NewDXLog(addr string, timeout time.Duration) *DXLog {
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	return &DXLog{Addr: addr, Timeout: timeout}
}

// Name implements Sender.
func (d *DXLog) Name() string { return "DXLog" }

// SendCall implements Sender. An ICMP "unreachable" on the connected socket
// means nobody listens (DXLog or its script is not running): failed. No reply
// but no error: sent, not confirmed.
func (d *DXLog) SendCall(ctx context.Context, call string) Result {
	if !ValidCall(call) {
		return Result{Skipped, "no valid callsign"}
	}
	conn, err := net.Dial("udp4", d.Addr)
	if err != nil {
		return Result{Failed, err.Error()}
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(d.Timeout))
	if _, err := conn.Write([]byte("CALL " + call)); err != nil {
		return Result{Failed, "not reached: " + err.Error()}
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			return Result{NotConfirmed, "sent, no reply"}
		}
		return Result{Failed, "not reached (is the DXLog script running?)"}
	}
	if string(buf[:n]) == "OK" {
		return Result{OK, ""}
	}
	return Result{Failed, "DXLog script said " + string(buf[:n])}
}
