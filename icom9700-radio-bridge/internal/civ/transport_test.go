package civ

// Live-session tests: sequence tracking and retransmit, keepalives, ping,
// token renewal, the CI-V data watchdog, session loss, serialized sends,
// bounded buffers and the never-log pin for credential-derived bytes.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// bufLogger is a slog logger capturing every line at every level — the
// never-log assertions read the raw buffer bytes.
type bufLogger struct {
	buf bytes.Buffer
	mu  sync.Mutex
	l   *slog.Logger
}

func newBufLogger() *bufLogger {
	bl := &bufLogger{}
	bl.l = slog.New(slog.NewTextHandler(bl, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return bl
}

func (bl *bufLogger) Write(p []byte) (int, error) {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	return bl.buf.Write(p)
}

func testLogger(*testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func (bl *bufLogger) String() string {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	return bl.buf.String()
}

func (bl *bufLogger) Bytes() []byte {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	return append([]byte(nil), bl.buf.Bytes()...)
}

// frameCollector gathers CI-V chunks delivered through OnCIVFrame.
type frameCollector struct {
	mu     sync.Mutex
	frames [][]byte
}

func (fc *frameCollector) add(chunk []byte) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.frames = append(fc.frames, append([]byte(nil), chunk...))
}

func (fc *frameCollector) all() [][]byte {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	out := make([][]byte, len(fc.frames))
	copy(out, fc.frames)
	return out
}

func (fc *frameCollector) count() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return len(fc.frames)
}

// dialLive performs a full handshake against fr with the test cadences plus
// per-test overrides, and registers cleanup.
func dialLive(t *testing.T, fr *fakeRadio, mut func(*Opts)) *Transport {
	t.Helper()
	o := testOpts(fr, testLogger(t))
	if mut != nil {
		mut(&o)
	}
	tr, err := Dial(context.Background(), o)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { tr.Close() })
	if !tr.Live() {
		t.Fatal("transport not live after dial")
	}
	return tr
}

func TestCIVSendReceiveRoundTrip(t *testing.T) {
	fr := newFakeRadio(t)
	fc := &frameCollector{}
	tr := dialLive(t, fr, func(o *Opts) { o.OnCIVFrame = fc.add })

	// Client -> radio: one frame, arriving byte-intact after the sub-header.
	frame := []byte{0xfe, 0xfe, 0xa2, 0xe0, 0x03, 0xfd}
	if err := tr.SendCIV(frame); err != nil {
		t.Fatalf("SendCIV: %v", err)
	}
	waitFor(t, time.Second, "frame at the fake radio", func() bool {
		return len(fr.civFramesReceived()) == 1
	})
	if got := fr.civFramesReceived()[0]; !bytes.Equal(got, frame) {
		t.Errorf("radio got % x, want % x", got, frame)
	}

	// Radio -> client: datalen-based parse. The fake may put arbitrary bytes
	// (0xFD included) anywhere in the payload — framing never scans for FD,
	// so the chunk must arrive exactly as sent, datalen bytes of it.
	padded := []byte{0xfe, 0xfe, 0xa2, 0xe0, 0x19, 0x01, 0xfd, 0xfd, 0xfd}
	fr.injectCIV(padded)
	waitFor(t, time.Second, "chunk delivered", func() bool { return fc.count() >= 1 })
	if got := fc.all()[0]; !bytes.Equal(got, padded) {
		t.Errorf("chunk = % x, want the exact datalen payload % x", got, padded)
	}
}

func TestPacketLossRetransmitRecoversWithoutDuplicates(t *testing.T) {
	fr := newFakeRadio(t)
	fc := &frameCollector{}
	dialLive(t, fr, func(o *Opts) { o.OnCIVFrame = fc.add })

	const total = 30
	// The radio "loses" 5 of its outgoing data packets (spread out); the
	// client must detect the sequence gaps, request retransmission and
	// deliver every frame exactly once, in order.
	lost := map[int]bool{3: true, 9: true, 14: true, 22: true, 27: true}
	for i := 0; i < total; i++ {
		if lost[i] {
			fr.mu.Lock()
			fr.dropOut[sockCiv]++
			fr.mu.Unlock()
		}
		fr.injectCIV([]byte{0xfe, 0xfe, 0xa2, 0xe0, 0x00, byte(i), 0xfd})
	}

	waitFor(t, 3*time.Second, "all 30 frames delivered exactly once after retransmit", func() bool {
		return fc.count() == total
	})
	got := fc.all()
	seen := map[string]int{}
	for i, f := range got {
		key := fmt.Sprintf("%x", f)
		seen[key]++
		if f[5] != byte(i) {
			t.Fatalf("frame %d carries marker %d — frames must deliver in sequence order", i, f[5])
		}
	}
	for key, n := range seen {
		if n != 1 {
			t.Fatalf("frame %s delivered %d times, duplicates are forbidden", key, n)
		}
	}
}

func TestRadioRetransmitRequestRecoversClientPackets(t *testing.T) {
	fr := newFakeRadio(t)
	tr := dialLive(t, fr, nil)

	// The client's first three CI-V data datagrams are lost on the way to
	// the radio; the radio notices the sequence gap, requests retransmission
	// and the client must re-send from its tx window.
	fr.dropIncomingData(sockCiv, 3)
	for i := 0; i < 6; i++ {
		if err := tr.SendCIV([]byte{0xfe, 0xfe, 0xa2, 0xe0, 0x05, byte(i), 0xfd}); err != nil {
			t.Fatalf("SendCIV: %v", err)
		}
	}
	waitFor(t, 3*time.Second, "all 6 frames at the radio after retransmit", func() bool {
		return len(fr.civFramesReceived()) == 6
	})
	// Every frame is at the radio exactly once. The re-sent frames
	// legitimately arrive after the live ones (retransmit is late by
	// nature), so this is a completeness/dedup check, not an order check.
	seen := map[byte]int{}
	for _, f := range fr.civFramesReceived() {
		seen[f[5]]++
	}
	for i := 0; i < 6; i++ {
		if seen[byte(i)] != 1 {
			t.Errorf("frame marker %d at the radio %d times, want exactly once", i, seen[byte(i)])
		}
	}
	reqs := 0
	for _, p := range fr.sentOn(sockCiv) {
		if len(p.data) >= ctrlLen && p.data[typeOff] == ptRetransmit {
			reqs++
		}
	}
	if reqs == 0 {
		t.Error("the radio issued no retransmit requests — the fake's loss script did not engage")
	}
}

func TestClientAnswersRadioRetransmitWithIdle(t *testing.T) {
	fr := newFakeRadio(t)
	dialLive(t, fr, nil)

	// A radio retransmit request for a seq the client never sent (nothing in
	// the client tx window) must be answered with a 16-byte idle carrying
	// that seq — the wfview behavior that keeps the radio's own retransmit
	// loop from spinning on an answer it can use.
	fr.mu.Lock()
	req := make([]byte, ctrlLen)
	putHeader(req, header{len: ctrlLen, typ: ptRetransmit, seq: 4242, sentID: fakeRadioID})
	fr.send(sockCiv, req) // radio -> client single-packet retransmit request
	fr.mu.Unlock()

	waitFor(t, time.Second, "16-byte idle answer for the unknown seq", func() bool {
		for _, p := range fr.recvOn(sockCiv) {
			if len(p.data) == ctrlLen && p.data[typeOff] == ptIdle {
				if h := parseHeader(p.data); h.seq == 4242 {
					return true
				}
			}
		}
		return false
	})
}

func TestTokenRenewalFiresWhileLive(t *testing.T) {
	fr := newFakeRadio(t)
	tr := dialLive(t, fr, nil) // TokenRenewal shrunk to 150ms

	// The handshake itself sends one renewal (requesttype 0x05); the live
	// renewal loop must keep firing within the configured window.
	waitFor(t, 2*time.Second, "at least 3 token renewals at the fake", func() bool {
		renew, _, _, _ := fr.counts()
		return renew >= 3
	})
	if !tr.Live() {
		t.Fatal("transport must stay live across successful renewals")
	}
}

func TestCivWatchdogReopensSilentStream(t *testing.T) {
	fr := newFakeRadio(t)
	dialLive(t, fr, nil) // CivSilence 200ms, StartDataPeriod 60ms

	// No CI-V data flows: after the silence bound the watchdog must re-send
	// the start-data (open) packet, repeating at the start-data period.
	waitFor(t, 2*time.Second, "watchdog re-open packets", func() bool {
		_, _, opens, _ := fr.counts()
		return opens >= 3 // initial open + at least 2 watchdog re-opens
	})

	// Data arriving disarms the watchdog: open count freezes while frames flow.
	fr.startAutoCiv(40 * time.Millisecond)
	waitFor(t, 2*time.Second, "auto civ frames flowing", func() bool {
		return fr.framesSentCount() >= 3
	})
	_, _, opensBefore, _ := fr.counts()
	time.Sleep(400 * time.Millisecond)
	_, _, opensAfter, _ := fr.counts()
	if opensAfter != opensBefore {
		t.Errorf("open packets while data flowing: %d -> %d — watchdog must disarm on data", opensBefore, opensAfter)
	}
	fr.stopAutoCiv()
}

func TestSessionLossOnKeepaliveSilence(t *testing.T) {
	fr := newFakeRadio(t)
	var lost atomic.Value // error
	ready := make(chan struct{}, 1)
	tr := dialLive(t, fr, func(o *Opts) {
		o.OnSessionLoss = func(err error) {
			lost.Store(err)
			select {
			case ready <- struct{}{}:
			default:
			}
		}
	})

	fr.stopAnswering()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("session-loss callback never fired after the radio went silent")
	}
	if err, _ := lost.Load().(error); !errors.Is(err, ErrSessionLost) {
		t.Fatalf("session-loss error = %v, want ErrSessionLost", err)
	}
	if tr.Live() {
		t.Error("transport must report not-live after session loss")
	}
	if err := tr.SendCIV([]byte{0xfe, 0xfd}); !errors.Is(err, ErrSessionLost) {
		t.Errorf("SendCIV after session loss = %v, want ErrSessionLost", err)
	}
}

func TestSessionLossOnRenewalRejection(t *testing.T) {
	fr := newFakeRadio(t)
	lost := make(chan error, 1)
	tr := dialLive(t, fr, func(o *Opts) {
		o.OnSessionLoss = func(err error) { lost <- err }
	})

	// The radio rejects the next renewal with response 0xffffffff (session
	// taken over / gone stale) — that must surface as session loss (R3: a
	// full re-login path, never a resume).
	fr.mu.Lock()
	p := make([]byte, tokenLen)
	putHeader(p, header{len: tokenLen, sentID: fakeRadioID, rcvdID: tr.ctrl.myID})
	p[reqReplyOff] = 0x02
	p[reqTypeOff] = 0x05
	putLE32(p, errOff, 0xffffffff)
	fr.send(sockCtrl, p)
	fr.mu.Unlock()

	select {
	case err := <-lost:
		if !errors.Is(err, ErrSessionLost) {
			t.Fatalf("session-loss error = %v, want ErrSessionLost", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("renewal rejection must surface as session loss")
	}
}

func TestConcurrentSendsSerialize(t *testing.T) {
	fr := newFakeRadio(t)
	tr := dialLive(t, fr, nil)

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i byte) {
			defer wg.Done()
			if err := tr.SendCIV([]byte{0xfe, 0xfe, 0xa2, 0xe0, 0x1c, 0x00, i, 0xfd}); err != nil {
				errs <- err
			}
		}(byte(i))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent SendCIV: %v", err)
	}

	waitFor(t, 2*time.Second, "all 8 frames at the radio", func() bool {
		return len(fr.civFramesReceived()) == n
	})
	// Serialization: the sub-header sendseq counter is assigned under the
	// send lock, so the seqs the radio saw must be strictly increasing in
	// arrival order and all distinct.
	seqs := fr.civSubSeqsReceived()
	seen := map[uint16]bool{}
	for i, s := range seqs {
		if seen[s] {
			t.Fatalf("sub-header sendseq %d reused — concurrent sends did not serialize", s)
		}
		seen[s] = true
		if i > 0 && s <= seqs[i-1] {
			t.Fatalf("sendseqs not strictly increasing at %d: %v", i, seqs)
		}
	}
	// Every frame arrived intact (no interleaved corruption).
	for _, f := range fr.civFramesReceived() {
		if len(f) != 8 || f[0] != 0xfe || f[7] != 0xfd {
			t.Fatalf("corrupted frame % x — concurrent sends interleaved", f)
		}
	}
}

func TestBuffersBoundedUnderFlood(t *testing.T) {
	fr := newFakeRadio(t)
	fc := &frameCollector{}
	tr := dialLive(t, fr, func(o *Opts) { o.OnCIVFrame = fc.add })

	// Flood: 2000 sequential frames in paced batches — fast enough that the
	// reader runs behind the writer and loss + retransmit recovery runs
	// mid-test, paced enough that the loopback kernel buffer never drops a
	// whole window at once (a burst loss beyond the protocol's own >50-missing
	// flush bound is unrecoverable BY DESIGN — wfview flushes and resyncs).
	// The plan's assertion is bounded growth: every buffer stays capped
	// throughout and delivery is exactly-once.
	const total = 2000
	const batch = 100
	for sent := 0; sent < total; sent += batch {
		for i := sent; i < sent+batch && i < total; i++ {
			fr.injectCIV([]byte{0xfe, 0xfe, 0xa2, 0xe0, 0x00, byte(i), byte(i >> 8), 0xfd})
		}
		// Let the reader drain before deepening the queue.
		waitFor(t, 5*time.Second, "reader to drain the batch", func() bool {
			return fc.count() >= sent+batch-10 || fc.count() >= total-10
		})
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			time.Sleep(50 * time.Millisecond)
			s := tr.Stats()
			for k, v := range s {
				if v > txWindowCap {
					t.Errorf("during flood: stats[%s] = %d exceeds the tx-window cap %d", k, v, txWindowCap)
				}
			}
			if s["civ_pending"] > maxMissingCap || s["civ_missing"] > maxMissingCap {
				t.Errorf("during flood: rx window unbounded: %v", s)
			}
		}
	}()

	waitFor(t, 30*time.Second, "delivery converges (loss recovery included)", func() bool {
		return fc.count() >= 98*total/100
	})
	<-done
	// Delivery must be exactly-once (no duplicates — that is the invariant
	// the retransmit machinery owes the consumer) and monotonic within the
	// pre-flush and post-flush runs.
	frames := fc.all()
	seen := map[int]bool{}
	prev, runs := -1, 1
	for _, f := range frames {
		got := int(f[5]) | int(f[6])<<8
		if seen[got] {
			t.Fatalf("frame %d delivered twice under flood", got)
		}
		seen[got] = true
		if got < prev {
			runs++ // a flush resync starts a new monotonic run
		}
		prev = got
	}
	if runs > 3 {
		t.Errorf("%d separate delivery runs — the stream should not resync repeatedly", runs)
	}
	s := tr.Stats()
	for k, v := range s {
		if v > txWindowCap {
			t.Fatalf("after flood: stats[%s] = %d exceeds the tx-window cap %d", k, v, txWindowCap)
		}
	}
	if s["civ_pending"] > maxMissingCap || s["civ_missing"] > maxMissingCap {
		t.Fatalf("after flood: rx window unbounded: %v", s)
	}
}

func TestSeqGapBeyondFlushThresholdResyncs(t *testing.T) {
	fr := newFakeRadio(t)
	fc := &frameCollector{}
	tr := dialLive(t, fr, func(o *Opts) {
		o.OnCIVFrame = fc.add
		o.MaxMissing = 10 // shrink the flush bound for the test
	})

	// Frames 1..5, then a jump straight to seq 100: the 94-packet hole
	// exceeds the flush threshold, buffers must flush and the stream must
	// resync on the new seq instead of buffering 94 missing entries forever.
	for s := uint16(1); s <= 5; s++ {
		fr.injectCIVAt(s, []byte{0xfd, byte(s)})
	}
	fr.injectCIVAt(100, []byte{0xfd, 100})
	fr.injectCIVAt(101, []byte{0xfd, 101})

	waitFor(t, 2*time.Second, "post-flush frames delivered", func() bool {
		return fc.count() >= 7
	})
	frames := fc.all()
	// First five in order, then the resync pair; the hole never emitted.
	for i := 0; i < 5; i++ {
		if frames[i][1] != byte(i+1) {
			t.Fatalf("frame %d = %d, want %d", i, frames[i][1], i+1)
		}
	}
	if frames[5][1] != 100 || frames[6][1] != 101 {
		t.Fatalf("after flush expected 100,101, got %d,%d", frames[5][1], frames[6][1])
	}
	if s := tr.Stats(); s["civ_pending"] > 10 || s["civ_missing"] > 10 {
		t.Fatalf("rx buffers exceeded the shrunk flush bound after resync: %+v", s)
	}
}

func TestLostFrameGiveUpAdvancesStream(t *testing.T) {
	fr := newFakeRadio(t)
	fc := &frameCollector{}
	tr := dialLive(t, fr, func(o *Opts) {
		o.OnCIVFrame = fc.add
		o.RetransmitTries = 2
	})

	// The radio permanently loses one of its packets (unreachable by
	// retransmit). After the give-up bound the client must advance past the
	// hole and keep delivering later frames instead of stalling.
	fr.injectCIV([]byte{0xfd, 1})
	fr.mu.Lock()
	fr.dropOut[sockCiv]++ // the NEXT radio datagram is dropped in transit
	fr.injectCIVLocked([]byte{0xfd, 2})
	lostSeq := fr.civSeq // the just-tracked seq is the dropped one
	fr.lostCiv[lostSeq] = true
	fr.mu.Unlock()
	fr.injectCIV([]byte{0xfd, 3})
	fr.injectCIV([]byte{0xfd, 4})

	waitFor(t, 3*time.Second, "frames beyond the permanent hole (1,3,4)", func() bool {
		return fc.count() >= 3
	})
	got := map[byte]bool{}
	var delivered [][]byte
	for _, f := range fc.all() {
		got[f[1]] = true
		delivered = append(delivered, f)
	}
	for _, want := range []byte{1, 3, 4} {
		if !got[want] {
			t.Errorf("frame %d missing after give-up: delivered %v", want, delivered)
		}
	}
	if got[2] {
		t.Error("the permanently lost frame must not appear")
	}
	if s := tr.Stats(); s["civ_pending"] > 4 || s["civ_missing"] > 4 {
		t.Fatalf("rx buffers grew past the hole: %+v", s)
	}
}

func TestCredentialsNeverLogged(t *testing.T) {
	fr := newFakeRadio(t)
	bl := newBufLogger()
	o := testOpts(fr, bl.l)
	tr, err := Dial(context.Background(), o)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer tr.Close()

	// Run a renewal and traffic through the session so every code path that
	// could conceivably log has run at debug level.
	waitFor(t, 2*time.Second, "renewal fired", func() bool {
		renew, _, _, _ := fr.counts()
		return renew >= 2
	})
	if err := tr.SendCIV([]byte{0xfe, 0xfe, 0xa2, 0xe0, 0x19, 0xfd}); err != nil {
		t.Fatalf("SendCIV: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	out := bl.Bytes()
	for _, secret := range []string{"operator1", "s3cret!"} {
		if bytes.Contains(out, []byte(secret)) {
			t.Errorf("credential %q appears in slog output — credential-bearing packets are never logged", secret)
		}
	}
	// The substituted (wire) forms of both secrets must not leak either —
	// the substitution table is public, so captured bytes ARE the password.
	encUser, encPass := passcode("operator1"), passcode("s3cret!")
	for _, enc := range [][]byte{encUser[:], encPass[:]} {
		if bytes.Contains(out, enc) {
			t.Errorf("substituted credential bytes % x appear in slog output", enc)
		}
	}
	// The session token must not leak either.
	var tok [4]byte
	binary.LittleEndian.PutUint32(tok[:], fakeToken)
	if bytes.Contains(out, tok[:]) {
		t.Error("session token bytes appear in slog output")
	}
	if len(out) == 0 {
		t.Fatal("debug logging produced no output — the capture is not exercising the log paths")
	}
}

func TestCloseCleanDisconnect(t *testing.T) {
	fr := newFakeRadio(t)
	tr := dialLive(t, fr, nil)

	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitFor(t, time.Second, "clean-disconnect packets at the radio", func() bool {
		// All four courtesy packets, on both sockets — the civ-socket pair
		// and the ctrl-socket pair arrive on different UDP sockets, so their
		// relative order genuinely races.
		_, logout, _, closes := fr.counts()
		if logout < 1 || closes < 1 {
			return false
		}
		foundCtrlLogout, foundTokenRemoval := false, false
		for _, p := range fr.recvOn(sockCtrl) {
			if len(p.data) == ctrlLen && p.data[typeOff] == ptLogout {
				foundCtrlLogout = true
			}
			if len(p.data) == tokenLen && p.data[typeOff] == ptIdle && p.data[reqTypeOff] == 0x01 {
				foundTokenRemoval = true
			}
		}
		return foundCtrlLogout && foundTokenRemoval
	})
	// CI-V stream: close (openclose magic 0x00) plus a control 0x05 there;
	// control stream: token removal (0x40 requesttype 0x01) plus a 0x05.
	foundCivClose, foundCivLogout := false, false
	for _, p := range fr.recvOn(sockCiv) {
		if len(p.data) == openCloseLen && p.data[magicOff] == 0x00 {
			foundCivClose = true
		}
		if len(p.data) == ctrlLen && p.data[typeOff] == ptLogout {
			foundCivLogout = true
		}
	}
	if !foundCivClose {
		t.Error("no openclose-close (magic 0x00) on the civ socket")
	}
	if !foundCivLogout {
		t.Error("no control disconnect (type 0x05) on the civ socket")
	}
	foundCtrlLogout, foundTokenRemoval := false, false
	for _, p := range fr.recvOn(sockCtrl) {
		if len(p.data) == ctrlLen && p.data[typeOff] == ptLogout {
			foundCtrlLogout = true
		}
		if len(p.data) == tokenLen && p.data[typeOff] == ptIdle && p.data[reqTypeOff] == 0x01 {
			foundTokenRemoval = true
		}
	}
	if !foundCtrlLogout {
		t.Error("no control disconnect (type 0x05) on the control socket")
	}
	if !foundTokenRemoval {
		t.Error("no token-removal packet (0x40 requesttype 0x01) on the control socket")
	}
	if tr.Live() {
		t.Error("Live() must be false after Close")
	}
	if err := tr.SendCIV([]byte{0xfe, 0xfd}); !errors.Is(err, ErrClosed) {
		t.Errorf("SendCIV after Close = %v, want ErrClosed", err)
	}
	// Idempotent.
	if err := tr.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

func TestPingRequestAnswered(t *testing.T) {
	fr := newFakeRadio(t)
	dialLive(t, fr, nil)

	// Unsolicited ping request from the radio: the client answers with the
	// reply flag set, echoing seq and uptime (wfview icomudpbase).
	fr.injectPingRequest(0x37, 0x11223344)
	waitFor(t, time.Second, "ping reply", func() bool {
		for _, p := range fr.recvOn(sockCtrl) {
			if len(p.data) == pingLen && p.data[typeOff] == ptPing && p.data[replyOff] == 0x01 {
				h := parseHeader(p.data)
				uptime := binary.LittleEndian.Uint32(p.data[pingTimeOff:])
				return h.seq == 0x37 && uptime == 0x11223344
			}
		}
		return false
	})
}

func TestIdleKeepaliveFlows(t *testing.T) {
	fr := newFakeRadio(t)
	dialLive(t, fr, nil) // IdlePeriod shrunk to 25ms

	// The client keeps the session warm with tracked idle packets on both
	// streams even with no work to send (the radio drops us without them).
	idles := func(sock string) int {
		n := 0
		for _, p := range fr.recvOn(sock) {
			if len(p.data) == ctrlLen && p.data[typeOff] == ptIdle && parseHeader(p.data).seq != 0 {
				n++
			}
		}
		return n
	}
	waitFor(t, 2*time.Second, "idles on the control stream", func() bool { return idles(sockCtrl) >= 3 })
	waitFor(t, 2*time.Second, "idles on the civ stream", func() bool { return idles(sockCiv) >= 3 })
}

func TestPingsRepeatAtPeriod(t *testing.T) {
	fr := newFakeRadio(t)
	dialLive(t, fr, nil) // PingPeriod shrunk to 60ms

	waitFor(t, 2*time.Second, "several client pings on the control stream", func() bool {
		n := 0
		for _, p := range fr.recvOn(sockCtrl) {
			if len(p.data) == pingLen && p.data[typeOff] == ptPing && p.data[replyOff] == 0x00 {
				n++
			}
		}
		return n >= 3
	})
}
