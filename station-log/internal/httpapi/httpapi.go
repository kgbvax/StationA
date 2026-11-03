// Package httpapi is station-log's HTTP interface: the ADIF upload
// (POST /import) and the read API a consumer loads its view from. It has no
// auth: it listens on loopback, or on the LAN behind allow_from.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"station-log/internal/importer"
	"station-log/internal/store"
)

// MaxUpload is the largest ADIF body POST /import accepts.
const MaxUpload = 96 << 20

// API serves the HTTP interface.
type API struct {
	Store     *store.Store
	Importer  *importer.Importer
	AllowFrom []netip.Prefix // empty = any client that reaches the listener
	Window    time.Duration
	Log       *slog.Logger
}

// Handler returns the routes.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET /v1/stats", a.stats)
	mux.HandleFunc("GET /v1/worked", a.worked)
	mux.HandleFunc("GET /v1/qsos", a.qsos)
	mux.HandleFunc("POST /import", a.importADIF)
	return a.guard(mux)
}

// guard rejects clients outside allow_from.
func (a *API) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(a.AllowFrom) > 0 {
			ap, err := netip.ParseAddrPort(r.RemoteAddr)
			ok := false
			if err == nil {
				for _, p := range a.AllowFrom {
					if p.Contains(ap.Addr().Unmap()) {
						ok = true
					}
				}
			}
			if !ok {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "client not in allow_from"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) stats(w http.ResponseWriter, _ *http.Request) {
	st, err := a.Store.Stats()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	out := struct {
		store.Stats
		DupWindow  string               `json:"dup_window"`
		LastImport *importer.LastImport `json:"last_import,omitempty"`
	}{Stats: st, DupWindow: a.Window.String()}
	if l, ok := a.Importer.Last(); ok {
		out.LastImport = &l
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) worked(w http.ResponseWriter, r *http.Request) {
	own := importer.NormalizeCall(r.URL.Query().Get("own"))
	var since time.Time
	if s := r.URL.Query().Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			fail(w, http.StatusBadRequest, errors.New("since: want RFC 3339"))
			return
		}
		since = t
	}
	rows, err := a.Store.WorkedView(own, since)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if rows == nil {
		rows = []store.Worked{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"own": own, "count": len(rows), "worked": rows})
}

func (a *API) qsos(w http.ResponseWriter, r *http.Request) {
	call := importer.NormalizeCall(r.URL.Query().Get("call"))
	if call == "" {
		fail(w, http.StatusBadRequest, errors.New("call is required"))
		return
	}
	rows, err := a.Store.ByCall(call, importer.NormalizeCall(r.URL.Query().Get("own")))
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if rows == nil {
		rows = []store.QSO{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"call": call, "count": len(rows), "qsos": rows})
}

func (a *API) importADIF(w http.ResponseWriter, r *http.Request) {
	// a browser page must not be able to write here
	if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
		fail(w, http.StatusForbidden, errors.New("browser requests are refused; upload with curl or a script"))
		return
	}
	q := r.URL.Query()
	opt := importer.Options{Source: strings.TrimSpace(q.Get("source"))}
	if rs := strings.TrimSpace(q.Get("replace_source")); rs != "" {
		if opt.Source != "" && opt.Source != rs {
			fail(w, http.StatusBadRequest, errors.New("source and replace_source must name the same source"))
			return
		}
		opt.Source, opt.ReplaceSource = rs, true
	}
	opt.Force = q.Get("force") == "1"
	if len(opt.Source) > 64 {
		fail(w, http.StatusBadRequest, errors.New("source is longer than 64 characters"))
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, MaxUpload+1))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if len(b) > MaxUpload {
		fail(w, http.StatusRequestEntityTooLarge, errors.New("the body is larger than 96 MiB"))
		return
	}
	rep, err := a.Importer.Import(r.Context(), string(b), opt)
	switch {
	case errors.Is(err, importer.ErrNoRecords), errors.Is(err, importer.ErrIncomplete), errors.Is(err, importer.ErrMassDelete):
		fail(w, http.StatusUnprocessableEntity, err)
	case errors.Is(err, context.Canceled):
		fail(w, http.StatusRequestTimeout, err)
	case err != nil:
		if a.Log != nil {
			a.Log.Error("import failed", "err", err)
		}
		fail(w, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusOK, rep)
	}
}

func fail(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
