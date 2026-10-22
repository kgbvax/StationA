// SPDX-License-Identifier: AGPL-3.0-or-later

package poll

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestScheduleAndBackoff(t *testing.T) {
	results := []error{nil, errors.New("a"), errors.New("b"), errors.New("c"), errors.New("d"), nil, errors.New("e")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waits []time.Duration
	i := 0
	p := Poller{
		Name: "t", Interval: 300 * time.Second, MinBackoff: 60 * time.Second, Timeout: time.Second,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Fn: func(actx context.Context) error {
			if _, ok := actx.Deadline(); !ok {
				t.Error("attempt without a timeout")
			}
			err := results[i]
			i++
			return err
		},
		Wait: func(_ context.Context, d time.Duration) bool {
			waits = append(waits, d)
			if len(waits) == len(results) {
				cancel()
				return false
			}
			return true
		},
	}
	p.Run(ctx)
	want := []time.Duration{300, 60, 120, 240, 300, 300, 60}
	if len(waits) != len(want) {
		t.Fatalf("waits = %v", waits)
	}
	for k := range want {
		if waits[k] != want[k]*time.Second {
			t.Fatalf("waits = %v, want %v s", waits, want)
		}
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Poller{Name: "t", Interval: time.Hour, MinBackoff: time.Hour, Timeout: time.Second,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Fn:  func(context.Context) error { return nil }}.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}
