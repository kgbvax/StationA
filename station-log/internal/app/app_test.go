package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"

	"station-log/internal/config"
	"station-log/internal/importer"
	"station-log/internal/log4om"
	"station-log/internal/store"
)

type msg struct {
	topic    string
	qos      byte
	retained bool
	payload  []byte
}

type fakePub struct {
	mu   sync.Mutex
	msgs []msg
	fail bool
	got  chan struct{}
}

func newFake() *fakePub { return &fakePub{got: make(chan struct{}, 1024)} }

func (f *fakePub) Publish(topic string, qos byte, retained bool, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return io.ErrClosedPipe
	}
	f.msgs = append(f.msgs, msg{topic, qos, retained, append([]byte(nil), payload...)})
	f.got <- struct{}{}
	return nil
}

func (f *fakePub) wait(t *testing.T, n int) []msg {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-f.got:
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d of %d messages", i, n)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]msg(nil), f.msgs...)
}

func setup(t *testing.T) (*Service, *fakePub, context.Context) {
	t.Helper()
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	cfg.DefaultOwnCall = "DL9ET"
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f := newFake()
	s.SetPublisher(f)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sharedmqtt.RunJobs(ctx, s.jobs)
	return s, f, ctx
}

const adi = `<CALL:5>K1ABC<QSO_DATE:8>20261101<TIME_ON:6>101500<BAND:3>20m<MODE:3>SSB<EOR>`
const adiLoTW = `<CALL:5>K1ABC<QSO_DATE:8>20261101<TIME_ON:6>101700<BAND:3>20m<MODE:3>SSB<LOTW_QSL_RCVD:1>Y<STATE:2>MA<EOR>`

func post(s *Service, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/import?source=t", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestEventsOnImport(t *testing.T) {
	s, f, _ := setup(t)
	if w := post(s, adi); w.Code != 200 {
		t.Fatal(w.Body)
	}
	msgs := f.wait(t, 1)
	m := msgs[0]
	if m.topic != "muehle/hf/station-log/event" || m.qos != 1 || m.retained {
		t.Fatalf("%+v", m)
	}
	var ev store.Event
	if err := json.Unmarshal(m.payload, &ev); err != nil || ev.Action != "added" || ev.ID != "1" || ev.QSO.ID != "1" || ev.QSO.Call != "K1ABC" || ev.QSO.OwnCall != "DL9ET" {
		t.Fatalf("%v %s", err, m.payload)
	}
	// the later LoTW arrival merges and emits "merged"; an unchanged re-import emits nothing
	post(s, adiLoTW)
	msgs = f.wait(t, 1)
	if err := json.Unmarshal(msgs[1].payload, &ev); err != nil || ev.Action != "merged" || ev.ID != "1" || !ev.QSO.Confirmed || ev.QSO.State != "MA" {
		t.Fatalf("%v %s", err, msgs[1].payload)
	}
	post(s, adiLoTW)
	time.Sleep(50 * time.Millisecond)
	f.mu.Lock()
	n := len(f.msgs)
	f.mu.Unlock()
	if n != 2 {
		t.Fatalf("%d messages", n)
	}
	// replace_source emits deleted
	r := httptest.NewRequest("POST", "/import?replace_source=t", strings.NewReader(`<CALL:5>W1AW <QSO_DATE:8>20261101<TIME_ON:6>121700<BAND:3>40m<MODE:2>CW<EOR>`))
	s.Handler().ServeHTTP(httptest.NewRecorder(), r)
	msgs = f.wait(t, 2)
	if err := json.Unmarshal(msgs[3].payload, &ev); err != nil || ev.Action != "deleted" || ev.QSO.Call != "K1ABC" {
		t.Fatalf("%v %s", err, msgs[3].payload)
	}
}

func TestStatePublishedRetained(t *testing.T) {
	s, f, ctx := setup(t)
	go s.statePublisher(ctx)
	s.forcePub.Store(true)
	s.poke()
	msgs := f.wait(t, 1)
	var st State
	if msgs[0].topic != "muehle/hf/station-log/state" || !msgs[0].retained || json.Unmarshal(msgs[0].payload, &st) != nil ||
		st.QSOCount != 0 || st.OwnCalls == nil || len(st.Problems) != 1 || st.LastImport != nil {
		t.Fatalf("%+v %s", msgs[0], msgs[0].payload)
	}
	post(s, adi)
	msgs = f.wait(t, 2) // the event, then the state
	var got State
	for _, m := range msgs[1:] {
		if m.topic == "muehle/hf/station-log/state" {
			if err := json.Unmarshal(m.payload, &got); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got.QSOCount != 1 || len(got.OwnCalls) != 1 || got.OwnCalls[0] != "DL9ET" || got.LastImport == nil || got.LastImport.Source != "t" ||
		got.LastImport.Added != 1 || len(got.Problems) != 0 || got.TS == "" {
		t.Fatalf("%+v", got)
	}
}

func TestDroppedEventsShowInProblems(t *testing.T) {
	s, f, _ := setup(t)
	f.fail = true
	post(s, adi)
	deadline := time.Now().Add(2 * time.Second)
	for s.dropped.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	st, err := s.BuildState()
	if err != nil || len(st.Problems) != 1 || !strings.Contains(st.Problems[0], "not published") {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestMetaPayload(t *testing.T) {
	s, _, _ := setup(t)
	b, err := s.metaPayload()
	var m struct {
		Role         string
		Capabilities map[string]any
	}
	if err != nil || json.Unmarshal(b, &m) != nil || m.Role != "log-store" || m.Capabilities["event"] != "muehle/hf/station-log/event" {
		t.Fatalf("%v %s", err, b)
	}
}

func TestLog4OMFeederMergesThroughStore(t *testing.T) {
	s, f, ctx := setup(t)
	ready := make(chan net.Addr, 1)
	l := &log4om.Listener{Addr: "127.0.0.1:0", AllowFrom: config.Prefixes([]string{"127.0.0.1"}), Log: s.log, Ready: ready,
		Handle: func(ctx context.Context, text string) {
			if _, err := s.imp.Import(ctx, text, importer.Options{Source: log4om.Source}); err != nil {
				t.Error(err)
			}
		}}
	go l.Run(ctx)
	addr := (<-ready).String()
	c, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte(adi))
	f.wait(t, 1)
	c.Write([]byte(adiLoTW))
	msgs := f.wait(t, 1)
	var ev store.Event
	if json.Unmarshal(msgs[1].payload, &ev) != nil || ev.Action != "merged" || ev.QSO.Source != "log4om" {
		t.Fatalf("%s", msgs[1].payload)
	}
	if n, _ := s.st.Count(); n != 1 {
		t.Fatal(n)
	}
}
