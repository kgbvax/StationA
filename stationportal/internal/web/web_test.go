package web

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

	"stationportal/internal/bus"
	"stationportal/internal/inventory"
	"stationportal/internal/probe"
)

func boolp(b bool) *bool { return &b }

func TestSlotHealth(t *testing.T) {
	cases := []struct {
		status string
		dev    *bool
		want   Health
	}{
		{"", nil, Unknown},
		{"offline", boolp(true), Down},
		{"online", boolp(false), Warn},
		{"online", nil, Up},
		{"online", boolp(true), Up},
	}
	for _, c := range cases {
		if h, _ := slotHealth(c.status, c.dev); h != c.want {
			t.Errorf("%q/%v: got %s want %s", c.status, c.dev, h, c.want)
		}
	}
}

func TestAggregate(t *testing.T) {
	h := map[string]Health{"a": Up, "b": Warn, "c": Down}
	for _, c := range []struct {
		addrs []string
		want  Health
	}{
		{[]string{"a"}, Up},
		{[]string{"a", "b"}, Warn},
		{[]string{"a", "c"}, Down},
		{[]string{"x"}, Unknown},
		{[]string{"a", "x"}, Warn},
	} {
		if got, _ := aggregate(c.addrs, h); got != c.want {
			t.Errorf("%v: got %s want %s", c.addrs, got, c.want)
		}
	}
}

const testInv = `
[[link]]
group = "Station"
name  = "ui"
url   = "%s"
host  = "scmino"
[[link]]
group = "Infra"
name  = "ext"
url   = "https://example.invalid/"
probe = "none"
[[slot]]
address   = "muehle/hf/pa"
component = "acom"
device    = "ACOM 1200S"
[[software]]
name  = "acom"
slots = ["muehle/hf/pa"]
[[software]]
name = "ui-sw"
link = "ui"
[[host]]
name = "scmino"
[[resource]]
name = "Ultrabeam"
kind = "antenna"
`

func newTestServer(t *testing.T) (*Server, *bus.Store) {
	t.Helper()
	ui := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(ui.Close)
	inv, err := inventory.Parse([]byte(strings.Replace(testInv, "%s", ui.URL+"/", 1)))
	if err != nil {
		t.Fatal(err)
	}
	store := bus.NewStore("muehle")
	now := time.Now()
	store.Update("muehle/hf/pa/meta", []byte(`{"device":{"model":"ACOM 1200S","serial":"acom-1200s"},"host":"shari"}`), now)
	store.Update("muehle/hf/pa/status", []byte("online"), now)
	store.Update("muehle/hf/pa/state", []byte(`{"device_online":false}`), now)
	store.Update("muehle/uhf/radio-bench/status", []byte("offline"), now)
	store.SetConnected(true, now)
	pr := probe.New(inv.Links, time.Second)
	pr.ProbeAll(context.Background())
	return &Server{Inv: inv, Store: store, Probes: pr, Broker: "tcp://b:1883", Host: "scmino",
		Revision: "0123456789abcdef+dirty", RefreshS: 30, Log: slog.New(slog.DiscardHandler)}, store
}

func TestBuildPageMergesLiveAndStatic(t *testing.T) {
	s, _ := newTestServer(t)
	p := s.page()
	if p.Summary.LinksTotal != 1 || p.Summary.LinksUp != 1 {
		t.Errorf("links summary %+v", p.Summary)
	}
	if len(p.Slots) != 2 || p.Slots[0].Address != "muehle/hf/pa" || p.Slots[1].Known {
		t.Fatalf("slots %+v", p.Slots)
	}
	pa := p.Slots[0]
	if pa.Health != Warn || pa.Live.Serial != "acom-1200s" || pa.DeviceOnline != "no" {
		t.Errorf("pa view %+v", pa)
	}
	if p.Slots[1].Health != Down {
		t.Errorf("extra slot health %s", p.Slots[1].Health)
	}
	sw := map[string]SoftwareView{}
	for _, v := range p.Software {
		sw[v.Name] = v
	}
	if sw["acom"].Health != Warn || sw["ui-sw"].Health != Up {
		t.Errorf("software health acom=%s ui-sw=%s", sw["acom"].Health, sw["ui-sw"].Health)
	}
	if !p.BusConnected {
		t.Error("bus should be connected")
	}
}

func TestRoutes(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Routes()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK {
		t.Fatalf("page status %d: %s", rec.Code, body)
	}
	for _, want := range []string{"Mühle station", "muehle/hf/pa", "serial acom-1200s", "Ultrabeam", "0123456&#43;dirty", `class="dot warn"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("page lacks %q", want)
		}
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/inventory", nil))
	var p Page
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || len(p.Slots) != 2 {
		t.Fatalf("json: %v %d", err, len(p.Slots))
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown path status %d", rec.Code)
	}
}

func TestAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		5 * time.Second: "5 s", 3 * time.Minute: "3 min", 5 * time.Hour: "5 h", 72 * time.Hour: "3 d", -time.Second: "now",
	} {
		if got := Age(d); got != want {
			t.Errorf("%v: %q want %q", d, got, want)
		}
	}
}

func TestDeviceTextOmitsWhatTheTitleSays(t *testing.T) {
	title, detail := deviceText(
		inventory.Slot{Device: "Shelly Plus 1PM — station master mains"},
		bus.Facts{Manufacturer: "Shelly", Model: "Shelly Plus 1PM", Serial: "abc"}, true)
	if title != "Shelly Plus 1PM — station master mains" || detail != "serial abc" {
		t.Fatalf("got %q / %q", title, detail)
	}
	_, detail = deviceText(inventory.Slot{Device: "FlexRadio FLEX-8400"}, bus.Facts{Model: "FLEX-6000", Serial: "x"}, true)
	if detail != "FLEX-6000 · serial x" {
		t.Fatalf("model mismatch must show the live model: %q", detail)
	}
	title, detail = deviceText(inventory.Slot{}, bus.Facts{Model: "IC-9700"}, false)
	if title != "IC-9700" || detail != "not in the inventory" {
		t.Fatalf("unknown slot: %q / %q", title, detail)
	}
}
