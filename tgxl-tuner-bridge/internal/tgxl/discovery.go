package tgxl

import (
	"context"
	"fmt"
	"net"
	"time"
)

// Discover listens for the TGXL's UDP 9010 broadcast and returns "ip:9010".
// Fallback only, used when tuner.address is empty.
func Discover(ctx context.Context, wait time.Duration) (string, error) {
	pc, err := net.ListenPacket("udp4", ":9010")
	if err != nil {
		return "", fmt.Errorf("discovery listen: %w", err)
	}
	defer pc.Close()
	deadline := time.Now().Add(wait)
	buf := make([]byte, 512)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		_ = pc.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			continue
		}
		if d, ok := ParseDiscovery(string(buf[:n])); ok {
			return net.JoinHostPort(d.IP, "9010"), nil
		}
	}
	return "", fmt.Errorf("no TunerGenius broadcast within %s", wait)
}
