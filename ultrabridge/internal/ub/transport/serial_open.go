package transport

import (
	"errors"
	"fmt"
	"time"

	"go.bug.st/serial"
)

// errReadTimeout reports that the port is alive but the device sent nothing
// within the read window. It is a *device silence* sentinel, distinct from a
// port-level fault (EIO, ENXIO, PortClosed, …): silence must not trigger the
// reopen self-heal (a healthy port would be cycled every poll tick), only
// mark the device offline.
var errReadTimeout = errors.New("read timeout")

// readWindow bounds each individual Read call. Without it the underlying
// serial Read blocks indefinitely (the driver default is NoTimeout), and a
// controller that goes silent after a link fault wedges Exchange inside a
// read forever — the poll loop freezes, state goes stale (a stuck
// `moving: true` outlives the outage), and nothing recovers until a process
// restart. 500 ms keeps each read bounded so readOnePacket's exchange
// deadline can actually fire.
const readWindow = 500 * time.Millisecond

// timeoutMappedPort makes device silence observable. The unix backend
// signals a read-window timeout by returning (0, nil) — indistinguishable
// from a well-behaved short read for callers that don't know the driver —
// so this wrapper converts that case into errReadTimeout. Real port errors
// (PortClosed, EIO, …) pass through untouched as port faults.
type timeoutMappedPort struct {
	port serial.Port
}

func (p timeoutMappedPort) Read(b []byte) (int, error) {
	n, err := p.port.Read(b)
	if err == nil && n == 0 {
		return 0, errReadTimeout
	}
	return n, err
}

func (p timeoutMappedPort) Write(b []byte) (int, error) { return p.port.Write(b) }
func (p timeoutMappedPort) Close() error                { return p.port.Close() }

// OpenSerial opens the serial port and returns a Device that can re-open it
// after the underlying USB-serial adapter drops and the kernel re-enumerates
// it. The opener captures the (stable) by-id port path plus baud, so a reopen
// after a disconnect re-resolves the symlink to the freshly attached tty —
// even though the kernel may have assigned it a different tty name. See Device
// for the self-heal semantics.
func OpenSerial(portName string, baud int) (Client, error) {
	opener := func() (byteReadWriteCloser, error) {
		mode := &serial.Mode{BaudRate: baud}
		port, err := serial.Open(portName, mode)
		if err != nil {
			return nil, err
		}
		if err := port.SetReadTimeout(readWindow); err != nil {
			_ = port.Close()
			return nil, fmt.Errorf("set read timeout on %s: %w", portName, err)
		}
		// Wrap so device silence surfaces as errReadTimeout (a distinct,
		// non-reopen condition) instead of a benign (0, nil) that would
		// wedge the exchange.
		return timeoutMappedPort{port: port}, nil
	}
	rw, err := opener()
	if err != nil {
		return nil, fmt.Errorf("open serial port %s: %w", portName, err)
	}
	return NewDevice(rw, opener), nil
}
