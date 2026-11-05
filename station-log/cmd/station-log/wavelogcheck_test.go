package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"station-log/internal/config"
)

const testToken = "wl2_check-token-not-real"

// fake of GET /api/v2/token and GET /api/v2/station (API v2 overview:
// Authorization: Bearer, envelope {"data","meta"}, error envelope).
func fakeInstance(scopes []string) *httptest.Server {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v)
	}
	guard := func(h func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+testToken {
				reply(w, 401, map[string]any{"error": map[string]any{"code": "invalid_token", "message": "Invalid or revoked API token"}})
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/index.php/api/v2/token", guard(func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"data": map[string]any{"name": "station-log", "scopes": scopes}, "meta": map[string]any{}})
	}))
	mux.HandleFunc("/index.php/api/v2/station", guard(func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"data": []map[string]any{
			{"station_id": 1, "station_callsign": "DL9ET", "station_profile_name": "Home"},
			{"station_id": 12, "station_callsign": "DL9ET/P", "station_profile_name": "Portable"},
		}, "meta": map[string]any{}})
	}))
	return httptest.NewServer(mux)
}

func checkCfg(url, token string) config.Config {
	cfg := config.Defaults()
	cfg.Wavelog.URL, cfg.Wavelog.Key = url, token
	return cfg
}

func TestWavelogCheckPrintsScopesAndStationsNotToken(t *testing.T) {
	srv := fakeInstance([]string{"qso:read", "station:read"})
	defer srv.Close()
	var out, errOut bytes.Buffer
	if code := wavelogCheck(context.Background(), checkCfg(srv.URL, testToken), &out, &errOut); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errOut.String())
	}
	got := out.String()
	for _, want := range []string{"qso:read", "station:read", "MISSING scope qso:write", "id 1 ", "id 12", "DL9ET/P", "Portable"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got+errOut.String(), testToken) {
		t.Fatal("token printed")
	}
}

func TestWavelogCheckInvalidToken(t *testing.T) {
	srv := fakeInstance(nil)
	defer srv.Close()
	var out, errOut bytes.Buffer
	if code := wavelogCheck(context.Background(), checkCfg(srv.URL, "wl2_wrong-token"), &out, &errOut); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut.String(), "invalid_token") || strings.Contains(out.String()+errOut.String(), "wl2_wrong-token") {
		t.Fatalf("out %q err %q", out.String(), errOut.String())
	}
}

func TestWavelogCheckRefusesLegacyKeyWithoutCalling(t *testing.T) {
	var errOut bytes.Buffer
	if code := wavelogCheck(context.Background(), checkCfg("https://wl.example.org", "legacykey"), &bytes.Buffer{}, &errOut); code != 2 ||
		!strings.Contains(errOut.String(), "wl2_") || strings.Contains(errOut.String(), "legacykey") {
		t.Fatalf("code %d err %q", code, errOut.String())
	}
}
