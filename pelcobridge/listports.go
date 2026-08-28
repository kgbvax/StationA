// Serial-port enumeration for the -list-ports flag.
//
// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"fmt"
	"os"
	"strings"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

// listSerialPorts prints every serial port the OS exposes, with USB metadata
// when the platform provides it (macOS IOKit / Linux sysfs / Windows setupapi),
// and marks which one the config/flags have selected. Used by -list-ports:
// pick a -port value from the output. Returns an exit code (0 on success).
func listSerialPorts(configured string) int {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		// Fall back to plain enumeration; some platforms have no detailed
		// enumerator but can still list port names.
		names, err2 := serial.GetPortsList()
		if err2 != nil {
			fmt.Fprintf(os.Stderr, "cannot enumerate serial ports: %v\n", err)
			return 1
		}
		for _, n := range names {
			reportPort(n, "", configured)
		}
		return 0
	}
	if len(ports) == 0 {
		fmt.Println("no serial ports found")
		return 0
	}
	for _, p := range ports {
		desc := ""
		if p.IsUSB {
			desc = fmt.Sprintf("  USB %s:%s", p.VID, p.PID)
			if p.Product != "" {
				desc += fmt.Sprintf(" %q", p.Product)
			}
			if p.SerialNumber != "" {
				desc += fmt.Sprintf(" serial=%s", p.SerialNumber)
			}
		}
		reportPort(p.Name, desc, configured)
	}
	return 0
}

// reportPort prints one port line, annotating the configured port.
func reportPort(name, detail, configured string) {
	mark := ""
	if strings.TrimSpace(configured) != "" && name == configured {
		mark = "   <- configured"
	}
	fmt.Printf("%s%s%s\n", name, detail, mark)
}
