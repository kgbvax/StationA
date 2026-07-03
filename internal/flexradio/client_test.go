package flexradio

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// pipeConn adapts a single net.Pipe end into a connection we can pass to
// newClientFromConn. net.Pipe already satisfies net.Conn.
//
// The test harness "radio" side writes SmartSDR-style lines into one end
// and reads commands from it; the client under test sits on the other end.

func TestHandshake_SendsExpectedCommands(t *testing.T) {
	clientConn, radioConn := net.Pipe()
	client := newClientFromConn(clientConn)

	// On the radio side, log commands the client sends and feed a version
	// banner so readVersionBanner returns. net.Pipe is synchronous, so the
	// reader and writer must run concurrently to avoid deadlock.
	var gotCmds []string
	var mu sync.Mutex
	radioCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Writer: sends the version banner (and any unsolicited status). Runs
	// concurrently so the reader below never blocks on a write.
	go func() {
		_, _ = io.WriteString(radioConn, "S0|version v3.4.1.10\n")
		<-radioCtx.Done()
	}()

	// Reader: collects client commands until we see "meter list".
	go func() {
		defer radioConn.Close()
		sc := bufio.NewScanner(radioConn)
		for sc.Scan() {
			line := sc.Text()
			mu.Lock()
			gotCmds = append(gotCmds, line)
			mu.Unlock()
			if strings.Contains(line, "meter list") {
				cancel()
				return
			}
		}
	}()

	ctx, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	if err := client.Handshake(ctx, 4991); err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	client.Close()

	// Wait for the radio goroutine to finish capturing.
	<-radioCtx.Done()
	time.Sleep(20 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(gotCmds, "\n")
	wantSubs := []string{
		"client udpport 4991",
		"sub slice all",
		"sub radio all",
		"sub interlock all",
		"sub atu all",
		"sub meter all",
		"meter list",
	}
	for _, w := range wantSubs {
		if !strings.Contains(joined, w) {
			t.Errorf("missing command %q in:\n%s", w, joined)
		}
	}
}

func TestRun_DispatchesStatusFrames(t *testing.T) {
	clientConn, radioConn := net.Pipe()
	client := newClientFromConn(clientConn)

	var got []string
	var mu sync.Mutex
	client.SetHandler(func(f Frame) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, f.Topic)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- client.Run(ctx) }()

	// Feed two status lines, then close.
	_, _ = io.WriteString(radioConn, "S0|interlock state=RECEIVING\n")
	_, _ = io.WriteString(radioConn, "S0|slice 0 0 freq=14.100.000 mode=USB active=1 tx=0\n")
	time.Sleep(50 * time.Millisecond)
	radioConn.Close()

	<-runErr

	mu.Lock()
	defer mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("got %d frames, want >= 2 (%v)", len(got), got)
	}
	// Both topics should have been observed.
	seen := map[string]bool{}
	for _, topic := range got {
		seen[topic] = true
	}
	for _, want := range []string{"interlock", "slice"} {
		if !seen[want] {
			t.Errorf("did not see topic %q (got %v)", want, got)
		}
	}
}

func TestRun_CtxCancel(t *testing.T) {
	clientConn, radioConn := net.Pipe()
	defer clientConn.Close()
	defer radioConn.Close()
	client := newClientFromConn(clientConn)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		_ = err // expected to return on cancel/close
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}
