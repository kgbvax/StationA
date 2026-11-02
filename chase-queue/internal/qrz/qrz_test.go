package qrz

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const pw = "qrz_super_secret_pw"

const found = `<?xml version="1.0" ?>
<QRZDatabase version="1.34" xmlns="http://xmldata.qrz.com">
  <Callsign>
    <call>EA8XYZ</call><fname>Juan</fname><name>Perez</name>
    <addr1>Calle 1</addr1><addr2>Las Palmas</addr2><country>Canary Islands</country>
    <lat>28.1</lat><lon>-15.4</lon><grid>IL28gc</grid>
    <image>https://cdn-xml.qrz.com/z/ea8xyz/pic.jpg</image>
    <dxcc>29</dxcc><cqzone>33</cqzone><ituzone>36</ituzone>
  </Callsign>
  <Session><Key>k2</Key><SubExp>Wed Jan 1 2027</SubExp></Session>
</QRZDatabase>`

func server(t *testing.T, logins *atomic.Int32, expireFirst bool) *httptest.Server {
	var expired atomic.Bool
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case q.Get("username") != "":
			logins.Add(1)
			if q.Get("password") != pw {
				fmt.Fprint(w, `<QRZDatabase><Session><Error>Username/password incorrect</Error></Session></QRZDatabase>`)
				return
			}
			fmt.Fprint(w, `<QRZDatabase><Session><Key>k1</Key><SubExp>Wed Jan 1 2027</SubExp></Session></QRZDatabase>`)
		case expireFirst && !expired.Swap(true):
			fmt.Fprint(w, `<QRZDatabase><Session><Error>Session Timeout</Error></Session></QRZDatabase>`)
		case q.Get("callsign") == "EA8XYZ":
			fmt.Fprint(w, found)
		default:
			fmt.Fprintf(w, `<QRZDatabase><Session><Key>k1</Key><Error>Not found: %s</Error></Session></QRZDatabase>`, q.Get("callsign"))
		}
	}))
}

func TestLookup(t *testing.T) {
	var logins atomic.Int32
	srv := server(t, &logins, false)
	defer srv.Close()
	c := &Client{API: srv.URL, User: "DL1ABC", Password: pw}
	r, err := c.Lookup(context.Background(), "EA8XYZ")
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "Juan Perez" || r.City != "Las Palmas" || r.Grid != "IL28gc" || !r.HasLL || r.Lat != 28.1 || r.CQZone != 33 || r.Image == "" {
		t.Fatalf("%+v", r)
	}
	if _, err := c.Lookup(context.Background(), "XX1XX"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if logins.Load() != 1 || c.Subscription() == "" {
		t.Fatalf("logins %d sub %q", logins.Load(), c.Subscription())
	}
}

func TestSessionTimeoutLogsInAgain(t *testing.T) {
	var logins atomic.Int32
	srv := server(t, &logins, true)
	defer srv.Close()
	c := &Client{API: srv.URL, User: "DL1ABC", Password: pw}
	if _, err := c.Lookup(context.Background(), "EA8XYZ"); err != nil {
		t.Fatal(err)
	}
	if logins.Load() != 2 {
		t.Fatalf("logins %d", logins.Load())
	}
}

func TestBadLoginAndSecretHygiene(t *testing.T) {
	var logins atomic.Int32
	srv := server(t, &logins, false)
	c := &Client{API: srv.URL, User: "DL1ABC", Password: "wrong_" + pw}
	if _, err := c.Lookup(context.Background(), "EA8XYZ"); !errors.Is(err, ErrAuth) {
		t.Fatalf("got %v", err)
	}
	srv.Close()
	// a transport error must not quote the URL (it carries the password)
	c = &Client{API: srv.URL, User: "DL1ABC", Password: pw}
	_, err := c.Lookup(context.Background(), "EA8XYZ")
	if err == nil || strings.Contains(err.Error(), pw) || strings.Contains(err.Error(), "password") {
		t.Fatalf("got %v", err)
	}
}

func TestNameFallbacks(t *testing.T) {
	db, err := Parse([]byte(`<QRZDatabase><Callsign><call>k1abc</call><name_fmt>Bob "B" Smith</name_fmt><fname>Bob</fname><lat>0</lat><lon>0</lon></Callsign></QRZDatabase>`))
	if err != nil {
		t.Fatal(err)
	}
	if r := db.Callsign.record(); r.Name != `Bob "B" Smith` || r.Call != "K1ABC" || r.HasLL {
		t.Fatalf("%+v", r)
	}
}
