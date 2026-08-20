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

	// The radio side reads each C1|... command the client sends and replies
	// with an R1|0|... line (matching the real SmartSDR protocol: the client
	// sends first, the radio replies). net.Pipe is synchronous, so the
	// reader/writer run concurrently to avoid deadlock.
	var gotCmds []string
	var mu sync.Mutex
	radioCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		defer radioConn.Close()
		defer cancel()
		sc := bufio.NewScanner(radioConn)
		for sc.Scan() {
			line := sc.Text()
			mu.Lock()
			gotCmds = append(gotCmds, line)
			mu.Unlock()
			reply := "R1|0|OK\n"
			if strings.Contains(line, "|version") {
				reply = "R1|0|0|v3.4.1.10\n"
			}
			if strings.Contains(line, "|info") {
				reply = "R1|0|model=\"FLEX-8400\",chassis_serial=\"test-1234\"\n"
			}
			if _, err := io.WriteString(radioConn, reply); err != nil {
				return
			}
		}
	}()

	ctx, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	info, err := client.Handshake(ctx)
	if err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if info.Model != "FLEX-8400" || info.Serial != "test-1234" {
		t.Errorf("RadioInfo = %+v, want Model=FLEX-8400 Serial=test-1234", info)
	}
	client.Close()

	select {
	case <-radioCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("radio goroutine did not finish; possible deadlock")
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(gotCmds, "\n")
	wantCmds := []string{
		"|version",
		"sub slice all",
		"sub radio all",
		"sub interlock all",
		"sub atu all",
		"|info",
		"sub dvk all", // SmartSDR v4+ DVK status stream (best-effort, fire-and-forget)
	}
	for _, w := range wantCmds {
		if !strings.Contains(joined, w) {
			t.Errorf("missing command %q in:\n%s", w, joined)
		}
	}
	for _, absent := range []string{"udpport", "meter"} {
		if strings.Contains(joined, absent) {
			t.Errorf("unexpected command containing %q in:\n%s", absent, joined)
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

// TestSend_DVK asserts DVKPlay/DVKStop emit the expected fire-and-forget
// SmartSDR wire strings. The radio end of the pipe is drained by a reader
// goroutine so the synchronous net.Pipe writes do not deadlock.
func TestSend_DVK(t *testing.T) {
	clientConn, radioConn := net.Pipe()
	client := newClientFromConn(clientConn)
	defer client.Close()

	cmds := make(chan string, 4)
	go func() {
		defer radioConn.Close()
		sc := bufio.NewScanner(radioConn)
		for sc.Scan() {
			cmds <- sc.Text()
		}
	}()

	if err := client.DVKPlay(3); err != nil {
		t.Fatalf("DVKPlay: %v", err)
	}
	select {
	case got := <-cmds:
		if got != "C1|dvk playback_start id=3" {
			t.Errorf("DVKPlay sent %q, want C1|dvk playback_start id=3", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DVKPlay: no command sent")
	}

	if err := client.DVKStop(3); err != nil {
		t.Fatalf("DVKStop: %v", err)
	}
	select {
	case got := <-cmds:
		if got != "C1|dvk playback_stop id=3" {
			t.Errorf("DVKStop sent %q, want C1|dvk playback_stop id=3", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DVKStop: no command sent")
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
