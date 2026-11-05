package working

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"station-log/internal/working/qrz"
)

// harness runs the queued closures inline (the jobs worker's serialization)
// and records the published documents.
type harness struct {
	f    *Feed
	mu   sync.Mutex
	docs []State
	now  time.Time
	fail bool
}

func newHarness(t *testing.T, r Resolver) *harness {
	t.Helper()
	h := &harness{now: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}
	h.f = &Feed{
		Resolver: r, StaleAfter: 10 * time.Minute,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Enqueue: func(fn func()) { fn() },
		Now:     func() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.now },
		Publish: func(b []byte) error {
			if h.fail {
				return errors.New("broker down")
			}
			var st State
			if err := json.Unmarshal(b, &st); err != nil {
				t.Fatalf("published document is not a State: %v", err)
			}
			h.docs = append(h.docs, st)
			return nil
		},
	}
	return h
}

func (h *harness) advance(d time.Duration) { h.mu.Lock(); h.now = h.now.Add(d); h.mu.Unlock() }

const dxlogXML = `<?xml version="1.0"?><lookupinfo><call>VK9XY</call><reason>SpaceOrTab</reason><logger>DXLog</logger>` +
	`<freq>1402476</freq><mode>CW</mode><azimuth>62.4</azimuth><distance>15420.3</distance></lookupinfo>`

func TestEveryDatagramRepublishesWithFreshTS(t *testing.T) {
	h := newHarness(t, stationResolver())
	ctx := context.Background()
	h.f.Handle(ctx, "dxlog", KindN1MM, []byte(dxlogXML))
	h.advance(time.Second)
	h.f.Handle(ctx, "dxlog", KindN1MM, []byte(dxlogXML)) // identical re-key
	if len(h.docs) != 2 {
		t.Fatalf("%d documents, want 2 (no dedup on the selection path)", len(h.docs))
	}
	if h.docs[0].Selected == nil || h.docs[0].Selected.Call != "VK9XY" || !h.docs[0].DeviceOnline {
		t.Fatalf("first document: %+v", h.docs[0])
	}
	if h.docs[0].Selected.TS == h.docs[1].Selected.TS {
		t.Fatal("re-key did not carry a fresh selected.ts")
	}
}

func TestEmptyCallClearsContactInfoDoesNot(t *testing.T) {
	h := newHarness(t, stationResolver())
	ctx := context.Background()
	h.f.Handle(ctx, "dxlog", KindN1MM, []byte(dxlogXML))
	h.f.Handle(ctx, "dxlog", KindN1MM, []byte(`<contactinfo><call>VK9XY</call></contactinfo>`))
	if len(h.docs) != 1 {
		t.Fatalf("contactinfo published a document (%d docs)", len(h.docs))
	}
	h.f.Handle(ctx, "dxlog", KindN1MM, []byte(`<lookupinfo><call></call><reason>CallChanged</reason></lookupinfo>`))
	if len(h.docs) != 2 || h.docs[1].Selected != nil {
		t.Fatalf("empty lookupinfo call must clear: %+v", h.docs)
	}
	b, _ := json.Marshal(h.docs[1])
	if string(b) == "" || json.Valid(b) == false {
		t.Fatal("bad document")
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if _, has := m["selected"]; has {
		t.Fatalf("cleared document still has selected: %s", b)
	}
}

func TestLog4OMBareCall(t *testing.T) {
	h := newHarness(t, stationResolver())
	h.f.Handle(context.Background(), "log4om", KindLog4OM, []byte("OK2XYZ\r\n"))
	if len(h.docs) != 1 || h.docs[0].Selected.Call != "OK2XYZ" || h.docs[0].Selected.Source != "log4om" {
		t.Fatalf("%+v", h.docs)
	}
	h.f.Handle(context.Background(), "log4om", KindLog4OM, []byte("!!"))
	if len(h.docs) != 1 {
		t.Fatal("an undecodable datagram published")
	}
}

func TestQRZFailureNeverBlocksPublication(t *testing.T) {
	r := stationResolver()
	r.QRZ = &fakeLookup{err: qrz.ErrAuth}
	h := newHarness(t, r)
	h.f.Handle(context.Background(), "log4om", KindLog4OM, []byte("VK9XY"))
	if len(h.docs) != 1 || h.docs[0].Selected.Call != "VK9XY" || h.docs[0].Selected.Lat != 0 {
		t.Fatalf("call+RF-only record expected: %+v", h.docs)
	}
	if !h.f.QRZAuthFailed() {
		t.Fatal("auth failure not remembered")
	}
	r2 := &fakeLookup{rec: qrz.Record{Call: "VK9XY", Country: "Australia"}}
	h.f.Resolver.QRZ = r2
	h.f.Handle(context.Background(), "log4om", KindLog4OM, []byte("VK9XY"))
	if h.f.QRZAuthFailed() {
		t.Fatal("auth failure not cleared by a good lookup")
	}
}

func TestZeroZeroIsNoCoordinates(t *testing.T) {
	r := stationResolver()
	sel := r.FromN1MM(n1mmDXLogNoPos(), "dxlog")
	if sel.Lat != 0 || sel.Lng != 0 || sel.Azimuth != 0 {
		t.Fatalf("a position was invented: %+v", sel)
	}
	b, _ := json.Marshal(sel)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"lat", "lng", "azimuth", "distance_km", "locator"} {
		if _, has := m[k]; has {
			t.Errorf("%s present in %s", k, b)
		}
	}
}

func TestLivenessAndBirth(t *testing.T) {
	h := newHarness(t, stationResolver())
	ctx := context.Background()

	// Birth with nothing seen: device_online false, replaces a stale retained document.
	h.f.Birth()
	if len(h.docs) != 1 || h.docs[0].DeviceOnline || h.docs[0].Selected != nil {
		t.Fatalf("birth document: %+v", h.docs)
	}
	if h.f.Live() {
		t.Fatal("live before the first datagram")
	}

	h.f.Handle(ctx, "dxlog", KindN1MM, []byte(dxlogXML))
	if !h.f.Live() || !h.docs[1].DeviceOnline {
		t.Fatal("not live after a datagram")
	}

	// Staleness: the watcher flips device_online and keeps the selection.
	h.advance(11 * time.Minute)
	if h.f.Live() {
		t.Fatal("still live after stale_after")
	}
	tctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { h.f.Run(tctx, time.Millisecond); close(done) }()
	deadline := time.After(2 * time.Second)
	for {
		h.mu.Lock()
		n := len(h.docs)
		h.mu.Unlock()
		if n >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no liveness flip published")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
	last := h.docs[len(h.docs)-1]
	if last.DeviceOnline || last.Selected == nil || last.Selected.Call != "VK9XY" {
		t.Fatalf("flip document: %+v", last)
	}

	// A reconnect re-announces the last document verbatim.
	n := len(h.docs)
	h.f.Birth()
	if len(h.docs) != n+1 || h.docs[n].Selected == nil || h.docs[n].TS != last.TS {
		t.Fatalf("birth replay: %+v", h.docs[n:])
	}
}

func TestPublishFailureIsNotFatal(t *testing.T) {
	h := newHarness(t, stationResolver())
	h.fail = true
	h.f.Handle(context.Background(), "dxlog", KindN1MM, []byte(dxlogXML)) // must not panic
	h.fail = false
	h.f.Birth()
	if len(h.docs) != 1 || h.docs[0].Selected == nil {
		t.Fatalf("the document kept for the next announce is lost: %+v", h.docs)
	}
}

func TestListenerAllowFromAndHandle(t *testing.T) {
	got := make(chan string, 4)
	ready := make(chan net.Addr, 1)
	l := &Listener{Name: "log4om", Kind: KindLog4OM, Addr: "127.0.0.1:0",
		AllowFrom: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)), Ready: ready,
		Handle: func(_ context.Context, l *Listener, data []byte) { got <- l.Name + ":" + string(data) }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)
	addr := <-ready
	c, err := net.Dial("udp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("DL1ABC"))
	select {
	case s := <-got:
		if s != "log4om:DL1ABC" {
			t.Fatal(s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no datagram")
	}

	// a sender outside allow_from is dropped
	l2 := &Listener{Name: "x", Kind: KindLog4OM, Addr: "127.0.0.1:0",
		AllowFrom: []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)), Ready: ready,
		Handle: func(_ context.Context, l *Listener, data []byte) { got <- "bad" }}
	go l2.Run(ctx)
	c2, _ := net.Dial("udp", (<-ready).String())
	c2.Write([]byte("DL1ABC"))
	select {
	case s := <-got:
		t.Fatalf("datagram outside allow_from reached the handler: %s", s)
	case <-time.After(200 * time.Millisecond):
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func TestListenerLogsRejectedSourceOnce(t *testing.T) {
	buf := &lockedBuf{}
	ready := make(chan net.Addr, 1)
	l := &Listener{Name: "dxlog", Kind: KindN1MM, Addr: "127.0.0.1:0",
		AllowFrom: []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")},
		Log:       slog.New(slog.NewTextHandler(buf, nil)), Ready: ready,
		Handle: func(context.Context, *Listener, []byte) {}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)
	c, _ := net.Dial("udp", (<-ready).String())
	for i := 0; i < 5; i++ {
		c.Write([]byte("x"))
	}
	deadline := time.Now().Add(2 * time.Second)
	for l.Rejected() < 5 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if l.Rejected() != 5 {
		t.Fatalf("rejected = %d", l.Rejected())
	}
	if n := strings.Count(buf.String(), "outside allow_from"); n != 1 {
		t.Fatalf("logged %d times, want once per source:\n%s", n, buf.String())
	}
}

func TestListenerRetriesBindUntilPortFrees(t *testing.T) {
	hold, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	buf := &lockedBuf{}
	ready := make(chan net.Addr, 1)
	fails := make(chan error, 100)
	up := make(chan struct{}, 1)
	l := &Listener{Name: "x", Kind: KindLog4OM, Addr: hold.LocalAddr().String(),
		AllowFrom: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
		Log:       slog.New(slog.NewTextHandler(buf, nil)), Ready: ready,
		MinBackoff: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond,
		OnFail: func(err error) { fails <- err }, OnUp: func() { up <- struct{}{} },
		Handle: func(context.Context, *Listener, []byte) {}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	for i := 0; i < 3; i++ {
		select {
		case <-fails:
		case <-time.After(2 * time.Second):
			t.Fatal("no retry")
		}
	}
	hold.Close()
	select {
	case <-up:
	case <-time.After(2 * time.Second):
		t.Fatal("never bound after the port was released")
	}
	<-ready
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Fatalf("%d warnings for one outage:\n%s", n, buf.String())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not stop on ctx cancel")
	}
}
