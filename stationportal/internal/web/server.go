// Package web serves the landing page (server-rendered HTML, no JavaScript),
// the same data as JSON at /api/inventory, and the hf_console operator quick
// reference at /quickref (a copy of ../hf_console/docs/quickref/
// hf-console-quickref.html that its build.sh refreshes).
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"stationportal/internal/bus"
	"stationportal/internal/inventory"
	"stationportal/internal/probe"
)

//go:embed assets/page.html assets/quickref.html
var assets embed.FS

// Server renders the landing page.
type Server struct {
	Inv      inventory.Inventory
	Store    *bus.Store
	Probes   *probe.Prober
	Broker   string
	Host     string
	Revision string
	RefreshS int
	Log      *slog.Logger
	Now      func() time.Time

	tmpl *template.Template
}

// Routes returns the HTTP handler.
func (s *Server) Routes() http.Handler {
	s.tmpl = template.Must(template.New("page.html").Funcs(template.FuncMap{
		"short": shortRev,
	}).ParseFS(assets, "assets/page.html"))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handlePage)
	mux.HandleFunc("GET /api/inventory", s.handleJSON)
	mux.HandleFunc("GET /quickref", handleQuickref)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

func (s *Server) page() Page {
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	p := buildPage(s.Inv, s.Store.Snapshot(), s.Probes)
	p.Generated, p.Host, p.Revision, p.RefreshS, p.Broker = now, s.Host, s.Revision, s.RefreshS, s.Broker
	connected, since, last := s.Store.Link()
	p.BusConnected = connected
	if !since.IsZero() {
		p.BusSince = Age(now.Sub(since))
	}
	if !last.IsZero() {
		p.LastMessage = Age(now.Sub(last))
	}
	return p
}

func (s *Server) handlePage(w http.ResponseWriter, _ *http.Request) {
	var buf bytes.Buffer
	if err := s.tmpl.Execute(&buf, s.page()); err != nil {
		s.Log.Error("render page", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = buf.WriteTo(w)
}

func (s *Server) handleJSON(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(s.page())
}

// handleQuickref serves the embedded quick reference. no-cache: a redeploy
// with rebuilt sheets must not hide behind a stale browser copy.
func handleQuickref(w http.ResponseWriter, _ *http.Request) {
	b, err := assets.ReadFile("assets/quickref.html")
	if err != nil {
		http.Error(w, "quick reference missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

// shortRev abbreviates a revision to 7 hex digits, keeping a "+dirty" mark.
func shortRev(r string) string {
	rev, dirty, _ := strings.Cut(r, "+")
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if dirty != "" {
		return rev + "+" + dirty
	}
	return rev
}
