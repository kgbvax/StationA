//go:build !windows

package desktop

import "unsafe"

// setWindowIcon is a no-op: macOS and Linux take the icon from the app bundle
// or desktop entry, not from the window.
func setWindowIcon(unsafe.Pointer) {}
