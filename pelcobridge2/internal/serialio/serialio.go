// Package serialio abstracts the byte transport between the engine and the
// rotator head. Transport is deliberately minimal (Read/Write/Close) so the
// engine can run against a real serial port, a loopback pair wired to the
// in-process sim head, or a test double.
package serialio

// Transport is the engine's view of the serial link. Read blocks until data or
// an error, exactly like a blocking serial read; a read timeout returns
// (0, nil) rather than an error so an idle line is not an error.
type Transport interface {
	Read(p []byte) (int, error)
	Write(b []byte) error
	Close() error
}
