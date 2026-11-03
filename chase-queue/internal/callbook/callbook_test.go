package callbook

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"chase-queue/internal/qrz"
	"chase-queue/internal/store"
)

type fake struct {
	mu    sync.Mutex
	asked []string
	err   error
}

func (f *fake) Lookup(_ context.Context, call string) (qrz.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, call)
	if f.err != nil {
		return qrz.Record{}, f.err
	}
	if call == "DL1ABC" {
		return qrz.Record{Call: "DL1ABC", Name: "Hans", Grid: "JO32we", Lat: 52.2, Lon: 7.8, HasLL: true}, nil
	}
	return qrz.Record{}, qrz.ErrNotFound
}

func (f *fake) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.asked) }

func newBook(t *testing.T, f *fake, st *store.Store) (*Book, chan struct{}) {
	t.Helper()
	changed := make(chan struct{}, 8)
	b := New(Config{Looker: f, Cache: st, Changed: func() { changed <- struct{}{} }}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.Run(ctx)
	return b, changed
}

func openStore(t *testing.T) *store.Store {
	st, err := store.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func wait(t *testing.T, c chan struct{}) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(2 * time.Second):
		t.Fatal("no lookup landed")
	}
}

func TestPortableLookupAndCache(t *testing.T) {
	st := openStore(t)
	f := &fake{}
	b, changed := newBook(t, f, st)
	if b.Info("EA8/DL1ABC") != nil {
		t.Fatal("answered before the lookup")
	}
	wait(t, changed)
	info := b.Info("EA8/DL1ABC")
	if info == nil || info.Name != "Hans" || info.Call != "DL1ABC" || info.Lat == nil || *info.Lat != 52.2 {
		t.Fatalf("%+v", info)
	}
	if f.count() != 2 { // EA8/DL1ABC not found, then DL1ABC
		t.Fatalf("asked %v", f.asked)
	}
	// a fresh book (restart) answers from the cache without asking QRZ
	f2 := &fake{}
	b2, changed2 := newBook(t, f2, st)
	b2.Info("EA8/DL1ABC")
	wait(t, changed2)
	if b2.Info("EA8/DL1ABC") == nil || f2.count() != 0 {
		t.Fatalf("asked %v", f2.asked)
	}
}

func TestNotFoundIsCachedAndBadCallsSkipped(t *testing.T) {
	f := &fake{}
	b, changed := newBook(t, f, openStore(t))
	b.Info("ZZ9ZZZ")
	wait(t, changed)
	for range 3 {
		if b.Info("ZZ9ZZZ") != nil {
			t.Fatal("not-found answered")
		}
	}
	if b.Info("not a call!") != nil {
		t.Fatal("bad call")
	}
	time.Sleep(50 * time.Millisecond)
	if f.count() != 1 {
		t.Fatalf("asked %v", f.asked)
	}
}

func TestAuthErrorBacksOff(t *testing.T) {
	f := &fake{err: qrz.ErrAuth}
	b, changed := newBook(t, f, openStore(t))
	b.Info("DL1ABC")
	wait(t, changed)
	if b.Problem() == "" {
		t.Fatal("no problem reported")
	}
	b.Info("DL1ABC")
	b.Info("K1ABC")
	time.Sleep(50 * time.Millisecond)
	if f.count() != 1 {
		t.Fatalf("asked %v", f.asked)
	}
}

func TestHomeCall(t *testing.T) {
	for in, want := range map[string]string{"EA8/DL1ABC": "DL1ABC", "DL1ABC/P": "DL1ABC", "VP2E/W1AW": "W1AW", "DL1ABC": "DL1ABC"} {
		if got := HomeCall(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}
