package ui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"pelcobridge2/internal/control"
)

// stubEngine answers requests in a goroutine; it records every intent seen.
type stubEngine struct {
	reqCh chan control.Request
	armed bool

	got   chan control.Intent
	reply func(it control.Intent) control.Result
}

func newStubEngine() *stubEngine {
	se := &stubEngine{
		reqCh: make(chan control.Request, 16),
		got:   make(chan control.Intent, 64),
	}
	se.reply = func(it control.Intent) control.Result { return control.Result{} }
	go func() {
		for req := range se.reqCh {
			se.got <- req.Intent
			if req.Reply != nil {
				req.Reply <- se.reply(req.Intent)
			}
		}
	}()
	return se
}

func (se *stubEngine) intents(t *testing.T, n int) []control.Intent {
	t.Helper()
	var out []control.Intent
	for len(out) < n {
		select {
		case it := <-se.got:
			out = append(out, it)
		case <-time.After(time.Second):
			t.Fatalf("only %d/%d intents arrived; have %v", len(out), n, out)
		}
	}
	return out
}

func (se *stubEngine) none(t *testing.T) {
	t.Helper()
	select {
	case it := <-se.got:
		t.Fatalf("unexpected intent %T", it)
	case <-time.After(100 * time.Millisecond):
	}
}

func key(k tea.KeyType, runes ...rune) tea.KeyMsg {
	return tea.KeyMsg{Type: k, Runes: runes}
}

// runCmd executes a command, descending into Batch members (tea.Batch returns
// the inner commands instead of running them).
func runCmd(c tea.Cmd) {
	if c == nil {
		return
	}
	if batch, ok := c().(tea.BatchMsg); ok {
		for _, inner := range batch {
			if inner != nil {
				_ = inner()
			}
		}
		return
	}
}

func runeKey(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func newTestModel(se *stubEngine) model {
	return New(Options{
		ReqCh:    se.reqCh,
		EvCh:     make(chan control.Event, 16),
		PortName: "TESTPORT", Baud: 2400, Addr: 1,
		JogHold: 50 * time.Millisecond,
	})
}

// The e-stop cuts through every state, including an open prompt.
func TestEstopAlwaysStops(t *testing.T) {
	for _, openPrompt := range []bool{false, true} {
		se := newStubEngine()
		m := newTestModel(se)
		if openPrompt {
			m2, _ := m.handleKey(key(tea.KeyRunes, 'A'))
			m = m2.(model)
			if m.prompt != promptArm {
				t.Fatal("arm prompt did not open")
			}
		}
		m2, _ := m.handleKey(key(tea.KeyEscape))
		m = m2.(model)
		if m.prompt != promptNone {
			t.Error("estop did not cancel the prompt")
		}
		if got := se.intents(t, 1); len(got) != 1 {
			t.Fatalf("estop submitted %d intents, want 1", len(got))
		} else if _, ok := got[0].(control.StopIntent); !ok {
			t.Errorf("estop submitted %T, want StopIntent", got[0])
		}
	}
}

// While a prompt is open the keyboard belongs to it: no motion key may fire.
func TestPromptOwnsKeyboard(t *testing.T) {
	se := newStubEngine()
	m := newTestModel(se)
	m2, _ := m.handleKey(key(tea.KeyRunes, 'A'))
	m = m2.(model)

	for _, k := range []tea.KeyMsg{key(tea.KeyUp), key(tea.KeyDown), key(tea.KeyLeft), key(tea.KeyRight)} {
		m2, _ = m.handleKey(k)
		m = m2.(model)
	}
	se.none(t)
	if m.prompt != promptArm {
		t.Error("prompt closed by motion key")
	}
}

// Arm flow: enter confirms, esc cancels; a successful arm calls OnArm.
func TestArmFlow(t *testing.T) {
	se := newStubEngine()
	se.armed = false
	armed := make(chan float64, 1)
	m := newTestModel(se)
	m.opts.OnArm = func(deg float64) { armed <- deg }

	// Bad azimuth: refused without touching the engine.
	m2, _ := m.handleKey(key(tea.KeyRunes, 'A'))
	m = m2.(model)
	m.input.SetValue("abc")
	m2, cmd := m.handleKey(key(tea.KeyEnter))
	m = m2.(model)
	if cmd != nil {
		if msg, ok := cmd().(armMsg); !ok || msg.err == nil {
			t.Fatal("garbage azimuth accepted")
		}
	}
	se.none(t)

	// Cancel path. ESC is the global e-stop: it cancels the prompt AND sends
	// one all-stop.
	m2, _ = m.handleKey(key(tea.KeyEscape))
	m = m2.(model)
	if m.prompt != promptNone {
		t.Error("esc did not cancel arm prompt")
	}
	stopped := se.intents(t, 1)
	if _, ok := stopped[0].(control.StopIntent); !ok {
		t.Errorf("esc in prompt sent %T, want StopIntent (e-stop)", stopped[0])
	}

	// Happy path: engine grants the fresh readback, then the arm.
	se.reply = func(it control.Intent) control.Result {
		switch it.(type) {
		case control.QueryPanIntent:
			return control.Result{Deg: 130}
		case control.ArmIntent:
			return control.Result{}
		}
		return control.Result{}
	}
	m2, _ = m.handleKey(key(tea.KeyRunes, 'A'))
	m = m2.(model)
	m.input.SetValue("30")
	m2, cmd = m.handleKey(key(tea.KeyEnter))
	m = m2.(model)
	msg := cmd().(armMsg)
	if msg.err != nil {
		t.Fatalf("arm refused: %v", msg.err)
	}
	if got := <-armed; got != 30 {
		t.Errorf("OnArm got %v, want 30", got)
	}
	ints := se.intents(t, 2)
	if _, ok := ints[0].(control.QueryPanIntent); !ok {
		t.Errorf("first arm step = %T, want QueryPanIntent", ints[0])
	}
	arm, ok := ints[1].(control.ArmIntent)
	if !ok || arm.TrueAz != 30 {
		t.Errorf("second arm step = %#v, want ArmIntent{30}", ints[1])
	}
}

// Self-test needs both confirm stages: y, then the typed word.
func TestSelfTestTwoStage(t *testing.T) {
	se := newStubEngine()
	m := newTestModel(se)

	// Stage 0 answered with n: cancelled, nothing sent.
	m2, _ := m.handleKey(key(tea.KeyRunes, 's'))
	m = m2.(model)
	m2, _ = m.handleKey(key(tea.KeyRunes, 'n'))
	m = m2.(model)
	se.none(t)

	// Full two-stage confirm sends SelfTestIntent.
	m2, _ = m.handleKey(key(tea.KeyRunes, 's'))
	m = m2.(model)
	m2, _ = m.handleKey(key(tea.KeyRunes, 'y'))
	m = m2.(model)
	if m.selfStage != 1 {
		t.Fatalf("stage after y = %d, want 1", m.selfStage)
	}
	// Wrong word: cancelled, nothing sent.
	m.input.SetValue("nope")
	m2, _ = m.handleKey(key(tea.KeyEnter))
	m = m2.(model)
	se.none(t)

	// Correct word.
	m2, _ = m.handleKey(key(tea.KeyRunes, 's'))
	m = m2.(model)
	m2, _ = m.handleKey(key(tea.KeyRunes, 'y'))
	m = m2.(model)
	m.input.SetValue("RIPCABLES")
	m2, _ = m.handleKey(key(tea.KeyEnter))
	m = m2.(model)
	got := se.intents(t, 1)
	if _, ok := got[0].(control.SelfTestIntent); !ok {
		t.Errorf("sent %T, want SelfTestIntent", got[0])
	}
}

// a/e issue exactly one query each (the engine answers them in the stub).
func TestQueryKeys(t *testing.T) {
	se := newStubEngine()
	se.reply = func(it control.Intent) control.Result {
		if _, ok := it.(control.QueryPanIntent); ok {
			return control.Result{Deg: 123.45}
		}
		return control.Result{Deg: 30}
	}
	m := newTestModel(se)
	m2, cmd := m.handleKey(key(tea.KeyRunes, 'a'))
	m = m2.(model)
	msg := cmd().(queryMsg)
	if msg.err != nil || msg.deg != 123.45 {
		t.Errorf("az query = %+v", msg)
	}
	m2, cmd = m.handleKey(key(tea.KeyRunes, 'e'))
	m = m2.(model)
	msg = cmd().(queryMsg)
	if msg.err != nil || msg.deg != 30 {
		t.Errorf("el query = %+v", msg)
	}
	if got := se.intents(t, 2); len(got) != 2 {
		t.Fatalf("queries = %v", got)
	}
}

// Jog submits a jog intent; the hold tick with the CURRENT sequence stops.
func TestJogHold(t *testing.T) {
	se := newStubEngine()
	m := newTestModel(se)
	m2, cmd := m.handleKey(key(tea.KeyUp))
	m = m2.(model)
	go func() { runCmd(cmd) }() // execute the batch: submit + arm the hold tick
	got := se.intents(t, 1)
	if j, ok := got[0].(control.JogIntent); !ok || j.Dir != control.DirUp {
		t.Fatalf("jog = %v, want JogIntent{up}", got[0])
	}
	if cmd == nil {
		t.Fatal("jog did not arm the hold tick")
	}
	// A stale tick (wrong sequence) must NOT stop.
	m.Update(jogHoldMsg(999))
	se.none(t)
	// The current-sequence tick stops.
	m.Update(jogHoldMsg(m.holdSeq))
	got2 := se.intents(t, 1)
	if _, ok := got2[0].(control.StopIntent); !ok {
		t.Errorf("hold expiry sent %T, want StopIntent", got2[0])
	}
}

// Jog keys are refused while armed=false is not known… they are submitted and
// the engine refuses; the TUI itself must not block them (the engine gates).
func TestJogSubmitsIntent(t *testing.T) {
	se := newStubEngine()
	m := newTestModel(se)
	m2, cmd := m.handleKey(key(tea.KeyLeft))
	m = m2.(model)
	if cmd != nil {
		go func() { runCmd(cmd) }()
	}
	got := se.intents(t, 1)
	if j, ok := got[0].(control.JogIntent); !ok || j.Dir != control.DirLeft {
		t.Errorf("sent %v, want JogIntent{left}", got[0])
	}
}
