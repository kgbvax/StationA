package transport

import (
	"fmt"

	"go.bug.st/serial"
)

// OpenSerial opens the serial port and returns a Device that can re-open it
// after the underlying USB-serial adapter drops and the kernel re-enumerates
// it. The opener captures the (stable) by-id port path plus baud, so a reopen
// after a disconnect re-resolves the symlink to the freshly attached tty —
// even though the kernel may have assigned it a different tty name. See Device
// for the self-heal semantics.
func OpenSerial(portName string, baud int) (Client, error) {
	opener := func() (byteReadWriteCloser, error) {
		mode := &serial.Mode{BaudRate: baud}
		return serial.Open(portName, mode)
	}
	rw, err := opener()
	if err != nil {
		return nil, fmt.Errorf("open serial port %s: %w", portName, err)
	}
	return NewDevice(rw, opener), nil
}
