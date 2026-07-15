package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"testui/internal/mqtt"
)

func TestRoutesServeStaticAndAPI(t *testing.T) {
	s, _ := newTestServer()
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status=%d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "app.js") {
		t.Errorf("index.html does not reference app.js: %s", body)
	}

	resp2, err := http.Get(srv.URL + "/app.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET /app.js status=%d", resp2.StatusCode)
	}

	resp3, err := http.Get(srv.URL + "/api/tree")
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	b3, _ := io.ReadAll(resp3.Body)
	if !strings.Contains(string(b3), `"slots"`) || !strings.Contains(string(b3), `"order"`) {
		t.Errorf("/api/tree body = %s", b3)
	}
}

func TestStreamSendsSnapshot(t *testing.T) {
	s, _ := newTestServer()
	s.tree.Update(mqtt.Message{Topic: "muehle/hf/radio/state", Payload: []byte(`{"freq_hz":14025000}`), Retained: true, TS: time.Now()})
	s.tree.Update(mqtt.Message{Topic: "muehle/hf/radio/status", Payload: []byte("online"), Retained: true, TS: time.Now()})

	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	// Cancel the request after the snapshot arrives (or after a deadline) so the
	// SSE handler's blocking Read unblocks instead of hanging the test.
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Errorf("content-type=%q", resp.Header.Get("Content-Type"))
	}
	out, _ := io.ReadAll(resp.Body) // returns when ctx cancels the stream
	if !strings.Contains(string(out), "event: snapshot") || !strings.Contains(string(out), "muehle/hf/radio") {
		t.Errorf("snapshot event missing: %q", string(out))
	}
	// Snapshot /state payload must arrive as a raw JSON object (not base64, not a
	// re-parse-failing shape) so the browser can render it.
	if !strings.Contains(string(out), `"payload":{"freq_hz":14025000}`) {
		t.Errorf("snapshot /state payload not raw JSON object: %q", string(out))
	}
	// /status must arrive as a JSON-quoted string the UI can strip.
	if !strings.Contains(string(out), `"payload":"online"`) {
		t.Errorf("snapshot /status payload not JSON string: %q", string(out))
	}
}

// TestStreamUpdateEventShape is the regression test for the critical bug: the live
// `update` event must marshal the tagged Event (lowercase keys, json.RawMessage
// payload), not a raw mqtt.Message (capitalized keys + base64 []byte), or the browser's
// applyUpdate throws on every update and the UI freezes after the initial snapshot.
func TestStreamUpdateEventShape(t *testing.T) {
	s, _ := newTestServer()
	s.tree.Update(mqtt.Message{Topic: "muehle/hf/radio/state", Payload: []byte(`{"freq_hz":14025000}`), Retained: true, TS: time.Now()})

	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// After the stream opens and the snapshot flushes, push a live update.
	go func() {
		time.Sleep(150 * time.Millisecond)
		s.tree.Update(mqtt.Message{Topic: "muehle/hf/pa/state", Payload: []byte(`{"keyed":"tx"}`), Retained: true, TS: time.Now()})
	}()

	out, _ := io.ReadAll(resp.Body) // returns when ctx cancels the stream
	body := string(out)
	if !strings.Contains(body, "event: update") {
		t.Fatalf("no update event in stream: %q", body)
	}
	if !strings.Contains(body, `"topic":"muehle/hf/pa/state"`) {
		t.Errorf("update event not lowercase-tagged JSON: %q", body)
	}
	if !strings.Contains(body, `"address":"muehle/hf/pa"`) || !strings.Contains(body, `"plane":"state"`) {
		t.Errorf("update event missing address/plane: %q", body)
	}
	if !strings.Contains(body, `"payload":{"keyed":"tx"}`) {
		t.Errorf("update payload not raw JSON object (base64?): %q", body)
	}
	if strings.Contains(body, `"Payload":"`) || strings.Contains(body, `"Topic":"`) {
		t.Errorf("update event still marshaling raw mqtt.Message: %q", body)
	}
}

// TestStreamClearedEvent asserts the wire shape of a clear: the update event carries
// "cleared":true and omits the payload (omitempty on an empty json.RawMessage), so the
// client clears the plane instead of storing an empty value.
func TestStreamClearedEvent(t *testing.T) {
	s, _ := newTestServer()
	s.tree.Update(mqtt.Message{Topic: "muehle/hf/radio/state", Payload: []byte(`{"freq_hz":14025000}`), Retained: true, TS: time.Now()})

	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	go func() {
		time.Sleep(150 * time.Millisecond)
		// Empty retained payload -> clear.
		s.tree.Update(mqtt.Message{Topic: "muehle/hf/radio/state", Payload: []byte{}, Retained: true, TS: time.Now()})
	}()

	out, _ := io.ReadAll(resp.Body)
	body := string(out)
	if !strings.Contains(body, `"cleared":true`) {
		t.Errorf("clear event missing cleared:true: %q", body)
	}
	// The clear update line must not carry a payload field (omitempty dropped it).
	if strings.Contains(body, `"plane":"state","payload":`) {
		t.Errorf("clear event should omit payload: %q", body)
	}
}