//go:build !windows

package kfs

import (
	"bytes"
	"os"
	"testing"
	"time"
)

// streamSize is how much TestStreamThroughput writes and reads back.
const streamSize = 256 << 20

// TestStreamThroughput writes a 256 MiB file in 1 MiB requests, then reads
// it back sequentially in 1 MiB and 512 KiB requests, logging MiB/s of
// each. It runs only with KFS_STREAM set, against the kernel share at
// KFS_STREAM_SOCK (a daemon's) or else an in-process fixture.
func TestStreamThroughput(t *testing.T) {
	if os.Getenv("KFS_STREAM") == "" {
		t.Skip("set KFS_STREAM to run")
	}
	sock := os.Getenv("KFS_STREAM_SOCK")
	if sock == "" {
		sock = newFixture(t, "vm").serve(t, "vm", true)
	}
	c := dial(t, sock)
	name := "stream." + time.Now().Format("150405.000000")
	file := must(c.Create(RootID, name, 0o644, CreateExcl))(t)
	defer c.Unlink(RootID, name)
	block := bytes.Repeat([]byte("0123456789abcdef"), MaxIO/16)
	rate := func(start time.Time) float64 { return streamSize / float64(1<<20) / time.Since(start).Seconds() }

	start := time.Now()
	for off := 0; off < streamSize; off += MaxIO {
		if _, err := c.Write(file.ID, uint64(off), block); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("WRITE 1 MiB requests: %.0f MiB/s", rate(start))

	for _, n := range []int{MaxIO, MaxIO / 2} {
		start := time.Now()
		for off := 0; off < streamSize; off += n {
			got, err := c.Read(file.ID, uint64(off), uint32(n))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != n || !bytes.Equal(got[:16], block[:16]) {
				t.Fatalf("read at %d: %d bytes", off, len(got))
			}
		}
		t.Logf("READ %d KiB requests: %.0f MiB/s", n>>10, rate(start))
	}
}
