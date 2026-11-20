package kfs

import (
	"errors"
	"fmt"
	"io"
	"net"
)

// Socket buffer sizes tried on a connection, largest first: macOS gives a
// Unix stream socket 8 KiB, so a 1 MiB frame would cross in 128 pieces with
// a context switch each.
const (
	sockBufferMax = 2 << 20
	sockBufferMin = 256 << 10
)

// sizeBuffers raises a Unix connection's send and receive buffers to hold
// a whole frame, halving from sockBufferMax to sockBufferMin while the
// system refuses, then leaving it as it is. Other connections are left alone.
func sizeBuffers(c net.Conn) {
	u, ok := c.(*net.UnixConn)
	if !ok {
		return
	}
	for _, set := range []func(int) error{u.SetReadBuffer, u.SetWriteBuffer} {
		for size := sockBufferMax; size >= sockBufferMin; size /= 2 {
			if set(size) == nil {
				break
			}
		}
	}
}

// frameStart is the initial size of a frame buffer: a request or reply
// other than a READ or WRITE payload fits, so most frames take one read.
const frameStart = 64 << 10

// frames reads length-prefixed frames from a stream into one reusable
// buffer, so a payload goes from the socket straight to where it is used:
// each read asks for as much as the buffer holds, and bytes past the frame
// (a peer that sent ahead) stay for the next one.
type frames struct {
	buf  []byte
	n    int // bytes of buf read so far
	used int // bytes of buf the last frame took
}

// read returns the next frame's body, valid until the next read; io.EOF
// when the stream ends between frames.
func (f *frames) read(r io.Reader) ([]byte, error) {
	if f.used > 0 {
		f.n = copy(f.buf, f.buf[f.used:f.n])
		f.used = 0
	}
	if f.buf == nil {
		f.buf = make([]byte, frameStart)
	}
	if err := f.fill(r, frameHeader); err != nil {
		return nil, err
	}
	size := le.Uint32(f.buf)
	if size > MaxFrame {
		return nil, fmt.Errorf("frame of %d bytes exceeds %d", size, MaxFrame)
	}
	total := frameHeader + int(size)
	if err := f.fill(r, total); err != nil {
		return nil, err
	}
	f.used = total
	return f.buf[frameHeader:total], nil
}

// fill reads until buf holds want bytes, growing it to fit.
func (f *frames) fill(r io.Reader, want int) error {
	if want > len(f.buf) {
		grown := make([]byte, want)
		copy(grown, f.buf[:f.n])
		f.buf = grown
	}
	for f.n < want {
		m, err := r.Read(f.buf[f.n:])
		f.n += m
		if err == nil || f.n >= want {
			continue
		}
		if errors.Is(err, io.EOF) && f.n > 0 {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	return nil
}
