package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"stationportal/internal/inventory"
)

func TestProbeKinds(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer ok.Close()
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer auth.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	deadAddr := dead.Addr().String()
	dead.Close()

	links := []inventory.Link{
		{Name: "ok", URL: ok.URL + "/"},
		{Name: "auth", URL: auth.URL + "/"},
		{Name: "tcp", URL: "mqtt://" + ln.Addr().String()},
		{Name: "dead", URL: "tcp://" + deadAddr},
		{Name: "ext", URL: "https://example.invalid/", Probe: "none"},
	}
	p := New(links, 2*time.Second)
	p.ProbeAll(context.Background())

	want := map[string]bool{"ok": true, "auth": true, "tcp": true, "dead": false, "ext": false}
	for _, l := range links {
		r, found := p.Result(l.URL)
		if !found {
			t.Fatalf("%s: no result", l.Name)
		}
		if r.Up != want[l.Name] {
			t.Errorf("%s: up=%v detail=%q", l.Name, r.Up, r.Detail)
		}
	}
	if r, _ := p.Result(auth.URL + "/"); r.Detail != "HTTP 401" {
		t.Errorf("auth detail %q", r.Detail)
	}
	if r, _ := p.Result("https://example.invalid/"); r.Kind != "none" {
		t.Errorf("none kind %q", r.Kind)
	}
}
