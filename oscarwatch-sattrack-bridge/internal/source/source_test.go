package source

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// wsServer runs h for every upgraded connection and returns the ws:// URL.
func wsServer(t *testing.T, h func(*websocket.Conn)) string {
	t.Helper()
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		h(c)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// drain reads until the client goes away — which also makes gorilla answer
// pings (the default ping handler runs inside the read).
func drain(c *websocket.Conn) {
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			return
		}
	}
}

func client(url string, ping time.Duration) *Client { return New(url, ping, quiet) }

func TestSnapshotOnConnectThenCancel(t *testing.T) {
	url := wsServer(t, func(c *websocket.Conn) {
		_ = c.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3}) // ignored
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"satelliteStatus"}`))
		drain(c)
	})
	ctx, cancel := context.WithCancel(context.Background())
	var connected atomic.Bool
	frames := make(chan string, 4)
	done := make(chan error, 1)
	go func() {
		done <- client(url, 0).Run(ctx, func() { connected.Store(true) }, func(b []byte) { frames <- string(b) })
	}()

	select {
	case f := <-frames:
		if f != `{"type":"satelliteStatus"}` {
			t.Fatalf("frame = %q", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame")
	}
	if !connected.Load() {
		t.Fatal("onConnect not called before the first frame")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if len(frames) != 0 {
		t.Fatal("binary frame delivered")
	}
}

func TestServerCloseReturnsError(t *testing.T) {
	url := wsServer(t, func(c *websocket.Conn) {
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{}`))
	})
	err := client(url, 0).Run(context.Background(), nil, func([]byte) {})
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want a read error", err)
	}
}

func TestDialFailure(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close() // nothing listens here now

	called := false
	err = client("ws://"+addr+"/", 0).Run(context.Background(), func() { called = true }, func([]byte) {})
	if err == nil || !strings.Contains(err.Error(), "dial") {
		t.Fatalf("Run = %v, want dial error", err)
	}
	if called {
		t.Fatal("onConnect called on a failed dial")
	}
}

func TestPongKeepsLinkAlive(t *testing.T) {
	url := wsServer(t, drain) // answers pings, never sends data
	c := client(url, 20*time.Millisecond)
	c.PongWait = 60 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	err := c.Run(ctx, nil, func([]byte) {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v: a silent server that answers pings must stay connected", err)
	}
}

func TestMissingPongDropsLink(t *testing.T) {
	url := wsServer(t, func(c *websocket.Conn) {
		c.SetPingHandler(func(string) error { return nil }) // swallow pings
		drain(c)
	})
	c := client(url, 20*time.Millisecond)
	c.PongWait = 60 * time.Millisecond

	start := time.Now()
	err := c.Run(context.Background(), nil, func([]byte) {})
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want a read-deadline error", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("dead link detected only after %v", time.Since(start))
	}
}

func TestOversizeFrameDropsLink(t *testing.T) {
	url := wsServer(t, func(c *websocket.Conn) {
		_ = c.WriteMessage(websocket.TextMessage, []byte(strings.Repeat("x", 4096)))
		drain(c)
	})
	c := client(url, 0)
	c.ReadLimit = 1024
	got := 0
	err := c.Run(context.Background(), nil, func([]byte) { got++ })
	if err == nil || got != 0 {
		t.Fatalf("Run = %v frames=%d, want read-limit error and no frame", err, got)
	}
}

func TestNoGoroutineLeakAcrossReconnects(t *testing.T) {
	url := wsServer(t, func(c *websocket.Conn) {
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{}`))
	})
	base := runtime.NumGoroutine()
	for range 20 {
		_ = client(url, 10*time.Millisecond).Run(context.Background(), nil, func([]byte) {})
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base+2 {
		t.Fatalf("goroutines %d after 20 runs, baseline %d", n, base)
	}
}
