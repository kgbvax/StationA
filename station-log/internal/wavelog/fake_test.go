package wavelog

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"station-log/internal/adif"
)

// fakeWavelog is an httptest stand-in for the two endpoints, built from the
// API assumptions listed in sync_test.go. It holds contacts in memory.
type fakeWavelog struct {
	t   *testing.T
	srv *httptest.Server
	key string

	mu          sync.Mutex
	rows        []fakeRow // id = index+1
	fetches     []int64   // fetchfromid of every get_contacts_adif call
	posts       []string  // ADIF string of every /api/qso call
	status      int       // non-zero: answer every request with this status
	retry       string    // Retry-After header with the forced status
	dup         bool      // /api/qso reports every record as a duplicate
	rejectAll   bool      // /api/qso reports every record as an error (not a duplicate)
	rejectMulti bool      // /api/qso refuses a string of more than one record
	failPage    int       // get_contacts_adif answers 500 from this call number on (1-based, 0 = never)
	bodies      []string  // raw request bodies (to check the key never leaks)
	body        string    // with status: the response body instead of "forced failure"
	okBody      string    // non-empty: answer every request 200 with exactly this body
	hits        int       // requests that reached the server
	failStation string    // get_contacts_adif answers 500 for this station id
}

type fakeRow struct {
	profile string
	rec     adif.Record
}

func newFake(t *testing.T) *fakeWavelog {
	f := &fakeWavelog{t: t, key: "test-key-not-real"}
	mux := http.NewServeMux()
	mux.HandleFunc("/index.php/api/get_contacts_adif", f.contacts)
	mux.HandleFunc("/index.php/api/qso", f.qso)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeWavelog) addRow(profile string, rec adif.Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, fakeRow{profile, rec})
}

func (f *fakeWavelog) forced(w http.ResponseWriter) bool {
	f.hits++
	if f.okBody != "" {
		w.Write([]byte(f.okBody))
		return true
	}
	if f.status == 0 {
		return false
	}
	if f.retry != "" {
		w.Header().Set("Retry-After", f.retry)
	}
	msg := "forced failure"
	if f.body != "" {
		msg = f.body
	}
	http.Error(w, msg, f.status)
	return true
}

func (f *fakeWavelog) decode(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	f.bodies = append(f.bodies, b.String())
	var m map[string]any
	if err := json.Unmarshal([]byte(b.String()), &m); err != nil {
		http.Error(w, "bad json", 400)
		return nil, false
	}
	if m["key"] != f.key {
		http.Error(w, `{"status":"failed","reason":"invalid api key"}`, 401)
		return nil, false
	}
	return m, true
}

func (f *fakeWavelog) contacts(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.forced(w) {
		return
	}
	m, ok := f.decode(w, r)
	if !ok {
		return
	}
	from := int64(m["fetchfromid"].(float64))
	limit := int(m["limit"].(float64))
	station := m["station_id"].(string)
	if f.failStation != "" && station == f.failStation {
		http.Error(w, "boom", 500)
		return
	}
	f.fetches = append(f.fetches, from)
	if f.failPage > 0 && len(f.fetches) >= f.failPage {
		http.Error(w, "boom", 500)
		return
	}
	var sb strings.Builder
	var last int64
	n := 0
	for i, row := range f.rows {
		id := int64(i + 1)
		if id <= from || row.profile != station || n >= limit {
			continue
		}
		sb.WriteString(adif.Format(row.rec))
		last, n = id, n+1
	}
	if n == 0 {
		last = from
	}
	// ids come back as a number for lastfetchedid and a string for exported_qsos on purpose
	json.NewEncoder(w).Encode(map[string]any{"adif": sb.String(), "lastfetchedid": last, "exported_qsos": itoa(n)})
}

func (f *fakeWavelog) qso(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.forced(w) {
		return
	}
	m, ok := f.decode(w, r)
	if !ok {
		return
	}
	text, _ := m["string"].(string)
	f.posts = append(f.posts, text)
	recs := adif.Parse(text)
	resp := map[string]any{"status": "created", "adif_count": len(recs), "adif_errors": 0, "message": []string{}}
	fail := func(msg string) {
		resp["adif_errors"], resp["adif_count"] = len(recs), 0
		resp["message"] = []string{msg}
	}
	switch {
	case f.rejectMulti && len(recs) > 1:
		fail("Too many records in one request")
	case f.dup:
		fail("Station profile 1: Duplicate for CALL on 20m")
	case f.rejectAll:
		fail("Invalid band")
	default:
		for _, rec := range recs {
			f.rows = append(f.rows, fakeRow{m["station_profile_id"].(string), rec})
		}
	}
	json.NewEncoder(w).Encode(resp)
}
