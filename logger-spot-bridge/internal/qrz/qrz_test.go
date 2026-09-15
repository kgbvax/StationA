// SPDX-License-Identifier: AGPL-3.0-or-later

package qrz

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// splitQuery parses a ";"-separated QRZ query (the spec's separator, which
// net/url's Query() deliberately does not split since Go 1.17).
func splitQuery(raw string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ";") {
		if i := strings.Index(pair, "="); i >= 0 {
			k, kerr := url.QueryUnescape(pair[:i])
			v, verr := url.QueryUnescape(pair[i+1:])
			if kerr == nil && verr == nil {
				out[k] = v
			}
		}
	}
	return out
}

// loginXML / lookupXML / errorXML are golden shapes from the QRZ spec
// (https://www.qrz.com/docs/xml/current_spec.html), including the default
// namespace the real endpoint carries.
const loginXML = `<?xml version="1.0" ?>
<QRZDatabase xmlns="http://xmldata.qrz.com" version="1.3.4">
  <Session>
    <Key>9781e431d8bdb0e7a2b8d9041c0b0c1d</Key>
    <Count>142</Count>
    <SubExp>2027-03-14</SubExp>
    <GMTime>2026-09-15 14:03:12</GMTime>
  </Session>
</QRZDatabase>`

const dl1abcXML = `<?xml version="1.0" ?>
<QRZDatabase xmlns="http://xmldata.qrz.com" version="1.3.4">
  <Session>
    <Key>9781e431d8bdb0e7a2b8d9041c0b0c1d</Key>
    <Count>143</Count>
    <SubExp>2027-03-14</SubExp>
  </Session>
  <Callsign>
    <call>DL1ABC</call>
    <fname>Jürgen</fname>
    <name>Müller</name>
    <addr1>Hauptstr. 1</addr1>
    <addr2>München</addr2>
    <country>Germany</country>
    <grid>JO60AB</grid>
    <lat>48.137222</lat>
    <lon>11.575556</lon>
    <dxcc>230</dxcc>
    <cqzone>14</cqzone>
    <ituzone>28</ituzone>
  </Callsign>
</QRZDatabase>`

func sessionErrXML(msg string) string {
	return fmt.Sprintf(`<?xml version="1.0" ?>
<QRZDatabase xmlns="http://xmldata.qrz.com">
  <Session><Error>%s</Error></Session>
</QRZDatabase>`, msg)
}

// fakeQRZ serves the scripted sequence of bodies and records every request's
// parsed query. Bodies are popped in order; an empty script answers 500.
type fakeQRZ struct {
	t      *testing.T
	bodies []string
	got    []map[string]string
	server *httptest.Server
}

func newFakeQRZ(t *testing.T, bodies ...string) *fakeQRZ {
	f := &fakeQRZ{t: t, bodies: bodies}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.got = append(f.got, splitQuery(r.URL.RawQuery))
		if len(f.bodies) == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(f.bodies[0]))
		f.bodies = f.bodies[1:]
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeQRZ) client() *Client {
	c := New("U", "P")
	c.base = f.server.URL + "/"
	return c
}

func TestLookupSuccess(t *testing.T) {
	f := newFakeQRZ(t, loginXML, dl1abcXML)
	rec, err := f.client().Lookup(context.Background(), " dl1abc ")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}

	if len(f.got) != 2 {
		t.Fatalf("got %d requests, want 2 (login+lookup)", len(f.got))
	}
	if f.got[0]["username"] != "U" || f.got[0]["password"] != "P" {
		t.Errorf("login query = %v, want username/password", f.got[0])
	}
	if f.got[1]["s"] != "9781e431d8bdb0e7a2b8d9041c0b0c1d" || f.got[1]["callsign"] != "DL1ABC" {
		t.Errorf("lookup query = %v, want session key + normalized call", f.got[1])
	}

	want := Record{
		Call: "DL1ABC", Grid: "JO60AB",
		Lat: 48.137222, Lon: 11.575556,
		Country: "Germany", Qth: "München",
	}
	if rec != want {
		t.Errorf("record = %+v, want %+v", rec, want)
	}
}

func TestLookupReusesSession(t *testing.T) {
	f := newFakeQRZ(t, loginXML, dl1abcXML, dl1abcXML)
	c := f.client()

	for i := 0; i < 2; i++ {
		if _, err := c.Lookup(context.Background(), "DL1ABC"); err != nil {
			t.Fatalf("lookup %d: %v", i, err)
		}
	}
	if len(f.got) != 3 {
		t.Fatalf("got %d requests, want 3 (one login, two lookups)", len(f.got))
	}
}

func TestSessionTimeoutRetriggersLoginOnce(t *testing.T) {
	// The session can die mid-run (QRZ invalidates server-side): the first
	// lookup establishes it, the second hits the stale-key answer and must
	// recover with exactly one re-login + retry.
	f := newFakeQRZ(t,
		loginXML,                         // initial login
		dl1abcXML,                        // first lookup succeeds
		sessionErrXML("session timeout"), // second lookup: stale session
		loginXML,                         // re-login
		dl1abcXML,                        // retried lookup succeeds
	)
	c := f.client()
	ctx := context.Background()

	if _, err := c.Lookup(ctx, "DL1ABC"); err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	rec, err := c.Lookup(ctx, "DL1ABC")
	if err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	if rec.Call != "DL1ABC" {
		t.Errorf("call = %q", rec.Call)
	}
	if len(f.got) != 5 {
		t.Fatalf("got %d requests, want 5 (login+lookup, stale lookup, re-login, retry)", len(f.got))
	}
}

func TestLookupNotFound(t *testing.T) {
	f := newFakeQRZ(t, loginXML, sessionErrXML("Not found: XX9XX"))
	if _, err := f.client().Lookup(context.Background(), "XX9XX"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestAuthFailure(t *testing.T) {
	f := newFakeQRZ(t, sessionErrXML("Username/password incorrect"))
	if _, err := f.client().Lookup(context.Background(), "DL1ABC"); !errors.Is(err, ErrAuth) {
		t.Fatalf("login err = %v, want ErrAuth", err)
	}
}

func TestUnrecognizedErrorPassesThrough(t *testing.T) {
	f := newFakeQRZ(t, loginXML, sessionErrXML("wait 12 seconds"))
	_, err := f.client().Lookup(context.Background(), "DL1ABC")
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrAuth) || errors.Is(err, errSession) {
		t.Fatalf("err = %v, want a plain wrapped error", err)
	}
	if !strings.Contains(err.Error(), "wait 12 seconds") {
		t.Errorf("err = %v, want the QRZ message verbatim", err)
	}
}

func TestEmptyCallRejected(t *testing.T) {
	f := newFakeQRZ(t) // any HTTP call here is a failure
	if _, err := f.client().Lookup(context.Background(), "  "); err == nil {
		t.Fatal("empty call must be rejected before any request")
	}
	if len(f.got) != 0 {
		t.Fatalf("got %d requests, want 0", len(f.got))
	}
}

func TestMalformedLatLonYieldZero(t *testing.T) {
	bad := strings.Replace(dl1abcXML, "<lat>48.137222</lat>", "<lat>north-ish</lat>", 1)
	f := newFakeQRZ(t, loginXML, bad)
	rec, err := f.client().Lookup(context.Background(), "DL1ABC")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if rec.Lat != 0 || rec.Lon != 11.575556 {
		t.Errorf("lat/lng = %v/%v, want 0/11.575556", rec.Lat, rec.Lon)
	}
}

func TestServiceCacheHitSkipsHTTP(t *testing.T) {
	f := newFakeQRZ(t, loginXML, dl1abcXML)
	cache := OpenCache(t.TempDir()+"/qrz.json", DefaultCacheDays, DefaultNegativeMinutes, 10)
	svc := NewService(f.client(), cache)
	ctx := context.Background()

	if _, err := svc.Lookup(ctx, "DL1ABC"); err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	rec, err := svc.Lookup(ctx, "dl1abc")
	if err != nil {
		t.Fatalf("cached lookup: %v", err)
	}
	if rec.Grid != "JO60AB" {
		t.Errorf("cached record = %+v", rec)
	}
	if len(f.got) != 2 {
		t.Fatalf("got %d requests, want 2 (cache must absorb the second lookup)", len(f.got))
	}
}

func TestServiceNegativeResultCached(t *testing.T) {
	f := newFakeQRZ(t, loginXML, sessionErrXML("Not found: XX9XX"))
	cache := OpenCache(t.TempDir()+"/qrz.json", DefaultCacheDays, DefaultNegativeMinutes, 10)
	svc := NewService(f.client(), cache)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := svc.Lookup(ctx, "XX9XX"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("lookup %d: err = %v, want ErrNotFound", i, err)
		}
	}
	if len(f.got) != 2 {
		t.Fatalf("got %d requests, want 2 (negative result must be cached)", len(f.got))
	}
}

func TestServiceAuthFailureNotCached(t *testing.T) {
	f := newFakeQRZ(t,
		sessionErrXML("Username/password incorrect"), // login fails
		loginXML,                                     // login succeeds after "fix"
		dl1abcXML,
	)
	cache := OpenCache(t.TempDir()+"/qrz.json", DefaultCacheDays, DefaultNegativeMinutes, 10)
	svc := NewService(f.client(), cache)
	ctx := context.Background()

	if _, err := svc.Lookup(ctx, "DL1ABC"); !errors.Is(err, ErrAuth) {
		t.Fatalf("first lookup: err = %v, want ErrAuth", err)
	}
	if _, err := svc.Lookup(ctx, "DL1ABC"); err != nil {
		t.Fatalf("retry after credential fix: %v", err)
	}
}
