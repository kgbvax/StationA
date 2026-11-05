// Package wavelog is the two-way sync with a Wavelog instance: a pull of its
// contacts through the importer (source "wavelog") and a push of the QSOs the
// loggers delivered. Every API detail here is from memory and unverified
// against a real instance (see CLAUDE.md and sync_test.go).
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
	"os"
	"strconv"
	"strings"
	"time"
)

// Source is the qso_sources tag of everything that came from Wavelog.
const Source = "wavelog"

// maxBody caps what is read from a response.
const maxBody = 32 << 20

// APIError is a failed request. Its text carries the status and a truncated,
// key-redacted body, never the request.
type APIError struct {
	Status     int // 0 = transport error
	Msg        string
	RetryAfter time.Duration
	// Auth marks a rejected key or missing permission: a config problem, not
	// a transient one.
	Auth bool
	// Record marks a non-2xx answer whose JSON body says the submitted record
	// itself is invalid (Wavelog's per-record failure). Every other non-2xx
	// (wrong URL, proxy page, 404/405/408/413, HTML 400) is an endpoint error:
	// back off, count no attempt against a QSO.
	Record bool
}

func (e *APIError) Error() string { return e.Msg }

// Client talks to one instance. The key travels in the JSON body only.
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

// ContactsPage is one get_contacts_adif response.
type ContactsPage struct {
	ADIF          string  `json:"adif"`
	LastFetchedID flexInt `json:"lastfetchedid"`
	Exported      flexInt `json:"exported_qsos"`
	Status        string  `json:"status,omitempty"`
	Reason        string  `json:"reason,omitempty"`
}

// failedStatus is a status text that says the request did not succeed.
func failedStatus(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "failed", "failure", "error", "fail", "denied":
		return true
	}
	return false
}

// PushResult is one /api/qso response.
type PushResult struct {
	Status     string          `json:"status"`
	AdifCount  flexInt         `json:"adif_count"`
	AdifErrors flexInt         `json:"adif_errors"`
	RawMessage json.RawMessage `json:"message"`
	Reason     string          `json:"reason"`
	Messages   []string        `json:"-"`
}

// Text joins the messages and the reason.
func (r PushResult) Text() string {
	return strings.Join(append(append([]string{}, r.Messages...), r.Reason), " ")
}

// GetContacts fetches rows with a primary key above from, for one station
// profile.
func (c *Client) GetContacts(ctx context.Context, stationID string, from int64, limit int) (ContactsPage, error) {
	var raw struct {
		ADIF          *string  `json:"adif"`
		LastFetchedID flexInt  `json:"lastfetchedid"`
		Exported      *flexInt `json:"exported_qsos"`
		Status        string   `json:"status"`
		Reason        string   `json:"reason"`
	}
	body := map[string]any{"key": c.Key, "station_id": stationID, "fetchfromid": from, "limit": limit}
	if err := c.post(ctx, "/index.php/api/get_contacts_adif", body, &raw); err != nil {
		return ContactsPage{}, err
	}
	p := ContactsPage{LastFetchedID: raw.LastFetchedID, Status: raw.Status, Reason: raw.Reason}
	if raw.ADIF != nil {
		p.ADIF = *raw.ADIF
	}
	if raw.Exported != nil {
		p.Exported = *raw.Exported
	}
	text := strings.TrimSpace(raw.Status + " " + raw.Reason)
	switch {
	case failedStatus(raw.Status):
		return p, &APIError{Status: 200, Msg: "Wavelog refused the request: " + c.snippet([]byte(text)), Auth: looksLikeAuth([]byte(text))}
	case raw.ADIF == nil && raw.Exported == nil:
		return p, &APIError{Status: 200, Msg: "unexpected response (no adif/exported_qsos): " + c.snippet([]byte(text)), Auth: looksLikeAuth([]byte(text))}
	}
	return p, nil
}

// PostQSO sends ADIF text to /api/qso for a station profile.
func (c *Client) PostQSO(ctx context.Context, stationID, adifText string) (PushResult, error) {
	var r PushResult
	body := map[string]any{"key": c.Key, "station_profile_id": stationID, "type": "adif", "string": adifText}
	if err := c.post(ctx, "/index.php/api/qso", body, &r); err != nil {
		return r, err
	}
	var s string
	if len(r.RawMessage) > 0 {
		if json.Unmarshal(r.RawMessage, &s) == nil {
			r.Messages = []string{s}
		} else {
			var arr []any
			if json.Unmarshal(r.RawMessage, &arr) == nil {
				for _, v := range arr {
					r.Messages = append(r.Messages, fmt.Sprint(v))
				}
			}
		}
	}
	text := strings.TrimSpace(r.Text())
	switch {
	case failedStatus(r.Status) && looksLikeAuth([]byte(text)):
		return r, &APIError{Status: 200, Msg: "Wavelog refused the request: " + c.snippet([]byte(text)), Auth: true}
	case strings.TrimSpace(r.Status) == "":
		return r, &APIError{Status: 200, Msg: "unexpected response (no status field): " + c.snippet([]byte(text))}
	}
	return r, nil
}

func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	if gap := c.MinGap - time.Since(c.last); gap > 0 {
		select {
		case <-time.After(gap):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.last = time.Now()
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return &APIError{Msg: c.redact(err.Error())}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &APIError{Msg: "request failed: " + c.redact(err.Error())}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	c.last = time.Now()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		ae := &APIError{Status: resp.StatusCode,
			Msg:  fmt.Sprintf("HTTP %d: %s", resp.StatusCode, c.snippet(data)),
			Auth: resp.StatusCode == 401 || resp.StatusCode == 403}
		ae.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && !ae.Auth && resp.StatusCode != 429 {
			var rec struct {
				Status string `json:"status"`
			}
			ae.Record = json.Unmarshal(data, &rec) == nil && strings.TrimSpace(rec.Status) != ""
		}
		return ae
	}
	if err := json.Unmarshal(data, out); err != nil {
		// an invalid key often comes back as a 200 with a status text
		return &APIError{Status: 200, Msg: "unreadable response: " + c.snippet(data), Auth: looksLikeAuth(data)}
	}
	return nil
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

func looksLikeAuth(data []byte) bool {
	l := strings.ToLower(string(data))
	return strings.Contains(l, "invalid api key") || strings.Contains(l, "invalid key") || strings.Contains(l, "unauthorized")
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
