package wavelog

// API assumptions encoded by these tests (FROM MEMORY, unverified against a
// real Wavelog instance; check each one before enabling the sync):
//
//	(a) both endpoints live under /index.php/api/ and take the API key as the
//	    "key" field of a JSON POST body (never in the URL);
//	(b) get_contacts_adif request fields: key, station_id, fetchfromid, limit;
//	(c) response fields: adif (ADIF text), lastfetchedid, exported_qsos; rows
//	    come in increasing primary-key order and lastfetchedid is the highest
//	    id returned (numbers may arrive as JSON numbers or strings);
//	(d) /api/qso accepts {key, station_profile_id, type:"adif", string} and a
//	    string with several records;
//	(e) a duplicate is reported inside a 200 response body (status/adif_errors/
//	    message text containing "Duplicate"), not as a non-200 code;
//	(f) pulled ADIF carries STATION_CALLSIGN (optional), BAND, MODE/SUBMODE,
//	    QSO_DATE, TIME_ON and the confirmation fields QSL_RCVD, LOTW_QSL_RCVD,
//	    EQSL_QSL_RCVD;
//	(g) /api/qso never updates an existing QSO;
//	(h) the key needs read access for pull and read/write access for push.
//
// The data is synthetic.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"station-log/internal/adif"
	"station-log/internal/importer"
	"station-log/internal/store"
)

func itoa(n int) string { return strconv.Itoa(n) }

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

type env struct {
	t    *testing.T
	f    *fakeWavelog
	st   *store.Store
	imp  *importer.Importer
	s    *Syncer
	logb *bytes.Buffer
	now  time.Time
}

func newEnv(t *testing.T, pull, push bool) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, f: newFake(t), st: st, logb: &bytes.Buffer{}, now: t0}
	e.imp = &importer.Importer{Store: st, Window: 5 * time.Minute}
	cl, err := NewClient(e.f.srv.URL, e.f.key, "")
	if err != nil {
		t.Fatal(err)
	}
	cl.MinGap = 0
	cl.HTTP = e.f.srv.Client()
	e.s = &Syncer{
		Cfg: Config{Pull: pull, Push: push, URLHost: "wl.test", Profiles: []Profile{{"DL9ET", "1"}},
			Interval: time.Minute, FullResync: 24 * time.Hour, Batch: 50, PullLimit: 2,
			PushSources: []string{"log4om", "adif"}, PushSince: t0.Add(-24 * time.Hour)},
		Client: cl, Store: st, Importer: e.imp, Now: func() time.Time { return e.now },
		Log: slog.New(slog.NewTextHandler(e.logb, nil)),
	}
	return e
}

func rec(call string, at time.Time, extra ...string) adif.Record {
	r := adif.Record{"CALL": call, "QSO_DATE": at.Format("20060102"), "TIME_ON": at.Format("150405"),
		"BAND": "20m", "MODE": "SSB", "FREQ": "14.200000"}
	for i := 0; i+1 < len(extra); i += 2 {
		r[extra[i]] = extra[i+1]
	}
	return r
}

func (e *env) logQSO(call string, at time.Time, own string, extra ...string) {
	e.t.Helper()
	r := rec(call, at, extra...)
	r["STATION_CALLSIGN"] = own
	r["RST_SENT"] = "59"
	if _, err := e.imp.Import(context.Background(), adif.Format(r), importer.Options{Source: "log4om"}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) count() int {
	n, err := e.st.Count()
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) cycle() error { return e.s.Cycle(context.Background(), true) }

func TestPullPagingCursorAndIncremental(t *testing.T) {
	e := newEnv(t, true, false)
	for i := 0; i < 5; i++ {
		e.f.addRow("1", rec("DK"+itoa(i)+"AA", t0.Add(time.Duration(i)*time.Hour))) // no STATION_CALLSIGN: profile own call applies
	}
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if e.count() != 5 {
		t.Fatalf("count %d, want 5", e.count())
	}
	if v, _, _ := e.st.Setting("wavelog_cursor_1"); v != "5" {
		t.Fatalf("cursor %q", v)
	}
	if got := ints(e.f.fetches); got != "0,2,4" {
		t.Fatalf("fetchfromid sequence %s, want 0,2,4", got)
	}
	e.f.fetches = nil
	e.f.addRow("1", rec("DK9ZZ", t0.Add(9*time.Hour)))
	e.now = e.now.Add(time.Hour) // no full resync yet
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if e.count() != 6 || ints(e.f.fetches) != "5" {
		t.Fatalf("count %d fetches %v: incremental pull should start at the cursor", e.count(), e.f.fetches)
	}
	if qs, _ := e.st.ByCall("DK9ZZ", "DL9ET"); len(qs) != 1 || qs[0].Source != "wavelog" {
		t.Fatalf("pulled QSO %+v", qs)
	}
}

func ints(v []int64) string {
	var s []string
	for _, n := range v {
		s = append(s, strconv.FormatInt(n, 10))
	}
	return strings.Join(s, ",")
}

func TestCursorOnlyAdvancesAfterGoodPage(t *testing.T) {
	e := newEnv(t, true, false)
	for i := 0; i < 5; i++ {
		e.f.addRow("1", rec("DK"+itoa(i)+"AA", t0.Add(time.Duration(i)*time.Hour)))
	}
	e.f.failPage = 2 // the second request fails
	if err := e.cycle(); err == nil {
		t.Fatal("want an error")
	}
	// full resync pass: the cursor is only written when the whole pass ended
	if v, ok, _ := e.st.Setting("wavelog_cursor_1"); ok && v != "0" {
		t.Fatalf("cursor %q after a failed pass", v)
	}
	if e.count() != 2 {
		t.Fatalf("first page should be stored, count %d", e.count())
	}
	e.f.failPage = 0
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if e.count() != 5 {
		t.Fatalf("count %d after resume", e.count())
	}
}

func TestFullResyncCatchesConfirmationOnOldRow(t *testing.T) {
	e := newEnv(t, true, false)
	e.f.addRow("1", rec("DK1AA", t0))
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	e.f.rows[0].rec["LOTW_QSL_RCVD"] = "Y" // Wavelog's own LoTW sync, same row id
	e.now = e.now.Add(time.Hour)
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if qs, _ := e.st.ByCall("DK1AA", ""); qs[0].LoTWRcvd {
		t.Fatal("an incremental pull must not see a change on an old row")
	}
	e.now = e.now.Add(25 * time.Hour)
	e.f.fetches = nil
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if qs, _ := e.st.ByCall("DK1AA", ""); !qs[0].LoTWRcvd {
		t.Fatal("the full resync should merge the confirmation")
	}
	if ints(e.f.fetches)[:1] != "0" {
		t.Fatalf("full resync starts at 0, got %v", e.f.fetches)
	}
}

func TestPushOnceAndRoundTripMerges(t *testing.T) {
	e := newEnv(t, true, true)
	e.logQSO("DK1AA", t0, "DL9ET")
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if len(e.f.posts) != 1 {
		t.Fatalf("posts %d, want 1", len(e.f.posts))
	}
	if !strings.Contains(e.f.posts[0], "<RST_SENT:2>59") {
		t.Fatalf("the raw record's RST should survive the push: %q", e.f.posts[0])
	}
	// the push cycle ran after the pull; the next pull returns it (fake stored it)
	e.now = e.now.Add(time.Hour)
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if e.count() != 1 {
		t.Fatalf("count %d: the pushed QSO coming back must merge, not add", e.count())
	}
	if len(e.f.posts) != 1 {
		t.Fatalf("posts %d: pushed QSO must not be pushed again", len(e.f.posts))
	}
	if st := e.s.Status(); st.Pushed != 1 || st.PushPending != 0 {
		t.Fatalf("status %+v", st)
	}
}

func TestPulledQSONeverPushed(t *testing.T) {
	e := newEnv(t, true, true)
	e.f.addRow("1", rec("DK2BB", t0, "STATION_CALLSIGN", "DL9ET"))
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if e.count() != 1 || len(e.f.posts) != 0 {
		t.Fatalf("count %d posts %d", e.count(), len(e.f.posts))
	}
}

func TestPushSinceDefaultsToEnableTime(t *testing.T) {
	e := newEnv(t, false, true)
	e.s.Cfg.PushSince = time.Time{}
	e.logQSO("DK1OLD", t0.Add(-48*time.Hour), "DL9ET")
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	e.logQSO("DK1NEW", t0.Add(time.Minute), "DL9ET")
	e.now = e.now.Add(time.Minute)
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if len(e.f.posts) != 1 || !strings.Contains(e.f.posts[0], "DK1NEW") {
		t.Fatalf("only the QSO after the enable time goes out: %q", e.f.posts)
	}
}

func TestNoProfileSkipped(t *testing.T) {
	e := newEnv(t, false, true)
	e.logQSO("DK1AA", t0, "DA0XYZ")
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if len(e.f.posts) != 0 || e.s.Status().SkippedNoProfile != 1 {
		t.Fatalf("posts %d status %+v", len(e.f.posts), e.s.Status())
	}
}

func TestDuplicateResponseCountsAsPushed(t *testing.T) {
	e := newEnv(t, false, true)
	e.f.dup = true
	e.logQSO("DK1AA", t0, "DL9ET")
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	qs, _ := e.st.ByCall("DK1AA", "")
	if msg, _ := e.st.PushError(qs[0].ID, "1"); msg != "duplicate" {
		t.Fatalf("last_error %q", msg)
	}
	n := len(e.f.posts)
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if len(e.f.posts) != n {
		t.Fatal("a duplicate must not be retried")
	}
}

func TestBatchFallsBackToSingleRecords(t *testing.T) {
	e := newEnv(t, false, true)
	e.f.rejectMulti = true
	e.logQSO("DK1AA", t0, "DL9ET")
	e.logQSO("DK2BB", t0.Add(time.Hour), "DL9ET")
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if len(e.f.posts) != 3 || e.s.Status().Pushed != 2 {
		t.Fatalf("posts %d status %+v", len(e.f.posts), e.s.Status())
	}
}

func TestPermanentFailureStopsAfterFiveAttempts(t *testing.T) {
	e := newEnv(t, false, true)
	e.f.rejectAll = true
	e.logQSO("DK1AA", t0, "DL9ET")
	for i := 0; i < 8; i++ {
		if err := e.cycle(); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.f.posts) != 5 {
		t.Fatalf("posts %d, want 5 attempts", len(e.f.posts))
	}
	if st := e.s.Status(); st.PushFailed != 1 || st.PushPending != 0 {
		t.Fatalf("status %+v", st)
	}
}

func TestAuthErrorBacksOffAndKeyNeverLeaks(t *testing.T) {
	e := newEnv(t, true, true)
	e.s.Client.Key = "wrong"
	err := e.cycle()
	ae, ok := err.(*APIError)
	if !ok || !ae.Auth {
		t.Fatalf("err %v, want an auth APIError", err)
	}
	e.s.finish(err)
	if st := e.s.Status(); st.BackoffSeconds < int(maxBackoff.Seconds()) || st.Error == "" {
		t.Fatalf("status %+v", st)
	}
	for _, s := range []string{e.logb.String(), err.Error()} {
		if strings.Contains(s, "wrong") || strings.Contains(s, e.f.key) {
			t.Fatalf("key in %q", s)
		}
	}
	b, _ := json.Marshal(e.s.Status())
	if strings.Contains(string(b), e.f.key) || strings.Contains(string(b), "wrong") {
		t.Fatal("key in status JSON")
	}
}

func TestKeyInEchoedBodyIsRedacted(t *testing.T) {
	c := &Client{Key: "secret-key-xyz"}
	if s := c.snippet([]byte("error for secret-key-xyz here")); strings.Contains(s, "secret-key-xyz") {
		t.Fatal(s)
	}
}

func TestOutageWarnsOnceAndBackoffDoublesAndResets(t *testing.T) {
	e := newEnv(t, true, true)
	fail := &APIError{Status: 500, Msg: "HTTP 500: boom"}
	var secs []int
	for i := 0; i < 5; i++ {
		e.s.finish(fail)
		secs = append(secs, e.s.Status().BackoffSeconds)
	}
	if n := strings.Count(e.logb.String(), "level=WARN"); n != 1 {
		t.Fatalf("%d warnings for one outage:\n%s", n, e.logb.String())
	}
	for i, want := range []int{30, 60, 120, 240, 480} {
		if secs[i] < want || secs[i] > want*6/5+1 {
			t.Fatalf("backoff %v, step %d want ~%d", secs, i, want)
		}
	}
	if e.s.Status().Error == "" {
		t.Fatal("error should be set")
	}
	e.s.finish(nil)
	if st := e.s.Status(); st.Error != "" || st.BackoffSeconds != 0 {
		t.Fatalf("status %+v", st)
	}
	e.s.finish(fail)
	if n := strings.Count(e.logb.String(), "level=WARN"); n != 2 {
		t.Fatalf("a new outage should warn again, got %d", n)
	}
}

func TestRetryAfterHonoured(t *testing.T) {
	e := newEnv(t, true, false)
	e.f.status, e.f.retry = 429, "600"
	err := e.cycle()
	ae, ok := err.(*APIError)
	if !ok || ae.RetryAfter != 10*time.Minute {
		t.Fatalf("err %#v", err)
	}
	e.s.finish(err)
	if e.s.Status().BackoffSeconds < 600 {
		t.Fatalf("backoff %d", e.s.Status().BackoffSeconds)
	}
}

func TestBuildADIFOverlaysMergedValuesOnRaw(t *testing.T) {
	c := store.PushCandidate{
		Raw: adif.Format(adif.Record{"CALL": "DK1AA", "RST_SENT": "59", "NAME": "Test", "GRIDSQUARE": "JO32"}),
		QSO: store.QSO{OwnCall: "DL9ET", Call: "DK1AA", Band: "20m", Mode: "SSB", At: t0, FreqHz: 14200000, Grid: "JO32WE", LoTWRcvd: true, DXCC: 230},
	}
	r := adif.Parse(BuildADIF(c))[0]
	if r["NAME"] != "Test" || r["RST_SENT"] != "59" || r["GRIDSQUARE"] != "JO32WE" || r["LOTW_QSL_RCVD"] != "Y" ||
		r["DXCC"] != "230" || r["FREQ"] != "14.200000" || r["TIME_ON"] != "120000" || r["STATION_CALLSIGN"] != "DL9ET" {
		t.Fatalf("%v", r)
	}
	// struct-only (no raw)
	r = adif.Parse(BuildADIF(store.PushCandidate{QSO: c.QSO}))[0]
	if r["CALL"] != "DK1AA" || r["BAND"] != "20m" {
		t.Fatalf("%v", r)
	}
}

func TestPanicInCycleIsRecovered(t *testing.T) {
	e := newEnv(t, true, false)
	e.s.Client = nil // nil dereference inside the pull
	if err := e.cycle(); err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("err %v", err)
	}
}

// restart builds a second Syncer over the same store, as after a service
// restart.
func (e *env) restart() *Syncer {
	old := e.s
	s := &Syncer{Cfg: old.Cfg, Client: old.Client, Store: old.Store, Importer: old.Importer,
		Now: old.Now, Log: old.Log}
	e.s = s
	return s
}

func (e *env) attempts(qsoID string) int {
	e.t.Helper()
	cs, err := e.st.PushCandidates(store.PushQuery{OwnCalls: []string{"DL9ET"}, ProfileID: "1"})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, c := range cs {
		if c.QSO.ID == qsoID {
			return c.Attempts
		}
	}
	return -1
}

func TestPullFailedStatusIsAnErrorNotSuccess(t *testing.T) {
	e := newEnv(t, true, false)
	e.f.addRow("1", rec("DK1AA", t0))
	e.f.okBody = `{"status":"failed","reason":"Invalid API key"}`
	err := e.cycle()
	var ae *APIError
	if !errors.As(err, &ae) || !ae.Auth {
		t.Fatalf("err %v, want an auth APIError", err)
	}
	if st := e.s.Status(); !st.LastPullAt.IsZero() || !st.LastFullResync.IsZero() {
		t.Fatalf("status %+v: nothing may be marked done", st)
	}
	if _, ok, _ := e.st.Setting("wavelog_full_1"); ok {
		t.Fatal("full resync marked done after a failed pull")
	}
}

func TestPullBodyWithoutAnyFieldIsAnError(t *testing.T) {
	e := newEnv(t, true, false)
	e.f.okBody = `{"hello":"world"}`
	if err := e.cycle(); err == nil {
		t.Fatal("a body with no adif/exported_qsos must be an error")
	}
	if _, ok, _ := e.st.Setting("wavelog_full_1"); ok || !e.s.Status().LastPullAt.IsZero() {
		t.Fatal("pull marked done")
	}
}

func TestPushBodyWithoutStatusOrWithFailedKeyIsEndpointError(t *testing.T) {
	for _, body := range []string{`{}`, `{"status":"failed","reason":"Invalid API key"}`} {
		e := newEnv(t, false, true)
		e.f.okBody = body
		e.logQSO("DK1AA", t0, "DL9ET")
		for i := 0; i < 7; i++ {
			if err := e.cycle(); err == nil {
				t.Fatalf("body %s: want an error", body)
			}
		}
		if a := e.attempts("1"); a > 0 {
			t.Fatalf("body %s: %d attempts counted against the QSO", body, a)
		}
	}
}

func TestEndpointErrorsCountNoAttempts(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{{404, "not found"}, {405, "nope"}, {408, "timeout"}, {413, "too big"}, {400, "<html>Bad Request</html>"}} {
		e := newEnv(t, false, true)
		e.f.status, e.f.body = tc.status, tc.body
		e.logQSO("DK1AA", t0, "DL9ET")
		for i := 0; i < 8; i++ {
			if err := e.cycle(); err == nil {
				t.Fatalf("%d: want an error", tc.status)
			}
		}
		if a := e.attempts("1"); a > 0 {
			t.Fatalf("%d: %d attempts counted", tc.status, a)
		}
		if st := e.s.Status(); st.PushFailed != 0 || st.PushPending != 1 {
			t.Fatalf("%d: status %+v", tc.status, st)
		}
		// the endpoint comes back: the backlog goes out
		e.f.status = 0
		if err := e.cycle(); err != nil || len(e.f.posts) != 1 {
			t.Fatalf("%d: recovery err %v posts %d", tc.status, err, len(e.f.posts))
		}
	}
}

func TestJSONRecordRefusalIsPerQSOFailure(t *testing.T) {
	e := newEnv(t, false, true)
	e.f.status, e.f.body = 400, `{"status":"failed","reason":"Invalid band"}`
	e.logQSO("DK1AA", t0, "DL9ET")
	for i := 0; i < 8; i++ {
		if err := e.cycle(); err != nil {
			t.Fatal(err)
		}
	}
	if st := e.s.Status(); st.PushFailed != 1 || e.attempts("1") != 5 {
		t.Fatalf("status %+v attempts %d", st, e.attempts("1"))
	}
}

func TestExhaustedAttemptsResetAfter24hAndOnURLChange(t *testing.T) {
	e := newEnv(t, false, true)
	e.f.rejectAll = true
	e.logQSO("DK1AA", t0, "DL9ET")
	for i := 0; i < 6; i++ {
		e.cycle()
	}
	if len(e.f.posts) != 5 || e.s.Status().PushFailed != 1 {
		t.Fatalf("posts %d status %+v", len(e.f.posts), e.s.Status())
	}
	e.f.rejectAll = false
	e.now = e.now.Add(23 * time.Hour)
	e.cycle()
	if len(e.f.posts) != 5 {
		t.Fatal("reset before 24 h")
	}
	e.now = e.now.Add(2 * time.Hour)
	if err := e.cycle(); err != nil || len(e.f.posts) != 6 || e.s.Status().PushFailed != 0 {
		t.Fatalf("err %v posts %d status %+v", err, len(e.f.posts), e.s.Status())
	}

	// profile map change resets too
	e2 := newEnv(t, false, true)
	e2.f.rejectAll = true
	e2.logQSO("DK1AA", t0, "DL9ET")
	for i := 0; i < 6; i++ {
		e2.cycle()
	}
	e2.f.rejectAll = false
	e2.restart().Cfg.Profiles = []Profile{{"DL9ET", "1"}, {"DL1XX", "7"}}
	if err := e2.cycle(); err != nil || len(e2.f.posts) != 6 {
		t.Fatalf("err %v posts %d", err, len(e2.f.posts))
	}
}

func TestURLChangeResetsCursorsAndLedger(t *testing.T) {
	// push-only: a pulled-back copy would carry the wavelog source and never be pushed again
	e := newEnv(t, false, true)
	e.s.Cfg.URL = "https://a.example"
	if err := e.st.SetSetting("wavelog_cursor_1", "7"); err != nil {
		t.Fatal(err)
	}
	e.logQSO("DK1BB", t0, "DL9ET")
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := e.st.Setting("wavelog_cursor_1"); v != "7" || len(e.f.posts) != 1 {
		t.Fatalf("cursor %q posts %d", v, len(e.f.posts))
	}
	// same URL after a restart: nothing resets
	e.restart()
	if err := e.cycle(); err != nil || len(e.f.posts) != 1 {
		t.Fatalf("err %v posts %d", err, len(e.f.posts))
	}
	s := e.restart()
	s.Cfg.URL = "https://b.example"
	e.f.posts = nil
	e.now = e.now.Add(time.Minute)
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if len(e.f.posts) != 1 {
		t.Fatalf("ledger not reset: %d posts", len(e.f.posts))
	}
	if !strings.Contains(s.Status().Note, "reset") || !strings.Contains(e.logb.String(), "level=WARN") {
		t.Fatalf("note %q log %s", s.Status().Note, e.logb.String())
	}
	if v, ok, _ := e.st.Setting("wavelog_cursor_1"); ok {
		t.Fatalf("cursor %q survived the URL change", v)
	}
}

func TestPushSincePersistedAcrossRestart(t *testing.T) {
	e := newEnv(t, false, true)
	e.s.Cfg.PushSince = time.Time{}
	e.logQSO("DK1OLD", t0.Add(-48*time.Hour), "DL9ET")
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(48 * time.Hour) // the restart is much later
	e.restart()
	e.logQSO("DK1MID", t0.Add(time.Hour), "DL9ET") // after enable, before the restart
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if len(e.f.posts) != 1 || !strings.Contains(e.f.posts[0], "DK1MID") {
		t.Fatalf("persisted push_since not used: %q", e.f.posts)
	}
}

func TestUnreadablePushSinceReinitialisesToNow(t *testing.T) {
	for _, bad := range []string{"garbage", ""} {
		e := newEnv(t, false, true)
		e.s.Cfg.PushSince = time.Time{}
		if err := e.st.SetSetting("wavelog_push_since", bad); err != nil {
			t.Fatal(err)
		}
		e.logQSO("DK1OLD", t0.Add(-48*time.Hour), "DL9ET")
		if err := e.cycle(); err != nil {
			t.Fatal(err)
		}
		if len(e.f.posts) != 0 {
			t.Fatalf("%q: history was bulk-pushed: %q", bad, e.f.posts)
		}
		v, _, _ := e.st.Setting("wavelog_push_since")
		if tm, err := time.Parse(time.RFC3339, v); err != nil || !tm.Equal(t0) {
			t.Fatalf("%q: setting %q", bad, v)
		}
		if !strings.Contains(e.logb.String(), "level=WARN") {
			t.Fatalf("%q: no warning", bad)
		}
	}
}

func TestProfilesAreIndependent(t *testing.T) {
	e := newEnv(t, true, true)
	e.s.Cfg.Profiles = []Profile{{"DL9ET", "1"}, {"DL1XX", "2"}}
	e.f.failStation = "1"
	e.f.addRow("2", rec("DK2AA", t0))
	e.logQSO("DK1AA", t0, "DL9ET")
	e.logQSO("DK2BB", t0, "DL1XX")
	err := e.cycle()
	if err == nil {
		t.Fatal("profile 1's pull failure must surface")
	}
	if e.count() != 3 {
		t.Fatalf("count %d: profile 2 must still be pulled", e.count())
	}
	if len(e.f.posts) != 2 {
		t.Fatalf("posts %d: both profiles' QSOs must be pushed", len(e.f.posts))
	}
	if !e.s.Status().LastPullAt.IsZero() {
		t.Fatal("last_pull_at must not advance on a partial pull")
	}
}

func TestKickRespectsBackoff(t *testing.T) {
	e := newEnv(t, false, true)
	e.s.Debounce = time.Millisecond
	e.s.Cfg.Interval = time.Hour
	e.f.status = 500
	e.logQSO("DK1AA", t0, "DL9ET")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	hits := func() int { e.f.mu.Lock(); defer e.f.mu.Unlock(); return e.f.hits }
	deadline := time.Now().Add(5 * time.Second)
	for e.s.Status().Error == "" {
		if time.Now().After(deadline) {
			t.Fatal("first cycle never failed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	before := hits()
	e.s.Kick()
	time.Sleep(200 * time.Millisecond)
	if hits() != before {
		t.Fatalf("kick pushed during backoff: %d -> %d requests", before, hits())
	}
}

func TestRetryAfterHTTPDate(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	if d := parseRetryAfter(now.Add(90*time.Second).Format(http.TimeFormat), now); d != 90*time.Second {
		t.Fatalf("date form: %v", d)
	}
	if d := parseRetryAfter("120", now); d != 2*time.Minute {
		t.Fatalf("seconds form: %v", d)
	}
	if d := parseRetryAfter(now.Add(-time.Hour).Format(http.TimeFormat), now); d != 0 {
		t.Fatalf("past date: %v", d)
	}
	// end to end through the client
	e := newEnv(t, true, false)
	e.f.status, e.f.retry = 429, time.Now().Add(10*time.Minute).UTC().Format(http.TimeFormat)
	var ae *APIError
	if err := e.cycle(); !errors.As(err, &ae) || ae.RetryAfter < 9*time.Minute {
		t.Fatalf("err %#v", err)
	}
}
