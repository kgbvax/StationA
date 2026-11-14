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
			// Echo the command's seq, as SmartSDR does: C<seq>|cmd -> R<seq>|...
			seq := strings.TrimPrefix(line[:strings.IndexByte(line, '|')], "C")
			reply := "R" + seq + "|0|OK\n"
			if strings.Contains(line, "|version") {
				reply = "R" + seq + "|0|0|v3.4.1.10\n"
			}
			if strings.Contains(line, "|info") {
				reply = "R" + seq + "|0|model=\"FLEX-8400\",chassis_serial=\"test-1234\"\n"
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
		"sub pan all",
		"sub tx all", // transmit status for the power ceiling
		"|info",
		"sub dvk all", // SmartSDR v4+ DVK status stream (best-effort, fire-and-forget)
		// `profile mic info` is also sent here (fire-and-forget) but is not
		// asserted in this net.Pipe test: the goroutine blocks on the
		// fire-and-forget reply after `sub dvk all` and never reads the next
		// command. It is covered deterministically by TestSend_MicProfile.
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
		if got != "C2|dvk playback_stop id=3" {
			t.Errorf("DVKStop sent %q, want C2|dvk playback_stop id=3", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DVKStop: no command sent")
	}
}

// TestSend_MicProfile asserts SetMicProfile emits the expected SmartSDR wire
// string, including a double-quoted name with spaces. The radio end of the pipe
// is drained by a reader goroutine so the synchronous net.Pipe write does not
// deadlock.
func TestSend_MicProfile(t *testing.T) {
	clientConn, radioConn := net.Pipe()
	client := newClientFromConn(clientConn)
	defer client.Close()

	cmds := make(chan string, 1)
	go func() {
		defer radioConn.Close()
		sc := bufio.NewScanner(radioConn)
		for sc.Scan() {
			cmds <- sc.Text()
		}
	}()

	if err := client.SetMicProfile("Default ProSet HC6"); err != nil {
		t.Fatalf("SetMicProfile: %v", err)
	}
	select {
	case got := <-cmds:
		if got != `C1|profile mic load "Default ProSet HC6"` {
			t.Errorf("SetMicProfile sent %q, want %q", got, `C1|profile mic load "Default ProSet HC6"`)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SetMicProfile: no command sent")
	}

}

func TestSend_SliceRxAnt(t *testing.T) {
	clientConn, radioConn := net.Pipe()
	client := newClientFromConn(clientConn)
	defer client.Close()

	cmds := make(chan string, 1)
	go func() {
		defer radioConn.Close()
		sc := bufio.NewScanner(radioConn)
		for sc.Scan() {
			cmds <- sc.Text()
		}
	}()

	if err := client.SetSliceRxAnt(1, "RX_A"); err != nil {
		t.Fatalf("SetSliceRxAnt: %v", err)
	}
	select {
	case got := <-cmds:
		if want := "C1|slice set 1 rxant=RX_A"; got != want {
			t.Errorf("SetSliceRxAnt sent %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SetSliceRxAnt: no command sent")
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

// ----- liveness probe (Run-owned; single reader) -------------------------------

// newProbeClient builds a client with shrunk probe bounds.
func newProbeClient(conn net.Conn, interval, timeout time.Duration) *Client {
	c := newClientFromConn(conn)
	c.probeInterval = interval
	c.probeTimeout = timeout
	return c
}

// A frozen peer (accepts commands, NEVER replies) must end Run within
// probeInterval + probeTimeout. Pre-fix, Run blocked in ReadString forever and
// the bridge's 5 s heartbeat kept laundering stale state as fresh.
func TestRun_ProbeTimesOutOnFrozenPeer(t *testing.T) {
	clientConn, radioConn := net.Pipe()
	defer radioConn.Close()
	client := newProbeClient(clientConn, 30*time.Millisecond, 50*time.Millisecond)

	probes := make(chan string, 8)
	go func() {
		defer radioConn.Close()
		sc := bufio.NewScanner(radioConn)
		for sc.Scan() {
			select {
			case probes <- sc.Text():
			default:
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- client.Run(ctx) }()

	select {
	case err := <-done:
		if !strings.Contains(err.Error(), "probe") {
			t.Errorf("err = %v, want a probe-timeout error", err)
		}
		if el := time.Since(start); el > 2*time.Second {
			t.Errorf("frozen peer detected only after %s; probe bounds not honored", el)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run never returned against a frozen peer")
	}
	select {
	case p := <-probes:
		if !strings.Contains(p, "|version") {
			t.Errorf("probe sent %q, want an eliciting version probe", p)
		}
	default:
		t.Error("no eliciting probe was written before the timeout")
	}
}

// A quiet-but-alive peer must NOT be dropped: each idle gap gets a probe, the
// reply proves the wire, and Run keeps cycling until ctx cancels. This pins
// the on-change-only discipline — silence alone is never fatal.
func TestRun_ProbeKeepsQuietPeerAlive(t *testing.T) {
	clientConn, radioConn := net.Pipe()
	defer radioConn.Close()
	client := newProbeClient(clientConn, 40*time.Millisecond, 500*time.Millisecond)

	var mu sync.Mutex
	probeCount := 0
	go func() {
		defer radioConn.Close()
		sc := bufio.NewScanner(radioConn)
		for sc.Scan() {
			mu.Lock()
			probeCount++
			mu.Unlock()
			_, _ = io.WriteString(radioConn, "R1|0|0|v3.4.1.10\n")
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()

	time.Sleep(300 * time.Millisecond) // several idle-gap cycles
	mu.Lock()
	got := probeCount
	mu.Unlock()
	if got < 2 {
		t.Fatalf("radio saw %d probes in 300ms; idle-gap probing is not cycling", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestSend_PowerCeilingCommands(t *testing.T) {
	clientConn, radioConn := net.Pipe()
	client := newClientFromConn(clientConn)
	defer client.Close()

	cmds := make(chan string, 3)
	go func() {
		defer radioConn.Close()
		sc := bufio.NewScanner(radioConn)
		for sc.Scan() {
			cmds <- sc.Text()
		}
	}()

	for _, tc := range []struct {
		send func() error
		want string
	}{
		{func() error { return client.SetRFPower(40) }, "C1|transmit set rfpower=40"},
		{func() error { return client.SetTunePower(10) }, "C2|transmit set tunepower=10"},
		{func() error { return client.SetMaxPowerLevel(100) }, "C3|transmit set max_power_level=100"},
	} {
		if err := tc.send(); err != nil {
			t.Fatalf("%s: %v", tc.want, err)
		}
		select {
		case got := <-cmds:
			if got != tc.want {
				t.Errorf("sent %q, want %q", got, tc.want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: no command sent", tc.want)
		}
	}
}

// TestRun_DeliversCommandReplies pins the reply plumbing: every command gets
// its own seq, the radio's R<seq>|<code>|<message> reply maps back to that
// command, and a refusal reaches the reply handler with its code. Before this,
// Send dropped every reply, so a refused `slice tune` left no trace.
func TestRun_DeliversCommandReplies(t *testing.T) {
	clientConn, radioConn := net.Pipe()
	client := newClientFromConn(clientConn)
	defer client.Close()

	replies := make(chan Reply, 4)
	client.SetReplyHandler(func(r Reply) { replies <- r })

	// The fake radio refuses the tune and accepts the mode change, echoing
	// each command's seq. The error code and text are illustrative, not a
	// recorded FLEX-8400 reply.
	go func() {
		defer radioConn.Close()
		sc := bufio.NewScanner(radioConn)
		for sc.Scan() {
			line := sc.Text()
			seq := strings.TrimPrefix(line[:strings.IndexByte(line, '|')], "C")
			reply := "R" + seq + "|0|\n"
			if strings.Contains(line, "slice tune") {
				reply = "R" + seq + "|50000016|Slice not owned by client\n"
			}
			if _, err := io.WriteString(radioConn, reply); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	if err := client.TuneSlice(0, 21_108_300); err != nil {
		t.Fatalf("TuneSlice: %v", err)
	}
	if err := client.SetSliceMode(0, "USB"); err != nil {
		t.Fatalf("SetSliceMode: %v", err)
	}

	want := []Reply{
		{Seq: 1, Code: 0x50000016, Message: "Slice not owned by client", Command: "slice tune 0 21.108300 autopan=1"},
		{Seq: 2, Code: 0, Message: "", Command: "slice set 0 mode=USB"},
	}
	for _, w := range want {
		select {
		case got := <-replies:
			if got != w {
				t.Errorf("reply = %+v, want %+v", got, w)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no reply delivered for %q", w.Command)
		}
	}
}

func TestParseReply(t *testing.T) {
	for _, tc := range []struct {
		line string
		want Reply
	}{
		{"R7|0|", Reply{Seq: 7}},
		{"R7|0|0|v3.4.1.10", Reply{Seq: 7, Message: "0|v3.4.1.10"}},
		{"R12|50000016|", Reply{Seq: 12, Code: 0x50000016}},
		// Recorded from the FLEX-8400 (SmartSDR 4.3.16), 2026-11-14.
		{"R1|500000A3|Invalid subscription object name", Reply{Seq: 1, Code: 0x500000A3, Message: "Invalid subscription object name"}},
		{"R1|0|SmartSDR-MB=4.3.16.42034#PSoC-MBTRX=1.0.9.0", Reply{Seq: 1, Message: "SmartSDR-MB=4.3.16.42034#PSoC-MBTRX=1.0.9.0"}},
	} {
		f, err := ParseFrame(tc.line)
		if err != nil {
			t.Fatalf("ParseFrame(%q): %v", tc.line, err)
		}
		got, err := parseReply(f)
		if err != nil {
			t.Fatalf("parseReply(%q): %v", tc.line, err)
		}
		if got != tc.want {
			t.Errorf("parseReply(%q) = %+v, want %+v", tc.line, got, tc.want)
		}
	}
}
