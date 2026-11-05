package desktop

import (
	"syscall"
	"unsafe"
)

// setWindowIcon gives the window the app icon for the title bar and the
// taskbar (glaze registers its window class without one). Best effort: a
// failure leaves the default icon.
func setWindowIcon(hwnd unsafe.Pointer) {
	if hwnd == nil || len(iconPNG) == 0 {
		return
	}
	user32 := syscall.NewLazyDLL("user32.dll")
	create := user32.NewProc("CreateIconFromResourceEx")
	send := user32.NewProc("SendMessageW")
	const (
		wmSetIcon  = 0x0080
		iconSmall  = 0
		iconBig    = 1
		resVersion = 0x00030000
	)
	for _, k := range []struct{ kind, size uintptr }{{iconBig, 48}, {iconSmall, 16}} {
		// A PNG is accepted as icon bits on Windows Vista and later.
		h, _, _ := create.Call(uintptr(unsafe.Pointer(&iconPNG[0])), uintptr(len(iconPNG)), 1, resVersion, k.size, k.size, 0)
		if h != 0 {
			send.Call(uintptr(hwnd), wmSetIcon, k.kind, h)
		}
	}
}
