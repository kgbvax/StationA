// SPDX-License-Identifier: AGPL-3.0-or-later

// Package nina reads DWD weather warnings for one district from the BBK NINA
// API (https://warnung.bund.de/api31, OpenAPI "Bundesamt für
// Bevölkerungsschutz: NINA API"):
//
//	GET /dashboard/{ARS}.json   → the district's current messages (all providers)
//	GET /warnings/{id}.json     → one message as CAP (info[].eventCode "II" etc.)
//
// Only DWD messages (id prefix "dwd.") are read; Cancel messages, DWD
// pre-information ("VORABINFORMATION …", which reuses the warning II codes)
// and non-Actual messages are skipped. A warning qualifies when its DWD II
// event code is in the configured list. Whether it is active now is a
// separate, clock-dependent question (Active).
//
// A poll either returns the complete qualifying set or an error: one failed
// detail request fails the whole poll. A failed poll is "unknown", never
// "no warnings" — the policy must not count it as quiet time.
package nina

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// maxBody bounds every response read (a dashboard is a few kB, a CAP message
// with eight languages about 30 kB).
const maxBody = 4 << 20

// Warning is one qualifying DWD warning.
type Warning struct {
	ID       string    `json:"id"`
	Event    string    `json:"event"`
	Headline string    `json:"headline,omitempty"`
	II       string    `json:"ii"`
	Onset    time.Time `json:"onset"`
	// Expires is zero when the message carries none (open-ended).
	Expires time.Time `json:"expires,omitzero"`
}

// ActiveAt reports onset <= now < expires (no lead time).
func (w Warning) ActiveAt(now time.Time) bool {
	if now.Before(w.Onset) {
		return false
	}
	return w.Expires.IsZero() || now.Before(w.Expires)
}

// Active returns the warnings active at now, in input order.
func Active(ws []Warning, now time.Time) []Warning {
	var out []Warning
	for _, w := range ws {
		if w.ActiveAt(now) {
			out = append(out, w)
		}
	}
	return out
}

// Client queries NINA.
type Client struct {
	BaseURL   string
	UserAgent string
	HTTP      *http.Client
	// Codes is the qualifying DWD II code set.
	Codes map[string]bool
}

// DistrictARS zero-pads a 12-digit ARS to district level (last 7 digits
// "0000000"): NINA only serves the dashboard per Kreis.
func DistrictARS(ars string) (string, error) {
	if len(ars) != 12 || strings.Trim(ars, "0123456789") != "" {
		return "", fmt.Errorf("nina: ARS %q is not 12 digits", ars)
	}
	return ars[:5] + "0000000", nil
}

// dashboard is the subset of ARSOverviewResult read here.
type dashboard []struct {
	ID      string `json:"id"`
	Payload struct {
		Data struct {
			MsgType  string `json:"msgType"`
			Provider string `json:"provider"`
		} `json:"data"`
	} `json:"payload"`
}

// capAlert is the subset of the CAP message read here.
type capAlert struct {
	Identifier string    `json:"identifier"`
	Status     string    `json:"status"`
	MsgType    string    `json:"msgType"`
	Sent       string    `json:"sent"`
	Info       []capInfo `json:"info"`
}

type capInfo struct {
	Language  string `json:"language"`
	Event     string `json:"event"`
	Headline  string `json:"headline"`
	EventCode []struct {
		ValueName string `json:"valueName"`
		Value     string `json:"value"`
	} `json:"eventCode"`
	Effective string `json:"effective"`
	Onset     string `json:"onset"`
	Expires   string `json:"expires"`
}

// Fetch returns the qualifying DWD warnings for the district of ars (not
// filtered by time), sorted by id.
func (c *Client) Fetch(ctx context.Context, ars string) ([]Warning, error) {
	district, err := DistrictARS(ars)
	if err != nil {
		return nil, err
	}
	var dash dashboard
	if err := c.getJSON(ctx, "/dashboard/"+district+".json", &dash); err != nil {
		return nil, fmt.Errorf("nina dashboard: %w", err)
	}
	var out []Warning
	for _, m := range dash {
		if !strings.HasPrefix(m.ID, "dwd.") {
			continue // other providers (MoWaS, KATWARN, BIWAPP, LHP, police)
		}
		if strings.EqualFold(m.Payload.Data.MsgType, "Cancel") {
			continue
		}
		w, ok, err := c.detail(ctx, m.ID)
		if err != nil {
			return nil, fmt.Errorf("nina warning %s: %w", m.ID, err)
		}
		if ok {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// detail fetches one CAP message; ok is false when it does not qualify.
func (c *Client) detail(ctx context.Context, id string) (Warning, bool, error) {
	var a capAlert
	if err := c.getJSON(ctx, "/warnings/"+url.PathEscape(id)+".json", &a); err != nil {
		return Warning{}, false, err
	}
	if strings.EqualFold(a.MsgType, "Cancel") || (a.Status != "" && !strings.EqualFold(a.Status, "Actual")) {
		return Warning{}, false, nil
	}
	info, ok := pickInfo(a.Info)
	if !ok {
		return Warning{}, false, errors.New("CAP message has no info block")
	}
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(info.Event)), "VORABINFORMATION") {
		return Warning{}, false, nil
	}
	ii := ""
	for _, ec := range info.EventCode {
		if ec.ValueName == "II" {
			ii = strings.TrimSpace(ec.Value)
		}
	}
	if !c.Codes[ii] {
		return Warning{}, false, nil
	}
	onset, err := firstTime(info.Onset, info.Effective, a.Sent)
	if err != nil {
		return Warning{}, false, fmt.Errorf("onset: %w", err)
	}
	var expires time.Time
	if strings.TrimSpace(info.Expires) != "" {
		if expires, err = time.Parse(time.RFC3339, strings.TrimSpace(info.Expires)); err != nil {
			return Warning{}, false, fmt.Errorf("expires: %w", err)
		}
	}
	return Warning{
		ID: id, Event: strings.TrimSpace(info.Event), Headline: strings.TrimSpace(info.Headline),
		II: ii, Onset: onset, Expires: expires,
	}, true, nil
}

// pickInfo prefers the German info block (the DWD original), else the first.
func pickInfo(infos []capInfo) (capInfo, bool) {
	if len(infos) == 0 {
		return capInfo{}, false
	}
	for _, i := range infos {
		if strings.HasPrefix(strings.ToLower(i.Language), "de") {
			return i, true
		}
	}
	return infos[0], true
}

// firstTime parses the first non-empty RFC 3339 value.
func firstTime(vals ...string) (time.Time, error) {
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		return time.Parse(time.RFC3339, v)
	}
	return time.Time{}, errors.New("missing")
}

func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}
