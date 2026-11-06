package tgxl

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type nopLog struct{}

func (nopLog) Infof(string, ...any)  {}
func (nopLog) Warnf(string, ...any)  {}
func (nopLog) Debugf(string, ...any) {}

// fakeTGXL is a minimal TGXL: banner, info, status, and generic R replies.
type fakeTGXL struct {
	ln net.Listener

	mu        sync.Mutex
	cmds      []string
	operate   int
	bypass    int
	tuning    int
	ptt       int
	fail      map[string]int // command name -> non-zero code
	silent    bool           // stop answering (dead link)
	sendExtra string         // pushed once to the client (e.g. "M|text")
}

func newFake(t *testing.T) *fakeTGXL {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeTGXL{ln: ln, operate: 1, fail: map[string]int{}}
	t.Cleanup(func() { ln.Close() })
	go f.serve()
	return f
}

func (f *fakeTGXL) addr() string { return f.ln.Addr().String() }

func (f *fakeTGXL) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(c)
	}
}

func (f *fakeTGXL) handle(c net.Conn) {
	defer c.Close()
	fmt.Fprint(c, "V1.1.8\n")
	f.mu.Lock()
	extra := f.sendExtra
	f.mu.Unlock()
	if extra != "" {
		fmt.Fprint(c, extra+"\n")
	}
	rd := bufio.NewReader(c)
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		head, body, _ := strings.Cut(line, "|")
		seq := strings.TrimPrefix(head, "C")
		name, _, _ := strings.Cut(body, " ")
		f.mu.Lock()
		if f.silent {
			f.mu.Unlock()
			continue
		}
		if name != "status" {
			f.cmds = append(f.cmds, body)
		}
		code := f.fail[name]
		switch name {
		case "info":
			fmt.Fprintf(c, "R%s|0|info serial=210387-1 version=1.1.8 nickname=Tuner_Genius_XL 3way=1\n", seq)
		case "status":
			fmt.Fprintf(c, "S%s|status fwd=40.0 peak=40.0 max=0.00 swr=-20.0 pttA=%d bandA=0 modeA=0 flexA= freqA=0.000 "+
				"bypassA=0 bypassRxA=0 antA=2 pttB=0 bandB=0 modeB=0 flexB= freqB=0.000 bypassB=0 bypassRxB=0 antB=0 "+
				"state=%d active=1 tuning=%d bypass=%d ag=0 relayC1=1 relayL=2 relayC2=3\n", seq, f.ptt, f.operate, f.tuning, f.bypass)
		default:
			if code == 0 {
				switch body {
				case "operate set=1":
					f.operate = 1
				case "operate set=0":
					f.operate = 0
				case "bypass set=1":
					f.bypass = 1
				case "bypass set=0":
					f.bypass = 0
				}
			}
			fmt.Fprintf(c, "R%s|%d|\n", seq, code)
		}
		f.mu.Unlock()
	}
}

func (f *fakeTGXL) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

func (f *fakeTGXL) set(fn func()) { f.mu.Lock(); fn(); f.mu.Unlock() }

type rec struct {
	mu sync.Mutex
	st State
	n  int
}

func (r *rec) push(s State) { r.mu.Lock(); r.st = s; r.n++; r.mu.Unlock() }
func (r *rec) get() State   { r.mu.Lock(); defer r.mu.Unlock(); return r.st }

func start(t *testing.T, f *fakeTGXL) (*Device, *rec, <-chan error) {
	d := New(f.addr(), "", 20*time.Millisecond, 20*time.Millisecond, false, nopLog{})
	r := &rec{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, r.push) }()
	return d, r, done
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestRunPublishesStatus(t *testing.T) {
	f := newFake(t)
	_, r, _ := start(t, f)
	waitFor(t, "status", func() bool { s := r.get(); return s.DeviceOnline && s.Antenna == 2 })
	s := r.get()
	if !s.Operate || !s.Inline || s.Fwd != 10 || s.SWR < 1.2 || s.SWR > 1.23 || s.RelayL != 2 || s.Settling {
		t.Fatalf("unexpected %+v", s)
	}
}

func TestInfoHandler(t *testing.T) {
	f := newFake(t)
	d := New(f.addr(), "", 20*time.Millisecond, 20*time.Millisecond, false, nopLog{})
	got := make(chan Info, 1)
	d.SetInfoHandler(func(i Info) { got <- i })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx, func(State) {})
	select {
	case i := <-got:
		if i.Serial != "210387-1" || i.Version != "1.1.8" || !i.ThreeWay {
			t.Fatalf("%+v", i)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no info")
	}
}

func TestSetInlineFromStandby(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.operate = 0 })
	d, r, _ := start(t, f)
	waitFor(t, "standby seen", func() bool { s := r.get(); return s.DeviceOnline && !s.Operate })
	if err := d.SetInline(true); err != nil {
		t.Fatal(err)
	}
	cmds := f.commands()
	want := []string{"info", "operate set=1", "bypass set=0"}
	if strings.Join(cmds, ",") != strings.Join(want, ",") {
		t.Fatalf("cmds %v want %v", cmds, want)
	}
	waitFor(t, "inline", func() bool { return r.get().Inline })
}

func TestSetInlineFalseBypassesWithoutStandby(t *testing.T) {
	f := newFake(t)
	d, r, _ := start(t, f)
	waitFor(t, "online", func() bool { return r.get().DeviceOnline })
	if err := d.SetInline(false); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.commands() {
		if strings.HasPrefix(c, "operate") {
			t.Fatalf("bypass must not touch operate: %v", f.commands())
		}
	}
	waitFor(t, "bypassed", func() bool { s := r.get(); return s.Operate && !s.Inline })
}

func TestTuneRaisesSettlingThenClears(t *testing.T) {
	f := newFake(t)
	d, r, _ := start(t, f)
	waitFor(t, "online", func() bool { return r.get().DeviceOnline })
	f.set(func() { f.tuning = 1 })
	if err := d.Tune(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "settling", func() bool { return r.get().Settling })
	// wait until a poll has actually observed tuning=1 (otherwise only the grace timer clears it)
	waitFor(t, "tuning observed", func() bool { d.mu.Lock(); defer d.mu.Unlock(); return !d.tuningSince.IsZero() })
	f.set(func() { f.tuning = 0 })
	waitFor(t, "settled", func() bool { s := r.get(); return !s.Settling && s.Fault == "" })
	if got := f.commands(); got[len(got)-1] != "autotune" {
		t.Fatalf("cmds %v", got)
	}
}

func TestTuneFastRecallNeverSeenClearsAfterGrace(t *testing.T) {
	d := New("x", "", time.Second, time.Second, false, nopLog{})
	var last State
	d.onTelemetry = func(s State) { last = s }
	t0 := time.Now()
	d.tuneAt = t0
	d.state.Settling = true
	st := RawStatus{Operate: true, Antenna: 1}
	d.handleStatus(st, t0.Add(time.Second))
	if !last.Settling {
		t.Fatal("settling must hold inside the grace window")
	}
	d.handleStatus(st, t0.Add(tuneStartGrace+time.Second))
	if last.Settling || last.Fault != "" {
		t.Fatalf("grace expiry: %+v", last)
	}
}

func TestTuneTimeoutFault(t *testing.T) {
	d := New("x", "", time.Second, time.Second, false, nopLog{})
	var last State
	d.onTelemetry = func(s State) { last = s }
	t0 := time.Now()
	st := RawStatus{Operate: true, Tuning: true}
	d.handleStatus(st, t0)
	if !last.Settling {
		t.Fatal("want settling")
	}
	d.handleStatus(st, t0.Add(tuneTimeout+time.Second))
	if last.Settling || last.Fault != "tune timeout" {
		t.Fatalf("%+v", last)
	}
}

func TestNonZeroReplyBecomesFault(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.fail["activate"] = 5 })
	d, r, _ := start(t, f)
	waitFor(t, "online", func() bool { return r.get().DeviceOnline })
	if err := d.SetAntenna(3); err == nil {
		t.Fatal("want error")
	}
	waitFor(t, "fault", func() bool { return r.get().Fault == "activate: code 5" })
	f.set(func() { f.fail["activate"] = 0 })
	if err := d.SetAntenna(3); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "fault cleared", func() bool { return r.get().Fault == "" })
	if err := d.SetAntenna(4); err == nil {
		t.Fatal("antenna 4 must be rejected")
	}
}

func TestMessageLine(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.sendExtra = "M|Manual Tuning" })
	_, r, _ := start(t, f)
	waitFor(t, "message", func() bool { return r.get().Message == "Manual Tuning" })
}

func TestSilentLinkEndsRun(t *testing.T) {
	f := newFake(t)
	d := New(f.addr(), "", 20*time.Millisecond, 20*time.Millisecond, false, nopLog{})
	d.readTimeout = 150 * time.Millisecond
	r := &rec{}
	done := make(chan error, 1)
	go func() { done <- d.Run(context.Background(), r.push) }()
	waitFor(t, "online", func() bool { return r.get().DeviceOnline })
	f.set(func() { f.silent = true })
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want read timeout error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not end on a silent link")
	}
}

func TestAuthRequiredWithoutCode(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			fmt.Fprint(c, "V1.1.8 AUTH\n")
			time.Sleep(time.Second)
			c.Close()
		}
	}()
	d := New(ln.Addr().String(), "", 20*time.Millisecond, 20*time.Millisecond, false, nopLog{})
	if err := d.Run(context.Background(), func(State) {}); err == nil || !strings.Contains(err.Error(), "auth") {
		t.Fatalf("err=%v", err)
	}
}

func TestPollIntervalFollowsPTT(t *testing.T) {
	d := New("x", "", 250*time.Millisecond, 50*time.Millisecond, false, nopLog{})
	if got := d.pollInterval(); got != 250*time.Millisecond {
		t.Fatalf("rx %v", got)
	}
	d.mu.Lock()
	d.state.PTT = true
	d.mu.Unlock()
	if got := d.pollInterval(); got != 50*time.Millisecond {
		t.Fatalf("tx %v", got)
	}
	if d2 := New("x", "", 100*time.Millisecond, 500*time.Millisecond, false, nopLog{}); d2.txPoll != 100*time.Millisecond {
		t.Fatalf("tx poll slower than rx must clamp to rx, got %v", d2.txPoll)
	}
}

func TestFastPollWhileKeyed(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.ptt = 1 })
	d := New(f.addr(), "", 400*time.Millisecond, 20*time.Millisecond, false, nopLog{})
	r := &rec{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx, r.push)
	// first status arrives after ~400ms (rx rate), sees ptt, then polling must speed up
	waitFor(t, "ptt seen", func() bool { return r.get().PTT })
	r.mu.Lock()
	n0 := r.n
	r.mu.Unlock()
	time.Sleep(300 * time.Millisecond)
	r.mu.Lock()
	n1 := r.n
	r.mu.Unlock()
	f.set(func() { f.ptt = 0 })
	if n1-n0 < 5 { // 300ms at 20ms would give ~15 updates only if values change; telemetry pushes every status
		t.Fatalf("expected fast polling while keyed, got %d telemetry pushes in 300ms", n1-n0)
	}
}

func TestStatePushOverlaysWithoutTouchingMeters(t *testing.T) {
	d := New("x", "", time.Second, time.Second, false, nopLog{})
	var last State
	d.onTelemetry = func(s State) { last = s }
	t0 := time.Now()
	d.handleStatus(RawStatus{FwdDBm: 50, SWRdB: -20, Operate: true, Antenna: 2, PTT: true}, t0)
	if last.Fwd != 100 {
		t.Fatalf("fwd %d", last.Fwd)
	}
	d.dispatch(Line{Kind: KindStatus, Msg: "state bypassA=0 antA=0 state=1 tuning=1 bypass=1 relayC1=0 relayL=4 relayC2=8"})
	if last.Fwd != 100 || !last.PTT || last.Inline || !last.Settling || last.Antenna != 0 || last.RelayL != 4 || last.RelayC2 != 8 {
		t.Fatalf("push overlay wrong: %+v", last)
	}
}
