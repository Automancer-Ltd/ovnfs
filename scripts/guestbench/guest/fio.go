//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// fioArgs are the options every fio phase shares: one 256 MiB file through
// the page cache with synchronous I/O. Pre-allocation is off because only
// some of the compared filesystems support it, which would make the same
// job do different work.
var fioArgs = []string{
	"--name=guestbench",
	"--filename=fio.dat",
	"--size=256M",
	"--ioengine=psync",
	"--direct=0",
	"--fallocate=none",
	"--output-format=json",
}

// fioRounds times buffered sequential and random writes and reads of one
// file in the tree's target directory; the read phases start cold, and the
// round removes what it created.
func (r *runner) fioRounds() (func(round int) error, error) {
	dir := filepath.Join(r.target(), "gb-fio")
	return func(round int) error {
		// Remove target itself afterwards when only this round created it.
		created := r.target()
		if _, err := os.Lstat(created); err == nil {
			created = dir
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		err := r.phases(round, []phase{
			{name: "fio.seq-write", body: fio(dir, "--rw=write", "--bs=1M", "--end_fsync=1")},
			{name: "fio.seq-read", cold: true, body: fio(dir, "--rw=read", "--bs=1M")},
			{name: "fio.rand-write", body: fio(dir, "--rw=randwrite", "--bs=4k", "--io_size=64M", "--end_fsync=1")},
			{name: "fio.rand-read", cold: true, body: fio(dir, "--rw=randread", "--bs=4k", "--io_size=64M")},
		})
		if err != nil {
			return err
		}
		return os.RemoveAll(created)
	}, nil
}

// fio returns a phase body running one fio job in dir with fioArgs and
// args, measured by fio's own report.
func fio(dir string, args ...string) func() (result, error) {
	argv := append(append([]string{"--directory=" + dir}, fioArgs...), args...)
	return func() (result, error) {
		var out bytes.Buffer
		cmd := command(dir, "fio", argv...)
		cmd.Stdout = &out
		if _, err := runCmd(cmd); err != nil {
			return result{}, err
		}
		return parseFio(out.Bytes())
	}
}

// fioReport is the part of fio's JSON report the runner reads.
type fioReport struct {
	Jobs []struct {
		Error int    `json:"error"`
		Read  fioDir `json:"read"`
		Write fioDir `json:"write"`
	} `json:"jobs"`
}

// fioDir is one I/O direction of a fio job's report.
type fioDir struct {
	IOBytes  int64   `json:"io_bytes"`
	IOPS     float64 `json:"iops"`
	Runtime  int64   `json:"runtime"` // milliseconds
	TotalIOs int64   `json:"total_ios"`
	Clat     struct {
		Percentile map[string]float64 `json:"percentile"` // nanoseconds
	} `json:"clat_ns"`
}

// p99 is the key of the 99th percentile in fio's percentile maps.
const p99 = "99.000000"

// parseFio turns fio's JSON report of one job into a result: the runtime of
// the direction that moved data, its bytes and I/O count, its IOPS and the
// 99th percentile of its completion latency.
func parseFio(out []byte) (result, error) {
	var rep fioReport
	// fio may print notes before the report.
	start := bytes.Index(out, []byte("\n{"))
	if !bytes.HasPrefix(out, []byte("{")) && start >= 0 {
		out = out[start+1:]
	}
	if err := json.NewDecoder(bytes.NewReader(out)).Decode(&rep); err != nil {
		return result{}, fmt.Errorf("fio report: %w", err)
	}
	if len(rep.Jobs) != 1 {
		return result{}, fmt.Errorf("fio report has %d jobs, want 1", len(rep.Jobs))
	}
	job := rep.Jobs[0]
	if job.Error != 0 {
		return result{}, fmt.Errorf("fio job failed with error %d", job.Error)
	}
	d := job.Read
	if job.Write.IOBytes > 0 {
		d = job.Write
	}
	if d.IOBytes == 0 || d.Runtime <= 0 {
		return result{}, errors.New("fio report shows no I/O")
	}
	lat, ok := d.Clat.Percentile[p99]
	if !ok {
		return result{}, fmt.Errorf("fio report lacks the %s completion latency percentile", p99)
	}
	return result{
		ns:    d.Runtime * 1e6,
		ops:   d.TotalIOs,
		bytes: d.IOBytes,
		extra: map[string]float64{"iops": d.IOPS, "p99_us": lat / 1e3},
	}, nil
}
