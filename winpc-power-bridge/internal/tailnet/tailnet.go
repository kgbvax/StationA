// Package tailnet wakes the Tailscale tunnels of this PC after a resume from
// sleep.
//
// Windows pauses Tailscale while the PC sleeps (Modern Standby too). After
// the resume, a remote computer often cannot open a tunnel to this PC, but
// this PC can open one to the remote computer. PingPeers does that for every
// online peer: it runs `tailscale ping --tsmp` to each of them, which opens
// the tunnel from this side. Tailscale is optional: with no Tailscale
// program, PingPeers does nothing.
package tailnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"time"
)

// PingTimeout bounds one `tailscale ping`.
const PingTimeout = 5 * time.Second

// Runner runs the Tailscale program with args and returns its standard
// output. Injected so tests do not start a program.
type Runner func(ctx context.Context, exe string, args ...string) ([]byte, error)

// ExecRunner is the Runner that starts the real program.
func ExecRunner(ctx context.Context, exe string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, exe, args...).Output()
}

// ErrNoTailscale means that the Tailscale program is not installed.
var ErrNoTailscale = errors.New("tailscale program not found")

// Result is what PingPeers did.
type Result struct {
	Peers  int // online peers found
	Pinged int // peers that answered
}

type statusJSON struct {
	Peer map[string]struct {
		HostName     string
		Online       bool
		TailscaleIPs []string
	}
}

// PingPeers runs `tailscale ping --tsmp` to every online peer, one after the
// other. A peer that does not answer is not an error: it is off or asleep.
// An error is returned only when the status cannot be read.
func PingPeers(ctx context.Context, run Runner, exe string) (Result, error) {
	out, err := run(ctx, exe, "status", "--json")
	if err != nil {
		var ee *exec.Error
		if errors.As(err, &ee) && errors.Is(ee.Err, exec.ErrNotFound) {
			return Result{}, ErrNoTailscale
		}
		return Result{}, fmt.Errorf("tailscale status: %w", err)
	}
	var st statusJSON
	if err := json.Unmarshal(out, &st); err != nil {
		return Result{}, fmt.Errorf("tailscale status: %w", err)
	}
	var addrs []string
	for _, p := range st.Peer {
		if p.Online && len(p.TailscaleIPs) > 0 {
			addrs = append(addrs, p.TailscaleIPs[0])
		}
	}
	sort.Strings(addrs)
	res := Result{Peers: len(addrs)}
	for _, a := range addrs {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		pctx, cancel := context.WithTimeout(ctx, PingTimeout+2*time.Second)
		_, err := run(pctx, exe, "ping", "--tsmp", "-c", "1", "--timeout", PingTimeout.String(), a)
		cancel()
		if err == nil {
			res.Pinged++
		}
	}
	return res, nil
}

// ResumeDetector reports a resume from sleep. Call Tick on a steady timer:
// when the wall clock jumped by more than the allowed gap since the last
// call, the PC slept in between.
type ResumeDetector struct {
	gap  time.Duration
	last time.Time
}

// NewResumeDetector returns a detector that reports a gap above maxGap.
func NewResumeDetector(maxGap time.Duration, now time.Time) *ResumeDetector {
	return &ResumeDetector{gap: maxGap, last: now.Round(0)}
}

// Tick records now and reports whether more than maxGap passed since the
// last Tick. now.Round(0) drops the monotonic reading, which can stand still
// while the PC sleeps.
func (d *ResumeDetector) Tick(now time.Time) bool {
	now = now.Round(0)
	resumed := now.Sub(d.last) > d.gap
	d.last = now
	return resumed
}
