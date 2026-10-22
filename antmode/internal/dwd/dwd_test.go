// SPDX-License-Identifier: AGPL-3.0-or-later

package dwd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// testdata/station_10637.json is a real response (Frankfurt/Main, fetched
// 2026-10-22, trimmed to three days): days[0] 2026-10-22 windDirection 2140,
// windGust 482; forecast1 wind arrays null.

func serve(t *testing.T, status int, body string, delay time.Duration) (*httptest.Server, *string) {
	var q string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.RawQuery
		if delay > 0 {
			time.Sleep(delay)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts, &q
}

func realBody(t *testing.T) string {
	b, err := os.ReadFile("testdata/station_10637.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFetchToday(t *testing.T) {
	ts, q := serve(t, 200, realBody(t), 0)
	c := &Client{BaseURL: ts.URL, UserAgent: "t"}
	// 2026-10-22 23:30 Berlin = 21:30 UTC: still the 22nd there.
	w, err := c.Fetch(context.Background(), "10637", time.Date(2026, 10, 22, 21, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if w.DirDeg != 214.0 || w.GustKmh != 48.2 || w.Day != "2026-10-22" {
		t.Fatalf("wind = %+v", w)
	}
	if *q != "stationIds=10637" {
		t.Fatalf("query = %q", *q)
	}
	// 22:30 UTC is already the 23rd in Berlin (CEST): the next day entry.
	w, err = c.Fetch(context.Background(), "10637", time.Date(2026, 10, 22, 22, 30, 0, 0, time.UTC))
	if err != nil || w.Day != "2026-10-23" || w.DirDeg != 182.0 || w.GustKmh != 61.1 {
		t.Fatalf("next day: %+v %v", w, err)
	}
}

func TestFetchNoData(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	cases := map[string]string{
		"station missing": `{"10638":{"days":[]}}`,
		"no today entry":  `{"10637":{"days":[{"dayDate":"2026-10-21","windDirection":100,"windGust":10}]}}`,
		"null direction":  `{"10637":{"days":[{"dayDate":"2026-10-22","windDirection":null,"windGust":10}]}}`,
		"null gust":       `{"10637":{"days":[{"dayDate":"2026-10-22","windDirection":100,"windGust":null}]}}`,
		"null days":       `{"10637":{"days":null}}`,
		"null station":    `{"10637":null}`,
	}
	for name, body := range cases {
		ts, _ := serve(t, 200, body, 0)
		if _, err := (&Client{BaseURL: ts.URL}).Fetch(context.Background(), "10637", now); !errors.Is(err, ErrNoData) {
			t.Errorf("%s: err = %v, want ErrNoData", name, err)
		}
	}
}

func TestFetchErrors(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"500":         {500, "oops"},
		"404":         {404, `{"error":"x"}`},
		"malformed":   {200, `{"10637":`},
		"implausible": {200, `{"10637":{"days":[{"dayDate":"2026-10-22","windDirection":9999,"windGust":10}]}}`},
	} {
		ts, _ := serve(t, tc.status, tc.body, 0)
		if _, err := (&Client{BaseURL: ts.URL}).Fetch(context.Background(), "10637", now); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	ts, _ := serve(t, 200, realBody(t), 300*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := (&Client{BaseURL: ts.URL}).Fetch(ctx, "10637", now); err == nil {
		t.Error("timeout: no error")
	}
}
