package ui

import (
	"strings"
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
// the inner commands instead of running them), and returns the produced msgs.
func runCmd(c tea.Cmd) []tea.Msg {
	if c == nil {
		return nil
	}
	msg := c()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var msgs []tea.Msg
		for _, inner := range batch {
			if inner != nil {
				if m := inner(); m != nil {
					msgs = append(msgs, m)
				}
			}
		}
		return msgs
	}
	if msg != nil {
		return []tea.Msg{msg}
	}
	return nil
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
		m2, cmd := m.handleKey(key(tea.KeyEscape))
		m = m2.(model)
		if m.prompt != promptNone {
			t.Error("estop did not cancel the prompt")
		}
		runCmd(cmd) // the stop is delivered asynchronously via the Cmd now
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
	var cmd tea.Cmd
	for _, bad := range []string{"abc", "nan", "inf"} {
		m.input.SetValue(bad)
		m2, cmd = m.handleKey(key(tea.KeyEnter))
		m = m2.(model)
		if cmd != nil {
			if msg, ok := cmd().(armMsg); !ok || msg.err == nil {
				t.Fatalf("azimuth %q accepted", bad)
			}
		}
		// Re-open the prompt for the next round.
		if m.prompt != promptArm {
			m2, _ = m.handleKey(key(tea.KeyRunes, 'A'))
			m = m2.(model)
		}
	}
	se.none(t)

	// Cancel path. ESC is the global e-stop: it cancels the prompt AND sends
	// one all-stop.
	m2, cmd = m.handleKey(key(tea.KeyEscape))
	m = m2.(model)
	runCmd(cmd)
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

// Goto prompt: enter submits one GotoAzElIntent shaped by the parsed answer;
// a parse error keeps the prompt open (entered text survives); esc cancels.
func TestGotoPrompt(t *testing.T) {
	se := newStubEngine()
	m := newTestModel(se)

	m2, _ := m.handleKey(key(tea.KeyRunes, 'g'))
	m = m2.(model)
	if m.prompt != promptGoto {
		t.Fatal("goto prompt did not open")
	}

	// Bad targets: refused locally, prompt stays open, nothing submitted.
	for _, bad := range []string{"", "abc", "200 45 10", "el 95", "nan 45", "200 -5", "el"} {
		m.input.SetValue(bad)
		m2, _ = m.handleKey(key(tea.KeyEnter))
		m = m2.(model)
		if m.prompt != promptGoto {
			t.Fatalf("target %q closed the prompt", bad)
		}
	}
	se.none(t)

	// Bare form: one number = az, two = az then el.
	m.input.SetValue("200 45")
	m2, _ = m.handleKey(key(tea.KeyEnter))
	m = m2.(model)
	if m.prompt != promptNone {
		t.Fatal("goto prompt still open after a valid target")
	}
	got := se.intents(t, 1)
	it, ok := got[0].(control.GotoAzElIntent)
	if !ok || !it.HasAz || !it.HasEl || it.Az != 200 || it.El != 45 {
		t.Fatalf("goto submitted %#v, want GotoAzElIntent{Az:200, El:45, both}", got[0])
	}

	// Keyword form, el only.
	m2, _ = m.handleKey(key(tea.KeyRunes, 'g'))
	m = m2.(model)
	m.input.SetValue("el 30")
	m2, _ = m.handleKey(key(tea.KeyEnter))
	m = m2.(model)
	got = se.intents(t, 1)
	it, ok = got[0].(control.GotoAzElIntent)
	if !ok || it.HasAz || !it.HasEl || it.El != 30 {
		t.Fatalf("goto submitted %#v, want GotoAzElIntent{El:30, el only}", got[0])
	}

	// A prompt owns the keyboard: motion keys must not fire mid-prompt.
	m2, _ = m.handleKey(key(tea.KeyRunes, 'g'))
	m = m2.(model)
	m2, _ = m.handleKey(key(tea.KeyUp))
	m = m2.(model)
	se.none(t)
	m2, cmd := m.handleKey(key(tea.KeyEscape))
	m = m2.(model)
	runCmd(cmd) // esc is also the e-stop
	if m.prompt != promptNone {
		t.Error("esc did not cancel goto prompt")
	}
	stopped := se.intents(t, 1)
	if _, ok := stopped[0].(control.StopIntent); !ok {
		t.Errorf("esc in goto prompt sent %T, want StopIntent (e-stop)", stopped[0])
	}
}

// The self-test is a y/n confirm: n cancels, y sends — and never opens while
// armed (disarmed-only is checked here too, mirroring the s-key pre-gate).
func TestSelfTestConfirm(t *testing.T) {
	se := newStubEngine()
	m := newTestModel(se)

	// s while armed: refused without opening the prompt.
	m.snap = &control.Snapshot{Armed: true}
	m2, _ := m.handleKey(key(tea.KeyRunes, 's'))
	m = m2.(model)
	if m.prompt != promptNone {
		t.Fatal("s opened the self-test prompt while armed")
	}
	se.none(t)

	// n cancels: nothing sent.
	m.snap = &control.Snapshot{}
	m2, _ = m.handleKey(key(tea.KeyRunes, 's'))
	m = m2.(model)
	if m.prompt != promptSelfTest {
		t.Fatal("s did not open the self-test prompt")
	}
	m2, _ = m.handleKey(key(tea.KeyRunes, 'n'))
	m = m2.(model)
	if m.prompt != promptNone {
		t.Fatal("n did not cancel the self-test prompt")
	}
	se.none(t)

	// y sends — the intent rides the blocking round-trip cmd now.
	m2, _ = m.handleKey(key(tea.KeyRunes, 's'))
	m = m2.(model)
	m2, cmd := m.handleKey(key(tea.KeyRunes, 'y'))
	m = m2.(model)
	runCmd(cmd)
	got := se.intents(t, 1)
	if _, ok := got[0].(control.SelfTestIntent); !ok {
		t.Errorf("sent %T, want SelfTestIntent", got[0])
	}
}

// c disables the self-check directly (safe direction, no confirm); C needs
// the y/n confirm and never opens while armed. Both go through a blocking
// round-trip, so a refusal lands in the status line — a refused intent must
// never read as "sent".
func TestSelfCheckKeys(t *testing.T) {
	se := newStubEngine()
	m := newTestModel(se)

	// c: straight through, disable; the success status is the cmd's verdict.
	// It sends even when the pane already claims "off" — that claim is the
	// engine's model, not proof, so a re-send is always in order.
	m.snap = &control.Snapshot{SelfCheck: "off"}
	m2, cmd := m.handleKey(key(tea.KeyRunes, 'c'))
	m = m2.(model)
	msgs := runCmd(cmd)
	got := se.intents(t, 1)
	if sc, ok := got[0].(control.SelfCheckIntent); !ok || sc.Enable {
		t.Fatalf("c sent %#v, want SelfCheckIntent{Enable:false}", got[0])
	}
	if len(msgs) != 1 {
		t.Fatalf("c produced %d msgs, want 1", len(msgs))
	}
	if sm := msgs[0].(selfMsg); sm.err != nil || sm.okStatus == "" {
		t.Errorf("c verdict = %+v, want success", sm)
	}

	// A refusal the engine answers (here: moving) surfaces as such in the
	// status line — never as a silent success.
	se.reply = func(it control.Intent) control.Result {
		if _, ok := it.(control.SelfCheckIntent); ok {
			return control.Result{Err: control.ErrMoving}
		}
		return control.Result{}
	}
	m2, cmd = m.handleKey(key(tea.KeyRunes, 'c'))
	m = m2.(model)
	msgs = runCmd(cmd)
	se.intents(t, 1)
	if len(msgs) != 1 {
		t.Fatalf("refused c produced %d msgs, want 1", len(msgs))
	}
	if sm := msgs[0].(selfMsg); sm.err != control.ErrMoving {
		t.Errorf("refused c verdict = %+v, want ErrMoving", sm)
	}
	m2, _ = m.Update(msgs[0])
	m = m2.(model)
	if !strings.Contains(m.status, "refused") {
		t.Errorf("status after refused disable = %q, want a refused: line", m.status)
	}
	se.reply = func(it control.Intent) control.Result { return control.Result{} }

	// C while armed: refused without opening the prompt.
	m.snap = &control.Snapshot{Armed: true, SelfCheck: "off"}
	m2, _ = m.handleKey(key(tea.KeyRunes, 'C'))
	m = m2.(model)
	if m.prompt != promptNone {
		t.Fatal("C opened the self-check prompt while armed")
	}
	se.none(t)

	// C disarmed: y/n confirm; n cancels, y enables.
	m.snap = &control.Snapshot{}
	m2, _ = m.handleKey(key(tea.KeyRunes, 'C'))
	m = m2.(model)
	if m.prompt != promptSelfCheck {
		t.Fatal("C did not open the self-check prompt")
	}
	m2, _ = m.handleKey(key(tea.KeyRunes, 'n'))
	m = m2.(model)
	if m.prompt != promptNone {
		t.Fatal("n did not cancel the self-check prompt")
	}
	se.none(t)

	m2, _ = m.handleKey(key(tea.KeyRunes, 'C'))
	m = m2.(model)
	m2, cmd = m.handleKey(key(tea.KeyRunes, 'y'))
	m = m2.(model)
	runCmd(cmd)
	got = se.intents(t, 1)
	if sc, ok := got[0].(control.SelfCheckIntent); !ok || !sc.Enable {
		t.Fatalf("C+y sent %#v, want SelfCheckIntent{Enable:true}", got[0])
	}

	// No "already on" short-circuit: "on" in the pane is the engine's
	// liveness-gated claim, not proof, so C always offers the confirm.
	m.snap = &control.Snapshot{SelfCheck: "on"}
	m2, _ = m.handleKey(key(tea.KeyRunes, 'C'))
	m = m2.(model)
	if m.prompt != promptSelfCheck {
		t.Fatal("C must always open the self-check prompt")
	}
	m2, cmd = m.handleKey(key(tea.KeyRunes, 'y'))
	m = m2.(model)
	runCmd(cmd)
	got = se.intents(t, 1)
	if sc, ok := got[0].(control.SelfCheckIntent); !ok || !sc.Enable {
		t.Fatalf("C+y with SelfCheck=on sent %#v, want SelfCheckIntent{Enable:true}", got[0])
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
// The tick's own sequence — not one reconstructed from the model — must match,
// or a lost sequence increment means release never stops the head (live bug).
func TestJogHold(t *testing.T) {
	se := newStubEngine()
	m := newTestModel(se)
	m2, cmd := m.handleKey(key(tea.KeyUp))
	m = m2.(model)
	done := make(chan []tea.Msg, 1)
	go func() { done <- runCmd(cmd) }() // execute the batch: submit + arm the hold tick
	got := se.intents(t, 1)
	if j, ok := got[0].(control.JogIntent); !ok || j.Dir != control.DirUp {
		t.Fatalf("jog = %v, want JogIntent{up}", got[0])
	}
	var msgs []tea.Msg
	select {
	case msgs = <-done:
	case <-time.After(time.Second):
		t.Fatal("hold tick never fired")
	}
	var seq int
	for _, msg := range msgs {
		if s, ok := msg.(jogHoldMsg); ok {
			seq = int(s)
		}
	}
	if seq == 0 {
		t.Fatalf("no hold tick armed; msgs=%v", msgs)
	}
	// The sequence the TICK carries must be the one the MODEL holds. With the
	// value-receiver bug these diverged (tick 1, model 0) and release never
	// stopped the head.
	if int(m.holdSeq) != seq {
		t.Fatalf("model holdSeq=%d, tick seq=%d — sequence lost", m.holdSeq, seq)
	}
	// A stale tick (wrong sequence) must NOT stop.
	_, cmd = m.Update(jogHoldMsg(seq + 999))
	runCmd(cmd)
	se.none(t)
	// The real tick stops.
	_, cmd = m.Update(jogHoldMsg(seq))
	runCmd(cmd)
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
