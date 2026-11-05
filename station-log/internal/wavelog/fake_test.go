package wavelog

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"station-log/internal/adif"
)

// fakeWavelog is an httptest stand-in for Wavelog API v2, built from the
// documented shapes only:
//
//	API v2 overview: "Authentication" (Authorization: Bearer wl2_...),
//	  "Scopes" (GET needs <res>:read, POST needs <res>:write), "Success
//	  responses" ({"data","meta"}), "Error responses" ({"error":{"code",
//	  "message","details"}}), "Pagination" (meta.has_more), "Rate limiting"
//	  (429 rate_limited, details.retry_after);
//	QSO resource: "Export QSOs as ADIF" (GET /api/v2/qso?format=adif&since_id
//	  &station_id&per_page: rows ascending by id, data.exported/lastfetchedid/
//	  adif, adif null when nothing new), "Import multiple QSOs (ADIF)" (POST
//	  import_type adif: 201 with data.parsed/imported/skipped/messages;
//	  nothing importable -> 400 validation_error).
//
// The unauthenticated probe of the real instance showed: GET status ->
// {"data":{"name":"Wavelog API","status":"ok"},...}; a bad token -> HTTP 401
// {"error":{"code":"invalid_token","message":"Invalid or revoked API token"}}.
type fakeWavelog struct {
	t   *testing.T
	srv *httptest.Server
	key string

	mu      sync.Mutex
	scopes  []string  // scopes the token carries
	rows    []fakeRow // id = index+1
	fetches []int64   // since_id of every ADIF export call
	posts   []string  // ADIF string of every POST import
	reqs    []string  // method + full request URI of every request
	auths   []string  // Authorization header of every request
	bodies  []string  // raw request bodies (to check the token never leaks)
	hits    int       // requests that reached the server

	status int    // non-zero: answer every request with this status
	retry  string // Retry-After header with the forced status
	body   string // with status: the response body instead of "forced failure"
	okBody string // non-empty: answer every request 200 with exactly this body

	dup         bool            // every record is skipped as a duplicate
	rejectAll   bool            // POST answers 400 validation_error
	rejectMulti bool            // POST of more than one record answers 400 validation_error
	badCalls    map[string]bool // POST containing such a CALL answers 400 validation_error, stores nothing
	partialBad  map[string]bool // such CALLs are not stored, the rest is (201, imported < parsed)
	failPage    int             // ADIF export answers 500 from this call number on (1-based, 0 = never)
	failStation string          // ADIF export answers 500 for this station id

	forbidStation string // export and import for this station id answer 403 forbidCode
	forbidCode    string // "forbidden" (default), insufficient_club_permission, club_access_revoked
}

type fakeRow struct {
	profile string
	rec     adif.Record
}

func newFake(t *testing.T) *fakeWavelog {
	f := &fakeWavelog{t: t, key: "wl2_test-token-not-real",
		scopes: []string{"qso:read", "qso:write", "station:read"}}
	mux := http.NewServeMux()
	mux.HandleFunc("/index.php/api/v2/qso", f.qso)
	mux.HandleFunc("/index.php/api/v2/token", f.token)
	mux.HandleFunc("/index.php/api/v2/station", f.station)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeWavelog) addRow(profile string, rec adif.Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, fakeRow{profile, rec})
}

func envelope(resource, method string, data any, meta map[string]any) map[string]any {
	m := map[string]any{"timestamp": "2026-11-01T12:00:00+00:00", "resource": resource, "method": method}
	for k, v := range meta {
		m[k] = v
	}
	return map[string]any{"data": data, "meta": m}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func apiErr(w http.ResponseWriter, status int, code, msg string, details map[string]any) {
	e := map[string]any{"code": code, "message": msg}
	if details != nil {
		e["details"] = details
	}
	writeJSON(w, status, map[string]any{"error": e})
}

// gate counts the request, records it and applies the forced answers and the
// auth/scope checks. It returns false when the request has been answered.
func (f *fakeWavelog) gate(w http.ResponseWriter, r *http.Request, scope string) bool {
	f.hits++
	f.reqs = append(f.reqs, r.Method+" "+r.URL.RequestURI())
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	b, _ := io.ReadAll(r.Body)
	f.bodies = append(f.bodies, string(b))
	r.Body = io.NopCloser(strings.NewReader(string(b)))
	if f.okBody != "" {
		w.Write([]byte(f.okBody))
		return false
	}
	if f.status != 0 {
		if f.retry != "" {
			w.Header().Set("Retry-After", f.retry)
		}
		msg := "forced failure"
		if f.body != "" {
			msg = f.body
		}
		if strings.HasPrefix(msg, "{") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.status)
			w.Write([]byte(msg))
		} else {
			http.Error(w, msg, f.status)
		}
		return false
	}
	if r.Header.Get("Authorization") != "Bearer "+f.key {
		apiErr(w, 401, "invalid_token", "Invalid or revoked API token", nil)
		return false
	}
	if scope != "" {
		for _, s := range f.scopes {
			if s == scope {
				return true
			}
		}
		apiErr(w, 403, "insufficient_scope", "Token is missing the required scope: "+scope,
			map[string]any{"required_scope": scope})
		return false
	}
	return true
}

func (f *fakeWavelog) token(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.gate(w, r, "") { // Token needs no scope (overview, "Scopes")
		return
	}
	writeJSON(w, 200, envelope("token", "GET", map[string]any{"name": "station-log", "scopes": f.scopes, "expires_at": nil}, nil))
}

func (f *fakeWavelog) station(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.gate(w, r, "station:read") {
		return
	}
	writeJSON(w, 200, envelope("station", "GET", []map[string]any{
		{"station_id": 1, "station_callsign": "DL9ET", "station_profile_name": "Home"},
		{"station_id": 7, "station_callsign": "DL9ET/P", "station_profile_name": "Portable"},
	}, nil))
}

func (f *fakeWavelog) qso(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		f.export(w, r)
	case http.MethodPost:
		f.importADIF(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, PATCH, DELETE")
		apiErr(w, 405, "method_not_allowed", "no", nil)
	}
}

func (f *fakeWavelog) export(w http.ResponseWriter, r *http.Request) {
	if !f.gate(w, r, "qso:read") {
		return
	}
	q := r.URL.Query()
	if q.Get("format") != "adif" {
		apiErr(w, 400, "validation_error", "this fake only does format=adif", nil)
		return
	}
	since, _ := strconv.ParseInt(q.Get("since_id"), 10, 64)
	per, _ := strconv.Atoi(q.Get("per_page"))
	station := q.Get("station_id")
	if f.failStation != "" && station == f.failStation {
		apiErr(w, 500, "internal_error", "boom", nil)
		return
	}
	if f.forbid(w, station) {
		return
	}
	f.fetches = append(f.fetches, since)
	if f.failPage > 0 && len(f.fetches) >= f.failPage {
		apiErr(w, 500, "internal_error", "boom", nil)
		return
	}
	var sb strings.Builder
	sb.WriteString("Wavelog ADIF export\n<ADIF_VER:5>3.1.7\n<EOH>\n")
	var last int64
	n, remaining := 0, 0
	for i, row := range f.rows { // ascending by id
		id := int64(i + 1)
		if id <= since || row.profile != station {
			continue
		}
		if n >= per {
			remaining++
			continue
		}
		sb.WriteString(adif.Format(row.rec))
		last, n = id, n+1
	}
	data := map[string]any{"exported": n, "lastfetchedid": last, "adif": sb.String()}
	if n == 0 { // "When there is nothing new, exported is 0 and adif is null"
		data["adif"] = nil
	}
	meta := map[string]any{"page": 1, "per_page": per, "count": n, "total": n + remaining, "has_more": remaining > 0}
	writeJSON(w, 200, envelope("qso", "GET", data, meta))
}

func (f *fakeWavelog) importADIF(w http.ResponseWriter, r *http.Request) {
	if !f.gate(w, r, "qso:write") {
		return
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(f.bodies[len(f.bodies)-1]), &m); err != nil {
		apiErr(w, 400, "invalid_json", "bad json", nil)
		return
	}
	pid, isNum := m["station_profile_id"].(float64)
	text, _ := m["adif"].(string)
	if m["import_type"] != "adif" || !isNum || text == "" {
		apiErr(w, 400, "validation_error", "import_type adif needs station_profile_id (integer) and adif", nil)
		return
	}
	profile := strconv.Itoa(int(pid))
	if f.forbid(w, profile) {
		return
	}
	f.posts = append(f.posts, text)
	recs := adif.Parse(text)
	res := func(imported, skipped int, msgs ...string) {
		if msgs == nil {
			msgs = []string{}
		}
		writeJSON(w, 201, envelope("qso", "POST", map[string]any{
			"parsed": len(recs), "imported": imported, "skipped": skipped, "messages": msgs}, nil))
	}
	for _, rec := range recs {
		if f.badCalls[rec["CALL"]] {
			apiErr(w, 400, "validation_error", "Record rejected: invalid CALL "+rec["CALL"], map[string]any{"messages": []string{"invalid call"}})
			return
		}
	}
	switch {
	case f.rejectMulti && len(recs) > 1, f.rejectAll:
		apiErr(w, 400, "validation_error", "Nothing could be imported: Invalid band", nil)
	case f.dup:
		res(0, len(recs))
	default:
		imported, skipped := 0, 0
		var msgs []string
		for _, rec := range recs {
			switch {
			case f.partialBad[rec["CALL"]]:
				msgs = append(msgs, "invalid record "+rec["CALL"])
			case f.has(profile, rec):
				skipped++ // "Records skipped as duplicates"
			default:
				f.rows = append(f.rows, fakeRow{profile, rec})
				imported++
			}
		}
		res(imported, skipped, msgs...)
	}
}

// has is Wavelog's duplicate rule, simplified: same call, date, time, band.
func (f *fakeWavelog) has(profile string, rec adif.Record) bool {
	for _, row := range f.rows {
		r := row.rec
		if row.profile == profile && r["CALL"] == rec["CALL"] && r["QSO_DATE"] == rec["QSO_DATE"] &&
			r["TIME_ON"] == rec["TIME_ON"] && r["BAND"] == rec["BAND"] {
			return true
		}
	}
	return false
}

// query returns the parsed query of the i-th recorded request.
func (f *fakeWavelog) query(i int) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, uri, _ := strings.Cut(f.reqs[i], " ")
	u, _ := url.Parse(uri)
	return u.Query()
}

// forbid answers the per-profile 403 envelope for forbidStation.
func (f *fakeWavelog) forbid(w http.ResponseWriter, station string) bool {
	if f.forbidStation == "" || station != f.forbidStation {
		return false
	}
	code := f.forbidCode
	if code == "" {
		code = "forbidden"
	}
	apiErr(w, 403, code, "You do not have access to station "+station, nil)
	return true
}
