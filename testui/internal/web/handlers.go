package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"testui/internal/mqtt"
)

// maxBody limits POST bodies for /api/publish and /api/clear. /cmd and /state payloads
// are tiny; a cap defends against an unbounded body exhausting memory.
const maxBody = 1 << 20 // 1 MiB

// publishReq is the body for POST /api/publish.
//
// topic   — the MQTT topic to publish to (must be under the configured site prefix).
// payload — JSON value (object, array, string, number, bool) OR a raw string. When it is
//           a JSON object/array/number/bool it is published verbatim; when it is a string
//           it is published as a UTF-8 byte slice (so /status "online" works too).
// qos     — optional QoS (0 or 1); defaults to 1.
// retain  — optional. Retained is allowed for /state and /meta but REJECTED for /cmd
//           (integration model §8: intent is never retained).
type publishReq struct {
	Topic   string          `json:"topic"`
	Payload json.RawMessage `json:"payload"`
	QoS     *byte           `json:"qos,omitempty"`
	Retain  *bool           `json:"retain,omitempty"`
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	var req publishReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Topic == "" {
		http.Error(w, "topic required", http.StatusBadRequest)
		return
	}
	if err := s.validateTopic(req.Topic, req.retainOrFalse()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	qos := byte(1)
	if req.QoS != nil {
		qos = *req.QoS
		if qos > 1 {
			http.Error(w, "qos must be 0 or 1", http.StatusBadRequest)
			return
		}
	}
	retain := req.retainOrFalse()

	if err := s.mqtt.Publish(req.Topic, qos, retain, req.Payload); err != nil {
		writePublishErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "topic": req.Topic, "retained": retain})
}

// clearReq is the body for POST /api/clear: publish a zero-length retained payload to
// wipe a stale retained topic (e.g. a ghost /state or /meta).
type clearReq struct {
	Topic string `json:"topic"`
}

func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	var req clearReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Topic == "" {
		http.Error(w, "topic required", http.StatusBadRequest)
		return
	}
	// Clearing is allowed on any retained plane including /cmd (clearing a retained
	// /cmd is the §8 self-healing-actuator exception's one-shot-clear mechanism).
	if err := s.validateTopicPrefix(req.Topic); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.mqtt.Publish(req.Topic, 1, true, []byte{}); err != nil {
		writePublishErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "topic": req.Topic, "cleared": true})
}

// validateTopic enforces the two publish guards:
//  1. the topic must be under the configured site prefix (no publishing outside the
//     station tree from the test UI), and
//  2. /cmd may never be retained (integration model §8 — a retained command re-fires
//     on every reconnect with no operator behind it).
func (s *Server) validateTopic(topic string, retain bool) error {
	if err := s.validateTopicPrefix(topic); err != nil {
		return err
	}
	if retain && strings.HasSuffix(topic, "/cmd") {
		return errors.New("retained publish to /cmd is rejected (integration model §8: intent is never retained)")
	}
	return nil
}

func (s *Server) validateTopicPrefix(topic string) error {
	prefix := s.site + "/"
	if !strings.HasPrefix(topic, prefix) {
		return errors.New("topic must be under " + prefix + " (the configured site)")
	}
	return nil
}

// writePublishErr maps an mqtt.Publish error to the right HTTP status: ErrShuttingDown
// is a 503 (the message did not go out because the relay is closing), anything else is
// a 502 (broker/transport failure).
func writePublishErr(w http.ResponseWriter, err error) {
	if errors.Is(err, mqtt.ErrShuttingDown) {
		http.Error(w, "publish failed: relay shutting down", http.StatusServiceUnavailable)
		return
	}
	http.Error(w, "publish failed: "+err.Error(), http.StatusBadGateway)
}

func (req *publishReq) retainOrFalse() bool {
	if req.Retain != nil {
		return *req.Retain
	}
	return false
}