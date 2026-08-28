package serialio

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"go.bug.st/serial"
	en "go.bug.st/serial/enumerator"
)

// PortInfo is one host serial port with whatever USB identity the OS exposes —
// enough to tell the head's RS-485 adapter apart from everything else plugged
// into the machine (bench reality: several FTDIs, a couple of CH340s).
type PortInfo struct {
	Name         string
	IsUSB        bool
	VID, PID     string
	Product      string
	SerialNumber string
}

// EnumeratePorts lists serial ports with details. Falls back to a plain
// name-only listing if the detailed enumerator fails or is not implemented
// on that OS; the Name field is then the whole truth. No active USB probing
// is requested (it can disturb devices) — passive fields only, so Product
// may be empty while VID:PID is present.
func EnumeratePorts() ([]PortInfo, error) {
	details, err := en.GetDetailedPortsList()
	if err != nil || details == nil {
		names, err2 := serial.GetPortsList()
		if err2 != nil {
			if err != nil {
				return nil, err
			}
			return nil, err2
		}
		out := make([]PortInfo, 0, len(names))
		for _, n := range names {
			out = append(out, PortInfo{Name: n})
		}
		return out, nil
	}
	out := make([]PortInfo, 0, len(details))
	for _, d := range details {
		if d == nil {
			continue
		}
		out = append(out, PortInfo{
			Name:         d.Name,
			IsUSB:        d.IsUSB,
			VID:          d.VID,
			PID:          d.PID,
			Product:      d.Product,
			SerialNumber: d.SerialNumber,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// WritePorts renders the enumeration for a human: one line per port, USB
// identity appended when known. Used by -list-ports in both binaries.
func WritePorts(w io.Writer, ports []PortInfo) {
	if len(ports) == 0 {
		fmt.Fprintln(w, "no serial ports found")
		return
	}
	for _, p := range ports {
		line := p.Name
		var attrs []string
		if p.IsUSB {
			attrs = append(attrs, p.VID+":"+p.PID)
			if p.Product != "" {
				attrs = append(attrs, `"`+p.Product+`"`)
			}
			if p.SerialNumber != "" {
				attrs = append(attrs, "sn="+p.SerialNumber)
			}
		}
		if len(attrs) > 0 {
			line += "  USB " + strings.Join(attrs, " ")
		}
		fmt.Fprintln(w, line)
	}
}
