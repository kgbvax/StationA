//go:build windows

package programs

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// stillActive is the GetExitCodeProcess value of a running process.
const stillActive = 259

// Waits of End: first for the program to close after WM_CLOSE, then for the
// exit after TerminateProcess.
const (
	closeWait     = 10 * time.Second
	terminateWait = 5 * time.Second
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procPostMessageW       = user32.NewProc("PostMessageW")
	procIsWindowVisible    = user32.NewProc("IsWindowVisible")
	procPowerCreateRequest = kernel32.NewProc("PowerCreateRequest")
	procPowerSetRequest    = kernel32.NewProc("PowerSetRequest")
	procPowerClearRequest  = kernel32.NewProc("PowerClearRequest")
)

// OSProc is the Windows process table.
type OSProc struct{}

// Find lists the processes with this image name (Toolhelp snapshot).
func (OSProc) Find(image string) ([]Process, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("CreateToolhelp32Snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)
	var out []Process
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if !strings.EqualFold(windows.UTF16ToString(e.ExeFile[:]), image) {
			continue
		}
		st, serr := startTime(e.ProcessID)
		if serr != nil {
			continue // exited since the snapshot, or no access
		}
		out = append(out, Process{PID: int(e.ProcessID), Start: st})
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, fmt.Errorf("Process32Next: %w", err)
	}
	return out, nil
}

// Alive reports whether p runs with the same creation time.
func (OSProc) Alive(p Process) bool {
	st, err := startTime(uint32(p.PID))
	return err == nil && st == p.Start
}

// Start runs the program with CREATE_BREAKAWAY_FROM_JOB, so that it is not in
// the Task Scheduler job of the agent and survives an end of the agent task.
// Standard handles go to NUL.
func (OSProc) Start(s Spec) (Process, error) {
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return Process{}, err
	}
	defer null.Close()
	p, err := os.StartProcess(s.Exe, append([]string{s.Exe}, s.Args...), &os.ProcAttr{
		Dir:   s.Workdir,
		Files: []*os.File{null, null, null},
		Sys:   &syscall.SysProcAttr{CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB},
	})
	if err != nil {
		return Process{}, err
	}
	// Read the creation time while os.Process still holds a handle, so the
	// PID cannot be reused in between.
	st, err := startTime(uint32(p.Pid))
	pid := p.Pid
	_ = p.Release()
	if err != nil {
		return Process{}, fmt.Errorf("started PID %d, creation time: %w", pid, err)
	}
	return Process{PID: pid, Start: st}, nil
}

// End asks the program to close (WM_CLOSE to its visible top-level windows),
// so that it can clean up (WSJT-X removes its lock file), and terminates it if
// it still runs after closeWait. The creation time is checked first, so a
// reused PID is never ended.
func (OSProc) End(p Process) error {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(p.PID))
	if err != nil {
		return fmt.Errorf("OpenProcess %d: %w", p.PID, err)
	}
	defer windows.CloseHandle(h)
	st, err := handleStartTime(h)
	if err != nil {
		return err
	}
	if st != p.Start {
		return fmt.Errorf("PID %d is another process now", p.PID)
	}
	if postClose(uint32(p.PID)) > 0 {
		if ev, _ := windows.WaitForSingleObject(h, uint32(closeWait/time.Millisecond)); ev == windows.WAIT_OBJECT_0 {
			return nil
		}
	}
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("TerminateProcess %d: %w", p.PID, err)
	}
	if ev, _ := windows.WaitForSingleObject(h, uint32(terminateWait/time.Millisecond)); ev != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("PID %d did not exit after TerminateProcess", p.PID)
	}
	return nil
}

// startTime returns the creation time (FILETIME, 100 ns units) of a running
// process.
func startTime(pid uint32) (uint64, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	return handleStartTime(h)
}

func handleStartTime(h windows.Handle) (uint64, error) {
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return 0, fmt.Errorf("GetExitCodeProcess: %w", err)
	}
	if code != stillActive {
		return 0, errors.New("process exited")
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return 0, fmt.Errorf("GetProcessTimes: %w", err)
	}
	return uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime), nil
}

// closeTarget is the window owner that the EnumWindows callback looks for.
// It is guarded by closeMu, so no Go pointer passes through the callback's
// lparam.
var closeTarget struct {
	pid    uint32
	posted int
}

var (
	closeMu sync.Mutex
	// enumCB is created once: windows.NewCallback slots are never freed.
	enumCB = windows.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		c := &closeTarget
		var pid uint32
		if _, err := windows.GetWindowThreadProcessId(windows.HWND(hwnd), &pid); err != nil || pid != c.pid {
			return 1
		}
		if v, _, _ := procIsWindowVisible.Call(hwnd); v == 0 {
			return 1
		}
		const wmClose = 0x0010
		if r, _, _ := procPostMessageW.Call(hwnd, wmClose, 0, 0); r != 0 {
			c.posted++
		}
		return 1 // continue
	})
)

// postClose posts WM_CLOSE to the visible top-level windows of pid and
// returns how many it reached.
func postClose(pid uint32) int {
	closeMu.Lock()
	defer closeMu.Unlock()
	closeTarget.pid, closeTarget.posted = pid, 0
	_ = windows.EnumWindows(enumCB, nil)
	return closeTarget.posted
}

// SystemPowerRequest is a kernel32 power request of type
// PowerRequestSystemRequired: while set, Windows does not put the PC to sleep
// for lack of user input. A power request is not bound to a thread (unlike
// SetThreadExecutionState), so it works from any goroutine.
type SystemPowerRequest struct {
	reason string
	mu     sync.Mutex
	h      windows.Handle
}

// NewSystemPowerRequest returns a power request with this reason text (shown
// by `powercfg /requests`).
func NewSystemPowerRequest(reason string) *SystemPowerRequest {
	return &SystemPowerRequest{reason: reason}
}

// reasonContext is the Win32 REASON_CONTEXT with a simple reason string.
type reasonContext struct {
	Version uint32
	Flags   uint32
	Reason  *uint16
	_       [2]uintptr // rest of the Detailed union member
}

const (
	powerRequestContextVersion      = 0
	powerRequestContextSimpleString = 0x1
	powerRequestSystemRequired      = 1
)

func (r *SystemPowerRequest) handle() (windows.Handle, error) {
	if r.h != 0 {
		return r.h, nil
	}
	s, err := windows.UTF16PtrFromString(r.reason)
	if err != nil {
		return 0, err
	}
	rc := reasonContext{Version: powerRequestContextVersion, Flags: powerRequestContextSimpleString, Reason: s}
	h, _, e := procPowerCreateRequest.Call(uintptr(unsafe.Pointer(&rc)))
	if windows.Handle(h) == windows.InvalidHandle || h == 0 {
		return 0, fmt.Errorf("PowerCreateRequest: %v", e)
	}
	r.h = windows.Handle(h)
	return r.h, nil
}

// Set activates the request.
func (r *SystemPowerRequest) Set() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, err := r.handle()
	if err != nil {
		return err
	}
	if ok, _, e := procPowerSetRequest.Call(uintptr(h), powerRequestSystemRequired); ok == 0 {
		return fmt.Errorf("PowerSetRequest: %v", e)
	}
	return nil
}

// Clear ends the request.
func (r *SystemPowerRequest) Clear() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, err := r.handle()
	if err != nil {
		return err
	}
	if ok, _, e := procPowerClearRequest.Call(uintptr(h), powerRequestSystemRequired); ok == 0 {
		return fmt.Errorf("PowerClearRequest: %v", e)
	}
	return nil
}
