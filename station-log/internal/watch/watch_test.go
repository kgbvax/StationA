package watch

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"station-log/internal/importer"
	"station-log/internal/store"
)

const adiA = `<CALL:5>K1ABC<QSO_DATE:8>20261101<TIME_ON:6>101500<BAND:3>20m<MODE:3>SSB<STATION_CALLSIGN:5>DL9ET<EOR>`
const adiB = `<CALL:5>W1AW <QSO_DATE:8>20261101<TIME_ON:4>1200<BAND:3>40m<MODE:2>CW<STATION_CALLSIGN:5>DL9ET<EOR>`

func setup(t *testing.T) (*Watcher, string, *int) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	events := 0
	imp := &importer.Importer{Store: st, Window: 5 * time.Minute, DefaultOwn: "DL9ET", Log: log,
		Sink: func(e []store.Event) { events += len(e) }}
	dir := t.TempDir()
	w := &Watcher{Dir: dir, Interval: time.Second, Source: "adif-drop", Store: st, Importer: imp, Log: log}
	return w, dir, &events
}

func write(t *testing.T, dir, name, body string, age time.Duration) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-age)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func TestImportsOnceAndSkipsOthers(t *testing.T) {
	w, dir, events := setup(t)
	write(t, dir, "a.adi", adiA, time.Minute)
	write(t, dir, "b.ADIF", adiB, time.Minute)
	write(t, dir, "c.txt", adiA, time.Minute)
	if err := os.Mkdir(filepath.Join(dir, "sub.adi"), 0o700); err != nil {
		t.Fatal(err)
	}
	w.Scan(context.Background())
	s := w.Status()
	if s.Files != 2 || s.Added != 2 || s.Error != "" || s.LastFile != "b.ADIF" || *events != 2 {
		t.Fatalf("%+v events=%d", s, *events)
	}
	w.Scan(context.Background()) // unchanged: nothing
	if s2 := w.Status(); s2.Files != 2 {
		t.Fatalf("%+v", s2)
	}
	// a restart (new Watcher, same store) does not re-import
	w2 := &Watcher{Dir: dir, Interval: time.Second, Source: "adif-drop", Store: w.Store, Importer: w.Importer, Log: w.Log}
	w2.Scan(context.Background())
	if s3 := w2.Status(); s3.Files != 0 {
		t.Fatalf("%+v", s3)
	}
	// a changed file is imported again; merge keeps the log at 2 QSOs
	write(t, dir, "a.adi", adiA+"\n", time.Minute)
	w.Scan(context.Background())
	if n, _ := w.Store.Count(); n != 2 || w.Status().Files != 3 {
		t.Fatalf("n=%d %+v", n, w.Status())
	}
	if _, err := os.Stat(filepath.Join(dir, "a.adi")); err != nil {
		t.Fatal("file must stay in place")
	}
}

func TestUnstableFileWaits(t *testing.T) {
	w, dir, _ := setup(t)
	write(t, dir, "new.adi", adiA, time.Second)
	w.Scan(context.Background())
	if w.Status().Files != 0 {
		t.Fatal("a file written 1 s ago must wait")
	}
	w.Now = func() time.Time { return time.Now().Add(10 * time.Second) }
	w.Scan(context.Background())
	if w.Status().Files != 1 {
		t.Fatalf("%+v", w.Status())
	}
}

func TestBadFileErrorSurvivesGoodFileInSamePass(t *testing.T) {
	w, dir, _ := setup(t)
	write(t, dir, "a-bad.adi", "this is not adif", time.Minute)
	write(t, dir, "b-good.adi", adiA, time.Minute)
	w.Scan(context.Background())
	s := w.Status()
	if s.Failed != 1 || s.Files != 1 || s.Added != 1 {
		t.Fatalf("%+v", s)
	}
	if !strings.HasPrefix(s.Error, "a-bad.adi: ") {
		t.Fatalf("the bad file's error must stay visible: %+v", s)
	}
	// still visible on the next pass, though the file is not retried
	w.Scan(context.Background())
	if s2 := w.Status(); !strings.HasPrefix(s2.Error, "a-bad.adi: ") || s2.Failed != 1 {
		t.Fatalf("%+v", s2)
	}
}

func TestFailedFileNotRetriedUnchanged(t *testing.T) {
	w, dir, _ := setup(t)
	write(t, dir, "bad.adi", "this is not adif", time.Minute)
	changes := 0
	w.OnChange = func() { changes++ }
	for i := 0; i < 3; i++ {
		w.Scan(context.Background())
	}
	if s := w.Status(); s.Failed != 1 || s.Error == "" {
		t.Fatalf("%+v", s)
	}
	if changes != 1 {
		t.Fatalf("state republished %d times, want 1", changes)
	}
}

func TestChangedFailedFileRetried(t *testing.T) {
	w, dir, _ := setup(t)
	write(t, dir, "x.adi", "this is not adif", time.Minute)
	w.Scan(context.Background())
	if w.Status().Failed != 1 {
		t.Fatalf("%+v", w.Status())
	}
	write(t, dir, "x.adi", adiA, time.Minute) // changed: size and mtime
	w.Scan(context.Background())
	if s := w.Status(); s.Files != 1 || s.Failed != 1 || s.Error != "" || s.Added != 1 {
		t.Fatalf("%+v", s)
	}
	// a failing file that disappears clears the error
	write(t, dir, "y.adi", "junk", time.Minute)
	w.Scan(context.Background())
	if w.Status().Error == "" {
		t.Fatal("want error")
	}
	if err := os.Remove(filepath.Join(dir, "y.adi")); err != nil {
		t.Fatal(err)
	}
	w.Scan(context.Background())
	if s := w.Status(); s.Error != "" {
		t.Fatalf("%+v", s)
	}
}

func TestRunWaitsForReady(t *testing.T) {
	w, dir, _ := setup(t)
	write(t, dir, "a.adi", adiA, time.Minute)
	ready := make(chan struct{})
	w.Ready = ready
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	time.Sleep(150 * time.Millisecond)
	if w.Status().Files != 0 {
		t.Fatal("scanned before ready")
	}
	close(ready)
	deadline := time.Now().Add(3 * time.Second)
	for w.Status().Files != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if w.Status().Files != 1 {
		t.Fatal("no scan after ready")
	}
	cancel()
	<-done
}

func TestMissingDirThenAppears(t *testing.T) {
	w, dir, _ := setup(t)
	w.Dir = filepath.Join(dir, "later")
	w.Scan(context.Background())
	if w.Status().Error == "" {
		t.Fatal("want an error for a missing directory")
	}
	if err := os.Mkdir(w.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, w.Dir, "a.adi", adiA, time.Minute)
	w.Scan(context.Background())
	if s := w.Status(); s.Error != "" || s.Files != 1 {
		t.Fatalf("%+v", s)
	}
}
