package serialio

import (
	"bytes"
	"testing"
)

func TestWritePorts(t *testing.T) {
	var b bytes.Buffer

	WritePorts(&b, nil)
	if got := b.String(); got != "no serial ports found\n" {
		t.Errorf("empty = %q", got)
	}

	b.Reset()
	WritePorts(&b, []PortInfo{
		{Name: "/dev/cu.Bluetooth-Incoming-Port"},
		{Name: "COM3", IsUSB: true, VID: "0403", PID: "6015",
			Product: "FTDI Dual RS232-HS", SerialNumber: "A1B2C3"},
	})
	out := b.String()
	want := []string{
		"/dev/cu.Bluetooth-Incoming-Port\n", // non-USB: name only
		"COM3  USB 0403:6015",               // vid:pid
		`"FTDI Dual RS232-HS"`,              // product
		"sn=A1B2C3",                         // serial number
	}
	for _, w := range want {
		if !bytes.Contains([]byte(out), []byte(w)) {
			t.Errorf("output %q missing %q", out, w)
		}
	}
}
