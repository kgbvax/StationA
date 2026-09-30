package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"oscarwatch-sattrack-bridge/internal/bridge"
	"oscarwatch-sattrack-bridge/internal/geo"
	"oscarwatch-sattrack-bridge/internal/source"
)

const so50Frame = `{"type":"satelliteStatus","version":1,"timestampUtc":"2026-07-07T11:04:00.000Z","inRange":true,
 "satellite":{"name":"SO-50","noradId":"27607","modeType":"FM VOICE"},
 "frequencies":{"uplinkHz":435300000,"downlinkHz":145850000,"uplinkMode":"FM","downlinkMode":"FM","isBeaconOnly":false},
 "bands":{"tx":"70cm","rx":"2m"},
 "tracking":{"azimuthDeg":91.7,"elevationDeg":1.9,"rangeKm":2100.5,"rangeRateKmPerSec":-4.92,"isSunlit":true},
 "dopplerStrategy":"full","wispDde":"SO-50 AZ91,7 EL1,9"}`

const noSatFrame = `{"type":"satelliteStatus","version":1,"inRange":false,"satellite":null,"wispDde":"** NO SATELLITE **"}`

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// recPub records every published /state document.
type recPub struct{ ch chan map[string]any }

func (r *recPub) Publish(p []byte) {
	var m map[string]any
	if err := json.Unmarshal(p, &m); err != nil {
		panic(err)
	}
	r.ch <- m
}

func newTestApp() (*app, *recPub) {
	pub := &recPub{ch: make(chan map[string]any, 64)}
	return &app{
		log:  quiet,
		b:    bridge.New(bridge.Meta(bridge.MetaOptions{Slot: "sat-track"})),
		obs:  &geo.Observer{Lat: 52.1875, Lng: 7.875},
		pub:  pub,
		in:   bridge.Input{Err: "oscarwatch: not connected yet"},
		kick: make(chan struct{}, 1),
	}, pub
}

// waitFor reads published snapshots until pred matches one.
func waitFor(t *testing.T, pub *recPub, what string, pred func(map[string]any) bool) map[string]any {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case m := <-pub.ch:
			if pred(m) {
				return m
			}
		case <-timeout:
			t.Fatalf("no snapshot with %s", what)
		}
	}
}

func TestLinkLifecycle(t *testing.T) {
	next := make(chan struct{})
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		// OscarWatch pushes the snapshot on connect, then updates.
		_ = c.WriteMessage(websocket.TextMessage, []byte(so50Frame))
		<-next
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"qsoLogged","version":1,"qso":{"id":1}}`))
		_ = c.WriteMessage(websocket.TextMessage, []byte(`garbage`))
		_ = c.WriteMessage(websocket.TextMessage, []byte(noSatFrame))
		<-next
		// return → close: the link drops
	}))
	defer srv.Close()

	a, pub := newTestApp()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.stateWorker(ctx)
	src := source.New("ws"+strings.TrimPrefix(srv.URL, "http"), 0, quiet)
	go a.wsLoop(ctx, src)

	m := waitFor(t, pub, "SO-50 tracked", func(m map[string]any) bool { return m["sat_name"] == "SO-50" })
	if m["device_online"] != true || m["in_range"] != true || m["az"] != 91.7 || m["sub_lat"] == nil {
		t.Fatalf("tracking snapshot: %v", m)
	}
	next <- struct{}{}

	m = waitFor(t, pub, "satellite cleared", func(m map[string]any) bool { return m["tracking"] == false })
	if m["device_online"] != true || m["sat_name"] != nil {
		t.Fatalf("no-satellite snapshot: %v", m)
	}
	next <- struct{}{}

	m = waitFor(t, pub, "link down", func(m map[string]any) bool { return m["device_online"] == false })
	if e, _ := m["error"].(string); !strings.HasPrefix(e, "oscarwatch: ") {
		t.Fatalf("offline snapshot error = %v", m["error"])
	}
}

func TestBirthRepublishesUnchangedSnapshot(t *testing.T) {
	a, pub := newTestApp()
	a.flush()
	first := <-pub.ch
	if first["device_online"] != false {
		t.Fatalf("initial snapshot: %v", first)
	}

	a.flush() // unchanged, no birth: nothing
	select {
	case m := <-pub.ch:
		t.Fatalf("unchanged snapshot republished: %v", m)
	default:
	}

	a.requestBirth()
	a.flush()
	select {
	case m := <-pub.ch:
		if m["ts"] != first["ts"] {
			t.Fatalf("birth did not republish verbatim: %v vs %v", m["ts"], first["ts"])
		}
	default:
		t.Fatal("birth published nothing")
	}
}

func TestLatestWins(t *testing.T) {
	// Several frames before the worker runs collapse into one publish of the
	// newest — a slow broker never replays stale look angles.
	a, pub := newTestApp()
	a.update(func(in *bridge.Input) { *in = bridge.Input{Online: true} })
	for _, az := range []string{"10.0", "20.0", "30.0"} {
		a.handleFrame([]byte(strings.Replace(so50Frame, `"azimuthDeg":91.7`, `"azimuthDeg":`+az, 1)))
	}
	a.flush()
	m := <-pub.ch
	if m["az"] != 30.0 {
		t.Fatalf("az = %v, want the newest (30)", m["az"])
	}
	select {
	case extra := <-pub.ch:
		t.Fatalf("extra publish: %v", extra)
	default:
	}
}
