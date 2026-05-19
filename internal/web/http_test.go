package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ubctrl/internal/ub/service"
	"ubctrl/internal/ub/transport"
)

func TestRoutesIntegration(t *testing.T) {
	ctrl := service.NewController(transport.NewMock())
	srv := New(ctrl)
	handler := srv.Routes()

	// Test GET /
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Test GET /api/status
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/status: status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Test POST /api/refresh
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/refresh", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/refresh: status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Test POST /api/retract
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/retract", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/retract: status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Test POST /api/frequency (invalid)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/frequency", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/frequency (no form): status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleEventsSSE(t *testing.T) {
	ctrl := service.NewController(transport.NewMock())
	srv := New(ctrl)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)

	done := make(chan struct{})
	go func() {
		srv.handleEvents(rec, req)
		close(done)
	}()

	// Simulate sending a status update
	srv.PublishStatus(ctrl.State())

	// We can't forcibly close the context in httptest, so just let the handler exit naturally.
	// Wait briefly for goroutine to finish.
	<-done
	output := rec.Body.String()
	if !strings.Contains(output, "event: status") {
		t.Fatalf("SSE output missing event: %s", output)
	}
}

func TestHandleEventsStreamingUnsupported(t *testing.T) {
	ctrl := service.NewController(transport.NewMock())
	srv := New(ctrl)

	// Use a ResponseWriter that doesn't implement http.Flusher
	type dummyWriter struct{ httptest.ResponseRecorder }

	rec := &dummyWriter{}
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	srv.handleEvents(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for streaming unsupported, got %d", rec.Code)
	}
}

func TestHandleRefreshMethodNotAllowed(t *testing.T) {
	ctrl := service.NewController(transport.NewMock())
	srv := New(ctrl)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/refresh", nil)
	srv.handleRefresh(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET, got %d", rec.Code)
	}
}

func TestHandleRetractMethodNotAllowed(t *testing.T) {
	ctrl := service.NewController(transport.NewMock())
	srv := New(ctrl)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/retract", nil)
	srv.handleRetract(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET, got %d", rec.Code)
	}
}

func TestHandleFrequencyInvalidValue(t *testing.T) {
	ctrl := service.NewController(transport.NewMock())
	srv := New(ctrl)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/frequency", strings.NewReader("frequency=notanumber&mode=normal"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.handleFrequency(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid frequency, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/frequency", strings.NewReader("frequency=70000&mode=normal"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.handleFrequency(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for out-of-range frequency, got %d", rec.Code)
	}
}

func TestHandleRetract(t *testing.T) {
	ctrl := service.NewController(transport.NewMock())
	srv := New(ctrl)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/retract", nil)
	srv.handleRetract(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("expected ok in response: %s", rec.Body.String())
	}
}

func TestHandleEventsMethodNotAllowed(t *testing.T) {
	ctrl := service.NewController(transport.NewMock())
	srv := New(ctrl)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/events", nil)
	srv.handleEvents(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusMethodNotAllowed, rec.Body.String())
	}
}

func TestHandleFrequencyModeSwitches(t *testing.T) {
	ctrl := service.NewController(transport.NewMock())
	srv := New(ctrl)

	tests := []struct {
		name     string
		freq     string
		mode     string
		wantMode string
	}{
		{name: "normal mode", freq: "14000", mode: "normal", wantMode: "normal"},
		{name: "180 mode", freq: "14000", mode: "180", wantMode: "180"},
		{name: "bidir mode", freq: "14000", mode: "bidir", wantMode: "bidir"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/frequency", strings.NewReader("frequency="+tt.freq+"&mode="+tt.mode))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

			srv.handleFrequency(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
			}

			state := ctrl.State()
			if got := state.ModeName; got != tt.wantMode {
				t.Fatalf("mode = %q, want %q", got, tt.wantMode)
			}
			if got := state.FrequencyKHz; got != 14000 {
				t.Fatalf("frequency = %d, want %d", got, 14000)
			}
		})
	}
}

func TestHandleFrequencyChangesFrequency(t *testing.T) {
	ctrl := service.NewController(transport.NewMock())
	srv := New(ctrl)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/frequency", strings.NewReader("frequency=14550&mode=normal"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	srv.handleFrequency(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	state := ctrl.State()
	if got := state.FrequencyKHz; got != 14550 {
		t.Fatalf("frequency = %d, want %d", got, 14550)
	}
	if got := state.ModeName; got != "normal" {
		t.Fatalf("mode = %q, want %q", got, "normal")
	}
}

func TestIndexRendersModeButtonsAndNoRefreshButton(t *testing.T) {
	ctrl := service.NewController(transport.NewMock())
	srv := New(ctrl)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	srv.handleIndex(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `onclick="setMode('normal')"`) || !strings.Contains(body, `onclick="setMode('180')"`) || !strings.Contains(body, `onclick="setMode('bidir')"`) {
		t.Fatalf("mode buttons not rendered as expected: %s", body)
	}
	if strings.Contains(body, "Refresh status") {
		t.Fatalf("refresh button should not be rendered")
	}
	if !strings.Contains(body, `id="moving-card"`) || !strings.Contains(body, `id="offline-card"`) {
		t.Fatalf("expected moving/offline cards to be rendered")
	}
	if !strings.Contains(body, `class="kv hidden" id="moving-card"`) {
		t.Fatalf("expected moving card to be hidden when not moving")
	}
	if !strings.Contains(body, `class="kv hidden" id="offline-card"`) {
		t.Fatalf("expected offline card to be hidden when online")
	}
	if strings.Contains(body, "Live updates are pushed with Server-Sent Events and the backend still polls the controller in the background.") {
		t.Fatalf("old footer text should not be present")
	}
	// Verify new layout structure
	if !strings.Contains(body, `id="freq-btn"`) {
		t.Fatalf("expected frequency button with id 'freq-btn'")
	}
	if !strings.Contains(body, `id="mode-normal"`) || !strings.Contains(body, `id="mode-180"`) || !strings.Contains(body, `id="mode-bidir"`) {
		t.Fatalf("expected mode buttons with individual IDs")
	}
}
