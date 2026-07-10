package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ultrabridge/internal/ub/protocol"
	"ultrabridge/internal/ub/transport"
)

func TestDebugHubToggle(t *testing.T) {
	h := NewDebugHub()
	if h.Enabled() {
		t.Fatal("new hub should start disabled")
	}
	h.SetEnabled(true)
	if !h.Enabled() {
		t.Fatal("hub should be enabled after SetEnabled(true)")
	}
	h.SetEnabled(false)
	if h.Enabled() {
		t.Fatal("hub should be disabled after SetEnabled(false)")
	}
}

func TestDebugHubPublishBroadcasts(t *testing.T) {
	h := NewDebugHub()
	ch := h.subscribe()
	defer h.unsubscribe(ch)

	h.Publish(transport.Trace{
		At:   time.Date(2026, 6, 30, 11, 0, 0, 0, time.UTC),
		Dir:  "tx",
		Com:  protocol.CmdChangeFrequency,
		Data: []byte{0x55, 0x53, 0x02},
	})

	select {
	case payload := <-ch:
		var v traceView
		if err := json.Unmarshal(payload, &v); err != nil {
			t.Fatalf("bad payload: %v", err)
		}
		if v.Dir != "tx" || v.Name != "change_frequency" || v.Data != "55 53 02" {
			t.Fatalf("unexpected trace view: %+v", v)
		}
	case <-time.After(time.Second):
		t.Fatal("no trace broadcast received")
	}
}

func TestFormatTraceUsesReplyNameForRx(t *testing.T) {
	v := formatTrace(transport.Trace{Dir: "rx", Com: protocol.ReplyBadParams, Err: "boom"})
	if v.Name != "bad_params" {
		t.Errorf("rx name = %q, want bad_params", v.Name)
	}
	if v.Err != "boom" {
		t.Errorf("err = %q, want boom", v.Err)
	}
}

func TestHandleDebugGetAndPost(t *testing.T) {
	srv := NewWithHub(nil, NewDebugHub())

	// GET reports disabled initially.
	rec := httptest.NewRecorder()
	srv.handleDebug(rec, httptest.NewRequest(http.MethodGet, "/api/debug", nil))
	if !strings.Contains(rec.Body.String(), `"enabled": false`) {
		t.Fatalf("GET body = %q, want enabled false", rec.Body.String())
	}

	// POST enabled=1 turns it on.
	req := httptest.NewRequest(http.MethodPost, "/api/debug", strings.NewReader("enabled=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	srv.handleDebug(rec, req)
	if !srv.debug.Enabled() {
		t.Fatal("POST enabled=1 did not enable debug")
	}
	if !strings.Contains(rec.Body.String(), `"enabled": true`) {
		t.Fatalf("POST body = %q, want enabled true", rec.Body.String())
	}

	// POST enabled=0 turns it off.
	req = httptest.NewRequest(http.MethodPost, "/api/debug", strings.NewReader("enabled=0"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	srv.handleDebug(rec, req)
	if srv.debug.Enabled() {
		t.Fatal("POST enabled=0 did not disable debug")
	}
}

func TestHexBytes(t *testing.T) {
	if got := hexBytes(nil); got != "" {
		t.Errorf("hexBytes(nil) = %q, want empty", got)
	}
	if got := hexBytes([]byte{0x00, 0x0F, 0xA0}); got != "00 0F A0" {
		t.Errorf("hexBytes = %q, want '00 0F A0'", got)
	}
}
