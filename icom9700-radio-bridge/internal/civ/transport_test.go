package civ

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"
)

// fastOptions builds Options against the fake radio with test-scale timers.
func fastOptions(f *FakeRadio) Options {
	return Options{
		Host:            "127.0.0.1",
		ControlPort:     f.Addr().Port,
		CIVPort:         f.CIVPort(),
		Username:        "bridge",
		Password:        "hunter2",
		PingInterval:    50 * time.Millisecond,
		LossWatchdog:    300 * time.Millisecond,
		ReauthInterval:  200 * time.Millisecond,
		ReauthTimeout:   300 * time.Millisecond,
		HandshakeTO:     400 * time.Millisecond,
		AreYouThere:     25 * time.Millisecond,
		CIVSilence:      150 * time.Millisecond,
		TxRetention:     2 * time.Second,
		RxBuffer:        30 * time.Millisecond,
		HandshakeBudget: 2 * time.Second,
		Logger:          slog.Default(),
	}
}

func dialFake(t *testing.T, f *FakeRadio, o Options) *Client {
	t.Helper()
	ctx := context.Background()
	cli, err := Dial(ctx, o)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(cli.Close)
	return cli
}

// waitFrames reads n frames from the client or fails after d.
func waitFrames(t *testing.T, cli *Client, n int, d time.Duration) [][]byte {
	t.Helper()
	var out [][]byte
	deadline := time.After(d)
	for len(out) < n {
		select {
		case f := <-cli.Frames():
			out = append(out, f)
		case <-deadline:
			t.Fatalf("timed out waiting for %d frames, got %d", n, len(out))
		}
	}
	return out
}

// TestHandshakeHappyPath pins the exact packet sequence from connect to
// CI-V-stream-open (the plan's U2 verification: a wfview log can be diffed
// against this).
func TestHandshakeHappyPath(t *testing.T) {
	f := NewFakeRadio(t)
	cli := dialFake(t, f, fastOptions(f))

	if got := cli.RadioName(); got != "IC-9700" {
		t.Errorf("RadioName = %q, want IC-9700", got)
	}

	// The control stream must show the full handshake in order.
	var fams []string
	var auth02, requested int
	f.mu.Lock()
	for _, p := range f.ctrlPackets {
		switch {
		case prefixEqual(p, sigAreYouThere):
			fams = append(fams, "pkt3")
		case prefixEqual(p, sigReady):
			fams = append(fams, "pkt6")
		case len(p) == 128 && p[0] == 0x80:
			fams = append(fams, "login")
		case len(p) == 64 && p[0] == 0x40:
			fams = append(fams, "auth")
		case prefixEqual(p, sigRequestAnswer):
			fams = append(fams, "request")
			requested++
		}
	}
	auth02 = f.auth02Count
	f.mu.Unlock()

	want := []string{
		"pkt3", "pkt3", "pkt6", "pkt6", // are-you-there / ready exchange
		"login", "auth", // login, first auth 0x02 (wfview's single immediate auth)
		"request",
	}
	if len(fams) < len(want) {
		t.Fatalf("handshake sequence too short: %v", fams)
	}
	for i, w := range want {
		if fams[i] != w {
			t.Fatalf("handshake sequence at %d = %s, want %s (full: %v)", i, fams[i], w, fams)
		}
	}
	// Exactly one immediate auth (the 0x05 renewal rides the timer).
	if auth02 != 1 {
		t.Errorf("first auth (0x02) count = %d, want 1", auth02)
	}
	if requested == 0 {
		t.Errorf("stream request never sent")
	}

	// And the CI-V stream was opened with the open packet. (The datagram is
	// in flight the moment Dial returns — poll for the fake to log it.)
	deadline := time.Now().Add(time.Second)
	for {
		f.mu.Lock()
		opens := f.civOpened
		f.mu.Unlock()
		if opens >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("civ open packets = %d, want 1", opens)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestHandshakeRadioSilent: the radio never answers are-you-there — Dial
// fails with ErrHandshakeTimeout and makes no login attempt (R2: no
// session attempts except after the handshake answers).
func TestHandshakeRadioSilent(t *testing.T) {
	f := NewFakeRadio(t)
	f.mu.Lock()
	f.silent = true
	f.mu.Unlock()

	o := fastOptions(f)
	o.HandshakeBudget = 200 * time.Millisecond
	ctx := context.Background()
	cli, err := Dial(ctx, o)
	if err == nil {
		cli.Close()
		t.Fatal("Dial succeeded against a silent radio")
	}
	if !errors.Is(err, ErrHandshakeTimeout) {
		t.Fatalf("err = %v, want ErrHandshakeTimeout", err)
	}
	logins, _, _, _, _ := f.counts()
	if logins != 0 {
		t.Errorf("login attempts = %d, want 0 (no login without are-you-there)", logins)
	}
}

// TestLoginRejected: wrong credentials are surfaced verbatim and never
// retried (the operator fixes them, the bridge reports).
func TestLoginRejected(t *testing.T) {
	f := NewFakeRadio(t)
	f.mu.Lock()
	f.refuseLogin = true
	f.mu.Unlock()

	cli, err := Dial(context.Background(), fastOptions(f))
	if err == nil {
		cli.Close()
		t.Fatal("Dial succeeded with refused login")
	}
	if !errors.Is(err, ErrLoginRejected) {
		t.Fatalf("err = %v, want ErrLoginRejected", err)
	}
	logins, _, _, _, _ := f.counts()
	if logins != 1 {
		t.Errorf("login attempts = %d, want exactly 1", logins)
	}
}

// TestSessionRefused: another client holds the session — the 0x50 ff ff ff
// answer surfaces as ErrConnectionRefused.
func TestSessionRefused(t *testing.T) {
	f := NewFakeRadio(t)
	f.mu.Lock()
	f.refuseSess = true
	f.mu.Unlock()

	cli, err := Dial(context.Background(), fastOptions(f))
	if err == nil {
		cli.Close()
		t.Fatal("Dial succeeded against a refused session")
	}
	if !errors.Is(err, ErrConnectionRefused) {
		t.Fatalf("err = %v, want ErrConnectionRefused", err)
	}
}

// The real radio's held-session login reject (bench 2026-09-20): a 20-byte
// 81 ff ff ff packet, then the radio keeps the control connection up and
// pings. Dial must surface ErrLoginBusy — and the abort must still send
// the control disconnect (0x05), or the radio stays busy for every later
// login attempt.
func TestLoginBusy(t *testing.T) {
	f := NewFakeRadio(t)
	f.SetBusyLogin(true)

	_, err := Dial(context.Background(), fastOptions(f))
	if err == nil {
		t.Fatal("Dial succeeded against a busy radio")
	}
	if !errors.Is(err, ErrLoginBusy) {
		t.Fatalf("err = %v, want ErrLoginBusy", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, pkt := range f.ctrlLog() {
			if len(pkt) >= 6 && pkt[4] == 0x05 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("abort sent no control disconnect (0x05) — the radio keeps the session busy")
}

// TestCIVRoundTrip: SendCIV reaches the fake with intact framing; inbound
// frames arrive ordered, payload-stripped.
func TestCIVRoundTrip(t *testing.T) {
	f := NewFakeRadio(t)
	cli := dialFake(t, f, fastOptions(f))

	// Outbound: 5 frames, strictly increasing inner sequence.
	for i := 0; i < 5; i++ {
		if err := cli.SendCIV([]byte{0xfe, 0xfe, 0xa2, 0xe0, 0x03, byte(i), 0xfd}); err != nil {
			t.Fatalf("SendCIV %d: %v", i, err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		log := f.civLog()
		got := 0
		inner := -1
		increasing := true
		for _, p := range log {
			if isData(p) {
				got++
				seq := int(binary.BigEndian.Uint16(p[19:21]))
				if seq <= inner {
					increasing = false
				}
				inner = seq
			}
		}
		if got == 5 {
			if !increasing {
				t.Error("inner data sequence not strictly increasing under concurrent sends")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d/5 data packets", got)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Inbound: the payload arrives stripped of header + sub-header. The
	// fake acks our cmd-03 probes first, so read past those.
	f.SendCIVFrame([]byte{0xfe, 0xfe, 0xa2, 0xe0, 0x15, 0x02, 0xfd}, false)
	want := []byte{0xfe, 0xfe, 0xa2, 0xe0, 0x15, 0x02, 0xfd}
	dl := time.After(3 * time.Second)
	for {
		select {
		case got := <-cli.Frames():
			// Skip every radio->controller reply (the fake's answers to
			// our cmd-03 probes); the injected controller-addressed frame
			// is the first one left.
			if len(got) >= 4 && got[0] == 0xfe && got[1] == 0xfe && got[2] == 0xe0 {
				continue
			}
			if string(got) != string(want) {
				t.Fatalf("frame = % x, want % x", got, want)
			}
			return
		case <-dl:
			t.Fatal("timed out waiting for the inbound frame")
		}
	}
}

// TestRxGapHealed: the fake withholds a frame; the client must detect the
// gap and request retransmission, after which frames arrive in order (the
// plan's packet-loss scenario, rx side).
func TestRxGapHealed(t *testing.T) {
	f := NewFakeRadio(t)
	cli := dialFake(t, f, fastOptions(f))

	f.SendCIVFrame([]byte{0x01}, false)
	f.SendCIVFrame([]byte{0x02}, true) // withheld — creates the gap
	f.SendCIVFrame([]byte{0x03}, false)

	// The fake serves the retransmit request from its tx log; the client
	// must deliver all three in order.
	var frames [][]byte
	func() {
		defer func() {
			if t.Failed() {
				var fams []string
				for _, p := range f.civLog() {
					switch {
					case prefixEqual(p, sigRetransmitSingle):
						fams = append(fams, "retx-req")
					case isData(p):
						fams = append(fams, "data")
					case isIdle(p):
						fams = append(fams, "idle")
					default:
						fams = append(fams, "other")
					}
				}
				t.Logf("civ log: %v", fams)
			}
		}()
		frames = waitFrames(t, cli, 3, 3*time.Second)
	}()
	for i, want := range [][]byte{{0x01}, {0x02}, {0x03}} {
		if string(frames[i]) != string(want) {
			t.Errorf("frame %d = % x, want % x (retransmit did not heal the gap in order)", i, frames[i], want)
		}
	}
}

// TestTxRetransmitServed: the radio loses a client packet and asks for it
// again; the client must resend the identical tracked datagram (twice, like
// the reference client).
func TestTxRetransmitServed(t *testing.T) {
	f := NewFakeRadio(t)
	cli := dialFake(t, f, fastOptions(f))

	payload := []byte{0xfe, 0xfe, 0xa2, 0xe0, 0x05, 0x00, 0x00, 0x50, 0x41, 0x01, 0xfd}
	if err := cli.SendCIV(payload); err != nil {
		t.Fatalf("SendCIV: %v", err)
	}

	// Learn the client's outer sequence for the packet from the fake's
	// receive log.
	deadline := time.Now().Add(2 * time.Second)
	var seq uint16
	found := false
	for time.Now().Before(deadline) {
		for _, p := range f.civLog() {
			if isData(p) {
				seq = dataSeq(p)
				found = true
			}
		}
		if found {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !found {
		t.Fatal("the client's data packet never reached the fake")
	}
	f.mu.Lock()
	to := f.cliCiv
	f.mu.Unlock()

	req := header(sigRetransmitSingle, 0, 0)
	binary.LittleEndian.PutUint16(req[6:8], seq)
	_, _ = f.civ.WriteToUDP(req, to)
	_, _ = f.civ.WriteToUDP(req, to)

	deadline = time.Now().Add(2 * time.Second)
	count := 0
	for count < 3 {
		log := f.civLog()
		count = 0
		for _, p := range log {
			if isData(p) && dataSeq(p) == seq {
				count++
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if count < 3 {
		t.Errorf("data packet seen %d times after retransmit request, want >= 3 (1 + two resent copies)", count)
	}
}

// TestTokenRenewal: renewals flow at the configured cadence while live and
// the session stays up (KTD: token renewal every 60 s — shrunk here).
func TestTokenRenewal(t *testing.T) {
	f := NewFakeRadio(t)
	o := fastOptions(f)
	o.ReauthInterval = 80 * time.Millisecond
	cli := dialFake(t, f, o)

	time.Sleep(400 * time.Millisecond)
	_, _, _, _, renewals := f.counts()
	if renewals < 2 {
		t.Errorf("renewals = %d in 400ms at an 80ms cadence, want >= 2", renewals)
	}
	select {
	case err := <-cli.Lost:
		t.Fatalf("session lost during renewals: %v", err)
	default:
	}
}

// TestCIVSilenceWatchdog: with no frames flowing the client re-opens the
// data stream at the configured cadence (the brief's 2 s watchdog, shrunk).
func TestCIVSilenceWatchdog(t *testing.T) {
	f := NewFakeRadio(t)
	o := fastOptions(f)
	o.CIVSilence = 100 * time.Millisecond
	cli := dialFake(t, f, o)
	_ = cli

	time.Sleep(450 * time.Millisecond)
	_, _, opens, _, _ := f.counts()
	if opens < 2 {
		t.Errorf("civ opens = %d after 450ms with a 100ms silence watchdog, want >= 2", opens)
	}
}

// TestSessionLossOnSilence: the radio stops answering entirely — the loss
// watchdog must end the session (R3's session-loss detection).
func TestSessionLossOnSilence(t *testing.T) {
	f := NewFakeRadio(t)
	cli := dialFake(t, f, fastOptions(f))

	f.SetSilent(true)
	select {
	case err := <-cli.Lost:
		if err == nil {
			t.Fatal("loss delivered nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session loss not detected within 2s of radio silence")
	}
}

// TestFloodBounded: a datagram flood must not wedge delivery or grow the
// queues unboundedly — after the flood the client still delivers fresh
// frames and stays live.
func TestFloodBounded(t *testing.T) {
	f := NewFakeRadio(t)
	cli := dialFake(t, f, fastOptions(f))

	for i := 0; i < 3000; i++ {
		f.SendCIVFrame([]byte{byte(i), 0x42}, false)
	}
	// The stream still works: a fresh frame arrives despite the flood.
	f.SendCIVFrame([]byte{0xfe, 0xfe, 0xa2, 0xe0, 0x19, 0xfd}, false)
	waitFrames(t, cli, 1, 3*time.Second)

	select {
	case err := <-cli.Lost:
		t.Fatalf("session lost under flood: %v", err)
	default:
	}
}

// TestConcurrentSendsSerialize: concurrent SendCIV callers must not tear
// the wire — every frame arrives, inner sequences strictly increasing.
func TestConcurrentSendsSerialize(t *testing.T) {
	f := NewFakeRadio(t)
	cli := dialFake(t, f, fastOptions(f))

	const n = 20
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			errs <- cli.SendCIV([]byte{0xfe, 0xfe, 0xa2, 0xe0, 0x1c, 0x00, byte(i), 0xfd})
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("SendCIV: %v", err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		log := f.civLog()
		seen := 0
		inner := -1
		ok := true
		for _, p := range log {
			if isData(p) {
				seen++
				s := int(binary.BigEndian.Uint16(p[19:21]))
				if s <= inner {
					ok = false
				}
				inner = s
			}
		}
		if seen == n {
			if !ok {
				t.Error("inner sequence not strictly increasing under concurrency")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d/%d data packets", seen, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestCloseClean: Close sends the close + disconnect packets and does not
// deliver a loss.
func TestCloseClean(t *testing.T) {
	f := NewFakeRadio(t)
	cli := dialFake(t, f, fastOptions(f))

	cli.Close()
	cli.Close() // idempotent

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, _, _, closes, _ := f.counts()
		if closes >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, _, _, closes, _ := f.counts()
	if closes == 0 {
		t.Error("close packet never sent")
	}
	select {
	case err := <-cli.Lost:
		t.Fatalf("clean close delivered a loss: %v", err)
	default:
	}
}

// TestLocalSIDDerivation checks the derived session-ID formula
// (IPv4 bytes << 16 | port) on a known address.
func TestLocalSIDDerivation(t *testing.T) {
	ip := net.IPv4(192, 168, 1, 20).To4()
	want := uint64(0xc0a80114)<<16 | 50002
	sid := uint64(binary.BigEndian.Uint32(ip))<<16 | uint64(50002&0xffff)
	if sid != want {
		t.Fatalf("sid = %012x, want %012x", sid, want)
	}
}
