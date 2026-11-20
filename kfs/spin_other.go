//go:build !unix

package kfs

import "net"

// spinConn is the connection as it is where sockets cannot be polled
// without blocking through syscall.Read.
type spinConn struct{ net.Conn }

// newSpinConn returns c unchanged in effect.
func newSpinConn(c net.Conn) *spinConn { return &spinConn{Conn: c} }
