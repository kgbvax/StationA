package web

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
)

//go:embed static/*
var staticFS embed.FS

// Publisher is the subset of mqtt.Client the HTTP layer needs. An interface keeps the
// handlers testable without a live broker.
type Publisher interface {
	Publish(topic string, qos byte, retained bool, payload []byte) error
}

// Server is the HTTP layer: serves the static UI and the JSON/SSE API.
type Server struct {
	tree *Tree
	mqtt Publisher
	site string // publish guard prefix (e.g. "muehle")
}

// New creates the server. site is the guard prefix: browser publish requests may only
// target topics under <site>/.
func New(tree *Tree, mqttClient Publisher, site string) *Server {
	return &Server{tree: tree, mqtt: mqttClient, site: site}
}

// Routes returns the HTTP handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Static UI (embedded). Serve files from static/ at their root paths.
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("embed static: %v", err)
	}
	mux.Handle("GET /", http.FileServer(http.FS(sub)))

	mux.HandleFunc("GET /api/tree", s.handleTree)
	mux.HandleFunc("GET /api/stream", s.handleStream)
	mux.HandleFunc("POST /api/publish", s.handlePublish)
	mux.HandleFunc("POST /api/clear", s.handleClear)

	return mux
}

// handleTree returns a one-shot snapshot of the whole slot tree.
func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	slots, order := s.tree.Snapshot()
	out := map[string]any{
		"slots": slots,
		"order": order,
	}
	writeJSON(w, out)
}

// handleStream is the Server-Sent Events endpoint: sends a snapshot event, then an
// update event per inbound MQTT message. Mirror ultrabridge/internal/web SSE pattern.
//
// Subscribe is registered BEFORE the snapshot is read so no Update+broadcast that fires
// in between is missed: any change that lands after subscribe but before the snapshot
// read is already in the snapshot (newer-or-equal), and any change after the snapshot
// read arrives as a live update after the snapshot event. The two locks (mu and subsMu)
// are separate, so the ordering — not a shared lock — is what makes this race-free.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, cancel := s.tree.Subscribe()
	defer cancel()

	slots, order := s.tree.Snapshot()
	snap, _ := json.Marshal(map[string]any{"slots": slots, "order": order})
	writeSSE(w, flusher, "snapshot", snap)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			// ev is *Event, a tagged struct with a json.RawMessage payload — NOT
			// mqtt.Message, which has no JSON tags and would serialize to capitalized
			// keys + base64 and break the browser's live update path.
			data, _ := json.Marshal(ev)
			if err := writeSSE(w, flusher, "update", data); err != nil {
				return
			}
		}
	}
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, event string, data []byte) error {
	if _, err := w.Write([]byte("event: " + event + "\n")); err != nil {
		return err
	}
	if _, err := w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	if _, err := w.Write([]byte("\n\n")); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}