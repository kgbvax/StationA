// SPDX-License-Identifier: AGPL-3.0-or-later

// Package dwd reads the day wind forecast for one DWD station from the DWD
// app backend (https://app-prod-ws.warnwetter.de/v30, OpenAPI "Deutscher
// Wetterdienst: API"):
//
//	GET /stationOverviewExtended?stationIds=<id>
//
// The response maps the station id to {forecast1, days[], …}. antmode uses the
// days[] entry for today's Europe/Berlin date: windDirection in tenths of a
// degree, windGust in tenths of km/h (verified live 2026-10-22: windDirection
// 2650 = 265.0°, windGust 482 = 48.2 km/h). The hourly forecast1 wind arrays
// came back null for every station tried, so they are not used.
//
// A missing station, a missing today entry or null wind fields are errors:
// the caller keeps its last value and lets it go stale.
package dwd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	_ "time/tzdata" // Europe/Berlin without relying on the host's zoneinfo
)

const maxBody = 4 << 20

// ErrNoData: the response has no usable wind for today.
var ErrNoData = errors.New("dwd: no wind data for today")

// Berlin is the DWD day boundary.
var Berlin = mustLoad("Europe/Berlin")

func mustLoad(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

// Wind is today's day forecast.
type Wind struct {
	DirDeg  float64
	GustKmh float64
	Day     string // YYYY-MM-DD (Europe/Berlin)
}

// Client queries the DWD app backend.
type Client struct {
	BaseURL   string
	UserAgent string
	HTTP      *http.Client
}

type day struct {
	DayDate       string `json:"dayDate"`
	WindDirection *int   `json:"windDirection"`
	WindGust      *int   `json:"windGust"`
}

type station struct {
	Days []day `json:"days"`
}

// Fetch returns the wind for the Europe/Berlin date of now.
func (c *Client) Fetch(ctx context.Context, stationID string, now time.Time) (Wind, error) {
	u := strings.TrimRight(c.BaseURL, "/") + "/stationOverviewExtended?stationIds=" + url.QueryEscape(stationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Wind{}, err
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
		return Wind{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return Wind{}, fmt.Errorf("dwd: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Wind{}, err
	}
	var all map[string]*station
	if err := json.Unmarshal(b, &all); err != nil {
		return Wind{}, fmt.Errorf("dwd: decode: %w", err)
	}
	st := all[stationID]
	if st == nil {
		return Wind{}, fmt.Errorf("%w (station %s not in response)", ErrNoData, stationID)
	}
	today := now.In(Berlin).Format("2006-01-02")
	for _, d := range st.Days {
		if d.DayDate != today {
			continue
		}
		if d.WindDirection == nil || d.WindGust == nil {
			return Wind{}, fmt.Errorf("%w (null wind fields for %s)", ErrNoData, today)
		}
		if *d.WindDirection < 0 || *d.WindDirection > 3600 || *d.WindGust < 0 {
			return Wind{}, fmt.Errorf("dwd: implausible wind dir=%d gust=%d", *d.WindDirection, *d.WindGust)
		}
		return Wind{DirDeg: float64(*d.WindDirection) / 10, GustKmh: float64(*d.WindGust) / 10, Day: today}, nil
	}
	return Wind{}, fmt.Errorf("%w (no days[] entry for %s)", ErrNoData, today)
}
