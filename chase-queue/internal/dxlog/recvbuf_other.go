//go:build !unix

package dxlog

import "net"

func recvBuffer(*net.UDPConn) int { return 0 }
