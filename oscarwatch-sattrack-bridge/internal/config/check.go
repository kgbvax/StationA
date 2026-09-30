package config

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

// Check validates the effective config the way a real start would and returns
// a redacted summary (one line per facet) plus every problem found. It backs
// `-check`, which deploy.sh runs before restarting the service: a missing
// password or an unresolvable host must fail the deploy, not surface in the
// journal later.
//
// Secrets never appear in the summary — only where they came from.
// resolve=false skips DNS lookups (unit tests); deploy.sh resolves.
func Check(cfg Config, resolve bool) ([]string, error) {
	var problems []string
	_, obsLine := cfg.Observer()
	lines := []string{
		fmt.Sprintf("broker   %s", cfg.MQTT.Broker),
		fmt.Sprintf("user     %s", cfg.MQTT.User),
		fmt.Sprintf("slot     %s/%s/%s", cfg.MQTT.Site, cfg.Slot.Station, cfg.Slot.Slot),
		fmt.Sprintf("source   %s (ping %s)", cfg.Source.URL, pingSummary(cfg.Source.PingInterval)),
		fmt.Sprintf("station  %s", obsLine),
	}

	switch {
	case os.Getenv(EnvPrefix+"MQTT_PASSWORD") != "":
		lines = append(lines, "password set (env "+EnvPrefix+"MQTT_PASSWORD)")
	case cfg.MQTT.Password != "":
		lines = append(lines, "password set ([mqtt] password in config.toml)")
	default:
		problems = append(problems, "no MQTT password — set "+EnvPrefix+"MQTT_PASSWORD in the env file")
	}

	hosts := []struct{ what, raw string }{{"broker", cfg.MQTT.Broker}, {"source", cfg.Source.URL}}
	for _, h := range hosts {
		u, err := url.Parse(h.raw)
		if err != nil || u.Hostname() == "" {
			problems = append(problems, fmt.Sprintf("%s %q does not parse as a URL with a host", h.what, h.raw))
			continue
		}
		if !resolve {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, err = net.DefaultResolver.LookupHost(ctx, u.Hostname())
		cancel()
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s host %q does not resolve: %v", h.what, u.Hostname(), err))
		}
	}

	if len(problems) == 0 {
		return lines, nil
	}
	return lines, fmt.Errorf("%d config problem(s): %s", len(problems), strings.Join(problems, "; "))
}

func pingSummary(d time.Duration) string {
	if d == 0 {
		return "off"
	}
	return d.String()
}
