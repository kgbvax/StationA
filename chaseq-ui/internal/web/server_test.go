package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"chaseq-ui/internal/action"
	"chaseq-ui/internal/bus"
	"chaseq-ui/internal/loggers"
	"chaseq-ui/internal/model"
)

type pub struct {
	mu   sync.Mutex
	msgs []string
}

func (p *pub) Publish(topic string, payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = append(p.msgs, topic+" "+string(payload))
	return nil
}
func (p *pub) n() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.msgs) }

type lg struct{ calls []string }

func (l *lg) Name() string { return "Log4OM" }
func (l *lg) SendCall(_ context.Context, call string) loggers.Result {
	l.calls = append(l.calls, call)
	return loggers.Result{Status: loggers.OK}
}

func newServer(t *testing.T) (*Server, *model.Model, *pub, *lg) {
	t.Helper()
	m := model.New(nil)
	m.SetChase(bus.ChaseState{Activity: "dx", Problems: []string{"Club Log confirmations not loaded yet"}, Ranked: []bus.Ranked{
		{RowID: 7, Call: "EA8XYZ", Band: "20m", Needed: true, Tag: "need"},
		{RowID: 8, Call: "K1A", Band: "40m"},
		{RowID: 9, Band: "20m", Needed: true},
	}})
	m.SetCQFinder(bus.CQFinderState{Rows: []bus.Candidate{{RowID: 7, FreqHz: 14231500}, {RowID: 8, FreqHz: 7074000}}})
	p := &pub{}
	l := &lg{}
	rad := &action.Radio{}
	rad.Set(bus.RadioState{FreqHz: 14231500})
	r := &action.Runner{Model: m, Radio: rad, Pub: p, ChaseCmd: "c", RadioCmd: "r", Loggers: []loggers.Sender{l},
		Timing: action.Timing{TuneWait: 50 * time.Millisecond, ConfirmWait: 50 * time.Millisecond, Poll: 5 * time.Millisecond}}
	s, err := New(m, r, filepath.Join(t.TempDir(), "filters.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, m, p, l
}

func do(t *testing.T, h http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Host = "127.0.0.1:8480"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

var csrf = map[string]string{"X-Chaseq": "1"}

func TestListRendersRowsTagsAndProblems(t *testing.T) {
	s, _, _, _ := newServer(t)
	body := do(t, s.Handler(), "GET", "/list", nil).Body.String()
	for _, want := range []string{"EA8XYZ", "14231.5", `class="badge need"`, "no call", "no freq", "Club Log confirmations not loaded yet", `class="row hi`} {
		if !strings.Contains(body, want) {
			t.Errorf("list missing %q", want)
		}
	}
	if strings.Index(body, "EA8XYZ") > strings.Index(body, "K1A") {
		t.Error("rows are not in chase-queue order")
	}
}

func TestActRunsTheHighlightedRow(t *testing.T) {
	s, _, p, l := newServer(t)
	rec := do(t, s.Handler(), "POST", "/act", csrf)
	if rec.Code != 200 {
		t.Fatalf("code = %d %s", rec.Code, rec.Body)
	}
	var rep action.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil || rep.Call != "EA8XYZ" {
		t.Fatalf("report = %+v (%v)", rep, err)
	}
	if len(l.calls) != 1 || l.calls[0] != "EA8XYZ" || p.n() == 0 {
		t.Errorf("calls=%v published=%d", l.calls, p.n())
	}
	if body := do(t, s.Handler(), "GET", "/list", nil).Body.String(); !strings.Contains(body, "Radio: ok") {
		t.Error("the result strip does not show the last action")
	}
}

func TestMoveDoesNotTouchRadioOrLoggers(t *testing.T) {
	s, m, p, l := newServer(t)
	for i := 0; i < 2; i++ {
		if rec := do(t, s.Handler(), "POST", "/move?d=1", csrf); rec.Code != 204 {
			t.Fatalf("code = %d", rec.Code)
		}
	}
	if v := m.Snapshot(); v.Highlight != 9 {
		t.Errorf("highlight = %d, want 9", v.Highlight)
	}
	if p.n() != 0 || len(l.calls) != 0 {
		t.Errorf("published=%d calls=%v", p.n(), l.calls)
	}
}

func TestWriteGuards(t *testing.T) {
	s, _, p, _ := newServer(t)
	if rec := do(t, s.Handler(), "POST", "/act", nil); rec.Code != 403 {
		t.Errorf("missing header: code = %d", rec.Code)
	}
	if rec := do(t, s.Handler(), "POST", "/act", map[string]string{"X-Chaseq": "1", "Origin": "http://evil.example"}); rec.Code != 403 {
		t.Errorf("foreign origin: code = %d", rec.Code)
	}
	if p.n() != 0 {
		t.Errorf("published %d messages for refused requests", p.n())
	}
}

func TestOfflineRefusesActions(t *testing.T) { // AE6
	s, m, p, _ := newServer(t)
	m.SetStatus(false)
	body := do(t, s.Handler(), "GET", "/list", nil).Body.String()
	if !strings.Contains(body, "chase-queue is offline") {
		t.Error("offline line missing")
	}
	for _, path := range []string{"/act", "/skip", "/undo"} {
		if rec := do(t, s.Handler(), "POST", path, csrf); rec.Code != 409 {
			t.Errorf("%s: code = %d, want 409", path, rec.Code)
		}
	}
	if p.n() != 0 {
		t.Errorf("published %d", p.n())
	}
}

func TestFiltersPersistAndShowHidden(t *testing.T) {
	s, _, _, _ := newServer(t)
	do(t, s.Handler(), "POST", "/filters?band=20m", csrf)
	do(t, s.Handler(), "POST", "/filters?needed=1", csrf)
	body := do(t, s.Handler(), "GET", "/list", nil).Body.String()
	if strings.Contains(body, "K1A") || !strings.Contains(body, "1 hidden by filter") {
		t.Errorf("filtered list wrong:\n%s", body)
	}
	if f := model.LoadFilters(s.FiltersPath); !f.NeededOnly || len(f.Bands) != 1 {
		t.Errorf("persisted = %+v", f)
	}
	if rec := do(t, s.Handler(), "POST", "/filters?band=99m", csrf); rec.Code != 400 {
		t.Errorf("bad band: code = %d", rec.Code)
	}
}

func TestEventsSendsListChanged(t *testing.T) {
	s, _, _, _ := newServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)
	read := func() string { l, _ := rd.ReadString('\n'); return l }
	if got := read(); got != "event: hello\n" {
		t.Fatalf("first line = %q", got)
	}
	read()
	read() // data + blank
	time.Sleep(50 * time.Millisecond)
	s.Notify()
	if got := read(); got != "event: list_changed\n" {
		t.Errorf("after Notify = %q", got)
	}
}

func TestHostGuardRefusesRebindingAndForeignOrigin(t *testing.T) {
	s, _, p, _ := newServer(t)
	for _, host := range []string{"evil.example:8480", "evil.example", "10.0.0.5:8480"} {
		req := httptest.NewRequest("POST", "/act", nil)
		req.Host = host
		req.Header.Set("X-Chaseq", "1")
		req.Header.Set("Origin", "http://"+host)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Errorf("Host %q: code = %d, want 403", host, rec.Code)
		}
		get := httptest.NewRequest("GET", "/list", nil)
		get.Host = host
		rec = httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, get)
		if rec.Code != 403 {
			t.Errorf("GET Host %q: code = %d, want 403", host, rec.Code)
		}
	}
	if p.n() != 0 {
		t.Errorf("published %d messages for refused requests", p.n())
	}
	// A loopback name still works (httptest uses example.com by default, so set it).
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.Host = "127.0.0.1:8480"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("loopback healthz code = %d", rec.Code)
	}
}

func TestRapidFilterTogglesAreNotLost(t *testing.T) {
	s, m, _, _ := newServer(t)
	done := make(chan struct{})
	for _, b := range []string{"20m", "40m", "80m", "15m"} {
		b := b
		go func() { do(t, s.Handler(), "POST", "/filters?band="+b, csrf); done <- struct{}{} }()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	if got := m.Filters().Bands; len(got) != 4 {
		t.Errorf("bands = %v, want all four toggles kept", got)
	}
}
