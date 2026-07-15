package mqtt

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestEnqueueNonBlocking verifies Enqueue never blocks when the channel is full:
// it drops the job instead. This is the property that keeps paho handlers off the
// blocking publish path.
func TestEnqueueNonBlocking(t *testing.T) {
	jobs := make(chan func(), 1)
	// Fill the buffer.
	jobs <- func() {}

	done := make(chan struct{})
	go func() {
		Enqueue(jobs, func() {}) // would block on a send; must drop instead
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Enqueue blocked on a full channel instead of dropping")
	}
}

// TestRunJobsRunsClosures verifies RunJobs runs queued closures until ctx done.
func TestRunJobsRunsClosures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	jobs := make(chan func(), 4)

	var ran atomic.Int32
	for i := 0; i < 3; i++ {
		Enqueue(jobs, func() { ran.Add(1) })
	}

	done := make(chan struct{})
	go func() { RunJobs(ctx, jobs); close(done) }()

	if !waitCnt(&ran, 3, time.Second) {
		t.Fatalf("ran %d closures, want 3", ran.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunJobs did not return after ctx cancel")
	}
}

func waitCnt(c *atomic.Int32, want int32, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if c.Load() >= want {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return c.Load() >= want
}