//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"golang.org/x/sys/unix"

	"github.com/stencil-hq/ovnfs/scripts/guestbench/wire"
)

// Shape of the network calibration.
const (
	rrTrips  = 2000      // one-byte round trips of net.rr
	netBytes = 256 << 20 // payload of net.up and net.down
	netChunk = 1 << 20   // size of each write or read of the payload
	// netTimeout bounds one connection, so a stuck server fails the run
	// instead of hanging the VM.
	netTimeout = 5 * time.Minute
	// clockSamples is how often syncClock reads the server's clock; the
	// reading with the shortest round trip has the smallest error.
	clockSamples = 3
)

// netRounds times the VM's network against the host's calibration server in
// the first round: latency as one-byte round trips, then throughput up and
// down. Each phase dials its own connection outside the timing.
func (r *runner) netRounds() (func(round int) error, error) {
	var c *net.TCPConn
	dial := func(mode byte) func() error {
		return func() (err error) {
			c, err = openEcho(r.plan.Echo, mode)
			return err
		}
	}
	// The timed trips start on an established path that has carried one.
	warm := func() error {
		if err := dial(wire.EchoRR)(); err != nil {
			return err
		}
		_, err := roundTrips(c, 1)
		return err
	}
	hangUp := func() error { return c.Close() }
	buf := make([]byte, netChunk)
	rr := func() (result, error) { return roundTrips(c, rrTrips) }
	up := func() (result, error) { return upload(c, buf) }
	down := func() (result, error) { return download(c, buf) }
	return func(round int) error {
		if round > 0 || r.plan.Echo == "" {
			return nil
		}
		return r.phases(round, []phase{
			{name: "net.rr", before: warm, body: rr, after: hangUp},
			{name: "net.up", before: dial(wire.EchoUp), body: up, after: hangUp},
			{name: "net.down", before: dial(wire.EchoDown), body: down, after: hangUp},
		})
	}, nil
}

// dialEcho connects to the calibration server at addr with Nagle's
// algorithm off and the connection's deadline set.
func dialEcho(addr string) (*net.TCPConn, error) {
	conn, err := net.DialTimeout("tcp", addr, netTimeout)
	if err != nil {
		return nil, err
	}
	c := conn.(*net.TCPConn)
	if err := errors.Join(c.SetNoDelay(true), c.SetDeadline(time.Now().Add(netTimeout))); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// openEcho dials the calibration server at addr and selects mode.
func openEcho(addr string, mode byte) (*net.TCPConn, error) {
	c, err := dialEcho(addr)
	if err != nil {
		return nil, err
	}
	if _, err := c.Write([]byte{mode}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// clockSample is one reading of the calibration server's wall clock.
type clockSample struct {
	server int64         // the server's Unix nanoseconds
	rtt    time.Duration // round trip that bounds the reading's error
	at     time.Time     // when the reply arrived, by the monotonic clock
}

// syncClock sets the guest's wall clock from the calibration server at addr.
// The VM boots at the Unix epoch, so without it files the guest writes would
// carry mtimes older than the fixture's and cargo's freshness checks would
// misjudge every build.
func syncClock(addr string) error {
	var best clockSample
	for i := range clockSamples {
		s, err := sampleClock(addr)
		if err != nil {
			return err
		}
		if i == 0 || s.rtt < best.rtt {
			best = s
		}
	}
	// The server's reading is taken halfway through the round trip on
	// average, and time has passed since the best reply arrived.
	now := best.server + int64(best.rtt/2) + int64(time.Since(best.at))
	ts := unix.NsecToTimespec(now)
	return unix.ClockSettime(unix.CLOCK_REALTIME, &ts)
}

// sampleClock reads the server's wall clock once, timing the request and
// its reply with the monotonic clock.
func sampleClock(addr string) (clockSample, error) {
	c, err := dialEcho(addr)
	if err != nil {
		return clockSample{}, err
	}
	defer c.Close()
	var reply [8]byte
	start := time.Now()
	if _, err := c.Write([]byte{wire.EchoTime}); err != nil {
		return clockSample{}, err
	}
	if _, err := io.ReadFull(c, reply[:]); err != nil {
		return clockSample{}, err
	}
	at := time.Now()
	return clockSample{
		server: int64(binary.LittleEndian.Uint64(reply[:])),
		rtt:    at.Sub(start),
		at:     at,
	}, nil
}

// roundTrips sends one byte and waits for its echo n times.
func roundTrips(c *net.TCPConn, n int) (result, error) {
	b := []byte{1}
	for range n {
		if _, err := c.Write(b); err != nil {
			return result{}, err
		}
		if _, err := io.ReadFull(c, b); err != nil {
			return result{}, err
		}
	}
	return result{ops: int64(n)}, nil
}

// upload writes netBytes in buf-sized writes, then half-closes and waits for
// the server's count, so the timing ends when the server has everything.
func upload(c *net.TCPConn, buf []byte) (result, error) {
	for sent := 0; sent < netBytes; sent += len(buf) {
		if _, err := c.Write(buf); err != nil {
			return result{}, err
		}
	}
	if err := c.CloseWrite(); err != nil {
		return result{}, err
	}
	var count [8]byte
	if _, err := io.ReadFull(c, count[:]); err != nil {
		return result{}, err
	}
	if got := binary.LittleEndian.Uint64(count[:]); got != netBytes {
		return result{}, fmt.Errorf("server received %d bytes, want %d", got, netBytes)
	}
	return result{bytes: netBytes}, nil
}

// download asks for netBytes and reads them in buf-sized reads until the
// server closes.
func download(c *net.TCPConn, buf []byte) (result, error) {
	var size [8]byte
	binary.LittleEndian.PutUint64(size[:], netBytes)
	if _, err := c.Write(size[:]); err != nil {
		return result{}, err
	}
	var got int64
	for {
		n, err := c.Read(buf)
		got += int64(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			return result{}, err
		}
	}
	if got != netBytes {
		return result{}, fmt.Errorf("received %d bytes, want %d", got, netBytes)
	}
	return result{bytes: netBytes}, nil
}
