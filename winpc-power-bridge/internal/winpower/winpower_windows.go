//go:build windows

package winpower

import (
	"errors"
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	powrprof = windows.NewLazySystemDLL("powrprof.dll")

	procGetLastInputInfo     = user32.NewProc("GetLastInputInfo")
	procGetTickCount64       = kernel32.NewProc("GetTickCount64")
	procSetSuspendState      = powrprof.NewProc("SetSuspendState")
	procIsPwrSuspendAllowed  = powrprof.NewProc("IsPwrSuspendAllowed")
	errSuspendNotAllowed     = errors.New("system reports no S1-S3 sleep state (IsPwrSuspendAllowed=false; Modern Standby machine or sleep disabled?)")
	errLastInputInfoFailed   = errors.New("GetLastInputInfo failed")
	errSetSuspendStateFailed = errors.New("SetSuspendState failed")
)

// lastInputInfo is the Win32 LASTINPUTINFO struct.
type lastInputInfo struct {
	cbSize uint32
	dwTime uint32
}

// UserIdle returns the time since the last keyboard or mouse input in the
// calling process's session. GetLastInputInfo only sees the session the
// process runs in, so the agent must run in the interactive user session (a
// logon Task Scheduler task), never as a service in session 0.
func UserIdle() (time.Duration, error) {
	info := lastInputInfo{cbSize: uint32(unsafe.Sizeof(lastInputInfo{}))}
	r1, _, e1 := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&info)))
	if r1 == 0 {
		return 0, fmt.Errorf("%w: %v", errLastInputInfoFailed, e1)
	}
	now, _, _ := procGetTickCount64.Call()
	return idleFromTicks(uint64(now), info.dwTime), nil
}

// CanSuspend reports whether the system supports a sleep state that
// SetSuspendState can enter. Checked before the agent goes offline on the bus,
// so an unsupported machine refuses without a /status flap.
func CanSuspend() error {
	if err := procIsPwrSuspendAllowed.Find(); err != nil {
		return err
	}
	r1, _, _ := procIsPwrSuspendAllowed.Call()
	if r1&0xff == 0 { // BOOLEAN (one byte)
		return errSuspendNotAllowed
	}
	return nil
}

// Suspend enables SeShutdownPrivilege on the process token and calls
// SetSuspendState(FALSE, FALSE, FALSE): sleep (not hibernate), not forced,
// wake events enabled (Wake-on-LAN must stay armed). The call returns when the
// system resumes, or at once with an error if the suspend was refused.
func Suspend() error {
	if err := enableShutdownPrivilege(); err != nil {
		return fmt.Errorf("enable SeShutdownPrivilege: %w", err)
	}
	r1, _, e1 := procSetSuspendState.Call(0, 0, 0)
	if r1 == 0 {
		return fmt.Errorf("%w: %v", errSetSuspendStateFailed, e1)
	}
	return nil
}

// enableShutdownPrivilege enables SE_SHUTDOWN_NAME on the process token. A
// standard (non-elevated) user holds this privilege, disabled by default.
func enableShutdownPrivilege() error {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
		return fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer token.Close()

	name, err := windows.UTF16PtrFromString("SeShutdownPrivilege")
	if err != nil {
		return err
	}
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, name, &luid); err != nil {
		return fmt.Errorf("LookupPrivilegeValue: %w", err)
	}
	tp := windows.Tokenprivileges{
		PrivilegeCount: 1,
		Privileges: [1]windows.LUIDAndAttributes{{
			Luid:       luid,
			Attributes: windows.SE_PRIVILEGE_ENABLED,
		}},
	}
	// AdjustTokenPrivileges succeeds with ERROR_NOT_ALL_ASSIGNED when the
	// token lacks the privilege; that case surfaces as a SetSuspendState
	// failure, which carries its own error.
	if err := windows.AdjustTokenPrivileges(token, false, &tp, 0, nil, nil); err != nil {
		return fmt.Errorf("AdjustTokenPrivileges: %w", err)
	}
	return nil
}
