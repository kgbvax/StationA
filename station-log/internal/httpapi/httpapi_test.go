package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"station-log/internal/importer"
	"station-log/internal/store"
)

func setup(t *testing.T, allow ...netip.Prefix) (*API, http.Handler) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := &API{Store: st, Importer: &importer.Importer{Store: st, Window: 5 * time.Minute, DefaultOwn: "DL9ET"},
		AllowFrom: allow, Window: 5 * time.Minute, Log: nil}
	return a, a.Handler()
}

const adi = `<CALL:5>K1ABC<QSO_DATE:8>20261101<TIME_ON:6>101500<BAND:3>20m<MODE:3>SSB<STATE:2>MA<EOR>
<CALL:5>W1AW <QSO_DATE:8>20261101<TIME_ON:6>111500<BAND:3>40m<MODE:2>CW<LOTW_QSL_RCVD:1>Y<EOR>
<CALL:5>BROKE<EOR>`

func do(h http.Handler, method, url, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, url, strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
}

func TestImportAndRead(t *testing.T) {
	_, h := setup(t)
	if w := do(h, "GET", "/healthz", "", nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	w := do(h, "POST", "/import?source=file1", adi, nil)
	var rep importer.Report
	decode(t, w, &rep)
	if w.Code != 200 || rep.Records != 3 || rep.Added != 2 || rep.SkippedInvalid != 1 {
		t.Fatalf("%d %+v", w.Code, rep)
	}
	w = do(h, "POST", "/import?source=file1", adi, nil)
	decode(t, w, &rep)
	if rep.Added != 0 || rep.Merged != 0 || rep.Unchanged != 2 {
		t.Fatalf("%+v", rep)
	}

	var st struct {
		QSOCount       int                  `json:"qso_count"`
		ConfirmedCount int                  `json:"confirmed_count"`
		OwnCalls       []string             `json:"own_calls"`
		Sources        map[string]int       `json:"sources"`
		LastImport     *importer.LastImport `json:"last_import"`
		DupWindow      string               `json:"dup_window"`
	}
	decode(t, do(h, "GET", "/v1/stats", "", nil), &st)
	if st.QSOCount != 2 || st.ConfirmedCount != 1 || st.OwnCalls[0] != "DL9ET" || st.Sources["file1"] != 2 || st.LastImport == nil || st.DupWindow != "5m0s" {
		t.Fatalf("%+v", st)
	}

	var wk struct {
		Own    string         `json:"own"`
		Count  int            `json:"count"`
		Worked []store.Worked `json:"worked"`
	}
	decode(t, do(h, "GET", "/v1/worked?own=dl9et", "", nil), &wk)
	if wk.Own != "DL9ET" || wk.Count != 2 || wk.Worked[0].Call != "K1ABC" && wk.Worked[1].Call != "K1ABC" {
		t.Fatalf("%+v", wk)
	}
	decode(t, do(h, "GET", "/v1/worked?own=NOBODY", "", nil), &wk)
	if wk.Count != 0 || wk.Worked == nil {
		t.Fatalf("%+v", wk)
	}
	decode(t, do(h, "GET", "/v1/worked?since=2999-01-01T00:00:00Z", "", nil), &wk)
	if wk.Count != 0 {
		t.Fatalf("%+v", wk)
	}
	if w := do(h, "GET", "/v1/worked?since=yesterday", "", nil); w.Code != 400 {
		t.Fatal(w.Code)
	}

	var qs struct {
		Count int         `json:"count"`
		QSOs  []store.QSO `json:"qsos"`
	}
	decode(t, do(h, "GET", "/v1/qsos?call=w1aw", "", nil), &qs)
	if qs.Count != 1 || qs.QSOs[0].Band != "40m" || !qs.QSOs[0].LoTWRcvd || qs.QSOs[0].ID == "" {
		t.Fatalf("%+v", qs)
	}
	if w := do(h, "GET", "/v1/qsos", "", nil); w.Code != 400 {
		t.Fatal(w.Code)
	}
}

func TestReplaceSourceHTTP(t *testing.T) {
	_, h := setup(t)
	do(h, "POST", "/import?source=l4", adi, nil)
	one := `<CALL:5>K1ABC<QSO_DATE:8>20261101<TIME_ON:6>101500<BAND:3>20m<MODE:3>SSB<EOR>`
	var rep importer.Report
	w := do(h, "POST", "/import?replace_source=l4", one, nil)
	decode(t, w, &rep)
	if w.Code != 200 || rep.Deleted != 1 || rep.Added != 0 {
		t.Fatalf("%d %+v", w.Code, rep)
	}
	if w := do(h, "POST", "/import?replace_source=l4&source=other", one, nil); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := do(h, "POST", "/import?replace_source=l4", "nothing here", nil); w.Code != 422 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var qs struct{ Count int }
	decode(t, do(h, "GET", "/v1/qsos?call=K1ABC", "", nil), &qs)
	if qs.Count != 1 {
		t.Fatal(qs)
	}
}

func TestGuards(t *testing.T) {
	if w := do(func() http.Handler { _, h := setup(t); return h }(), "POST", "/import", adi, map[string]string{"Origin": "http://evil"}); w.Code != 403 {
		t.Fatalf("origin: %d", w.Code)
	}
	_, h := setup(t, netip.MustParsePrefix("192.168.1.0/24"))
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.RemoteAddr = "10.0.0.5:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("outside allow_from: %d", w.Code)
	}
	r.RemoteAddr = "192.168.1.197:1234"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("inside allow_from: %d", w.Code)
	}
}
