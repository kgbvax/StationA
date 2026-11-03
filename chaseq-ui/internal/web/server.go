// Package web serves the loopback pages the window shows: the ranked list,
// its filters and the result of the last action, kept live over SSE.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"chaseq-ui/internal/action"
	"chaseq-ui/internal/bus"
	"chaseq-ui/internal/loggers"
	"chaseq-ui/internal/model"
)

//go:embed pages
var pagesFS embed.FS

//go:embed static
var staticFS embed.FS

// Bands are the filter buttons, low to high.
var Bands = []string{"160m", "80m", "60m", "40m", "30m", "20m", "17m", "15m", "12m", "10m", "6m", "2m", "70cm"}

// Server is the HTTP side.
type Server struct {
	Model       *model.Model
	Runner      *action.Runner
	FiltersPath string
	Quit        func()

	tpl *template.Template

	mu   sync.Mutex
	last *action.Report
	subs map[chan struct{}]struct{}
}

// New builds a Server.
func New(m *model.Model, r *action.Runner, filtersPath string, quit func()) (*Server, error) {
	t, err := template.New("").Funcs(template.FuncMap{
		"khz": func(hz int64) string { return fmt.Sprintf("%.1f", float64(hz)/1e3) },
	}).ParseFS(pagesFS, "pages/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{Model: m, Runner: r, FiltersPath: filtersPath, Quit: quit, tpl: t, subs: map[chan struct{}]struct{}{}}, nil
}

// Notify tells every open page the list changed. Safe from any goroutine.
func (s *Server) Notify() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.subs {
		select {
		case c <- struct{}{}:
		default: // a notification is already pending
		}
	}
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /list", s.list)
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /move", s.write(s.move))
	mux.HandleFunc("POST /act", s.write(s.act))
	mux.HandleFunc("POST /skip", s.write(s.skip))
	mux.HandleFunc("POST /undo", s.write(s.undo))
	mux.HandleFunc("POST /filters", s.write(s.filters))
	mux.HandleFunc("POST /api/quit", s.write(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		if s.Quit != nil {
			go s.Quit()
		}
	}))
	return s.hostGuard(mux)
}

// hostGuard refuses any request whose Host is not a loopback name: a page on
// another site that rebinds its DNS name to 127.0.0.1 would otherwise be
// same-origin with this server and could press Enter for the operator.
func (s *Server) hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "host not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		h = hostport
	}
	switch h {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	return false
}

// write guards a state-changing route: same-origin only, and a custom header
// a cross-site form cannot set.
func (s *Server) write(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" {
			u, err := url.Parse(o)
			if err != nil || u.Host != r.Host || !loopbackHost(u.Host) {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		if r.Header.Get("X-Chaseq") == "" {
			http.Error(w, "missing X-Chaseq header", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

type pageData struct {
	View   model.View
	Bands  []string
	Last   *action.Report
	Active map[string]bool
	// ActiveAwards marks the award codes in the filter.
	ActiveAwards map[string]bool
}

func (s *Server) data() pageData {
	v := s.Model.Snapshot()
	d := pageData{View: v, Bands: Bands, Active: map[string]bool{}, ActiveAwards: map[string]bool{}}
	for _, b := range v.Filters.Bands {
		d.Active[b] = true
	}
	for _, a := range v.Filters.Awards {
		d.ActiveAwards[a] = true
	}
	s.mu.Lock()
	d.Last = s.last
	s.mu.Unlock()
	return d
}

func (s *Server) page(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, "index.html", s.data()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) list(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.tpl.ExecuteTemplate(w, "list.html", s.data()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.subs, ch); s.mu.Unlock() }()
	fmt.Fprint(w, "event: hello\ndata: {}\n\n")
	fl.Flush()
	// The model's own staleness timer has no event, so tick the page too.
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			fmt.Fprint(w, "event: list_changed\ndata: {}\n\n")
		case <-tick.C:
			fmt.Fprint(w, "event: list_changed\ndata: {}\n\n")
		}
		fl.Flush()
	}
}

func rowParam(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.URL.Query().Get("row"), 10, 64)
	return id, err == nil
}

func (s *Server) move(w http.ResponseWriter, r *http.Request) {
	d, err := strconv.Atoi(r.URL.Query().Get("d"))
	if err != nil || d < -50 || d > 50 {
		http.Error(w, "bad d", http.StatusBadRequest)
		return
	}
	s.Model.Move(d)
	s.Notify()
	w.WriteHeader(http.StatusNoContent)
}

// run records and returns a report. A refused action (offline) is a 409.
func (s *Server) run(w http.ResponseWriter, rep action.Report) {
	s.mu.Lock()
	s.last = &rep
	s.mu.Unlock()
	s.Notify()
	w.Header().Set("Content-Type", "application/json")
	if len(rep.Parts) == 1 && rep.Parts[0].Name == "List" && rep.Parts[0].Status == loggers.Failed && !s.Model.Snapshot().ActionsEnabled {
		w.WriteHeader(http.StatusConflict)
	}
	_ = json.NewEncoder(w).Encode(rep)
}

func (s *Server) act(w http.ResponseWriter, r *http.Request) {
	id, ok := rowParam(r)
	if !ok {
		// Enter acts on the highlighted row.
		row, found := s.Model.Highlighted()
		if !found {
			http.Error(w, "nothing highlighted", http.StatusBadRequest)
			return
		}
		id = row.RowID
	} else {
		s.Model.Select(id)
	}
	s.run(w, s.Runner.Act(r.Context(), id))
}

func (s *Server) skip(w http.ResponseWriter, r *http.Request) {
	id, ok := rowParam(r)
	if !ok {
		row, found := s.Model.Highlighted()
		if !found {
			http.Error(w, "nothing highlighted", http.StatusBadRequest)
			return
		}
		id = row.RowID
	}
	s.run(w, s.Runner.Skip(r.Context(), id))
}

func (s *Server) undo(w http.ResponseWriter, r *http.Request) {
	s.run(w, s.Runner.Undo(r.Context()))
}

func (s *Server) filters(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	band := q.Get("band")
	if band != "" {
		ok := false
		for _, x := range Bands {
			ok = ok || x == band
		}
		if !ok {
			http.Error(w, "bad band", http.StatusBadRequest)
			return
		}
	}
	award, source := q.Get("award"), q.Get("source")
	if len(award) > 32 || strings.ContainsAny(award, "<>\"'&") {
		http.Error(w, "bad award", http.StatusBadRequest)
		return
	}
	if source != "" && source != bus.SourceCluster && source != bus.SourceBmrec {
		http.Error(w, "bad source", http.StatusBadRequest)
		return
	}
	f := s.Model.UpdateFilters(func(f model.Filters) model.Filters {
		if band != "" {
			f = f.ToggleBand(band)
		}
		if award != "" {
			f = f.ToggleAward(award)
		}
		if source != "" {
			f = f.ToggleSource(source)
		}
		if n := q.Get("needed"); n != "" {
			f.NeededOnly = n == "1"
		}
		if q.Get("clear") == "1" {
			f = model.Filters{}
		}
		return f
	})
	_ = model.SaveFilters(s.FiltersPath, f)
	s.Notify()
	w.WriteHeader(http.StatusNoContent)
}

// Serve serves on ln until ctx is done.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second,
		// Open SSE streams end with ctx instead of holding Shutdown up.
		BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		<-ctx.Done()
		sh, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		_ = srv.Shutdown(sh)
	}()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
