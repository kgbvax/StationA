package transport

import (
	"fmt"

	"go.bug.st/serial"
)

func OpenSerial(portName string, baud int) (Client, error) {
	mode := &serial.Mode{BaudRate: baud}
	port, err := serial.Open(portName, mode)
	if err != nil {
		return nil, fmt.Errorf("open serial port %s: %w", portName, err)
	}
	return NewDevice(port), nil
}
