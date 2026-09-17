// SPDX-License-Identifier: AGPL-3.0-or-later

// Package qrz resolves callsigns to position and identity via the QRZ.com
// XML API (xmldata.qrz.com), with a session-key login and a persistent disk
// cache. It exists because Log4OM's outbound CALLSIGN datagram is the bare
// callsign (see internal/log4om) — the bridge resolves the operator-keyed
// call here instead of relying on the logger to know where the station is.
//
// Wire contract (https://www.qrz.com/docs/xml/current_spec.html): login with
// ?username=…;password=… returns <Session><Key>; lookups pass s=KEY and
// callsign=CALL. An idle session times out and the lookup response says so —
// the client re-logs in once and retries. Only what the selected-station
// record needs is modeled: grid, lat/lon, country, city.
package qrz

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBase is the QRZ XML endpoint (the "current" versioned path).
const DefaultBase = "https://xmldata.qrz.com/xml/current/"

// RequestTimeout bounds one HTTP request; a lookup is at most login+lookup,
// so the worker can stall ~2× this worst case when QRZ re-authenticates.
const RequestTimeout = 5 * time.Second

var (
	// ErrNotFound means QRZ has no record for the call. It is negative-
	// cached briefly so a typo does not re-hit the API on every keystroke.
	ErrNotFound = errors.New("qrz: callsign not found")
	// ErrAuth means the credentials or the subscription are wrong. Not
	// cached: fixing the config must take effect immediately.
	ErrAuth = errors.New("qrz: authentication failed (subscription, username or password)")
)

// Record is the QRZ answer reduced to what the selected-station record uses.
type Record struct {
	Call    string  `json:"call"`
	Grid    string  `json:"grid,omitempty"`
	Lat     float64 `json:"lat,omitempty"`
	Lon     float64 `json:"lon,omitempty"`
	Country string  `json:"country,omitempty"`
	Qth     string  `json:"qth,omitempty"`  // QRZ addr2 (city)
	Name    string  `json:"name,omitempty"` // QRZ fname + name (operator)
}

// NormalizeCall canonicalizes a callsign for lookup and cache keys.
func NormalizeCall(call string) string {
	return strings.ToUpper(strings.TrimSpace(call))
}

// Client is a logged-in QRZ XML session. Safe for concurrent use.
type Client struct {
	base string
	user string
	pass string
	http *http.Client

	mu      sync.Mutex
	session string
}

// New builds a client against DefaultBase.
func New(user, pass string) *Client {
	return &Client{
		base: DefaultBase,
		user: user,
		pass: pass,
		http: &http.Client{Timeout: RequestTimeout},
	}
}

// Lookup resolves one call. A session-timeout answer triggers exactly one
// re-login and retry; any other error is returned as-is (ErrNotFound,
// ErrAuth, or a wrapped transport/parse error).
func (c *Client) Lookup(ctx context.Context, call string) (Record, error) {
	call = NormalizeCall(call)
	if call == "" {
		return Record{}, fmt.Errorf("qrz: empty callsign")
	}

	rec, err := c.query(ctx, call)
	if errors.Is(err, errSession) {
		if lerr := c.login(ctx); lerr != nil {
			return Record{}, lerr
		}
		rec, err = c.query(ctx, call)
	}
	return rec, err
}

type qrzResponse struct {
	Session struct {
		Key   string `xml:"Key"`
		Error string `xml:"Error"`
	} `xml:"Session"`
	Callsign struct {
		Call    string `xml:"call"`
		Addr2   string `xml:"addr2"`
		Country string `xml:"country"`
		Grid    string `xml:"grid"`
		Lat     string `xml:"lat"`
		Lon     string `xml:"lon"`
		Fname   string `xml:"fname"`
		Name    string `xml:"name"` // surname in QRZ's XML
	} `xml:"Callsign"`
}

var errSession = errors.New("qrz: session expired")

// query runs one callsign lookup with the current session key.
func (c *Client) query(ctx context.Context, call string) (Record, error) {
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == "" {
		return Record{}, errSession
	}

	data, err := c.get(ctx, "?s="+url.QueryEscape(session)+";callsign="+url.QueryEscape(call))
	if err != nil {
		return Record{}, err
	}
	var resp qrzResponse
	if err := xml.Unmarshal(data, &resp); err != nil {
		return Record{}, fmt.Errorf("qrz: decode lookup response: %w", err)
	}
	if err := sessionError(resp.Session.Error); err != nil {
		return Record{}, err
	}
	c.setSession(resp.Session.Key)
	return recordFrom(resp, call), nil
}

// login authenticates and stores the session key.
func (c *Client) login(ctx context.Context) error {
	data, err := c.get(ctx, "?username="+url.QueryEscape(c.user)+";password="+url.QueryEscape(c.pass))
	if err != nil {
		return err
	}
	var resp qrzResponse
	if err := xml.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("qrz: decode login response: %w", err)
	}
	if err := sessionError(resp.Session.Error); err != nil {
		return err
	}
	if resp.Session.Key == "" {
		return fmt.Errorf("qrz: login returned no session key")
	}
	c.setSession(resp.Session.Key)
	return nil
}

// sessionError maps QRZ's free-text <Error> to typed errors. The strings are
// not versioned by QRZ; match on the stable fragments ("session timeout",
// "not found", "username/password", "not subscribed") and pass anything else
// through verbatim.
func sessionError(s string) error {
	switch {
	case s == "":
		return nil
	case strings.Contains(strings.ToLower(s), "session timeout"),
		strings.EqualFold(strings.TrimSpace(s), "not logged in"):
		return errSession
	case strings.Contains(strings.ToLower(s), "not found"):
		return ErrNotFound
	case strings.Contains(strings.ToLower(s), "username/password"),
		strings.Contains(strings.ToLower(s), "not subscribed"):
		return ErrAuth
	default:
		return fmt.Errorf("qrz: %s", s)
	}
}

func recordFrom(resp qrzResponse, call string) Record {
	rec := Record{
		Call:    resp.Callsign.Call,
		Grid:    resp.Callsign.Grid,
		Country: resp.Callsign.Country,
		Qth:     resp.Callsign.Addr2,
		Name:    strings.TrimSpace(resp.Callsign.Fname + " " + resp.Callsign.Name),
	}
	if rec.Call == "" {
		rec.Call = call
	}
	rec.Lat, _ = strconv.ParseFloat(strings.TrimSpace(resp.Callsign.Lat), 64)
	rec.Lon, _ = strconv.ParseFloat(strings.TrimSpace(resp.Callsign.Lon), 64)
	return rec
}

func (c *Client) setSession(key string) {
	if key == "" {
		return
	}
	c.mu.Lock()
	c.session = key
	c.mu.Unlock()
}

func (c *Client) get(ctx context.Context, params string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+params, nil)
	if err != nil {
		return nil, fmt.Errorf("qrz: build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qrz: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("qrz: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("qrz: read response: %w", err)
	}
	return body, nil
}

// Service wraps a Client with the persistent cache and implements the
// bridge resolver's lookup seam. Lookup order: unexpired positive hit →
// unexpired negative hit (ErrNotFound) → QRZ.
type Service struct {
	client *Client
	cache  *Cache
}

// NewService combines a client with a cache. Cache write failures are
// deliberately swallowed (documented): persistence is an optimization — the
// lookup answer still stands.
func NewService(client *Client, cache *Cache) *Service {
	return &Service{client: client, cache: cache}
}

func (s *Service) Lookup(ctx context.Context, call string) (Record, error) {
	call = NormalizeCall(call)

	if rec, ok, err := s.cache.Get(call); ok || err != nil {
		return rec, err
	}

	rec, err := s.client.Lookup(ctx, call)
	switch {
	case err == nil:
		_ = s.cache.Put(rec)
	case errors.Is(err, ErrNotFound):
		_ = s.cache.PutNotFound(call)
	}
	return rec, err
}
