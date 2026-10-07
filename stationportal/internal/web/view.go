package web

import (
	"fmt"
	"strings"
	"time"

	"stationportal/internal/bus"
	"stationportal/internal/inventory"
	"stationportal/internal/probe"
)

// Health is the traffic-light state of one row.
type Health string

const (
	Up      Health = "up"
	Warn    Health = "warn"
	Down    Health = "down"
	Unknown Health = "unknown"
	None    Health = "none" // not monitored
)

// LinkView is one service link with its latest probe.
type LinkView struct {
	inventory.Link
	Health    Health
	Detail    string
	Clickable bool
}

// LinkGroup is one titled block of links.
type LinkGroup struct {
	Name  string
	Links []LinkView
}

// SlotView is one bus address: static inventory merged with live facts.
type SlotView struct {
	inventory.Slot
	Known        bool // listed in the inventory
	Live         bus.Facts
	Title        string // inventory description, else the live model
	Detail       string // live facts the title does not already say
	Status       string
	DeviceOnline string // "yes" / "no" / "—"
	Health       Health
	HealthText   string
}

// SoftwareView is one software component with derived liveness.
type SoftwareView struct {
	inventory.Software
	Health     Health
	HealthText string
}

// Summary counts for the header.
type Summary struct {
	SlotsUp, SlotsTotal, LinksUp, LinksTotal int
}

// Page is everything the template (and /api/inventory) renders.
type Page struct {
	Generated    time.Time
	Host         string
	Revision     string
	RefreshS     int
	Broker       string
	BusConnected bool
	BusSince     string
	LastMessage  string
	Summary      Summary
	Groups       []LinkGroup
	Slots        []SlotView
	Software     []SoftwareView
	Hosts        []inventory.Host
	Resources    []inventory.Resource
}

// slotHealth applies the station's two-layer liveness: the bridge's /status
// LWT AND the snapshot's device_online.
func slotHealth(status string, deviceOnline *bool) (Health, string) {
	switch {
	case status == "":
		return Unknown, "no status"
	case status != "online":
		return Down, "bridge " + status
	case deviceOnline != nil && !*deviceOnline:
		return Warn, "device offline"
	default:
		return Up, "online"
	}
}

func buildPage(inv inventory.Inventory, slots []bus.Slot, probes *probe.Prober) Page {
	var p Page

	// Links, grouped in inventory order.
	groupIdx := map[string]int{}
	for _, l := range inv.Links {
		lv := LinkView{Link: l, Health: Unknown, Detail: "not probed yet"}
		kind, _, _ := inventory.ProbeTarget(l)
		lv.Clickable = kind == "http" || kind == "none"
		if kind == "none" {
			lv.Health, lv.Detail = None, "external"
		} else if r, ok := probes.Result(l.URL); ok {
			lv.Detail = r.Detail
			if r.Up {
				lv.Health = Up
				p.Summary.LinksUp++
			} else {
				lv.Health = Down
			}
		}
		if kind != "none" {
			p.Summary.LinksTotal++
		}
		i, ok := groupIdx[l.Group]
		if !ok {
			i = len(p.Groups)
			groupIdx[l.Group] = i
			p.Groups = append(p.Groups, LinkGroup{Name: l.Group})
		}
		p.Groups[i].Links = append(p.Groups[i].Links, lv)
	}

	// Slots: inventory order first, then live addresses the inventory lacks.
	live := map[string]bus.Slot{}
	for _, s := range slots {
		live[s.Address] = s
	}
	health := map[string]Health{}
	add := func(st inventory.Slot, known bool) {
		v := SlotView{Slot: st, Known: known, DeviceOnline: "—"}
		ls, seen := live[st.Address]
		if seen {
			v.Live = bus.MetaFacts(ls.Meta)
			v.Status = ls.Status
			if ls.DeviceOnline != nil {
				v.DeviceOnline = map[bool]string{true: "yes", false: "no"}[*ls.DeviceOnline]
			}
		}
		v.Title, v.Detail = deviceText(st, v.Live, known)
		v.Health, v.HealthText = slotHealth(ls.Status, ls.DeviceOnline)
		health[st.Address] = v.Health
		p.Summary.SlotsTotal++
		if v.Health == Up {
			p.Summary.SlotsUp++
		}
		p.Slots = append(p.Slots, v)
	}
	inInv := map[string]bool{}
	for _, st := range inv.Slots {
		inInv[st.Address] = true
		add(st, true)
	}
	for _, s := range slots {
		if !inInv[s.Address] {
			add(inventory.Slot{Address: s.Address}, false)
		}
	}

	// Software liveness: owned slots, else the named link's probe.
	linkHealth := map[string]LinkView{}
	for _, g := range p.Groups {
		for _, l := range g.Links {
			linkHealth[l.Name] = l
		}
	}
	for _, sw := range inv.Software {
		v := SoftwareView{Software: sw, Health: None, HealthText: "not monitored"}
		switch {
		case sw.Name == "stationportal":
			v.Health, v.HealthText = Up, "serving this page"
		case len(sw.Slots) > 0:
			v.Health, v.HealthText = aggregate(sw.Slots, health)
		case sw.Link != "":
			l := linkHealth[sw.Link]
			v.Health, v.HealthText = l.Health, l.Detail
		}
		p.Software = append(p.Software, v)
	}

	p.Hosts = inv.Hosts
	p.Resources = inv.Resources
	return p
}

// deviceText composes the device cell: the inventory's description as the
// title (it says what the device is for), then only the live /meta facts the
// title does not already contain.
func deviceText(st inventory.Slot, f bus.Facts, known bool) (title, detail string) {
	title = st.Device
	if title == "" {
		title = f.Model
	}
	var parts []string
	for _, v := range []string{f.Manufacturer, f.Model} {
		if v != "" && !strings.Contains(strings.ToLower(title), strings.ToLower(v)) {
			parts = append(parts, v)
		}
	}
	if f.Serial != "" {
		parts = append(parts, "serial "+f.Serial)
	}
	if f.Firmware != "" {
		parts = append(parts, "fw "+f.Firmware)
	}
	if st.Note != "" {
		parts = append(parts, st.Note)
	}
	if !known {
		parts = append(parts, "not in the inventory")
	}
	return title, strings.Join(parts, " · ")
}

// aggregate folds slot healths: any down -> down, any warn -> warn, all up
// -> up, nothing seen -> unknown.
func aggregate(addresses []string, health map[string]Health) (Health, string) {
	var up, warn, down, unknown int
	for _, a := range addresses {
		switch health[a] {
		case Up:
			up++
		case Warn:
			warn++
		case Down:
			down++
		default:
			unknown++
		}
	}
	n := len(addresses)
	if n == 1 {
		switch {
		case down > 0:
			return Down, "slot offline"
		case warn > 0:
			return Warn, "device offline"
		}
	}
	switch {
	case down > 0:
		return Down, fmt.Sprintf("%d/%d slot(s) offline", down, n)
	case warn > 0:
		return Warn, fmt.Sprintf("%d/%d device(s) offline", warn, n)
	case up == n:
		return Up, map[bool]string{true: "slot online", false: "all slots online"}[n == 1]
	case up == 0:
		return Unknown, "no status on the bus"
	default:
		return Warn, fmt.Sprintf("%d/%d slot(s) without status", unknown, n)
	}
}

// Age renders a duration coarsely for humans.
func Age(d time.Duration) string {
	switch {
	case d < 0:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	default:
		return fmt.Sprintf("%d d", int(d.Hours()/24))
	}
}
