package clublog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"chase-queue/internal/store"
)

// DefaultAPI is Club Log's token API.
const DefaultAPI = "https://clublog.org/api/v1"

// Errors of the confirmation fetch. ErrAuth must not be retried.
var (
	ErrAuth    = errors.New("Club Log rejected the token")
	ErrNoToken = errors.New("no Club Log token configured")
)

// RateLimited is a 429; retry after Wait.
type RateLimited struct{ Wait time.Duration }

func (r *RateLimited) Error() string { return "Club Log rate limit; retry in " + r.Wait.String() }

// Confirmations fetches the operator's DXCC matrix with a personal API token
// and keeps the entity/band cells that count as confirmed. The token is held
// here only: it is never logged and never part of an error.
type Confirmations struct {
	API   string
	Token string
	Call  string
	// Confirmed lists the matrix status values that mean confirmed (or
	// verified). The values are not documented by Club Log; check them with
	// the -clublog-probe mode against the web chart.
	Confirmed []int
	HTTP      *http.Client
	UserAgent string
}

// Fetch downloads and parses the matrix.
func (c *Confirmations) Fetch(ctx context.Context) ([]store.Confirmation, error) {
	body, err := c.get(ctx, "/logs/"+url.PathEscape(strings.ToUpper(c.Call))+"/dxcc-matrix")
	if err != nil {
		return nil, err
	}
	return ParseMatrix(body, c.Confirmed)
}

// Probe returns the matrix's status values and how many cells carry each,
// so the operator can check which values mean confirmed.
func (c *Confirmations) Probe(ctx context.Context) (map[int]int, error) {
	body, err := c.get(ctx, "/logs/"+url.PathEscape(strings.ToUpper(c.Call))+"/dxcc-matrix")
	if err != nil {
		return nil, err
	}
	cells, err := parseCells(body)
	if err != nil {
		return nil, err
	}
	out := map[int]int{}
	for _, cell := range cells {
		out[cell.status]++
	}
	return out, nil
}

func (c *Confirmations) get(ctx context.Context, path string) ([]byte, error) {
	if c.Token == "" {
		return nil, ErrNoToken
	}
	api := c.API
	if api == "" {
		api = DefaultAPI
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(api, "/")+path, nil)
	if err != nil {
		return nil, errors.New("bad Club Log request")
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	ua := c.UserAgent
	if ua == "" {
		ua = "chase-queue (StationA)"
	}
	req.Header.Set("User-Agent", ua)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	res, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the URL: it never carries the token, but keep errors short
		}
		return nil, fmt.Errorf("Club Log request failed: %w", err)
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusUnprocessableEntity:
		return nil, ErrAuth
	case res.StatusCode == http.StatusTooManyRequests:
		wait := time.Minute
		if s, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && s > 0 {
			wait = time.Duration(s) * time.Second
		}
		return nil, &RateLimited{Wait: wait}
	case res.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("Club Log answered %s", res.Status)
	}
	return io.ReadAll(io.LimitReader(res.Body, 8<<20))
}

type cell struct {
	dxcc   int
	band   string
	status int
}

// parseCells reads {"<adif>": {"<band>": <status>}}, also when wrapped in a
// top-level object under "matrix", "dxcc" or "data". A band key may be "20"
// or "20m"; a status may be a number or a numeric string.
func parseCells(body []byte) ([]cell, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, errors.New("the Club Log matrix is not a JSON object")
	}
	for _, k := range []string{"matrix", "dxcc", "data"} {
		if inner, ok := top[k]; ok {
			var m map[string]json.RawMessage
			if json.Unmarshal(inner, &m) == nil {
				top = m
				break
			}
		}
	}
	var out []cell
	for k, raw := range top {
		adif, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		var bands map[string]json.RawMessage
		if json.Unmarshal(raw, &bands) != nil {
			continue
		}
		for b, sraw := range bands {
			st, ok := number(sraw)
			if !ok {
				continue
			}
			b = strings.ToLower(strings.TrimSpace(b))
			if !strings.HasSuffix(b, "m") {
				b += "m"
			}
			out = append(out, cell{adif, b, st})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the Club Log matrix has no entity/band cells")
	}
	return out, nil
}

func number(raw json.RawMessage) (int, bool) {
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return int(f), true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		return n, err == nil
	}
	return 0, false
}

// ParseMatrix keeps the cells whose status is in confirmed.
func ParseMatrix(body []byte, confirmed []int) ([]store.Confirmation, error) {
	cells, err := parseCells(body)
	if err != nil {
		return nil, err
	}
	want := map[int]bool{}
	for _, s := range confirmed {
		want[s] = true
	}
	var out []store.Confirmation
	for _, c := range cells {
		if want[c.status] {
			out = append(out, store.Confirmation{DXCC: c.dxcc, Band: c.band})
		}
	}
	return out, nil
}
