//go:build !windows

package winpower

import "time"

// UserIdle returns ErrUnsupported on non-Windows platforms.
func UserIdle() (time.Duration, error) { return 0, ErrUnsupported }

// CanSuspend returns ErrUnsupported on non-Windows platforms.
func CanSuspend() error { return ErrUnsupported }

// Suspend returns ErrUnsupported on non-Windows platforms.
func Suspend() error { return ErrUnsupported }
