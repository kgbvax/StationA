package logview

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

const worked = `{"own":"DL9ET","count":3,"worked":[
 {"call":"EA8XYZ","band":"20m","mode_class":"cw","mode":"CW","at":"2025-05-01T10:00:00Z","confirmed":true,"dxcc":29},
 {"call":"EA8XYZ/P","band":"40m","mode_class":"data","mode":"FT8","at":"2025-06-01T10:00:00Z","confirmed":false,"dxcc":29},
 {"call":"K1ABC","band":"20m","mode_class":"phone","mode":"SSB","at":"2025-04-01T10:00:00Z","confirmed":false}]}`

type fake struct {
	body   atomic.Value // string
	status atomic.Int32
	hits   atomic.Int32
	query  atomic.Value
}

func newFake(t *testing.T) (*fake, *httptest.Server) {
	f := &fake{}
	f.body.Store(worked)
	f.status.Store(200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.query.Store(r.URL.RequestURI())
		if st := int(f.status.Load()); st != 200 {
			http.Error(w, "down", st)
			return
		}
		io.WriteString(w, f.body.Load().(string))
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func ev(action, id, own, call, band, class string, confirmed bool, updated string) []byte {
	b, _ := json.Marshal(map[string]any{"id": id, "action": action, "qso": map[string]any{
		"own_call": own, "call": call, "band": band, "mode": "CW", "mode_class": class,
		"at": "2025-07-01T10:00:00Z", "updated": updated, "confirmed": confirmed, "dxcc": 29}})
	return b
}

func TestLoadAndSummary(t *testing.T) {
	f, srv := newFake(t)
	v := New(Config{URL: srv.URL + "/", Own: "dl9et"}, quiet)
	if v.Loaded() || v.Summary("EA8XYZ") != nil {
		t.Fatal("loaded before Load")
	}
	if err := v.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if q := f.query.Load().(string); q != "/v1/worked?own=DL9ET" {
		t.Fatalf("query %q", q)
	}
	// the portable form and the plain call are one station
	w := v.Summary("ea8xyz")
	if w == nil || w.QSOs != 2 || strings.Join(w.Bands, ",") != "40m,20m" || strings.Join(w.Modes, ",") != "CW,FT8" ||
		!w.Last.Equal(time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("%+v", w)
	}
	if v.Summary("DL1ZZZ") != nil {
		t.Fatal("never worked")
	}
	wk := v.Worked()
	if len(wk) != 3 || len(v.Confirmed()) != 1 || v.Confirmed()[0].DXCC != 29 || v.Confirmed()[0].Band != "20m" {
		t.Fatalf("%v %v", wk, v.Confirmed())
	}
	if v.Problem() != "" {
		t.Fatal(v.Problem())
	}
}

func TestEventsIncremental(t *testing.T) {
	_, srv := newFake(t)
	v := New(Config{URL: srv.URL, Own: "DL9ET"}, quiet)
	v.Load(context.Background())
	// a new QSO adds a band; confirming an old one turns the flag on; no reload
	v.Apply(ev("added", "7", "DL9ET", "K1ABC", "15m", "cw", false, "2025-07-01T10:00:01Z"))
	v.Apply(ev("merged", "1", "DL9ET", "EA8XYZ", "40m", "data", true, "2025-07-01T10:00:02Z"))
	if w := v.Summary("K1ABC"); w == nil || w.QSOs != 2 || w.Bands[0] != "20m" || w.Bands[1] != "15m" {
		t.Fatalf("%+v", w)
	}
	if n := len(v.Confirmed()); n != 2 {
		t.Fatalf("confirmed %v", v.Confirmed())
	}
	// the same event again, and another own call, change nothing
	select {
	case <-v.Changes():
	default:
		t.Fatal("no change signalled")
	}
	v.Apply(ev("added", "7", "DL9ET", "K1ABC", "15m", "cw", false, "2025-07-01T10:00:01Z"))
	v.Apply(ev("added", "9", "DL0XX", "W1AW", "20m", "cw", false, "2025-07-01T10:00:03Z"))
	v.Apply([]byte(`not json`))
	select {
	case <-v.Changes():
		t.Fatal("duplicate or foreign event changed the view")
	default:
	}
	if v.Summary("W1AW") != nil {
		t.Fatal("foreign own call")
	}
}

func TestDeletedReloads(t *testing.T) {
	f, srv := newFake(t)
	v := New(Config{URL: srv.URL, Reload: time.Hour}, quiet)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { v.Run(ctx); close(done) }()
	waitFor(t, v.Loaded)
	f.body.Store(`{"worked":[{"call":"K1ABC","band":"20m","mode_class":"phone","mode":"SSB","at":"2025-04-01T10:00:00Z"}]}`)
	v.Apply(ev("deleted", "1", "DL9ET", "EA8XYZ", "20m", "cw", true, "2025-07-01T10:00:00Z"))
	waitFor(t, func() bool { return v.Summary("EA8XYZ") == nil })
	if v.Summary("K1ABC") == nil || len(v.Confirmed()) != 0 {
		t.Fatal("reload lost rows")
	}
	cancel()
	<-done
}

func TestEventDuringLoadIsKept(t *testing.T) {
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate
		io.WriteString(w, worked)
	}))
	defer srv.Close()
	v := New(Config{URL: srv.URL}, quiet)
	errc := make(chan error, 1)
	go func() { errc <- v.Load(context.Background()) }()
	waitFor(t, func() bool { v.mu.Lock(); defer v.mu.Unlock(); return v.loading })
	v.Apply(ev("added", "7", "DL9ET", "N0NEW", "10m", "cw", false, "2025-07-01T10:00:01Z"))
	close(gate)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if v.Summary("N0NEW") == nil {
		t.Fatal("event that raced the load was lost")
	}
}

func TestUnreachableKeepsLastView(t *testing.T) {
	f, srv := newFake(t)
	v := New(Config{URL: srv.URL}, quiet)
	if err := v.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.status.Store(503)
	if err := v.Load(context.Background()); err == nil {
		t.Fatal("503 must fail")
	}
	if !v.Loaded() || v.Summary("K1ABC") == nil {
		t.Fatal("last good view dropped")
	}
	if p := v.Problem(); !strings.Contains(p, "station-log not reachable") || !strings.Contains(p, "last loaded") {
		t.Fatal(p)
	}
	f.status.Store(200)
	v.Load(context.Background())
	if v.Problem() != "" {
		t.Fatal(v.Problem())
	}
	// a server that is not there at all: no URL in the problem text
	srv.Close()
	v.Load(context.Background())
	if p := v.Problem(); !strings.Contains(p, "not reachable") || strings.Contains(p, "http://") {
		t.Fatal(p)
	}
}

func TestNeverLoadedProblem(t *testing.T) {
	_, srv := newFake(t)
	srv.Close()
	v := New(Config{URL: srv.URL}, quiet)
	if v.Load(context.Background()) == nil || v.Loaded() || !strings.HasPrefix(v.Problem(), "station-log not reachable") {
		t.Fatal(v.Problem())
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timeout")
}

func TestEntriesCarryStateEntityAndConfirmation(t *testing.T) {
	f, srv := newFake(t)
	f.body.Store(`{"worked":[{"call":"k1abc","band":"20m","mode_class":"phone","mode":"SSB","at":"2025-04-01T10:00:00Z","dxcc":291,"state":"ny"}]}`)
	v := New(Config{URL: srv.URL}, quiet)
	if err := v.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	es := v.Entries()
	if len(es) != 1 || es[0].Call != "K1ABC" || es[0].DXCC != 291 || es[0].State != "NY" || es[0].Band != "20m" || es[0].ModeClass != "phone" || es[0].Confirmed {
		t.Fatalf("%+v", es)
	}
	// a merge event brings the confirmation and a corrected state
	v.Apply([]byte(`{"id":"9","action":"merged","qso":{"own_call":"DL9ET","call":"K1ABC","band":"20m","mode":"SSB","mode_class":"phone","at":"2025-04-01T10:00:00Z","updated":"2025-07-01T10:00:00Z","confirmed":true,"dxcc":291,"state":"CA"}}`))
	es = v.Entries()
	if len(es) != 1 || !es[0].Confirmed || es[0].State != "CA" {
		t.Fatalf("%+v", es)
	}
}

func TestSameSecondEventsBothApply(t *testing.T) {
	_, srv := newFake(t)
	v := New(Config{URL: srv.URL, Own: "DL9ET"}, quiet)
	v.Load(context.Background())
	v.Apply(ev("added", "7", "DL9ET", "K1ABC", "15m", "cw", false, "2025-07-01T10:00:01Z"))
	v.Apply(ev("merged", "7", "DL9ET", "K1ABC", "15m", "cw", true, "2025-07-01T10:00:01Z"))
	found := false
	for _, c := range v.Confirmed() {
		found = found || c.Band == "15m"
	}
	if !found {
		t.Fatalf("the merged event of the same second was dropped: %v", v.Confirmed())
	}
}
