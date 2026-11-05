package app

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// handler is the local HTTP API: contest setup for bmrec's page (which proxies
// it) and a health check. It listens on loopback and has no auth; the
// operator's callsign list and contest files never leave this host through it.
func (s *Service) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) })
	// /debug/rows[?call=XX]: why each candidate row (bmrec rows and cluster spots) is, or is not,
	// on the ranked list. Loopback only, read only; for "why is my qualified station missing".
	mux.HandleFunc("GET /debug/rows", func(w http.ResponseWriter, r *http.Request) {
		t := s.eng.Trace()
		if c := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("call"))); c != "" {
			rows := t.Rows[:0:0]
			for _, row := range t.Rows {
				if strings.Contains(strings.ToUpper(row.Call), c) {
					rows = append(rows, row)
				}
			}
			t.Rows = rows
		}
		writeJSON(w, http.StatusOK, t)
	})
	mux.HandleFunc("GET /contest", func(w http.ResponseWriter, _ *http.Request) {
		ov, err := s.Overview()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, ov)
	})
	mux.HandleFunc("POST /contest/files", func(w http.ResponseWriter, r *http.Request) {
		// a browser page must not be able to write here: bmrec's server proxies this
		if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "JSON from the bmrec server only"})
			return
		}
		select {
		case s.uploadSem <- struct{}{}:
			defer func() { <-s.uploadSem }()
		default:
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "another upload is in progress"})
			return
		}
		b, err := io.ReadAll(io.LimitReader(r.Body, contestUploadMax+64<<10+1))
		if err != nil || len(b) > contestUploadMax+64<<10 {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "the file is larger than 8 MiB"})
			return
		}
		var body struct{ Kind, Name, Text string }
		if err := json.Unmarshal(b, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		f, err := s.Upload(strings.ToLower(body.Kind), body.Name, body.Text)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, f)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
