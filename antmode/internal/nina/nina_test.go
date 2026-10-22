// SPDX-License-Identifier: AGPL-3.0-or-later

package nina

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Real samples fetched from https://warnung.bund.de/api31 on 2026-10-22:
// testdata/dashboard_145210000000.json (Erzgebirgskreis) and its one DWD
// message, testdata/warning_sturmboeen.json (STURMBÖEN, II 52, trimmed to
// the de/en info blocks).
const realID = "dwd.2.49.0.0.276.0.DWD.PVW.1792572900000.e4e0bc6f-3fbe-49bd-ad0d-b3491e51ca2a.MUL"

var codes = map[string]bool{"31": true, "33": true, "51": true, "52": true, "53": true, "54": true, "55": true, "56": true}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// server serves routes (path → body); a body starting with "!" is an HTTP
// status code instead.
type server struct {
	mu     sync.Mutex
	routes map[string]string
	hits   map[string]int
	ua     string
}

func newServer(t *testing.T, routes map[string]string) (*server, *httptest.Server) {
	s := &server{routes: routes, hits: map[string]int{}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits[r.URL.Path]++
		s.ua = r.Header.Get("User-Agent")
		body, ok := s.routes[r.URL.Path]
		s.mu.Unlock()
		switch {
		case !ok:
			http.NotFound(w, r)
		case body == "!500":
			http.Error(w, "boom", http.StatusInternalServerError)
		case body == "!sleep":
			time.Sleep(500 * time.Millisecond)
			_, _ = w.Write([]byte("[]"))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(ts.Close)
	return s, ts
}

func client(base string) *Client {
	return &Client{BaseURL: base, UserAgent: "antmode-test", HTTP: &http.Client{Timeout: 2 * time.Second}, Codes: codes}
}

func TestFetchRealSample(t *testing.T) {
	s, ts := newServer(t, map[string]string{
		"/dashboard/145210000000.json":  string(mustRead(t, "testdata/dashboard_145210000000.json")),
		"/warnings/" + realID + ".json": string(mustRead(t, "testdata/warning_sturmboeen.json")),
	})
	// A municipality-level ARS (Oberwiesenthal) is zero-padded to the district.
	ws, err := client(ts.URL).Fetch(context.Background(), "145215555555")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 {
		t.Fatalf("got %d warnings, want 1: %+v", len(ws), ws)
	}
	w := ws[0]
	if w.ID != realID || w.II != "52" || w.Event != "STURMBÖEN" || w.Headline != "Amtliche WARNUNG vor STURMBÖEN" {
		t.Fatalf("warning = %+v", w)
	}
	cest := time.FixedZone("CEST", 2*3600)
	if !w.Onset.Equal(time.Date(2026, 10, 22, 0, 0, 0, 0, cest)) || !w.Expires.Equal(time.Date(2026, 10, 22, 15, 0, 0, 0, cest)) {
		t.Fatalf("onset/expires = %v / %v", w.Onset, w.Expires)
	}
	if s.ua != "antmode-test" {
		t.Fatalf("User-Agent = %q", s.ua)
	}
}

func dashEntry(id, msgType string) map[string]any {
	return map[string]any{"id": id, "payload": map[string]any{"data": map[string]any{"msgType": msgType, "provider": "DWD"}}}
}

func capMsg(id, msgType, event, ii, onset, expires string) string {
	info := map[string]any{"language": "de-DE", "event": event, "headline": "Amtliche WARNUNG vor " + event,
		"onset": onset, "effective": "2026-10-21T10:00:00+02:00"}
	if ii != "" {
		info["eventCode"] = []map[string]string{{"valueName": "GROUP", "value": "WIND"}, {"valueName": "II", "value": ii}}
	} else {
		info["eventCode"] = nil
	}
	if expires != "" {
		info["expires"] = expires
	} else {
		info["expires"] = nil
	}
	b, _ := json.Marshal(map[string]any{"identifier": id, "status": "Actual", "msgType": msgType,
		"sent": "2026-10-21T10:00:00+02:00", "info": []any{info}})
	return string(b)
}

func TestFetchFilters(t *testing.T) {
	dash, _ := json.Marshal([]any{
		dashEntry("dwd.a-windboeen", "Alert"),
		dashEntry("dwd.b-cancel", "Cancel"),
		dashEntry("mow.c-mowas", "Alert"),
		dashEntry("dwd.d-rain-storm", "Alert"),
		dashEntry("dwd.e-vorab", "Alert"),
		dashEntry("dwd.f-null-fields", "Update"),
		dashEntry("dwd.g-open-ended", "Update"),
		dashEntry("dwd.h-cap-cancel", "Alert"),
		dashEntry("dwd.i-orkan", "Update"),
	})
	s, ts := newServer(t, map[string]string{
		"/dashboard/091620000000.json": string(dash),
		"/warnings/dwd.a-windboeen.json": capMsg("dwd.a-windboeen", "Alert", "WINDBÖEN", "51",
			"2026-10-22T06:00:00+02:00", "2026-10-22T18:00:00+02:00"),
		"/warnings/dwd.d-rain-storm.json": capMsg("dwd.d-rain-storm", "Alert", "STARKES GEWITTER", "34",
			"2026-10-22T06:00:00+02:00", "2026-10-22T18:00:00+02:00"),
		"/warnings/dwd.e-vorab.json": capMsg("dwd.e-vorab", "Alert", "VORABINFORMATION ORKANBÖEN", "55",
			"2026-10-22T06:00:00+02:00", "2026-10-23T18:00:00+02:00"),
		"/warnings/dwd.f-null-fields.json": capMsg("dwd.f-null-fields", "Update", "STURMBÖEN", "",
			"2026-10-22T06:00:00+02:00", ""),
		"/warnings/dwd.g-open-ended.json": capMsg("dwd.g-open-ended", "Update", "SCHWERE STURMBÖEN", "53",
			"", ""),
		"/warnings/dwd.h-cap-cancel.json": capMsg("dwd.h-cap-cancel", "Cancel", "STURMBÖEN", "52",
			"2026-10-22T06:00:00+02:00", "2026-10-22T18:00:00+02:00"),
		"/warnings/dwd.i-orkan.json": capMsg("dwd.i-orkan", "Update", "ORKANBÖEN", "55",
			"2026-10-22T20:00:00+02:00", "2026-10-23T02:00:00+02:00"),
	})
	ws, err := client(ts.URL).Fetch(context.Background(), "091620000000")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, w := range ws {
		ids = append(ids, w.ID)
	}
	if got := strings.Join(ids, ","); got != "dwd.a-windboeen,dwd.g-open-ended,dwd.i-orkan" {
		t.Fatalf("qualifying = %s", got)
	}
	// Cancel (dashboard level) and the MoWaS message are never fetched.
	if s.hits["/warnings/dwd.b-cancel.json"] != 0 || s.hits["/warnings/mow.c-mowas.json"] != 0 {
		t.Fatalf("fetched skipped messages: %v", s.hits)
	}
	// Open-ended: onset falls back to effective, no expiry.
	g := ws[1]
	if !g.Expires.IsZero() || !g.Onset.Equal(time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("open-ended = %+v", g)
	}

	// Active: onset <= now < expires, no lead time.
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	act := func(now string) string {
		var out []string
		for _, w := range Active(ws, at(now)) {
			out = append(out, w.ID)
		}
		return strings.Join(out, ",")
	}
	if got := act("2026-10-22T05:59:59+02:00"); got != "dwd.g-open-ended" {
		t.Errorf("before onset: %s", got)
	}
	if got := act("2026-10-22T06:00:00+02:00"); got != "dwd.a-windboeen,dwd.g-open-ended" {
		t.Errorf("at onset: %s", got)
	}
	if got := act("2026-10-22T18:00:00+02:00"); got != "dwd.g-open-ended" {
		t.Errorf("at expiry: %s", got)
	}
	if got := act("2026-10-22T19:59:00+02:00"); got != "dwd.g-open-ended" {
		t.Errorf("future warning is not active (no lead time): %s", got)
	}
	if got := act("2026-10-22T21:00:00+02:00"); got != "dwd.g-open-ended,dwd.i-orkan" {
		t.Errorf("later: %s", got)
	}
}

func TestFetchEmptyDashboard(t *testing.T) {
	_, ts := newServer(t, map[string]string{"/dashboard/091620000000.json": "[]"})
	ws, err := client(ts.URL).Fetch(context.Background(), "091620000000")
	if err != nil || len(ws) != 0 {
		t.Fatalf("ws=%v err=%v", ws, err)
	}
}

func TestFetchFailuresAreErrors(t *testing.T) {
	dash, _ := json.Marshal([]any{dashEntry("dwd.x", "Alert")})
	cases := map[string]map[string]string{
		"dashboard 500":       {"/dashboard/091620000000.json": "!500"},
		"dashboard missing":   {},
		"dashboard malformed": {"/dashboard/091620000000.json": "{not json"},
		"detail 500":          {"/dashboard/091620000000.json": string(dash), "/warnings/dwd.x.json": "!500"},
		"detail 404":          {"/dashboard/091620000000.json": string(dash)},
		"detail malformed":    {"/dashboard/091620000000.json": string(dash), "/warnings/dwd.x.json": `{"info":"x"}`},
		"detail no info":      {"/dashboard/091620000000.json": string(dash), "/warnings/dwd.x.json": `{"msgType":"Alert","info":[]}`},
		"bad onset": {"/dashboard/091620000000.json": string(dash),
			"/warnings/dwd.x.json": capMsg("dwd.x", "Alert", "STURMBÖEN", "52", "yesterday", "")},
	}
	for name, routes := range cases {
		_, ts := newServer(t, routes)
		if ws, err := client(ts.URL).Fetch(context.Background(), "091620000000"); err == nil {
			t.Errorf("%s: no error (ws=%v) — a failed poll must never read as no warnings", name, ws)
		}
	}
}

func TestFetchTimeout(t *testing.T) {
	_, ts := newServer(t, map[string]string{"/dashboard/091620000000.json": "!sleep"})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := client(ts.URL).Fetch(ctx, "091620000000"); err == nil {
		t.Fatal("timeout not reported")
	}
}

func TestDistrictARS(t *testing.T) {
	for in, want := range map[string]string{"091620000000": "091620000000", "145215555555": "145210000000"} {
		if got, err := DistrictARS(in); err != nil || got != want {
			t.Errorf("DistrictARS(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "09162", "0916200000001", "09162000000x"} {
		if _, err := DistrictARS(bad); err == nil {
			t.Errorf("DistrictARS(%q) accepted", bad)
		}
	}
}
