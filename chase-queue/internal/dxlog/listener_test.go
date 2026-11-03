package dxlog

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"chase-queue/internal/store"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := c.LocalAddr().String()
	c.Close()
	return a
}

func start(t *testing.T, cfg ListenerConfig) (*Listener, func()) {
	t.Helper()
	l := NewListener(cfg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for !l.State().Listening {
		if time.Now().After(deadline) {
			t.Fatal("not listening")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return l, func() { cancel(); <-done }
}

func send(t *testing.T, addr string, b []byte) {
	t.Helper()
	c, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(b); err != nil {
		t.Fatal(err)
	}
}

func lo() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")} }

func active(name string) func() (string, bool) { return func() (string, bool) { return name, true } }

func recv(t *testing.T, l *Listener) (Event, bool) {
	select {
	case e := <-l.Events():
		return e, true
	case <-time.After(500 * time.Millisecond):
		return Event{}, false
	}
}

func TestListenerAcceptsAndFilters(t *testing.T) {
	addr := freeAddr(t)
	l, stop := start(t, ListenerConfig{Listen: addr, AllowFrom: lo(), Active: active("ukeidxssb")})
	defer stop()
	send(t, addr, fixture(t, "contactinfo.xml"))
	e, ok := recv(t, l)
	if !ok || e.QSO.Call != "G3XYZ" {
		t.Fatalf("not delivered: %+v %v", e, ok)
	}
	// other root ignored
	send(t, addr, fixture(t, "radioinfo.xml"))
	// other contest dropped with its name recorded
	send(t, addr, []byte(strings.Replace(string(fixture(t, "contactinfo.xml")), "UKEIDXSSB", "CQWWSSB", 1)))
	time.Sleep(200 * time.Millisecond)
	s := l.State()
	if s.Accepted != 1 || s.Ignored != 1 || s.DroppedContest != 1 || s.LastContestName != "CQWWSSB" {
		t.Fatalf("%+v", s)
	}
}

func TestListenerAllowFrom(t *testing.T) {
	addr := freeAddr(t)
	l, stop := start(t, ListenerConfig{Listen: addr, AllowFrom: []netip.Prefix{netip.MustParsePrefix("10.9.9.0/24")}, Active: active("")})
	defer stop()
	send(t, addr, fixture(t, "contactinfo.xml"))
	time.Sleep(200 * time.Millisecond)
	if s := l.State(); s.DroppedHost != 1 || s.Accepted != 0 {
		t.Fatalf("%+v", s)
	}
	l2addr := freeAddr(t)
	l2, stop2 := start(t, ListenerConfig{Listen: l2addr})
	defer stop2()
	send(t, l2addr, fixture(t, "contactinfo.xml"))
	time.Sleep(200 * time.Millisecond)
	if s := l2.State(); s.DroppedHost != 1 {
		t.Fatalf("empty allow_from must accept nothing: %+v", s)
	}
}

func TestListenerCompleteLogAndOff(t *testing.T) {
	addr := freeAddr(t)
	on := true
	var mu sync.Mutex
	l, stop := start(t, ListenerConfig{Listen: addr, AllowFrom: lo(),
		Active: func() (string, bool) { mu.Lock(); defer mu.Unlock(); return "", on }})
	defer stop()
	send(t, addr, fixture(t, "contactreplace.xml"))
	if _, ok := recv(t, l); !ok || !l.State().CompleteLogSeen {
		t.Fatal("replace not seen")
	}
	l.ResetCompleteLog()
	if l.State().CompleteLogSeen {
		t.Fatal("reset failed")
	}
	mu.Lock()
	on = false
	mu.Unlock()
	send(t, addr, fixture(t, "contactinfo.xml"))
	time.Sleep(200 * time.Millisecond)
	if s := l.State(); s.Accepted != 1 || s.DroppedContest != 0 {
		t.Fatalf("off must drop silently: %+v", s)
	}
}

func TestListenerBindRetry(t *testing.T) {
	hold, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := hold.LocalAddr().String()
	l := NewListener(ListenerConfig{Listen: addr, AllowFrom: lo(), RetryMin: 30 * time.Millisecond, RetryMax: 60 * time.Millisecond}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	time.Sleep(150 * time.Millisecond)
	if s := l.State(); s.Listening || s.LastError == "" {
		t.Fatalf("expected bind error: %+v", s)
	}
	hold.Close()
	deadline := time.Now().Add(2 * time.Second)
	for !l.State().Listening {
		if time.Now().After(deadline) {
			t.Fatal("never bound")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if l.State().LastError != "" {
		t.Fatal("error not cleared")
	}
}

func TestListenerBurst(t *testing.T) {
	addr := freeAddr(t)
	l, stop := start(t, ListenerConfig{Listen: addr, AllowFrom: lo(), QueueLen: 4096})
	defer stop()
	if l.State().RecvBuffer < 0 {
		t.Fatal("buffer")
	}
	c, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tmpl := string(fixture(t, "contactreplace.xml"))
	const n = 2000
	got := make(chan int)
	go func() {
		k := 0
		for range l.Events() {
			k++
			if k == n {
				break
			}
		}
		got <- k
	}()
	for i := 0; i < n; i++ {
		c.Write([]byte(tmpl))
		if i%100 == 99 {
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case k := <-got:
		if k != n {
			t.Fatalf("got %d", k)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("lost datagrams: %+v", l.State())
	}
}

type fakeStore struct {
	mu   sync.Mutex
	ups  [][]store.ContestQSO
	dels []string
}

func (f *fakeStore) UpsertContestQSOs(_ int64, q []store.ContestQSO, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ups = append(f.ups, append([]store.ContestQSO(nil), q...))
	return nil
}
func (f *fakeStore) DeleteContestQSO(_ int64, st, id string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dels = append(f.dels, st+"/"+id)
	return 1, nil
}

func TestWriterBatchesAndDeletes(t *testing.T) {
	fs := &fakeStore{}
	ch := make(chan Event, 10)
	_, q, _ := Parse(fixture(t, "contactinfo.xml"))
	for i := 0; i < 5; i++ {
		ch <- Event{Kind: KindQSO, QSO: q}
	}
	_, d, _ := Parse(fixture(t, "contactdelete.xml"))
	ch <- Event{Kind: KindDelete, QSO: d}
	ch <- Event{Kind: KindReplace, QSO: q} // a replace only upserts
	close(ch)
	Write(context.Background(), ch, WriterConfig{Store: fs, Contest: func() int64 { return 1 }}, nil)
	if len(fs.ups) != 2 || len(fs.ups[0]) != 5 || len(fs.ups[1]) != 1 {
		t.Fatalf("batches: %v", fs.ups)
	}
	if len(fs.dels) != 1 {
		t.Fatalf("dels: %v", fs.dels)
	}
}

func TestWriterNoContestDrops(t *testing.T) {
	fs := &fakeStore{}
	ch := make(chan Event, 1)
	_, q, _ := Parse(fixture(t, "contactinfo.xml"))
	ch <- Event{Kind: KindQSO, QSO: q}
	close(ch)
	Write(context.Background(), ch, WriterConfig{Store: fs, Contest: func() int64 { return 0 }}, nil)
	if len(fs.ups) != 0 {
		t.Fatal("stored without a contest")
	}
}

func TestWriterDropsEventsFromAnotherInstance(t *testing.T) {
	fs := &fakeStore{}
	ch := make(chan Event, 3)
	_, q, _ := Parse(fixture(t, "contactinfo.xml"))
	ch <- Event{Kind: KindQSO, QSO: q, Instance: 1}
	ch <- Event{Kind: KindQSO, QSO: q, Instance: 2}
	close(ch)
	Write(context.Background(), ch, WriterConfig{Store: fs, Contest: func() int64 { return 2 }}, nil)
	if len(fs.ups) != 1 || len(fs.ups[0]) != 1 {
		t.Fatalf("%v", fs.ups)
	}
}
