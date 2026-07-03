package flexradio

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// FlexRadio discovery: the radio listens for a UDP broadcast on port 4992
// and replies with a textual payload describing itself. The default
// broadcast is the legacy "discovery" command; modern SmartSDR radios also
// broadcast unsolicited presence packets on the same port.
//
// Reply payload looks like (key=value, whitespace separated):
//
//	version=3.4.1 serial=1234-5678-8400.12345 nickname=Flex6400 model=FLEX-8400
//	ip=192.168.1.50 port=4992 ... status=Available ...

const (
	DiscoveryPort    = 4992
	discoveryTimeout = 3 * time.Second
)

// DiscoveredRadio describes a radio that replied to discovery.
type DiscoveredRadio struct {
	Serial   string
	Model    string
	Nickname string
	IP       net.IP
	Port     int
	Version  string
	Status   string // "Available", "InUse", ...
	Raw      string // raw reply, for diagnostics
}

// Discover broadcasts a discovery request and returns the first radio that
// replies, optionally matching wantSerial (empty = any). The function
// honors ctx for cancellation/timeout.
//
// If a unicast discovery target is preferred, callers should pass
// RadioHost directly rather than using Discover.
func Discover(ctx context.Context, wantSerial string) (*DiscoveredRadio, error) {
	// We need to send from a known local address so the reply can come back
	// to us; use a wildcard bind and let the OS pick.
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("listen udp: %w", err)
	}
	defer conn.Close()

	bcast := &net.UDPAddr{IP: net.IPv4bcast, Port: DiscoveryPort}
	// The discovery payload is intentionally minimal. Any datagram to 4992
	// triggers a reply on modern firmware; the empty bytes are fine.
	if _, err := conn.WriteToUDP([]byte("discovery"), bcast); err != nil {
		return nil, fmt.Errorf("broadcast discovery: %w", err)
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(discoveryTimeout)
	}
	_ = conn.SetReadDeadline(deadline)

	buf := make([]byte, 1500)
	var first *DiscoveredRadio
	for {
		select {
		case <-ctx.Done():
			if first != nil {
				return first, nil
			}
			return nil, ctx.Err()
		default:
		}
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if first != nil {
				return first, nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil, ctx.Err()
			}
			// Timeout / transient error: stop collecting.
			return nil, fmt.Errorf("read discovery reply: %w", err)
		}
		raw := string(buf[:n])
		r := parseDiscoveryReply(raw)
		if wantSerial != "" {
			if !strings.EqualFold(r.Serial, wantSerial) {
				continue // different radio, keep waiting
			}
			return r, nil
		}
		// Prefer an Available radio; otherwise keep the first reply and
		// try once more for a better candidate.
		if first == nil {
			first = r
		}
		if strings.EqualFold(r.Status, "Available") {
			return r, nil
		}
	}
}

// parseDiscoveryReply parses the key=value whitespace-separated discovery
// reply into a DiscoveredRadio. Unknown keys are ignored.
func parseDiscoveryReply(raw string) *DiscoveredRadio {
	r := &DiscoveredRadio{Raw: raw, Port: DiscoveryPort}
	for _, tok := range strings.Fields(raw) {
		eq := strings.IndexByte(tok, '=')
		if eq <= 0 {
			continue
		}
		k, v := tok[:eq], tok[eq+1:]
		switch strings.ToLower(k) {
		case "serial":
			r.Serial = v
		case "model":
			r.Model = v
		case "nickname":
			r.Nickname = v
		case "ip":
			if ip := net.ParseIP(v); ip != nil {
				r.IP = ip
			}
		case "port":
			if n, err := strconv.Atoi(v); err == nil {
				r.Port = n
			}
		case "version":
			r.Version = v
		case "status":
			r.Status = v
		}
	}
	return r
}
