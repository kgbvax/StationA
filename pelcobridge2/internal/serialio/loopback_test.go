package serialio

import (
	"sync"
	"testing"
	"time"
)

// The loopback pair stands in for a serial link in engine and rotctld tests:
// what one side writes the other reads, Close unblocks a blocked Read, and the
// latency knob reproduces real wire turnaround ordering.

func TestPipeRoundTrip(t *testing.T) {
	a, b := Pair()
	defer a.Close()
	defer b.Close()

	if err := a.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 16)
	n, err := b.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("got %q", buf[:n])
	}
}

func TestPipeBidirectional(t *testing.T) {
	a, b := Pair()
	defer a.Close()
	defer b.Close()

	// Both directions work concurrently — the engine and the simulated head
	// write at each other over the same pair.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := a.Write([]byte{0x01}); err != nil {
			t.Errorf("a write: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := b.Write([]byte{0x02}); err != nil {
			t.Errorf("b write: %v", err)
		}
	}()
	wg.Wait()

	buf := make([]byte, 8)
	n, err := b.Read(buf) // a's write
	if err != nil || buf[0] != 0x01 {
		t.Fatalf("b read a: n=%d buf=% X err=%v", n, buf[:n], err)
	}
	n, err = a.Read(buf) // b's write
	if err != nil || buf[0] != 0x02 {
		t.Fatalf("a read b: n=%d buf=% X err=%v", n, buf[:n], err)
	}
}

func TestPipeLatencyPreservesOrder(t *testing.T) {
	a, b := Pair()
	defer a.Close()
	defer b.Close()
	a.SetLatency(5 * time.Millisecond)

	// Frames written back-to-back must arrive in write order even with
	// latency — the engine's frameGap relies on replies not overtaking.
	for i := 0; i < 5; i++ {
		if err := a.Write([]byte{byte(i)}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	buf := make([]byte, 1)
	for i := 0; i < 5; i++ {
		if _, err := b.Read(buf); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if buf[0] != byte(i) {
			t.Fatalf("out of order: got %d want %d", buf[0], i)
		}
	}
}

func TestPipeCloseUnblocksRead(t *testing.T) {
	a, b := Pair()
	defer a.Close()

	done := make(chan error, 1)
	go func() {
		_, err := b.Read(make([]byte, 8))
		done <- err
	}()

	// Let the reader block first, then close from this side.
	time.Sleep(20 * time.Millisecond)
	if err := b.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case err := <-done:
		if err != ErrClosed {
			t.Fatalf("blocked read after close: err=%v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not unblock the reader")
	}

	// Double close is a no-op.
	if err := b.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestPipeWriteAfterClose(t *testing.T) {
	a, b := Pair()
	defer b.Close()
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := a.Write([]byte("x")); err != ErrClosed {
		t.Fatalf("write after close: err=%v, want ErrClosed", err)
	}
}
