// SPDX-License-Identifier: AGPL-3.0-or-later

// Package poll runs one periodic HTTP poll (NINA warnings, DWD wind) in its
// own goroutine: a per-attempt timeout, the regular interval after a
// success, and an exponential backoff (MinBackoff doubling up to the
// interval) after a failure. Failures log through logging.Streak — one Warn
// per outage, repeats at Info — and the recovery at Info.
package poll

import (
	"context"
	"log/slog"
	"time"

	"codeberg.org/kgbvax/stationa/shared/logging"
)

// Poller is one periodic poll.
type Poller struct {
	Name       string
	Interval   time.Duration
	MinBackoff time.Duration
	Timeout    time.Duration
	// Fn performs one attempt and hands its result to the engine (Enqueue);
	// the returned error only drives the schedule and the log.
	Fn  func(ctx context.Context) error
	Log *slog.Logger
	// Wait sleeps d or returns false when ctx is done (tests replace it).
	Wait func(ctx context.Context, d time.Duration) bool
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Run polls until ctx is done. The first attempt runs at once.
func (p Poller) Run(ctx context.Context) {
	wait := p.Wait
	if wait == nil {
		wait = sleep
	}
	var streak logging.Streak
	failing := false
	backoff := p.MinBackoff
	for ctx.Err() == nil {
		actx, cancel := context.WithTimeout(ctx, p.Timeout)
		err := p.Fn(actx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		next := p.Interval
		if err != nil {
			next = backoff
			streak.Fail(p.Log, p.Name+" poll failed", "err", err, "retry_in", next)
			failing = true
			backoff *= 2
			if backoff > p.Interval {
				backoff = p.Interval
			}
		} else {
			if failing {
				p.Log.Info(p.Name + " poll recovered")
			}
			streak.Reset()
			failing = false
			backoff = p.MinBackoff
		}
		if !wait(ctx, next) {
			return
		}
	}
}
