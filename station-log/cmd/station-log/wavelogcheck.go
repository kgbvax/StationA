package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"time"

	"station-log/internal/config"
	"station-log/internal/wavelog"
)

// wavelogCheck is -wavelog-check: with the token from STATION_LOG_WAVELOG_KEY
// it asks the instance who the token is (GET /api/v2/token) and which station
// locations it can see (GET /api/v2/station), so the operator can fill
// [[wavelog.profile]] own_call -> station_profile_id. It prints the scopes
// and the locations, never the token. Returns the exit code.
func wavelogCheck(ctx context.Context, cfg config.Config, out, errOut io.Writer) int {
	if err := cfg.WavelogEndpoint(); err != nil {
		fmt.Fprintf(errOut, "station-log: %v\n", err)
		return 2
	}
	w := cfg.Wavelog
	cl, err := wavelog.NewClient(w.URL, w.Key, w.CAFile)
	if err != nil {
		fmt.Fprintf(errOut, "station-log: %v\n", err)
		return 2
	}
	cl.MinGap = 0
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	u, _ := url.Parse(w.URL)
	fmt.Fprintf(out, "Wavelog %s (API v2)\n", u.Host)

	code := 0
	ti, err := cl.Token(ctx)
	switch {
	case err != nil:
		fmt.Fprintf(errOut, "token check failed: %v\n", err)
		var ae *wavelog.APIError
		if errors.As(err, &ae) && ae.Auth {
			return 1 // the token is not accepted; the station call would fail too
		}
		code = 1
	default:
		for _, k := range []string{"name", "owner", "user", "expires_at", "created_at"} {
			if v, ok := ti.Fields[k]; ok {
				fmt.Fprintf(out, "token %s: %s\n", k, v)
			}
		}
		if len(ti.Scopes) == 0 {
			fmt.Fprintf(out, "token scopes: not reported by /api/v2/token (fields: %v)\n", ti.Keys)
		} else {
			fmt.Fprintf(out, "token scopes: %v\n", ti.Scopes)
			for _, need := range []struct{ scope, why string }{
				{"qso:read", "pull"}, {"qso:write", "push"}, {"station:read", "this check"}} {
				if !slices.Contains(ti.Scopes, need.scope) {
					fmt.Fprintf(out, "MISSING scope %s (needed for %s)\n", need.scope, need.why)
				}
			}
		}
	}

	sts, err := cl.Stations(ctx)
	if err != nil {
		fmt.Fprintf(errOut, "station list failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "station locations (%d): set station_profile_id for own_call in [[wavelog.profile]]\n", len(sts))
	for _, s := range sts {
		fmt.Fprintf(out, "  id %-6s callsign %-12s name %s\n", s.ID, s.Callsign, s.Name)
	}
	return code
}
