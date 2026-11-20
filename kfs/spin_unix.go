//go:build unix

package kfs

import (
	"errors"
	"io"
	"net"
	"syscall"
	"time"
)

// spinWindow is how long a connection polls for its next request after
// sending a reply before it parks in the runtime's network poller. A VMM
// port sends the next request of a busy guest CPU within microseconds,
// and waking a parked goroutine costs about as much again (a netpoller
// wakeup and a thread handoff); polling keeps one CPU busy for at most this
// long per reply, so a connection that goes quiet stops costing anything
// right after.
const spinWindow = 50 * time.Microsecond

// spinConn reads a Unix connection, polling briefly without blocking for
// the first read after each reply (see spinWindow) before it waits the
// usual way.
type spinConn struct {
	net.Conn
	raw   syscall.RawConn
	armed bool // a reply was sent and no read since
}

// newSpinConn returns c with its first read after each reply polled; a
// connection that is no Unix socket reads as usual.
func newSpinConn(c net.Conn) *spinConn {
	s := &spinConn{Conn: c}
	if u, ok := c.(*net.UnixConn); ok {
		if raw, err := u.SyscallConn(); err == nil {
			s.raw = raw
		}
	}
	return s
}

// Write sends a reply and arms the poll for the request that follows it.
func (s *spinConn) Write(p []byte) (int, error) {
	n, err := s.Conn.Write(p)
	s.armed = err == nil
	return n, err
}

// Read reads the next bytes, polling for spinWindow first when armed. The
// callback polls the non-blocking socket until data, an error or the end of
// the window; returning false then parks the reader until the socket is
// readable, after which it reads once more without polling.
func (s *spinConn) Read(p []byte) (int, error) {
	if !s.armed || s.raw == nil || len(p) == 0 {
		return s.Conn.Read(p)
	}
	s.armed = false
	var n int
	var rerr error
	deadline := time.Now().Add(spinWindow)
	polling := true
	err := s.raw.Read(func(fd uintptr) bool {
		for {
			n, rerr = syscall.Read(int(fd), p)
			if !errors.Is(rerr, syscall.EAGAIN) {
				return true
			}
			if !polling || !time.Now().Before(deadline) {
				polling = false
				return false
			}
		}
	})
	if err != nil {
		return 0, err
	}
	switch {
	case rerr != nil:
		return 0, rerr
	case n == 0:
		return 0, io.EOF
	}
	return n, nil
}
