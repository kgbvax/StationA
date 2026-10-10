// Package winpower is the Windows platform layer of winpc-power-bridge: the
// user-idle time of the interactive session and system suspend (S3 sleep).
//
// The Windows implementation is in winpower_windows.go. On other platforms
// (the developer Mac) every function returns ErrUnsupported, so the module
// builds and tests everywhere.
package winpower

import (
	"errors"
	"time"
)

// ErrUnsupported is returned on platforms without the Windows power API.
var ErrUnsupported = errors.New("unsupported on this platform")

// idleFromTicks converts the current tick count (GetTickCount64, ms since
// boot) and the tick count of the last input event (LASTINPUTINFO.dwTime, a
// 32-bit tick count) into the idle duration. dwTime wraps every ~49.7 days, so
// the difference is taken in 32-bit arithmetic: the low 32 bits of now minus
// dwTime is correct across one wrap. (Idle times above ~49.7 days alias, which
// does not matter for a 30-minute guard.)
func idleFromTicks(now uint64, lastInput uint32) time.Duration {
	return time.Duration(uint32(now)-lastInput) * time.Millisecond
}
