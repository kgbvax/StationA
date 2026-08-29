package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"testui/internal/mqtt"
)

func TestValidateTopicGuards(t *testing.T) {
	s := &Server{site: "muehle"}

	if err := s.validateTopic("homeassistant/foo/config", false); err == nil {
		t.Error("expected rejection of out-of-site topic")
	}
	if err := s.validateTopic("muehle/hf/radio/cmd", true); err == nil ||
		!strings.Contains(err.Error(), "never retained") {
		t.Errorf("expected /cmd retain rejection, got %v", err)
	}
	if err := s.validateTopic("muehle/hf/radio/cmd", false); err != nil {
		t.Errorf("non-retained /cmd should be allowed: %v", err)
	}
	if err := s.validateTopic("muehle/hf/radio/state", true); err != nil {
		t.Errorf("retained /state should be allowed: %v", err)
	}
	if err := s.validateTopic("muehle/hf/radio/meta", true); err != nil {
		t.Errorf("retained /meta should be allowed: %v", err)
	}
}

type stubMQTT struct {
	lastTopic    string
	lastRetained bool
	lastQoS      byte
	lastPayload  []byte
	calls        int
	err          error // if set, Publish returns this error instead of nil
}

func (s *stubMQTT) Publish(topic string, qos byte, retained bool, payload []byte) error {
	s.lastTopic, s.lastRetained, s.lastQoS, s.lastPayload = topic, retained, qos, payload
	s.calls++
	if s.err != nil {
		return s.err
	}
	return nil
}

func newTestServer() (*Server, *stubMQTT) {
	m := &stubMQTT{}
	return &Server{tree: NewTree(), mqtt: m, site: "muehle"}, m
}

func doJSON(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

func TestPublishStateRetainedAllowed(t *testing.T) {
	s, m := newTestServer()
	rec := doJSON(t, s, "/api/publish", `{"topic":"muehle/hf/radio/state","payload":{"freq_hz":14025000},"retain":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if m.lastTopic != "muehle/hf/radio/state" || !m.lastRetained {
		t.Errorf("publish call = %q retained=%v", m.lastTopic, m.lastRetained)
	}
	if string(m.lastPayload) != `{"freq_hz":14025000}` {
		t.Errorf("payload=%q want {\"freq_hz\":14025000}", m.lastPayload)
	}
}

func TestPublishCmdRetainedRejected(t *testing.T) {
	s, m := newTestServer()
	rec := doJSON(t, s, "/api/publish", `{"topic":"muehle/hf/radio/cmd","payload":{"set_freq_hz":14074000},"retain":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if m.calls != 0 {
		t.Errorf("no publish should have happened, calls=%d", m.calls)
	}
}

func TestPublishOutOfSiteRejected(t *testing.T) {
	s, m := newTestServer()
	rec := doJSON(t, s, "/api/publish", `{"topic":"other/hf/radio/cmd","payload":{"x":1}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if m.calls != 0 {
		t.Errorf("no publish should have happened, calls=%d", m.calls)
	}
}

func TestPublishStatusStringPayload(t *testing.T) {
	s, m := newTestServer()
	// A bare string payload (JSON string) is published verbatim as UTF-8 bytes —
	// needed for the "online"/"offline" status convention.
	rec := doJSON(t, s, "/api/publish", `{"topic":"muehle/hf/radio/status","payload":"online","retain":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if string(m.lastPayload) != `"online"` {
		t.Errorf("payload=%q want \"online\"", m.lastPayload)
	}
}

func TestClearSendsEmptyRetained(t *testing.T) {
	s, m := newTestServer()
	rec := doJSON(t, s, "/api/clear", `{"topic":"muehle/hf/radio/state"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if m.lastTopic != "muehle/hf/radio/state" || !m.lastRetained || len(m.lastPayload) != 0 {
		t.Errorf("clear call = %q retained=%v payload=%q (want empty retained)", m.lastTopic, m.lastRetained, m.lastPayload)
	}
}

func TestClearOutOfSiteRejected(t *testing.T) {
	s, m := newTestServer()
	rec := doJSON(t, s, "/api/clear", `{"topic":"homeassistant/x/config"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if m.calls != 0 {
		t.Errorf("no publish should have happened, calls=%d", m.calls)
	}
}

// TestPublishErrShuttingDownMaps503: a Publish that returns mqtt.ErrShuttingDown must
// surface as 503, not a false 200 and not a 502.
func TestPublishErrShuttingDownMaps503(t *testing.T) {
	s, m := newTestServer()
	m.err = mqtt.ErrShuttingDown
	rec := doJSON(t, s, "/api/publish", `{"topic":"muehle/hf/radio/state","payload":{"x":1}}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for ErrShuttingDown, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestPublishBrokerErrorMaps502: a non-sentinel Publish error surfaces as 502.
func TestPublishBrokerErrorMaps502(t *testing.T) {
	s, m := newTestServer()
	m.err = errors.New("broker unreachable")
	rec := doJSON(t, s, "/api/publish", `{"topic":"muehle/hf/radio/state","payload":{"x":1}}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for broker error, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestPublishBodyTooLarge: a body over the MaxBytesReader cap is rejected with 400
// before reaching the broker.
func TestPublishBodyTooLarge(t *testing.T) {
	s, m := newTestServer()
	// Build a JSON object with a value well over the 1 MiB cap.
	big := strings.Repeat("a", 1<<20)
	body := `{"topic":"muehle/hf/radio/state","payload":"` + big + `"}`
	rec := doJSON(t, s, "/api/publish", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized body, got %d body=%s", rec.Code, rec.Body.String())
	}
	if m.calls != 0 {
		t.Errorf("no publish should have happened, calls=%d", m.calls)
	}
}