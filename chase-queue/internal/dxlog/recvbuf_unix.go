//go:build unix

package dxlog

import (
	"net"
	"syscall"
)

// recvBuffer reads back the receive buffer the kernel granted. Linux
// reports twice the value that was set, which is the usable size.
func recvBuffer(c *net.UDPConn) int {
	rc, err := c.SyscallConn()
	if err != nil {
		return 0
	}
	size := 0
	_ = rc.Control(func(fd uintptr) {
		size, _ = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
	})
	return size
}
