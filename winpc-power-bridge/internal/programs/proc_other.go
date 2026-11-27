//go:build !windows

package programs

// OSProc is the process table of the platform. Off Windows every call fails
// with ErrUnsupported.
type OSProc struct{}

// Find returns ErrUnsupported on non-Windows platforms.
func (OSProc) Find(string) ([]Process, error) { return nil, ErrUnsupported }

// Alive is false on non-Windows platforms.
func (OSProc) Alive(Process) bool { return false }

// Start returns ErrUnsupported on non-Windows platforms.
func (OSProc) Start(Spec) (Process, error) { return Process{}, ErrUnsupported }

// End returns ErrUnsupported on non-Windows platforms.
func (OSProc) End(Process) error { return ErrUnsupported }

// SystemPowerRequest is the system power request. Off Windows it fails with
// ErrUnsupported.
type SystemPowerRequest struct{}

// NewSystemPowerRequest returns the platform power request.
func NewSystemPowerRequest(string) *SystemPowerRequest { return &SystemPowerRequest{} }

// Set returns ErrUnsupported on non-Windows platforms.
func (*SystemPowerRequest) Set() error { return ErrUnsupported }

// Clear returns ErrUnsupported on non-Windows platforms.
func (*SystemPowerRequest) Clear() error { return ErrUnsupported }
