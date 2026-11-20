//go:build darwin || linux

package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/stencil-hq/ovnfs/scripts/guestbench/wire"
)

// echoServer is the calibration peer of the net workload: it measures what
// the VM's network costs before any file protocol rides on it. The guest
// reaches it on the user-net gateway, which maps to host loopback.
type echoServer struct {
	l net.Listener
}

// startEcho listens on a free port of 127.0.0.1 and serves until Close.
func startEcho() (*echoServer, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("echo server: %w", err)
	}
	s := &echoServer{l: l}
	go s.serve()
	return s, nil
}

// port returns the port the server listens on.
func (s *echoServer) port() int { return s.l.Addr().(*net.TCPAddr).Port }

// Close stops accepting; connections end with their VM.
func (s *echoServer) Close() error { return s.l.Close() }

// serve accepts connections until the listener closes.
func (s *echoServer) serve() {
	for {
		c, err := s.l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			if err := handleEcho(c); err != nil {
				fmt.Fprintln(os.Stderr, "guestbench: echo:", err)
			}
		}()
	}
}

// echoBuf sizes the server's reads and writes.
const echoBuf = 256 << 10

// handleEcho serves one connection in the mode its first byte names (see
// wire.EchoRR, wire.EchoUp, wire.EchoDown).
func handleEcho(c net.Conn) error {
	var mode [1]byte
	if _, err := io.ReadFull(c, mode[:]); err != nil {
		return nil // a probe that sent nothing
	}
	buf := make([]byte, echoBuf)
	switch mode[0] {
	case wire.EchoRR:
		// Plain reads and writes: no splice or sendfile fast path between a
		// socket and itself.
		_, err := io.CopyBuffer(struct{ io.Writer }{c}, struct{ io.Reader }{c}, buf)
		return err
	case wire.EchoUp:
		n, err := io.CopyBuffer(io.Discard, struct{ io.Reader }{c}, buf)
		if err != nil {
			return err
		}
		_, err = c.Write(binary.LittleEndian.AppendUint64(nil, uint64(n)))
		return err
	case wire.EchoTime:
		// The clock is read last, so only the reply's flight separates it from
		// the guest's receipt.
		_, err := c.Write(binary.LittleEndian.AppendUint64(make([]byte, 0, 8), uint64(time.Now().UnixNano())))
		return err
	case wire.EchoDown:
		var size [8]byte
		if _, err := io.ReadFull(c, size[:]); err != nil {
			return err
		}
		for left := binary.LittleEndian.Uint64(size[:]); left > 0; {
			n := min(left, uint64(len(buf)))
			if _, err := c.Write(buf[:n]); err != nil {
				return err
			}
			left -= n
		}
		return nil
	}
	return fmt.Errorf("unknown mode %q", mode[0])
}
