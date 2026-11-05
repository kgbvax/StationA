// Package wavelog is the two-way sync with a Wavelog instance: a pull of its
// contacts through the importer (source "wavelog") and a push of the QSOs the
// loggers delivered. It speaks the documented REST API v2
// (https://docs.wavelog.org/developer/api-v2/, Wavelog >= 3.1.0). The tests
// encode the documented shapes with httptest fakes; against the operator's
// real instance only the unauthenticated probe (status endpoint, invalid_token
// error shape) has been verified. See CLAUDE.md.
package wavelog

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Source is the qso_sources tag of everything that came from Wavelog.
const Source = "wavelog"

// maxBody caps what is read from a response.
const maxBody = 32 << 20

// MaxPerPage is the documented maximum of per_page (API v2 overview,
// "Pagination").
const MaxPerPage = 5000

// APIError is a failed request. Its text carries the status, the v2 error
// code and a truncated, token-redacted message, never the request.
type APIError struct {
	Status     int // 0 = transport error
	Code       string
	Msg        string
	RetryAfter time.Duration
	// Auth marks a token problem: a dead token (401) or a token short of a
	// scope (403 insufficient_scope, Scope set). A config problem, not a
	// transient one.
	Auth bool
	// Profile marks a per-profile refusal (any other 403: forbidden = the
	// station id is not the token owner's, insufficient_club_permission,
	// club_access_revoked): the token is fine, this one station profile is
	// not reachable with it. Skip that profile, carry on with the others.
	Profile bool
	// Scope is set for 403 insufficient_scope: the scope the token lacks
	// (details.required_scope), "unknown" when the answer did not name it.
	Scope string
	// Record marks a 400 validation_error: the submitted record itself is
	// invalid. Every other failure (404/405/5xx, HTML, 429, ...) is an
	// endpoint error: back off, count no attempt against a QSO.
	Record bool
	// Err is the wrapped cause (a context error), if any.
	Err error
}

func (e *APIError) Error() string { return e.Msg }
func (e *APIError) Unwrap() error { return e.Err }

// Client talks to one instance. The token travels in the Authorization
// header only, never in a URL or body.
type Client struct {
	BaseURL string
	Key     string
	HTTP    *http.Client
	// MinGap is the least time between two requests.
	MinGap time.Duration

	last time.Time
}

// NewClient builds a client with a 30 s timeout, TLS verification on (plus an
// optional extra CA file) and no redirect following.
func NewClient(baseURL, key, caFile string) (*Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("wavelog ca_file: %w", err)
		}
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("wavelog ca_file %s: no certificate found", caFile)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"), Key: key, MinGap: time.Second,
		HTTP: &http.Client{Timeout: 30 * time.Second, Transport: tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

// flexInt decodes a JSON number or numeric string.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("not a number: %q", s)
	}
	*f = flexInt(n)
	return nil
}

// ContactsPage is one page of GET /api/v2/qso?format=adif.
type ContactsPage struct {
	ADIF          string // "" when nothing new (data.adif is null)
	LastFetchedID int64  // highest id of the page; 0 when empty
	Exported      int
	HasMore       bool
}

// GetContacts fetches one ADIF page of a station location: rows with an id
// above sinceID, ascending by id (QSO resource, "Export QSOs as ADIF").
func (c *Client) GetContacts(ctx context.Context, stationID string, sinceID int64, perPage int) (ContactsPage, error) {
	q := url.Values{}
	q.Set("format", "adif")
	q.Set("since_id", strconv.FormatInt(sinceID, 10))
	q.Set("station_id", stationID)
	q.Set("per_page", strconv.Itoa(min(max(perPage, 1), MaxPerPage)))
	data, err := c.do(ctx, http.MethodGet, "/index.php/api/v2/qso", q, nil)
	if err != nil {
		return ContactsPage{}, err
	}
	var env struct {
		Data *struct {
			ADIF          *string `json:"adif"`
			Exported      flexInt `json:"exported"`
			LastFetchedID flexInt `json:"lastfetchedid"`
		} `json:"data"`
		Meta struct {
			HasMore bool `json:"has_more"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(data, &env); err != nil || env.Data == nil {
		return ContactsPage{}, &APIError{Status: 200, Msg: "unexpected response (no data object): " + c.snippet(data)}
	}
	p := ContactsPage{LastFetchedID: int64(env.Data.LastFetchedID), Exported: int(env.Data.Exported), HasMore: env.Meta.HasMore}
	if env.Data.ADIF != nil {
		p.ADIF = *env.Data.ADIF
	}
	return p, nil
}

// ImportResult is the bulk-import summary (QSO resource, "Import multiple
// QSOs (ADIF)").
type ImportResult struct {
	Parsed   int
	Imported int
	Skipped  int // duplicates Wavelog already had
	Messages []string
}

// Text joins the messages.
func (r ImportResult) Text() string { return strings.Join(r.Messages, " ") }

// PostADIF imports ADIF text into a station location: POST /api/v2/qso with
// import_type adif.
func (c *Client) PostADIF(ctx context.Context, stationID, adifText string) (ImportResult, error) {
	id, err := strconv.Atoi(strings.TrimSpace(stationID))
	if err != nil || id < 1 {
		return ImportResult{}, &APIError{Msg: fmt.Sprintf("station_profile_id %q is not a positive integer", stationID)}
	}
	body := map[string]any{"import_type": "adif", "station_profile_id": id, "adif": adifText}
	data, aerr := c.do(ctx, http.MethodPost, "/index.php/api/v2/qso", nil, body)
	if aerr != nil {
		return ImportResult{}, aerr
	}
	var env struct {
		Data *struct {
			Parsed   *flexInt        `json:"parsed"`
			Imported flexInt         `json:"imported"`
			Skipped  flexInt         `json:"skipped"`
			Messages json.RawMessage `json:"messages"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil || env.Data == nil || env.Data.Parsed == nil {
		return ImportResult{}, &APIError{Status: 200, Msg: "unexpected response (no data.parsed): " + c.snippet(data)}
	}
	r := ImportResult{Parsed: int(*env.Data.Parsed), Imported: int(env.Data.Imported), Skipped: int(env.Data.Skipped)}
	var msgs []any
	if json.Unmarshal(env.Data.Messages, &msgs) == nil {
		for _, m := range msgs {
			r.Messages = append(r.Messages, c.redact(fmt.Sprint(m)))
		}
	}
	return r, nil
}

// TokenInfo is what GET /api/v2/token ("whoami") told us. The shape is not
// in the saved docs, so it is read leniently.
type TokenInfo struct {
	Scopes []string
	Fields map[string]string // scalar fields, token-redacted
	Keys   []string          // top-level field names of data
}

// Token calls GET /api/v2/token (needs no scope).
func (c *Client) Token(ctx context.Context) (TokenInfo, error) {
	data, err := c.do(ctx, http.MethodGet, "/index.php/api/v2/token", nil, nil)
	if err != nil {
		return TokenInfo{}, err
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if json.Unmarshal(data, &env) != nil || env.Data == nil {
		return TokenInfo{}, &APIError{Status: 200, Msg: "unexpected response (no data object): " + c.snippet(data)}
	}
	ti := TokenInfo{Fields: map[string]string{}}
	for k, v := range env.Data {
		ti.Keys = append(ti.Keys, k)
		switch x := v.(type) {
		case string:
			if k == "scopes" || k == "scope" {
				ti.Scopes = append(ti.Scopes, splitScopes(x)...)
			} else if !strings.EqualFold(k, "token") && !strings.Contains(strings.ToLower(k), "hash") {
				ti.Fields[k] = c.redact(x)
			}
		case []any:
			if k == "scopes" || k == "scope" {
				for _, e := range x {
					if s, ok := e.(string); ok {
						ti.Scopes = append(ti.Scopes, s)
					}
				}
			}
		case float64, bool:
			ti.Fields[k] = fmt.Sprint(x)
		}
	}
	sort.Strings(ti.Keys)
	sort.Strings(ti.Scopes)
	return ti, nil
}

func splitScopes(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' })
}

// Station is one station location (logbook profile).
type Station struct {
	ID       string
	Callsign string
	Name     string
}

// Stations calls GET /api/v2/station (scope station:read). The item field
// names are not in the saved docs; the usual Wavelog names are tried.
func (c *Client) Stations(ctx context.Context) ([]Station, error) {
	q := url.Values{}
	q.Set("per_page", strconv.Itoa(MaxPerPage))
	data, err := c.do(ctx, http.MethodGet, "/index.php/api/v2/station", q, nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		Data []map[string]any `json:"data"`
	}
	if json.Unmarshal(data, &env) != nil || env.Data == nil {
		return nil, &APIError{Status: 200, Msg: "unexpected response (data is not a list): " + c.snippet(data)}
	}
	pick := func(m map[string]any, keys ...string) string {
		for _, k := range keys {
			switch v := m[k].(type) {
			case string:
				if v != "" {
					return c.redact(v)
				}
			case float64:
				return strconv.FormatInt(int64(v), 10)
			}
		}
		return ""
	}
	var out []Station
	for _, m := range env.Data {
		out = append(out, Station{
			ID:       pick(m, "station_id", "id", "station_profile_id"),
			Callsign: pick(m, "station_callsign", "callsign", "call"),
			Name:     pick(m, "station_profile_name", "name", "profile_name"),
		})
	}
	return out, nil
}

// do sends one request and returns the body of a 2xx answer; everything else
// becomes an *APIError classified per the v2 error envelope (API v2
// overview, "Error responses").
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) ([]byte, *APIError) {
	if gap := c.MinGap - time.Since(c.last); gap > 0 {
		select {
		case <-time.After(gap):
		case <-ctx.Done():
			return nil, &APIError{Msg: ctx.Err().Error(), Err: ctx.Err()}
		}
	}
	c.last = time.Now()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, &APIError{Msg: err.Error()}
		}
		rd = bytes.NewReader(b)
	}
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, &APIError{Msg: c.redact(err.Error())}
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &APIError{Msg: ctx.Err().Error(), Err: ctx.Err()}
		}
		return nil, &APIError{Msg: "request failed: " + c.redact(err.Error())}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	c.last = time.Now()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, c.classify(resp, data)
	}
	return data, nil
}

// classify turns a non-2xx answer into an APIError.
func (c *Client) classify(resp *http.Response, data []byte) *APIError {
	var env struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details any    `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &env) // HTML or text: no envelope
	ae := &APIError{Status: resp.StatusCode}
	text := c.snippet(data)
	var details map[string]any
	if env.Error != nil {
		ae.Code = c.redact(env.Error.Code)
		text = c.snippet([]byte(env.Error.Message))
		details, _ = env.Error.Details.(map[string]any)
	}
	label := fmt.Sprintf("HTTP %d", resp.StatusCode)
	if ae.Code != "" {
		label += " " + ae.Code
	}
	ae.Msg = label + ": " + text
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		ae.Auth = true // invalid_token, token_expired, unauthorized
	case resp.StatusCode == http.StatusForbidden && ae.Code == "insufficient_scope":
		ae.Auth = true
		ae.Scope = "unknown"
		if s, ok := details["required_scope"].(string); ok && s != "" {
			ae.Scope = c.redact(s)
		}
		ae.Msg = fmt.Sprintf("HTTP 403 insufficient_scope: token lacks scope %s", ae.Scope)
	case resp.StatusCode == http.StatusForbidden:
		ae.Profile = true // forbidden, insufficient_club_permission, club_access_revoked, ...
	case resp.StatusCode == http.StatusTooManyRequests:
		ae.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		if ae.RetryAfter == 0 {
			switch v := details["retry_after"].(type) {
			case float64:
				ae.RetryAfter = time.Duration(v * float64(time.Second))
			case string:
				ae.RetryAfter = parseRetryAfter(v, time.Now())
			}
		}
	case resp.StatusCode == http.StatusBadRequest && ae.Code == "validation_error":
		ae.Record = true
	case resp.StatusCode == http.StatusNotFound && ae.Code != "not_found":
		ae.Msg += " (API v2 needs Wavelog 3.1.0 or later)"
	}
	return ae
}

// parseRetryAfter reads delta-seconds or an HTTP date; 0 = absent or past.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n > 0 {
			return time.Duration(n) * time.Second
		}
		return 0
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

func (c *Client) redact(s string) string {
	if c.Key != "" {
		s = strings.ReplaceAll(s, c.Key, "***")
	}
	return s
}

func (c *Client) snippet(data []byte) string {
	s := strings.Join(strings.Fields(c.redact(string(data))), " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
