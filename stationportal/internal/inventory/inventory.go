// Package inventory holds the static half of the landing page: service links,
// hosts, the slot -> component map, software components and passive resources.
// The live half (device facts from /meta, liveness from /status and
// /state.device_online) comes from the bus; see internal/bus.
//
// The default inventory is inventory.toml, built into the binary, so it is
// versioned with the repo and a redeploy updates it. A device-side override
// file (config `inventory`) replaces it wholesale.
package inventory

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

//go:embed inventory.toml
var defaultTOML []byte

// Link is one service the landing page links to and probes.
type Link struct {
	Group string `toml:"group"`
	Name  string `toml:"name"`
	URL   string `toml:"url"`
	Host  string `toml:"host"`
	Note  string `toml:"note"`
	// Probe: "" = derive from the URL scheme (http/https -> HTTP GET,
	// mqtt/tcp -> TCP connect), "none" = do not probe.
	Probe string `toml:"probe"`
}

// Host is one computer in the station network.
type Host struct {
	Name     string `toml:"name"`
	Addr     string `toml:"addr"`
	Hardware string `toml:"hardware"`
	OS       string `toml:"os"`
	Storage  string `toml:"storage"`
	Role     string `toml:"role"`
}

// Slot maps a bus address to the component that owns it and the physical
// device behind it. The live /meta wins over Device where it says more.
type Slot struct {
	Address   string `toml:"address"`
	Component string `toml:"component"`
	Device    string `toml:"device"`
	Link      string `toml:"link"`
	Host      string `toml:"host"`
	Note      string `toml:"note"`
}

// Software is one software component (repo component or external program).
type Software struct {
	Name  string   `toml:"name"`
	Path  string   `toml:"path"`
	Kind  string   `toml:"kind"`
	Host  string   `toml:"host"`
	Unit  string   `toml:"unit"`
	Ports string   `toml:"ports"`
	Slots []string `toml:"slots"`
	// Link names a [[link]] whose probe stands in for liveness when the
	// component owns no slot (UIs, the broker, external programs).
	Link string `toml:"link"`
	Note string `toml:"note"`
}

// Resource is a passive piece of hardware with no bus presence.
type Resource struct {
	Name string `toml:"name"`
	Kind string `toml:"kind"`
	Note string `toml:"note"`
}

// Inventory is the whole static inventory.
type Inventory struct {
	Links     []Link     `toml:"link"`
	Hosts     []Host     `toml:"host"`
	Slots     []Slot     `toml:"slot"`
	Software  []Software `toml:"software"`
	Resources []Resource `toml:"resource"`
}

// Default returns the inventory built into the binary.
func Default() (Inventory, error) { return Parse(defaultTOML) }

// Load returns the override file at path, or the built-in inventory when
// path is empty.
func Load(path string) (Inventory, error) {
	if path == "" {
		return Default()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Inventory{}, err
	}
	return Parse(b)
}

// Parse decodes and validates an inventory document.
func Parse(b []byte) (Inventory, error) {
	var inv Inventory
	// Strict: a key in the wrong table (or a typo) is an error, not a silent
	// no-op — the inventory is hand-edited.
	if err := toml.NewDecoder(bytes.NewReader(b)).DisallowUnknownFields().Decode(&inv); err != nil {
		return Inventory{}, fmt.Errorf("parse inventory: %w", err)
	}
	return inv, inv.Validate()
}

// Validate rejects entries the page cannot render or probe sensibly.
func (inv Inventory) Validate() error {
	var errs []error
	for i, l := range inv.Links {
		if l.Name == "" {
			errs = append(errs, fmt.Errorf("link %d: empty name", i))
		}
		if _, _, err := ProbeTarget(l); err != nil {
			errs = append(errs, fmt.Errorf("link %q: %w", l.Name, err))
		}
	}
	seen := map[string]bool{}
	for _, s := range inv.Slots {
		if s.Address == "" || strings.Count(s.Address, "/") < 1 {
			errs = append(errs, fmt.Errorf("slot %q: address must be <site>/…", s.Address))
		}
		if seen[s.Address] {
			errs = append(errs, fmt.Errorf("slot %q: duplicate address", s.Address))
		}
		seen[s.Address] = true
	}
	for i, h := range inv.Hosts {
		if h.Name == "" {
			errs = append(errs, fmt.Errorf("host %d: empty name", i))
		}
	}
	links := map[string]bool{}
	for _, l := range inv.Links {
		links[l.Name] = true
	}
	for i, s := range inv.Software {
		if s.Name == "" {
			errs = append(errs, fmt.Errorf("software %d: empty name", i))
		}
		if s.Link != "" && !links[s.Link] {
			errs = append(errs, fmt.Errorf("software %q: link %q is not a [[link]] name", s.Name, s.Link))
		}
	}
	return errors.Join(errs...)
}

// ProbeTarget returns how a link is probed: kind "http" with the URL, kind
// "tcp" with host:port, or kind "none".
func ProbeTarget(l Link) (kind, target string, err error) {
	if l.Probe == "none" {
		return "none", "", nil
	}
	u, err := url.Parse(l.URL)
	if err != nil || u.Host == "" {
		return "", "", fmt.Errorf("url %q: not an absolute URL", l.URL)
	}
	switch u.Scheme {
	case "http", "https":
		return "http", l.URL, nil
	case "mqtt", "tcp", "ws", "wss":
		host := u.Host
		if u.Port() == "" {
			switch u.Scheme {
			case "ws":
				host += ":80"
			case "wss":
				host += ":443"
			default:
				return "", "", fmt.Errorf("url %q: tcp probe needs a port", l.URL)
			}
		}
		return "tcp", host, nil
	default:
		return "", "", fmt.Errorf("url %q: scheme %q has no probe (set probe = \"none\")", l.URL, u.Scheme)
	}
}
