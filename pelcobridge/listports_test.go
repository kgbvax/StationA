// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"strings"
	"testing"
)

// TestReportPortMarksConfigured pins the -list-ports output shape: the port
// line carries the optional USB detail and the configured port gets the
// "<- configured" annotation (exact-name match, no trim fuzz on the port).
func TestReportPortMarksConfigured(t *testing.T) {
	cases := []struct {
		name, detail, configured, want, notWant string
	}{
		{
			name: "/dev/tty.usbmodem5AF50020681", detail: `  USB 1A86:55D3 "USB-Enhanced-SERIAL CH343" serial=5AF5002068`,
			configured: "/dev/tty.usbmodem5AF50020681",
			want:       `/dev/tty.usbmodem5AF50020681  USB 1A86:55D3 "USB-Enhanced-SERIAL CH343" serial=5AF5002068   <- configured`,
		},
		{
			name: "COM7", detail: `  USB 1A86:55D3 "USB-Enhanced-SERIAL CH343"`,
			configured: "com7", // config case differs — no annotation (exact match only)
			want:       `COM7  USB 1A86:55D3 "USB-Enhanced-SERIAL CH343"`,
			notWant:    "<- configured",
		},
		{
			name: "COM3", detail: "", configured: "COM3",
			want: "COM3   <- configured",
		},
		{
			name: "/dev/tty.Bluetooth-Incoming-Port", detail: "", configured: "",
			want:    "/dev/tty.Bluetooth-Incoming-Port",
			notWant: "<- configured",
		},
	}
	for _, c := range cases {
		got := captureStdout(t, func() { reportPort(c.name, c.detail, c.configured) })
		if got != c.want+"\n" {
			t.Errorf("reportPort(%q,%q,%q) = %q, want %q", c.name, c.detail, c.configured, got, c.want)
		}
		if c.notWant != "" && strings.Contains(got, c.notWant) {
			t.Errorf("reportPort(%q,%q,%q) = %q, must not contain %q", c.name, c.detail, c.configured, got, c.notWant)
		}
	}
}
