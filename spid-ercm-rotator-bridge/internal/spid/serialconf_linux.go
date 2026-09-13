// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux

package spid

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// configurePort puts the tty into raw 8N1 mode at the configured baud with a
// 1 s read timeout (VMIN=0, VTIME=10), stdlib-only — the bridge keeps plain
// file I/O on the /dev/serial/by-id path with no third-party serial library.
// Runs on every open/reopen; shari (linux/arm64) is the deploy target.
func configurePort(f *os.File, baud int) error {
	var tio syscall.Termios
	if err := ioctlPtr(f.Fd(), syscall.TCGETS, unsafe.Pointer(&tio)); err != nil {
		return fmt.Errorf("tcgets: %w", err)
	}

	// Raw mode (cf. cfmakeraw): the Rot1Prog packet bytes must pass through
	// untouched — no CR/NL translation, no echo, no line buffering.
	tio.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK |
		syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	tio.Oflag &^= syscall.OPOST
	tio.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON |
		syscall.ISIG | syscall.IEXTEN
	tio.Cflag &^= syscall.CSIZE | syscall.PARENB
	tio.Cflag |= syscall.CS8 | syscall.CREAD | syscall.CLOCAL

	speed, ok := baudBits(baud)
	if !ok {
		return fmt.Errorf("unsupported baud rate %d", baud)
	}
	const cbaud = 0x100f // linux CBAUD mask (not exported by stdlib syscall)
	tio.Cflag = (tio.Cflag &^ cbaud) | speed

	tio.Cc[syscall.VMIN] = 0
	tio.Cc[syscall.VTIME] = 10 // deciseconds → 1 s read timeout for the poll

	if err := ioctlPtr(f.Fd(), syscall.TCSETS, unsafe.Pointer(&tio)); err != nil {
		return fmt.Errorf("tcsets: %w", err)
	}
	return nil
}

func ioctlPtr(fd, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// baudBits maps a decimal baud rate onto the termios speed constant.
func baudBits(baud int) (uint32, bool) {
	switch baud {
	case 1200:
		return syscall.B1200, true // Rot1Prog is fixed at 1200 (KTD5)
	case 4800:
		return syscall.B4800, true
	case 9600:
		return syscall.B9600, true
	case 19200:
		return syscall.B19200, true
	case 38400:
		return syscall.B38400, true
	default:
		return 0, false
	}
}
