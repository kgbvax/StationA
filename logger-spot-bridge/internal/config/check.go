package config

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"time"
)

// Check validates the effective config the way a real start would and returns
// a redacted summary (one line per facet) plus every problem found. It is the
// `-check` mode behind deploy.sh's post-update verification: an update that
// left the bridge with an unresolvable broker or a missing password must fail
// the deploy, not surface as "no such host" in bridge.log hours later.
//
// Secrets never appear in the summary — only where they came from.
//
// resolve=false skips the DNS lookup of the broker host (offline checks, unit
// tests); resolve=true performs it with a short timeout, as deploy.sh does.
func Check(cfg Config, resolve bool) ([]string, error) {
	var problems []string
	lines := []string{
		fmt.Sprintf("broker   %s", cfg.MQTT.Broker),
		fmt.Sprintf("user     %s", cfg.MQTT.User),
		fmt.Sprintf("slot     %s", schemaSlot(cfg)),
		fmt.Sprintf("locator  %s", orUnset(cfg.StationLocator)),
		fmt.Sprintf("qrz      %s", qrzSummary(cfg)),
	}

	switch passwordSource(cfg) {
	case passwordEnv:
		lines = append(lines, "password set (env LOGGER_SPOT_BRIDGE_MQTT_PASSWORD)")
	case passwordConfig:
		lines = append(lines, "password set ([mqtt] password in config.toml)")
	default:
		problems = append(problems,
			"no MQTT password — set LOGGER_SPOT_BRIDGE_MQTT_PASSWORD in start-bridge.cmd")
	}

	host, err := brokerHost(cfg.MQTT.Broker)
	if err != nil {
		problems = append(problems, err.Error())
	} else if resolve {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := net.DefaultResolver.LookupHost(ctx, host); err != nil {
			problems = append(problems, fmt.Sprintf("broker host %q does not resolve: %v", host, err))
		}
	}

	if len(problems) == 0 {
		return lines, nil
	}
	return lines, fmt.Errorf("%d config problem(s): %s", len(problems), joinProblems(problems))
}

type passwordSourceKind int

const (
	passwordMissing passwordSourceKind = iota
	passwordEnv
	passwordConfig
)

func passwordSource(cfg Config) passwordSourceKind {
	// Mirrors applyEnv: the env var wins over the (discouraged) TOML field.
	if os.Getenv("LOGGER_SPOT_BRIDGE_MQTT_PASSWORD") != "" {
		return passwordEnv
	}
	if cfg.MQTT.Password != "" {
		return passwordConfig
	}
	return passwordMissing
}

func brokerHost(broker string) (string, error) {
	u, err := url.Parse(broker)
	if err != nil {
		return "", fmt.Errorf("broker %q does not parse as a URL: %v", broker, err)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("broker %q has no host", broker)
	}
	return u.Hostname(), nil
}

func schemaSlot(cfg Config) string {
	return cfg.MQTT.Site + "/" + cfg.Slot.Station + "/" + cfg.Slot.Slot
}

func qrzSummary(cfg Config) string {
	if !cfg.QRZ.Enabled {
		return "off"
	}
	return "on (user " + cfg.QRZ.Username + ")"
}

func orUnset(s string) string {
	if s == "" {
		return "(unset — logger bearings only)"
	}
	return s
}

func joinProblems(ps []string) string {
	out := ""
	for i, p := range ps {
		if i > 0 {
			out += "; "
		}
		out += p
	}
	return out
}
